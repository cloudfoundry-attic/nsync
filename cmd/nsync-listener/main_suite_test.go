package main_test

import (
	"encoding/json"
	"testing"

	"github.com/cloudfoundry-incubator/consuladapter"
	"github.com/cloudfoundry/storeadapter/storerunner/etcdstorerunner"
	. "github.com/onsi/ginkgo"
	"github.com/onsi/ginkgo/config"
	. "github.com/onsi/gomega"
	"github.com/onsi/gomega/gexec"
)

var (
	listenerPath string

	receptorPath string
	receptorPort int
)

var etcdRunner *etcdstorerunner.ETCDClusterRunner
var natsPort int

var consulRunner *consuladapter.ClusterRunner
var consulSession *consuladapter.Session

func TestListener(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Listener Suite")
}

var _ = SynchronizedBeforeSuite(func() []byte {
	listener, err := gexec.Build("github.com/cloudfoundry-incubator/nsync/cmd/nsync-listener", "-race")
	Ω(err).ShouldNot(HaveOccurred())

	receptor, err := gexec.Build("github.com/cloudfoundry-incubator/receptor/cmd/receptor", "-race")
	Ω(err).ShouldNot(HaveOccurred())

	payload, err := json.Marshal(map[string]string{
		"listener": listener,
		"receptor": receptor,
	})
	Ω(err).ShouldNot(HaveOccurred())

	return payload
}, func(payload []byte) {
	binaries := map[string]string{}

	err := json.Unmarshal(payload, &binaries)
	Ω(err).ShouldNot(HaveOccurred())

	listenerPath = string(binaries["listener"])
	receptorPath = string(binaries["receptor"])

	natsPort = 4001 + GinkgoParallelNode()
	etcdPort := 5001 + GinkgoParallelNode()
	receptorPort = 6001 + GinkgoParallelNode()*2

	etcdRunner = etcdstorerunner.NewETCDClusterRunner(etcdPort, 1)

	consulRunner = consuladapter.NewClusterRunner(
		9001+config.GinkgoConfig.ParallelNode*consuladapter.PortOffsetLength,
		1,
		"http",
	)

})

var _ = BeforeEach(func() {
	etcdRunner.Start()
	consulRunner.Start()
	consulRunner.WaitUntilReady()
	consulSession = consulRunner.NewSession("a-session")
})

var _ = AfterEach(func() {
	etcdRunner.Stop()
	consulRunner.Stop()
})

var _ = SynchronizedAfterSuite(func() {
}, func() {
	gexec.CleanupBuildArtifacts()
})
