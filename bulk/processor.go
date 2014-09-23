package bulk

import (
	"crypto/tls"
	"net/http"
	"os"
	"time"

	"github.com/cloudfoundry-incubator/nsync/recipebuilder"
	"github.com/cloudfoundry-incubator/runtime-schema/bbs"
	"github.com/cloudfoundry-incubator/runtime-schema/cc_messages"
	"github.com/cloudfoundry-incubator/runtime-schema/metric"
	"github.com/cloudfoundry/gunk/timeprovider"
	"github.com/pivotal-golang/lager"
)

const (
	syncDesiredLRPsDuration = metric.Duration("DesiredLRPSyncDuration")
)

type Processor struct {
	bbs             bbs.NsyncBBS
	pollingInterval time.Duration
	ccFetchTimeout  time.Duration
	freshnessTTL    time.Duration
	bulkBatchSize   uint
	skipCertVerify  bool
	logger          lager.Logger
	fetcher         Fetcher
	differ          Differ
	timeProvider    timeprovider.TimeProvider
}

func NewProcessor(
	bbs bbs.NsyncBBS,
	pollingInterval time.Duration,
	ccFetchTimeout time.Duration,
	freshnessTTL time.Duration,
	bulkBatchSize uint,
	skipCertVerify bool,
	logger lager.Logger,
	fetcher Fetcher,
	differ Differ,
	timeProvider timeprovider.TimeProvider,
) *Processor {
	return &Processor{
		bbs:             bbs,
		pollingInterval: pollingInterval,
		ccFetchTimeout:  ccFetchTimeout,
		freshnessTTL:    freshnessTTL,
		bulkBatchSize:   bulkBatchSize,
		skipCertVerify:  skipCertVerify,
		logger:          logger,
		fetcher:         fetcher,
		differ:          differ,
		timeProvider:    timeProvider,
	}
}

func (p *Processor) Run(signals <-chan os.Signal, ready chan<- struct{}) error {
	close(ready)

	stop := p.sync(signals)

	for {
		if stop {
			return nil
		}
		select {
		case <-signals:
			return nil
		case <-time.After(p.pollingInterval):
			stop = p.sync(signals)
		}
	}
}

func (p *Processor) sync(signals <-chan os.Signal) bool {
	start := p.timeProvider.Time()
	duration := time.Duration(0)
	defer func() {
		syncDesiredLRPsDuration.Send(duration)
	}()

	processLog := p.logger.Session("processor")

	processLog.Info("getting-desired-lrps-from-bbs")

	existing, err := p.bbs.GetAllDesiredLRPsByDomain(recipebuilder.LRPDomain)
	if err != nil {
		p.logger.Error("failed-to-get-desired-lrps", err)
		return false
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

	fetchErrs := make(chan error)

	go func() {
		fetchErrs <- p.fetcher.Fetch(fromCC, httpClient)
	}()

	changes := p.differ.Diff(existing, fromCC)
	fetchErr := <-fetchErrs
	if fetchErr != nil {
		processLog.Error("failed-to-fetch", fetchErr)
		return false
	}

	for _, change := range changes {
		select {
		case <-signals:
			// allow interruption while processing changes
			return true
		default:
			p.bbs.ChangeDesiredLRP(change)
		}
	}

	err = p.bbs.BumpFreshness(recipebuilder.LRPDomain, p.freshnessTTL)
	if err != nil {
		processLog.Error("failed-to-bump-freshness", err)
	}

	duration = p.timeProvider.Time().Sub(start)
	return false
}
