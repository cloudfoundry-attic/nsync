package runner

import (
	"os/exec"

	"github.com/tedsuo/ifrit"
	"github.com/tedsuo/ifrit/ginkgomon"
)

func NewRunner(startedMessage string, bin string, argv ...string) ifrit.Runner {
	return &ginkgomon.Runner{
		Command:       exec.Command(bin, argv...),
		StartCheck:    startedMessage,
		AnsiColorCode: "35m",
	}
}
