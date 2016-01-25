package handlers

import (
	"encoding/json"
	"net/http"

	"github.com/cloudfoundry-incubator/bbs"
	"github.com/cloudfoundry-incubator/nsync/recipebuilder"
	"github.com/cloudfoundry-incubator/runtime-schema/cc_messages"
	"github.com/pivotal-golang/lager"
)

type TaskHandler struct {
	logger         lager.Logger
	recipeBuilders map[string]recipebuilder.RecipeBuilder
	bbsClient      bbs.Client
}

func NewTaskHandler(
	logger lager.Logger,
	bbsClient bbs.Client,
	recipeBuilders map[string]recipebuilder.RecipeBuilder,
) TaskHandler {
	return TaskHandler{
		logger:         logger,
		recipeBuilders: recipeBuilders,
		bbsClient:      bbsClient,
	}
}

func (h *TaskHandler) DesireTask(resp http.ResponseWriter, req *http.Request) {
	taskGuid := req.FormValue(":task_guid")
	logger := h.logger.Session("create-task", lager.Data{
		"method":  req.Method,
		"request": req.URL.String(),
	})

	logger.Info("serving")
	defer logger.Info("complete")

	task := cc_messages.TaskRequestFromCC{}
	err := json.NewDecoder(req.Body).Decode(&task)
	if err != nil {
		logger.Error("parse-task-request-failed", err)
		resp.WriteHeader(http.StatusBadRequest)
		return
	}

	builder := h.recipeBuilders[task.Lifecycle]
	desiredTask, err := builder.BuildTask(&task)
	if err != nil {
		logger.Error("building-task-failed", err)
		resp.WriteHeader(http.StatusBadRequest)
		return
	}

	logger.Info("desiring-task")
	err = h.bbsClient.DesireTask(taskGuid, cc_messages.RunningTaskDomain, desiredTask)
	if err != nil {
		logger.Error("desire-task-failed", err)
		resp.WriteHeader(http.StatusBadRequest)
		return
	}

	resp.WriteHeader(http.StatusAccepted)
}
