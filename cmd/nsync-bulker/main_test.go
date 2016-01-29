package main_test

import (
	"encoding/json"
	"net/http"
	"os/exec"
	"time"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
	"github.com/onsi/gomega/ghttp"
	"github.com/pivotal-golang/clock"
	"github.com/pivotal-golang/lager"
	"github.com/pivotal-golang/lager/lagertest"
	"github.com/tedsuo/ifrit"
	"github.com/tedsuo/ifrit/ginkgomon"

	"github.com/cloudfoundry-incubator/bbs/models"
	"github.com/cloudfoundry-incubator/diego-ssh/keys"
	"github.com/cloudfoundry-incubator/locket"
	"github.com/cloudfoundry-incubator/routing-info/cfroutes"
	"github.com/cloudfoundry-incubator/routing-info/tcp_routes"
	"github.com/cloudfoundry-incubator/runtime-schema/cc_messages"

	"github.com/cloudfoundry-incubator/nsync/recipebuilder"
)

var _ = Describe("Syncing desired state with CC", func() {
	const interruptTimeout = 2 * time.Second

	var (
		fakeCC *ghttp.Server

		process ifrit.Process

		domainTTL time.Duration

		bulkerLockName    = "nsync_bulker_lock"
		pollingInterval   time.Duration
		heartbeatInterval time.Duration

		logger lager.Logger
	)

	startBulker := func(check bool) ifrit.Process {
		runner := ginkgomon.New(ginkgomon.Config{
			Name:          "nsync-bulker",
			AnsiColorCode: "97m",
			StartCheck:    "nsync.bulker.started",
			Command: exec.Command(
				bulkerPath,
				"-ccBaseURL", fakeCC.URL(),
				"-pollingInterval", pollingInterval.String(),
				"-domainTTL", domainTTL.String(),
				"-bulkBatchSize", "10",
				"-lifecycle", "buildpack/some-stack:some-health-check.tar.gz",
				"-lifecycle", "docker:the/docker/lifecycle/path.tgz",
				"-fileServerURL", "http://file-server.com",
				"-lockRetryInterval", "1s",
				"-consulCluster", consulRunner.ConsulCluster(),
				"-bbsAddress", bbsURL.String(),
			),
		})

		if !check {
			runner.StartCheck = ""
		}

		return ginkgomon.Invoke(runner)
	}

	itIsMissingDomain := func() {
		It("is missing domain", func() {
			Eventually(bbsClient.Domains, 5*domainTTL).ShouldNot(ContainElement("cf-apps"))
		})
	}

	BeforeEach(func() {
		logger = lagertest.NewTestLogger("test")

		fakeCC = ghttp.NewServer()

		pollingInterval = 500 * time.Millisecond
		domainTTL = 1 * time.Second
		heartbeatInterval = 30 * time.Second

		desiredAppResponses := map[string]string{
			"process-guid-1": `{
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
					"routing_info": {
						"http_routes":
							[
								{"hostname": "route-1"},
								{"hostname": "route-2"},
								{"hostname": "new-route"}
							]
					},
					"droplet_uri": "source-url-1",
					"stack": "some-stack",
					"start_command": "start-command-1",
					"execution_metadata": "execution-metadata-1",
					"health_check_timeout_in_seconds": 123456,
					"etag": "1.1"
				}`,
			"process-guid-2": `{
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
					"routing_info": {
						"http_routes": [
								{ "hostname": "route-3", "route_service_url":"https://rs.example.com"}
							]
						},
					"droplet_uri": "source-url-1",
					"stack": "some-stack",
					"start_command": "start-command-1",
					"execution_metadata": "execution-metadata-1",
					"health_check_timeout_in_seconds": 123456,
					"etag": "2.1"
				}`,
			"process-guid-3": `{
					"disk_mb": 512,
					"environment": [],
					"file_descriptors": 8,
					"num_instances": 4,
					"log_guid": "log-guid-3",
					"memory_mb": 128,
					"process_guid": "process-guid-3",
					"routing_info": { "http_routes": [] },
					"droplet_uri": "source-url-3",
					"stack": "some-stack",
					"start_command": "start-command-3",
					"execution_metadata": "execution-metadata-3",
					"health_check_timeout_in_seconds": 123456,
					"etag": "3.1"
				}`,
		}

		fakeCC.RouteToHandler("GET", "/internal/bulk/apps",
			ghttp.RespondWith(200, `{
					"token": {},
					"fingerprints": [
							{
								"process_guid": "process-guid-1",
								"etag": "1.1"
							},
							{
								"process_guid": "process-guid-2",
								"etag": "2.1"
							},
							{
								"process_guid": "process-guid-3",
								"etag": "3.1"
							}
					]
				}`),
		)

		fakeCC.RouteToHandler("POST", "/internal/bulk/apps",
			http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				var processGuids []string
				decoder := json.NewDecoder(req.Body)
				err := decoder.Decode(&processGuids)
				Expect(err).NotTo(HaveOccurred())

				appResponses := make([]json.RawMessage, 0, len(processGuids))
				for _, processGuid := range processGuids {
					appResponses = append(appResponses, json.RawMessage(desiredAppResponses[processGuid]))
				}

				payload, err := json.Marshal(appResponses)
				Expect(err).NotTo(HaveOccurred())

				w.Write(payload)
			}),
		)
	})

	AfterEach(func() {
		defer fakeCC.Close()
	})

	Describe("when the CC polling interval elapses", func() {
		var desired1, desired2 *models.DesiredLRP
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
				"routing_info": {
					"http_routes": [
					{"hostname": "route-1"},
					{"hostname": "route-2"}
					]
				},
				"droplet_uri": "source-url-1",
				"stack": "some-stack",
				"start_command": "start-command-1",
				"execution_metadata": "execution-metadata-1",
				"health_check_timeout_in_seconds": 123456,
				"etag": "old-etag-1"
			}`), &existing1)
			Expect(err).NotTo(HaveOccurred())

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
				"routing_info": {
					"http_routes": [
					{"hostname": "route-1"},
					{"hostname": "route-2"}
					],
					"tcp_routes": [
					{"router_group_guid": "guid-1", "external_port":5222, "container_port":60000}
					]
				},
				"droplet_uri": "source-url-1",
				"stack": "some-stack",
				"start_command": "start-command-1",
				"execution_metadata": "execution-metadata-1",
				"health_check_timeout_in_seconds": 123456,
				"etag": "old-etag-2"
			}`), &existing2)
			Expect(err).NotTo(HaveOccurred())

			builder := recipebuilder.NewBuildpackRecipeBuilder(
				lagertest.NewTestLogger("test"),
				recipebuilder.Config{
					Lifecycles: map[string]string{
						"buildpack/some-stack": "some-health-check.tar.gz",
						"docker":               "the/docker/lifecycle/path.tgz",
					},
					FileServerURL: "http://file-server.com",
					KeyFactory:    keys.RSAKeyPairFactory,
				},
			)

			desired1, err = builder.Build(&existing1)
			Expect(err).NotTo(HaveOccurred())

			desired2, err = builder.Build(&existing2)
			Expect(err).NotTo(HaveOccurred())

			err = bbsClient.DesireLRP(desired1)
			Expect(err).NotTo(HaveOccurred())

			err = bbsClient.DesireLRP(desired2)
			Expect(err).NotTo(HaveOccurred())
		})

		JustBeforeEach(func() {
			process = startBulker(true)
		})

		AfterEach(func() {
			ginkgomon.Interrupt(process, interruptTimeout)
		})

		Context("once the state has been synced with CC", func() {
			JustBeforeEach(func() {
				Eventually(func() ([]*models.DesiredLRP, error) { return bbsClient.DesiredLRPs(models.DesiredLRPFilter{}) }, 5).Should(HaveLen(3))
			})

			It("it (adds), (updates), and (removes extra) LRPs", func() {
				defaultNofile := recipebuilder.DefaultFileDescriptorLimit
				nofile := uint64(16)

				expectedCachedDependencies := []*models.CachedDependency{
					{
						From:     "http://file-server.com/v1/static/some-health-check.tar.gz",
						To:       "/tmp/lifecycle",
						CacheKey: "buildpack-some-stack-lifecycle",
					},
				}

				expectedSetupActions1 := models.Serial(
					&models.DownloadAction{
						From:     "source-url-1",
						To:       ".",
						CacheKey: "droplets-process-guid-1",
						User:     "vcap",
					},
				)

				expectedSetupActions2 := models.Serial(
					&models.DownloadAction{
						From:     "source-url-1",
						To:       ".",
						CacheKey: "droplets-process-guid-2",
						User:     "vcap",
					},
				)

				expectedSetupActions3 := models.Serial(
					&models.DownloadAction{
						From:     "source-url-3",
						To:       ".",
						CacheKey: "droplets-process-guid-3",
						User:     "vcap",
					},
				)

				routeMessage := json.RawMessage([]byte(`[{"hostnames":["route-1","route-2","new-route"],"port":8080}]`))
				routes := &models.Routes{
					cfroutes.CF_ROUTER: &routeMessage,
				}

				desiredLRPsWithoutModificationTag := func() []*models.DesiredLRP {
					lrps, err := bbsClient.DesiredLRPs(models.DesiredLRPFilter{})
					Expect(err).NotTo(HaveOccurred())

					result := []*models.DesiredLRP{}
					for _, lrp := range lrps {
						lrp.ModificationTag = nil
						result = append(result, lrp)
					}
					return result
				}

				Eventually(desiredLRPsWithoutModificationTag).Should(ContainElement(&models.DesiredLRP{
					ProcessGuid:        "process-guid-1",
					Domain:             "cf-apps",
					Instances:          42,
					RootFs:             models.PreloadedRootFS("some-stack"),
					CachedDependencies: expectedCachedDependencies,
					Setup:              models.WrapAction(expectedSetupActions1),
					StartTimeout:       123456,
					EnvironmentVariables: []*models.EnvironmentVariable{
						{Name: "LANG", Value: recipebuilder.DefaultLANG},
					},
					Action: models.WrapAction(models.Codependent(&models.RunAction{
						User: "vcap",
						Path: "/tmp/lifecycle/launcher",
						Args: []string{"app", "start-command-1", "execution-metadata-1"},
						Env: []*models.EnvironmentVariable{
							{Name: "env-key-1", Value: "env-value-1"},
							{Name: "env-key-2", Value: "env-value-2"},
							{Name: "PORT", Value: "8080"},
						},
						ResourceLimits: &models.ResourceLimits{Nofile: &nofile},
						LogSource:      recipebuilder.AppLogSource,
					})),
					Monitor: models.WrapAction(models.Timeout(
						&models.ParallelAction{
							Actions: []*models.Action{
								&models.Action{
									RunAction: &models.RunAction{
										User:      "vcap",
										Path:      "/tmp/lifecycle/healthcheck",
										Args:      []string{"-port=8080"},
										LogSource: "HEALTH",
										ResourceLimits: &models.ResourceLimits{
											Nofile: &defaultNofile,
										},
										SuppressLogOutput: true,
									},
								},
							},
						},
						30*time.Second,
					)),
					DiskMb:    1024,
					MemoryMb:  256,
					CpuWeight: 1,
					Ports: []uint32{
						8080,
					},
					Routes:             routes,
					LogGuid:            "log-guid-1",
					LogSource:          recipebuilder.LRPLogSource,
					MetricsGuid:        "log-guid-1",
					Privileged:         true,
					Annotation:         "1.1",
					LegacyDownloadUser: "vcap",
				}))

				nofile = 16
				newRouteMessage := json.RawMessage([]byte(`[{"hostnames":["route-3"],"port":8080,"route_service_url":"https://rs.example.com"}]`))
				newTcpRouteMessage := json.RawMessage([]byte(`[{"router_group_guid":"guid-1","external_port":5222,"container_port":60000}]`))
				newRoutes := &models.Routes{
					cfroutes.CF_ROUTER:    &newRouteMessage,
					tcp_routes.TCP_ROUTER: &newTcpRouteMessage,
				}
				Eventually(desiredLRPsWithoutModificationTag).Should(ContainElement(&models.DesiredLRP{
					ProcessGuid:        "process-guid-2",
					Domain:             "cf-apps",
					Instances:          4,
					RootFs:             models.PreloadedRootFS("some-stack"),
					CachedDependencies: expectedCachedDependencies,
					Setup:              models.WrapAction(expectedSetupActions2),
					StartTimeout:       123456,
					EnvironmentVariables: []*models.EnvironmentVariable{
						{Name: "LANG", Value: recipebuilder.DefaultLANG},
					},
					Action: models.WrapAction(models.Codependent(&models.RunAction{
						User: "vcap",
						Path: "/tmp/lifecycle/launcher",
						Args: []string{"app", "start-command-1", "execution-metadata-1"},
						Env: []*models.EnvironmentVariable{
							{Name: "env-key-1", Value: "env-value-1"},
							{Name: "env-key-2", Value: "env-value-2"},
							{Name: "PORT", Value: "8080"},
						},
						ResourceLimits: &models.ResourceLimits{Nofile: &nofile},
						LogSource:      recipebuilder.AppLogSource,
					})),
					Monitor: models.WrapAction(models.Timeout(
						&models.ParallelAction{
							Actions: []*models.Action{
								&models.Action{
									RunAction: &models.RunAction{
										User:      "vcap",
										Path:      "/tmp/lifecycle/healthcheck",
										Args:      []string{"-port=8080"},
										LogSource: "HEALTH",
										ResourceLimits: &models.ResourceLimits{
											Nofile: &defaultNofile,
										},
										SuppressLogOutput: true,
									},
								},
							},
						},
						30*time.Second,
					)),
					DiskMb:    1024,
					MemoryMb:  256,
					CpuWeight: 1,
					Ports: []uint32{
						8080,
					},
					Routes:             newRoutes,
					LogGuid:            "log-guid-1",
					LogSource:          recipebuilder.LRPLogSource,
					MetricsGuid:        "log-guid-1",
					Privileged:         true,
					Annotation:         "2.1",
					LegacyDownloadUser: "vcap",
				}))

				nofile = 8
				emptyRouteMessage := json.RawMessage([]byte(`[{"hostnames":[],"port":8080}]`))
				emptyRoutes := &models.Routes{
					cfroutes.CF_ROUTER: &emptyRouteMessage,
				}
				Eventually(desiredLRPsWithoutModificationTag).Should(ContainElement(&models.DesiredLRP{
					ProcessGuid:        "process-guid-3",
					Domain:             "cf-apps",
					Instances:          4,
					RootFs:             models.PreloadedRootFS("some-stack"),
					CachedDependencies: expectedCachedDependencies,
					Setup:              models.WrapAction(expectedSetupActions3),
					StartTimeout:       123456,
					EnvironmentVariables: []*models.EnvironmentVariable{
						{Name: "LANG", Value: recipebuilder.DefaultLANG},
					},
					Action: models.WrapAction(models.Codependent(&models.RunAction{
						User: "vcap",
						Path: "/tmp/lifecycle/launcher",
						Args: []string{"app", "start-command-3", "execution-metadata-3"},
						Env: []*models.EnvironmentVariable{
							{Name: "PORT", Value: "8080"},
						},
						ResourceLimits: &models.ResourceLimits{Nofile: &nofile},
						LogSource:      recipebuilder.AppLogSource,
					})),
					Monitor: models.WrapAction(models.Timeout(
						&models.ParallelAction{
							Actions: []*models.Action{
								&models.Action{
									RunAction: &models.RunAction{
										User:      "vcap",
										Path:      "/tmp/lifecycle/healthcheck",
										Args:      []string{"-port=8080"},
										LogSource: "HEALTH",
										ResourceLimits: &models.ResourceLimits{
											Nofile: &defaultNofile,
										},
										SuppressLogOutput: true,
									},
								},
							},
						},
						30*time.Second,
					)),
					DiskMb:    512,
					MemoryMb:  128,
					CpuWeight: 1,
					Ports: []uint32{
						8080,
					},
					Routes:             emptyRoutes,
					LogGuid:            "log-guid-3",
					LogSource:          recipebuilder.LRPLogSource,
					MetricsGuid:        "log-guid-3",
					Privileged:         true,
					Annotation:         "3.1",
					LegacyDownloadUser: "vcap",
				}))
			})

			Describe("domains", func() {
				Context("when cc is available", func() {
					It("updates the domains", func() {
						Eventually(func() []string {
							resp, err := bbsClient.Domains()
							Expect(err).NotTo(HaveOccurred())
							return resp
						}).Should(ContainElement("cf-apps"))
					})
				})

				Context("when cc stops being available", func() {
					It("stops updating the domains", func() {
						Eventually(bbsClient.Domains, 5*pollingInterval).Should(ContainElement("cf-apps"))

						logger.Debug("stopping-fake-cc")
						fakeCC.HTTPTestServer.Close()
						logger.Debug("finished-stopping-fake-cc")

						Eventually(func() ([]string, error) {
							logger := logger.Session("domain-polling")
							logger.Debug("getting-domains")
							domains, err := bbsClient.Domains()
							logger.Debug("finished-getting-domains", lager.Data{"domains": domains, "error": err})
							return domains, err
						}, 2*domainTTL).ShouldNot(ContainElement("cf-apps"))
					})
				})
			})
		})

		Context("when LRPs in a different domain exist", func() {
			var otherDomainDesired *models.DesiredLRP
			var otherDomainDesiredResponse *models.DesiredLRP

			BeforeEach(func() {
				otherDomainDesired = &models.DesiredLRP{
					ProcessGuid: "some-other-lrp",
					Domain:      "some-domain",
					RootFs:      models.PreloadedRootFS("some-stack"),
					Instances:   1,
					Action: models.WrapAction(&models.RunAction{
						User: "me",
						Path: "reboot",
					}),
				}

				err := bbsClient.DesireLRP(otherDomainDesired)
				Expect(err).NotTo(HaveOccurred())

				otherDomainDesiredResponse, err = bbsClient.DesiredLRPByProcessGuid("some-other-lrp")
				Expect(err).NotTo(HaveOccurred())
			})

			It("leaves them alone", func() {
				Eventually(func() ([]*models.DesiredLRP, error) { return bbsClient.DesiredLRPs(models.DesiredLRPFilter{}) }, 5).Should(HaveLen(4))

				nowDesired, err := bbsClient.DesiredLRPs(models.DesiredLRPFilter{})
				Expect(err).NotTo(HaveOccurred())

				otherDomainDesiredResponse.Ports = nil
				Expect(nowDesired).To(ContainElement(otherDomainDesiredResponse))
			})
		})
	})

	Context("when the bulker loses the lock", func() {
		BeforeEach(func() {
			heartbeatInterval = 1 * time.Second
		})

		JustBeforeEach(func() {
			process = startBulker(true)

			Eventually(bbsClient.Domains, 5*domainTTL).Should(ContainElement("cf-apps"))

			consulRunner.DestroySession("nsync-bulker")
		})

		AfterEach(func() {
			ginkgomon.Interrupt(process, interruptTimeout)
		})

		itIsMissingDomain()

		It("exits with an error", func() {
			Eventually(process.Wait(), 2*domainTTL).Should(Receive(HaveOccurred()))
		})
	})

	Context("when the bulker initially does not have the lock", func() {
		var competingBulkerProcess ifrit.Process

		BeforeEach(func() {
			heartbeatInterval = 1 * time.Second

			competingBulker := locket.NewLock(logger, consulRunner.NewConsulClient(), locket.LockSchemaPath(bulkerLockName), []byte("something-else"), clock.NewClock(), locket.RetryInterval, locket.LockTTL)
			competingBulkerProcess = ifrit.Invoke(competingBulker)
		})

		JustBeforeEach(func() {
			process = startBulker(false)
		})

		AfterEach(func() {
			ginkgomon.Interrupt(process, interruptTimeout)
			ginkgomon.Kill(competingBulkerProcess)
		})

		itIsMissingDomain()

		Context("when the lock becomes available", func() {
			BeforeEach(func() {
				ginkgomon.Kill(competingBulkerProcess)

				time.Sleep(pollingInterval + 10*time.Millisecond)
			})

			It("is updated", func() {
				Eventually(func() ([]string, error) {
					logger := logger.Session("domain-polling")
					logger.Debug("getting-domains")
					domains, err := bbsClient.Domains()
					logger.Debug("finished-getting-domains", lager.Data{"domains": domains, "error": err})
					return domains, err
				}, 4*domainTTL).Should(ContainElement("cf-apps"))
			})
		})
	})
})
