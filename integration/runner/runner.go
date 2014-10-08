package runner

import (
	"os/exec"
	"time"

	"github.com/tedsuo/ifrit"
	"github.com/tedsuo/ifrit/ginkgomon"
)

func NewRunner(startedMessage string, bin string, argv ...string) ifrit.Runner {
	return ginkgomon.New(ginkgomon.Config{
		Name:              bin,
		AnsiColorCode:     "97m",
		StartCheck:        startedMessage,
		StartCheckTimeout: 5 * time.Second,
		Command:           exec.Command(bin, argv...),
	})
}
