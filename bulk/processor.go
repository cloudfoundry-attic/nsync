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
	"github.com/cloudfoundry-incubator/route-emitter/cfroutes"
	"github.com/cloudfoundry-incubator/runtime-schema/cc_messages"
	"github.com/cloudfoundry-incubator/runtime-schema/metric"
	"github.com/cloudfoundry/gunk/workpool"
	"github.com/pivotal-golang/clock"
	"github.com/pivotal-golang/lager"
)

const (
	syncDesiredLRPsDuration = metric.Duration("DesiredLRPSyncDuration")
)

type Processor struct {
	receptorClient  receptor.Client
	pollingInterval time.Duration
	domainTTL       time.Duration
	bulkBatchSize   uint
	skipCertVerify  bool
	logger          lager.Logger
	fetcher         Fetcher
	builders        map[string]recipebuilder.RecipeBuilder
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
	builders map[string]recipebuilder.RecipeBuilder,
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
		builders:        builders,
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
	logger.Info("starting")
	defer logger.Info("done")

	existing, err := p.getDesiredLRPs(logger)
	if err != nil {
		return false
	}

	existingLRPMap := organizeLRPsByProcessGuid(existing)
	differ := NewDiffer(existingLRPMap)

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
		logger.Session("fetch-missing-desired-lrps-from-cc"),
		cancel,
		httpClient,
		differ.Missing(),
	)

	createErrors := p.createMissingDesiredLRPs(logger, cancel, missingApps)

	staleApps, staleAppErrors := p.fetcher.FetchDesiredApps(
		logger.Session("fetch-stale-desired-lrps-from-cc"),
		cancel,
		httpClient,
		differ.Stale(),
	)

	updateErrors := p.updateStaleDesiredLRPs(logger, cancel, staleApps, existingLRPMap)

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

	logger.Info("processing-updates-and-creates")
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
		case sig := <-signals:
			logger.Info("exiting", lager.Data{"received-signal": sig})
			close(cancel)
			return true
		}
	}
	logger.Info("done-processing-updates-and-creates")

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

		err = p.receptorClient.UpsertDomain(cc_messages.AppLRPDomain, p.domainTTL)
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

			works := make([]func(), len(desireAppRequests))

			for i, desireAppRequest := range desireAppRequests {
				desireAppRequest := desireAppRequest
				var builder recipebuilder.RecipeBuilder = p.builders["buildpack"]
				if desireAppRequest.DockerImageUrl != "" {
					builder = p.builders["docker"]
				}

				works[i] = func() {
					logger.Debug("building-create-desired-lrp-request", desireAppRequestDebugData(&desireAppRequest))
					createReq, err := builder.Build(&desireAppRequest)
					if err != nil {
						logger.Error("failed-building-create-desired-lrp-request", err, lager.Data{"process-guid": desireAppRequest.ProcessGuid})
						errc <- err
						return
					}
					logger.Debug("succeeded-building-create-desired-lrp-request", desireAppRequestDebugData(&desireAppRequest))

					logger.Debug("creating-desired-lrp", createDesiredReqDebugData(createReq))
					err = p.receptorClient.CreateDesiredLRP(*createReq)
					if err != nil {
						logger.Error("failed-creating-desired-lrp", err, lager.Data{"process-guid": createReq.ProcessGuid})
						errc <- err
						return
					}
					logger.Debug("succeeded-creating-desired-lrp", createDesiredReqDebugData(createReq))
				}
			}

			throttler, err := workpool.NewThrottler(50, works)
			if err != nil {
				errc <- err
				return
			}

			logger.Info("processing-batch", lager.Data{"size": len(desireAppRequests)})
			throttler.Work()
			logger.Info("done-processing-batch", lager.Data{"size": len(desireAppRequests)})
		}
	}()

	return errc
}

func (p *Processor) updateStaleDesiredLRPs(
	logger lager.Logger,
	cancel <-chan struct{},
	stale <-chan []cc_messages.DesireAppRequestFromCC,
	existingLRPMap map[string]*receptor.DesiredLRPResponse,
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

			works := make([]func(), len(staleAppRequests))

			for i, desireAppRequest := range staleAppRequests {
				desireAppRequest := desireAppRequest
				var builder recipebuilder.RecipeBuilder = p.builders["buildpack"]
				if desireAppRequest.DockerImageUrl != "" {
					builder = p.builders["docker"]
				}

				works[i] = func() {
					processGuid := desireAppRequest.ProcessGuid
					existingLRP := existingLRPMap[desireAppRequest.ProcessGuid]

					updateReq := receptor.DesiredLRPUpdateRequest{}
					updateReq.Instances = &desireAppRequest.NumInstances
					updateReq.Annotation = &desireAppRequest.ETag

					exposedPort, err := builder.ExtractExposedPort(desireAppRequest.ExecutionMetadata)
					if err != nil {
						logger.Error("failed-updating-stale-lrp", err, lager.Data{
							"process-guid":       processGuid,
							"execution-metadata": desireAppRequest.ExecutionMetadata,
						})
						errc <- err
						return
					}

					updateReq.Routes = cfroutes.LegacyCFRoutes{
						{Hostnames: desireAppRequest.Routes, Port: exposedPort},
					}.LegacyRoutingInfo()

					for k, v := range existingLRP.Routes {
						if k != cfroutes.CF_ROUTER {
							updateReq.Routes[k] = v
						}
					}

					logger.Debug("updating-stale-lrp", updateDesiredRequestDebugData(processGuid, &updateReq))
					err = p.receptorClient.UpdateDesiredLRP(processGuid, updateReq)
					if err != nil {
						logger.Error("failed-updating-stale-lrp", err, lager.Data{
							"process-guid": processGuid,
						})
						errc <- err
						return
					}
					logger.Debug("succeeded-updating-stale-lrp", updateDesiredRequestDebugData(processGuid, &updateReq))
				}
			}

			throttler, err := workpool.NewThrottler(50, works)
			if err != nil {
				errc <- err
				return
			}

			logger.Info("processing-batch", lager.Data{"size": len(staleAppRequests)})
			throttler.Work()
			logger.Info("done-processing-batch", lager.Data{"size": len(staleAppRequests)})
		}
	}()

	return errc
}

func (p *Processor) getDesiredLRPs(logger lager.Logger) ([]receptor.DesiredLRPResponse, error) {
	logger.Info("getting-desired-lrps-from-bbs")
	existing, err := p.receptorClient.DesiredLRPsByDomain(cc_messages.AppLRPDomain)
	if err != nil {
		logger.Error("failed-getting-desired-lrps-from-bbs", err)
		return nil, err
	}
	logger.Info("succeeded-getting-desired-lrps-from-bbs", lager.Data{"count": len(existing)})

	return existing, nil
}

func (p *Processor) deleteExcess(logger lager.Logger, cancel <-chan struct{}, excess []string) {
	logger = logger.Session("delete-excess")

	logger.Info("processing-batch", lager.Data{"size": len(excess)})
	for _, deleteGuid := range excess {
		err := p.receptorClient.DeleteDesiredLRP(deleteGuid)
		if err != nil {
			logger.Error("failed-processing-batch", err, lager.Data{"delete-request": deleteGuid})
		}
	}
	logger.Info("succeeded-processing-batch")
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

func organizeLRPsByProcessGuid(list []receptor.DesiredLRPResponse) map[string]*receptor.DesiredLRPResponse {
	result := make(map[string]*receptor.DesiredLRPResponse)
	for _, l := range list {
		lrp := l
		result[lrp.ProcessGuid] = &lrp
	}

	return result
}

func updateDesiredRequestDebugData(processGuid string, updateDesiredRequest *receptor.DesiredLRPUpdateRequest) lager.Data {
	return lager.Data{
		"process-guid": processGuid,
		"instances":    updateDesiredRequest.Instances,
	}
}

func createDesiredReqDebugData(createDesiredRequest *receptor.DesiredLRPCreateRequest) lager.Data {
	return lager.Data{
		"process-guid": createDesiredRequest.ProcessGuid,
		"log-guid":     createDesiredRequest.LogGuid,
		"metric-guid":  createDesiredRequest.MetricsGuid,
		"root-fs":      createDesiredRequest.RootFS,
		"instances":    createDesiredRequest.Instances,
		"timeout":      createDesiredRequest.StartTimeout,
		"disk":         createDesiredRequest.DiskMB,
		"memory":       createDesiredRequest.MemoryMB,
		"cpu":          createDesiredRequest.CPUWeight,
		"privileged":   createDesiredRequest.Privileged,
	}
}

func desireAppRequestDebugData(desireAppRequest *cc_messages.DesireAppRequestFromCC) lager.Data {
	return lager.Data{
		"process-guid": desireAppRequest.ProcessGuid,
		"log-guid":     desireAppRequest.LogGuid,
		"stack":        desireAppRequest.Stack,
		"memory":       desireAppRequest.MemoryMB,
		"disk":         desireAppRequest.DiskMB,
		"file":         desireAppRequest.FileDescriptors,
		"instances":    desireAppRequest.NumInstances,
		"allow-ssh":    desireAppRequest.AllowSSH,
		"etag":         desireAppRequest.ETag,
	}
}
