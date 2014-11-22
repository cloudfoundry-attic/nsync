package main_test

import (
	"encoding/json"
	"testing"

	"github.com/cloudfoundry/storeadapter/storerunner/etcdstorerunner"
	. "github.com/onsi/ginkgo"
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
	receptorPort = 6001 + GinkgoParallelNode()

	etcdRunner = etcdstorerunner.NewETCDClusterRunner(etcdPort, 1)
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
