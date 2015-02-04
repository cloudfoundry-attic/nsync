package recipebuilder_test

import (
	"time"

	"github.com/cloudfoundry-incubator/nsync/recipebuilder"
	"github.com/cloudfoundry-incubator/receptor"
	"github.com/cloudfoundry-incubator/runtime-schema/cc_messages"
	"github.com/cloudfoundry-incubator/runtime-schema/models"
	"github.com/pivotal-golang/lager"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
)

var _ = Describe("Recipe Builder", func() {
	var (
		builder       *recipebuilder.RecipeBuilder
		err           error
		desiredAppReq cc_messages.DesireAppRequestFromCC
		desiredLRP    *receptor.DesiredLRPCreateRequest
		lifecycles    map[string]string
		egressRules   []models.SecurityGroupRule
	)

	defaultNofile := recipebuilder.DefaultFileDescriptorLimit

	BeforeEach(func() {
		logger := lager.NewLogger("fakelogger")

		lifecycles = map[string]string{
			"some-stack": "some-lifecycle.tgz",
		}

		egressRules = []models.SecurityGroupRule{
			{
				Protocol:     "TCP",
				Destinations: []string{"0.0.0.0/0"},
				PortRange:    &models.PortRange{Start: 80, End: 443},
			},
		}

		builder = recipebuilder.New(lifecycles, "the/docker/lifecycle/path.tgz", "http://file-server.com", logger)

		desiredAppReq = cc_messages.DesireAppRequestFromCC{
			ProcessGuid:       "the-app-guid-the-app-version",
			DropletUri:        "http://the-droplet.uri.com",
			Stack:             "some-stack",
			StartCommand:      "the-start-command with-arguments",
			ExecutionMetadata: "the-execution-metadata",
			Environment: cc_messages.Environment{
				{Name: "foo", Value: "bar"},
			},
			MemoryMB:        128,
			DiskMB:          512,
			FileDescriptors: 32,
			NumInstances:    23,
			Hostnames:       []string{"route1", "route2"},
			LogGuid:         "the-log-id",

			HealthCheckType:             cc_messages.PortHealthCheckType,
			HealthCheckTimeoutInSeconds: 123456,

			EgressRules: egressRules,

			ETag: "etag-updated-at",
		}
	})

	JustBeforeEach(func() {
		desiredLRP, err = builder.Build(&desiredAppReq)
	})

	Describe("CPU weight calculation", func() {
		Context("when the memory limit is below the minimum value", func() {
			BeforeEach(func() {
				desiredAppReq.MemoryMB = recipebuilder.MinCpuProxy - 9999
			})
			It("returns 1", func() {
				Ω(desiredLRP.CPUWeight).Should(Equal(uint(1)))
			})
		})

		Context("when the memory limit is above the maximum value", func() {
			BeforeEach(func() {
				desiredAppReq.MemoryMB = recipebuilder.MaxCpuProxy + 9999
			})
			It("returns 100", func() {
				Ω(desiredLRP.CPUWeight).Should(Equal(uint(100)))
			})
		})

		Context("when the memory limit is in between the minimum and maximum value", func() {
			BeforeEach(func() {
				desiredAppReq.MemoryMB = (recipebuilder.MinCpuProxy + recipebuilder.MaxCpuProxy) / 2
			})
			It("returns 50", func() {
				Ω(desiredLRP.CPUWeight).Should(Equal(uint(50)))
			})
		})
	})

	Context("when everything is correct", func() {
		It("does not error", func() {
			Ω(err).ShouldNot(HaveOccurred())
		})

		It("builds a valid DesiredLRP", func() {
			Ω(desiredLRP.ProcessGuid).Should(Equal("the-app-guid-the-app-version"))
			Ω(desiredLRP.Instances).Should(Equal(23))
			Ω(*desiredLRP.Routes).Should(Equal(receptor.RoutingInfo{
				CFRoutes: []receptor.CFRoute{
					{
						Port:      8080,
						Hostnames: []string{"route1", "route2"},
					},
				},
			}))
			Ω(desiredLRP.Annotation).Should(Equal("etag-updated-at"))
			Ω(desiredLRP.Stack).Should(Equal("some-stack"))
			Ω(desiredLRP.MemoryMB).Should(Equal(128))
			Ω(desiredLRP.DiskMB).Should(Equal(512))
			Ω(desiredLRP.Ports).Should(Equal([]uint16{8080}))
			Ω(desiredLRP.Privileged).Should(BeTrue())
			Ω(desiredLRP.StartTimeout).Should(Equal(uint(123456)))

			Ω(desiredLRP.LogGuid).Should(Equal("the-log-id"))
			Ω(desiredLRP.LogSource).Should(Equal("CELL"))

			expectedSetup := models.Serial([]models.Action{
				&models.DownloadAction{
					From: "http://file-server.com/v1/static/some-lifecycle.tgz",
					To:   "/tmp/lifecycle",
				},
				&models.DownloadAction{
					From:     "http://the-droplet.uri.com",
					To:       ".",
					CacheKey: "droplets-the-app-guid-the-app-version",
				},
			}...)
			Ω(desiredLRP.Setup).Should(Equal(expectedSetup))

			runAction, ok := desiredLRP.Action.(*models.RunAction)
			Ω(ok).Should(BeTrue())

			Ω(desiredLRP.Monitor).Should(Equal(&models.TimeoutAction{
				Timeout: 30 * time.Second,
				Action: &models.RunAction{
					Path:      "/tmp/lifecycle/healthcheck",
					Args:      []string{"-port=8080"},
					LogSource: "HEALTH",
					ResourceLimits: models.ResourceLimits{
						Nofile: &defaultNofile,
					},
				},
			}))

			Ω(runAction.Path).Should(Equal("/tmp/lifecycle/launcher"))
			Ω(runAction.Args).Should(Equal([]string{
				"/app",
				"the-start-command with-arguments",
				"the-execution-metadata",
			}))
			Ω(runAction.LogSource).Should(Equal("APP"))

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

			Ω(desiredLRP.EgressRules).Should(ConsistOf(egressRules))
		})

		Context("when no health check is specified", func() {
			BeforeEach(func() {
				desiredAppReq.HealthCheckType = cc_messages.UnspecifiedHealthCheckType
			})

			It("sets up the port check for backwards compatibility", func() {
				downloadDestinations := []string{}
				for _, action := range desiredLRP.Setup.(*models.SerialAction).Actions {
					switch a := action.(type) {
					case *models.DownloadAction:
						downloadDestinations = append(downloadDestinations, a.To)
					}
				}

				Ω(downloadDestinations).Should(ContainElement("/tmp/lifecycle"))

				Ω(desiredLRP.Monitor).Should(Equal(&models.TimeoutAction{
					Timeout: 30 * time.Second,
					Action: &models.RunAction{
						Path:           "/tmp/lifecycle/healthcheck",
						Args:           []string{"-port=8080"},
						LogSource:      "HEALTH",
						ResourceLimits: models.ResourceLimits{Nofile: &defaultNofile},
					},
				}))
			})
		})

		Context("when the 'none' health check is specified", func() {
			BeforeEach(func() {
				desiredAppReq.HealthCheckType = cc_messages.NoneHealthCheckType
			})

			It("does not populate the monitor action", func() {
				Ω(desiredLRP.Monitor).Should(BeNil())
			})

			It("still downloads the lifecycle, since we need it for the launcher", func() {
				downloadDestinations := []string{}
				for _, action := range desiredLRP.Setup.(*models.SerialAction).Actions {
					switch a := action.(type) {
					case *models.DownloadAction:
						downloadDestinations = append(downloadDestinations, a.To)
					}
				}

				Ω(downloadDestinations).Should(ContainElement("/tmp/lifecycle"))
			})
		})
	})

	Context("when there is a docker image url instead of a droplet uri", func() {
		BeforeEach(func() {
			desiredAppReq.DockerImageUrl = "user/repo:tag"
			desiredAppReq.DropletUri = ""
		})

		It("does not error", func() {
			Ω(err).ShouldNot(HaveOccurred())
		})

		It("uses an unprivileged container", func() {
			Ω(desiredLRP.Privileged).Should(BeFalse())
		})

		It("converts the docker image url into a root fs path", func() {
			Ω(desiredLRP.RootFSPath).Should(Equal("docker:///user/repo#tag"))
		})

		It("uses the docker lifecycle", func() {
			Ω(desiredLRP.Setup.(*models.SerialAction).Actions[0]).Should(Equal(&models.DownloadAction{
				From: "http://file-server.com/v1/static/the/docker/lifecycle/path.tgz",
				To:   "/tmp/lifecycle",
			}))
		})

		Context("and the docker image url has no tag", func() {
			BeforeEach(func() {
				desiredAppReq.DockerImageUrl = "user/repo"
			})

			It("does not specify a url fragment for the tag, assumes garden-linux sets a default", func() {
				Ω(desiredLRP.RootFSPath).Should(Equal("docker:///user/repo"))
			})
		})
	})

	Context("when there is a docker image url AND a droplet uri", func() {
		BeforeEach(func() {
			desiredAppReq.DockerImageUrl = "user/repo:tag"
			desiredAppReq.DropletUri = "http://the-droplet.uri.com"
		})

		It("should error", func() {
			Ω(err).Should(MatchError(recipebuilder.ErrMultipleAppSources))
		})
	})

	Context("when there is NEITHER a docker image url NOR a droplet uri", func() {
		BeforeEach(func() {
			desiredAppReq.DockerImageUrl = ""
			desiredAppReq.DropletUri = ""
		})

		It("should error", func() {
			Ω(err).Should(MatchError(recipebuilder.ErrAppSourceMissing))
		})
	})

	Context("when there is no file descriptor limit", func() {
		BeforeEach(func() {
			desiredAppReq.FileDescriptors = 0
		})

		It("does not error", func() {
			Ω(err).ShouldNot(HaveOccurred())
		})

		It("sets a default FD limit on the run action", func() {
			runAction, ok := desiredLRP.Action.(*models.RunAction)
			Ω(ok).Should(BeTrue())

			Ω(runAction.ResourceLimits.Nofile).ShouldNot(BeNil())
			Ω(*runAction.ResourceLimits.Nofile).Should(Equal(recipebuilder.DefaultFileDescriptorLimit))
		})
	})

	Context("when requesting a stack with no associated health-check", func() {
		BeforeEach(func() {
			desiredAppReq.Stack = "some-other-stack"
		})

		It("should error", func() {
			Ω(err).Should(MatchError(recipebuilder.ErrNoLifecycleDefined))
		})
	})
})
