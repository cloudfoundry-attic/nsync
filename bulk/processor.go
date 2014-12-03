package bulk

import (
	"crypto/tls"
	"net"
	"net/http"
	"os"
	"time"

	"github.com/cloudfoundry-incubator/nsync/recipebuilder"
	"github.com/cloudfoundry-incubator/receptor"
	"github.com/cloudfoundry-incubator/runtime-schema/cc_messages"
	"github.com/cloudfoundry-incubator/runtime-schema/metric"
	"github.com/cloudfoundry/gunk/timeprovider"
	"github.com/pivotal-golang/lager"
)

const (
	syncDesiredLRPsDuration = metric.Duration("DesiredLRPSyncDuration")
)

type Processor struct {
	receptorClient  receptor.Client
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
	receptorClient receptor.Client,
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
		receptorClient:  receptorClient,
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
	start := p.timeProvider.Now()
	duration := time.Duration(0)
	defer func() {
		syncDesiredLRPsDuration.Send(duration)
	}()

	processLog := p.logger.Session("processor")

	processLog.Info("getting-desired-lrps-from-bbs")

	existing, err := p.receptorClient.DesiredLRPsByDomain(recipebuilder.LRPDomain)
	if err != nil {
		p.logger.Error("failed-to-get-desired-lrps", err)
		return false
	}

	fromCC := make(chan *cc_messages.DesireAppRequestFromCC)

	httpClient := &http.Client{
		Timeout: p.ccFetchTimeout,
		Transport: &http.Transport{
			Proxy: http.ProxyFromEnvironment,
			Dial: (&net.Dialer{
				Timeout:   10 * time.Second,
				KeepAlive: 30 * time.Second,
			}).Dial,
			TLSHandshakeTimeout: 10 * time.Second,
			TLSClientConfig: &tls.Config{
				InsecureSkipVerify: p.skipCertVerify,
				MinVersion:         tls.VersionTLS10,
			},
		},
	}

	processLog.Info("fetching-desired-from-cc")

	fetchErrs := make(chan error)

	go func() {
		fetchErrs <- p.fetcher.Fetch(fromCC, httpClient)
	}()

	lrpChan := make(chan *receptor.DesiredLRPCreateRequest)
	deleteListChan := make(chan []string)

	go p.differ.Diff(existing, fromCC, lrpChan, deleteListChan)

	deleteList := []string{}

	for deleteListChan != nil || lrpChan != nil {
		select {
		case deletes, ok := <-deleteListChan:
			if !ok {
				deleteListChan = nil
				break
			}

			deleteList = deletes

		case createReq, ok := <-lrpChan:
			if !ok {
				lrpChan = nil
				break
			}

			err := p.receptorClient.CreateDesiredLRP(*createReq)
			if err != nil {
				processLog.Error(
					"failed-to-create-desired-lrp",
					err,
					lager.Data{"create-request": createReq},
				)
			}

		case <-signals:
			// allow interruption while processing changes
			return true
		}
	}

	fetchErr := <-fetchErrs
	if fetchErr != nil {
		processLog.Error("failed-to-fetch", fetchErr)
		return false
	}

	for _, deleteGuid := range deleteList {
		select {
		case <-signals:
			return true
		default:
			err := p.receptorClient.DeleteDesiredLRP(deleteGuid)
			if err != nil {
				processLog.Error("failed-to-delete-desired-lrp", err, lager.Data{
					"delete-request": deleteGuid,
				})
			}
		}
	}

	freshness := receptor.FreshDomainBumpRequest{
		Domain:       recipebuilder.LRPDomain,
		TTLInSeconds: int(p.freshnessTTL.Seconds()),
	}

	err = p.receptorClient.BumpFreshDomain(freshness)
	if err != nil {
		processLog.Error("failed-to-bump-freshness", err)
	}

	duration = p.timeProvider.Now().Sub(start)
	return false
}
