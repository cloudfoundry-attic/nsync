package config_test

import (
	"time"

	. "code.cloudfoundry.org/nsync/config"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
)

var _ = Describe("Config", func() {
	It("generates a config with the default values", func() {
		bulkerConfig, err := New("../fixtures/empty_bulker_config.json")
		Expect(err).ToNot(HaveOccurred())

		Expect(bulkerConfig.BBSCancelTaskPoolSize).To(Equal(50))
		Expect(bulkerConfig.BBSClientSessionCacheSize).To(Equal(0))
		Expect(bulkerConfig.BBSFailTaskPoolSize).To(Equal(50))
		Expect(bulkerConfig.BBSMaxIdleConnsPerHost).To(Equal(0))
		Expect(bulkerConfig.BBSUpdateLRPWorkers).To(Equal(50))
		Expect(bulkerConfig.CCBulkBatchSize).To(Equal(uint(500)))
		Expect(bulkerConfig.CCPollingInterval).To(Equal(30 * time.Second))
		Expect(bulkerConfig.CCTimeout).To(Equal(30 * time.Second))
		Expect(bulkerConfig.DomainTTL).To(Equal(2 * time.Minute))
		Expect(bulkerConfig.DropsondePort).To(Equal(3457))
		Expect(bulkerConfig.PrivilegedContainers).To(Equal(false))
		Expect(bulkerConfig.SkipCertVerify).To(Equal(false))
	})

	It("reads from the config file and populates the config", func() {
		bulkerConfig, err := New("../fixtures/bulker_config.json")
		Expect(err).ToNot(HaveOccurred())

		Expect(bulkerConfig.BBSAddress).To(Equal("https://foobar.com"))
		Expect(bulkerConfig.BBSCancelTaskPoolSize).To(Equal(1234))
		Expect(bulkerConfig.CCBulkBatchSize).To(Equal(uint(117)))
		Expect(bulkerConfig.CCPollingInterval).To(Equal(120 * time.Second))
		Expect(bulkerConfig.SkipCertVerify).To(BeTrue())
	})
})
