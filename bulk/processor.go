package bulk

import (
	"crypto/tls"
	"net/http"
	"os"
	"time"

	"github.com/cloudfoundry-incubator/runtime-schema/bbs"
	"github.com/cloudfoundry-incubator/runtime-schema/models"
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
}

func NewProcessor(
	bbs bbs.NsyncBBS,
	pollingInterval time.Duration,
	ccFetchTimeout time.Duration,
	bulkBatchSize uint,
	skipCertVerify bool,
	logger lager.Logger,
	fetcher Fetcher) *Processor {
	return &Processor{
		bbs:             bbs,
		pollingInterval: pollingInterval,
		ccFetchTimeout:  ccFetchTimeout,
		bulkBatchSize:   bulkBatchSize,
		skipCertVerify:  skipCertVerify,
		logger:          logger,
		fetcher:         fetcher,
	}
}

func (p *Processor) Run(signals <-chan os.Signal, ready chan<- struct{}) error {
	close(ready)

	for {
		existing, err := p.bbs.GetAllDesiredLRPs()
		if err != nil {
			p.logger.Error("getting-desired-lrps-failed", err)
			select {
			case <-signals:
				return nil
			case <-time.After(p.pollingInterval):
				continue
			}
		}

		fromCC := make(chan models.DesiredLRP)

		httpClient := &http.Client{
			Timeout: p.ccFetchTimeout,
			Transport: &http.Transport{
				TLSClientConfig: &tls.Config{
					InsecureSkipVerify: p.skipCertVerify,
				},
			},
		}

		go p.fetcher.Fetch(fromCC, httpClient)

		changes := Diff(existing, fromCC)

	dance:
		for {
			select {
			case change, ok := <-changes:
				if !ok {
					changes = nil
					break
				}

				p.bbs.ChangeDesiredLRP(change)
			case <-signals:
				return nil
			case <-time.After(p.pollingInterval):
				break dance
			}
		}
	}

	return nil
}
