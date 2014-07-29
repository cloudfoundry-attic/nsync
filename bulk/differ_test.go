package bulk_test

import (
	. "github.com/cloudfoundry-incubator/nsync/bulk"
	"github.com/cloudfoundry-incubator/nsync/bulk/fakes"
	"github.com/cloudfoundry-incubator/runtime-schema/models"
	"github.com/pivotal-golang/lager/lagertest"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
)

var _ = Describe("Differ", func() {
	var desired chan<- models.DesireAppRequestFromCC
	var changes <-chan models.DesiredLRPChange
	var existingLRPs []models.DesiredLRP

	var builder *fakes.FakeRecipeBuilder

	var differ Differ

	BeforeEach(func() {
		desired = nil

		existingLRPs = []models.DesiredLRP{
			{
				ProcessGuid: "process-guid-1",

				Instances: 2,
				Stack:     "stack-1",

				Actions: []models.ExecutorAction{
					{
						Action: models.DownloadAction{
							From: "http://example.com",
							To:   "/tmp/internet",
						},
					},
				},
			},
			{
				ProcessGuid: "process-guid-2",

				Instances: 1,
				Stack:     "stack-2",

				Actions: []models.ExecutorAction{
					{
						Action: models.RunAction{
							Path: "reboot",
						},
					},
				},
			},
		}

		builder = new(fakes.FakeRecipeBuilder)

		differ = NewDiffer(builder, lagertest.NewTestLogger("test"))
	})

	JustBeforeEach(func() {
		desiredChan := make(chan models.DesireAppRequestFromCC)

		desired = desiredChan

		changes = differ.Diff(existingLRPs, desiredChan)
	})

	AfterEach(func() {
		// consume from changes channel
		for _ = range changes {
		}
	})

	Context("when a desired LRP comes in from CC", func() {
		var newlyDesiredApp models.DesireAppRequestFromCC

		BeforeEach(func() {
			newlyDesiredApp = models.DesireAppRequestFromCC{
				ProcessGuid:  "new-process-guid",
				NumInstances: 1,
				DropletUri:   "http://example.com",
				Stack:        "some-stack",
			}
		})

		JustBeforeEach(func() {
			desired <- newlyDesiredApp
		})

		Context("and it is not in the desired set", func() {
			newlyDesiredLRP := models.DesiredLRP{
				ProcessGuid: "new-process-guid",

				Instances: 1,
				Stack:     "stack-2",

				Actions: []models.ExecutorAction{
					{
						Action: models.RunAction{
							Path: "ls",
						},
					},
				},
			}

			BeforeEach(func() {
				builder.BuildReturns(newlyDesiredLRP, nil)
			})

			AfterEach(func() {
				close(desired)
			})

			It("emits a change no before, but an after", func() {
				Eventually(changes).Should(Receive(Equal(models.DesiredLRPChange{
					Before: nil,
					After:  &newlyDesiredLRP,
				})))

				Ω(builder.BuildArgsForCall(0)).Should(Equal(newlyDesiredApp))
			})
		})

		Context("and it is in the desired set", func() {
			BeforeEach(func() {
				newlyDesiredApp.ProcessGuid = existingLRPs[1].ProcessGuid
			})

			AfterEach(func() {
				close(desired)
			})

			Context("with the same values", func() {
				BeforeEach(func() {
					builder.BuildReturns(existingLRPs[1], nil)
				})

				It("does not emit any change", func() {
					Consistently(changes, 0.2).ShouldNot(Receive())
				})
			})

			Context("but has different values", func() {
				var changedLRP models.DesiredLRP

				BeforeEach(func() {
					newlyDesiredApp.NumInstances = 42

					changedLRP = existingLRPs[1]
					changedLRP.Instances = 42

					builder.BuildReturns(changedLRP, nil)
				})

				It("emits a change with before and after", func() {
					Eventually(changes).Should(Receive(Equal(models.DesiredLRPChange{
						Before: &existingLRPs[1],
						After:  &changedLRP,
					})))
				})
			})
		})

		Context("and the desired stream ends", func() {
			JustBeforeEach(func() {
				close(desired)
			})

			It("closes the desired changes channel", func() {
				Eventually(changes).Should(BeClosed())
			})

			Context("and there were extras in the existing desired set", func() {
				BeforeEach(func() {
					builder.BuildReturns(existingLRPs[1], nil)
				})

				It("emits a change for each with before, but no after", func() {
					Eventually(changes).Should(Receive(Equal(models.DesiredLRPChange{
						Before: &existingLRPs[0],
					})))
				})

				It("closes the desired changes channel", func() {
					Eventually(changes).Should(BeClosed())
				})
			})
		})
	})
})
