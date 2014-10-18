package main_test

import (
	"testing"

	"github.com/cloudfoundry/storeadapter"
	"github.com/cloudfoundry/storeadapter/storerunner/etcdstorerunner"
	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
	"github.com/onsi/gomega/gexec"
)

var bulkerPath string

var etcdRunner *etcdstorerunner.ETCDClusterRunner
var etcdClient storeadapter.StoreAdapter

func TestBulker(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Bulker Suite")
}

var _ = SynchronizedBeforeSuite(func() []byte {
	bulker, err := gexec.Build("github.com/cloudfoundry-incubator/nsync/cmd/nsync-bulker", "-race")
	Ω(err).ShouldNot(HaveOccurred())
	return []byte(bulker)
}, func(bulker []byte) {
	etcdPort := 5001 + GinkgoParallelNode()
	etcdRunner = etcdstorerunner.NewETCDClusterRunner(etcdPort, 1)
	bulkerPath = string(bulker)
	etcdClient = etcdRunner.Adapter()
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
