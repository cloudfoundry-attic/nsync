package bulk

import (
	"github.com/cloudfoundry-incubator/receptor"
	"github.com/cloudfoundry-incubator/runtime-schema/cc_messages"
	"github.com/pivotal-golang/lager"
)

//go:generate counterfeiter -o fakes/fake_differ.go . Differ
type Differ interface {
	Diff(
		logger lager.Logger,
		cancel <-chan struct{},
		existing []receptor.DesiredLRPResponse,
		desiredFingerprints <-chan []cc_messages.CCDesiredAppFingerprint,
		missingFingerprints chan<- []cc_messages.CCDesiredAppFingerprint,
	) []string
}

type differ struct{}

func NewDiffer() Differ {
	return &differ{}
}

func (d *differ) Diff(
	logger lager.Logger,
	cancel <-chan struct{},
	existing []receptor.DesiredLRPResponse,
	desiredFingerprints <-chan []cc_messages.CCDesiredAppFingerprint,
	missingFingerprints chan<- []cc_messages.CCDesiredAppFingerprint,
) []string {
	logger = logger.Session("diff")
	logger.Info("starting")
	defer logger.Info("finished")

	defer close(missingFingerprints)

	existingLRPs := organizeLRPsByProcessGuid(existing)

LOOP:
	for {
		select {
		case <-cancel:
			return []string{}

		case desired, ok := <-desiredFingerprints:
			if !ok {
				break LOOP
			}

			missing := []cc_messages.CCDesiredAppFingerprint{}

			for _, fingerprint := range desired {
				if desiredLRP, ok := existingLRPs[fingerprint.ProcessGuid]; ok {
					delete(existingLRPs, fingerprint.ProcessGuid)
					if desiredLRP.Annotation == fingerprint.ETag {
						continue
					}
				}

				logger.Info("found-missing-or-stale-desired-lrp", lager.Data{
					"guid": fingerprint.ProcessGuid,
					"etag": fingerprint.ETag,
				})

				missing = append(missing, fingerprint)
			}

			select {
			case missingFingerprints <- missing:
			case <-cancel:
				return []string{}
			}
		}
	}

	deleteList := make([]string, 0, len(existingLRPs))
	for _, lrp := range existingLRPs {
		deleteList = append(deleteList, lrp.ProcessGuid)
	}

	return deleteList
}

func organizeLRPsByProcessGuid(list []receptor.DesiredLRPResponse) map[string]*receptor.DesiredLRPResponse {
	result := make(map[string]*receptor.DesiredLRPResponse)
	for _, l := range list {
		lrp := l
		result[lrp.ProcessGuid] = &lrp
	}

	return result
}
