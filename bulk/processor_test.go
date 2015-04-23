package bulk_test

import (
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"time"

	"github.com/cloudfoundry-incubator/nsync/bulk"
	"github.com/cloudfoundry-incubator/nsync/bulk/fakes"
	"github.com/cloudfoundry-incubator/receptor"
	"github.com/cloudfoundry-incubator/receptor/fake_receptor"
	"github.com/cloudfoundry-incubator/route-emitter/cfroutes"
	"github.com/cloudfoundry-incubator/runtime-schema/cc_messages"
	"github.com/cloudfoundry/dropsonde/metric_sender/fake"
	"github.com/cloudfoundry/dropsonde/metrics"
	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
	"github.com/pivotal-golang/clock/fakeclock"
	"github.com/pivotal-golang/lager"
	"github.com/pivotal-golang/lager/lagertest"
	"github.com/tedsuo/ifrit"
)

var _ = Describe("Processor", func() {
	var (
		fingerprintsToFetch []cc_messages.CCDesiredAppFingerprint
		existingDesired     []receptor.DesiredLRPResponse

		receptorClient *fake_receptor.FakeClient
		fetcher        *fakes.FakeFetcher
		recipeBuilder  *fakes.FakeRecipeBuilder

		processor ifrit.Runner

		process      ifrit.Process
		syncDuration time.Duration
		metricSender *fake.FakeMetricSender
		clock        *fakeclock.FakeClock

		pollingInterval time.Duration
	)

	BeforeEach(func() {
		metricSender = fake.NewFakeMetricSender()
		metrics.Initialize(metricSender)

		syncDuration = 900900
		pollingInterval = 500 * time.Millisecond
		clock = fakeclock.NewFakeClock(time.Now())

		fingerprintsToFetch = []cc_messages.CCDesiredAppFingerprint{
			{ProcessGuid: "current-process-guid", ETag: "current-etag"},
			{ProcessGuid: "stale-process-guid", ETag: "new-etag"},
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
				results = append(results, lrp)
			}

			desired := make(chan []cc_messages.DesireAppRequestFromCC, 1)
			desired <- results
			close(desired)

			errors := make(chan error, 1)
			close(errors)

			return desired, errors
		}

		recipeBuilder = new(fakes.FakeRecipeBuilder)
		recipeBuilder.BuildStub = func(ccRequest *cc_messages.DesireAppRequestFromCC) (*receptor.DesiredLRPCreateRequest, error) {
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

		processor = bulk.NewProcessor(
			receptorClient,
			500*time.Millisecond,
			time.Second,
			10,
			false,
			lagertest.NewTestLogger("test"),
			fetcher,
			recipeBuilder,
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
			Consistently(recipeBuilder.BuildCallCount).Should(Equal(0))
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
			Eventually(receptorClient.UpdateDesiredLRPCallCount).Should(Equal(1))
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

				Ω(receptorClient.DeleteDesiredLRPArgsForCall(0)).Should(Equal("excess-process-guid"))
			})
		})

		Context("and the differ discovers missing apps", func() {
			It("uses the recipe builder to construct the create LRP request", func() {
				Eventually(recipeBuilder.BuildCallCount).Should(Equal(1))
				Consistently(recipeBuilder.BuildCallCount).Should(Equal(1))

				Eventually(recipeBuilder.BuildArgsForCall(0)).Should(Equal(
					&cc_messages.DesireAppRequestFromCC{
						ProcessGuid: "new-process-guid",
						ETag:        "new-etag",
						Routes:      []string{"host-new-process-guid"},
					}))
			})

			It("creates a desired LRP for the missing app", func() {
				Eventually(receptorClient.CreateDesiredLRPCallCount).Should(Equal(1))
				Consistently(receptorClient.CreateDesiredLRPCallCount).Should(Equal(1))
				Ω(receptorClient.CreateDesiredLRPArgsForCall(0).ProcessGuid).Should(Equal("new-process-guid"))
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
						Ω(receptorClient.DeleteDesiredLRPArgsForCall(0)).Should(Equal("excess-process-guid"))
					})
				})
			})

			Context("when building the desire LRP request fails", func() {
				BeforeEach(func() {
					recipeBuilder.BuildReturns(nil, errors.New("nope"))
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
						Ω(receptorClient.DeleteDesiredLRPArgsForCall(0)).Should(Equal("excess-process-guid"))

						Eventually(receptorClient.UpdateDesiredLRPCallCount).Should(Equal(1))
						Consistently(receptorClient.UpdateDesiredLRPCallCount).Should(Equal(1))

						updatedGuid, _ := receptorClient.UpdateDesiredLRPArgsForCall(0)
						Ω(updatedGuid).Should(Equal("stale-process-guid"))
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
						Ω(receptorClient.DeleteDesiredLRPArgsForCall(0)).Should(Equal("excess-process-guid"))

						Eventually(receptorClient.UpdateDesiredLRPCallCount).Should(Equal(1))
						Consistently(receptorClient.UpdateDesiredLRPCallCount).Should(Equal(1))

						updatedGuid, _ := receptorClient.UpdateDesiredLRPArgsForCall(0)
						Ω(updatedGuid).Should(Equal("stale-process-guid"))
					})
				})
			})
		})

		Context("and the differ provides creates and deletes", func() {
			It("sends them to the receptor and updates the domain", func() {
				Eventually(receptorClient.CreateDesiredLRPCallCount).Should(Equal(1))
				Eventually(receptorClient.DeleteDesiredLRPCallCount).Should(Equal(1))
				Eventually(receptorClient.UpsertDomainCallCount).Should(Equal(1))

				Ω(receptorClient.CreateDesiredLRPArgsForCall(0)).Should(Equal(receptor.DesiredLRPCreateRequest{
					ProcessGuid: "new-process-guid",
					Annotation:  "new-etag",
				}))
				Ω(receptorClient.DeleteDesiredLRPArgsForCall(0)).Should(Equal("excess-process-guid"))

				d, ttl := receptorClient.UpsertDomainArgsForCall(0)
				Ω(d).Should(Equal("cf-apps"))
				Ω(ttl).Should(Equal(1 * time.Second))
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
			It("sends the correct update desired lrp request", func() {
				Eventually(receptorClient.UpdateDesiredLRPCallCount).Should(Equal(1))

				expectedEtag := "new-etag"
				expectedInstances := 0
				opaqueRouteMessage := json.RawMessage([]byte(`{ "some-route-key": "some-route-value" }`))
				cfRoute := cfroutes.CFRoutes{
					{Hostnames: []string{"host-stale-process-guid"}, Port: 8080},
				}
				cfRoutePayload, err := json.Marshal(cfRoute)
				Ω(err).ShouldNot(HaveOccurred())
				cfRouteMessage := json.RawMessage(cfRoutePayload)

				expectedRoutingInfo := receptor.RoutingInfo{
					"router-route-data": &opaqueRouteMessage,
					cfroutes.CF_ROUTER:  &cfRouteMessage,
				}

				processGuid, updateReq := receptorClient.UpdateDesiredLRPArgsForCall(0)
				Ω(processGuid).Should(Equal("stale-process-guid"))
				Ω(updateReq).Should(Equal(receptor.DesiredLRPUpdateRequest{
					Annotation: &expectedEtag,
					Instances:  &expectedInstances,
					Routes:     expectedRoutingInfo,
				}))
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
})
