package main_test

import (
	"fmt"
	"os"
	"strings"
	"time"

	Bbs "github.com/cloudfoundry-incubator/runtime-schema/bbs"
	"github.com/cloudfoundry-incubator/runtime-schema/models"
	"github.com/cloudfoundry/gunk/timeprovider"
	"github.com/cloudfoundry/yagnats"
	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
	"github.com/onsi/gomega/gbytes"
	"github.com/pivotal-golang/lager/lagertest"
	"github.com/tedsuo/ifrit"
	"github.com/tedsuo/ifrit/ginkgomon"

	"github.com/cloudfoundry-incubator/nsync/testrunner"
)

var _ = Describe("Syncing desired state with CC", func() {
	var (
		natsClient yagnats.NATSConn
		bbs        *Bbs.BBS

		runner  ifrit.Runner
		process ifrit.Process
	)

	newNSyncRunner := func() *ginkgomon.Runner {
		return testrunner.NewRunner(
			"nsync.listener.started",
			listenerPath,
			"-etcdCluster", strings.Join(etcdRunner.NodeURLS(), ","),
			"-natsAddresses", fmt.Sprintf("127.0.0.1:%d", natsPort),
			"-circuses", `{"some-stack": "some-health-check.tar.gz"}`,
			"-dockerCircusPath", "the/docker/circus/path.tgz",
			"-heartbeatInterval", "1s",
		)
	}

	BeforeEach(func() {
		bbs = Bbs.NewBBS(etcdRunner.Adapter(), timeprovider.NewTimeProvider(), lagertest.NewTestLogger("test"))
		runner = newNSyncRunner()
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

	Context("when NATS is up", func() {
		BeforeEach(func() {
			natsRunner.Start()

			natsClient = natsRunner.MessageBus
		})

		AfterEach(func() {
			natsRunner.Stop()
		})

		Context("and the nsync listener is started", func() {
			BeforeEach(func() {
				process = ginkgomon.Invoke(runner)
			})

			AfterEach(func() {
				process.Signal(os.Interrupt)
				Eventually(process.Wait(), 5).Should(Receive())
			})

			Describe("and a 'diego.desire.app' message is recieved", func() {
				BeforeEach(func() {
					publishDesireWithInstances(3)
				})

				It("registers an app desire in etcd", func() {
					Eventually(bbs.GetAllDesiredLRPs, 10).Should(HaveLen(1))
				})

				Context("when an app is no longer desired", func() {
					BeforeEach(func() {
						Eventually(bbs.GetAllDesiredLRPs).Should(HaveLen(1))

						publishDesireWithInstances(0)
					})

					It("should remove the desired state from etcd", func() {
						Eventually(bbs.GetAllDesiredLRPs).Should(HaveLen(0))
					})
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
			var processCh chan ifrit.Process

			BeforeEach(func() {
				processCh = make(chan ifrit.Process, 1)

				go func() {
					processCh <- ginkgomon.Invoke(runner)
				}()
			})

			AfterEach(func() {
				natsRunner.Stop()
			})

			It("starts only after nats comes up", func() {
				Consistently(processCh).ShouldNot(Receive())

				natsRunner.Start()

				var process ifrit.Process
				Eventually(processCh, 5*time.Second).Should(Receive(&process))

				process.Signal(os.Interrupt)
				Eventually(process.Wait(), 5).Should(Receive())
			})
		})
	})
})
