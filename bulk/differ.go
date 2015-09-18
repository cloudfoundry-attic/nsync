package bulk

import (
	"github.com/cloudfoundry-incubator/bbs/models"
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
	existingSchedulingInfos map[string]*models.DesiredLRPSchedulingInfo

	stale   chan []cc_messages.CCDesiredAppFingerprint
	missing chan []cc_messages.CCDesiredAppFingerprint
	deleted chan []string
}

func NewDiffer(existing map[string]*models.DesiredLRPSchedulingInfo) Differ {
	return &differ{
		existingSchedulingInfos: copySchedulingInfoMap(existing),

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

		for {
			select {
			case <-cancel:
				return

			case batch, open := <-fingerprints:
				if !open {
					remaining := remainingProcessGuids(d.existingSchedulingInfos)
					if len(remaining) > 0 {
						d.deleted <- remaining
					}
					return
				}

				missing := []cc_messages.CCDesiredAppFingerprint{}
				stale := []cc_messages.CCDesiredAppFingerprint{}

				for _, fingerprint := range batch {
					desiredLRP, found := d.existingSchedulingInfos[fingerprint.ProcessGuid]
					if !found {
						logger.Info("found-missing-desired-lrp", lager.Data{
							"guid": fingerprint.ProcessGuid,
							"etag": fingerprint.ETag,
						})

						missing = append(missing, fingerprint)
						continue
					}

					delete(d.existingSchedulingInfos, fingerprint.ProcessGuid)

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

func copySchedulingInfoMap(schedulingInfoMap map[string]*models.DesiredLRPSchedulingInfo) map[string]*models.DesiredLRPSchedulingInfo {
	clone := map[string]*models.DesiredLRPSchedulingInfo{}
	for k, v := range schedulingInfoMap {
		clone[k] = v
	}
	return clone
}

func remainingProcessGuids(remaining map[string]*models.DesiredLRPSchedulingInfo) []string {
	keys := make([]string, 0, len(remaining))
	for _, schedulingInfo := range remaining {
		keys = append(keys, schedulingInfo.ProcessGuid)
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
