package recipebuilder_test

import (
	. "github.com/cloudfoundry-incubator/nsync/recipebuilder"
	"github.com/cloudfoundry-incubator/runtime-schema/models"
	"github.com/pivotal-golang/lager"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
)

var _ = Describe("Recipe Builder", func() {
	var (
		builder                   *RecipeBuilder
		repAddrRelativeToExecutor string
		desiredLRP                models.DesireAppRequestFromCC
		circuses                  map[string]string
		fileServerURL             string
	)

	BeforeEach(func() {
		fileServerURL = "http://file-server.com"
		repAddrRelativeToExecutor = "127.0.0.1:20515"
		logger := lager.NewLogger("fakelogger")

		circuses = map[string]string{
			"some-stack": "some-circus.tgz",
		}

		builder = New(repAddrRelativeToExecutor, circuses, logger)

		desiredLRP = models.DesireAppRequestFromCC{
			ProcessGuid:  "the-app-guid-the-app-version",
			DropletUri:   "http://the-droplet.uri.com",
			Stack:        "some-stack",
			StartCommand: "the-start-command",
			Environment: []models.EnvironmentVariable{
				{Name: "foo", Value: "bar"},
				{Name: "VCAP_APPLICATION", Value: `{"foo": 1, "bar": 2}`},
			},
			MemoryMB:        128,
			DiskMB:          512,
			FileDescriptors: 32,
			NumInstances:    23,
			Routes:          []string{"route1", "route2"},
			LogGuid:         "the-log-id",
		}
	})

	It("builds a valid DesiredLRP", func() {
		desiredLRP, err := builder.Build(desiredLRP)
		Ω(err).ShouldNot(HaveOccurred())

		Ω(desiredLRP.ProcessGuid).Should(Equal("the-app-guid-the-app-version"))
		Ω(desiredLRP.Instances).Should(Equal(23))
		Ω(desiredLRP.Routes).Should(Equal([]string{"route1", "route2"}))
		Ω(desiredLRP.Stack).Should(Equal("some-stack"))
		Ω(desiredLRP.MemoryMB).Should(Equal(128))
		Ω(desiredLRP.DiskMB).Should(Equal(512))
		Ω(desiredLRP.Ports).Should(Equal([]models.PortMapping{{ContainerPort: 8080}}))

		Ω(desiredLRP.Log).Should(Equal(models.LogConfig{
			Guid:       "the-log-id",
			SourceName: "App",
		}))

		Ω(desiredLRP.Actions).Should(HaveLen(3))

		Ω(desiredLRP.Actions[0].Action).Should(Equal(models.DownloadAction{
			From:    "PLACEHOLDER_FILESERVER_URL/v1/static/some-circus.tgz",
			To:      "/tmp/circus",
			Extract: true,
		}))

		Ω(desiredLRP.Actions[1].Action).Should(Equal(models.DownloadAction{
			From:     "http://the-droplet.uri.com",
			To:       ".",
			Extract:  true,
			CacheKey: "droplets-the-app-guid-the-app-version",
		}))

		parallelAction, ok := desiredLRP.Actions[2].Action.(models.ParallelAction)
		Ω(ok).Should(BeTrue())

		runAction, ok := parallelAction.Actions[0].Action.(models.RunAction)
		Ω(ok).Should(BeTrue())

		monitorAction, ok := parallelAction.Actions[1].Action.(models.MonitorAction)
		Ω(ok).Should(BeTrue())

		Ω(monitorAction.Action.Action).Should(Equal(models.RunAction{
			Path: "/tmp/circus/spy",
			Args: []string{"-addr=:8080"},
		}))

		Ω(monitorAction.HealthyHook).Should(Equal(models.HealthRequest{
			Method: "PUT",
			URL:    "http://" + repAddrRelativeToExecutor + "/lrp_running/the-app-guid-the-app-version/PLACEHOLDER_INSTANCE_INDEX/PLACEHOLDER_INSTANCE_GUID",
		}))

		Ω(monitorAction.HealthyThreshold).ShouldNot(BeZero())
		Ω(monitorAction.UnhealthyThreshold).ShouldNot(BeZero())

		Ω(runAction.Path).Should(Equal("/tmp/circus/soldier"))
		Ω(runAction.Args).Should(Equal([]string{"/app", "the-start-command"}))

		numFiles := uint64(32)
		Ω(runAction.ResourceLimits).Should(Equal(models.ResourceLimits{
			Nofile: &numFiles,
		}))

		Ω(runAction.Env).Should(ContainElement(models.EnvironmentVariable{
			Name:  "foo",
			Value: "bar",
		}))

		Ω(runAction.Env).Should(ContainElement(models.EnvironmentVariable{
			Name:  "PORT",
			Value: "8080",
		}))

		Ω(runAction.Env).Should(ContainElement(models.EnvironmentVariable{
			Name:  "VCAP_APP_PORT",
			Value: "8080",
		}))

		Ω(runAction.Env).Should(ContainElement(models.EnvironmentVariable{
			Name:  "VCAP_APP_HOST",
			Value: "0.0.0.0",
		}))

		var vcapApplication string
		for _, env := range runAction.Env {
			if env.Name == "VCAP_APPLICATION" {
				vcapApplication = env.Value
			}
		}

		// note: instance index must be an integer, but we cannot inline the
		// replacement term and still have valid JSON. the lrpreprocessor in app
		// manager must replace it including the surrounding quotes.
		Ω(vcapApplication).Should(MatchJSON(
			`{
				"foo": 1,
				"bar": 2,
				"port": 8080,
				"host": "0.0.0.0",
				"instance_id": "PLACEHOLDER_INSTANCE_GUID",
				"instance_index": "PLACEHOLDER_INSTANCE_INDEX"
			}`,
		))
	})

	Context("when there is no file descriptor limit", func() {
		BeforeEach(func() {
			desiredLRP.FileDescriptors = 0
		})

		It("does not set any FD limit on the run action", func() {
			auction, err := builder.Build(desiredLRP)
			Ω(err).ShouldNot(HaveOccurred())

			Ω(auction.Actions).Should(HaveLen(3))

			parallelAction, ok := auction.Actions[2].Action.(models.ParallelAction)
			Ω(ok).Should(BeTrue())

			runAction, ok := parallelAction.Actions[0].Action.(models.RunAction)
			Ω(ok).Should(BeTrue())

			Ω(runAction.ResourceLimits).Should(Equal(models.ResourceLimits{
				Nofile: nil,
			}))
		})
	})

	Context("when requesting a stack with no associated health-check", func() {
		BeforeEach(func() {
			desiredLRP.Stack = "some-other-stack"
		})

		It("should error", func() {
			auction, err := builder.Build(desiredLRP)
			Ω(err).Should(MatchError(ErrNoCircusDefined))
			Ω(auction).Should(BeZero())
		})
	})
})
