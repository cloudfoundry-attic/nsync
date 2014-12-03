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

		processor ifrit.Runner

		process      ifrit.Process
		syncDuration time.Duration
		metricSender *fake.FakeMetricSender
		timeProvider *faketimeprovider.FakeTimeProvider
	)

	BeforeEach(func() {
		metricSender = fake.NewFakeMetricSender()
		metrics.Initialize(metricSender)
		syncDuration = 900900
		timeProvider = faketimeprovider.New(time.Now())

		fetcher = new(fakes.FakeFetcher)
		fetcher.FetchStub = func(results chan<- *cc_messages.DesireAppRequestFromCC, httpClient *http.Client) error {
			close(results)
			return nil
		}

		differ = new(fakes.FakeDiffer)

		differ.DiffStub = func(
			existing []receptor.DesiredLRPResponse,
			desiredChan <-chan *cc_messages.DesireAppRequestFromCC,
			createChan chan<- *receptor.DesiredLRPCreateRequest,
			deleteListChan chan<- []string,
		) {
			createChan <- &receptor.DesiredLRPCreateRequest{}
			deleteListChan <- []string{"my-app-to-delete"}
			close(createChan)
			close(deleteListChan)
		}

		receptorClient = new(fake_receptor.FakeClient)

		receptorClient.BumpFreshDomainStub = func(receptor.FreshDomainBumpRequest) error {
			timeProvider.Increment(syncDuration)
			return nil
		}

		processor = bulk.NewProcessor(
			receptorClient,
			500*time.Millisecond,
			time.Second,
			time.Second,
			10,
			false,
			lager.NewLogger("test"),
			fetcher,
			differ,
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
			Eventually(receptorClient.DesiredLRPsByDomainCallCount).Should(Equal(1))

			t1 := time.Now()

			Eventually(receptorClient.DesiredLRPsByDomainCallCount).Should(Equal(2))

			t2 := time.Now()

			Ω(t2.Sub(t1)).Should(BeNumerically("~", 500*time.Millisecond, 100*time.Millisecond))
		})

		It("does not call the differ, the fetcher, or the receptor client for updates", func() {
			Consistently(fetcher.FetchCallCount).Should(Equal(0))
			Consistently(differ.DiffCallCount).Should(Equal(0))
			Consistently(receptorClient.CreateDesiredLRPCallCount).Should(Equal(0))
			Consistently(receptorClient.DeleteDesiredLRPCallCount).Should(Equal(0))
			Consistently(receptorClient.BumpFreshDomainCallCount).Should(Equal(0))
		})
	})

	Context("when fetching succeeds", func() {
		It("emits the total time taken to talk to CC and then updated desired state", func() {
			Eventually(receptorClient.BumpFreshDomainCallCount, 5).Should(Equal(1))

			Eventually(func() fake.Metric { return metricSender.GetValue("DesiredLRPSyncDuration") }).Should(Equal(fake.Metric{
				Value: float64(syncDuration),
				Unit:  "nanos",
			}))
		})

		Context("and the fetcher has cc messages", func() {
			BeforeEach(func() {
				fetcher.FetchStub = func(results chan<- *cc_messages.DesireAppRequestFromCC, httpClient *http.Client) error {
					results <- &cc_messages.DesireAppRequestFromCC{}
					close(results)
					return nil
				}

				differ.DiffStub = func(
					existing []receptor.DesiredLRPResponse,
					desiredChan <-chan *cc_messages.DesireAppRequestFromCC,
					createChan chan<- *receptor.DesiredLRPCreateRequest,
					deleteListChan chan<- []string,
				) {
					defer GinkgoRecover()
					Ω(desiredChan).Should(Receive(Equal(&cc_messages.DesireAppRequestFromCC{})))
					close(createChan)
					close(deleteListChan)
				}
			})

			It("sends the cc messages to the differ", func() {
				// assertion established in BeforeEach
			})
		})

		Context("and the differ provides creates and deletes", func() {
			It("sends them to the receptor and bumps the freshness", func() {
				Eventually(receptorClient.CreateDesiredLRPCallCount).Should(Equal(1))
				Eventually(receptorClient.DeleteDesiredLRPCallCount).Should(Equal(1))
				Eventually(receptorClient.BumpFreshDomainCallCount).Should(Equal(1))

				Ω(receptorClient.CreateDesiredLRPArgsForCall(0)).Should(Equal(receptor.DesiredLRPCreateRequest{}))

				Ω(receptorClient.DeleteDesiredLRPArgsForCall(0)).Should(Equal("my-app-to-delete"))

				Ω(receptorClient.BumpFreshDomainArgsForCall(0)).Should(Equal(receptor.FreshDomainBumpRequest{
					Domain:       "cf-apps",
					TTLInSeconds: 1,
				}))
			})

			Context("and the create request fails", func() {
				BeforeEach(func() {
					receptorClient.CreateDesiredLRPReturns(errors.New("create failed!"))
				})

				It("sends all the other updates", func() {
					Eventually(receptorClient.CreateDesiredLRPCallCount).Should(Equal(1))
					Eventually(receptorClient.DeleteDesiredLRPCallCount).Should(Equal(1))
					Eventually(receptorClient.BumpFreshDomainCallCount).Should(Equal(1))
				})
			})

			Context("and the delete request fails", func() {
				BeforeEach(func() {
					receptorClient.DeleteDesiredLRPReturns(errors.New("delete failed!"))
				})

				It("sends all the other updates", func() {
					Eventually(receptorClient.CreateDesiredLRPCallCount).Should(Equal(1))
					Eventually(receptorClient.DeleteDesiredLRPCallCount).Should(Equal(1))
					Eventually(receptorClient.BumpFreshDomainCallCount).Should(Equal(1))
				})
			})
		})
	})

	Context("when fetching fails", func() {
		BeforeEach(func() {
			fetcher.FetchStub = func(results chan<- *cc_messages.DesireAppRequestFromCC, httpClient *http.Client) error {
				close(results)
				return errors.New("whoops, failed to fetch")
			}
		})

		It("keeps calm and carries on", func() {
			Consistently(process.Wait()).ShouldNot(Receive())
		})

		It("does not bump the freshness", func() {
			Consistently(receptorClient.BumpFreshDomainCallCount).Should(Equal(0))
		})

		Context("and the differ provides creates, updates, and deletes", func() {
			It("sends the creates and updates but not the deletes", func() {
				Eventually(receptorClient.CreateDesiredLRPCallCount).Should(Equal(1))
				Consistently(receptorClient.DeleteDesiredLRPCallCount).Should(Equal(0))
			})
		})
	})
})
