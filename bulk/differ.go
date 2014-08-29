package bulk

import (
	"reflect"

	"github.com/cloudfoundry-incubator/runtime-schema/cc_messages"
	"github.com/cloudfoundry-incubator/runtime-schema/models"
	"github.com/pivotal-golang/lager"
)

type Differ interface {
	Diff(existing []models.DesiredLRP, newChan <-chan cc_messages.DesireAppRequestFromCC) []models.DesiredLRPChange
}

type RecipeBuilder interface {
	Build(cc_messages.DesireAppRequestFromCC) (models.DesiredLRP, error)
}

type differ struct {
	builder RecipeBuilder
	logger  lager.Logger
}

func NewDiffer(builder RecipeBuilder, logger lager.Logger) Differ {
	return &differ{
		builder: builder,
		logger:  logger,
	}
}

func (d *differ) Diff(existing []models.DesiredLRP, newChan <-chan cc_messages.DesireAppRequestFromCC) []models.DesiredLRPChange {
	existingLRPs := organizeLRPsByProcessGuid(existing)

	diffLog := d.logger.Session("diff")

	changes := []models.DesiredLRPChange{}

	for fromCC := range newChan {
		change := d.changeForDesiredLRP(diffLog, existingLRPs, fromCC)

		delete(existingLRPs, fromCC.ProcessGuid)

		if change != nil {
			changes = append(changes, *change)
		}
	}

	for _, lrp := range existingLRPs {
		deletedLRP := lrp

		diffLog.Info("found-extra-desired-lrp", lager.Data{
			"guid": lrp.ProcessGuid,
		})

		changes = append(changes, models.DesiredLRPChange{Before: &deletedLRP})
	}

	return changes
}

func (d *differ) changeForDesiredLRP(logger lager.Logger, existing map[string]models.DesiredLRP, fromCC cc_messages.DesireAppRequestFromCC) *models.DesiredLRPChange {
	var beforeLRP *models.DesiredLRP

	if existingLRP, ok := existing[fromCC.ProcessGuid]; ok {
		if isInSync(existingLRP, fromCC) {
			return nil
		} else {
			beforeLRP = &existingLRP
		}
	}

	desiredLRP, err := d.builder.Build(fromCC)
	if err != nil {
		logger.Error("failed-to-build-recipe", err, lager.Data{
			"desired-by-cc": fromCC,
		})

		return nil
	}

	if beforeLRP == nil {
		logger.Info("found-missing-desired-lrp", lager.Data{
			"guid": fromCC.ProcessGuid,
		})
	} else {
		logger.Info("found-out-of-sync-desired-lrp", lager.Data{
			"guid": fromCC.ProcessGuid,
		})
	}

	return &models.DesiredLRPChange{
		Before: beforeLRP,
		After:  &desiredLRP,
	}
}

func isInSync(existingLRP models.DesiredLRP, fromCC cc_messages.DesireAppRequestFromCC) bool {
	routesA := map[string]bool{}
	routesB := map[string]bool{}

	for _, r := range existingLRP.Routes {
		routesA[r] = true
	}

	for _, r := range existingLRP.Routes {
		routesB[r] = true
	}

	// we only really keep the # instances and total routes in sync with the desired state.
	// everything else should result in the app version changing, and thus a whole new process guid.
	return fromCC.NumInstances == existingLRP.Instances &&
		reflect.DeepEqual(routesA, routesB)
}

func organizeLRPsByProcessGuid(list []models.DesiredLRP) map[string]models.DesiredLRP {
	result := make(map[string]models.DesiredLRP)
	for _, lrp := range list {
		result[lrp.ProcessGuid] = lrp
	}
	return result
}
