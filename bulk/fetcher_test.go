package bulk_test

import (
	"encoding/base64"
	"net/http"
	"time"

	. "github.com/cloudfoundry-incubator/nsync/bulk"
	"github.com/cloudfoundry-incubator/runtime-schema/models"
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
								"instances": 2,
								"log_guid": "log-guid-1",
								"memory_mb": 256,
								"process_guid": "process-guid-1",
								"routes": [ "route-1", "route-2" ],
								"source_url": "source-url-1",
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
								"instances": 4,
								"log_guid": "log-guid-2",
								"memory_mb": 512,
								"process_guid": "process-guid-2",
								"routes": [ "route-3", "route-4" ],
								"source_url": "source-url-2",
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
								"instances": 4,
								"log_guid": "log-guid-3",
								"memory_mb": 128,
								"process_guid": "process-guid-3",
								"routes": [],
								"source_url": "source-url-3",
								"stack": "stack-3",
								"start_command": "start-command-3"
							}
						]
					}`),
				),
			)

			results := make(chan models.DesiredLRP, 3)
			err := fetcher.Fetch(results, time.Second)
			Ω(err).ShouldNot(HaveOccurred())

			Ω(fakeCC.ReceivedRequests()).Should(HaveLen(2))

			Ω(results).Should(Receive(Equal(models.DesiredLRP{
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
			})))

			Ω(results).Should(Receive(Equal(models.DesiredLRP{
				ProcessGuid:     "process-guid-2",
				Instances:       4,
				Stack:           "stack-2",
				MemoryMB:        512,
				DiskMB:          2048,
				FileDescriptors: 32,
				Source:          "source-url-2",
				StartCommand:    "start-command-2",
				Environment: []models.EnvironmentVariable{
					{Name: "env-key-3", Value: "env-value-3"},
					{Name: "env-key-4", Value: "env-value-4"},
				},
				Routes:  []string{"route-3", "route-4"},
				LogGuid: "log-guid-2",
			})))

			Ω(results).Should(Receive(Equal(models.DesiredLRP{
				ProcessGuid:     "process-guid-3",
				Instances:       4,
				Stack:           "stack-3",
				MemoryMB:        128,
				DiskMB:          512,
				FileDescriptors: 8,
				Source:          "source-url-3",
				StartCommand:    "start-command-3",
				Environment:     []models.EnvironmentVariable{},
				Routes:          []string{},
				LogGuid:         "log-guid-3",
			})))

			Ω(results).Should(BeClosed())
		})
	})

	Context("when the API times out", func() {
		BeforeEach(func() {
			fakeCC.AppendHandlers(func(w http.ResponseWriter, req *http.Request) {
				time.Sleep(100 * time.Millisecond)
				w.Write([]byte(`{
						"token": {"id":"another-token-id"},
						"apps": [
							{
								"disk_mb": 512,
								"environment": [],
								"file_descriptors": 8,
								"instances": 4,
								"log_guid": "log-guid-3",
								"memory_mb": 128,
								"process_guid": "process-guid-3",
								"routes": [],
								"source_url": "source-url-3",
								"stack": "stack-3",
								"start_command": "start-command-3"
							}
						]
					}`))
			})
		})

		It("returns an error", func() {
			results := make(chan models.DesiredLRP, 3)
			err := fetcher.Fetch(results, 50*time.Millisecond)
			Ω(err).Should(HaveOccurred())
		})
	})

	Context("when the API returns an error response", func() {
		BeforeEach(func() {
			fakeCC.AppendHandlers(ghttp.RespondWith(403, ""))
		})

		It("returns an error", func() {
			results := make(chan models.DesiredLRP, 3)
			err := fetcher.Fetch(results, time.Second)
			Ω(err).Should(HaveOccurred())
		})
	})

	Context("when the server responds with invalid JSON", func() {
		BeforeEach(func() {
			fakeCC.AppendHandlers(ghttp.RespondWith(200, "{"))
		})

		It("returns an error", func() {
			results := make(chan models.DesiredLRP, 3)
			err := fetcher.Fetch(results, time.Second)
			Ω(err).Should(HaveOccurred())
		})
	})
})

func base64Encode(input string) string {
	return base64.StdEncoding.EncodeToString([]byte(input))
}
