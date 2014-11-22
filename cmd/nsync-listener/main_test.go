package main_test

import (
	"fmt"
	"os/exec"
	"strings"
	"time"

	receptorrunner "github.com/cloudfoundry-incubator/receptor/cmd/receptor/testrunner"
	Bbs "github.com/cloudfoundry-incubator/runtime-schema/bbs"
	"github.com/cloudfoundry-incubator/runtime-schema/models"
	"github.com/cloudfoundry/gunk/diegonats"
	"github.com/cloudfoundry/gunk/timeprovider"
	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
	"github.com/onsi/gomega/gbytes"
	"github.com/pivotal-golang/lager/lagertest"
	"github.com/tedsuo/ifrit"
	"github.com/tedsuo/ifrit/ginkgomon"
)

var _ = Describe("Syncing desired state with CC", func() {
	var (
		gnatsdProcess ifrit.Process
		natsClient    diegonats.NATSClient
		bbs           *Bbs.BBS

		receptorProcess ifrit.Process

		runner  ifrit.Runner
		process ifrit.Process
	)

	startNATS := func() {
		gnatsdProcess, natsClient = diegonats.StartGnatsd(natsPort)
	}

	stopNATS := func() {
		ginkgomon.Kill(gnatsdProcess)
	}

	startReceptor := func() ifrit.Process {
		return ginkgomon.Invoke(receptorrunner.New(receptorPath, receptorrunner.Args{
			Address:                  fmt.Sprintf("127.0.0.1:%d", receptorPort),
			EtcdCluster:              strings.Join(etcdRunner.NodeURLS(), ","),
			InitialHeartbeatInterval: 1 * time.Second,
		}))
	}

	newNSyncRunner := func() *ginkgomon.Runner {
		return ginkgomon.New(ginkgomon.Config{
			Name:          "nsync",
			AnsiColorCode: "97m",
			StartCheck:    "nsync.listener.started",
			Command: exec.Command(
				listenerPath,
				"-etcdCluster", strings.Join(etcdRunner.NodeURLS(), ","),
				"-diegoAPIURL", fmt.Sprintf("http://127.0.0.1:%d", receptorPort),
				"-natsAddresses", fmt.Sprintf("127.0.0.1:%d", natsPort),
				"-circuses", `{"some-stack": "some-health-check.tar.gz"}`,
				"-dockerCircusPath", "the/docker/circus/path.tgz",
				"-fileServerURL", "http://file-server.com",
				"-heartbeatInterval", "1s",
				"-logLevel", "debug",
			),
		})
	}

	BeforeEach(func() {
		bbs = Bbs.NewBBS(etcdRunner.Adapter(), timeprovider.NewTimeProvider(), lagertest.NewTestLogger("test"))
		receptorProcess = startReceptor()
		runner = newNSyncRunner()
	})

	AfterEach(func() {
		ginkgomon.Interrupt(receptorProcess)
	})

	var publishDesireWithInstances = func(nInstances int) {
		err := natsClient.Publish("diego.desire.app", []byte(fmt.Sprintf(`
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
		Ω(err).ShouldNot(HaveOccurred())
	}

	var createRunningInstance = func(processGuid, instanceGuid string, index int) {
		err := bbs.ReportActualLRPAsRunning(models.ActualLRP{
			ProcessGuid:  processGuid,
			InstanceGuid: instanceGuid,
			Domain:       "domain",
			Index:        index,
		}, "cell-0")
		Ω(err).ShouldNot(HaveOccurred())
	}

	var publishKillIndex = func(processGuid string, index int) {
		err := natsClient.Publish("diego.kill.index", []byte(fmt.Sprintf(`
      {
        "process_guid": "%s",
        "index": %d
      }
    `, processGuid, index)))
		Ω(err).ShouldNot(HaveOccurred())
	}

	Context("when NATS is up", func() {
		BeforeEach(func() {
			startNATS()
		})

		AfterEach(func() {
			stopNATS()
		})

		Context("and the nsync listener is started", func() {
			BeforeEach(func() {
				process = ginkgomon.Invoke(runner)
			})

			AfterEach(func() {
				ginkgomon.Interrupt(process)
			})

			Describe("and a 'diego.desire.app' message is recieved", func() {
				BeforeEach(func() {
					publishDesireWithInstances(3)
				})

				It("registers an app desire in etcd", func() {
					Eventually(bbs.DesiredLRPs, 10).Should(HaveLen(1))
				})

				Context("when an app is no longer desired", func() {
					BeforeEach(func() {
						Eventually(bbs.DesiredLRPs).Should(HaveLen(1))

						publishDesireWithInstances(0)
					})

					It("should remove the desired state from etcd", func() {
						Eventually(bbs.DesiredLRPs).Should(HaveLen(0))
					})
				})
			})

			Describe("and a 'diego.kill.index' message is recieved", func() {
				var processGuid = "process-guid"

				BeforeEach(func() {
					createRunningInstance(processGuid, "instance-0", 0)
					createRunningInstance(processGuid, "instance-1", 1)
					createRunningInstance(processGuid, "instance-2", 1)
					publishKillIndex(processGuid, 1)
				})

				It("requests instances at the correct index are stopped", func() {
					Eventually(bbs.StopLRPInstances).Should(HaveLen(2))

					Ω(bbs.StopLRPInstances()).Should(ConsistOf(
						models.StopLRPInstance{
							ProcessGuid:  processGuid,
							InstanceGuid: "instance-1",
							Index:        1,
						},
						models.StopLRPInstance{
							ProcessGuid:  processGuid,
							InstanceGuid: "instance-2",
							Index:        1,
						},
					))
				})
			})

			Context("and a second nsync listener is started", func() {
				var (
					desiredLRPChanges <-chan models.DesiredLRPChange
					stopWatching      chan<- bool

					secondRunner  *ginkgomon.Runner
					secondProcess ifrit.Process
				)

				BeforeEach(func() {
					secondRunner = newNSyncRunner()
					secondRunner.StartCheck = ""

					secondProcess = ginkgomon.Invoke(secondRunner)

					changes, stop, _ := bbs.WatchForDesiredLRPChanges()

					desiredLRPChanges = changes
					stopWatching = stop
				})

				AfterEach(func() {
					close(stopWatching)
					ginkgomon.Interrupt(secondProcess)
				})

				Describe("the second listener", func() {
					It("does not become active", func() {
						Consistently(secondRunner.Buffer, 5*time.Second).ShouldNot(gbytes.Say("nsync.listener.started"))
					})
				})

				Context("and the first listener goes away", func() {
					BeforeEach(func() {
						ginkgomon.Interrupt(process)
					})

					Describe("the second listener", func() {
						It("eventually becomes active", func() {
							Eventually(secondRunner.Buffer, 5*time.Second).Should(gbytes.Say("nsync.listener.started"))
						})
					})
				})

				Context("and a 'diego.desire.app' message is received", func() {
					BeforeEach(func() {
						publishDesireWithInstances(3)
					})

					It("does not emit duplicate events", func() {
						Eventually(desiredLRPChanges).Should(Receive())
						Consistently(desiredLRPChanges).ShouldNot(Receive())
					})
				})
			})
		})
	})

	Describe("when NATS is not up", func() {
		Context("and the nsync listener is started", func() {

			BeforeEach(func() {
				process = ifrit.Background(runner)
			})

			AfterEach(func() {
				defer stopNATS()
				defer ginkgomon.Interrupt(process)
			})

			It("starts only after nats comes up", func() {
				Consistently(process.Ready()).ShouldNot(BeClosed())

				startNATS()
				Eventually(process.Ready(), 5*time.Second).Should(BeClosed())
			})
		})
	})
})
