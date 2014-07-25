package bulk

import (
	"reflect"

	"github.com/cloudfoundry-incubator/runtime-schema/models"
)

type Differ interface {
	Diff(existing []models.DesiredLRP, newChan <-chan models.DesireAppRequestFromCC) <-chan models.DesiredLRPChange
}

type RecipeBuilder interface {
	Build(models.DesireAppRequestFromCC) (models.DesiredLRP, error)
}

type differ struct {
	builder RecipeBuilder
}

func NewDiffer(builder RecipeBuilder) Differ {
	return &differ{
		builder: builder,
	}
}

func (d *differ) Diff(existing []models.DesiredLRP, newChan <-chan models.DesireAppRequestFromCC) <-chan models.DesiredLRPChange {
	changeChan := make(chan models.DesiredLRPChange)
	existingLRPs := organizeLRPsByProcessGuid(existing)

	go func() {
		for fromCC := range newChan {
			desiredLRP, err := d.builder.Build(fromCC)
			if err != nil {
				continue
			}

			change := changeForDesiredLRP(existingLRPs, desiredLRP)
			delete(existingLRPs, desiredLRP.ProcessGuid)

			if change != nil {
				changeChan <- *change
			}
		}

		for _, lrp := range existingLRPs {
			changeChan <- models.DesiredLRPChange{Before: &lrp}
		}

		close(changeChan)
	}()

	return changeChan
}

func changeForDesiredLRP(existing map[string]models.DesiredLRP, desired models.DesiredLRP) *models.DesiredLRPChange {
	if existingLRP, ok := existing[desired.ProcessGuid]; ok {
		if reflect.DeepEqual(existingLRP, desired) {
			return nil
		} else {
			return &models.DesiredLRPChange{Before: &existingLRP, After: &desired}
		}
	} else {
		return &models.DesiredLRPChange{After: &desired}
	}
}

func organizeLRPsByProcessGuid(list []models.DesiredLRP) map[string]models.DesiredLRP {
	result := make(map[string]models.DesiredLRP)
	for _, lrp := range list {
		result[lrp.ProcessGuid] = lrp
	}
	return result
}
