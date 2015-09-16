package handlers

import (
	"encoding/json"
	"net/http"

	"github.com/cloudfoundry-incubator/bbs"
	"github.com/cloudfoundry-incubator/bbs/models"
	"github.com/cloudfoundry-incubator/nsync/helpers"
	"github.com/cloudfoundry-incubator/nsync/recipebuilder"
	"github.com/cloudfoundry-incubator/route-emitter/cfroutes"
	"github.com/cloudfoundry-incubator/runtime-schema/cc_messages"
	"github.com/cloudfoundry-incubator/runtime-schema/metric"
	"github.com/pivotal-golang/lager"
)

const (
	desiredLRPCounter = metric.Counter("LRPsDesired")
)

type DesireAppHandler struct {
	recipeBuilders map[string]recipebuilder.RecipeBuilder
	bbsClient      bbs.Client
	logger         lager.Logger
}

func NewDesireAppHandler(logger lager.Logger, bbsClient bbs.Client, builders map[string]recipebuilder.RecipeBuilder) DesireAppHandler {
	return DesireAppHandler{
		recipeBuilders: builders,
		bbsClient:      bbsClient,
		logger:         logger,
	}
}

func (h *DesireAppHandler) DesireApp(resp http.ResponseWriter, req *http.Request) {
	processGuid := req.FormValue(":process_guid")
	logger := h.logger.Session("desire-app", lager.Data{
		"process_guid": processGuid,
	})

	desiredApp := cc_messages.DesireAppRequestFromCC{}
	err := json.NewDecoder(req.Body).Decode(&desiredApp)
	if err != nil {
		logger.Error("parse-desired-app-request-failed", err)
		resp.WriteHeader(http.StatusBadRequest)
		return
	}

	if processGuid != desiredApp.ProcessGuid {
		logger.Error("process-guid-mismatch", err, lager.Data{"body-process-guid": desiredApp.ProcessGuid})
		resp.WriteHeader(http.StatusBadRequest)
		return
	}

	statusCode := http.StatusConflict

	for tries := 2; tries > 0 && statusCode == http.StatusConflict; tries-- {
		existingLRP, err := h.getDesiredLRP(processGuid)
		if err != nil {
			logger.Error("unexpected-error-from-get-desired-lrp", err)
			statusCode = http.StatusServiceUnavailable
			break
		}

		if existingLRP != nil {
			err = h.updateDesiredApp(logger, existingLRP, desiredApp)
		} else {
			err = h.createDesiredApp(logger, desiredApp)
		}

		if err != nil {
			mErr := models.ConvertError(err)
			switch mErr.Type {
			case models.Error_ResourceConflict:
				fallthrough
			case models.Error_ResourceExists:
				statusCode = http.StatusConflict
			default:
				if _, ok := err.(recipebuilder.Error); ok {
					statusCode = http.StatusBadRequest
				} else {
					statusCode = http.StatusServiceUnavailable
				}
			}
		} else {
			statusCode = http.StatusAccepted
			desiredLRPCounter.Increment()
		}
	}

	resp.WriteHeader(statusCode)
}

func (h *DesireAppHandler) getDesiredLRP(processGuid string) (*models.DesiredLRP, error) {
	lrp, err := h.bbsClient.DesiredLRPByProcessGuid(processGuid)
	if err == nil {
		return lrp, nil
	}

	bbsError := models.ConvertError(err)
	if bbsError.Type == models.Error_ResourceNotFound {
		return nil, nil
	}

	return nil, err
}

func (h *DesireAppHandler) createDesiredApp(
	logger lager.Logger,
	desireAppMessage cc_messages.DesireAppRequestFromCC,
) error {
	var builder recipebuilder.RecipeBuilder = h.recipeBuilders["buildpack"]
	if desireAppMessage.DockerImageUrl != "" {
		builder = h.recipeBuilders["docker"]
	}

	desiredLRP, err := builder.Build(&desireAppMessage)
	if err != nil {
		logger.Error("failed-to-build-recipe", err)
		return err
	}

	err = h.bbsClient.DesireLRP(desiredLRP)
	if err != nil {
		logger.Error("failed-to-create-lrp", err)
		return err
	}

	return nil
}

func (h *DesireAppHandler) updateDesiredApp(
	logger lager.Logger,
	existingLRP *models.DesiredLRP,
	desireAppMessage cc_messages.DesireAppRequestFromCC,
) error {
	var builder recipebuilder.RecipeBuilder = h.recipeBuilders["buildpack"]
	if desireAppMessage.DockerImageUrl != "" {
		builder = h.recipeBuilders["docker"]
	}
	port, err := builder.ExtractExposedPort(desireAppMessage.ExecutionMetadata)
	if err != nil {
		logger.Error("failed to-get-exposed-port", err)
		return err
	}

	cfRoutes, err := helpers.CCRouteInfoToCFRoutes(desireAppMessage.RoutingInfo, port)
	if err != nil {
		logger.Error("failed-to-marshal-routes", err)
		return err
	}

	cfRoutesJson, err := json.Marshal(cfRoutes)

	if err != nil {
		logger.Error("failed-to-marshal-routes", err)
		return err
	}

	routes := existingLRP.Routes
	if routes == nil {
		routes = &models.Routes{}
	}

	cfRoutesMessage := json.RawMessage(cfRoutesJson)
	(*routes)[cfroutes.CF_ROUTER] = &cfRoutesMessage
	instances := int32(desireAppMessage.NumInstances)
	updateRequest := &models.DesiredLRPUpdate{
		Annotation: &desireAppMessage.ETag,
		Instances:  &instances,
		Routes:     routes,
	}

	err = h.bbsClient.UpdateDesiredLRP(desireAppMessage.ProcessGuid, updateRequest)
	if err != nil {
		logger.Error("failed-to-update-lrp", err)
		return err
	}

	return nil
}
