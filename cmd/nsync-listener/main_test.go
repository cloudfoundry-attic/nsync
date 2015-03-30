package main_test

import (
	"fmt"
	"net/http"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/cloudfoundry-incubator/nsync"
	receptorrunner "github.com/cloudfoundry-incubator/receptor/cmd/receptor/testrunner"
	Bbs "github.com/cloudfoundry-incubator/runtime-schema/bbs"
	"github.com/cloudfoundry/storeadapter"
	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
	"github.com/pivotal-golang/clock"
	"github.com/pivotal-golang/lager/lagertest"
	"github.com/tedsuo/ifrit"
	"github.com/tedsuo/ifrit/ginkgomon"
	"github.com/tedsuo/rata"
)

var _ = Describe("Nsync Listener", func() {
	var (
		nsyncPort    int
		exitDuration = 3 * time.Second
		bbs          *Bbs.BBS

		requestGenerator *rata.RequestGenerator
		httpClient       *http.Client
		response         *http.Response
		err              error

		receptorProcess ifrit.Process

		runner  ifrit.Runner
		process ifrit.Process

		etcdAdapter storeadapter.StoreAdapter
	)

	startReceptor := func() ifrit.Process {
		return ginkgomon.Invoke(receptorrunner.New(receptorPath, receptorrunner.Args{
			Address:     fmt.Sprintf("127.0.0.1:%d", receptorPort),
			EtcdCluster: strings.Join(etcdRunner.NodeURLS(), ","),
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
				"-lifecycles", `{"buildpack/some-stack": "some-health-check.tar.gz","docker":"the/docker/lifecycle/path.tgz"}`,
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
		Ω(err).ShouldNot(HaveOccurred())
		req.Header.Set("Content-Type", "application/json")

		return httpClient.Do(req)
	}

	BeforeEach(func() {
		nsyncPort = 8888 + GinkgoParallelNode()
		nsyncURL := fmt.Sprintf("http://127.0.0.1:%d", nsyncPort)

		requestGenerator = rata.NewRequestGenerator(nsyncURL, nsync.Routes)
		httpClient = http.DefaultClient

		etcdAdapter = etcdRunner.Adapter()
		bbs = Bbs.NewBBS(etcdAdapter, clock.NewClock(), lagertest.NewTestLogger("test"))
		receptorProcess = startReceptor()

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
			Ω(err).ShouldNot(HaveOccurred())
			Ω(response.StatusCode).Should(Equal(http.StatusAccepted))
			Eventually(bbs.DesiredLRPs, 10).Should(HaveLen(1))
		})
	})

	Describe("Stop an app", func() {
		var stopResponse *http.Response

		stopApp := func(guid string) (*http.Response, error) {
			req, err := requestGenerator.CreateRequest(nsync.StopAppRoute, rata.Params{"process_guid": guid}, nil)
			Ω(err).ShouldNot(HaveOccurred())

			return httpClient.Do(req)
		}

		BeforeEach(func() {
			response, err = requestDesireWithInstances(3)
			Ω(err).ShouldNot(HaveOccurred())
			Ω(response.StatusCode).Should(Equal(http.StatusAccepted))
			Eventually(bbs.ActualLRPs, 10).Should(HaveLen(3))
		})

		JustBeforeEach(func() {
			var err error
			stopResponse, err = stopApp("the-guid")
			Ω(err).ShouldNot(HaveOccurred())
		})

		It("accepts the request", func() {
			Ω(stopResponse.StatusCode).Should(Equal(http.StatusAccepted))
		})

		It("deletes the desired LRP", func() {
			Eventually(bbs.DesiredLRPs).Should(HaveLen(0))
		})
	})

	Describe("Kill an app instance", func() {
		killIndex := func(guid string, index int) (*http.Response, error) {
			req, err := requestGenerator.CreateRequest(nsync.KillIndexRoute, rata.Params{"process_guid": "the-guid", "index": strconv.Itoa(index)}, nil)
			Ω(err).ShouldNot(HaveOccurred())

			return httpClient.Do(req)
		}

		BeforeEach(func() {
			response, err = requestDesireWithInstances(3)
			Ω(err).ShouldNot(HaveOccurred())
			Ω(response.StatusCode).Should(Equal(http.StatusAccepted))
			Eventually(bbs.ActualLRPs, 10).Should(HaveLen(3))
		})

		It("kills an index", func() {
			resp, err := killIndex("the-guid", 0)
			Ω(err).ShouldNot(HaveOccurred())
			Ω(resp.StatusCode).Should(Equal(http.StatusAccepted))

			Eventually(bbs.ActualLRPs, 10).Should(HaveLen(2))
		})

		It("fails when the index is invalid", func() {
			resp, err := killIndex("the-guid", 4)
			Ω(err).ShouldNot(HaveOccurred())
			Ω(resp.StatusCode).Should(Equal(http.StatusBadRequest))
		})
	})
})
