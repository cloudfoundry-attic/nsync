package nsync_test

import (
	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"

	"testing"
)

func TestNsync(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Nsync Suite")
}
