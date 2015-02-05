package listen_test

import (
	"encoding/json"
	"errors"
	"syscall"

	. "github.com/cloudfoundry-incubator/nsync/listen"
	"github.com/cloudfoundry-incubator/nsync/listen/fakes"
	"github.com/cloudfoundry-incubator/receptor"
	"github.com/cloudfoundry-incubator/receptor/fake_receptor"
	"github.com/cloudfoundry-incubator/route-emitter/cfroutes"
	"github.com/cloudfoundry-incubator/runtime-schema/cc_messages"
	"github.com/cloudfoundry-incubator/runtime-schema/models"
	"github.com/cloudfoundry/dropsonde/metric_sender/fake"
	"github.com/cloudfoundry/dropsonde/metrics"
	"github.com/cloudfoundry/gunk/diegonats"
	"github.com/pivotal-golang/lager/lagertest"
	"github.com/tedsuo/ifrit"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
	"github.com/onsi/gomega/gbytes"
)

const (
	stopIndexTopic       = "diego.stop.index"
	desireAppTopic       = "diego.desire.app"
	desireDockerAppTopic = "diego.docker.desire.app"
)

var _ = Describe("Listen", func() {
	var (
		builder            *fakes.FakeRecipeBuilder
		fakenats           *diegonats.FakeNATSClient
		desireAppRequest   cc_messages.DesireAppRequestFromCC
		logger             *lagertest.TestLogger
		fakeReceptorClient *fake_receptor.FakeClient

		process ifrit.Process

		metricSender *fake.FakeMetricSender
	)

	BeforeEach(func() {
		logger = lagertest.NewTestLogger("test")

		fakenats = diegonats.NewFakeClient()

		builder = new(fakes.FakeRecipeBuilder)
		fakeReceptorClient = new(fake_receptor.FakeClient)

		runner := Listen{
			NATSClient:     fakenats,
			ReceptorClient: fakeReceptorClient,
			Logger:         logger,
			RecipeBuilder:  builder,
		}

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

		process = ifrit.Envoke(runner)
	})

	AfterEach(func() {
		process.Signal(syscall.SIGINT)
		Eventually(process.Wait()).Should(Receive())
	})

	Describe("when a desire app message is received", func() {
		JustBeforeEach(func() {
			messagePayload, err := json.Marshal(desireAppRequest)
			Ω(err).ShouldNot(HaveOccurred())

			fakenats.Publish(desireAppTopic, messagePayload)
		})

		Context("when the desired LRP does not exist", func() {
			var newlyDesiredLRP receptor.DesiredLRPCreateRequest

			BeforeEach(func() {
				fakeReceptorClient.GetDesiredLRPReturns(receptor.DesiredLRPResponse{}, receptor.Error{
					Type:    receptor.DesiredLRPNotFound,
					Message: "Desired LRP with guid 'new-process-guid' not found",
				})
				builder.BuildReturns(&newlyDesiredLRP, nil)
			})

			Context("when the desired app is a traditional app", func() {
				BeforeEach(func() {
					newlyDesiredLRP = receptor.DesiredLRPCreateRequest{
						ProcessGuid: "new-process-guid",
						Instances:   1,
						Stack:       "stack-2",
						Action: &models.RunAction{
							Path: "ls",
						},
						Annotation: "last-modified-etag",
					}
				})

				It("desires the LRP in the bbs", func() {
					Eventually(fakeReceptorClient.CreateDesiredLRPCallCount).Should(Equal(1))

					Eventually(fakeReceptorClient.GetDesiredLRPCallCount).Should(Equal(1))
					Ω(fakeReceptorClient.CreateDesiredLRPArgsForCall(0)).Should(Equal(newlyDesiredLRP))

					Ω(builder.BuildArgsForCall(0)).Should(Equal(&desireAppRequest))
				})
			})

			Context("when the desired app is a docker app", func() {
				BeforeEach(func() {
					desireAppRequest.DockerImageUrl = "https:///docker.com/docker"

					newlyDesiredLRP = receptor.DesiredLRPCreateRequest{
						ProcessGuid: "new-process-guid",
						Instances:   1,
						Stack:       "stack-2",
						RootFSPath:  "docker:///docker.com/docker",
						Action: &models.RunAction{
							Path: "ls",
						},
					}
				})

				It("desires the LRP in the bbs", func() {
					Eventually(fakeReceptorClient.CreateDesiredLRPCallCount).Should(Equal(1))

					Eventually(fakeReceptorClient.GetDesiredLRPCallCount).Should(Equal(1))
					Ω(fakeReceptorClient.CreateDesiredLRPArgsForCall(0)).Should(Equal(newlyDesiredLRP))
					Ω(builder.BuildArgsForCall(0)).Should(Equal(&desireAppRequest))
				})
			})

			It("increments the desired LRPs counter", func() {
				Eventually(func() uint64 {
					return metricSender.GetCounter("LRPsDesired")
				}).Should(Equal(uint64(1)))
			})

			Context("when the number of desired app instances is zero", func() {
				BeforeEach(func() {
					desireAppRequest.NumInstances = 0
				})

				It("deletes the desired LRP", func() {
					Eventually(fakeReceptorClient.DeleteDesiredLRPCallCount).Should(Equal(1))
					Ω(fakeReceptorClient.DeleteDesiredLRPArgsForCall(0)).Should(Equal("some-guid"))
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
					Consistently(fakeReceptorClient.DeleteDesiredLRPCallCount).Should(Equal(0))
				})
			})
		})

		Context("when desired LRP already exists", func() {
			BeforeEach(func() {
				fakeReceptorClient.GetDesiredLRPReturns(receptor.DesiredLRPResponse{
					ProcessGuid: "some-guid",
				}, nil)
			})

			It("checks to see if LRP already exists", func() {
				Eventually(fakeReceptorClient.GetDesiredLRPCallCount).Should(Equal(1))
			})

			It("updates the LRP in bbs", func() {
				Eventually(fakeReceptorClient.UpdateDesiredLRPCallCount).Should(Equal(1))

				processGuid, updateRequest := fakeReceptorClient.UpdateDesiredLRPArgsForCall(0)
				Ω(processGuid).Should(Equal("some-guid"))
				Ω(*updateRequest.Instances).Should(Equal(2))
				Ω(*updateRequest.Annotation).Should(Equal("last-modified-etag"))
				Ω(updateRequest.Routes).Should(Equal(cfroutes.CFRoutes{
					{Hostnames: []string{"route1", "route2"}, Port: 8080},
				}.RoutingInfo()))
			})
		})
	})

	Describe("when an invalid desire app message is received", func() {
		BeforeEach(func() {
			fakenats.Publish(desireAppTopic, []byte(`
        {
          "some_random_key": "does not matter"
      `))
		})

		It("logs an error", func() {
			Eventually(logger.TestSink.Buffer).Should(gbytes.Say("parse-nats-message-failed"))
		})

		It("does not desire the LRP", func() {
			Consistently(fakeReceptorClient.DeleteDesiredLRPCallCount).Should(Equal(0))
		})
	})

	Describe("when a stop index message is received", func() {
		var killIndexRequest cc_messages.KillIndexRequestFromCC

		JustBeforeEach(func() {
			killIndexRequest = cc_messages.KillIndexRequestFromCC{
				ProcessGuid: "process-guid",
				Index:       1,
			}
			messagePayload, err := json.Marshal(killIndexRequest)
			Ω(err).ShouldNot(HaveOccurred())

			fakenats.Publish(stopIndexTopic, messagePayload)
		})

		It("makes stop requests for those instances", func() {
			Eventually(fakeReceptorClient.KillActualLRPByProcessGuidAndIndexCallCount).Should(Equal(1))

			processGuid, stopIndex := fakeReceptorClient.KillActualLRPByProcessGuidAndIndexArgsForCall(0)
			Ω(processGuid).Should(Equal(killIndexRequest.ProcessGuid))
			Ω(stopIndex).Should(Equal(killIndexRequest.Index))
		})
	})
})
