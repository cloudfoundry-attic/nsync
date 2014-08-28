package bulk_test

import (
	"encoding/base64"
	"net/http"
	"time"

	. "github.com/cloudfoundry-incubator/nsync/bulk"
	"github.com/cloudfoundry-incubator/runtime-schema/cc_messages"
	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
	"github.com/onsi/gomega/ghttp"
)

var _ = Describe("Fetcher", func() {
	var (
		fetcher Fetcher
		fakeCC  *ghttp.Server
	)

	BeforeEach(func() {
		fakeCC = ghttp.NewServer()

		fetcher = &CCFetcher{
			BaseURI:   fakeCC.URL(),
			BatchSize: 2,
			Username:  "the-username",
			Password:  "the-password",
		}
	})

	Describe("running a batch update process", func() {
		It("gets the desired state of all apps from the CC", func() {
			fakeCC.AppendHandlers(
				ghttp.CombineHandlers(
					ghttp.VerifyRequest("GET", "/internal/bulk/apps", "batch_size=2&token={}"),
					ghttp.VerifyHeaderKV("Authorization", "Basic "+base64Encode("the-username:the-password")),
					ghttp.RespondWith(200, `{
						"token": {"id":"the-token-id"},
						"apps": [
							{
								"disk_mb": 1024,
								"environment": [
									{ "name": "env-key-1", "value": "env-value-1" },
									{ "name": "env-key-2", "value": "env-value-2" }
								],
								"file_descriptors": 16,
								"num_instances": 2,
								"log_guid": "log-guid-1",
								"memory_mb": 256,
								"process_guid": "process-guid-1",
								"routes": [ "route-1", "route-2" ],
								"droplet_uri": "source-url-1",
								"stack": "stack-1",
								"start_command": "start-command-1"
							},
							{
								"disk_mb": 2048,
								"environment": [
									{ "name": "env-key-3", "value": "env-value-3" },
									{ "name": "env-key-4", "value": "env-value-4" }
								],
								"file_descriptors": 32,
								"num_instances": 4,
								"log_guid": "log-guid-2",
								"memory_mb": 512,
								"process_guid": "process-guid-2",
								"routes": [ "route-3", "route-4" ],
								"droplet_uri": "source-url-2",
								"stack": "stack-2",
								"start_command": "start-command-2"
							}
						]
					}`),
				),
				ghttp.CombineHandlers(
					ghttp.VerifyRequest("GET", "/internal/bulk/apps", `batch_size=2&token={"id":"the-token-id"}`),
					ghttp.VerifyHeaderKV("Authorization", "Basic "+base64Encode("the-username:the-password")),
					ghttp.RespondWith(200, `{
						"token": {"id":"another-token-id"},
						"apps": [
							{
								"disk_mb": 512,
								"environment": [],
								"file_descriptors": 8,
								"num_instances": 4,
								"log_guid": "log-guid-3",
								"memory_mb": 128,
								"process_guid": "process-guid-3",
								"routes": [],
								"droplet_uri": "source-url-3",
								"stack": "stack-3",
								"start_command": "start-command-3"
							}
						]
					}`),
				),
			)

			results := make(chan cc_messages.DesireAppRequestFromCC, 3)
			httpClient := &http.Client{Timeout: time.Second}
			err := fetcher.Fetch(results, httpClient)
			Ω(err).ShouldNot(HaveOccurred())

			Ω(fakeCC.ReceivedRequests()).Should(HaveLen(2))

			Ω(results).Should(Receive(Equal(cc_messages.DesireAppRequestFromCC{
				ProcessGuid:     "process-guid-1",
				NumInstances:    2,
				Stack:           "stack-1",
				MemoryMB:        256,
				DiskMB:          1024,
				FileDescriptors: 16,
				DropletUri:      "source-url-1",
				StartCommand:    "start-command-1",
				Environment: cc_messages.Environment{
					{Name: "env-key-1", Value: "env-value-1"},
					{Name: "env-key-2", Value: "env-value-2"},
				},
				Routes:  []string{"route-1", "route-2"},
				LogGuid: "log-guid-1",
			})))

			Ω(results).Should(Receive(Equal(cc_messages.DesireAppRequestFromCC{
				ProcessGuid:     "process-guid-2",
				NumInstances:    4,
				Stack:           "stack-2",
				MemoryMB:        512,
				DiskMB:          2048,
				FileDescriptors: 32,
				DropletUri:      "source-url-2",
				StartCommand:    "start-command-2",
				Environment: cc_messages.Environment{
					{Name: "env-key-3", Value: "env-value-3"},
					{Name: "env-key-4", Value: "env-value-4"},
				},
				Routes:  []string{"route-3", "route-4"},
				LogGuid: "log-guid-2",
			})))

			Ω(results).Should(Receive(Equal(cc_messages.DesireAppRequestFromCC{
				ProcessGuid:     "process-guid-3",
				NumInstances:    4,
				Stack:           "stack-3",
				MemoryMB:        128,
				DiskMB:          512,
				FileDescriptors: 8,
				DropletUri:      "source-url-3",
				StartCommand:    "start-command-3",
				Environment:     cc_messages.Environment{},
				Routes:          []string{},
				LogGuid:         "log-guid-3",
			})))

			Ω(results).Should(BeClosed())
		})
	})

	Context("when the API times out", func() {
		ccResponseTime := 100 * time.Millisecond

		BeforeEach(func() {
			fakeCC.AppendHandlers(func(w http.ResponseWriter, req *http.Request) {
				time.Sleep(ccResponseTime)

				w.Write([]byte(`{
						"token": {"id":"another-token-id"},
						"apps": [
							{
								"disk_mb": 512,
								"environment": [],
								"file_descriptors": 8,
								"num_instances": 4,
								"log_guid": "log-guid-3",
								"memory_mb": 128,
								"process_guid": "process-guid-3",
								"routes": [],
								"droplet_uri": "source-url-3",
								"stack": "stack-3",
								"start_command": "start-command-3"
							}
						]
					}`))
			})
		})

		It("returns an error", func() {
			results := make(chan cc_messages.DesireAppRequestFromCC, 3)
			httpClient := &http.Client{Timeout: ccResponseTime / 2}
			err := fetcher.Fetch(results, httpClient)
			Ω(err).Should(HaveOccurred())
		})

		It("closes the results channel", func() {
			results := make(chan cc_messages.DesireAppRequestFromCC, 3)
			httpClient := &http.Client{Timeout: ccResponseTime / 2}
			go fetcher.Fetch(results, httpClient)
			Eventually(results).Should(BeClosed())
		})
	})

	Context("when the API returns an error response", func() {
		BeforeEach(func() {
			fakeCC.AppendHandlers(ghttp.RespondWith(403, ""))
		})

		It("returns an error", func() {
			results := make(chan cc_messages.DesireAppRequestFromCC, 3)
			err := fetcher.Fetch(results, http.DefaultClient)
			Ω(err).Should(HaveOccurred())
		})

		It("closes the results channel", func() {
			results := make(chan cc_messages.DesireAppRequestFromCC, 3)
			go fetcher.Fetch(results, http.DefaultClient)
			Eventually(results).Should(BeClosed())
		})
	})

	Context("when the server responds with invalid JSON", func() {
		BeforeEach(func() {
			fakeCC.AppendHandlers(ghttp.RespondWith(200, "{"))
		})

		It("returns an error", func() {
			results := make(chan cc_messages.DesireAppRequestFromCC, 3)
			err := fetcher.Fetch(results, http.DefaultClient)
			Ω(err).Should(HaveOccurred())
		})

		It("closes the results channel", func() {
			results := make(chan cc_messages.DesireAppRequestFromCC, 3)
			go fetcher.Fetch(results, http.DefaultClient)
			Eventually(results).Should(BeClosed())
		})
	})
})

func base64Encode(input string) string {
	return base64.StdEncoding.EncodeToString([]byte(input))
}
