package main_test

import (
	"encoding/json"
	"os"
	"strings"
	"time"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
	"github.com/onsi/gomega/gexec"
	"github.com/onsi/gomega/ghttp"
	"github.com/pivotal-golang/lager/lagertest"
	"github.com/tedsuo/ifrit"

	Bbs "github.com/cloudfoundry-incubator/runtime-schema/bbs"
	"github.com/cloudfoundry-incubator/runtime-schema/bbs/shared"
	"github.com/cloudfoundry-incubator/runtime-schema/cc_messages"
	"github.com/cloudfoundry-incubator/runtime-schema/models"
	"github.com/cloudfoundry/gunk/timeprovider"
	"github.com/cloudfoundry/storeadapter"

	"github.com/cloudfoundry-incubator/nsync/integration/runner"
	"github.com/cloudfoundry-incubator/nsync/recipebuilder"
)

var _ = Describe("Syncing desired state with CC", func() {
	var (
		bbs    *Bbs.BBS
		fakeCC *ghttp.Server

		run     ifrit.Runner
		process ifrit.Process

		freshnessTTL time.Duration

		bulkerLockName    = "nsync_bulker_lock"
		pollingInterval   = time.Second
		heartbeatInterval time.Duration
	)

	startBulker := func() {
		process = ifrit.Invoke(run)
		// time.Sleep(pollingInterval)
	}

	checkFreshness := func() []string {
		domains, err := bbs.GetAllFreshness()
		Ω(err).ShouldNot(HaveOccurred())
		return domains
	}

	itIsNotFresh := func() {
		It("is not fresh", func() {
			Eventually(checkFreshness, 2*freshnessTTL).ShouldNot(ContainElement("cf-apps"))
		})
	}

	BeforeEach(func() {
		bbs = Bbs.NewBBS(etcdClient, timeprovider.NewTimeProvider(), lagertest.NewTestLogger("test"))

		fakeCC = ghttp.NewServer()

		freshnessTTL = 1 * time.Second
		heartbeatInterval = 30 * time.Second

		fakeCC.RouteToHandler("GET", "/internal/bulk/apps",
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
							"start_command": "start-command-1",
							"execution_metadata": "execution-metadata-1"
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
							"start_command": "start-command-2",
							"execution_metadata": "execution-metadata-2"
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
							"start_command": "start-command-3",
							"execution_metadata": "execution-metadata-3"
						}
					]
				}`),
		)
	})

	JustBeforeEach(func() {
		run = runner.NewRunner(
			"nsync.bulker.started",
			bulkerPath,
			"-ccBaseURL", fakeCC.URL(),
			"-etcdCluster", strings.Join(etcdRunner.NodeURLS(), ","),
			"-pollingInterval", pollingInterval.String(),
			"-freshnessTTL", freshnessTTL.String(),
			"-bulkBatchSize", "10",
			"-circuses", `{"some-stack": "some-health-check.tar.gz"}`,
			"-dockerCircusPath", "the/docker/circus/path.tgz",
			"-heartbeatInterval", heartbeatInterval.String(),
		)
	})

	AfterEach(func() {
		defer fakeCC.Close()
		process.Signal(os.Interrupt)
		Eventually(process.Wait(), 5).Should(Receive())
	})

	Describe("when the CC polling interval elapses", func() {
		var desired1, desired2 models.DesiredLRP

		JustBeforeEach(func() {
			startBulker()
		})

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
				"start_command": "start-command-1",
				"execution_metadata": "execution-metadata-1"
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
				"start_command": "start-command-1",
				"execution_metadata": "execution-metadata-1"
			}`), &existing2)
			Ω(err).ShouldNot(HaveOccurred())

			builder := recipebuilder.New(
				"some.rep.address",
				map[string]string{"some-stack": "some-health-check.tar.gz"},
				"the/docker/circus/path.tgz",
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
		})

		Context("once the state has been synced with CC", func() {
			JustBeforeEach(func() {
				Eventually(bbs.GetAllDesiredLRPs, 5).Should(HaveLen(3))
			})

			It("it (adds), (updates), and (removes extra) LRPs", func() {
				nowDesired, err := bbs.GetAllDesiredLRPs()
				Ω(err).ShouldNot(HaveOccurred())

				nofile := uint64(16)

				Ω(nowDesired).Should(ContainElement(models.DesiredLRP{
					ProcessGuid: "process-guid-1",
					Domain:      "cf-apps",
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
									Args: []string{"/app", "start-command-1", "execution-metadata-1"},
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
					Domain:      "cf-apps",
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
									Args: []string{"/app", "start-command-2", "execution-metadata-2"},
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
					Domain:      "cf-apps",
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
									Args: []string{"/app", "start-command-3", "execution-metadata-3"},
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

			Describe("the freshness", func() {
				Context("when cc is available", func() {
					It("bumps the freshness", func() {
						Eventually(checkFreshness).Should(ContainElement("cf-apps"))
					})
				})

				Context("when cc stops being available", func() {
					It("stops bumping the freshness", func() {
						Eventually(checkFreshness).Should(ContainElement("cf-apps"))
						fakeCC.HTTPTestServer.Close()
						Eventually(checkFreshness, 2*freshnessTTL).ShouldNot(ContainElement("cf-apps"))
					})
				})
			})
		})

		Context("when LRPs in a different domain exist", func() {
			var otherDomainDesired models.DesiredLRP

			BeforeEach(func() {
				otherDomainDesired = models.DesiredLRP{
					ProcessGuid: "some-other-lrp",
					Domain:      "some-domain",
					Stack:       "some-stack",
					Actions: []models.ExecutorAction{
						{
							Action: models.RunAction{
								Path: "reboot",
							},
						},
					},
				}

				err := bbs.DesireLRP(otherDomainDesired)
				Ω(err).ShouldNot(HaveOccurred())
			})

			It("leaves them alone", func() {
				Eventually(bbs.GetAllDesiredLRPs, 5).Should(HaveLen(4))

				nowDesired, err := bbs.GetAllDesiredLRPs()
				Ω(err).ShouldNot(HaveOccurred())

				Ω(nowDesired).Should(ContainElement(otherDomainDesired))
			})
		})
	})

	Context("when the bulker loses the lock", func() {
		BeforeEach(func() {
			heartbeatInterval = 1 * time.Second
		})

		JustBeforeEach(func() {
			startBulker()
			time.Sleep(50 * time.Millisecond)

			err := etcdClient.Update(storeadapter.StoreNode{
				Key:   shared.LockSchemaPath(bulkerLockName),
				Value: []byte("something-else"),
			})
			Ω(err).ShouldNot(HaveOccurred())

			time.Sleep(pollingInterval + 10*time.Millisecond)
		})

		itIsNotFresh()

		It("exits with an error", func() {
			Eventually(run).Should(gexec.Exit(1))
		})
	})

	Context("when the bulker initially does not have the lock", func() {
		BeforeEach(func() {
			heartbeatInterval = 1 * time.Second
			pollingInterval = 100 * time.Millisecond

			err := etcdClient.Create(storeadapter.StoreNode{
				Key:   shared.LockSchemaPath(bulkerLockName),
				Value: []byte("something-else"),
			})
			Ω(err).ShouldNot(HaveOccurred())
		})

		JustBeforeEach(func() {
			startBulker()
		})

		itIsNotFresh()

		Context("when the lock becomes available", func() {
			BeforeEach(func() {
				err := etcdClient.Delete(shared.LockSchemaPath(bulkerLockName))
				Ω(err).ShouldNot(HaveOccurred())

				time.Sleep(pollingInterval + 10*time.Millisecond)
			})

			It("is fresh", func() {
				Eventually(checkFreshness, 2*freshnessTTL).Should(ContainElement("cf-apps"))
			})
		})
	})
})
