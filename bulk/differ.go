package bulk

import (
	"reflect"

	"github.com/cloudfoundry-incubator/runtime-schema/models"
)

func Diff(existing []models.DesiredLRP, newChan <-chan models.DesiredLRP) <-chan models.DesiredLRPChange {
	changeChan := make(chan models.DesiredLRPChange)
	existingLRPs := organizeLRPsByProcessGuid(existing)

	go func() {
		for lrp := range newChan {
			change := changeForNewLRP(existingLRPs, lrp)
			delete(existingLRPs, lrp.ProcessGuid)
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

func changeForNewLRP(existing map[string]models.DesiredLRP, newLRP models.DesiredLRP) *models.DesiredLRPChange {
	if existingLRP, ok := existing[newLRP.ProcessGuid]; ok {
		if reflect.DeepEqual(existingLRP, newLRP) {
			return nil
		} else {
			return &models.DesiredLRPChange{Before: &existingLRP, After: &newLRP}
		}
	} else {
		return &models.DesiredLRPChange{After: &newLRP}
	}
}

func organizeLRPsByProcessGuid(list []models.DesiredLRP) map[string]models.DesiredLRP {
	result := make(map[string]models.DesiredLRP)
	for _, lrp := range list {
		result[lrp.ProcessGuid] = lrp
	}
	return result
}
