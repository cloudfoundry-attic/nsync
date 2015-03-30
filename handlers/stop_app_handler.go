package handlers

import (
	"net/http"

	"github.com/cloudfoundry-incubator/receptor"
	"github.com/pivotal-golang/lager"
)

type StopAppHandler struct {
	receptorClient receptor.Client
	logger         lager.Logger
}

func NewStopAppHandler(logger lager.Logger, receptorClient receptor.Client) *StopAppHandler {
	return &StopAppHandler{
		logger:         logger,
		receptorClient: receptorClient,
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

	err := h.receptorClient.DeleteDesiredLRP(processGuid)
	if err != nil {
		logger.Error("failed-to-delete-desired-lrp", err)
		if rerr, ok := err.(receptor.Error); ok {
			if rerr.Type == receptor.DesiredLRPNotFound {
				resp.WriteHeader(http.StatusNotFound)
				return
			}
		}
		resp.WriteHeader(http.StatusServiceUnavailable)
		return
	}

	resp.WriteHeader(http.StatusAccepted)
}
