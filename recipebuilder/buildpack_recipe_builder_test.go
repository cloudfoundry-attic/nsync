package recipebuilder_test

import (
	"encoding/json"
	"errors"
	"time"

	"github.com/cloudfoundry-incubator/bbs/models"
	"github.com/cloudfoundry-incubator/diego-ssh/keys"
	"github.com/cloudfoundry-incubator/diego-ssh/keys/fake_keys"
	"github.com/cloudfoundry-incubator/diego-ssh/routes"
	"github.com/cloudfoundry-incubator/nsync/recipebuilder"
	"github.com/cloudfoundry-incubator/routing-info/cfroutes"
	"github.com/cloudfoundry-incubator/runtime-schema/cc_messages"
	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
	"github.com/pivotal-golang/lager/lagertest"
)

var _ = Describe("Buildpack Recipe Builder", func() {
	var (
		builder        *recipebuilder.BuildpackRecipeBuilder
		err            error
		desiredAppReq  cc_messages.DesireAppRequestFromCC
		lifecycles     map[string]string
		egressRules    []*models.SecurityGroupRule
		fakeKeyFactory *fake_keys.FakeSSHKeyFactory
		logger         *lagertest.TestLogger
	)

	defaultNofile := recipebuilder.DefaultFileDescriptorLimit

	BeforeEach(func() {
		logger = lagertest.NewTestLogger("test")

		lifecycles = map[string]string{
			"buildpack/some-stack": "some-lifecycle.tgz",
			"docker":               "the/docker/lifecycle/path.tgz",
		}

		egressRules = []*models.SecurityGroupRule{
			{
				Protocol:     "TCP",
				Destinations: []string{"0.0.0.0/0"},
				PortRange:    &models.PortRange{Start: 80, End: 443},
			},
		}

		fakeKeyFactory = &fake_keys.FakeSSHKeyFactory{}
		config := recipebuilder.Config{lifecycles, "http://file-server.com", fakeKeyFactory}
		builder = recipebuilder.NewBuildpackRecipeBuilder(logger, config)

		routingInfo, err := cc_messages.CCHTTPRoutes{
			{Hostname: "route1"},
			{Hostname: "route2"},
		}.CCRouteInfo()
		Expect(err).NotTo(HaveOccurred())

		desiredAppReq = cc_messages.DesireAppRequestFromCC{
			ProcessGuid:       "the-app-guid-the-app-version",
			DropletUri:        "http://the-droplet.uri.com",
			Stack:             "some-stack",
			StartCommand:      "the-start-command with-arguments",
			ExecutionMetadata: "the-execution-metadata",
			Environment: []*models.EnvironmentVariable{
				{Name: "foo", Value: "bar"},
			},
			MemoryMB:        128,
			DiskMB:          512,
			FileDescriptors: 32,
			NumInstances:    23,
			RoutingInfo:     routingInfo,
			LogGuid:         "the-log-id",

			HealthCheckType:             cc_messages.PortHealthCheckType,
			HealthCheckTimeoutInSeconds: 123456,

			EgressRules: egressRules,

			ETag: "etag-updated-at",
		}
	})

	Describe("Build", func() {
		var desiredLRP *models.DesiredLRP

		JustBeforeEach(func() {
			desiredLRP, err = builder.Build(&desiredAppReq)
		})

		Describe("CPU weight calculation", func() {
			Context("when the memory limit is below the minimum value", func() {
				BeforeEach(func() {
					desiredAppReq.MemoryMB = recipebuilder.MinCpuProxy - 9999
				})

				It("returns 1", func() {
					Expect(desiredLRP.CpuWeight).To(BeEquivalentTo(1))
				})
			})

			Context("when the memory limit is above the maximum value", func() {
				BeforeEach(func() {
					desiredAppReq.MemoryMB = recipebuilder.MaxCpuProxy + 9999
				})

				It("returns 100", func() {
					Expect(desiredLRP.CpuWeight).To(BeEquivalentTo(100))
				})
			})

			Context("when the memory limit is in between the minimum and maximum value", func() {
				BeforeEach(func() {
					desiredAppReq.MemoryMB = (recipebuilder.MinCpuProxy + recipebuilder.MaxCpuProxy) / 2
				})

				It("returns 50", func() {
					Expect(desiredLRP.CpuWeight).To(BeEquivalentTo(50))
				})
			})
		})

		Context("when everything is correct", func() {
			It("does not error", func() {
				Expect(err).NotTo(HaveOccurred())
			})

			It("builds a valid DesiredLRP", func() {
				Expect(desiredLRP.ProcessGuid).To(Equal("the-app-guid-the-app-version"))
				Expect(desiredLRP.Instances).To(BeEquivalentTo(23))
				Expect(*desiredLRP.Routes).To(Equal(cfroutes.CFRoutes{
					{Hostnames: []string{"route1", "route2"}, Port: 8080},
				}.RoutingInfo()))

				Expect(desiredLRP.Annotation).To(Equal("etag-updated-at"))
				Expect(desiredLRP.RootFs).To(Equal(models.PreloadedRootFS("some-stack")))
				Expect(desiredLRP.MemoryMb).To(BeEquivalentTo(128))
				Expect(desiredLRP.DiskMb).To(BeEquivalentTo(512))
				Expect(desiredLRP.Ports).To(Equal([]uint32{8080}))
				Expect(desiredLRP.Privileged).To(BeTrue())
				Expect(desiredLRP.StartTimeout).To(BeEquivalentTo(123456))

				Expect(desiredLRP.LogGuid).To(Equal("the-log-id"))
				Expect(desiredLRP.LogSource).To(Equal("CELL"))

				Expect(desiredLRP.EnvironmentVariables).To(ConsistOf(&models.EnvironmentVariable{"LANG", recipebuilder.DefaultLANG}))

				Expect(desiredLRP.MetricsGuid).To(Equal("the-log-id"))

				expectedSetup := models.Serial(
					&models.DownloadAction{
						From:     "http://the-droplet.uri.com",
						To:       ".",
						CacheKey: "droplets-the-app-guid-the-app-version",
						User:     "vcap",
					},
				)
				Expect(desiredLRP.Setup.GetValue()).To(Equal(expectedSetup))

				expectedCacheDependencies := []*models.CachedDependency{
					&models.CachedDependency{
						From:     "http://file-server.com/v1/static/some-lifecycle.tgz",
						To:       "/tmp/lifecycle",
						CacheKey: "buildpack-some-stack-lifecycle",
					},
				}
				Expect(desiredLRP.CachedDependencies).To(BeEquivalentTo(expectedCacheDependencies))
				Expect(desiredLRP.LegacyDownloadUser).To(Equal("vcap"))

				parallelRunAction := desiredLRP.Action.CodependentAction
				Expect(parallelRunAction.Actions).To(HaveLen(1))

				runAction := parallelRunAction.Actions[0].RunAction

				Expect(desiredLRP.Monitor.GetValue()).To(Equal(models.Timeout(
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
						},
					},
					30*time.Second,
				)))

				Expect(runAction.Path).To(Equal("/tmp/lifecycle/launcher"))
				Expect(runAction.Args).To(Equal([]string{
					"app",
					"the-start-command with-arguments",
					"the-execution-metadata",
				}))

				Expect(runAction.LogSource).To(Equal("APP"))

				numFiles := uint64(32)
				Expect(runAction.ResourceLimits).To(Equal(&models.ResourceLimits{
					Nofile: &numFiles,
				}))

				Expect(runAction.Env).To(ContainElement(&models.EnvironmentVariable{
					Name:  "foo",
					Value: "bar",
				}))

				Expect(runAction.Env).To(ContainElement(&models.EnvironmentVariable{
					Name:  "PORT",
					Value: "8080",
				}))

				Expect(desiredLRP.EgressRules).To(ConsistOf(egressRules))
			})

			Context("when route service url is specified in RoutingInfo", func() {
				BeforeEach(func() {
					routingInfo, err := cc_messages.CCHTTPRoutes{
						{Hostname: "route1"},
						{Hostname: "route2", RouteServiceUrl: "https://rs.example.com"},
					}.CCRouteInfo()
					Expect(err).NotTo(HaveOccurred())
					desiredAppReq.RoutingInfo = routingInfo
				})

				It("sets up routes with the route service url", func() {
					routes := *desiredLRP.Routes
					cfRoutesJson := routes[cfroutes.CF_ROUTER]
					cfRoutes := cfroutes.CFRoutes{}

					err := json.Unmarshal(*cfRoutesJson, &cfRoutes)
					Expect(err).ToNot(HaveOccurred())

					Expect(cfRoutes).To(ConsistOf([]cfroutes.CFRoute{
						{Hostnames: []string{"route1"}, Port: 8080},
						{Hostnames: []string{"route2"}, Port: 8080, RouteServiceUrl: "https://rs.example.com"},
					}))
				})
			})

			Context("when no health check is specified", func() {
				BeforeEach(func() {
					desiredAppReq.HealthCheckType = cc_messages.UnspecifiedHealthCheckType
				})

				It("sets up the port check for backwards compatibility", func() {
					downloadDestinations := []string{}
					for _, dep := range desiredLRP.CachedDependencies {
						if dep != nil {
							downloadDestinations = append(downloadDestinations, dep.To)
						}
					}

					Expect(downloadDestinations).To(ContainElement("/tmp/lifecycle"))

					Expect(desiredLRP.Monitor.GetValue()).To(Equal(models.Timeout(
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
							},
						},
						30*time.Second,
					)))
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
					for _, dep := range desiredLRP.CachedDependencies {
						if dep != nil {
							downloadDestinations = append(downloadDestinations, dep.To)
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
					expectedCacheDependencies := []*models.CachedDependency{
						{
							From:     "http://file-server.com/v1/static/some-lifecycle.tgz",
							To:       "/tmp/lifecycle",
							CacheKey: "buildpack-some-stack-lifecycle",
						},
					}

					Expect(desiredLRP.CachedDependencies).To(BeEquivalentTo(expectedCacheDependencies))
				})

				It("runs the ssh daemon in the container", func() {
					expectedNumFiles := uint64(32)

					expectedAction := models.Codependent(
						&models.RunAction{
							User: "vcap",
							Path: "/tmp/lifecycle/launcher",
							Args: []string{
								"app",
								"the-start-command with-arguments",
								"the-execution-metadata",
							},
							Env: []*models.EnvironmentVariable{
								{Name: "foo", Value: "bar"},
								{Name: "PORT", Value: "8080"},
							},
							ResourceLimits: &models.ResourceLimits{
								Nofile: &expectedNumFiles,
							},
							LogSource: "APP",
						},
						&models.RunAction{
							User: "vcap",
							Path: "/tmp/lifecycle/diego-sshd",
							Args: []string{
								"-address=0.0.0.0:2222",
								"-hostKey=pem-host-private-key",
								"-authorizedKey=authorized-user-key",
								"-inheritDaemonEnv",
								"-logLevel=fatal",
							},
							Env: []*models.EnvironmentVariable{
								{Name: "foo", Value: "bar"},
								{Name: "PORT", Value: "8080"},
							},
							ResourceLimits: &models.ResourceLimits{
								Nofile: &expectedNumFiles,
							},
						},
					)

					Expect(desiredLRP.Action.GetValue()).To(Equal(expectedAction))
				})

				It("opens up the default ssh port", func() {
					Expect(desiredLRP.Ports).To(Equal([]uint32{
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

					Expect(desiredLRP.Routes).To(Equal(&models.Routes{
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
				Expect(err).To(MatchError(recipebuilder.ErrDropletSourceMissing))
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
				parallelRunAction := desiredLRP.Action.CodependentAction
				Expect(parallelRunAction.Actions).To(HaveLen(1))

				runAction := parallelRunAction.Actions[0].RunAction

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

		Context("when app ports are passed", func() {
			BeforeEach(func() {
				desiredAppReq.Ports = []uint32{1456, 2345, 3456}
			})

			It("builds a DesiredLRP with the correct ports", func() {
				Expect(desiredLRP.Ports).To(Equal([]uint32{1456, 2345, 3456}))
			})

			It("sets the health check to the first provided port", func() {
				Expect(desiredLRP.Monitor.GetValue()).To(Equal(models.Timeout(
					&models.ParallelAction{
						Actions: []*models.Action{
							&models.Action{
								RunAction: &models.RunAction{
									User:      "vcap",
									Path:      "/tmp/lifecycle/healthcheck",
									Args:      []string{"-port=1456"},
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
									Args:      []string{"-port=2345"},
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
									Args:      []string{"-port=3456"},
									LogSource: "HEALTH",
									ResourceLimits: &models.ResourceLimits{
										Nofile: &defaultNofile,
									},
								},
							},
						},
					},
					30*time.Second,
				)))
			})
		})
	})

	Describe("ExtractExposedPorts", func() {
		Context("when desired app request has ports specified", func() {
			BeforeEach(func() {
				desiredAppReq.Ports = []uint32{1456, 2345, 3456}
			})

			It("returns the ports specified in desired app request as exposed ports", func() {
				ports, err := builder.ExtractExposedPorts(&desiredAppReq)
				Expect(err).ToNot(HaveOccurred())
				Expect(ports).To(Equal(desiredAppReq.Ports))
			})
		})

		Context("when desired app request does not have any ports specified", func() {
			It("returns the slice with default port", func() {
				ports, err := builder.ExtractExposedPorts(&desiredAppReq)
				Expect(err).ToNot(HaveOccurred())
				Expect(ports).To(Equal([]uint32{8080}))
			})
		})
	})
})
