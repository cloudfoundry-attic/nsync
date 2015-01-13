package bulk

import (
	"crypto/tls"
	"net"
	"net/http"
	"os"
	"sync"
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

//go:generate counterfeiter -o fakes/fake_recipe_builder.go . RecipeBuilder
type RecipeBuilder interface {
	Build(*cc_messages.DesireAppRequestFromCC) (*receptor.DesiredLRPCreateRequest, error)
}

type Processor struct {
	receptorClient  receptor.Client
	pollingInterval time.Duration
	ccFetchTimeout  time.Duration
	domainTTL       time.Duration
	bulkBatchSize   uint
	skipCertVerify  bool
	logger          lager.Logger
	fetcher         Fetcher
	differ          Differ
	builder         RecipeBuilder
	timeProvider    timeprovider.TimeProvider
}

func NewProcessor(
	receptorClient receptor.Client,
	pollingInterval time.Duration,
	ccFetchTimeout time.Duration,
	domainTTL time.Duration,
	bulkBatchSize uint,
	skipCertVerify bool,
	logger lager.Logger,
	fetcher Fetcher,
	differ Differ,
	builder RecipeBuilder,
	timeProvider timeprovider.TimeProvider,
) *Processor {
	return &Processor{
		receptorClient:  receptorClient,
		pollingInterval: pollingInterval,
		ccFetchTimeout:  ccFetchTimeout,
		domainTTL:       domainTTL,
		bulkBatchSize:   bulkBatchSize,
		skipCertVerify:  skipCertVerify,
		logger:          logger,
		fetcher:         fetcher,
		differ:          differ,
		builder:         builder,
		timeProvider:    timeProvider,
	}
}

func (p *Processor) Run(signals <-chan os.Signal, ready chan<- struct{}) error {
	close(ready)

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

	timer := p.timeProvider.NewTimer(p.pollingInterval)
	stop := p.sync(signals, httpClient)

	for {
		if stop {
			return nil
		}

		select {
		case <-signals:
			return nil
		case <-timer.C():
			stop = p.sync(signals, httpClient)
			timer.Reset(p.pollingInterval)
		}
	}
}

func (p *Processor) sync(signals <-chan os.Signal, httpClient *http.Client) bool {
	start := p.timeProvider.Now()
	duration := time.Duration(0)
	defer func() {
		syncDesiredLRPsDuration.Send(duration)
	}()

	logger := p.logger.Session("processor")

	logger.Info("getting-desired-lrps-from-bbs")
	existing, err := p.receptorClient.DesiredLRPsByDomain(recipebuilder.LRPDomain)
	if err != nil {
		p.logger.Error("failed-to-get-desired-lrps", err)
		return false
	}

	fetchError := make(chan error, 1)
	done := make(chan error)

	fingerprintsFromCC := make(chan []cc_messages.CCDesiredAppFingerprint)
	missingFingerprints := make(chan []cc_messages.CCDesiredAppFingerprint)
	desireAppRequestsFromCC := make(chan []cc_messages.DesireAppRequestFromCC)

	wg := &sync.WaitGroup{}
	cancel := make(chan struct{})
	notFresh := make(chan struct{})

	wg.Add(1)
	go func() {
		defer close(fetchError)
		defer wg.Done()

		err := p.fetcher.FetchFingerprints(logger, cancel, fingerprintsFromCC, httpClient)
		if err != nil {
			notFresh <- struct{}{}
			fetchError <- err
		}
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()

		deleteList := p.differ.Diff(logger, cancel, existing, fingerprintsFromCC, missingFingerprints)

		select {
		case err, ok := <-fetchError:
			if err != nil {
				return
			}
			if !ok {
				p.deleteExcess(logger, cancel, deleteList)
			}
		}
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		err := p.fetcher.FetchDesiredApps(logger, cancel, missingFingerprints, desireAppRequestsFromCC, httpClient)
		if err != nil {
			notFresh <- struct{}{}
		}
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		err := p.createMissingDesiredLRPs(logger, cancel, desireAppRequestsFromCC)
		if err != nil {
			notFresh <- struct{}{}
		}
	}()

	go func() {
		wg.Wait()
		close(done)
	}()

	bumpFreshness := true

PROCESS:
	for {
		select {
		case _, ok := <-done:
			if !ok {
				break PROCESS
			}

		case <-notFresh:
			bumpFreshness = false

		case <-signals:
			close(cancel)
			signals = nil
			return true
		}
	}

	if bumpFreshness {
		err = p.receptorClient.UpsertDomain(recipebuilder.LRPDomain, p.domainTTL)
		if err != nil {
			logger.Error("failed-to-upsert-domain", err)
		}
	}

	duration = p.timeProvider.Now().Sub(start)
	return signals == nil
}

func (p *Processor) createMissingDesiredLRPs(
	logger lager.Logger,
	cancel <-chan struct{},
	missing <-chan []cc_messages.DesireAppRequestFromCC,
) error {
	var createError error

CREATE_LOOP:
	for {
		logger = logger.Session("create-missing-desired-lrps")

		var desireAppRequests []cc_messages.DesireAppRequestFromCC
		select {
		case <-cancel:
			break CREATE_LOOP
		case selected, ok := <-missing:
			if !ok {
				break CREATE_LOOP
			}
			desireAppRequests = selected
		}

		logger.Info(
			"processing-batch",
			lager.Data{"size": len(desireAppRequests)},
		)

		for _, desireAppRequest := range desireAppRequests {
			createReq, err := p.builder.Build(&desireAppRequest)
			if err != nil {
				createError = err
				logger.Error(
					"failed-to-build-create-desired-lrp-request",
					err,
					lager.Data{"desire-app-request": desireAppRequest},
				)
				continue
			}

			err = p.receptorClient.CreateDesiredLRP(*createReq)
			if err != nil {
				createError = err
				logger.Error(
					"failed-to-create-desired-lrp",
					err,
					lager.Data{"create-request": createReq},
				)
			}
		}
	}

	return createError
}

func (p *Processor) deleteExcess(logger lager.Logger, cancel <-chan struct{}, excess []string) {
	logger = logger.Session("delete-excess")

	logger.Info(
		"processing-batch",
		lager.Data{"size": len(excess)},
	)

	for _, deleteGuid := range excess {
		err := p.receptorClient.DeleteDesiredLRP(deleteGuid)
		if err != nil {
			logger.Error(
				"failed-to-delete-desired-lrp",
				err,
				lager.Data{"delete-request": deleteGuid},
			)
		}
	}
}
