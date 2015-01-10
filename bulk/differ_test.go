package bulk_test

import (
	"github.com/cloudfoundry-incubator/nsync/bulk"
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

		cancelChan  chan struct{}
		desiredChan chan []cc_messages.CCDesiredAppFingerprint
		missingChan chan []cc_messages.CCDesiredAppFingerprint

		deleteList []string

		logger *lagertest.TestLogger

		differ bulk.Differ
	)

	BeforeEach(func() {
		logger = lagertest.NewTestLogger("test")
		differ = bulk.NewDiffer()

		existingLRP = receptor.DesiredLRPResponse{
			ProcessGuid: "process-guid-1",
			Instances:   1,
			Stack:       "stack-1",
			Action: &models.DownloadAction{
				From: "http://example.com",
				To:   "/tmp/internet",
			},
			Annotation: "some-etag-1",
		}

		cancelChan = make(chan struct{})
		desiredChan = make(chan []cc_messages.CCDesiredAppFingerprint, 1)
		missingChan = make(chan []cc_messages.CCDesiredAppFingerprint, 1)
	})

	Context("when not cancelling", func() {
		Context("when desired apps come in from CC", func() {
			var existingAppFingerprint cc_messages.CCDesiredAppFingerprint
			var desiredAppFingerprints []cc_messages.CCDesiredAppFingerprint

			BeforeEach(func() {
				existingAppFingerprint = cc_messages.CCDesiredAppFingerprint{
					ProcessGuid: existingLRP.ProcessGuid,
					ETag:        existingLRP.Annotation,
				}

				desiredAppFingerprints = []cc_messages.CCDesiredAppFingerprint{
					existingAppFingerprint,
				}
			})

			JustBeforeEach(func() {
				deleteList = differ.Diff(
					logger,
					cancelChan,
					[]receptor.DesiredLRPResponse{existingLRP},
					desiredChan,
					missingChan,
				)
			})

			AfterEach(func() {
				Eventually(missingChan).Should(BeClosed())
			})

			Context("existing desired LRPs and desired apps are consistent", func() {
				BeforeEach(func() {
					desiredChan <- desiredAppFingerprints
					close(desiredChan)
				})

				It("sends an empty slice across the missing channel", func() {
					Eventually(missingChan).Should(Receive(ConsistOf([]cc_messages.CCDesiredAppFingerprint{})))
				})

				It("returns an empty delete list", func() {
					Ω(deleteList).Should(BeEmpty())
				})
			})

			Context("and some are missing from the existing desired LRPs set", func() {
				var missingAppFingerprints []cc_messages.CCDesiredAppFingerprint

				BeforeEach(func() {
					missingAppFingerprints = []cc_messages.CCDesiredAppFingerprint{
						cc_messages.CCDesiredAppFingerprint{
							ProcessGuid: "missing-guid-1",
							ETag:        "missing-etag-1",
						},
						cc_messages.CCDesiredAppFingerprint{
							ProcessGuid: "missing-guid-2",
							ETag:        "missing-etag-2",
						},
					}

					desiredAppFingerprints := []cc_messages.CCDesiredAppFingerprint{
						existingAppFingerprint,
						missingAppFingerprints[0],
						missingAppFingerprints[1],
					}

					desiredChan <- desiredAppFingerprints
					close(desiredChan)
				})

				It("sends a slice of missing fingerprints across the missing channel", func() {
					Eventually(missingChan).Should(Receive(ConsistOf(missingAppFingerprints)))

					Ω(deleteList).Should(BeEmpty())
				})
			})

			Context("and an existing desired LRP is not a desired app", func() {
				BeforeEach(func() {
					desiredChan <- []cc_messages.CCDesiredAppFingerprint{}
					close(desiredChan)
				})

				It("includes the process guid of the desired LRP in the delete slice", func() {
					Eventually(missingChan).Should(Receive(ConsistOf([]cc_messages.CCDesiredAppFingerprint{})))

					Ω(deleteList).Should(ConsistOf(existingLRP.ProcessGuid))
				})
			})

			Context("and an existing desired LRP has a stale ETag", func() {
				var fingerprint cc_messages.CCDesiredAppFingerprint

				BeforeEach(func() {
					fingerprint = cc_messages.CCDesiredAppFingerprint{
						ProcessGuid: existingLRP.ProcessGuid,
						ETag:        "updated-etag",
					}
					desiredAppFingerprints = []cc_messages.CCDesiredAppFingerprint{
						fingerprint,
					}
					desiredChan <- desiredAppFingerprints
					close(desiredChan)
				})

				It("treats the existing LRP as missing", func() {
					Eventually(missingChan).Should(Receive(ConsistOf(fingerprint)))
				})

				It("does not add the existing LRP to the delete list", func() {
					Ω(deleteList).Should(BeEmpty())
				})
			})
		})

		Context("while the desired app channel remains open", func() {
			var done chan struct{}
			BeforeEach(func() {
				done = make(chan struct{})
				desiredChan = make(chan []cc_messages.CCDesiredAppFingerprint)
				missingChan = make(chan []cc_messages.CCDesiredAppFingerprint)

				go func() {
					deleteList = differ.Diff(
						logger,
						cancelChan,
						[]receptor.DesiredLRPResponse{},
						desiredChan,
						missingChan,
					)
					close(done)
				}()
			})

			AfterEach(func() {
				Eventually(missingChan).Should(BeClosed())
			})

			It("continues to process the apps in batches", func() {
				desiredAppFingerprints := []cc_messages.CCDesiredAppFingerprint{}

				Eventually(desiredChan).Should(BeSent(desiredAppFingerprints))
				Eventually(missingChan).Should(Receive())

				Eventually(desiredChan).Should(BeSent(desiredAppFingerprints))
				Eventually(missingChan).Should(Receive())

				close(desiredChan)
				Eventually(done).Should(BeClosed())
			})
		})
	})

	Describe("cancelling", func() {
		var done chan struct{}
		BeforeEach(func() {
			done = make(chan struct{})
			desiredChan = make(chan []cc_messages.CCDesiredAppFingerprint)
			missingChan = make(chan []cc_messages.CCDesiredAppFingerprint)

			go func() {
				deleteList = differ.Diff(
					logger,
					cancelChan,
					[]receptor.DesiredLRPResponse{},
					desiredChan,
					missingChan,
				)
				close(done)
			}()
		})

		Context("when waiting for desired fingerprints", func() {
			It("exits when cancelled", func() {
				close(cancelChan)

				Eventually(done).Should(BeClosed())
				Eventually(missingChan).Should(BeClosed())
			})

			It("returns an empty delete list", func() {
				close(cancelChan)
				Eventually(done).Should(BeClosed())

				Ω(deleteList).Should(BeEmpty())
			})
		})

		Context("when waiting to send missing fingerprints", func() {
			It("exits when cancelled", func() {
				Eventually(desiredChan).Should(BeSent([]cc_messages.CCDesiredAppFingerprint{}))
				close(cancelChan)

				Eventually(done).Should(BeClosed())
				Eventually(missingChan).Should(BeClosed())
			})

			It("returns an empty delete list", func() {
				close(cancelChan)
				Eventually(done).Should(BeClosed())

				Ω(deleteList).Should(BeEmpty())
			})
		})
	})
})
