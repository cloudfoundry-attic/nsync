package main_test

import (
	"encoding/json"
	"os/exec"
	"strings"
	"time"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
	"github.com/onsi/gomega/ghttp"
	"github.com/pivotal-golang/lager/lagertest"
	"github.com/tedsuo/ifrit"
	"github.com/tedsuo/ifrit/ginkgomon"

	Bbs "github.com/cloudfoundry-incubator/runtime-schema/bbs"
	"github.com/cloudfoundry-incubator/runtime-schema/bbs/shared"
	"github.com/cloudfoundry-incubator/runtime-schema/cc_messages"
	"github.com/cloudfoundry-incubator/runtime-schema/models"
	"github.com/cloudfoundry/gunk/timeprovider"
	"github.com/cloudfoundry/storeadapter"

	"github.com/cloudfoundry-incubator/nsync/recipebuilder"
)

var _ = Describe("Syncing desired state with CC", func() {
	var (
		bbs    *Bbs.BBS
		fakeCC *ghttp.Server

		process ifrit.Process

		freshnessTTL time.Duration

		bulkerLockName    = "nsync_bulker_lock"
		pollingInterval   time.Duration
		heartbeatInterval time.Duration
	)

	startBulker := func(check bool) ifrit.Process {
		runner := ginkgomon.New(ginkgomon.Config{
			Name:          "nsync-bulker",
			AnsiColorCode: "97m",
			StartCheck:    "nsync.bulker.started",
			Command: exec.Command(
				bulkerPath,
				"-ccBaseURL", fakeCC.URL(),
				"-etcdCluster", strings.Join(etcdRunner.NodeURLS(), ","),
				"-pollingInterval", pollingInterval.String(),
				"-freshnessTTL", freshnessTTL.String(),
				"-bulkBatchSize", "10",
				"-circuses", `{"some-stack": "some-health-check.tar.gz"}`,
				"-dockerCircusPath", "the/docker/circus/path.tgz",
				"-fileServerURL", "http://file-server.com",
				"-heartbeatInterval", heartbeatInterval.String(),
			),
		})

		if !check {
			runner.StartCheck = ""
		}

		return ginkgomon.Invoke(runner)
	}

	checkFreshness := func() []string {
		freshnesses, err := bbs.Freshnesses()
		Ω(err).ShouldNot(HaveOccurred())

		domains := make([]string, 0, len(freshnesses))

		for _, freshness := range freshnesses {
			domains = append(domains, freshness.Domain)
		}

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

		pollingInterval = 500 * time.Millisecond
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

	AfterEach(func() {
		defer fakeCC.Close()
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
				"process_guid": "process-guid-2",
				"routes": [ "route-1", "route-2" ],
				"droplet_uri": "source-url-1",
				"stack": "some-stack",
				"start_command": "start-command-1",
				"execution_metadata": "execution-metadata-1"
			}`), &existing2)
			Ω(err).ShouldNot(HaveOccurred())

			builder := recipebuilder.New(
				map[string]string{"some-stack": "some-health-check.tar.gz"},
				"the/docker/circus/path.tgz",
				"http://file-server.com",
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

		JustBeforeEach(func() {
			process = startBulker(true)
		})

		AfterEach(func() {
			ginkgomon.Interrupt(process)
		})

		Context("once the state has been synced with CC", func() {
			JustBeforeEach(func() {
				Eventually(bbs.DesiredLRPs, 5).Should(HaveLen(3))
			})

			It("it (adds), (updates), and (removes extra) LRPs", func() {
				nowDesired, err := bbs.DesiredLRPs()
				Ω(err).ShouldNot(HaveOccurred())

				nofile := uint64(16)

				expectedSetupActions1 := models.Serial(
					&models.DownloadAction{
						From:     "http://file-server.com/v1/static/some-health-check.tar.gz",
						To:       "/tmp/circus",
						CacheKey: "",
					},
					&models.DownloadAction{
						From:     "source-url-1",
						To:       ".",
						CacheKey: "droplets-process-guid-1",
					},
				)

				expectedSetupActions2 := models.Serial(
					&models.DownloadAction{
						From:     "http://file-server.com/v1/static/some-health-check.tar.gz",
						To:       "/tmp/circus",
						CacheKey: "",
					},
					&models.DownloadAction{
						From:     "source-url-2",
						To:       ".",
						CacheKey: "droplets-process-guid-2",
					},
				)

				expectedSetupActions3 := models.Serial(
					&models.DownloadAction{
						From:     "http://file-server.com/v1/static/some-health-check.tar.gz",
						To:       "/tmp/circus",
						CacheKey: "",
					},
					&models.DownloadAction{
						From:     "source-url-3",
						To:       ".",
						CacheKey: "droplets-process-guid-3",
					},
				)

				Ω(nowDesired).Should(ContainElement(models.DesiredLRP{
					ProcessGuid: "process-guid-1",
					Domain:      "cf-apps",
					Instances:   42,
					Stack:       "some-stack",
					Setup:       expectedSetupActions1,
					Action: &models.RunAction{
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
						LogSource:      recipebuilder.AppLogSource,
					},
					Monitor: &models.TimeoutAction{
						Action: &models.RunAction{
							Path:      "/tmp/circus/spy",
							Args:      []string{"-addr=:8080"},
							LogSource: recipebuilder.HealthLogSource,
						},
						Timeout:   recipebuilder.MonitorTimeout,
						LogSource: recipebuilder.HealthLogSource,
					},
					DiskMB:    1024,
					MemoryMB:  256,
					CPUWeight: 1,
					Ports: []uint32{
						8080,
					},
					Routes:    []string{"route-1", "route-2", "new-route"},
					LogGuid:   "log-guid-1",
					LogSource: recipebuilder.LRPLogSource,
				}))

				nofile = 32

				Ω(nowDesired).Should(ContainElement(models.DesiredLRP{
					ProcessGuid: "process-guid-2",
					Domain:      "cf-apps",
					Instances:   4,
					Stack:       "some-stack",
					Setup:       expectedSetupActions2,
					Action: &models.RunAction{
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
						LogSource:      recipebuilder.AppLogSource,
					},
					Monitor: &models.TimeoutAction{
						Action: &models.RunAction{
							Path:      "/tmp/circus/spy",
							Args:      []string{"-addr=:8080"},
							LogSource: recipebuilder.HealthLogSource,
						},
						Timeout:   recipebuilder.MonitorTimeout,
						LogSource: recipebuilder.HealthLogSource,
					},
					DiskMB:    2048,
					MemoryMB:  512,
					CPUWeight: 4,
					Ports: []uint32{
						8080,
					},
					Routes:    []string{"route-3", "route-4"},
					LogGuid:   "log-guid-2",
					LogSource: recipebuilder.LRPLogSource,
				}))

				nofile = 8
				Ω(nowDesired).Should(ContainElement(models.DesiredLRP{
					ProcessGuid: "process-guid-3",
					Domain:      "cf-apps",
					Instances:   4,
					Stack:       "some-stack",
					Setup:       expectedSetupActions3,
					Action: &models.RunAction{
						Path: "/tmp/circus/soldier",
						Args: []string{"/app", "start-command-3", "execution-metadata-3"},
						Env: []models.EnvironmentVariable{
							{Name: "PORT", Value: "8080"},
							{Name: "VCAP_APP_PORT", Value: "8080"},
							{Name: "VCAP_APP_HOST", Value: "0.0.0.0"},
						},
						ResourceLimits: models.ResourceLimits{Nofile: &nofile},
						LogSource:      recipebuilder.AppLogSource,
					},
					Monitor: &models.TimeoutAction{
						Action: &models.RunAction{
							Path:      "/tmp/circus/spy",
							Args:      []string{"-addr=:8080"},
							LogSource: recipebuilder.HealthLogSource,
						},
						Timeout:   recipebuilder.MonitorTimeout,
						LogSource: recipebuilder.HealthLogSource,
					},
					DiskMB:    512,
					MemoryMB:  128,
					CPUWeight: 1,
					Ports: []uint32{
						8080,
					},
					Routes:    []string{},
					LogGuid:   "log-guid-3",
					LogSource: recipebuilder.LRPLogSource,
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
					Instances:   1,
					Action: &models.RunAction{
						Path: "reboot",
					},
				}

				err := bbs.DesireLRP(otherDomainDesired)
				Ω(err).ShouldNot(HaveOccurred())
			})

			It("leaves them alone", func() {
				Eventually(bbs.DesiredLRPs, 5).Should(HaveLen(4))

				nowDesired, err := bbs.DesiredLRPs()
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
			process = startBulker(true)

			Eventually(checkFreshness, 2*freshnessTTL).Should(ContainElement("cf-apps"))

			err := etcdClient.Update(storeadapter.StoreNode{
				Key:   shared.LockSchemaPath(bulkerLockName),
				Value: []byte("something-else"),
			})
			Ω(err).ShouldNot(HaveOccurred())
		})

		AfterEach(func() {
			ginkgomon.Interrupt(process)
		})

		itIsNotFresh()

		It("exits with an error", func() {
			Eventually(process.Wait(), 2*freshnessTTL).Should(Receive(HaveOccurred()))
		})
	})

	Context("when the bulker initially does not have the lock", func() {
		BeforeEach(func() {
			heartbeatInterval = 1 * time.Second

			err := etcdClient.Create(storeadapter.StoreNode{
				Key:   shared.LockSchemaPath(bulkerLockName),
				Value: []byte("something-else"),
			})
			Ω(err).ShouldNot(HaveOccurred())
		})

		JustBeforeEach(func() {
			process = startBulker(false)
		})

		AfterEach(func() {
			ginkgomon.Interrupt(process)
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
