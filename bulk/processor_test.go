package bulk_test

import (
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/cloudfoundry-incubator/nsync/bulk"
	"github.com/cloudfoundry-incubator/nsync/bulk/fakes"
	"github.com/cloudfoundry-incubator/nsync/recipebuilder"
	"github.com/cloudfoundry-incubator/receptor"
	"github.com/cloudfoundry-incubator/receptor/fake_receptor"
	"github.com/cloudfoundry-incubator/route-emitter/cfroutes"
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
)

var _ = Describe("Processor", func() {
	var (
		fingerprintsToFetch []cc_messages.CCDesiredAppFingerprint
		existingDesired     []receptor.DesiredLRPResponse

		receptorClient         *fake_receptor.FakeClient
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

		staleRouteMessage := json.RawMessage([]byte(`{ "some-route-key": "some-route-value" }`))
		existingDesired = []receptor.DesiredLRPResponse{
			{ProcessGuid: "current-process-guid", Annotation: "current-etag"},
			{
				ProcessGuid: "stale-process-guid",
				Annotation:  "stale-etag",
				Routes: receptor.RoutingInfo{
					"router-route-data": &staleRouteMessage,
				},
			},
			{
				ProcessGuid: "docker-process-guid",
				Annotation:  "docker-etag",
				Routes: receptor.RoutingInfo{
					"router-route-data": &staleRouteMessage,
				},
			},
			{ProcessGuid: "excess-process-guid", Annotation: "excess-etag"},
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
				lrp := cc_messages.DesireAppRequestFromCC{
					ProcessGuid: fingerprint.ProcessGuid,
					ETag:        fingerprint.ETag,
					Routes:      []string{"host-" + fingerprint.ProcessGuid},
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

		buildpackRecipeBuilder = new(fakes.FakeRecipeBuilder)
		buildpackRecipeBuilder.BuildStub = func(ccRequest *cc_messages.DesireAppRequestFromCC) (*receptor.DesiredLRPCreateRequest, error) {
			createRequest := receptor.DesiredLRPCreateRequest{
				ProcessGuid: ccRequest.ProcessGuid,
				Annotation:  ccRequest.ETag,
			}
			return &createRequest, nil
		}
		buildpackRecipeBuilder.ExtractExposedPortStub = func(desiredAppMetadata string) (uint16, error) {
			return 8080, nil
		}

		dockerRecipeBuilder = new(fakes.FakeRecipeBuilder)
		dockerRecipeBuilder.BuildStub = func(ccRequest *cc_messages.DesireAppRequestFromCC) (*receptor.DesiredLRPCreateRequest, error) {
			createRequest := receptor.DesiredLRPCreateRequest{
				ProcessGuid: ccRequest.ProcessGuid,
				Annotation:  ccRequest.ETag,
			}
			return &createRequest, nil
		}

		receptorClient = new(fake_receptor.FakeClient)
		receptorClient.DesiredLRPsByDomainReturns(existingDesired, nil)

		receptorClient.UpsertDomainStub = func(string, time.Duration) error {
			clock.Increment(syncDuration)
			return nil
		}

		logger = lagertest.NewTestLogger("test")

		processor = bulk.NewProcessor(
			receptorClient,
			500*time.Millisecond,
			time.Second,
			10,
			false,
			logger,
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
		process.Signal(os.Interrupt)
		Eventually(process.Wait()).Should(Receive())
	})

	Describe("when getting all desired LRPs fails", func() {
		BeforeEach(func() {
			receptorClient.DesiredLRPsByDomainReturns(nil, errors.New("oh no!"))
		})

		It("keeps calm and carries on", func() {
			Consistently(process.Wait()).ShouldNot(Receive())
		})

		It("tries again after the polling interval", func() {
			clock.Increment(pollingInterval / 2)
			Consistently(receptorClient.DesiredLRPsByDomainCallCount).Should(Equal(1))

			clock.Increment(pollingInterval)
			Eventually(receptorClient.DesiredLRPsByDomainCallCount).Should(Equal(2))
		})

		It("does not call the differ, the fetcher, or the receptor client for updates", func() {
			Consistently(fetcher.FetchFingerprintsCallCount).Should(Equal(0))
			Consistently(fetcher.FetchDesiredAppsCallCount).Should(Equal(0))
			Consistently(buildpackRecipeBuilder.BuildCallCount).Should(Equal(0))
			Consistently(receptorClient.CreateDesiredLRPCallCount).Should(Equal(0))
			Consistently(receptorClient.DeleteDesiredLRPCallCount).Should(Equal(0))
			Consistently(receptorClient.UpdateDesiredLRPCallCount).Should(Equal(0))
			Consistently(receptorClient.UpsertDomainCallCount).Should(Equal(0))
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
			Consistently(receptorClient.UpsertDomainCallCount).Should(Equal(0))
		})

		It("sends the creates and updates for the apps it got but not the deletes", func() {
			Eventually(receptorClient.CreateDesiredLRPCallCount).Should(Equal(1))
			Eventually(receptorClient.UpdateDesiredLRPCallCount).Should(Equal(2))
			Consistently(receptorClient.DeleteDesiredLRPCallCount).Should(Equal(0))
		})
	})

	Context("when fetching fingerprints succeeds", func() {
		It("emits the total time taken to talk to CC and then update desired state", func() {
			Eventually(receptorClient.UpsertDomainCallCount, 5).Should(Equal(1))

			Eventually(func() fake.Metric { return metricSender.GetValue("DesiredLRPSyncDuration") }).Should(Equal(fake.Metric{
				Value: float64(syncDuration),
				Unit:  "nanos",
			}))
		})

		Context("and the differ discovers desired LRPs to delete", func() {
			It("the processor deletes them", func() {
				Eventually(receptorClient.DeleteDesiredLRPCallCount).Should(Equal(1))
				Consistently(receptorClient.DeleteDesiredLRPCallCount).Should(Equal(1))

				Expect(receptorClient.DeleteDesiredLRPArgsForCall(0)).To(Equal("excess-process-guid"))
			})
		})

		Context("and the differ discovers missing apps", func() {
			It("uses the recipe builder to construct the create LRP request", func() {
				Eventually(buildpackRecipeBuilder.BuildCallCount).Should(Equal(1))
				Consistently(buildpackRecipeBuilder.BuildCallCount).Should(Equal(1))

				Eventually(buildpackRecipeBuilder.BuildArgsForCall(0)).Should(Equal(
					&cc_messages.DesireAppRequestFromCC{
						ProcessGuid: "new-process-guid",
						ETag:        "new-etag",
						Routes:      []string{"host-new-process-guid"},
					}))
			})

			It("creates a desired LRP for the missing app", func() {
				Eventually(receptorClient.CreateDesiredLRPCallCount).Should(Equal(1))
				Consistently(receptorClient.CreateDesiredLRPCallCount).Should(Equal(1))
				Expect(receptorClient.CreateDesiredLRPArgsForCall(0).ProcessGuid).To(Equal("new-process-guid"))
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
					Consistently(receptorClient.UpsertDomainCallCount).Should(Equal(0))
				})

				Context("and the differ provides creates, updates, and deletes", func() {
					It("sends the deletes but not the creates or updates", func() {
						Consistently(receptorClient.CreateDesiredLRPCallCount).Should(Equal(0))
						Consistently(receptorClient.UpdateDesiredLRPCallCount).Should(Equal(0))

						Eventually(receptorClient.DeleteDesiredLRPCallCount).Should(Equal(1))
						Consistently(receptorClient.DeleteDesiredLRPCallCount).Should(Equal(1))
						Expect(receptorClient.DeleteDesiredLRPArgsForCall(0)).To(Equal("excess-process-guid"))
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
					Consistently(receptorClient.UpsertDomainCallCount).Should(Equal(0))
				})

				Context("and the differ provides creates, updates, and deletes", func() {
					It("continues to send the deletes and updates", func() {
						Eventually(receptorClient.DeleteDesiredLRPCallCount).Should(Equal(1))
						Consistently(receptorClient.DeleteDesiredLRPCallCount).Should(Equal(1))
						Expect(receptorClient.DeleteDesiredLRPArgsForCall(0)).To(Equal("excess-process-guid"))

						Eventually(receptorClient.UpdateDesiredLRPCallCount).Should(Equal(2))
						Consistently(receptorClient.UpdateDesiredLRPCallCount).Should(Equal(2))

						updatedGuid, _ := receptorClient.UpdateDesiredLRPArgsForCall(0)
						Expect(updatedGuid).To(Equal("stale-process-guid"))
					})
				})
			})

			Context("when creating the missing desired LRP fails", func() {
				BeforeEach(func() {
					receptorClient.CreateDesiredLRPReturns(errors.New("nope"))
				})

				It("keeps calm and carries on", func() {
					Consistently(process.Wait()).ShouldNot(Receive())
				})

				It("does not update the domain", func() {
					Consistently(receptorClient.UpsertDomainCallCount).Should(Equal(0))
				})

				Context("and the differ provides creates, updates, and deletes", func() {
					It("continues to send the deletes and updates", func() {
						Eventually(receptorClient.DeleteDesiredLRPCallCount).Should(Equal(1))
						Consistently(receptorClient.DeleteDesiredLRPCallCount).Should(Equal(1))
						Expect(receptorClient.DeleteDesiredLRPArgsForCall(0)).To(Equal("excess-process-guid"))

						Eventually(receptorClient.UpdateDesiredLRPCallCount).Should(Equal(2))
						Consistently(receptorClient.UpdateDesiredLRPCallCount).Should(Equal(2))

						updatedGuid, _ := receptorClient.UpdateDesiredLRPArgsForCall(0)
						Expect(updatedGuid).To(Equal("stale-process-guid"))
					})
				})
			})
		})

		Context("and the differ provides creates and deletes", func() {
			It("sends them to the receptor and updates the domain", func() {
				Eventually(receptorClient.CreateDesiredLRPCallCount).Should(Equal(1))
				Eventually(receptorClient.DeleteDesiredLRPCallCount).Should(Equal(1))
				Eventually(receptorClient.UpsertDomainCallCount).Should(Equal(1))

				Expect(receptorClient.CreateDesiredLRPArgsForCall(0)).To(Equal(receptor.DesiredLRPCreateRequest{
					ProcessGuid: "new-process-guid",
					Annotation:  "new-etag",
				}))

				Expect(receptorClient.DeleteDesiredLRPArgsForCall(0)).To(Equal("excess-process-guid"))

				d, ttl := receptorClient.UpsertDomainArgsForCall(0)
				Expect(d).To(Equal("cf-apps"))
				Expect(ttl).To(Equal(1 * time.Second))
			})

			Context("and the create request fails", func() {
				BeforeEach(func() {
					receptorClient.CreateDesiredLRPReturns(errors.New("create failed!"))
				})

				It("does not update the domain", func() {
					Consistently(receptorClient.UpsertDomainCallCount).Should(Equal(0))
				})

				It("sends all the other updates", func() {
					Eventually(receptorClient.CreateDesiredLRPCallCount).Should(Equal(1))
					Eventually(receptorClient.DeleteDesiredLRPCallCount).Should(Equal(1))
				})
			})

			Context("and the delete request fails", func() {
				BeforeEach(func() {
					receptorClient.DeleteDesiredLRPReturns(errors.New("delete failed!"))
				})

				It("sends all the other updates", func() {
					Eventually(receptorClient.CreateDesiredLRPCallCount).Should(Equal(1))
					Eventually(receptorClient.DeleteDesiredLRPCallCount).Should(Equal(1))
					Eventually(receptorClient.UpsertDomainCallCount).Should(Equal(1))
				})
			})
		})

		Context("and the differ detects stale lrps", func() {
			var (
				expectedEtag      = "new-etag"
				expectedInstances = 0

				expectedRouteHost   string
				expectedPort        uint16
				expectedRoutingInfo receptor.RoutingInfo

				expectedClientCallCount int

				processGuids []string
				updateReqs   []receptor.DesiredLRPUpdateRequest
			)

			BeforeEach(func() {
				expectedPort = 8080
				expectedRouteHost = "host-stale-process-guid"
				expectedClientCallCount = 2
			})

			JustBeforeEach(func() {
				Eventually(receptorClient.UpdateDesiredLRPCallCount).Should(Equal(expectedClientCallCount))

				opaqueRouteMessage := json.RawMessage([]byte(`{ "some-route-key": "some-route-value" }`))
				cfRoute := cfroutes.CFRoutes{
					{Hostnames: []string{expectedRouteHost}, Port: expectedPort},
				}
				cfRoutePayload, err := json.Marshal(cfRoute)
				Expect(err).NotTo(HaveOccurred())
				cfRouteMessage := json.RawMessage(cfRoutePayload)

				expectedRoutingInfo = receptor.RoutingInfo{
					"router-route-data": &opaqueRouteMessage,
					cfroutes.CF_ROUTER:  &cfRouteMessage,
				}

				for i := 0; i < expectedClientCallCount; i++ {
					processGuid, updateReq := receptorClient.UpdateDesiredLRPArgsForCall(i)
					processGuids = append(processGuids, processGuid)
					updateReqs = append(updateReqs, updateReq)
				}
			})

			It("sends the correct update desired lrp request", func() {
				Expect(processGuids).To(ContainElement("stale-process-guid"))
				Expect(updateReqs).To(ContainElement(receptor.DesiredLRPUpdateRequest{
					Annotation: &expectedEtag,
					Instances:  &expectedInstances,
					Routes:     expectedRoutingInfo,
				}))
			})

			Context("with exposed docker port", func() {
				BeforeEach(func() {
					expectedRouteHost = "host-docker-process-guid"
					expectedPort = 7070

					dockerRecipeBuilder.ExtractExposedPortStub = func(desiredAppMetadata string) (uint16, error) {
						return expectedPort, nil
					}
				})

				It("sends the correct port in the desired lrp request", func() {
					Expect(processGuids).To(ContainElement("docker-process-guid"))
					Expect(updateReqs).To(ContainElement(receptor.DesiredLRPUpdateRequest{
						Annotation: &expectedEtag,
						Instances:  &expectedInstances,
						Routes:     expectedRoutingInfo,
					}))
				})
			})

			Context("with incorrect docker port", func() {
				BeforeEach(func() {
					expectedClientCallCount = 1
					dockerRecipeBuilder.ExtractExposedPortStub = func(desiredAppMetadata string) (uint16, error) {
						return 0, errors.New("our-specific-test-error")
					}
				})

				It("sends the correct update desired lrp request", func() {
					Expect(processGuids).To(ContainElement("stale-process-guid"))
					Expect(updateReqs).To(ContainElement(receptor.DesiredLRPUpdateRequest{
						Annotation: &expectedEtag,
						Instances:  &expectedInstances,
						Routes:     expectedRoutingInfo,
					}))
				})

				It("logs an error for the incorrect docker port", func() {
					Eventually(logger.TestSink.Buffer).Should(gbytes.Say(`"data":{"error":"our-specific-test-error","process-guid":"docker-process-guid"`))
				})

				It("propagates the error", func() {
					Eventually(logger.TestSink.Buffer).Should(gbytes.Say(`sync.not-bumping-freshness-because-of","log_level":2,"data":{"error":"our-specific-test-error"`))
				})
			})
		})

		Context("when updating the desired lrp fails", func() {
			BeforeEach(func() {
				receptorClient.UpdateDesiredLRPReturns(errors.New("boom"))
			})

			It("does not update the domain", func() {
				Consistently(receptorClient.UpsertDomainCallCount).Should(Equal(0))
			})

			It("sends all the other updates", func() {
				Eventually(receptorClient.CreateDesiredLRPCallCount).Should(Equal(1))
				Eventually(receptorClient.DeleteDesiredLRPCallCount).Should(Equal(1))
			})
		})

	})
})
