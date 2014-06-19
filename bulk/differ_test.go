package bulk_test

import (
	. "github.com/cloudfoundry-incubator/nsync/bulk"
	"github.com/cloudfoundry-incubator/runtime-schema/models"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
)

var _ = Describe("Differ", func() {
	var desired chan<- models.DesiredLRP
	var changes <-chan models.DesiredLRPChange
	var existingLRPs []models.DesiredLRP

	BeforeEach(func() {
		desiredChan := make(chan models.DesiredLRP)
		existingLRPs = []models.DesiredLRP{
			{
				ProcessGuid:     "process-guid-1",
				Instances:       2,
				Stack:           "stack-1",
				MemoryMB:        256,
				DiskMB:          1024,
				FileDescriptors: 16,
				Source:          "source-url-1",
				StartCommand:    "start-command-1",
				Environment: []models.EnvironmentVariable{
					{Name: "env-key-1", Value: "env-value-1"},
					{Name: "env-key-2", Value: "env-value-2"},
				},
				Routes:  []string{"route-1", "route-2"},
				LogGuid: "log-guid-1",
			},
			{
				ProcessGuid:     "process-guid-2",
				Instances:       1,
				Stack:           "stack-2",
				MemoryMB:        255,
				DiskMB:          1023,
				FileDescriptors: 15,
				Source:          "source-url-2",
				StartCommand:    "start-command-2",
				Environment: []models.EnvironmentVariable{
					{Name: "env-key-2", Value: "env-value-2"},
					{Name: "env-key-3", Value: "env-value-3"},
				},
				Routes:  []string{"route-3", "route-4"},
				LogGuid: "log-guid-2",
			},
		}

		desired = desiredChan
		changes = Diff(existingLRPs, desiredChan)
	})

	Context("when a desired LRP comes in", func() {
		Context("and it is not in the desired set", func() {
			newlyDesiredLRP := models.DesiredLRP{
				ProcessGuid:     "process-guid-3",
				Instances:       2,
				Stack:           "stack-2",
				MemoryMB:        255,
				DiskMB:          1023,
				FileDescriptors: 15,
				Source:          "source-url-2",
				StartCommand:    "start-command-2",
				Environment: []models.EnvironmentVariable{
					{Name: "env-key-2", Value: "env-value-2"},
					{Name: "env-key-3", Value: "env-value-3"},
				},
				Routes:  []string{"route-3", "route-4"},
				LogGuid: "log-guid-2",
			}

			BeforeEach(func() {
				desired <- newlyDesiredLRP
			})

			It("emits a change no before, but an after", func() {
				Eventually(changes).Should(Receive(Equal(models.DesiredLRPChange{
					Before: nil,
					After:  &newlyDesiredLRP,
				})))
			})
		})

		Context("and it is in the desired set", func() {
			Context("with the same values", func() {
				BeforeEach(func() {
					desired <- existingLRPs[1]
				})

				It("does not emit any change", func() {
					Consistently(changes, 0.2).ShouldNot(Receive())
				})
			})

			Context("but has different values", func() {
				var changedLRP models.DesiredLRP

				BeforeEach(func() {
					changedLRP = existingLRPs[1]
					changedLRP.Source = "new-source"
					desired <- changedLRP
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
					desired <- existingLRPs[1]
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
