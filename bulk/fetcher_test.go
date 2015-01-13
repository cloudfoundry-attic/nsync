package bulk_test

import (
	"net/http"
	"net/url"
	"time"

	. "github.com/cloudfoundry-incubator/nsync/bulk"
	"github.com/cloudfoundry-incubator/runtime-schema/cc_messages"
	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
	"github.com/onsi/gomega/ghttp"
	"github.com/pivotal-golang/lager/lagertest"
)

var _ = Describe("Fetcher", func() {
	var (
		fetcher    Fetcher
		fakeCC     *ghttp.Server
		logger     *lagertest.TestLogger
		httpClient *http.Client

		cancel chan struct{}
	)

	BeforeEach(func() {
		fakeCC = ghttp.NewServer()
		logger = lagertest.NewTestLogger("test")

		cancel = make(chan struct{})
		httpClient = &http.Client{Timeout: time.Second}

		fetcher = &CCFetcher{
			BaseURI:   fakeCC.URL(),
			BatchSize: 2,
			Username:  "the-username",
			Password:  "the-password",
		}
	})

	Describe("Fetching Desired App Fingerprints", func() {
		Context("without cancelling", func() {
			var results chan []cc_messages.CCDesiredAppFingerprint
			var fetchErr error

			BeforeEach(func() {
				results = make(chan []cc_messages.CCDesiredAppFingerprint, 1)
			})

			JustBeforeEach(func() {
				fetchErr = fetcher.FetchFingerprints(logger, cancel, results, httpClient)
			})

			Context("when retrieving fingerprints", func() {
				BeforeEach(func() {
					results = make(chan []cc_messages.CCDesiredAppFingerprint, 2)

					fakeCC.AppendHandlers(
						ghttp.CombineHandlers(
							ghttp.VerifyRequest("GET", "/internal/bulk/apps", "batch_size=2&format=fingerprint&token={}"),
							ghttp.VerifyBasicAuth("the-username", "the-password"),
							ghttp.RespondWith(200, `{
						"token": {"id":"the-token-id"},
						"fingerprints": [
							{
								"process_guid": "process-guid-1",
								"etag": "1234567.890"
							},
							{
								"process_guid": "process-guid-2",
								"etag": "2345678.901"
							}
						]
					}`),
						),
						ghttp.CombineHandlers(
							ghttp.VerifyRequest("GET", "/internal/bulk/apps", `batch_size=2&format=fingerprint&token={"id":"the-token-id"}`),
							ghttp.VerifyBasicAuth("the-username", "the-password"),
							ghttp.RespondWith(200, `{
							"token": {"id":"another-token-id"},
							"fingerprints": [
								{
									"process_guid": "process-guid-3",
									"etag": "3456789.012"
								}
							]
						}`),
						),
					)
				})

				It("retrieves fingerprints of all apps desired by CC", func() {
					Ω(fetchErr).ShouldNot(HaveOccurred())

					Ω(fakeCC.ReceivedRequests()).Should(HaveLen(2))

					Eventually(results).Should(Receive(ConsistOf(
						cc_messages.CCDesiredAppFingerprint{
							ProcessGuid: "process-guid-1",
							ETag:        "1234567.890",
						},
						cc_messages.CCDesiredAppFingerprint{
							ProcessGuid: "process-guid-2",
							ETag:        "2345678.901",
						},
					)))

					Eventually(results).Should(Receive(ConsistOf(
						cc_messages.CCDesiredAppFingerprint{
							ProcessGuid: "process-guid-3",
							ETag:        "3456789.012",
						})))

					Eventually(results).Should(BeClosed())
				})
			})

			Context("when the response is missing a bulk token", func() {
				Context("when the cc responds with a full batch", func() {
					BeforeEach(func() {
						fakeCC.AppendHandlers(
							ghttp.CombineHandlers(
								ghttp.VerifyRequest("GET", "/internal/bulk/apps", "batch_size=2&format=fingerprint&token={}"),
								ghttp.VerifyBasicAuth("the-username", "the-password"),
								ghttp.RespondWith(200, `{
							"fingerprints": [
								{
									"process_guid": "process-guid-1",
									"etag": "1234567.890"
								},
								{
									"process_guid": "process-guid-2",
									"etag": "2345678.901"
								}
							]
						}`),
							),
						)
					})

					It("returns an error", func() {
						Ω(fetchErr).Should(MatchError("token not included in response"))
					})

					It("sends the first batch of fingerprints, then closes the results channel", func() {
						Eventually(results).Should(Receive(ConsistOf(
							cc_messages.CCDesiredAppFingerprint{
								ProcessGuid: "process-guid-1",
								ETag:        "1234567.890",
							},
							cc_messages.CCDesiredAppFingerprint{
								ProcessGuid: "process-guid-2",
								ETag:        "2345678.901",
							},
						)))

						Eventually(results).Should(BeClosed())
					})
				})

				Context("when the cc responds with a partial batch", func() {
					BeforeEach(func() {
						fakeCC.AppendHandlers(
							ghttp.CombineHandlers(
								ghttp.VerifyRequest("GET", "/internal/bulk/apps", "batch_size=2&format=fingerprint&token={}"),
								ghttp.VerifyBasicAuth("the-username", "the-password"),
								ghttp.RespondWith(200, `{
							"fingerprints": [
								{
									"process_guid": "process-guid-1",
									"etag": "1234567.890"
								}
							]
						}`),
							),
						)
					})

					It("does not return an error", func() {
						Ω(fetchErr).ShouldNot(HaveOccurred())
					})

					It("sends the first batch of fingerprints, then closes the results channel", func() {
						Eventually(results).Should(Receive(ConsistOf(
							cc_messages.CCDesiredAppFingerprint{
								ProcessGuid: "process-guid-1",
								ETag:        "1234567.890",
							},
						)))

						Eventually(results).Should(BeClosed())
					})
				})
			})

			Context("when the API times out", func() {
				ccResponseTime := 100 * time.Millisecond

				BeforeEach(func() {
					fakeCC.AppendHandlers(func(w http.ResponseWriter, req *http.Request) {
						time.Sleep(ccResponseTime)

						w.Write([]byte(`{
						"token": {"id":"another-token-id"},
						"fingerprints": [
							{
								"process_guid": "process-guid-1",
								"etag": "1234567.890"
							},
							{
								"process_guid": "process-guid-2",
								"etag": "2345678.901"
							}
						]
					}`))
					})

					httpClient = &http.Client{Timeout: ccResponseTime / 2}
				})

				It("returns an error", func() {
					Ω(fetchErr).Should(BeAssignableToTypeOf(&url.Error{}))
				})

				It("closes the results channel", func() {
					Eventually(results).Should(BeClosed())
				})
			})

			Context("when the API returns an error response", func() {
				BeforeEach(func() {
					fakeCC.AppendHandlers(ghttp.RespondWith(403, ""))
				})

				It("returns an error", func() {
					Ω(fetchErr).Should(MatchError(ContainSubstring("403")))
				})

				It("closes the results channel", func() {
					Eventually(results).Should(BeClosed())
				})
			})

			Context("when the server responds with invalid JSON", func() {
				BeforeEach(func() {
					fakeCC.AppendHandlers(ghttp.RespondWith(200, "{"))
				})

				It("returns an error", func() {
					Ω(fetchErr).Should(HaveOccurred())
				})

				It("closes the results channel", func() {
					Eventually(results).Should(BeClosed())
				})
			})
		})

		Context("when cancelling", func() {
			var results chan []cc_messages.CCDesiredAppFingerprint
			var errChan chan error

			BeforeEach(func() {
				fakeCC.AppendHandlers(
					ghttp.CombineHandlers(
						ghttp.VerifyRequest("GET", "/internal/bulk/apps", "batch_size=2&format=fingerprint&token={}"),
						ghttp.RespondWith(200, `{
							"token": {"id":"another-token-id"},
							"fingerprints": [
								{
									"process_guid": "process-guid-3",
									"etag": "3456789.012"
								}
							]
						}`),
					),
				)

				results = make(chan []cc_messages.CCDesiredAppFingerprint)
				errChan = make(chan error)

				go func() {
					errChan <- fetcher.FetchFingerprints(logger, cancel, results, http.DefaultClient)
				}()
			})

			Context("when waiting to send fingerprints", func() {
				It("exits when cancelled", func() {
					close(cancel)

					Eventually(results).Should(BeClosed())
					Eventually(errChan).Should(Receive(BeNil()))
				})
			})
		})
	})

	Describe("Fetching Desired App Request Messages from CC", func() {
		var (
			cancel           chan struct{}
			fingerprintsChan chan []cc_messages.CCDesiredAppFingerprint
			resultsChan      chan []cc_messages.DesireAppRequestFromCC
		)

		BeforeEach(func() {
			cancel = make(chan struct{})
			fingerprintsChan = make(chan []cc_messages.CCDesiredAppFingerprint, 1)
			resultsChan = make(chan []cc_messages.DesireAppRequestFromCC)
		})

		Context("when not cancelling", func() {
			Context("when retrieving multiple batches", func() {
				var desireRequests []cc_messages.DesireAppRequestFromCC

				BeforeEach(func() {
					desireRequests = []cc_messages.DesireAppRequestFromCC{
						{
							ProcessGuid:  "process-guid-1",
							DropletUri:   "source-url-1",
							Stack:        "stack-1",
							StartCommand: "start-command-1",
							Environment: cc_messages.Environment{
								{Name: "env-key-1", Value: "env-value-1"},
								{Name: "env-key-2", Value: "env-value-2"},
							},
							MemoryMB:        256,
							DiskMB:          1024,
							FileDescriptors: 16,
							NumInstances:    2,
							Routes: []string{
								"route-1",
								"route-2",
							},
							LogGuid: "log-guid-1",
							ETag:    "1234567.1890",
						},
						{
							ProcessGuid:  "process-guid-2",
							DropletUri:   "source-url-2",
							Stack:        "stack-2",
							StartCommand: "start-command-2",
							Environment: cc_messages.Environment{
								{Name: "env-key-3", Value: "env-value-3"},
								{Name: "env-key-4", Value: "env-value-4"},
							},
							MemoryMB:        512,
							DiskMB:          2048,
							FileDescriptors: 32,
							NumInstances:    4,
							Routes: []string{
								"route-3",
								"route-4",
							},
							LogGuid: "log-guid-2",
							ETag:    "2345678.2901",
						},
						{
							ProcessGuid:     "process-guid-3",
							DropletUri:      "source-url-3",
							Stack:           "stack-3",
							StartCommand:    "start-command-3",
							Environment:     cc_messages.Environment{},
							MemoryMB:        128,
							DiskMB:          512,
							FileDescriptors: 8,
							NumInstances:    4,
							Routes:          []string{},
							LogGuid:         "log-guid-3",
							ETag:            "3456789.3012",
						},
					}

					fakeCC.AppendHandlers(
						ghttp.CombineHandlers(
							ghttp.VerifyRequest("POST", "/internal/bulk/apps"),
							ghttp.VerifyBasicAuth("the-username", "the-password"),
							ghttp.VerifyJSON(`[
							"process-guid-1",
							"process-guid-2"
						]
						`),
							ghttp.RespondWithJSONEncoded(200, desireRequests[:2]),
						),
						ghttp.CombineHandlers(
							ghttp.VerifyRequest("POST", "/internal/bulk/apps"),
							ghttp.VerifyBasicAuth("the-username", "the-password"),
							ghttp.VerifyJSON(`[
							"process-guid-3"
						]
						`),
							ghttp.RespondWithJSONEncoded(200, desireRequests[2:]),
						),
					)

					fingerprintsChan = make(chan []cc_messages.CCDesiredAppFingerprint)
				})

				It("gets desire app request messages for each fingerprints batch", func() {
					errChan := make(chan error)
					go func() {
						defer GinkgoRecover()

						errChan <- fetcher.FetchDesiredApps(logger, cancel, fingerprintsChan, resultsChan, httpClient)
					}()

					fingerprints := []cc_messages.CCDesiredAppFingerprint{
						cc_messages.CCDesiredAppFingerprint{
							ProcessGuid: "process-guid-1",
							ETag:        "1234567.890",
						},
						cc_messages.CCDesiredAppFingerprint{
							ProcessGuid: "process-guid-2",
							ETag:        "2345678.901",
						},
					}
					Eventually(fingerprintsChan).Should(BeSent(fingerprints))
					Eventually(resultsChan).Should(Receive(ConsistOf(desireRequests[:2])))

					fingerprints = []cc_messages.CCDesiredAppFingerprint{
						cc_messages.CCDesiredAppFingerprint{
							ProcessGuid: "process-guid-3",
							ETag:        "3456789.012",
						},
					}
					Eventually(fingerprintsChan).Should(BeSent(fingerprints))
					Eventually(resultsChan).Should(Receive(ConsistOf(desireRequests[2])))

					close(fingerprintsChan)

					Eventually(resultsChan).Should(BeClosed())
					Eventually(errChan).Should(Receive(BeNil()))

					Ω(fakeCC.ReceivedRequests()).Should(HaveLen(2))
				})
			})

			Context("when processing a single batch", func() {
				var fetchErr error

				JustBeforeEach(func() {
					close(fingerprintsChan)
					fetchErr = fetcher.FetchDesiredApps(logger, cancel, fingerprintsChan, resultsChan, httpClient)
				})

				Context("when the fingerprint batch is empty", func() {
					BeforeEach(func() {
						fingerprintsChan <- []cc_messages.CCDesiredAppFingerprint{}
					})

					It("does send a request to the CC", func() {
						Ω(fetchErr).ShouldNot(HaveOccurred())

						Ω(fakeCC.ReceivedRequests()).Should(HaveLen(0))
					})
				})

				Context("when the API times out", func() {
					ccResponseTime := 100 * time.Millisecond

					BeforeEach(func() {
						fakeCC.AppendHandlers(func(w http.ResponseWriter, req *http.Request) {
							time.Sleep(ccResponseTime)
							w.Write([]byte(`[]`))
						})

						httpClient = &http.Client{Timeout: ccResponseTime / 2}
						fingerprintsChan <- []cc_messages.CCDesiredAppFingerprint{
							{ProcessGuid: "process-guid", ETag: "123"},
						}
					})

					It("keeps calm and carries on", func() {
						Ω(fetchErr).ShouldNot(HaveOccurred())
					})

					It("closes the results channel", func() {
						Eventually(resultsChan).Should(BeClosed())
					})
				})

				Context("when the API returns an error response", func() {
					BeforeEach(func() {
						fakeCC.AppendHandlers(ghttp.RespondWith(403, ""))

						fingerprintsChan <- []cc_messages.CCDesiredAppFingerprint{
							{ProcessGuid: "process-guid", ETag: "123"},
						}
					})

					It("keeps calm and carries on", func() {
						Ω(fetchErr).ShouldNot(HaveOccurred())
					})

					It("closes the results channel", func() {
						Eventually(resultsChan).Should(BeClosed())
					})
				})

				Context("when the server responds with invalid JSON", func() {
					BeforeEach(func() {
						fakeCC.AppendHandlers(ghttp.RespondWith(200, "{"))

						fingerprintsChan <- []cc_messages.CCDesiredAppFingerprint{
							{ProcessGuid: "process-guid", ETag: "123"},
						}
					})

					It("keeps calm and carries on", func() {
						Ω(fetchErr).ShouldNot(HaveOccurred())
					})

					It("closes the results channel", func() {
						Eventually(resultsChan).Should(BeClosed())
					})
				})
			})
		})

		Context("when cancelling", func() {
			var errChan chan error

			BeforeEach(func() {
				fakeCC.AppendHandlers(
					ghttp.CombineHandlers(
						ghttp.VerifyRequest("POST", "/internal/bulk/apps"),
						ghttp.RespondWithJSONEncoded(200, []cc_messages.DesireAppRequestFromCC{}),
					),
				)

				errChan = make(chan error)
				resultsChan = make(chan []cc_messages.DesireAppRequestFromCC)

				go func() {
					errChan <- fetcher.FetchDesiredApps(logger, cancel, fingerprintsChan, resultsChan, httpClient)
				}()
			})

			Context("when waiting for fingerprints", func() {
				It("exits when cancelled", func() {
					close(cancel)

					Eventually(resultsChan).Should(BeClosed())
					Eventually(errChan).Should(Receive(BeNil()))
				})
			})

			Context("when waiting to send results", func() {
				BeforeEach(func() {
					fingerprintsChan <- []cc_messages.CCDesiredAppFingerprint{
						{ProcessGuid: "process-guid", ETag: "an-etag"},
					}
				})

				It("exits when cancelled", func() {
					close(cancel)

					Eventually(resultsChan).Should(BeClosed())
					Eventually(errChan).Should(Receive(BeNil()))
				})
			})
		})
	})
})
