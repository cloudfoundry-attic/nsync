package handlers_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"io/ioutil"
	"net/http"
	"net/http/httptest"
	"net/url"

	"github.com/cloudfoundry-incubator/nsync/bulk/fakes"
	"github.com/cloudfoundry-incubator/nsync/handlers"
	"github.com/cloudfoundry-incubator/receptor"
	"github.com/cloudfoundry-incubator/receptor/fake_receptor"
	"github.com/cloudfoundry-incubator/route-emitter/cfroutes"
	"github.com/cloudfoundry-incubator/runtime-schema/cc_messages"
	"github.com/cloudfoundry-incubator/runtime-schema/models"
	"github.com/cloudfoundry/dropsonde/metric_sender/fake"
	"github.com/cloudfoundry/dropsonde/metrics"
	"github.com/pivotal-golang/lager/lagertest"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
	"github.com/onsi/gomega/gbytes"
)

var _ = Describe("DesireAppHandler", func() {
	var (
		logger           *lagertest.TestLogger
		fakeReceptor     *fake_receptor.FakeClient
		builder          *fakes.FakeRecipeBuilder
		desireAppRequest cc_messages.DesireAppRequestFromCC
		metricSender     *fake.FakeMetricSender

		request          *http.Request
		responseRecorder *httptest.ResponseRecorder
	)

	BeforeEach(func() {
		logger = lagertest.NewTestLogger("test")
		fakeReceptor = new(fake_receptor.FakeClient)
		builder = new(fakes.FakeRecipeBuilder)

		desireAppRequest = cc_messages.DesireAppRequestFromCC{
			ProcessGuid:  "some-guid",
			DropletUri:   "http://the-droplet.uri.com",
			Stack:        "some-stack",
			StartCommand: "the-start-command",
			Environment: cc_messages.Environment{
				{Name: "foo", Value: "bar"},
				{Name: "VCAP_APPLICATION", Value: "{\"application_name\":\"my-app\"}"},
			},
			MemoryMB:        128,
			DiskMB:          512,
			FileDescriptors: 32,
			NumInstances:    2,
			Routes:          []string{"route1", "route2"},
			LogGuid:         "some-log-guid",
			ETag:            "last-modified-etag",
		}

		metricSender = fake.NewFakeMetricSender()
		metrics.Initialize(metricSender)

		responseRecorder = httptest.NewRecorder()

		var err error
		request, err = http.NewRequest("POST", "", nil)
		Ω(err).ShouldNot(HaveOccurred())
		request.Form = url.Values{
			":process_guid": []string{"some-guid"},
		}
	})

	JustBeforeEach(func() {
		if request.Body == nil {
			jsonBytes, err := json.Marshal(&desireAppRequest)
			Ω(err).ShouldNot(HaveOccurred())
			reader := bytes.NewReader(jsonBytes)

			request.Body = ioutil.NopCloser(reader)
		}

		handler := handlers.NewDesireAppHandler(logger, fakeReceptor, builder)
		handler.DesireApp(responseRecorder, request)
	})

	Context("when the desired LRP does not exist", func() {
		var newlyDesiredLRP receptor.DesiredLRPCreateRequest

		BeforeEach(func() {
			newlyDesiredLRP = receptor.DesiredLRPCreateRequest{
				ProcessGuid: "new-process-guid",
				Instances:   1,
				RootFS:      models.PreloadedRootFS("stack-2"),
				Action: &models.RunAction{
					Path: "ls",
				},
				Annotation: "last-modified-etag",
			}

			fakeReceptor.GetDesiredLRPReturns(receptor.DesiredLRPResponse{}, receptor.Error{
				Type:    receptor.DesiredLRPNotFound,
				Message: "Desired LRP with guid 'new-process-guid' not found",
			})
			builder.BuildReturns(&newlyDesiredLRP, nil)
		})

		It("creates the desired LRP", func() {
			Ω(fakeReceptor.CreateDesiredLRPCallCount()).Should(Equal(1))

			Ω(fakeReceptor.GetDesiredLRPCallCount()).Should(Equal(1))
			Ω(fakeReceptor.CreateDesiredLRPArgsForCall(0)).Should(Equal(newlyDesiredLRP))

			Ω(builder.BuildArgsForCall(0)).Should(Equal(&desireAppRequest))
		})

		It("responds with 202 Accepted", func() {
			Ω(responseRecorder.Code).Should(Equal(http.StatusAccepted))
		})

		It("increments the desired LRPs counter", func() {
			Ω(metricSender.GetCounter("LRPsDesired")).Should(Equal(uint64(1)))
		})

		Context("when the receptor fails", func() {
			BeforeEach(func() {
				fakeReceptor.CreateDesiredLRPReturns(errors.New("oh no"))
			})

			It("responds with a ServiceUnavailabe error", func() {
				Ω(responseRecorder.Code).Should(Equal(http.StatusServiceUnavailable))
			})
		})

		Context("when the number of desired app instances is zero", func() {
			BeforeEach(func() {
				desireAppRequest.NumInstances = 0
			})

			It("deletes the desired LRP", func() {
				Ω(fakeReceptor.DeleteDesiredLRPCallCount()).Should(Equal(1))
				Ω(fakeReceptor.DeleteDesiredLRPArgsForCall(0)).Should(Equal("some-guid"))
			})

			It("responds with 202 Accepted", func() {
				Ω(responseRecorder.Code).Should(Equal(http.StatusAccepted))
			})

			It("does not increments the desired LRPs counter", func() {
				Ω(metricSender.GetCounter("LRPsDesired")).Should(Equal(uint64(0)))
			})

			Context("when the receptor fails", func() {
				BeforeEach(func() {
					fakeReceptor.DeleteDesiredLRPReturns(errors.New("oh no"))
				})

				It("responds with a ServiceUnavailabe error", func() {
					Ω(responseRecorder.Code).Should(Equal(http.StatusServiceUnavailable))
				})
			})
		})

		Context("when building the recipe fails to build", func() {
			BeforeEach(func() {
				builder.BuildReturns(nil, errors.New("oh no!"))
			})

			It("logs an error", func() {
				Eventually(logger.TestSink.Buffer).Should(gbytes.Say("failed-to-build-recipe"))
				Eventually(logger.TestSink.Buffer).Should(gbytes.Say("oh no!"))
			})

			It("does not desire the LRP", func() {
				Consistently(fakeReceptor.DeleteDesiredLRPCallCount).Should(Equal(0))
			})

			It("responds with 500 Accepted", func() {
				Ω(responseRecorder.Code).Should(Equal(http.StatusInternalServerError))
			})
		})
	})

	Context("when desired LRP already exists", func() {
		BeforeEach(func() {
			fakeReceptor.GetDesiredLRPReturns(receptor.DesiredLRPResponse{
				ProcessGuid: "some-guid",
			}, nil)
		})

		It("checks to see if LRP already exists", func() {
			Eventually(fakeReceptor.GetDesiredLRPCallCount).Should(Equal(1))
		})

		It("updates the LRP", func() {
			Eventually(fakeReceptor.UpdateDesiredLRPCallCount).Should(Equal(1))

			processGuid, updateRequest := fakeReceptor.UpdateDesiredLRPArgsForCall(0)
			Ω(processGuid).Should(Equal("some-guid"))
			Ω(*updateRequest.Instances).Should(Equal(2))
			Ω(*updateRequest.Annotation).Should(Equal("last-modified-etag"))
			Ω(updateRequest.Routes).Should(Equal(cfroutes.CFRoutes{
				{Hostnames: []string{"route1", "route2"}, Port: 8080},
			}.RoutingInfo()))
		})

		It("responds with 202 Accepted", func() {
			Ω(responseRecorder.Code).Should(Equal(http.StatusAccepted))
		})

		Context("when the receptor fails", func() {
			BeforeEach(func() {
				fakeReceptor.UpdateDesiredLRPReturns(errors.New("oh no"))
			})

			It("responds with a ServiceUnavailabe error", func() {
				Ω(responseRecorder.Code).Should(Equal(http.StatusServiceUnavailable))
			})
		})
	})

	Context("when an invalid desire app message is received", func() {
		BeforeEach(func() {
			reader := bytes.NewBufferString("not valid json")
			request.Body = ioutil.NopCloser(reader)
		})

		It("does not call the receptor", func() {
			Ω(fakeReceptor.KillActualLRPByProcessGuidAndIndexCallCount()).Should(BeZero())
		})

		It("responds with 400 Bad Request", func() {
			Ω(responseRecorder.Code).Should(Equal(http.StatusBadRequest))
		})

		It("logs an error", func() {
			Eventually(logger.TestSink.Buffer).Should(gbytes.Say("parse-desired-app-request-failed"))
		})

		It("does not touch the LRP", func() {
			Ω(fakeReceptor.CreateDesiredLRPCallCount()).Should(Equal(0))
			Ω(fakeReceptor.UpdateDesiredLRPCallCount()).Should(Equal(0))
			Ω(fakeReceptor.DeleteDesiredLRPCallCount()).Should(Equal(0))
		})

	})

	Context("when the process guids do not match", func() {
		BeforeEach(func() {
			request.Form.Set(":process_guid", "another-guid")
		})

		It("does not call the receptor", func() {
			Ω(fakeReceptor.KillActualLRPByProcessGuidAndIndexCallCount()).Should(BeZero())
		})

		It("responds with 400 Bad Request", func() {
			Ω(responseRecorder.Code).Should(Equal(http.StatusBadRequest))
		})

		It("logs an error", func() {
			Eventually(logger.TestSink.Buffer).Should(gbytes.Say("desire-app.process-guid-mismatch"))
		})

		It("does not touch the LRP", func() {
			Ω(fakeReceptor.CreateDesiredLRPCallCount()).Should(Equal(0))
			Ω(fakeReceptor.UpdateDesiredLRPCallCount()).Should(Equal(0))
			Ω(fakeReceptor.DeleteDesiredLRPCallCount()).Should(Equal(0))
		})
	})
})
