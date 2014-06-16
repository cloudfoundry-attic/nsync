package listen_test

import (
	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"

	"testing"
)

func TestListen(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Listen Suite")
}
