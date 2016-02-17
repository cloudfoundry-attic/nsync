package main_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/cloudfoundry-incubator/bbs/models"
	"github.com/cloudfoundry-incubator/nsync"
	"github.com/cloudfoundry-incubator/nsync/recipebuilder"
	"github.com/cloudfoundry-incubator/routing-info/cfroutes"
	"github.com/cloudfoundry-incubator/routing-info/tcp_routes"
	"github.com/hashicorp/consul/api"
	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
	"github.com/onsi/gomega/gbytes"
	"github.com/onsi/gomega/gexec"
	"github.com/tedsuo/ifrit"
	"github.com/tedsuo/ifrit/ginkgomon"
	"github.com/tedsuo/rata"
)

var _ = Describe("Nsync Listener", func() {
	const exitDuration = 3 * time.Second

	var (
		nsyncPort int

		requestGenerator *rata.RequestGenerator
		httpClient       *http.Client
		response         *http.Response
		err              error

		process ifrit.Process
	)

	requestDesireWithInstances := func(nInstances int) (*http.Response, error) {
		req, err := requestGenerator.CreateRequest(nsync.DesireAppRoute, rata.Params{"process_guid": "the-guid"}, strings.NewReader(`{
        "process_guid": "the-guid",
        "droplet_uri": "http://the-droplet.uri.com",
        "start_command": "the-start-command",
        "execution_metadata": "execution-metadata-1",
        "memory_mb": 128,
        "disk_mb": 512,
        "file_descriptors": 32,
        "num_instances": `+strconv.Itoa(nInstances)+`,
        "stack": "some-stack",
        "log_guid": "the-log-guid",
        "health_check_timeout_in_seconds": 123456,
        "ports": [8080,5222],
        "etag": "2.1",
        "routing_info": {
			"http_routes": [
			{"hostname": "route-1"}
			],
			"tcp_routes": [
			{"router_group_guid": "guid-1", "external_port":5222, "container_port":60000}
			]
		}
			}`))
		Expect(err).NotTo(HaveOccurred())
		req.Header.Set("Content-Type", "application/json")

		return httpClient.Do(req)
	}

	BeforeEach(func() {
		nsyncPort = 8888 + GinkgoParallelNode()
		nsyncURL := fmt.Sprintf("http://127.0.0.1:%d", nsyncPort)

		requestGenerator = rata.NewRequestGenerator(nsyncURL, nsync.Routes)
		httpClient = http.DefaultClient

		runner := newNSyncRunner(fmt.Sprintf("127.0.0.1:%d", nsyncPort))
		process = ginkgomon.Invoke(runner)
	})

	AfterEach(func() {
		ginkgomon.Interrupt(process, exitDuration)
	})

	Describe("Desire an app", func() {
		BeforeEach(func() {
			response, err = requestDesireWithInstances(3)
		})

		It("desires the app from the bbs", func() {
			Expect(err).NotTo(HaveOccurred())
			Expect(response.StatusCode).To(Equal(http.StatusAccepted))
			Eventually(func() ([]*models.DesiredLRP, error) {
				return bbsClient.DesiredLRPs(models.DesiredLRPFilter{})
			}, 10).Should(HaveLen(1))
			desiredLrps, _ := bbsClient.DesiredLRPs(models.DesiredLRPFilter{})
			newRouteMessage := json.RawMessage([]byte(`[{"hostnames":["route-1"],"port":8080}]`))
			newTcpRouteMessage := json.RawMessage([]byte(`[{"router_group_guid":"guid-1","external_port":5222,"container_port":60000}]`))
			newRoutes := &models.Routes{
				cfroutes.CF_ROUTER:    &newRouteMessage,
				tcp_routes.TCP_ROUTER: &newTcpRouteMessage,
			}
			defaultNofile := recipebuilder.DefaultFileDescriptorLimit
			nofile := uint64(32)

			expectedCachedDependencies := []*models.CachedDependency{
				{
					From:     "http://file-server.com/v1/static/some-health-check.tar.gz",
					To:       "/tmp/lifecycle",
					CacheKey: "buildpack-some-stack-lifecycle",
				},
			}

			expectedSetupActions := models.Serial(
				&models.DownloadAction{
					From:     "http://the-droplet.uri.com",
					To:       ".",
					CacheKey: "droplets-the-guid",
					User:     "vcap",
				},
			)
			desiredLrpWithoutModificationTag := desiredLrps[0]
			desiredLrpWithoutModificationTag.ModificationTag = nil
			Expect(desiredLrpWithoutModificationTag).To(Equal(&models.DesiredLRP{
				ProcessGuid:  "the-guid",
				Domain:       "cf-apps",
				Instances:    3,
				RootFs:       models.PreloadedRootFS("some-stack"),
				StartTimeout: 123456,
				EnvironmentVariables: []*models.EnvironmentVariable{
					{Name: "LANG", Value: recipebuilder.DefaultLANG},
				},
				CachedDependencies: expectedCachedDependencies,
				Setup:              models.WrapAction(expectedSetupActions),
				Action: models.WrapAction(models.Codependent(&models.RunAction{
					User: "vcap",
					Path: "/tmp/lifecycle/launcher",
					Args: []string{"app", "the-start-command", "execution-metadata-1"},
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
							&models.Action{
								RunAction: &models.RunAction{
									User:      "vcap",
									Path:      "/tmp/lifecycle/healthcheck",
									Args:      []string{"-port=5222"},
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
					8080, 5222,
				},
				Routes:             newRoutes,
				LogGuid:            "the-log-guid",
				LogSource:          recipebuilder.LRPLogSource,
				MetricsGuid:        "the-log-guid",
				Privileged:         true,
				Annotation:         "2.1",
				LegacyDownloadUser: "vcap",
			}))
		})
	})

	Describe("Stop an app", func() {
		var stopResponse *http.Response

		stopApp := func(guid string) (*http.Response, error) {
			req, err := requestGenerator.CreateRequest(nsync.StopAppRoute, rata.Params{"process_guid": guid}, nil)
			Expect(err).NotTo(HaveOccurred())

			return httpClient.Do(req)
		}

		BeforeEach(func() {
			response, err = requestDesireWithInstances(3)
			Expect(err).NotTo(HaveOccurred())
			Expect(response.StatusCode).To(Equal(http.StatusAccepted))
			Eventually(func() ([]*models.ActualLRPGroup, error) { return bbsClient.ActualLRPGroups(models.ActualLRPFilter{}) }, 10).Should(HaveLen(3))
		})

		JustBeforeEach(func() {
			var err error
			stopResponse, err = stopApp("the-guid")
			Expect(err).NotTo(HaveOccurred())
		})

		It("accepts the request", func() {
			Expect(stopResponse.StatusCode).To(Equal(http.StatusAccepted))
		})

		It("deletes the desired LRP", func() {
			Eventually(func() ([]*models.DesiredLRP, error) { return bbsClient.DesiredLRPs(models.DesiredLRPFilter{}) }).Should(HaveLen(0))
		})
	})

	Describe("Kill an app instance", func() {
		killIndex := func(guid string, index int) (*http.Response, error) {
			req, err := requestGenerator.CreateRequest(nsync.KillIndexRoute, rata.Params{"process_guid": "the-guid", "index": strconv.Itoa(index)}, nil)
			Expect(err).NotTo(HaveOccurred())

			return httpClient.Do(req)
		}

		BeforeEach(func() {
			response, err = requestDesireWithInstances(3)
			Expect(err).NotTo(HaveOccurred())
			Expect(response.StatusCode).To(Equal(http.StatusAccepted))
			Eventually(func() ([]*models.ActualLRPGroup, error) { return bbsClient.ActualLRPGroups(models.ActualLRPFilter{}) }, 10).Should(HaveLen(3))
		})

		It("kills an index", func() {
			resp, err := killIndex("the-guid", 0)
			Expect(err).NotTo(HaveOccurred())
			Expect(resp.StatusCode).To(Equal(http.StatusAccepted))

			Eventually(func() ([]*models.ActualLRPGroup, error) { return bbsClient.ActualLRPGroups(models.ActualLRPFilter{}) }, 10).Should(HaveLen(2))
		})

		It("fails when the index is invalid", func() {
			resp, err := killIndex("the-guid", 4)
			Expect(err).NotTo(HaveOccurred())
			Expect(resp.StatusCode).To(Equal(http.StatusNotFound))
		})
	})

	Describe("Desire a task", func() {
		BeforeEach(func() {
			req, err := requestGenerator.CreateRequest(nsync.TasksRoute, rata.Params{}, strings.NewReader(`{
			"task_guid": "the-guid",
			"droplet_uri": "http://the-droplet.uri.com",
			"command": "the-start-command",
			"memory_mb": 128,
			"disk_mb": 512,
			"rootfs": "some-stack",
			"log_guid": "the-log-guid",
			"completion_callback": "http://google.com",
			"lifecycle": "buildpack"
	}`))

			Expect(err).NotTo(HaveOccurred())
			req.Header.Set("Content-Type", "application/json")

			response, err = httpClient.Do(req)
		})

		It("desires the task from the bbs", func() {
			Expect(err).NotTo(HaveOccurred())
			Expect(response.StatusCode).To(Equal(http.StatusAccepted))

			Eventually(func() ([]*models.Task, error) {
				return bbsClient.Tasks()
			}, 10).Should(HaveLen(1))

			task, err := bbsClient.TaskByGuid("the-guid")
			Expect(err).NotTo(HaveOccurred())

			expectedCachedDependencies := []*models.CachedDependency{
				{
					From:     "http://file-server.com/v1/static/some-health-check.tar.gz",
					To:       "/tmp/lifecycle",
					CacheKey: "buildpack-some-stack-lifecycle",
				},
			}

			expectedActions := models.Serial(
				&models.DownloadAction{
					From:     "http://the-droplet.uri.com",
					To:       ".",
					CacheKey: "",
					User:     "vcap",
				},
				&models.RunAction{
					User:           "vcap",
					Path:           "/tmp/lifecycle/launcher",
					Args:           []string{"app", "the-start-command", ""},
					LogSource:      "TASK",
					ResourceLimits: &models.ResourceLimits{},
				},
			)

			Expect(task.TaskDefinition).To(BeEquivalentTo(&models.TaskDefinition{
				Privileged:            true,
				LogGuid:               "the-log-guid",
				MemoryMb:              128,
				DiskMb:                512,
				CpuWeight:             1,
				RootFs:                models.PreloadedRootFS("some-stack"),
				CompletionCallbackUrl: "http://google.com",
				CachedDependencies:    expectedCachedDependencies,
				Action:                models.WrapAction(expectedActions),
				LegacyDownloadUser:    "vcap",
			}))
		})
	})
})

var _ = Describe("Nsync Listener Initialization", func() {
	const exitDuration = 3 * time.Second

	var (
		nsyncPort int

		runner  *ginkgomon.Runner
		process ifrit.Process
	)

	BeforeEach(func() {
		nsyncPort = 8888 + GinkgoParallelNode()
		nsyncAddress := fmt.Sprintf("127.0.0.1:%d", nsyncPort)

		runner = newNSyncRunner(nsyncAddress)
	})

	JustBeforeEach(func() {
		process = ifrit.Invoke(runner)
	})

	AfterEach(func() {
		ginkgomon.Interrupt(process, exitDuration)
	})

	Describe("Flag validation", func() {
		Context("when the listenAddress does not match host:port pattern", func() {
			BeforeEach(func() {
				runner = newNSyncRunner("portless")
			})

			It("exits with an error", func() {
				Eventually(runner).Should(gexec.Exit(2))
				Expect(runner.Buffer()).Should(gbytes.Say("missing port"))
			})
		})

		Context("when the listenAddress port is not a number or recognized service", func() {
			BeforeEach(func() {
				runner = newNSyncRunner("127.0.0.1:onehundred")
			})

			It("exits with an error", func() {
				Eventually(runner).Should(gexec.Exit(2))
				Expect(runner.Buffer()).Should(gbytes.Say("unknown port"))
			})
		})
	})

	Describe("Initialization", func() {
		It("registers itself with consul", func() {
			services, err := consulClient.Agent().Services()
			Expect(err).ToNot(HaveOccurred())

			Expect(services).To(HaveKeyWithValue("nsync",
				&api.AgentService{
					Service: "nsync",
					ID:      "nsync",
					Port:    nsyncPort,
				}))
		})

		It("registers a TTL healthcheck", func() {
			checks, err := consulClient.Agent().Checks()
			Expect(err).ToNot(HaveOccurred())

			Expect(checks).To(HaveKeyWithValue("service:nsync",
				&api.AgentCheck{
					Node:        "0",
					CheckID:     "service:nsync",
					Name:        "Service 'nsync' check",
					Status:      "passing",
					ServiceID:   "nsync",
					ServiceName: "nsync",
				}))
		})
	})
})

var newNSyncRunner = func(nsyncListenAddress string) *ginkgomon.Runner {
	return ginkgomon.New(ginkgomon.Config{
		Name:          "nsync",
		AnsiColorCode: "97m",
		StartCheck:    "nsync.listener.started",
		Command: exec.Command(
			listenerPath,
			"-bbsAddress", bbsURL.String(),
			"-listenAddress", nsyncListenAddress,
			"-lifecycle", "buildpack/some-stack:some-health-check.tar.gz",
			"-lifecycle", "docker:the/docker/lifecycle/path.tgz",
			"-fileServerURL", "http://file-server.com",
			"-logLevel", "debug",
			"-consulCluster", consulRunner.ConsulCluster(),
		),
	})
}
