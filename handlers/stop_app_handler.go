package handlers

import (
	"net/http"

	"github.com/cloudfoundry-incubator/bbs"
	"github.com/cloudfoundry-incubator/bbs/models"
	"github.com/pivotal-golang/lager"
)

type StopAppHandler struct {
	bbsClient bbs.Client
	logger    lager.Logger
}

func NewStopAppHandler(logger lager.Logger, bbsClient bbs.Client) *StopAppHandler {
	return &StopAppHandler{
		logger:    logger,
		bbsClient: bbsClient,
	}
}

func (h *StopAppHandler) StopApp(resp http.ResponseWriter, req *http.Request) {
	processGuid := req.FormValue(":process_guid")
	logger := h.logger.Session("stop-app", lager.Data{"process-guid": processGuid})

	if processGuid == "" {
		logger.Error("missing-process-guid", missingParameterErr)
		resp.WriteHeader(http.StatusBadRequest)
		return
	}

	logger.Info("stop-request-from-cc", lager.Data{"processGuid": processGuid})

	err := h.bbsClient.RemoveDesiredLRP(processGuid)
	if err != nil {
		logger.Error("failed-to-delete-desired-lrp", err)

		bbsError := models.ConvertError(err)
		if bbsError.Type == models.Error_ResourceNotFound {
			resp.WriteHeader(http.StatusNotFound)
			return
		}

		resp.WriteHeader(http.StatusServiceUnavailable)
		return
	}

	resp.WriteHeader(http.StatusAccepted)
}
