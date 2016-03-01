package bulk_test

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/cloudfoundry-incubator/bbs/fake_bbs"
	"github.com/cloudfoundry-incubator/bbs/models"
	"github.com/cloudfoundry-incubator/nsync/bulk"
	"github.com/cloudfoundry-incubator/nsync/bulk/fakes"
	"github.com/cloudfoundry-incubator/nsync/recipebuilder"
	"github.com/cloudfoundry-incubator/routing-info/cfroutes"
	"github.com/cloudfoundry-incubator/routing-info/tcp_routes"
	"github.com/cloudfoundry-incubator/runtime-schema/cc_messages"
	"github.com/cloudfoundry/dropsonde/metric_sender/fake"
	"github.com/cloudfoundry/dropsonde/metrics"
	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
	"github.com/onsi/gomega/gbytes"
	"github.com/pivotal-golang/clock/fakeclock"
	"github.com/pivotal-golang/lager"
	"github.com/pivotal-golang/lager/lagertest"
	"github.com/tedsuo/ifrit"
	"github.com/tedsuo/ifrit/ginkgomon"
)

var _ = Describe("Processor", func() {
	var (
		fingerprintsToFetch     []cc_messages.CCDesiredAppFingerprint
		taskStatesToFetch       []cc_messages.CCTaskState
		existingSchedulingInfos []*models.DesiredLRPSchedulingInfo

		bbsClient              *fake_bbs.FakeClient
		taskClient             *fakes.FakeTaskClient
		fetcher                *fakes.FakeFetcher
		buildpackRecipeBuilder *fakes.FakeRecipeBuilder
		dockerRecipeBuilder    *fakes.FakeRecipeBuilder

		processor ifrit.Runner

		process      ifrit.Process
		syncDuration time.Duration
		metricSender *fake.FakeMetricSender
		clock        *fakeclock.FakeClock

		pollingInterval time.Duration

		logger *lagertest.TestLogger
	)

	BeforeEach(func() {
		metricSender = fake.NewFakeMetricSender()
		metrics.Initialize(metricSender, nil)

		syncDuration = 900900
		pollingInterval = 500 * time.Millisecond
		clock = fakeclock.NewFakeClock(time.Now())

		fingerprintsToFetch = []cc_messages.CCDesiredAppFingerprint{
			{ProcessGuid: "current-process-guid", ETag: "current-etag"},
			{ProcessGuid: "stale-process-guid", ETag: "new-etag"},
			{ProcessGuid: "docker-process-guid", ETag: "new-etag"},
			{ProcessGuid: "new-process-guid", ETag: "new-etag"},
		}
		tcpRouteInfo := cc_messages.CCTCPRoutes{
			{
				RouterGroupGuid: "router-group-guid",
				ExternalPort:    11111,
				ContainerPort:   9999,
			},
		}
		tcpRoutesJson, err := json.Marshal(tcpRouteInfo)
		Expect(err).NotTo(HaveOccurred())
		staleTcpRouteMessage := json.RawMessage(tcpRoutesJson)

		staleRouteMessage := json.RawMessage([]byte(`{ "some-route-key": "some-route-value" }`))
		existingSchedulingInfos = []*models.DesiredLRPSchedulingInfo{
			{
				DesiredLRPKey: models.NewDesiredLRPKey("current-process-guid", "domain", "log-guid"),
				Annotation:    "current-etag"},
			{
				DesiredLRPKey: models.NewDesiredLRPKey("stale-process-guid", "domain", "log-guid"),
				Annotation:    "stale-etag",
				Routes: models.Routes{
					"router-route-data":   &staleRouteMessage,
					tcp_routes.TCP_ROUTER: &staleTcpRouteMessage,
				},
			},
			{
				DesiredLRPKey: models.NewDesiredLRPKey("docker-process-guid", "domain", "log-guid"),
				Annotation:    "docker-etag",
				Routes: models.Routes{
					"router-route-data": &staleRouteMessage,
				},
			},
			{
				DesiredLRPKey: models.NewDesiredLRPKey("excess-process-guid", "domain", "log-guid"),
				Annotation:    "excess-etag",
			},
		}

		fetcher = new(fakes.FakeFetcher)
		fetcher.FetchFingerprintsStub = func(
			logger lager.Logger,
			cancel <-chan struct{},
			httpClient *http.Client,
		) (<-chan []cc_messages.CCDesiredAppFingerprint, <-chan error) {
			results := make(chan []cc_messages.CCDesiredAppFingerprint, 1)
			errors := make(chan error, 1)

			results <- fingerprintsToFetch
			close(results)
			close(errors)

			return results, errors
		}

		fetcher.FetchDesiredAppsStub = func(
			logger lager.Logger,
			cancel <-chan struct{},
			httpClient *http.Client,
			fingerprints <-chan []cc_messages.CCDesiredAppFingerprint,
		) (<-chan []cc_messages.DesireAppRequestFromCC, <-chan error) {
			batch := <-fingerprints

			results := []cc_messages.DesireAppRequestFromCC{}
			for _, fingerprint := range batch {
				routeInfo, err := cc_messages.CCHTTPRoutes{
					{Hostname: "host-" + fingerprint.ProcessGuid},
				}.CCRouteInfo()
				Expect(err).NotTo(HaveOccurred())

				lrp := cc_messages.DesireAppRequestFromCC{
					ProcessGuid: fingerprint.ProcessGuid,
					ETag:        fingerprint.ETag,
					RoutingInfo: routeInfo,
				}
				if strings.HasPrefix(fingerprint.ProcessGuid, "docker") {
					lrp.DockerImageUrl = "some-image"
				}
				results = append(results, lrp)
			}

			desired := make(chan []cc_messages.DesireAppRequestFromCC, 1)
			desired <- results
			close(desired)

			errors := make(chan error, 1)
			close(errors)

			return desired, errors
		}

		fetcher.FetchTaskStatesStub = func(
			logger lager.Logger,
			cancel <-chan struct{},
			httpClient *http.Client,
		) (<-chan []cc_messages.CCTaskState, <-chan error) {
			results := make(chan []cc_messages.CCTaskState, 1)
			errors := make(chan error, 1)

			results <- taskStatesToFetch
			close(results)
			close(errors)

			return results, errors
		}

		buildpackRecipeBuilder = new(fakes.FakeRecipeBuilder)
		buildpackRecipeBuilder.BuildStub = func(ccRequest *cc_messages.DesireAppRequestFromCC) (*models.DesiredLRP, error) {
			createRequest := models.DesiredLRP{
				ProcessGuid: ccRequest.ProcessGuid,
				Annotation:  ccRequest.ETag,
			}
			return &createRequest, nil
		}
		buildpackRecipeBuilder.ExtractExposedPortsStub = func(ccRequest *cc_messages.DesireAppRequestFromCC) ([]uint32, error) {
			return []uint32{8080}, nil
		}

		dockerRecipeBuilder = new(fakes.FakeRecipeBuilder)
		dockerRecipeBuilder.BuildStub = func(ccRequest *cc_messages.DesireAppRequestFromCC) (*models.DesiredLRP, error) {
			createRequest := models.DesiredLRP{
				ProcessGuid: ccRequest.ProcessGuid,
				Annotation:  ccRequest.ETag,
			}
			return &createRequest, nil
		}

		bbsClient = new(fake_bbs.FakeClient)
		bbsClient.DesiredLRPSchedulingInfosReturns(existingSchedulingInfos, nil)

		bbsClient.UpsertDomainStub = func(string, time.Duration) error {
			clock.Increment(syncDuration)
			return nil
		}

		taskClient = new(fakes.FakeTaskClient)

		logger = lagertest.NewTestLogger("test")

		processor = bulk.NewProcessor(
			logger,
			bbsClient,
			taskClient,
			500*time.Millisecond,
			time.Second,
			10,
			50,
			50,
			false,
			fetcher,
			map[string]recipebuilder.RecipeBuilder{
				"buildpack": buildpackRecipeBuilder,
				"docker":    dockerRecipeBuilder,
			},
			clock,
		)
	})

	JustBeforeEach(func() {
		process = ifrit.Invoke(processor)
	})

	AfterEach(func() {
		ginkgomon.Interrupt(process)
	})

	Context("when fetching succeeds", func() {
		It("emits the total time taken to talk to CC and then update desired state", func() {
			Eventually(bbsClient.UpsertDomainCallCount, 5).Should(Equal(1))

			Eventually(func() fake.Metric { return metricSender.GetValue("DesiredLRPSyncDuration") }).Should(Equal(fake.Metric{
				Value: float64(syncDuration),
				Unit:  "nanos",
			}))
		})

		Context("desired lrps", func() {
			Context("and the differ discovers desired LRPs to delete", func() {
				It("the processor deletes them", func() {
					Eventually(bbsClient.RemoveDesiredLRPCallCount).Should(Equal(1))
					Consistently(bbsClient.RemoveDesiredLRPCallCount).Should(Equal(1))

					Expect(bbsClient.RemoveDesiredLRPArgsForCall(0)).To(Equal("excess-process-guid"))
				})
			})

			Context("and the differ discovers missing apps", func() {
				It("uses the recipe builder to construct the create LRP request", func() {
					Eventually(buildpackRecipeBuilder.BuildCallCount).Should(Equal(1))
					Consistently(buildpackRecipeBuilder.BuildCallCount).Should(Equal(1))

					expectedRoutingInfo, err := cc_messages.CCHTTPRoutes{
						{Hostname: "host-new-process-guid"},
					}.CCRouteInfo()
					Expect(err).NotTo(HaveOccurred())

					Eventually(buildpackRecipeBuilder.BuildArgsForCall(0)).Should(Equal(
						&cc_messages.DesireAppRequestFromCC{
							ProcessGuid: "new-process-guid",
							ETag:        "new-etag",
							RoutingInfo: expectedRoutingInfo,
						}))
				})

				It("creates a desired LRP for the missing app", func() {
					Eventually(bbsClient.DesireLRPCallCount).Should(Equal(1))
					Consistently(bbsClient.DesireLRPCallCount).Should(Equal(1))
					Expect(bbsClient.DesireLRPArgsForCall(0).ProcessGuid).To(Equal("new-process-guid"))
				})

				Context("when fetching desire app requests from the CC fails", func() {
					BeforeEach(func() {
						fetcher.FetchDesiredAppsStub = func(
							logger lager.Logger,
							cancel <-chan struct{},
							httpClient *http.Client,
							fingerprints <-chan []cc_messages.CCDesiredAppFingerprint,
						) (<-chan []cc_messages.DesireAppRequestFromCC, <-chan error) {
							desireAppRequests := make(chan []cc_messages.DesireAppRequestFromCC)
							close(desireAppRequests)

							<-fingerprints

							errorsChan := make(chan error, 1)
							errorsChan <- errors.New("boom")
							close(errorsChan)

							return desireAppRequests, errorsChan
						}
					})

					It("keeps calm and carries on", func() {
						Consistently(process.Wait()).ShouldNot(Receive())
					})

					It("does not update the domain", func() {
						Consistently(bbsClient.UpsertDomainCallCount).Should(Equal(0))
					})

					Context("and the differ provides creates, updates, and deletes", func() {
						It("sends the deletes but not the creates or updates", func() {
							Consistently(bbsClient.DesireLRPCallCount).Should(Equal(0))
							Consistently(bbsClient.UpdateDesiredLRPCallCount).Should(Equal(0))

							Eventually(bbsClient.RemoveDesiredLRPCallCount).Should(Equal(1))
							Consistently(bbsClient.RemoveDesiredLRPCallCount).Should(Equal(1))
							Expect(bbsClient.RemoveDesiredLRPArgsForCall(0)).To(Equal("excess-process-guid"))
						})
					})
				})

				Context("when building the desire LRP request fails", func() {
					BeforeEach(func() {
						buildpackRecipeBuilder.BuildReturns(nil, errors.New("nope"))
					})

					It("keeps calm and carries on", func() {
						Consistently(process.Wait()).ShouldNot(Receive())
					})

					It("does not update the domain", func() {
						Consistently(bbsClient.UpsertDomainCallCount).Should(Equal(0))
					})

					Context("and the differ provides creates, updates, and deletes", func() {
						It("continues to send the deletes and updates", func() {
							Eventually(bbsClient.RemoveDesiredLRPCallCount).Should(Equal(1))
							Consistently(bbsClient.RemoveDesiredLRPCallCount).Should(Equal(1))
							Expect(bbsClient.RemoveDesiredLRPArgsForCall(0)).To(Equal("excess-process-guid"))

							Eventually(bbsClient.UpdateDesiredLRPCallCount).Should(Equal(2))
							Consistently(bbsClient.UpdateDesiredLRPCallCount).Should(Equal(2))

							updatedGuid1, _ := bbsClient.UpdateDesiredLRPArgsForCall(0)
							updatedGuid2, _ := bbsClient.UpdateDesiredLRPArgsForCall(1)
							Expect([]string{updatedGuid1, updatedGuid2}).To(ConsistOf("stale-process-guid", "docker-process-guid"))
						})
					})
				})

				Context("when creating the missing desired LRP fails", func() {
					BeforeEach(func() {
						bbsClient.DesireLRPReturns(errors.New("nope"))
					})

					It("keeps calm and carries on", func() {
						Consistently(process.Wait()).ShouldNot(Receive())
					})

					It("does not update the domain", func() {
						Consistently(bbsClient.UpsertDomainCallCount).Should(Equal(0))
					})

					Context("and the differ provides creates, updates, and deletes", func() {
						It("continues to send the deletes and updates", func() {
							Eventually(bbsClient.RemoveDesiredLRPCallCount).Should(Equal(1))
							Consistently(bbsClient.RemoveDesiredLRPCallCount).Should(Equal(1))
							Expect(bbsClient.RemoveDesiredLRPArgsForCall(0)).To(Equal("excess-process-guid"))

							Eventually(bbsClient.UpdateDesiredLRPCallCount).Should(Equal(2))
							Consistently(bbsClient.UpdateDesiredLRPCallCount).Should(Equal(2))

							updatedGuid1, _ := bbsClient.UpdateDesiredLRPArgsForCall(0)
							updatedGuid2, _ := bbsClient.UpdateDesiredLRPArgsForCall(1)
							Expect([]string{updatedGuid1, updatedGuid2}).To(ConsistOf("stale-process-guid", "docker-process-guid"))
						})
					})
				})
			})

			Context("and the differ provides creates and deletes", func() {
				It("sends them to the bbs and updates the domain", func() {
					Eventually(bbsClient.DesireLRPCallCount).Should(Equal(1))
					Eventually(bbsClient.RemoveDesiredLRPCallCount).Should(Equal(1))
					Eventually(bbsClient.UpsertDomainCallCount).Should(Equal(1))

					Expect(bbsClient.DesireLRPArgsForCall(0)).To(BeEquivalentTo(&models.DesiredLRP{
						ProcessGuid: "new-process-guid",
						Annotation:  "new-etag",
					}))

					Expect(bbsClient.RemoveDesiredLRPArgsForCall(0)).To(Equal("excess-process-guid"))

					d, ttl := bbsClient.UpsertDomainArgsForCall(0)
					Expect(d).To(Equal("cf-apps"))
					Expect(ttl).To(Equal(1 * time.Second))
				})

				Context("and the create request fails", func() {
					BeforeEach(func() {
						bbsClient.DesireLRPReturns(errors.New("create failed!"))
					})

					It("does not update the domain", func() {
						Consistently(bbsClient.UpsertDomainCallCount).Should(Equal(0))
					})

					It("sends all the other updates", func() {
						Eventually(bbsClient.DesireLRPCallCount).Should(Equal(1))
						Eventually(bbsClient.RemoveDesiredLRPCallCount).Should(Equal(1))
					})
				})

				Context("and the delete request fails", func() {
					BeforeEach(func() {
						bbsClient.RemoveDesiredLRPReturns(errors.New("delete failed!"))
					})

					It("sends all the other updates", func() {
						Eventually(bbsClient.DesireLRPCallCount).Should(Equal(1))
						Eventually(bbsClient.RemoveDesiredLRPCallCount).Should(Equal(1))
						Eventually(bbsClient.UpsertDomainCallCount).Should(Equal(1))
					})
				})
			})

			Context("and the differ detects stale lrps", func() {
				var (
					expectedEtag      = "new-etag"
					expectedInstances = int32(0)

					expectedRouteHost   string
					expectedPort        uint32
					expectedRoutingInfo *models.Routes

					expectedClientCallCount int

					processGuids []string
					updateReqs   []*models.DesiredLRPUpdate
				)

				BeforeEach(func() {
					expectedPort = 8080
					expectedRouteHost = "host-stale-process-guid"
					expectedClientCallCount = 2
				})

				JustBeforeEach(func() {
					Eventually(bbsClient.UpdateDesiredLRPCallCount).Should(Equal(expectedClientCallCount))

					opaqueRouteMessage := json.RawMessage([]byte(`{ "some-route-key": "some-route-value" }`))
					cfRoute := cfroutes.CFRoutes{
						{Hostnames: []string{expectedRouteHost}, Port: expectedPort},
					}
					cfRoutePayload, err := json.Marshal(cfRoute)
					Expect(err).NotTo(HaveOccurred())
					cfRouteMessage := json.RawMessage(cfRoutePayload)

					tcpRouteMessage := json.RawMessage([]byte(`[]`))

					expectedRoutingInfo = &models.Routes{
						"router-route-data":   &opaqueRouteMessage,
						cfroutes.CF_ROUTER:    &cfRouteMessage,
						tcp_routes.TCP_ROUTER: &tcpRouteMessage,
					}

					for i := 0; i < expectedClientCallCount; i++ {
						processGuid, updateReq := bbsClient.UpdateDesiredLRPArgsForCall(i)
						processGuids = append(processGuids, processGuid)
						updateReqs = append(updateReqs, updateReq)
					}
				})

				It("sends the correct update desired lrp request", func() {
					Expect(processGuids).To(ContainElement("stale-process-guid"))
					Expect(updateReqs).To(ContainElement(&models.DesiredLRPUpdate{
						Annotation: &expectedEtag,
						Instances:  &expectedInstances,
						Routes:     expectedRoutingInfo,
					}))
				})

				Context("with exposed docker port", func() {
					BeforeEach(func() {
						expectedRouteHost = "host-docker-process-guid"
						expectedPort = 7070

						dockerRecipeBuilder.ExtractExposedPortsStub = func(ccRequest *cc_messages.DesireAppRequestFromCC) ([]uint32, error) {
							return []uint32{expectedPort}, nil
						}
					})

					It("sends the correct port in the desired lrp request", func() {
						Expect(processGuids).To(ContainElement("docker-process-guid"))
						Expect(updateReqs).To(ContainElement(&models.DesiredLRPUpdate{
							Annotation: &expectedEtag,
							Instances:  &expectedInstances,
							Routes:     expectedRoutingInfo,
						}))
					})
				})

				Context("with incorrect docker port", func() {
					BeforeEach(func() {
						expectedClientCallCount = 1
						dockerRecipeBuilder.ExtractExposedPortsStub = func(ccRequest *cc_messages.DesireAppRequestFromCC) ([]uint32, error) {
							return nil, errors.New("our-specific-test-error")
						}
					})

					It("sends the correct update desired lrp request", func() {
						Expect(processGuids).To(ContainElement("stale-process-guid"))
						Expect(updateReqs).To(ContainElement(&models.DesiredLRPUpdate{
							Annotation: &expectedEtag,
							Instances:  &expectedInstances,
							Routes:     expectedRoutingInfo,
						}))
					})

					It("logs an error for the incorrect docker port", func() {
						Eventually(logger.TestSink.Buffer).Should(gbytes.Say(`"data":{"error":"our-specific-test-error","execution-metadata":"","process-guid":"docker-process-guid"`))
					})

					It("propagates the error", func() {
						Eventually(logger.TestSink.Buffer).Should(gbytes.Say(`sync.not-bumping-freshness-because-of","log_level":2,"data":{"error":"our-specific-test-error"`))
					})
				})
			})

			Context("when updating the desired lrp fails", func() {
				BeforeEach(func() {
					bbsClient.UpdateDesiredLRPReturns(errors.New("boom"))
				})

				Context("because the desired lrp is invalid", func() {
					BeforeEach(func() {
						validationError := models.NewError(models.Error_InvalidRequest, "some-validation-error")
						bbsClient.UpdateDesiredLRPReturns(validationError.ToError())
					})

					It("updates the domain", func() {
						Eventually(bbsClient.UpsertDomainCallCount).Should(Equal(1))
					})

					It("correctly emits the total number of invalid LRPs found while bulking", func() {
						Eventually(func() fake.Metric {
							return metricSender.GetValue("NsyncInvalidDesiredLRPsFound")
						}).Should(Equal(fake.Metric{Value: 2, Unit: "Metric"}))
					})
				})

				It("does not update the domain", func() {
					Consistently(bbsClient.UpsertDomainCallCount).Should(Equal(0))
				})

				It("sends all the other updates", func() {
					Eventually(bbsClient.DesireLRPCallCount).Should(Equal(1))
					Eventually(bbsClient.RemoveDesiredLRPCallCount).Should(Equal(1))
				})
			})

			Context("when creating the desired lrp fails", func() {
				BeforeEach(func() {
					bbsClient.DesireLRPReturns(errors.New("boom"))
				})

				Context("because the desired lrp is invalid", func() {
					BeforeEach(func() {
						validationError := models.NewError(models.Error_InvalidRequest, "some-validation-error")
						bbsClient.DesireLRPReturns(validationError.ToError())
					})

					It("updates the domain", func() {
						Eventually(bbsClient.UpsertDomainCallCount).Should(Equal(1))
					})

					It("correctly emits the total number of invalid LRPs found while bulking", func() {
						Eventually(func() fake.Metric {
							return metricSender.GetValue("NsyncInvalidDesiredLRPsFound")
						}).Should(Equal(fake.Metric{Value: 1, Unit: "Metric"}))
					})
				})

				It("does not update the domain", func() {
					Consistently(bbsClient.UpsertDomainCallCount).Should(Equal(0))
				})

				It("sends all the other updates", func() {
					Eventually(bbsClient.DesireLRPCallCount).Should(Equal(1))
					Eventually(bbsClient.RemoveDesiredLRPCallCount).Should(Equal(1))
				})
			})
		})

		Context("tasks", func() {
			Context("when bbs does not know about a running task", func() {
				BeforeEach(func() {
					taskStatesToFetch = []cc_messages.CCTaskState{
						{TaskGuid: "task-guid-1", State: cc_messages.TaskStateRunning, CompletionCallbackUrl: "asdf"},
					}
				})

				It("fails the task", func() {
					Eventually(taskClient.FailTaskCallCount).Should(Equal(1))
					_, taskState, _ := taskClient.FailTaskArgsForCall(0)
					Expect(taskState.TaskGuid).Should(Equal("task-guid-1"))
					Expect(taskState.CompletionCallbackUrl).Should(Equal("asdf"))
				})

				Context("and failing the task fails", func() {
					BeforeEach(func() {
						taskClient.FailTaskReturns(errors.New("nope"))
					})

					It("does not update the domain", func() {
						Consistently(bbsClient.UpsertDomainCallCount).Should(Equal(0))
					})

					It("sends all the other updates", func() {
						Eventually(bbsClient.DesireLRPCallCount).Should(Equal(1))
						Eventually(bbsClient.RemoveDesiredLRPCallCount).Should(Equal(1))
					})
				})
			})

			Context("when bbs does not know about a pending task", func() {
				BeforeEach(func() {
					taskStatesToFetch = []cc_messages.CCTaskState{
						{TaskGuid: "task-guid-1", State: cc_messages.TaskStatePending},
					}
				})

				It("does not fail the task", func() {
					Consistently(taskClient.FailTaskCallCount).Should(Equal(0))
				})
			})

			Context("when bbs does not know about a completed task", func() {
				BeforeEach(func() {
					taskStatesToFetch = []cc_messages.CCTaskState{
						{TaskGuid: "task-guid-1", State: cc_messages.TaskStateSucceeded},
					}
				})

				It("does not fail the task", func() {
					Consistently(taskClient.FailTaskCallCount).Should(Equal(0))
				})
			})

			Context("when bbs does not know about a canceling task", func() {
				BeforeEach(func() {
					taskStatesToFetch = []cc_messages.CCTaskState{
						{TaskGuid: "task-guid-1", State: cc_messages.TaskStateRunning, CompletionCallbackUrl: "asdf"},
					}
				})

				It("fails the task", func() {
					Eventually(taskClient.FailTaskCallCount).Should(Equal(1))
					_, taskState, _ := taskClient.FailTaskArgsForCall(0)
					Expect(taskState.TaskGuid).Should(Equal("task-guid-1"))
					Expect(taskState.CompletionCallbackUrl).Should(Equal("asdf"))
				})

				Context("and failing the task fails", func() {
					BeforeEach(func() {
						taskClient.FailTaskReturns(errors.New("nope"))
					})

					It("does not update the domain", func() {
						Consistently(bbsClient.UpsertDomainCallCount).Should(Equal(0))
					})

					It("sends all the other updates", func() {
						Eventually(bbsClient.DesireLRPCallCount).Should(Equal(1))
						Eventually(bbsClient.RemoveDesiredLRPCallCount).Should(Equal(1))
					})
				})
			})

			Context("when bbs knows about a running task", func() {
				BeforeEach(func() {
					taskStatesToFetch = []cc_messages.CCTaskState{
						{TaskGuid: "task-guid-1", State: cc_messages.TaskStateRunning},
					}

					bbsClient.TasksByDomainReturns([]*models.Task{{TaskGuid: "task-guid-1"}}, nil)
				})

				It("does not fail the task", func() {
					Consistently(taskClient.FailTaskCallCount).Should(Equal(0))
				})
			})
		})
	})

	Context("when getting all desired LRPs fails", func() {
		BeforeEach(func() {
			bbsClient.DesiredLRPSchedulingInfosReturns(nil, errors.New("oh no!"))
		})

		It("keeps calm and carries on", func() {
			Consistently(process.Wait()).ShouldNot(Receive())
		})

		It("tries again after the polling interval", func() {
			Eventually(bbsClient.DesiredLRPSchedulingInfosCallCount).Should(Equal(1))
			clock.Increment(pollingInterval / 2)
			Consistently(bbsClient.DesiredLRPSchedulingInfosCallCount).Should(Equal(1))

			clock.Increment(pollingInterval)
			Eventually(bbsClient.DesiredLRPSchedulingInfosCallCount).Should(Equal(2))
		})

		It("does not call the differ, the fetcher, or the bbs client for updates", func() {
			Consistently(fetcher.FetchFingerprintsCallCount).Should(Equal(0))
			Consistently(fetcher.FetchDesiredAppsCallCount).Should(Equal(0))
			Consistently(buildpackRecipeBuilder.BuildCallCount).Should(Equal(0))
			Consistently(bbsClient.DesireLRPCallCount).Should(Equal(0))
			Consistently(bbsClient.RemoveDesiredLRPCallCount).Should(Equal(0))
			Consistently(bbsClient.UpdateDesiredLRPCallCount).Should(Equal(0))
			Consistently(bbsClient.UpsertDomainCallCount).Should(Equal(0))
		})
	})

	Context("when fetching fingerprints fails", func() {
		BeforeEach(func() {
			fetcher.FetchFingerprintsStub = func(
				logger lager.Logger,
				cancel <-chan struct{},
				httpClient *http.Client,
			) (<-chan []cc_messages.CCDesiredAppFingerprint, <-chan error) {
				results := make(chan []cc_messages.CCDesiredAppFingerprint, 1)
				errorsChan := make(chan error, 1)

				results <- fingerprintsToFetch
				close(results)

				errorsChan <- errors.New("uh oh")
				close(errorsChan)

				return results, errorsChan
			}
		})

		It("keeps calm and carries on", func() {
			Consistently(process.Wait()).ShouldNot(Receive())
		})

		It("does not update the domain", func() {
			Consistently(bbsClient.UpsertDomainCallCount).Should(Equal(0))
		})

		It("sends the creates and updates for the apps it got but not the deletes", func() {
			Eventually(bbsClient.DesireLRPCallCount).Should(Equal(1))
			Eventually(bbsClient.UpdateDesiredLRPCallCount).Should(Equal(2))
			Consistently(bbsClient.RemoveDesiredLRPCallCount).Should(Equal(0))
		})
	})

	Context("when fetching task states fails", func() {
		BeforeEach(func() {
			fetcher.FetchTaskStatesStub = func(
				logger lager.Logger,
				cancel <-chan struct{},
				httpClient *http.Client,
			) (<-chan []cc_messages.CCTaskState, <-chan error) {
				results := make(chan []cc_messages.CCTaskState, 1)
				errorsChan := make(chan error, 1)

				close(results)

				errorsChan <- errors.New("uh oh")
				close(errorsChan)

				return results, errorsChan
			}
		})

		It("keeps calm and carries on", func() {
			Consistently(process.Wait()).ShouldNot(Receive())
		})

		It("does not update the domain", func() {
			Consistently(bbsClient.UpsertDomainCallCount).Should(Equal(0))
		})

		It("doesn't fail any tasks", func() {
			Consistently(taskClient.FailTaskCallCount).Should(Equal(0))
		})
	})

	Context("when getting all tasks fails", func() {
		BeforeEach(func() {
			bbsClient.TasksByDomainReturns(nil, errors.New("oh no!"))
		})

		It("keeps calm and carries on", func() {
			Consistently(process.Wait()).ShouldNot(Receive())
		})

		It("tries again after the polling interval", func() {
			Eventually(bbsClient.TasksByDomainCallCount).Should(Equal(1))
			clock.Increment(pollingInterval / 2)
			Consistently(bbsClient.TasksByDomainCallCount).Should(Equal(1))

			clock.Increment(pollingInterval)
			Eventually(bbsClient.TasksByDomainCallCount).Should(Equal(2))
		})

		It("does not call the the fetcher, or the task client for updates", func() {
			Consistently(fetcher.FetchTaskStatesCallCount).Should(Equal(0))
			Consistently(taskClient.FailTaskCallCount).Should(Equal(0))
		})
	})
})
