package bulk_test

import (
	"errors"
	"net/http"
	"os"
	"time"

	. "github.com/cloudfoundry-incubator/nsync/bulk"
	"github.com/cloudfoundry-incubator/nsync/bulk/fakes"
	"github.com/cloudfoundry-incubator/runtime-schema/bbs/fake_bbs"
	"github.com/cloudfoundry-incubator/runtime-schema/cc_messages"
	"github.com/cloudfoundry-incubator/runtime-schema/models"
	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
	"github.com/pivotal-golang/lager"
	"github.com/pivotal-golang/lager/lagertest"
	"github.com/tedsuo/ifrit"
)

var _ = Describe("Processor", func() {
	var (
		bbs     *fake_bbs.FakeNsyncBBS
		fetcher *fakes.FakeFetcher
		differ  Differ

		processor ifrit.Runner

		process ifrit.Process
	)

	BeforeEach(func() {
		bbs = new(fake_bbs.FakeNsyncBBS)
		fetcher = new(fakes.FakeFetcher)
		differ = NewDiffer(new(fakes.FakeRecipeBuilder), lagertest.NewTestLogger("test"))

		processor = NewProcessor(
			bbs,
			500*time.Millisecond,
			time.Second,
			time.Second,
			10,
			false,
			lager.NewLogger("test"),
			fetcher,
			differ,
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
			bbs.GetAllDesiredLRPsByDomainReturns(nil, errors.New("oh no!"))
		})

		It("keeps calm and carries on", func() {
			Consistently(process.Wait()).ShouldNot(Receive())
		})

		It("tries again after the polling interval", func() {
			Eventually(bbs.GetAllDesiredLRPsByDomainCallCount).Should(Equal(1))

			t1 := time.Now()

			Eventually(bbs.GetAllDesiredLRPsByDomainCallCount).Should(Equal(2))

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

			fetcher.FetchStub = func(results chan<- cc_messages.DesireAppRequestFromCC, httpClient *http.Client) error {
				results <- cc_messages.DesireAppRequestFromCC{}
				results <- cc_messages.DesireAppRequestFromCC{}
				results <- cc_messages.DesireAppRequestFromCC{}
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
