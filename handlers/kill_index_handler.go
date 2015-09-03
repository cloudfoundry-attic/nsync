package handlers

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"

	"github.com/cloudfoundry-incubator/bbs"
	"github.com/cloudfoundry-incubator/bbs/models"
	"github.com/pivotal-golang/lager"
)

var (
	missingParameterErr = errors.New("missing from request")
	invalidNumberErr    = errors.New("not a number")
)

type KillIndexHandler struct {
	bbsClient bbs.Client
	logger    lager.Logger
}

func NewKillIndexHandler(logger lager.Logger, bbsClient bbs.Client) KillIndexHandler {
	return KillIndexHandler{
		bbsClient: bbsClient,
		logger:    logger,
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

	err = h.killActualLRPByProcessGuidAndIndex(processGuid, index)
	if err != nil {
		status := http.StatusServiceUnavailable
		bbsError := models.ConvertError(err)
		if bbsError.Type == models.Error_ResourceNotFound {
			err = fmt.Errorf("process-guid '%s' does not exist or has no instance at index %d", processGuid, index)
			status = http.StatusNotFound
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

func (h *KillIndexHandler) killActualLRPByProcessGuidAndIndex(processGuid string, index int) error {
	actualLRPGroup, err := h.bbsClient.ActualLRPGroupByProcessGuidAndIndex(processGuid, index)
	if err != nil {
		return err
	}

	actualLRP, _ := actualLRPGroup.Resolve()
	return h.bbsClient.RetireActualLRP(&actualLRP.ActualLRPKey)
}
