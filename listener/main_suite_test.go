package main_test

import (
	"testing"

	"github.com/cloudfoundry/gunk/diegonats"
	"github.com/cloudfoundry/storeadapter/storerunner/etcdstorerunner"
	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
	"github.com/onsi/gomega/gexec"
)

var listenerPath string

var etcdRunner *etcdstorerunner.ETCDClusterRunner
var natsRunner *diegonats.NATSRunner
var natsPort int

func TestListener(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Listener Suite")
}

var _ = SynchronizedBeforeSuite(func() []byte {
	listener, err := gexec.Build("github.com/cloudfoundry-incubator/nsync/listener", "-race")
	Ω(err).ShouldNot(HaveOccurred())
	return []byte(listener)
}, func(listener []byte) {
	listenerPath = string(listener)

	etcdPort := 5001 + GinkgoParallelNode()
	natsPort = 4001 + GinkgoParallelNode()

	etcdRunner = etcdstorerunner.NewETCDClusterRunner(etcdPort, 1)
	natsRunner = diegonats.NewRunner(natsPort)
})

var _ = BeforeEach(func() {
	etcdRunner.Start()
})

var _ = AfterEach(func() {
	etcdRunner.Stop()
})

var _ = SynchronizedAfterSuite(func() {
	etcdRunner.Stop()
}, func() {
	gexec.CleanupBuildArtifacts()
})
