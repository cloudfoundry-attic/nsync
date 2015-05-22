package recipebuilder_test

import (
	"encoding/json"
	"errors"
	"time"

	"github.com/cloudfoundry-incubator/diego-ssh/keys"
	"github.com/cloudfoundry-incubator/diego-ssh/keys/fake_keys"
	"github.com/cloudfoundry-incubator/diego-ssh/routes"
	"github.com/cloudfoundry-incubator/nsync/recipebuilder"
	"github.com/cloudfoundry-incubator/receptor"
	"github.com/cloudfoundry-incubator/route-emitter/cfroutes"
	"github.com/cloudfoundry-incubator/runtime-schema/cc_messages"
	"github.com/cloudfoundry-incubator/runtime-schema/models"
	"github.com/pivotal-golang/lager"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
)

var _ = Describe("Recipe Builder", func() {
	var (
		builder        *recipebuilder.RecipeBuilder
		err            error
		desiredAppReq  cc_messages.DesireAppRequestFromCC
		desiredLRP     *receptor.DesiredLRPCreateRequest
		lifecycles     map[string]string
		egressRules    []models.SecurityGroupRule
		fakeKeyFactory *fake_keys.FakeSSHKeyFactory
	)

	defaultNofile := recipebuilder.DefaultFileDescriptorLimit

	BeforeEach(func() {
		logger := lager.NewLogger("fakelogger")

		lifecycles = map[string]string{
			"buildpack/some-stack": "some-lifecycle.tgz",
			"docker":               "the/docker/lifecycle/path.tgz",
		}

		egressRules = []models.SecurityGroupRule{
			{
				Protocol:     "TCP",
				Destinations: []string{"0.0.0.0/0"},
				PortRange:    &models.PortRange{Start: 80, End: 443},
			},
		}

		fakeKeyFactory = &fake_keys.FakeSSHKeyFactory{}
		builder = recipebuilder.New(logger, lifecycles, "http://file-server.com", fakeKeyFactory)

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
			Routes:          []string{"route1", "route2"},
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
				Expect(desiredLRP.CPUWeight).To(Equal(uint(1)))
			})
		})

		Context("when the memory limit is above the maximum value", func() {
			BeforeEach(func() {
				desiredAppReq.MemoryMB = recipebuilder.MaxCpuProxy + 9999
			})

			It("returns 100", func() {
				Expect(desiredLRP.CPUWeight).To(Equal(uint(100)))
			})
		})

		Context("when the memory limit is in between the minimum and maximum value", func() {
			BeforeEach(func() {
				desiredAppReq.MemoryMB = (recipebuilder.MinCpuProxy + recipebuilder.MaxCpuProxy) / 2
			})

			It("returns 50", func() {
				Expect(desiredLRP.CPUWeight).To(Equal(uint(50)))
			})
		})
	})

	Context("when everything is correct", func() {
		It("does not error", func() {
			Expect(err).NotTo(HaveOccurred())
		})

		It("builds a valid DesiredLRP", func() {
			Expect(desiredLRP.ProcessGuid).To(Equal("the-app-guid-the-app-version"))
			Expect(desiredLRP.Instances).To(Equal(23))
			Expect(desiredLRP.Routes).To(Equal(cfroutes.CFRoutes{
				{Hostnames: []string{"route1", "route2"}, Port: 8080},
			}.RoutingInfo()))

			Expect(desiredLRP.Annotation).To(Equal("etag-updated-at"))
			Expect(desiredLRP.RootFS).To(Equal(models.PreloadedRootFS("some-stack")))
			Expect(desiredLRP.MemoryMB).To(Equal(128))
			Expect(desiredLRP.DiskMB).To(Equal(512))
			Expect(desiredLRP.Ports).To(Equal([]uint16{8080}))
			Expect(desiredLRP.Privileged).To(BeTrue())
			Expect(desiredLRP.StartTimeout).To(Equal(uint(123456)))

			Expect(desiredLRP.LogGuid).To(Equal("the-log-id"))
			Expect(desiredLRP.LogSource).To(Equal("CELL"))

			Expect(desiredLRP.EnvironmentVariables).To(ConsistOf(receptor.EnvironmentVariable{"LANG", recipebuilder.DefaultLANG}))

			Expect(desiredLRP.MetricsGuid).To(Equal("the-log-id"))

			expectedSetup := models.Serial([]models.Action{
				&models.DownloadAction{
					From:     "http://file-server.com/v1/static/some-lifecycle.tgz",
					To:       "/tmp/lifecycle",
					CacheKey: "buildpack-some-stack-lifecycle",
				},
				&models.DownloadAction{
					From:     "http://the-droplet.uri.com",
					To:       ".",
					CacheKey: "droplets-the-app-guid-the-app-version",
				},
			}...)
			Expect(desiredLRP.Setup).To(Equal(expectedSetup))

			parallelRunAction, ok := desiredLRP.Action.(*models.ParallelAction)
			Expect(ok).To(BeTrue())
			Expect(parallelRunAction.Actions).To(HaveLen(1))

			runAction, ok := parallelRunAction.Actions[0].(*models.RunAction)
			Expect(ok).To(BeTrue())

			Expect(desiredLRP.Monitor).To(Equal(&models.TimeoutAction{
				Timeout: 30 * time.Second,
				Action: &models.RunAction{
					Path: "/tmp/lifecycle/healthcheck",
					Args: []string{"-port=8080"},
					Env: []models.EnvironmentVariable{
						models.EnvironmentVariable{Name: "PORT", Value: "8080"},
					},
					LogSource: "HEALTH",
					ResourceLimits: models.ResourceLimits{
						Nofile: &defaultNofile,
					},
				},
			}))

			Expect(runAction.Path).To(Equal("/tmp/lifecycle/launcher"))
			Expect(runAction.Args).To(Equal([]string{
				"app",
				"the-start-command with-arguments",
				"the-execution-metadata",
			}))

			Expect(runAction.LogSource).To(Equal("APP"))

			numFiles := uint64(32)
			Expect(runAction.ResourceLimits).To(Equal(models.ResourceLimits{
				Nofile: &numFiles,
			}))

			Expect(runAction.Env).To(ContainElement(models.EnvironmentVariable{
				Name:  "foo",
				Value: "bar",
			}))

			Expect(runAction.Env).To(ContainElement(models.EnvironmentVariable{
				Name:  "PORT",
				Value: "8080",
			}))

			Expect(desiredLRP.EgressRules).To(ConsistOf(egressRules))
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

				Expect(downloadDestinations).To(ContainElement("/tmp/lifecycle"))

				Expect(desiredLRP.Monitor).To(Equal(&models.TimeoutAction{
					Timeout: 30 * time.Second,
					Action: &models.RunAction{
						Path: "/tmp/lifecycle/healthcheck",
						Args: []string{"-port=8080"},
						Env: []models.EnvironmentVariable{
							models.EnvironmentVariable{Name: "PORT", Value: "8080"},
						},
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
				Expect(desiredLRP.Monitor).To(BeNil())
			})

			It("still downloads the lifecycle, since we need it for the launcher", func() {
				downloadDestinations := []string{}
				for _, action := range desiredLRP.Setup.(*models.SerialAction).Actions {
					switch a := action.(type) {
					case *models.DownloadAction:
						downloadDestinations = append(downloadDestinations, a.To)
					}
				}

				Expect(downloadDestinations).To(ContainElement("/tmp/lifecycle"))
			})
		})

		Context("when allow ssh is true", func() {
			BeforeEach(func() {
				desiredAppReq.AllowSSH = true

				keyPairChan := make(chan keys.KeyPair, 2)

				fakeHostKeyPair := &fake_keys.FakeKeyPair{}
				fakeHostKeyPair.PEMEncodedPrivateKeyReturns("pem-host-private-key")
				fakeHostKeyPair.FingerprintReturns("host-fingerprint")

				fakeUserKeyPair := &fake_keys.FakeKeyPair{}
				fakeUserKeyPair.AuthorizedKeyReturns("authorized-user-key")
				fakeUserKeyPair.PEMEncodedPrivateKeyReturns("pem-user-private-key")

				keyPairChan <- fakeHostKeyPair
				keyPairChan <- fakeUserKeyPair

				fakeKeyFactory.NewKeyPairStub = func(bits int) (keys.KeyPair, error) {
					return <-keyPairChan, nil
				}
			})

			It("setup should download the ssh daemon", func() {
				expectedSetup := models.Serial([]models.Action{
					&models.DownloadAction{
						From:     "http://file-server.com/v1/static/some-lifecycle.tgz",
						To:       "/tmp/lifecycle",
						CacheKey: "buildpack-some-stack-lifecycle",
					},
					&models.DownloadAction{
						From:     "http://the-droplet.uri.com",
						To:       ".",
						CacheKey: "droplets-the-app-guid-the-app-version",
					},
					&models.DownloadAction{
						From:     "http://file-server.com/v1/static/diego-sshd/diego-sshd.tgz",
						To:       "/tmp/ssh",
						CacheKey: "diego-sshd",
					},
				}...)

				Expect(desiredLRP.Setup).To(Equal(expectedSetup))
			})

			It("runs the ssh daemon in the container", func() {
				expectedNumFiles := uint64(32)

				expectedAction := models.Parallel([]models.Action{
					&models.RunAction{
						Path: "/tmp/lifecycle/launcher",
						Args: []string{
							"app",
							"the-start-command with-arguments",
							"the-execution-metadata",
						},
						Env: []models.EnvironmentVariable{
							{Name: "foo", Value: "bar"},
							{Name: "PORT", Value: "8080"},
						},
						ResourceLimits: models.ResourceLimits{
							Nofile: &expectedNumFiles,
						},
						LogSource: "APP",
					},
					&models.RunAction{
						Path: "/tmp/ssh/diego-sshd",
						Args: []string{
							"-address=0.0.0.0:2222",
							"-hostKey=pem-host-private-key",
							"-authorizedKey=authorized-user-key",
							"-inheritDaemonEnv",
							"-logLevel=fatal",
						},
						Env: []models.EnvironmentVariable{
							{Name: "foo", Value: "bar"},
							{Name: "PORT", Value: "8080"},
						},
						ResourceLimits: models.ResourceLimits{
							Nofile: &expectedNumFiles,
						},
					},
				}...)

				Expect(desiredLRP.Action).To(Equal(expectedAction))
			})

			It("opens up the default ssh port", func() {
				Expect(desiredLRP.Ports).To(Equal([]uint16{
					8080,
					2222,
				}))
			})

			It("declares ssh routing information in the LRP", func() {
				cfRoutePayload, err := json.Marshal(cfroutes.CFRoutes{
					{Hostnames: []string{"route1", "route2"}, Port: 8080},
				})
				Expect(err).NotTo(HaveOccurred())

				sshRoutePayload, err := json.Marshal(routes.SSHRoute{
					ContainerPort:   2222,
					PrivateKey:      "pem-user-private-key",
					HostFingerprint: "host-fingerprint",
				})
				Expect(err).NotTo(HaveOccurred())

				cfRouteMessage := json.RawMessage(cfRoutePayload)
				sshRouteMessage := json.RawMessage(sshRoutePayload)

				Expect(desiredLRP.Routes).To(Equal(receptor.RoutingInfo{
					cfroutes.CF_ROUTER: &cfRouteMessage,
					routes.DIEGO_SSH:   &sshRouteMessage,
				}))
			})

			Context("when generating the host key fails", func() {
				BeforeEach(func() {
					fakeKeyFactory.NewKeyPairReturns(nil, errors.New("boom"))
				})

				It("should return an error", func() {
					Expect(err).To(HaveOccurred())
				})
			})

			Context("when generating the user key fails", func() {
				BeforeEach(func() {
					errorCh := make(chan error, 2)
					errorCh <- nil
					errorCh <- errors.New("woops")

					fakeKeyFactory.NewKeyPairStub = func(bits int) (keys.KeyPair, error) {
						return nil, <-errorCh
					}
				})

				It("should return an error", func() {
					Expect(err).To(HaveOccurred())
				})
			})
		})
	})

	Context("when there is a docker image url instead of a droplet uri", func() {
		BeforeEach(func() {
			desiredAppReq.DockerImageUrl = "user/repo:tag"
			desiredAppReq.DropletUri = ""
		})

		It("does not error", func() {
			Expect(err).NotTo(HaveOccurred())
		})

		It("uses an unprivileged container", func() {
			Expect(desiredLRP.Privileged).To(BeFalse())
		})

		It("converts the docker image url into a root fs path", func() {
			Expect(desiredLRP.RootFS).To(Equal("docker:///user/repo#tag"))
		})

		It("uses the docker lifecycle", func() {
			Expect(desiredLRP.Setup.(*models.SerialAction).Actions[0]).To(Equal(&models.DownloadAction{
				From:     "http://file-server.com/v1/static/the/docker/lifecycle/path.tgz",
				To:       "/tmp/lifecycle",
				CacheKey: "docker-lifecycle",
			}))
		})

		testRootFSPath := func(imageUrl string, expectedRootFSPath string) func() {
			return func() {
				BeforeEach(func() {
					desiredAppReq.DockerImageUrl = imageUrl
				})

				It("builds correct rootFS path", func() {
					Expect(desiredLRP.RootFS).To(Equal(expectedRootFSPath))
				})
			}
		}

		Context("and the docker image url has no host", func() {
			Context("and image only", testRootFSPath("image", "docker:///library/image"))
			//does not specify a url fragment for the tag, assumes garden-linux sets a default
			Context("and user/image", testRootFSPath("user/image", "docker:///user/image"))
			Context("and a image with tag", testRootFSPath("image:tag", "docker:///library/image#tag"))
			Context("and a user/image with tag", testRootFSPath("user/image:tag", "docker:///user/image#tag"))
		})

		Context("and the docker image url has host:port", func() {
			Context("and image only", testRootFSPath("10.244.2.6:8080/image", "docker://10.244.2.6:8080/image"))
			Context("and user/image", testRootFSPath("10.244.2.6:8080/user/image", "docker://10.244.2.6:8080/user/image"))
			Context("and a image with tag", testRootFSPath("10.244.2.6:8080/image:tag", "docker://10.244.2.6:8080/image#tag"))
			Context("and a user/image with tag", testRootFSPath("10.244.2.6:8080/user/image:tag", "docker://10.244.2.6:8080/user/image#tag"))
		})

		Context("and the docker image url has host docker.io", func() {
			Context("and image only", testRootFSPath("docker.io/image", "docker://docker.io/library/image"))
			Context("and user/image", testRootFSPath("docker.io/user/image", "docker://docker.io/user/image"))
			Context("and image with tag", testRootFSPath("docker.io/image:tag", "docker://docker.io/library/image#tag"))
			Context("and a user/image with tag", testRootFSPath("docker.io/user/image:tag", "docker://docker.io/user/image#tag"))
		})

		Context("and the docker image url has scheme", func() {
			BeforeEach(func() {
				desiredAppReq.DockerImageUrl = "https://docker.io/repo"
			})

			It("errors", func() {
				Expect(err).To(HaveOccurred())
			})
		})

		It("does not set the container's LANG", func() {
			Expect(desiredLRP.EnvironmentVariables).To(BeEmpty())
		})
	})

	Context("when there is a docker image url AND a droplet uri", func() {
		BeforeEach(func() {
			desiredAppReq.DockerImageUrl = "user/repo:tag"
			desiredAppReq.DropletUri = "http://the-droplet.uri.com"
		})

		It("should error", func() {
			Expect(err).To(MatchError(recipebuilder.ErrMultipleAppSources))
		})
	})

	Context("when there is NEITHER a docker image url NOR a droplet uri", func() {
		BeforeEach(func() {
			desiredAppReq.DockerImageUrl = ""
			desiredAppReq.DropletUri = ""
		})

		It("should error", func() {
			Expect(err).To(MatchError(recipebuilder.ErrAppSourceMissing))
		})
	})

	Context("when there is no file descriptor limit", func() {
		BeforeEach(func() {
			desiredAppReq.FileDescriptors = 0
		})

		It("does not error", func() {
			Expect(err).NotTo(HaveOccurred())
		})

		It("sets a default FD limit on the run action", func() {
			parallelRunAction, ok := desiredLRP.Action.(*models.ParallelAction)
			Expect(ok).To(BeTrue())
			Expect(parallelRunAction.Actions).To(HaveLen(1))

			runAction, ok := parallelRunAction.Actions[0].(*models.RunAction)
			Expect(ok).To(BeTrue())

			Expect(runAction.ResourceLimits.Nofile).NotTo(BeNil())
			Expect(*runAction.ResourceLimits.Nofile).To(Equal(recipebuilder.DefaultFileDescriptorLimit))
		})
	})

	Context("when requesting a stack with no associated health-check", func() {
		BeforeEach(func() {
			desiredAppReq.Stack = "some-other-stack"
		})

		It("should error", func() {
			Expect(err).To(MatchError(recipebuilder.ErrNoLifecycleDefined))
		})
	})
})
