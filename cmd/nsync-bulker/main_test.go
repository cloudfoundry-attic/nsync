package main_test

import (
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"time"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
	"github.com/onsi/gomega/ghttp"
	"github.com/pivotal-golang/lager"
	"github.com/pivotal-golang/lager/lagertest"
	"github.com/tedsuo/ifrit"
	"github.com/tedsuo/ifrit/ginkgomon"

	"github.com/cloudfoundry-incubator/receptor"
	receptorrunner "github.com/cloudfoundry-incubator/receptor/cmd/receptor/testrunner"
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
		bbs            *Bbs.BBS
		fakeCC         *ghttp.Server
		receptorClient receptor.Client

		receptorProcess ifrit.Process
		process         ifrit.Process

		domainTTL time.Duration

		bulkerLockName    = "nsync_bulker_lock"
		pollingInterval   time.Duration
		heartbeatInterval time.Duration

		logger lager.Logger
	)

	startReceptor := func() ifrit.Process {
		return ginkgomon.Invoke(receptorrunner.New(receptorPath, receptorrunner.Args{
			Address:     fmt.Sprintf("127.0.0.1:%d", receptorPort),
			EtcdCluster: strings.Join(etcdRunner.NodeURLS(), ","),
		}))
	}

	startBulker := func(check bool) ifrit.Process {
		runner := ginkgomon.New(ginkgomon.Config{
			Name:          "nsync-bulker",
			AnsiColorCode: "97m",
			StartCheck:    "nsync.bulker.started",
			Command: exec.Command(
				bulkerPath,
				"-etcdCluster", strings.Join(etcdRunner.NodeURLS(), ","),
				"-ccBaseURL", fakeCC.URL(),
				"-pollingInterval", pollingInterval.String(),
				"-domainTTL", domainTTL.String(),
				"-bulkBatchSize", "10",
				"-circuses", `{"some-stack": "some-health-check.tar.gz"}`,
				"-dockerCircusPath", "the/docker/circus/path.tgz",
				"-fileServerURL", "http://file-server.com",
				"-heartbeatInterval", heartbeatInterval.String(),
				"-diegoAPIURL", fmt.Sprintf("http://127.0.0.1:%d", receptorPort),
			),
		})

		if !check {
			runner.StartCheck = ""
		}

		return ginkgomon.Invoke(runner)
	}

	checkDomains := func() []string {
		domains, err := bbs.Domains()
		Ω(err).ShouldNot(HaveOccurred())

		return domains
	}

	itIsMissingDomain := func() {
		It("is missing domain", func() {
			Eventually(checkDomains, 5*domainTTL).ShouldNot(ContainElement("cf-apps"))
		})
	}

	BeforeEach(func() {
		logger = lagertest.NewTestLogger("test")
		bbs = Bbs.NewBBS(etcdClient, timeprovider.NewTimeProvider(), logger)

		fakeCC = ghttp.NewServer()
		receptorProcess = startReceptor()

		receptorClient = receptor.NewClient(fmt.Sprintf("http://127.0.0.1:%d", receptorPort))

		pollingInterval = 500 * time.Millisecond
		domainTTL = 1 * time.Second
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
							"disk_mb": 1024,
							"environment": [
								{ "name": "env-key-1", "value": "env-value-1" },
								{ "name": "env-key-2", "value": "env-value-2" }
							],
							"file_descriptors": 16,
							"num_instances": 4,
							"log_guid": "log-guid-1",
							"memory_mb": 256,
							"process_guid": "process-guid-2",
							"routes": [ "route-3", "route-4" ],
							"droplet_uri": "source-url-1",
							"stack": "some-stack",
							"start_command": "start-command-1",
							"execution_metadata": "execution-metadata-1"
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
		ginkgomon.Interrupt(receptorProcess)
	})

	Describe("when the CC polling interval elapses", func() {
		var desired1, desired2 *receptor.DesiredLRPCreateRequest

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

			desired1, err = builder.Build(&existing1)
			Ω(err).ShouldNot(HaveOccurred())

			desired2, err = builder.Build(&existing2)
			Ω(err).ShouldNot(HaveOccurred())

			err = receptorClient.CreateDesiredLRP(*desired1)
			Ω(err).ShouldNot(HaveOccurred())

			err = receptorClient.CreateDesiredLRP(*desired2)
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
						From:     "source-url-1",
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

				Eventually(bbs.DesiredLRPs).Should(ContainElement(models.DesiredLRP{
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
						},
						ResourceLimits: models.ResourceLimits{Nofile: &nofile},
						LogSource:      recipebuilder.AppLogSource,
					},
					Monitor: &models.RunAction{
						Path:      "/tmp/circus/spy",
						Args:      []string{"-addr=:8080"},
						LogSource: recipebuilder.HealthLogSource,
					},
					DiskMB:    1024,
					MemoryMB:  256,
					CPUWeight: 1,
					Ports: []uint32{
						8080,
					},
					Routes:     []string{"route-1", "route-2", "new-route"},
					LogGuid:    "log-guid-1",
					LogSource:  recipebuilder.LRPLogSource,
					Privileged: true,
				}))

				nofile = 16
				Eventually(bbs.DesiredLRPs).Should(ContainElement(models.DesiredLRP{
					ProcessGuid: "process-guid-2",
					Domain:      "cf-apps",
					Instances:   4,
					Stack:       "some-stack",
					Setup:       expectedSetupActions2,
					Action: &models.RunAction{
						Path: "/tmp/circus/soldier",
						Args: []string{"/app", "start-command-1", "execution-metadata-1"},
						Env: []models.EnvironmentVariable{
							{Name: "env-key-1", Value: "env-value-1"},
							{Name: "env-key-2", Value: "env-value-2"},
							{Name: "PORT", Value: "8080"},
						},
						ResourceLimits: models.ResourceLimits{Nofile: &nofile},
						LogSource:      recipebuilder.AppLogSource,
					},
					Monitor: &models.RunAction{
						Path:      "/tmp/circus/spy",
						Args:      []string{"-addr=:8080"},
						LogSource: recipebuilder.HealthLogSource,
					},
					DiskMB:    1024,
					MemoryMB:  256,
					CPUWeight: 1,
					Ports: []uint32{
						8080,
					},
					Routes:     []string{"route-3", "route-4"},
					LogGuid:    "log-guid-1",
					LogSource:  recipebuilder.LRPLogSource,
					Privileged: true,
				}))

				nofile = 8
				Eventually(bbs.DesiredLRPs).Should(ContainElement(models.DesiredLRP{
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
						},
						ResourceLimits: models.ResourceLimits{Nofile: &nofile},
						LogSource:      recipebuilder.AppLogSource,
					},
					Monitor: &models.RunAction{
						Path:      "/tmp/circus/spy",
						Args:      []string{"-addr=:8080"},
						LogSource: recipebuilder.HealthLogSource,
					},
					DiskMB:    512,
					MemoryMB:  128,
					CPUWeight: 1,
					Ports: []uint32{
						8080,
					},
					Routes:     []string{},
					LogGuid:    "log-guid-3",
					LogSource:  recipebuilder.LRPLogSource,
					Privileged: true,
				}))
			})

			Describe("domains", func() {
				Context("when cc is available", func() {
					It("updates the domains", func() {
						Eventually(checkDomains).Should(ContainElement("cf-apps"))
					})
				})

				Context("when cc stops being available", func() {
					It("stops updating the domains", func() {
						Eventually(checkDomains).Should(ContainElement("cf-apps"))
						fakeCC.HTTPTestServer.Close()
						Eventually(checkDomains, 2*domainTTL).ShouldNot(ContainElement("cf-apps"))
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

				err := bbs.DesireLRP(logger, otherDomainDesired)
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

			Eventually(checkDomains, 2*domainTTL).Should(ContainElement("cf-apps"))

			err := etcdClient.Update(storeadapter.StoreNode{
				Key:   shared.LockSchemaPath(bulkerLockName),
				Value: []byte("something-else"),
			})
			Ω(err).ShouldNot(HaveOccurred())
		})

		AfterEach(func() {
			ginkgomon.Interrupt(process)
		})

		itIsMissingDomain()

		It("exits with an error", func() {
			Eventually(process.Wait(), 2*domainTTL).Should(Receive(HaveOccurred()))
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

		itIsMissingDomain()

		Context("when the lock becomes available", func() {
			BeforeEach(func() {
				err := etcdClient.Delete(shared.LockSchemaPath(bulkerLockName))
				Ω(err).ShouldNot(HaveOccurred())

				time.Sleep(pollingInterval + 10*time.Millisecond)
			})

			It("is updated", func() {
				Eventually(checkDomains, 2*domainTTL).Should(ContainElement("cf-apps"))
			})
		})
	})
})
