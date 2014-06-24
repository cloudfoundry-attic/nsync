package bulk_test

import (
	"errors"
	"net/http"
	"os"
	"time"

	. "github.com/cloudfoundry-incubator/nsync/bulk"
	"github.com/cloudfoundry-incubator/nsync/bulk/fakefetcher"
	"github.com/cloudfoundry-incubator/runtime-schema/bbs/fake_bbs"
	"github.com/cloudfoundry-incubator/runtime-schema/models"
	"github.com/cloudfoundry/gosteno"
	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
	"github.com/tedsuo/ifrit"
)

var _ = Describe("Processor", func() {
	var (
		bbs     *fake_bbs.FakeNsyncBBS
		fetcher *fakefetcher.FakeFetcher

		processor ifrit.Runner

		process ifrit.Process
	)

	BeforeEach(func() {
		bbs = new(fake_bbs.FakeNsyncBBS)
		fetcher = new(fakefetcher.FakeFetcher)

		processor = NewProcessor(
			bbs,
			500*time.Millisecond,
			time.Second,
			10,
			false,
			gosteno.NewLogger("test"),
			fetcher,
		)
	})

	JustBeforeEach(func() {
		process = ifrit.Envoke(processor)
	})

	AfterEach(func() {
		process.Signal(os.Interrupt)
		Eventually(process.Wait()).Should(Receive())
	})

	Describe("when getting all desired LRPs fails", func() {
		BeforeEach(func() {
			bbs.GetAllDesiredLRPsReturns(nil, errors.New("oh no!"))
		})

		It("keeps calm and carries on", func() {
			Consistently(process.Wait()).ShouldNot(Receive())
		})

		It("tries again after the polling interval", func() {
			Eventually(bbs.GetAllDesiredLRPsCallCount).Should(Equal(1))

			t1 := time.Now()

			Eventually(bbs.GetAllDesiredLRPsCallCount).Should(Equal(2))

			t2 := time.Now()

			Ω(t2.Sub(t1)).Should(BeNumerically("~", 500*time.Millisecond, 100*time.Millisecond))
		})
	})

	Context("when changing the desired LRP fails", func() {
		BeforeEach(func() {
			results := make(chan error, 3)
			results <- nil
			results <- errors.New("logic error")
			results <- nil
			close(results)

			bbs.ChangeDesiredLRPStub = func(change models.DesiredLRPChange) error {
				return <-results
			}

			fetcher.FetchStub = func(results chan<- models.DesiredLRP, httpClient *http.Client) error {
				results <- models.DesiredLRP{}
				results <- models.DesiredLRP{}
				results <- models.DesiredLRP{}
				return nil
			}
		})

		It("keeps calm and carries on", func() {
			Consistently(process.Wait()).ShouldNot(Receive())
		})

		It("continues to process other changes", func() {
			Eventually(bbs.ChangeDesiredLRPCallCount).Should(Equal(3))
		})
	})
})
