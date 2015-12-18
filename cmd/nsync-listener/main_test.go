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
	"github.com/cloudfoundry/storeadapter"
	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
	"github.com/pivotal-golang/lager"
	"github.com/pivotal-golang/lager/lagertest"
	"github.com/tedsuo/ifrit"
	"github.com/tedsuo/ifrit/ginkgomon"
	"github.com/tedsuo/rata"
)

var _ = Describe("Nsync Listener", func() {
	var (
		nsyncPort    int
		exitDuration = 3 * time.Second

		requestGenerator *rata.RequestGenerator
		httpClient       *http.Client
		response         *http.Response
		err              error

		runner  ifrit.Runner
		process ifrit.Process

		etcdAdapter storeadapter.StoreAdapter

		logger lager.Logger
	)

	newNSyncRunner := func(nsyncPort int, args ...string) *ginkgomon.Runner {
		return ginkgomon.New(ginkgomon.Config{
			Name:          "nsync",
			AnsiColorCode: "97m",
			StartCheck:    "nsync.listener.started",
			Command: exec.Command(
				listenerPath,
				"-bbsAddress", bbsURL.String(),
				"-listenAddress", fmt.Sprintf("127.0.0.1:%d", nsyncPort),
				"-lifecycle", "buildpack/some-stack:some-health-check.tar.gz",
				"-lifecycle", "docker:the/docker/lifecycle/path.tgz",
				"-fileServerURL", "http://file-server.com",
				"-logLevel", "debug",
			),
		})
	}

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

		etcdAdapter = etcdRunner.Adapter(nil)
		logger = lagertest.NewTestLogger("test")

		runner = newNSyncRunner(nsyncPort)
		process = ginkgomon.Invoke(runner)
	})

	AfterEach(func() {
		etcdAdapter.Disconnect()
		ginkgomon.Interrupt(process, exitDuration)
	})

	Describe("Desire an app", func() {
		BeforeEach(func() {
			response, err = requestDesireWithInstances(3)
		})

		It("desires the app from the bbs", func() {
			Expect(err).NotTo(HaveOccurred())
			Expect(response.StatusCode).To(Equal(http.StatusAccepted))
			Eventually(func() ([]*models.DesiredLRP, error) { return bbsClient.DesiredLRPs(models.DesiredLRPFilter{}) }, 10).Should(HaveLen(1))
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
})
