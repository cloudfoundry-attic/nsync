package listen_test

import (
	"encoding/json"
	"syscall"

	. "github.com/cloudfoundry-incubator/nsync/listen"
	"github.com/cloudfoundry-incubator/runtime-schema/bbs/fake_bbs"
	"github.com/cloudfoundry-incubator/runtime-schema/models"
	steno "github.com/cloudfoundry/gosteno"
	"github.com/cloudfoundry/yagnats/fakeyagnats"
	"github.com/tedsuo/ifrit"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
)

var _ = Describe("Listen", func() {
	var (
		fakenats         *fakeyagnats.FakeYagnats
		logSink          *steno.TestingSink
		desireAppRequest models.DesireAppRequestFromCC
		bbs              *fake_bbs.FakeNsyncBBS

		process ifrit.Process
	)

	BeforeEach(func() {
		logSink = steno.NewTestingSink()

		steno.Init(&steno.Config{
			Sinks: []steno.Sink{logSink},
		})

		logger := steno.NewLogger("the-logger")
		steno.EnterTestMode()

		fakenats = fakeyagnats.New()

		bbs = new(fake_bbs.FakeNsyncBBS)

		runner := Listen{
			NATSClient: fakenats,
			BBS:        bbs,
			Logger:     logger,
		}

		desireAppRequest = models.DesireAppRequestFromCC{
			ProcessGuid:  "some-guid",
			DropletUri:   "http://the-droplet.uri.com",
			Stack:        "some-stack",
			StartCommand: "the-start-command",
			Environment: []models.EnvironmentVariable{
				{Name: "foo", Value: "bar"},
				{Name: "VCAP_APPLICATION", Value: "{\"application_name\":\"my-app\"}"},
			},
			MemoryMB:        128,
			DiskMB:          512,
			FileDescriptors: 32,
			NumInstances:    2,
			Routes:          []string{"route1", "route2"},
			LogGuid:         "some-log-guid",
		}

		process = ifrit.Envoke(runner)
	})

	AfterEach(func() {
		process.Signal(syscall.SIGINT)
		Eventually(process.Wait()).Should(Receive())
	})

	Describe("when a 'diego.desire.app' message is received", func() {
		JustBeforeEach(func() {
			messagePayload, err := json.Marshal(desireAppRequest)
			Ω(err).ShouldNot(HaveOccurred())

			fakenats.Publish("diego.desire.app", messagePayload)
		})

		It("marks the LRP desired in the bbs", func() {
			Eventually(bbs.DesireLRPCallCount).Should(Equal(1))
			Ω(bbs.DesireLRPArgsForCall(0)).Should(Equal(models.DesiredLRP{
				ProcessGuid:  "some-guid",
				Instances:    2,
				MemoryMB:     128,
				DiskMB:       512,
				Stack:        "some-stack",
				StartCommand: "the-start-command",
				Environment: []models.EnvironmentVariable{
					{Name: "foo", Value: "bar"},
					{Name: "VCAP_APPLICATION", Value: "{\"application_name\":\"my-app\"}"},
				},
				FileDescriptors: 32,
				Source:          "http://the-droplet.uri.com",
				Routes:          []string{"route1", "route2"},
				LogGuid:         "some-log-guid",
			}))
		})

		Context("when the number of desired app instances is zero", func() {
			BeforeEach(func() {
				desireAppRequest.NumInstances = 0
			})

			It("deletes the desired LRP from BBS", func() {
				Eventually(bbs.RemoveDesiredLRPByProcessGuidCallCount).Should(Equal(1))
				Ω(bbs.RemoveDesiredLRPByProcessGuidArgsForCall(0)).Should(Equal("some-guid"))
			})
		})
	})

	Describe("when a invalid 'diego.desire.app' message is received", func() {
		BeforeEach(func() {
			fakenats.Publish("diego.desire.app", []byte(`
        {
          "some_random_key": "does not matter"
      `))
		})

		It("logs an error", func() {
			Eventually(logSink.Records).ShouldNot(HaveLen(0))
			Ω(logSink.Records()[0].Message).Should(ContainSubstring("Failed to parse NATS message."))
		})

		It("does not put a desired LRP into the BBS", func() {
			Consistently(bbs.DesireLRPCallCount).Should(Equal(0))
		})
	})
})
