package bulk

import (
	"crypto/tls"
	"net/http"
	"os"
	"time"

	"github.com/cloudfoundry-incubator/nsync/recipebuilder"
	"github.com/cloudfoundry-incubator/runtime-schema/bbs"
	"github.com/cloudfoundry-incubator/runtime-schema/cc_messages"
	"github.com/pivotal-golang/lager"
)

type Processor struct {
	bbs             bbs.NsyncBBS
	pollingInterval time.Duration
	ccFetchTimeout  time.Duration
	bulkBatchSize   uint
	skipCertVerify  bool
	logger          lager.Logger
	fetcher         Fetcher
	differ          Differ
}

func NewProcessor(
	bbs bbs.NsyncBBS,
	pollingInterval time.Duration,
	ccFetchTimeout time.Duration,
	bulkBatchSize uint,
	skipCertVerify bool,
	logger lager.Logger,
	fetcher Fetcher,
	differ Differ,
) *Processor {
	return &Processor{
		bbs:             bbs,
		pollingInterval: pollingInterval,
		ccFetchTimeout:  ccFetchTimeout,
		bulkBatchSize:   bulkBatchSize,
		skipCertVerify:  skipCertVerify,
		logger:          logger,
		fetcher:         fetcher,
		differ:          differ,
	}
}

func (p *Processor) Run(signals <-chan os.Signal, ready chan<- struct{}) error {
	close(ready)

	for {
		processLog := p.logger.Session("processor")

		processLog.Info("getting-desired-lrps-from-bbs")

		existing, err := p.bbs.GetAllDesiredLRPsByDomain(recipebuilder.LRPDomain)
		if err != nil {
			p.logger.Error("failed-to-get-desired-lrps", err)
			select {
			case <-signals:
				return nil
			case <-time.After(p.pollingInterval):
				continue
			}
		}

		fromCC := make(chan cc_messages.DesireAppRequestFromCC)

		httpClient := &http.Client{
			Timeout: p.ccFetchTimeout,
			Transport: &http.Transport{
				TLSClientConfig: &tls.Config{
					InsecureSkipVerify: p.skipCertVerify,
				},
			},
		}

		processLog.Info("fetching-desired-from-cc")

		go p.fetcher.Fetch(fromCC, httpClient)

		changes := p.differ.Diff(existing, fromCC)

	dance:
		for {
			select {
			case change, ok := <-changes:
				if !ok {
					changes = nil
					break dance
				}

				p.bbs.ChangeDesiredLRP(change)
			case <-signals:
				return nil
			}
		}

		select {
		case <-signals:
			return nil
		case <-time.After(p.pollingInterval):
		}
	}

	panic("unreachable")
}
