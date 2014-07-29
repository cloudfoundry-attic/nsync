package main_test

import (
	"encoding/json"
	"os"
	"strings"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
	"github.com/onsi/gomega/ghttp"
	"github.com/pivotal-golang/lager/lagertest"
	"github.com/tedsuo/ifrit"

	Bbs "github.com/cloudfoundry-incubator/runtime-schema/bbs"
	"github.com/cloudfoundry-incubator/runtime-schema/cc_messages"
	"github.com/cloudfoundry-incubator/runtime-schema/models"
	"github.com/cloudfoundry/gunk/timeprovider"

	"github.com/cloudfoundry-incubator/nsync/integration/runner"
	"github.com/cloudfoundry-incubator/nsync/recipebuilder"
)

var _ = Describe("Syncing desired state with CC", func() {
	var (
		bbs    *Bbs.BBS
		fakeCC *ghttp.Server

		run     ifrit.Runner
		process ifrit.Process
	)

	BeforeEach(func() {
		bbs = Bbs.NewBBS(etcdRunner.Adapter(), timeprovider.NewTimeProvider(), lagertest.NewTestLogger("test"))

		fakeCC = ghttp.NewServer()

		run = runner.NewRunner(
			"nsync.bulker.started",
			bulkerPath,
			"-ccBaseURL", fakeCC.URL(),
			"-etcdCluster", strings.Join(etcdRunner.NodeURLS(), ","),
			"-pollingInterval", "100ms",
			"-bulkBatchSize", "10",
			"-circuses", `{"some-stack": "some-health-check.tar.gz"}`,
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
		var desired1, desired2 models.DesiredLRP

		BeforeEach(func() {
			var existing1 cc_messages.DesireAppRequestFromCC
			var existing2 cc_messages.DesireAppRequestFromCC

			// total instances is different in the response from CC
			err := json.Unmarshal([]byte(`{
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
				"stack": "some-stack",
				"start_command": "start-command-1"
			}`), &existing1)
			Ω(err).ShouldNot(HaveOccurred())

			err = json.Unmarshal([]byte(`{
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
				"stack": "some-stack",
				"start_command": "start-command-1"
			}`), &existing2)
			Ω(err).ShouldNot(HaveOccurred())

			builder := recipebuilder.New(
				"some.rep.address",
				map[string]string{"some-stack": "some-health-check.tar.gz"},
				lagertest.NewTestLogger("test"),
			)

			desired1, err = builder.Build(existing1)
			Ω(err).ShouldNot(HaveOccurred())

			desired2, err = builder.Build(existing2)
			Ω(err).ShouldNot(HaveOccurred())

			err = bbs.DesireLRP(desired1)
			Ω(err).ShouldNot(HaveOccurred())

			err = bbs.DesireLRP(desired2)
			Ω(err).ShouldNot(HaveOccurred())

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
							"num_instances": 42,
							"log_guid": "log-guid-1",
							"memory_mb": 256,
							"process_guid": "process-guid-1",
							"routes": [ "route-1", "route-2", "new-route" ],
							"droplet_uri": "source-url-1",
							"stack": "some-stack",
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
							"stack": "some-stack",
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
							"stack": "some-stack",
							"start_command": "start-command-3"
						}
					]
				}`),
			))
		})

		It("gets the desired state of all apps from the CC", func() {
			Eventually(bbs.GetAllDesiredLRPs, 5).Should(HaveLen(3))

			nowDesired, err := bbs.GetAllDesiredLRPs()
			Ω(err).ShouldNot(HaveOccurred())

			nofile := uint64(16)

			Ω(nowDesired).Should(ContainElement(models.DesiredLRP{
				ProcessGuid: "process-guid-1",
				Instances:   42,
				Stack:       "some-stack",
				Actions: []models.ExecutorAction{
					{
						Action: models.DownloadAction{
							From:     "PLACEHOLDER_FILESERVER_URL/v1/static/some-health-check.tar.gz",
							To:       "/tmp/circus",
							Extract:  true,
							CacheKey: "",
						},
					},
					{
						Action: models.DownloadAction{
							From:     "source-url-1",
							To:       ".",
							Extract:  true,
							CacheKey: "droplets-process-guid-1",
						},
					},
					models.Parallel(
						models.ExecutorAction{
							models.RunAction{
								Path: "/tmp/circus/soldier",
								Args: []string{"/app", "start-command-1"},
								Env: []models.EnvironmentVariable{
									{Name: "env-key-1", Value: "env-value-1"},
									{Name: "env-key-2", Value: "env-value-2"},
									{Name: "PORT", Value: "8080"},
									{Name: "VCAP_APP_PORT", Value: "8080"},
									{Name: "VCAP_APP_HOST", Value: "0.0.0.0"},
								},
								ResourceLimits: models.ResourceLimits{Nofile: &nofile},
							},
						},
						models.ExecutorAction{
							models.MonitorAction{
								Action: models.ExecutorAction{
									Action: models.RunAction{
										Path: "/tmp/circus/spy",
										Args: []string{"-addr=:8080"},
									},
								},
								HealthyHook: models.HealthRequest{
									Method: "PUT",
									URL:    "http://127.0.0.1:20515/lrp_running/process-guid-1/PLACEHOLDER_INSTANCE_INDEX/PLACEHOLDER_INSTANCE_GUID",
								},
								HealthyThreshold:   1,
								UnhealthyThreshold: 1,
							},
						},
					),
				},
				DiskMB:   1024,
				MemoryMB: 256,
				Ports: []models.PortMapping{
					{ContainerPort: 8080, HostPort: 0},
				},
				Routes: []string{"route-1", "route-2", "new-route"},
				Log:    models.LogConfig{Guid: "log-guid-1", SourceName: "App"},
			}))

			nofile = 32

			Ω(nowDesired).Should(ContainElement(models.DesiredLRP{
				ProcessGuid: "process-guid-2",
				Instances:   4,
				Stack:       "some-stack",
				Actions: []models.ExecutorAction{
					{
						Action: models.DownloadAction{
							From:     "PLACEHOLDER_FILESERVER_URL/v1/static/some-health-check.tar.gz",
							To:       "/tmp/circus",
							Extract:  true,
							CacheKey: "",
						},
					},
					{
						Action: models.DownloadAction{
							From:     "source-url-2",
							To:       ".",
							Extract:  true,
							CacheKey: "droplets-process-guid-2",
						},
					},
					models.Parallel(
						models.ExecutorAction{
							models.RunAction{
								Path: "/tmp/circus/soldier",
								Args: []string{"/app", "start-command-2"},
								Env: []models.EnvironmentVariable{
									{Name: "env-key-3", Value: "env-value-3"},
									{Name: "env-key-4", Value: "env-value-4"},
									{Name: "PORT", Value: "8080"},
									{Name: "VCAP_APP_PORT", Value: "8080"},
									{Name: "VCAP_APP_HOST", Value: "0.0.0.0"},
								},
								ResourceLimits: models.ResourceLimits{Nofile: &nofile},
							},
						},
						models.ExecutorAction{
							models.MonitorAction{
								Action: models.ExecutorAction{
									Action: models.RunAction{
										Path: "/tmp/circus/spy",
										Args: []string{"-addr=:8080"},
									},
								},
								HealthyHook: models.HealthRequest{
									Method: "PUT",
									URL:    "http://127.0.0.1:20515/lrp_running/process-guid-2/PLACEHOLDER_INSTANCE_INDEX/PLACEHOLDER_INSTANCE_GUID",
								},
								HealthyThreshold:   1,
								UnhealthyThreshold: 1,
							},
						},
					),
				},
				DiskMB:   2048,
				MemoryMB: 512,
				Ports: []models.PortMapping{
					{ContainerPort: 8080, HostPort: 0},
				},
				Routes: []string{"route-3", "route-4"},
				Log:    models.LogConfig{Guid: "log-guid-2", SourceName: "App"},
			}))

			nofile = 8
			Ω(nowDesired).Should(ContainElement(models.DesiredLRP{
				ProcessGuid: "process-guid-3",
				Instances:   4,
				Stack:       "some-stack",
				Actions: []models.ExecutorAction{
					{
						Action: models.DownloadAction{
							From:     "PLACEHOLDER_FILESERVER_URL/v1/static/some-health-check.tar.gz",
							To:       "/tmp/circus",
							Extract:  true,
							CacheKey: "",
						},
					},
					{
						Action: models.DownloadAction{
							From:     "source-url-3",
							To:       ".",
							Extract:  true,
							CacheKey: "droplets-process-guid-3",
						},
					},
					models.Parallel(
						models.ExecutorAction{
							models.RunAction{
								Path: "/tmp/circus/soldier",
								Args: []string{"/app", "start-command-3"},
								Env: []models.EnvironmentVariable{
									{Name: "PORT", Value: "8080"},
									{Name: "VCAP_APP_PORT", Value: "8080"},
									{Name: "VCAP_APP_HOST", Value: "0.0.0.0"},
								},
								ResourceLimits: models.ResourceLimits{Nofile: &nofile},
							},
						},
						models.ExecutorAction{
							models.MonitorAction{
								Action: models.ExecutorAction{
									Action: models.RunAction{
										Path: "/tmp/circus/spy",
										Args: []string{"-addr=:8080"},
									},
								},
								HealthyHook: models.HealthRequest{
									Method: "PUT",
									URL:    "http://127.0.0.1:20515/lrp_running/process-guid-3/PLACEHOLDER_INSTANCE_INDEX/PLACEHOLDER_INSTANCE_GUID",
								},
								HealthyThreshold:   1,
								UnhealthyThreshold: 1,
							},
						},
					),
				},
				DiskMB:   512,
				MemoryMB: 128,
				Ports: []models.PortMapping{
					{ContainerPort: 8080, HostPort: 0},
				},
				Routes: []string{},
				Log:    models.LogConfig{Guid: "log-guid-3", SourceName: "App"},
			}))
		})
	})
})
