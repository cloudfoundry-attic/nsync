package bulk_test

import (
	"errors"
	"net/http"
	"os"
	"time"

	"github.com/cloudfoundry-incubator/nsync/bulk"
	"github.com/cloudfoundry-incubator/nsync/bulk/fakes"
	"github.com/cloudfoundry-incubator/receptor"
	"github.com/cloudfoundry-incubator/receptor/fake_receptor"
	"github.com/cloudfoundry-incubator/runtime-schema/cc_messages"
	"github.com/cloudfoundry/dropsonde/metric_sender/fake"
	"github.com/cloudfoundry/dropsonde/metrics"
	"github.com/cloudfoundry/gunk/timeprovider/faketimeprovider"
	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
	"github.com/pivotal-golang/lager"
	"github.com/tedsuo/ifrit"
)

var _ = Describe("Processor", func() {
	var (
		receptorClient *fake_receptor.FakeClient
		fetcher        *fakes.FakeFetcher
		differ         *fakes.FakeDiffer
		recipeBuilder  *fakes.FakeRecipeBuilder

		processor ifrit.Runner

		process      ifrit.Process
		syncDuration time.Duration
		metricSender *fake.FakeMetricSender
		timeProvider *faketimeprovider.FakeTimeProvider

		pollingInterval time.Duration
	)

	BeforeEach(func() {
		metricSender = fake.NewFakeMetricSender()
		metrics.Initialize(metricSender)

		syncDuration = 900900
		pollingInterval = 500 * time.Millisecond
		timeProvider = faketimeprovider.New(time.Now())

		fetcher = new(fakes.FakeFetcher)
		fetcher.FetchFingerprintsStub = func(
			logger lager.Logger,
			cancel <-chan struct{},
			results chan<- []cc_messages.CCDesiredAppFingerprint,
			httpClient *http.Client,
		) error {
			results <- []cc_messages.CCDesiredAppFingerprint{{}}
			close(results)
			return nil
		}

		differ = new(fakes.FakeDiffer)
		differ.DiffStub = func(
			logger lager.Logger,
			cancel <-chan struct{},
			existing []receptor.DesiredLRPResponse,
			desired <-chan []cc_messages.CCDesiredAppFingerprint,
			missing chan<- []cc_messages.CCDesiredAppFingerprint,
		) []string {
			<-desired

			missing <- []cc_messages.CCDesiredAppFingerprint{{}}
			close(missing)

			return []string{"my-app-to-delete"}
		}

		fetcher.FetchDesiredAppsStub = func(
			logger lager.Logger,
			cancel <-chan struct{},
			fingerprints <-chan []cc_messages.CCDesiredAppFingerprint,
			desired chan<- []cc_messages.DesireAppRequestFromCC,
			httpClient *http.Client,
		) error {
			<-fingerprints

			desired <- []cc_messages.DesireAppRequestFromCC{{}}
			close(desired)

			return nil
		}

		recipeBuilder = new(fakes.FakeRecipeBuilder)
		recipeBuilder.BuildStub = func(*cc_messages.DesireAppRequestFromCC) (*receptor.DesiredLRPCreateRequest, error) {
			return &receptor.DesiredLRPCreateRequest{}, nil
		}

		receptorClient = new(fake_receptor.FakeClient)
		receptorClient.UpsertDomainStub = func(string, time.Duration) error {
			timeProvider.Increment(syncDuration)
			return nil
		}

		processor = bulk.NewProcessor(
			receptorClient,
			pollingInterval,
			time.Second,
			time.Second,
			10,
			false,
			lager.NewLogger("test"),
			fetcher,
			differ,
			recipeBuilder,
			timeProvider,
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
			timeProvider.Increment(pollingInterval / 2)
			Consistently(receptorClient.DesiredLRPsByDomainCallCount).Should(Equal(1))

			timeProvider.Increment(pollingInterval)
			Eventually(receptorClient.DesiredLRPsByDomainCallCount).Should(Equal(2))
		})

		It("does not call the differ, the fetcher, or the receptor client for updates", func() {
			Consistently(fetcher.FetchFingerprintsCallCount).Should(Equal(0))
			Consistently(differ.DiffCallCount).Should(Equal(0))
			Consistently(fetcher.FetchDesiredAppsCallCount).Should(Equal(0))
			Consistently(recipeBuilder.BuildCallCount).Should(Equal(0))
			Consistently(receptorClient.CreateDesiredLRPCallCount).Should(Equal(0))
			Consistently(receptorClient.DeleteDesiredLRPCallCount).Should(Equal(0))
			Consistently(receptorClient.UpsertDomainCallCount).Should(Equal(0))
		})
	})

	Context("when fetching fingerprints fails", func() {
		BeforeEach(func() {
			fetcher.FetchFingerprintsStub = func(
				logger lager.Logger,
				cancel <-chan struct{},
				results chan<- []cc_messages.CCDesiredAppFingerprint,
				httpClient *http.Client,
			) error {
				results <- []cc_messages.CCDesiredAppFingerprint{{}}
				close(results)
				return errors.New("uh oh")
			}
		})

		It("keeps calm and carries on", func() {
			Consistently(process.Wait()).ShouldNot(Receive())
		})

		It("does not update the domain", func() {
			Consistently(receptorClient.UpsertDomainCallCount).Should(Equal(0))
		})

		Context("and the differ provides creates, updates, and deletes", func() {
			It("sends the creates and updates but not the deletes", func() {
				Eventually(receptorClient.CreateDesiredLRPCallCount).Should(Equal(1))
				Consistently(receptorClient.DeleteDesiredLRPCallCount).Should(Equal(0))
			})
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

		Context("and the fetcher has desired app fingerprints", func() {
			BeforeEach(func() {
				differ.DiffStub = func(
					logger lager.Logger,
					cancel <-chan struct{},
					existing []receptor.DesiredLRPResponse,
					desired <-chan []cc_messages.CCDesiredAppFingerprint,
					missing chan<- []cc_messages.CCDesiredAppFingerprint,
				) []string {
					defer GinkgoRecover()

					Eventually(desired).Should(Receive(Equal([]cc_messages.CCDesiredAppFingerprint{{}})))
					close(missing)

					return []string{}
				}
			})

			It("sends the fingerprints to the differ", func() {
				// make sure we get to the assertion in the Diff stub
				Eventually(differ.DiffCallCount).Should(Equal(1))
				Consistently(differ.DiffCallCount).Should(Equal(1))
			})
		})

		Context("and the differ discovers desired LRPs to delete", func() {
			It("the processor deletes them", func() {
				Eventually(receptorClient.DeleteDesiredLRPCallCount).Should(Equal(1))
				Eventually(receptorClient.UpsertDomainCallCount).Should(Equal(1))

				Ω(receptorClient.DeleteDesiredLRPArgsForCall(0)).Should(Equal("my-app-to-delete"))
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

		Context("and the differ discovers missing apps", func() {
			BeforeEach(func() {
				fetcher.FetchDesiredAppsStub = func(
					logger lager.Logger,
					cancel <-chan struct{},
					fingerprints <-chan []cc_messages.CCDesiredAppFingerprint,
					desireAppRequests chan<- []cc_messages.DesireAppRequestFromCC,
					httpClient *http.Client,
				) error {
					defer GinkgoRecover()

					Eventually(fingerprints).Should(Receive(Equal([]cc_messages.CCDesiredAppFingerprint{{}})))
					Eventually(desireAppRequests).Should(BeSent([]cc_messages.DesireAppRequestFromCC{
						{ProcessGuid: "missing-process-guid"},
					}))

					close(desireAppRequests)
					return nil
				}
			})

			It("sends fingerprints for the missing apps to fetcher", func() {
				// make sure we get to the assertion in the FetchDesiredApps stub
				Eventually(fetcher.FetchDesiredAppsCallCount).Should(Equal(1))
				Consistently(fetcher.FetchDesiredAppsCallCount).Should(Equal(1))
			})

			It("uses the recipe builder to construct the create LRP request", func() {
				Eventually(recipeBuilder.BuildCallCount).Should(Equal(1))
				Consistently(recipeBuilder.BuildCallCount).Should(Equal(1))

				Eventually(recipeBuilder.BuildArgsForCall(0)).Should(Equal(
					&cc_messages.DesireAppRequestFromCC{
						ProcessGuid: "missing-process-guid",
					}))
			})

			It("creates a desired LRP for the missing app", func() {
				Eventually(receptorClient.CreateDesiredLRPCallCount).Should(Equal(1))
				Consistently(receptorClient.CreateDesiredLRPCallCount).Should(Equal(1))
			})

			Context("when fetching desire app requests from the CC fails", func() {
				BeforeEach(func() {
					fetcher.FetchDesiredAppsStub = func(
						logger lager.Logger,
						cancel <-chan struct{},
						fingerprints <-chan []cc_messages.CCDesiredAppFingerprint,
						desired chan<- []cc_messages.DesireAppRequestFromCC,
						httpClient *http.Client,
					) error {
						defer close(desired)
						<-fingerprints

						return errors.New("boom")
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
						Eventually(receptorClient.DeleteDesiredLRPCallCount).Should(Equal(1))
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
					It("sends the deletes but not the creates or updates", func() {
						Consistently(receptorClient.CreateDesiredLRPCallCount).Should(Equal(0))
						Eventually(receptorClient.DeleteDesiredLRPCallCount).Should(Equal(1))
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
					It("continues to send the deletes", func() {
						Eventually(receptorClient.DeleteDesiredLRPCallCount).Should(Equal(1))
					})
				})
			})
		})

		Context("and the differ provides creates and deletes", func() {
			It("sends them to the receptor and updates the domain", func() {
				Eventually(receptorClient.CreateDesiredLRPCallCount).Should(Equal(1))
				Eventually(receptorClient.DeleteDesiredLRPCallCount).Should(Equal(1))
				Eventually(receptorClient.UpsertDomainCallCount).Should(Equal(1))

				Ω(receptorClient.CreateDesiredLRPArgsForCall(0)).Should(Equal(receptor.DesiredLRPCreateRequest{}))
				Ω(receptorClient.DeleteDesiredLRPArgsForCall(0)).Should(Equal("my-app-to-delete"))

				d, ttl := receptorClient.UpsertDomainArgsForCall(0)
				Ω(d).Should(Equal("cf-apps"))
				Ω(ttl).Should(Equal(1 * time.Second))
			})
		})
	})
})
