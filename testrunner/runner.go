package testrunner

import (
	"os/exec"
	"time"

	"github.com/tedsuo/ifrit/ginkgomon"
)

func NewRunner(startedMessage string, bin string, argv ...string) *ginkgomon.Runner {
	return ginkgomon.New(ginkgomon.Config{
		Name:              bin,
		AnsiColorCode:     "97m",
		StartCheck:        startedMessage,
		StartCheckTimeout: 5 * time.Second,
		Command:           exec.Command(bin, argv...),
	})
}
