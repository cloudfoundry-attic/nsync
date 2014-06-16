package main_test

import (
	"testing"

	"github.com/cloudfoundry/storeadapter/storerunner/etcdstorerunner"
	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
	"github.com/onsi/gomega/gexec"
)

var bulkerPath string

var etcdRunner *etcdstorerunner.ETCDClusterRunner

func TestBulker(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Bulker Suite")
}

var _ = BeforeSuite(func() {
	var err error

	bulkerPath, err = gexec.Build("github.com/cloudfoundry-incubator/nsync/bulker", "-race")
	Ω(err).ShouldNot(HaveOccurred())

	etcdPort := 5001 + GinkgoParallelNode()

	etcdRunner = etcdstorerunner.NewETCDClusterRunner(etcdPort, 1)
})

var _ = BeforeEach(func() {
	etcdRunner.Start()
})

var _ = AfterEach(func() {
	etcdRunner.Stop()
})

var _ = AfterSuite(func() {
	gexec.CleanupBuildArtifacts()
})
