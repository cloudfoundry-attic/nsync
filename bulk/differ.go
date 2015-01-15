package bulk

import (
	"github.com/cloudfoundry-incubator/receptor"
	"github.com/cloudfoundry-incubator/runtime-schema/cc_messages"
	"github.com/pivotal-golang/lager"
)

//go:generate counterfeiter -o fakes/fake_differ.go . Differ
type Differ interface {
	Diff(logger lager.Logger, cancel <-chan struct{}, fingerprints <-chan []cc_messages.CCDesiredAppFingerprint) <-chan error

	Stale() <-chan []cc_messages.CCDesiredAppFingerprint

	Missing() <-chan []cc_messages.CCDesiredAppFingerprint

	Deleted() <-chan []string
}

type differ struct {
	existing []receptor.DesiredLRPResponse

	stale   chan []cc_messages.CCDesiredAppFingerprint
	missing chan []cc_messages.CCDesiredAppFingerprint
	deleted chan []string
}

func NewDiffer(existing []receptor.DesiredLRPResponse) Differ {
	return &differ{
		existing: existing,

		stale:   make(chan []cc_messages.CCDesiredAppFingerprint, 1),
		missing: make(chan []cc_messages.CCDesiredAppFingerprint, 1),
		deleted: make(chan []string, 1),
	}
}

func (d *differ) Diff(
	logger lager.Logger,
	cancel <-chan struct{},
	fingerprints <-chan []cc_messages.CCDesiredAppFingerprint,
) <-chan error {
	logger = logger.Session("diff")

	errc := make(chan error, 1)

	go func() {
		defer func() {
			close(d.missing)
			close(d.stale)
			close(d.deleted)
			close(errc)
		}()

		existingLRPs := organizeLRPsByProcessGuid(d.existing)

		for {
			select {
			case <-cancel:
				return

			case batch, open := <-fingerprints:
				if !open {
					remaining := remainingProcessGuids(existingLRPs)
					if len(remaining) > 0 {
						d.deleted <- remaining
					}
					return
				}

				missing := []cc_messages.CCDesiredAppFingerprint{}
				stale := []cc_messages.CCDesiredAppFingerprint{}

				for _, fingerprint := range batch {
					desiredLRP, found := existingLRPs[fingerprint.ProcessGuid]
					if !found {
						logger.Info("found-missing-desired-lrp", lager.Data{
							"guid": fingerprint.ProcessGuid,
							"etag": fingerprint.ETag,
						})

						missing = append(missing, fingerprint)
						continue
					}

					delete(existingLRPs, fingerprint.ProcessGuid)

					if desiredLRP.Annotation != fingerprint.ETag {
						logger.Info("found-stale-lrp", lager.Data{
							"guid": fingerprint.ProcessGuid,
							"etag": fingerprint.ETag,
						})

						stale = append(stale, fingerprint)
					}
				}

				if len(missing) > 0 {
					select {
					case d.missing <- missing:
					case <-cancel:
						return
					}
				}

				if len(stale) > 0 {
					select {
					case d.stale <- stale:
					case <-cancel:
						return
					}
				}
			}
		}
	}()

	return errc
}

func organizeLRPsByProcessGuid(list []receptor.DesiredLRPResponse) map[string]*receptor.DesiredLRPResponse {
	result := make(map[string]*receptor.DesiredLRPResponse)
	for _, l := range list {
		lrp := l
		result[lrp.ProcessGuid] = &lrp
	}

	return result
}

func remainingProcessGuids(remaining map[string]*receptor.DesiredLRPResponse) []string {
	keys := make([]string, 0, len(remaining))
	for _, lrp := range remaining {
		keys = append(keys, lrp.ProcessGuid)
	}

	return keys
}

func (d *differ) Stale() <-chan []cc_messages.CCDesiredAppFingerprint {
	return d.stale
}

func (d *differ) Missing() <-chan []cc_messages.CCDesiredAppFingerprint {
	return d.missing
}

func (d *differ) Deleted() <-chan []string {
	return d.deleted
}
