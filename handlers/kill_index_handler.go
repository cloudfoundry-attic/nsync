package handlers

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/cloudfoundry-incubator/receptor"
	"github.com/pivotal-golang/lager"
)

var (
	missingParameterErr = errors.New("missing from request")
	invalidNumberErr    = errors.New("not a number")
)

type KillIndexHandler struct {
	receptorClient receptor.Client
	logger         lager.Logger
}

func NewKillIndexHandler(logger lager.Logger, receptorClient receptor.Client) KillIndexHandler {
	return KillIndexHandler{
		receptorClient: receptorClient,
		logger:         logger,
	}
}

func (h *KillIndexHandler) KillIndex(resp http.ResponseWriter, req *http.Request) {
	processGuid := req.FormValue(":process_guid")
	indexString := req.FormValue(":index")
	logger := h.logger.Session("kill-index", lager.Data{
		"ProcessGuid": processGuid,
		"Index":       indexString,
	})

	if processGuid == "" {
		logger.Error("missing-process-guid", missingParameterErr)
		resp.WriteHeader(http.StatusBadRequest)
		return
	}

	if indexString == "" {
		logger.Error("missing-index", missingParameterErr)
		resp.WriteHeader(http.StatusBadRequest)
		return
	}

	index, err := strconv.Atoi(indexString)
	if err != nil {
		logger.Error("invalid-index", invalidNumberErr)
		resp.WriteHeader(http.StatusBadRequest)
		return
	}

	err = h.receptorClient.KillActualLRPByProcessGuidAndIndex(processGuid, index)
	if err != nil {
		status := http.StatusServiceUnavailable
		switch e := err.(type) {
		case receptor.Error:
			if e.Type == receptor.ActualLRPIndexNotFound {
				status = http.StatusBadRequest
			}
		default:
		}

		resp.WriteHeader(status)
		logger.Error("request-kill-actual-lrp-index-failed", err)
		return
	}

	logger.Info("requested-stop-index", lager.Data{
		"process_guid": processGuid,
		"index":        index,
	})
	resp.WriteHeader(http.StatusAccepted)
}
