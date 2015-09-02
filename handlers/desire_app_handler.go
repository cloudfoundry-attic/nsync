package handlers

import (
	"encoding/json"
	"net/http"

	"github.com/cloudfoundry-incubator/nsync/recipebuilder"
	"github.com/cloudfoundry-incubator/receptor"
	"github.com/cloudfoundry-incubator/route-emitter/cfroutes"
	"github.com/cloudfoundry-incubator/runtime-schema/cc_messages"
	"github.com/cloudfoundry-incubator/runtime-schema/metric"
	"github.com/pivotal-golang/lager"
)

const (
	desiredLRPCounter = metric.Counter("LRPsDesired")
)

//go:generate counterfeiter -o fakes/fake_recipe_builder.go . RecipeBuilder
type RecipeBuilder interface {
	Build(*cc_messages.DesireAppRequestFromCC) (*receptor.DesiredLRPCreateRequest, error)
}

type DesireAppHandler struct {
	recipeBuilders map[string]recipebuilder.RecipeBuilder
	receptorClient receptor.Client
	logger         lager.Logger
}

func NewDesireAppHandler(logger lager.Logger, receptorClient receptor.Client, builders map[string]recipebuilder.RecipeBuilder) DesireAppHandler {
	return DesireAppHandler{
		recipeBuilders: builders,
		receptorClient: receptorClient,
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
			if receptorErr, ok := err.(receptor.Error); ok &&
				(receptorErr.Type == receptor.DesiredLRPAlreadyExists ||
					receptorErr.Type == receptor.ResourceConflict) {
				statusCode = http.StatusConflict
			} else if _, ok := err.(recipebuilder.Error); ok {
				statusCode = http.StatusBadRequest
			} else {
				statusCode = http.StatusServiceUnavailable
			}
		} else {
			statusCode = http.StatusAccepted
			desiredLRPCounter.Increment()
		}
	}

	resp.WriteHeader(statusCode)
}

func (h *DesireAppHandler) getDesiredLRP(processGuid string) (*receptor.DesiredLRPResponse, error) {
	lrp, err := h.receptorClient.GetDesiredLRP(processGuid)
	if err == nil {
		return &lrp, nil
	}

	if rerr, ok := err.(receptor.Error); ok {
		if rerr.Type == receptor.DesiredLRPNotFound {
			return nil, nil
		}
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

	err = h.receptorClient.CreateDesiredLRP(*desiredLRP)
	if err != nil {
		logger.Error("failed-to-create-lrp", err)
		return err
	}

	return nil
}

func (h *DesireAppHandler) updateDesiredApp(
	logger lager.Logger,
	existingLRP *receptor.DesiredLRPResponse,
	desireAppMessage cc_messages.DesireAppRequestFromCC,
) error {
	routes := existingLRP.Routes

	var builder recipebuilder.RecipeBuilder = h.recipeBuilders["buildpack"]
	if desireAppMessage.DockerImageUrl != "" {
		builder = h.recipeBuilders["docker"]
	}
	port, err := builder.ExtractExposedPort(desireAppMessage.ExecutionMetadata)
	if err != nil {
		logger.Error("failed to-get-exposed-port", err)
		return err
	}

	cfRoutesJson, err := json.Marshal(cfroutes.LegacyCFRoutes{
		{Hostnames: desireAppMessage.Routes, Port: port},
	})
	if err != nil {
		logger.Error("failed-to-marshal-routes", err)
		return err
	}

	cfRoutesMessage := json.RawMessage(cfRoutesJson)
	routes[cfroutes.CF_ROUTER] = &cfRoutesMessage

	updateRequest := receptor.DesiredLRPUpdateRequest{
		Annotation: &desireAppMessage.ETag,
		Instances:  &desireAppMessage.NumInstances,
		Routes:     routes,
	}

	err = h.receptorClient.UpdateDesiredLRP(desireAppMessage.ProcessGuid, updateRequest)
	if err != nil {
		logger.Error("failed-to-update-lrp", err)
		return err
	}

	return nil
}

func (h *DesireAppHandler) deleteDesiredApp(logger lager.Logger, processGuid string) error {
	err := h.receptorClient.DeleteDesiredLRP(processGuid)
	if err == nil {
		return nil
	}

	if rerr, ok := err.(receptor.Error); ok {
		if rerr.Type == receptor.DesiredLRPNotFound {
			logger.Info("lrp-already-deleted")
			return nil
		}
	}

	logger.Error("failed-to-remove", err)
	return err
}
