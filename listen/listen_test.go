package listen_test

import (
	"encoding/json"
	"errors"
	"syscall"

	. "github.com/cloudfoundry-incubator/nsync/listen"
	"github.com/cloudfoundry-incubator/nsync/listen/fakes"
	"github.com/cloudfoundry-incubator/runtime-schema/bbs/fake_bbs"
	"github.com/cloudfoundry-incubator/runtime-schema/cc_messages"
	"github.com/cloudfoundry-incubator/runtime-schema/models"
	"github.com/cloudfoundry/yagnats/fakeyagnats"
	"github.com/pivotal-golang/lager/lagertest"
	"github.com/tedsuo/ifrit"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
	"github.com/onsi/gomega/gbytes"
)

var _ = Describe("Listen", func() {
	var (
		builder          *fakes.FakeRecipeBuilder
		fakenats         *fakeyagnats.FakeYagnats
		desireAppRequest cc_messages.DesireAppRequestFromCC
		logger           *lagertest.TestLogger
		bbs              *fake_bbs.FakeNsyncBBS

		process ifrit.Process
	)

	BeforeEach(func() {
		logger = lagertest.NewTestLogger("test")

		fakenats = fakeyagnats.New()

		bbs = new(fake_bbs.FakeNsyncBBS)

		builder = new(fakes.FakeRecipeBuilder)

		runner := Listen{
			NATSClient:    fakenats,
			BBS:           bbs,
			Logger:        logger,
			RecipeBuilder: builder,
		}

		desireAppRequest = cc_messages.DesireAppRequestFromCC{
			ProcessGuid:  "some-guid",
			DropletUri:   "http://the-droplet.uri.com",
			Stack:        "some-stack",
			StartCommand: "the-start-command",
			Environment: cc_messages.Environment{
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

		newlyDesiredLRP := models.DesiredLRP{
			ProcessGuid: "new-process-guid",

			Instances: 1,
			Stack:     "stack-2",

			Actions: []models.ExecutorAction{
				{
					Action: models.RunAction{
						Path: "ls",
					},
				},
			},
		}

		BeforeEach(func() {
			builder.BuildReturns(newlyDesiredLRP, nil)
		})

		It("marks the LRP desired in the bbs", func() {
			Eventually(bbs.DesireLRPCallCount).Should(Equal(1))

			Ω(bbs.DesireLRPArgsForCall(0)).Should(Equal(newlyDesiredLRP))

			Ω(builder.BuildArgsForCall(0)).Should(Equal(desireAppRequest))
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

		Describe("when building the recipe fails to build", func() {
			BeforeEach(func() {
				builder.BuildReturns(models.DesiredLRP{}, errors.New("oh no!"))
			})

			It("logs an error", func() {
				Eventually(logger.TestSink.Buffer).Should(gbytes.Say("failed-to-build-recipe"))
				Eventually(logger.TestSink.Buffer).Should(gbytes.Say("oh no!"))
			})

			It("does not put a desired LRP into the BBS", func() {
				Consistently(bbs.DesireLRPCallCount).Should(Equal(0))
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
			Eventually(logger.TestSink.Buffer).Should(gbytes.Say("parse-nats-message-failed"))
		})

		It("does not put a desired LRP into the BBS", func() {
			Consistently(bbs.DesireLRPCallCount).Should(Equal(0))
		})
	})
})
