package bulk_test

import (
	"github.com/cloudfoundry-incubator/nsync/bulk"
	"github.com/cloudfoundry-incubator/nsync/bulk/fakes"
	"github.com/cloudfoundry-incubator/receptor"
	"github.com/cloudfoundry-incubator/runtime-schema/cc_messages"
	"github.com/cloudfoundry-incubator/runtime-schema/models"
	"github.com/pivotal-golang/lager/lagertest"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
)

var _ = Describe("Differ", func() {
	var (
		existingLRP receptor.DesiredLRPResponse

		desiredChan    chan *cc_messages.DesireAppRequestFromCC
		createChan     chan *receptor.DesiredLRPCreateRequest
		deleteListChan chan []string

		builder *fakes.FakeRecipeBuilder

		differ bulk.Differ
	)

	BeforeEach(func() {
		desiredChan = nil
		createChan = nil
		deleteListChan = nil

		existingLRP = receptor.DesiredLRPResponse{
			ProcessGuid: "process-guid-1",
			Instances:   1,
			Stack:       "stack-1",
			Action: &models.DownloadAction{
				From: "http://example.com",
				To:   "/tmp/internet",
			},
		}

		builder = new(fakes.FakeRecipeBuilder)

		differ = bulk.NewDiffer(builder, lagertest.NewTestLogger("test"))

		desiredChan = make(chan *cc_messages.DesireAppRequestFromCC, 1)
		createChan = make(chan *receptor.DesiredLRPCreateRequest, 1)
		deleteListChan = make(chan []string, 1)
	})

	JustBeforeEach(func() {
		go differ.Diff(
			[]receptor.DesiredLRPResponse{existingLRP},
			desiredChan,
			createChan,
			deleteListChan,
		)
		close(desiredChan)
	})

	Context("when a desired App comes in from CC", func() {
		var newlyDesiredApp *cc_messages.DesireAppRequestFromCC

		BeforeEach(func() {
			newlyDesiredApp = &cc_messages.DesireAppRequestFromCC{
				ProcessGuid:  "new-app-process-guid",
				NumInstances: 1,
				DropletUri:   "http://example.com",
				Stack:        "some-stack",
			}

			desiredChan <- newlyDesiredApp
		})

		Context("and it is not in the desired LRPs set", func() {
			newCreateReq := &receptor.DesiredLRPCreateRequest{
				ProcessGuid: "new-process-guid",
				Instances:   1,
				Stack:       "stack-2",
				Action: &models.RunAction{
					Path: "ls",
				},
			}

			BeforeEach(func() {
				builder.BuildReturns(newCreateReq, nil)
			})

			It("sends a create request", func() {
				Eventually(deleteListChan).Should(Receive())
				Eventually(deleteListChan).Should(BeClosed())
				Eventually(createChan).Should(Receive(Equal(newCreateReq)))

				Ω(builder.BuildArgsForCall(0)).Should(Equal(newlyDesiredApp))
			})
		})

		Context("and it is in the desired LRPs set", func() {
			BeforeEach(func() {
				existingLRP.ProcessGuid = newlyDesiredApp.ProcessGuid
			})

			Context("with the same values", func() {
				It("sends no request", func() {
					deleteList := &[]string{}

					Eventually(deleteListChan).Should(Receive(deleteList))
					Eventually(deleteListChan).Should(BeClosed())
					Consistently(createChan).ShouldNot(Receive())

					Ω(*deleteList).Should(HaveLen(0))
				})
			})
		})
	})

	Context("when the existing LRP is not in the CC's set of desired Apps", func() {
		It("sends a delete request for the existing LRP", func() {
			deleteList := &[]string{}

			Eventually(deleteListChan).Should(Receive(deleteList))
			Eventually(deleteListChan).Should(BeClosed())
			Consistently(createChan).ShouldNot(Receive())
			Ω(*deleteList).Should(ContainElement(existingLRP.ProcessGuid))
		})
	})
})
