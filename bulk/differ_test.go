package bulk_test

import (
	. "github.com/cloudfoundry-incubator/nsync/bulk"
	"github.com/cloudfoundry-incubator/nsync/bulk/fakes"
	"github.com/cloudfoundry-incubator/runtime-schema/cc_messages"
	"github.com/cloudfoundry-incubator/runtime-schema/models"
	"github.com/pivotal-golang/lager/lagertest"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
)

var _ = Describe("Differ", func() {
	var existingLRP models.DesiredLRP
	var desired chan cc_messages.DesireAppRequestFromCC
	var changes []models.DesiredLRPChange

	var builder *fakes.FakeRecipeBuilder

	var differ Differ

	BeforeEach(func() {
		desired = nil

		existingLRP = models.DesiredLRP{
			ProcessGuid: "process-guid-1",

			Instances: 1,
			Stack:     "stack-1",

			Action: &models.DownloadAction{
				From: "http://example.com",
				To:   "/tmp/internet",
			},
		}

		builder = new(fakes.FakeRecipeBuilder)

		differ = NewDiffer(builder, lagertest.NewTestLogger("test"))

		desired = make(chan cc_messages.DesireAppRequestFromCC, 1)
	})

	JustBeforeEach(func() {
		close(desired)
		changes = differ.Diff([]models.DesiredLRP{existingLRP}, desired)
	})

	Context("when a desired App comes in from CC", func() {
		var newlyDesiredApp cc_messages.DesireAppRequestFromCC

		BeforeEach(func() {
			newlyDesiredApp = cc_messages.DesireAppRequestFromCC{
				ProcessGuid:  "new-app-process-guid",
				NumInstances: 1,
				DropletUri:   "http://example.com",
				Stack:        "some-stack",
			}

			desired <- newlyDesiredApp
		})

		Context("and it is not in the desired LRPs set", func() {
			newlyDesiredLRP := models.DesiredLRP{
				ProcessGuid: "new-process-guid",

				Instances: 1,
				Stack:     "stack-2",

				Action: &models.RunAction{
					Path: "ls",
				},
			}

			BeforeEach(func() {
				builder.BuildReturns(newlyDesiredLRP, nil)
			})

			It("contains a change with no before, but an after", func() {
				Ω(changes).Should(ContainElement(models.DesiredLRPChange{
					Before: nil,
					After:  &newlyDesiredLRP,
				}))

				Ω(builder.BuildArgsForCall(0)).Should(Equal(newlyDesiredApp))
			})
		})

		Context("and it is in the desired LRPs set", func() {
			BeforeEach(func() {
				existingLRP.ProcessGuid = newlyDesiredApp.ProcessGuid
			})

			Context("with the same values", func() {
				It("does not contain any change", func() {
					Ω(changes).Should(BeEmpty())
				})
			})

			Context("but has different values", func() {
				var changedLRP models.DesiredLRP

				BeforeEach(func() {
					changedLRP = existingLRP
					changedLRP.Instances = 1

					existingLRP.Instances = 42

					builder.BuildReturns(changedLRP, nil)
				})

				It("contains a change with before and after", func() {
					Ω(changes).Should(ContainElement(models.DesiredLRPChange{
						Before: &existingLRP,
						After:  &changedLRP,
					}))
				})
			})
		})
	})

	Context("when the existing LRP is not in the CC's set of desired Apps", func() {
		It("contains a change to remove the extra LRP", func() {
			Ω(changes).Should(ContainElement(models.DesiredLRPChange{
				Before: &existingLRP,
				After:  nil,
			}))
		})
	})
})
