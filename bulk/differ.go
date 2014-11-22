package bulk

import (
	"github.com/cloudfoundry-incubator/receptor"
	"github.com/cloudfoundry-incubator/runtime-schema/cc_messages"
	"github.com/pivotal-golang/lager"
)

type Differ interface {
	Diff(
		existing []receptor.DesiredLRPResponse,
		desiredChan <-chan *cc_messages.DesireAppRequestFromCC,
		lrpChan chan<- *receptor.DesiredLRPCreateRequest,
		deleteListChan chan<- []string,
	)
}

type RecipeBuilder interface {
	Build(*cc_messages.DesireAppRequestFromCC) (*receptor.DesiredLRPCreateRequest, error)
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

func (d *differ) Diff(
	existing []receptor.DesiredLRPResponse,
	desiredChan <-chan *cc_messages.DesireAppRequestFromCC,
	lrpChan chan<- *receptor.DesiredLRPCreateRequest,
	deleteListChan chan<- []string,
) {
	defer close(deleteListChan)

	existingLRPs := organizeLRPsByProcessGuid(existing)

	diffLog := d.logger.Session("diff")

	for fromCC := range desiredChan {
		createReq := d.createDesiredLRPRequest(diffLog, fromCC)
		if createReq != nil {
			lrpChan <- createReq
		}

		delete(existingLRPs, fromCC.ProcessGuid)
	}

	close(lrpChan)

	deleteList := []string{}
	for _, lrp := range existingLRPs {
		diffLog.Info("found-extra-desired-lrp", lager.Data{
			"guid": lrp.ProcessGuid,
		})
		deleteList = append(deleteList, lrp.ProcessGuid)
	}
	deleteListChan <- deleteList
}

func (d *differ) createDesiredLRPRequest(
	logger lager.Logger,
	fromCC *cc_messages.DesireAppRequestFromCC,
) *receptor.DesiredLRPCreateRequest {
	var createReq *receptor.DesiredLRPCreateRequest

	logger.Info("found-missing-desired-lrp", lager.Data{
		"guid": fromCC.ProcessGuid,
	})

	createReq, err := d.builder.Build(fromCC)
	if err != nil {
		logger.Error("failed-to-build-recipe", err, lager.Data{
			"desired-by-cc": fromCC,
		})

		return nil
	}

	return createReq
}

func organizeLRPsByProcessGuid(list []receptor.DesiredLRPResponse) map[string]*receptor.DesiredLRPResponse {
	result := make(map[string]*receptor.DesiredLRPResponse)
	for _, l := range list {
		lrp := l
		result[lrp.ProcessGuid] = &lrp
	}

	return result
}
