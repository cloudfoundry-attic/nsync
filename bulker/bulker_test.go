package main_test

import (
	"os"
	"strings"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
	"github.com/onsi/gomega/ghttp"
	"github.com/tedsuo/ifrit"

	Bbs "github.com/cloudfoundry-incubator/runtime-schema/bbs"
	"github.com/cloudfoundry-incubator/runtime-schema/models"
	steno "github.com/cloudfoundry/gosteno"
	"github.com/cloudfoundry/gunk/timeprovider"

	"github.com/cloudfoundry-incubator/nsync/integration/runner"
)

var _ = Describe("Syncing desired state with CC", func() {
	var (
		bbs    *Bbs.BBS
		fakeCC *ghttp.Server

		run     ifrit.Runner
		process ifrit.Process
	)

	BeforeEach(func() {
		logSink := steno.NewTestingSink()

		steno.Init(&steno.Config{
			Sinks: []steno.Sink{logSink},
		})

		logger := steno.NewLogger("the-logger")
		steno.EnterTestMode()

		bbs = Bbs.NewBBS(etcdRunner.Adapter(), timeprovider.NewTimeProvider(), logger)

		fakeCC = ghttp.NewServer()

		run = runner.NewRunner(
			"nsync.bulker.started",
			bulkerPath,
			"-ccBaseURL", fakeCC.URL(),
			"-etcdCluster", strings.Join(etcdRunner.NodeURLS(), ","),
			"-pollingInterval", "100ms",
			"-bulkBatchSize", "10",
		)
	})

	JustBeforeEach(func() {
		process = ifrit.Envoke(run)
	})

	AfterEach(func() {
		process.Signal(os.Interrupt)
		Eventually(process.Wait(), 5).Should(Receive())
	})

	Describe("when the CC polling interval elapses", func() {
		BeforeEach(func() {
			bbs.DesireLRP(models.DesiredLRP{
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
			})

			bbs.DesireLRP(models.DesiredLRP{
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
			})

			fakeCC.AppendHandlers(ghttp.CombineHandlers(
				ghttp.VerifyRequest("GET", "/internal/bulk/apps"),
				ghttp.RespondWith(200, `{
					"token": {},
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
							"start_command": "the-new-start-command-1"
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
						},
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
			))
		})

		It("gets the desired state of all apps from the CC", func() {
			Eventually(bbs.GetAllDesiredLRPs, 5).Should(Equal([]models.DesiredLRP{
				{
					ProcessGuid:     "process-guid-1",
					Instances:       2,
					Stack:           "stack-1",
					MemoryMB:        256,
					DiskMB:          1024,
					FileDescriptors: 16,
					Source:          "source-url-1",
					StartCommand:    "the-new-start-command-1",
					Environment: []models.EnvironmentVariable{
						{Name: "env-key-1", Value: "env-value-1"},
						{Name: "env-key-2", Value: "env-value-2"},
					},
					Routes:  []string{"route-1", "route-2"},
					LogGuid: "log-guid-1",
				},
				{
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
				},
				{
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
				},
			}))
		})
	})
})
