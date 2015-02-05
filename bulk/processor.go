package bulk

import (
	"crypto/tls"
	"net"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/cloudfoundry-incubator/cf_http"
	"github.com/cloudfoundry-incubator/nsync/recipebuilder"
	"github.com/cloudfoundry-incubator/receptor"
	"github.com/cloudfoundry-incubator/runtime-schema/cc_messages"
	"github.com/cloudfoundry-incubator/runtime-schema/metric"
	"github.com/pivotal-golang/clock"
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
	domainTTL       time.Duration
	bulkBatchSize   uint
	skipCertVerify  bool
	logger          lager.Logger
	fetcher         Fetcher
	builder         RecipeBuilder
	clock           clock.Clock
}

func NewProcessor(
	receptorClient receptor.Client,
	pollingInterval time.Duration,
	domainTTL time.Duration,
	bulkBatchSize uint,
	skipCertVerify bool,
	logger lager.Logger,
	fetcher Fetcher,
	builder RecipeBuilder,
	clock clock.Clock,
) *Processor {
	return &Processor{
		receptorClient:  receptorClient,
		pollingInterval: pollingInterval,
		domainTTL:       domainTTL,
		bulkBatchSize:   bulkBatchSize,
		skipCertVerify:  skipCertVerify,
		logger:          logger,
		fetcher:         fetcher,
		builder:         builder,
		clock:           clock,
	}
}

func (p *Processor) Run(signals <-chan os.Signal, ready chan<- struct{}) error {
	close(ready)

	httpClient := cf_http.NewClient()
	httpClient.Transport = &http.Transport{
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
	}

	timer := p.clock.NewTimer(p.pollingInterval)
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
	start := p.clock.Now()
	defer func() {
		duration := p.clock.Now().Sub(start)
		syncDesiredLRPsDuration.Send(duration)
	}()

	logger := p.logger.Session("sync")

	existing, err := p.getDesiredLRPs(logger)
	if err != nil {
		return false
	}

	differ := NewDiffer(existing)

	cancel := make(chan struct{})

	fingerprints, fingerprintErrors := p.fetcher.FetchFingerprints(
		logger,
		cancel,
		httpClient,
	)

	diffErrors := differ.Diff(
		logger,
		cancel,
		fingerprints,
	)

	missingApps, missingAppsErrors := p.fetcher.FetchDesiredApps(
		logger,
		cancel,
		httpClient,
		differ.Missing(),
	)

	createErrors := p.createMissingDesiredLRPs(logger, cancel, missingApps)

	staleApps, staleAppErrors := p.fetcher.FetchDesiredApps(
		logger,
		cancel,
		httpClient,
		differ.Stale(),
	)

	updateErrors := p.updateStaleDesiredLRPs(logger, cancel, staleApps)

	bumpFreshness := true
	success := true

	fingerprintErrors, fingerprintErrorCount := countErrors(fingerprintErrors)

	errors := mergeErrors(
		fingerprintErrors,
		diffErrors,
		missingAppsErrors,
		staleAppErrors,
		createErrors,
		updateErrors,
	)

process_loop:
	for {
		select {
		case err, open := <-errors:
			if err != nil {
				logger.Error("not-bumping-freshness-because-of", err)
				bumpFreshness = false
			}
			if !open {
				break process_loop
			}
		case <-signals:
			close(cancel)
			return true
		}
	}

	if <-fingerprintErrorCount != 0 {
		logger.Error("failed-to-fetch-all-cc-fingerprints", nil)
		success = false
	}

	if success {
		deleteList := <-differ.Deleted()
		p.deleteExcess(logger, cancel, deleteList)
	}

	if bumpFreshness && success {
		logger.Info("bumping-freshness")

		err = p.receptorClient.UpsertDomain(recipebuilder.LRPDomain, p.domainTTL)
		if err != nil {
			logger.Error("failed-to-upsert-domain", err)
		}
	}

	return false
}

func (p *Processor) createMissingDesiredLRPs(
	logger lager.Logger,
	cancel <-chan struct{},
	missing <-chan []cc_messages.DesireAppRequestFromCC,
) <-chan error {
	logger = logger.Session("create-missing-desired-lrps")

	errc := make(chan error, 1)

	go func() {
		defer close(errc)

		for {
			var desireAppRequests []cc_messages.DesireAppRequestFromCC

			select {
			case <-cancel:
				return

			case selected, open := <-missing:
				if !open {
					return
				}

				desireAppRequests = selected
			}

			logger.Info("processing-batch", lager.Data{"size": len(desireAppRequests)})

			for _, desireAppRequest := range desireAppRequests {
				createReq, err := p.builder.Build(&desireAppRequest)
				if err != nil {
					logger.Error("failed-to-build-create-desired-lrp-request", err, lager.Data{
						"desire-app-request": desireAppRequest,
					})
					errc <- err
					continue
				}

				err = p.receptorClient.CreateDesiredLRP(*createReq)
				if err != nil {
					logger.Error("failed-to-create-desired-lrp", err, lager.Data{
						"create-request": createReq,
					})
					errc <- err
					continue
				}
			}
		}
	}()

	return errc
}

func (p *Processor) updateStaleDesiredLRPs(
	logger lager.Logger,
	cancel <-chan struct{},
	stale <-chan []cc_messages.DesireAppRequestFromCC,
) <-chan error {
	logger = logger.Session("update-stale-desired-lrps")

	errc := make(chan error, 1)

	go func() {
		defer close(errc)

		for {
			var staleAppRequests []cc_messages.DesireAppRequestFromCC

			select {
			case <-cancel:
				return

			case selected, open := <-stale:
				if !open {
					return
				}

				staleAppRequests = selected
			}

			logger.Info("processing-batch", lager.Data{"size": len(staleAppRequests)})

			for _, desireAppRequest := range staleAppRequests {
				updateReq := receptor.DesiredLRPUpdateRequest{}
				updateReq.Instances = &desireAppRequest.NumInstances
				updateReq.Annotation = &desireAppRequest.ETag
				updateReq.Routes = receptor.CFRoutes{
					{Hostnames: desireAppRequest.Hostnames, Port: recipebuilder.DefaultPort},
				}.RoutingInfo()

				err := p.receptorClient.UpdateDesiredLRP(desireAppRequest.ProcessGuid, updateReq)
				if err != nil {
					logger.Error("failed-to-update-stale-lrp", err, lager.Data{
						"update-request": updateReq,
					})
					errc <- err
					continue
				}
			}
		}
	}()

	return errc
}

func (p *Processor) getDesiredLRPs(logger lager.Logger) ([]receptor.DesiredLRPResponse, error) {
	logger.Info("getting-desired-lrps-from-bbs")

	existing, err := p.receptorClient.DesiredLRPsByDomain(recipebuilder.LRPDomain)
	if err != nil {
		logger.Error("failed-to-get-desired-lrps", err)
		return nil, err
	}

	logger.Info("got-desired-lrps-from-bbs", lager.Data{"count": len(existing)})
	return existing, nil
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

func countErrors(source <-chan error) (<-chan error, <-chan int) {
	count := make(chan int, 1)
	dest := make(chan error, 1)
	var errorCount int

	wg := sync.WaitGroup{}
	wg.Add(1)

	go func() {
		for e := range source {
			errorCount++
			dest <- e
		}

		close(dest)
		wg.Done()
	}()

	go func() {
		wg.Wait()

		count <- errorCount
		close(count)
	}()

	return dest, count
}

func mergeErrors(channels ...<-chan error) <-chan error {
	out := make(chan error)
	wg := sync.WaitGroup{}

	for _, ch := range channels {
		wg.Add(1)

		go func(c <-chan error) {
			for e := range c {
				out <- e
			}
			wg.Done()
		}(ch)
	}

	go func() {
		wg.Wait()
		close(out)
	}()

	return out
}
