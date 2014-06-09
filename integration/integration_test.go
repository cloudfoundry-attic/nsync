package integration_test

import (
	"fmt"
	"time"

	"github.com/cloudfoundry/storeadapter/test_helpers"

	"github.com/cloudfoundry-incubator/nsync/integration/nsync_runner"
	Bbs "github.com/cloudfoundry-incubator/runtime-schema/bbs"
	steno "github.com/cloudfoundry/gosteno"
	"github.com/cloudfoundry/gunk/timeprovider"
	"github.com/cloudfoundry/yagnats"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
)

var _ = Describe("Starting apps", func() {
	var (
		natsClient yagnats.NATSClient
		bbs        *Bbs.BBS
	)

	BeforeEach(func() {
		natsClient = natsRunner.MessageBus

		logSink := steno.NewTestingSink()

		steno.Init(&steno.Config{
			Sinks: []steno.Sink{logSink},
		})

		logger := steno.NewLogger("the-logger")
		steno.EnterTestMode()

		bbs = Bbs.NewBBS(etcdRunner.Adapter(), timeprovider.NewTimeProvider(), logger)

		var err error
		var presenceStatus <-chan bool

		fileServerPresence, presenceStatus, err = bbs.MaintainFileServerPresence(time.Second, "http://some.file.server", "file-server-id")
		Ω(err).ShouldNot(HaveOccurred())

		Eventually(presenceStatus).Should(Receive(BeTrue()))

		test_helpers.NewStatusReporter(presenceStatus)

		runner = nsync_runner.New(
			nsyncPath,
			etcdRunner.NodeURLS(),
			[]string{fmt.Sprintf("127.0.0.1:%d", natsPort)},
			"127.0.0.1:20515",
		)

		runner.Start()
	})

	AfterEach(func() {
		runner.KillWithFire()
		fileServerPresence.Remove()
	})

	var publishDesireWithInstances = func(nInstances int) {
		natsClient.Publish("diego.desire.app", []byte(fmt.Sprintf(`
        {
          "process_guid": "the-guid",
          "droplet_uri": "http://the-droplet.uri.com",
          "start_command": "the-start-command",
          "memory_mb": 128,
          "disk_mb": 512,
          "file_descriptors": 32,
          "num_instances": %d,
          "stack": "some-stack",
          "log_guid": "the-log-guid"
        }
      `, nInstances)))
	}

	Describe("when a 'diego.desire.app' message is recieved", func() {
		JustBeforeEach(func() {
			publishDesireWithInstances(3)
		})

		It("registers an app desire in etcd", func() {
			Eventually(bbs.GetAllDesiredLRPs).Should(HaveLen(1))
		})

		Context("when an app is no longer desired", func() {
			JustBeforeEach(func() {
				Eventually(bbs.GetAllDesiredLRPs).Should(HaveLen(1))
				publishDesireWithInstances(0)
			})

			It("should remove the desired state from etcd", func() {
				Eventually(bbs.GetAllDesiredLRPs).Should(HaveLen(0))
			})
		})
	})
})
