package handlers

import (
	"encoding/json"
	"net/http"

	"github.com/cloudfoundry-incubator/bbs"
	"github.com/cloudfoundry-incubator/bbs/models"
	"github.com/cloudfoundry-incubator/nsync/helpers"
	"github.com/cloudfoundry-incubator/nsync/recipebuilder"
	"github.com/cloudfoundry-incubator/routing-info/cfroutes"
	"github.com/cloudfoundry-incubator/routing-info/tcp_routes"
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
		"method":       req.Method,
		"request":      req.URL.String(),
	})

	logger.Info("serving")
	defer logger.Info("complete")

	desiredApp := cc_messages.DesireAppRequestFromCC{}
	err := json.NewDecoder(req.Body).Decode(&desiredApp)
	if err != nil {
		logger.Error("parse-desired-app-request-failed", err)
		resp.WriteHeader(http.StatusBadRequest)
		return
	}
	logger.Info("request-from-cc", lager.Data{"routing_info": desiredApp.RoutingInfo})

	envNames := []string{}
	for _, envVar := range desiredApp.Environment {
		envNames = append(envNames, envVar.Name)
	}
	logger.Debug("environment", lager.Data{"keys": envNames})

	if processGuid != desiredApp.ProcessGuid {
		logger.Error("process-guid-mismatch", err, lager.Data{"body-process-guid": desiredApp.ProcessGuid})
		resp.WriteHeader(http.StatusBadRequest)
		return
	}

	statusCode := http.StatusConflict

	for tries := 2; tries > 0 && statusCode == http.StatusConflict; tries-- {
		existingLRP, err := h.getDesiredLRP(logger, processGuid)
		if err != nil {
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

func (h *DesireAppHandler) getDesiredLRP(logger lager.Logger, processGuid string) (*models.DesiredLRP, error) {
	logger.Debug("fetching-desired-lrp")
	lrp, err := h.bbsClient.DesiredLRPByProcessGuid(processGuid)
	logger.Debug("fetched-desired-lrp")
	if err == nil {
		logger.Debug("desired-lrp-already-present")
		return lrp, nil
	}

	bbsError := models.ConvertError(err)
	if bbsError.Type == models.Error_ResourceNotFound {
		logger.Error("desired-lrp-not-found", err)
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

	logger.Debug("creating-desired-lrp", lager.Data{"routes": sanitizeRoutes(desiredLRP.Routes)})
	err = h.bbsClient.DesireLRP(desiredLRP)
	if err != nil {
		logger.Error("failed-to-create-lrp", err)
		return err
	}
	logger.Debug("created-desired-lrp")

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
	ports, err := builder.ExtractExposedPorts(&desireAppMessage)
	if err != nil {
		logger.Error("failed to-get-exposed-port", err)
		return err
	}

	updateRoutes, err := helpers.CCRouteInfoToRoutes(desireAppMessage.RoutingInfo, ports)
	if err != nil {
		logger.Error("failed-to-marshal-routes", err)
		return err
	}

	routes := existingLRP.Routes
	if routes == nil {
		routes = &models.Routes{}
	}

	if value, ok := updateRoutes[cfroutes.CF_ROUTER]; ok {
		(*routes)[cfroutes.CF_ROUTER] = value
	}
	if value, ok := updateRoutes[tcp_routes.TCP_ROUTER]; ok {
		(*routes)[tcp_routes.TCP_ROUTER] = value
	}
	instances := int32(desireAppMessage.NumInstances)
	updateRequest := &models.DesiredLRPUpdate{
		Annotation: &desireAppMessage.ETag,
		Instances:  &instances,
		Routes:     routes,
	}

	logger.Debug("updating-desired-lrp", lager.Data{"routes": sanitizeRoutes(existingLRP.Routes)})
	err = h.bbsClient.UpdateDesiredLRP(desireAppMessage.ProcessGuid, updateRequest)
	if err != nil {
		logger.Error("failed-to-update-lrp", err)
		return err
	}
	logger.Debug("updated-desired-lrp")

	return nil
}

func sanitizeRoutes(routes *models.Routes) *models.Routes {
	newRoutes := make(models.Routes)
	if routes != nil {
		cfRoutes := *routes
		newRoutes[cfroutes.CF_ROUTER] = cfRoutes[cfroutes.CF_ROUTER]
		newRoutes[tcp_routes.TCP_ROUTER] = cfRoutes[tcp_routes.TCP_ROUTER]
	}
	return &newRoutes
}
