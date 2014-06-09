package integration_test

import (
	"testing"

	"github.com/cloudfoundry-incubator/nsync/integration/nsync_runner"
	"github.com/cloudfoundry-incubator/runtime-schema/bbs/services_bbs"
	"github.com/cloudfoundry/gunk/natsrunner"
	"github.com/cloudfoundry/storeadapter/storerunner/etcdstorerunner"
	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
	"github.com/onsi/gomega/gexec"
)

var nsyncPath string
var etcdRunner *etcdstorerunner.ETCDClusterRunner
var natsRunner *natsrunner.NATSRunner
var natsPort int
var fileServerPresence services_bbs.Presence
var runner *nsync_runner.NsyncRunner

func TestNsyncMain(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Integration Suite")
}

var _ = BeforeSuite(func() {
	var err error
	nsyncPath, err = gexec.Build("github.com/cloudfoundry-incubator/nsync", "-race")
	Ω(err).ShouldNot(HaveOccurred())

	etcdPort := 5001 + GinkgoParallelNode()
	natsPort = 4001 + GinkgoParallelNode()

	etcdRunner = etcdstorerunner.NewETCDClusterRunner(etcdPort, 1)

	natsRunner = natsrunner.NewNATSRunner(natsPort)
})

var _ = BeforeEach(func() {
	etcdRunner.Start()
	natsRunner.Start()
})

var _ = AfterEach(func() {
	etcdRunner.Stop()
	natsRunner.Stop()
})

var _ = AfterSuite(func() {
	gexec.CleanupBuildArtifacts()
	if etcdRunner != nil {
		etcdRunner.Stop()
	}
	if natsRunner != nil {
		natsRunner.Stop()
	}
	if runner != nil {
		runner.KillWithFire()
	}
})
