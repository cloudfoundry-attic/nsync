package main_test

import (
	"fmt"
	"net/http"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/cloudfoundry-incubator/nsync"
	"github.com/cloudfoundry-incubator/receptor"
	receptorrunner "github.com/cloudfoundry-incubator/receptor/cmd/receptor/testrunner"
	"github.com/cloudfoundry-incubator/runtime-schema/bbs/lrp_bbs"
	"github.com/cloudfoundry-incubator/runtime-schema/bbs/services_bbs"
	"github.com/cloudfoundry-incubator/runtime-schema/cb"
	"github.com/cloudfoundry/storeadapter"
	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
	"github.com/pivotal-golang/clock"
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
		lrpBBS       *lrp_bbs.LRPBBS

		requestGenerator *rata.RequestGenerator
		httpClient       *http.Client
		response         *http.Response
		err              error

		receptorProcess ifrit.Process
		receptorClient  receptor.Client

		runner  ifrit.Runner
		process ifrit.Process

		etcdAdapter storeadapter.StoreAdapter

		logger lager.Logger
	)

	startReceptor := func(address, taskAddress string) ifrit.Process {
		return ginkgomon.Invoke(receptorrunner.New(receptorPath, receptorrunner.Args{
			Address:            address,
			TaskHandlerAddress: taskAddress,
			EtcdCluster:        strings.Join(etcdRunner.NodeURLS(), ","),
			ConsulCluster:      consulRunner.ConsulCluster(),
			BBSAddress:         bbsURL.String(),
		}))
	}

	newNSyncRunner := func(nsyncPort int, args ...string) *ginkgomon.Runner {
		return ginkgomon.New(ginkgomon.Config{
			Name:          "nsync",
			AnsiColorCode: "97m",
			StartCheck:    "nsync.listener.started",
			Command: exec.Command(
				listenerPath,
				"-diegoAPIURL", fmt.Sprintf("http://127.0.0.1:%d", receptorPort),
				"-nsyncURL", fmt.Sprintf("http://127.0.0.1:%d", nsyncPort),
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
        "memory_mb": 128,
        "disk_mb": 512,
        "file_descriptors": 32,
        "num_instances": `+strconv.Itoa(nInstances)+`,
        "stack": "some-stack",
        "log_guid": "the-log-guid"
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
		receptorAddress := fmt.Sprintf("127.0.0.1:%d", receptorPort)
		receptorTaskAddress := fmt.Sprintf("127.0.0.1:%d", receptorPort+1)
		logger = lagertest.NewTestLogger("test")
		clock := clock.NewClock()
		lrpBBS = lrp_bbs.New(
			etcdAdapter,
			clock,
			cb.NewCellClient(),
			cb.NewAuctioneerClient(),
			services_bbs.New(consulRunner.NewSession("a-session"), clock, logger.Session("services-bbs")),
		)
		receptorURL := fmt.Sprintf("http://127.0.0.1:%d", receptorPort)
		receptorProcess = startReceptor(receptorAddress, receptorTaskAddress)
		receptorClient = receptor.NewClient(receptorURL)

		runner = newNSyncRunner(nsyncPort)
		process = ginkgomon.Invoke(runner)
	})

	AfterEach(func() {
		etcdAdapter.Disconnect()
		ginkgomon.Interrupt(receptorProcess, exitDuration)
		ginkgomon.Interrupt(process, exitDuration)
	})

	Describe("Desire an app", func() {
		BeforeEach(func() {
			response, err = requestDesireWithInstances(3)
		})

		It("desires the app in etcd", func() {
			Expect(err).NotTo(HaveOccurred())
			Expect(response.StatusCode).To(Equal(http.StatusAccepted))
			Eventually(func() ([]receptor.DesiredLRPResponse, error) { return receptorClient.DesiredLRPs() }, 10).Should(HaveLen(1))
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
			Eventually(func() ([]receptor.ActualLRPResponse, error) { return receptorClient.ActualLRPs() }, 10).Should(HaveLen(3))
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
			Eventually(func() ([]receptor.DesiredLRPResponse, error) { return receptorClient.DesiredLRPs() }).Should(HaveLen(0))
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
			Eventually(func() ([]receptor.ActualLRPResponse, error) { return receptorClient.ActualLRPs() }, 10).Should(HaveLen(3))
		})

		It("kills an index", func() {
			resp, err := killIndex("the-guid", 0)
			Expect(err).NotTo(HaveOccurred())
			Expect(resp.StatusCode).To(Equal(http.StatusAccepted))

			Eventually(func() ([]receptor.ActualLRPResponse, error) { return receptorClient.ActualLRPs() }, 10).Should(HaveLen(2))
		})

		It("fails when the index is invalid", func() {
			resp, err := killIndex("the-guid", 4)
			Expect(err).NotTo(HaveOccurred())
			Expect(resp.StatusCode).To(Equal(http.StatusNotFound))
		})
	})
})
