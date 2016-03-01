package bulk

import (
	"github.com/cloudfoundry-incubator/runtime-schema/cc_messages"
	"github.com/pivotal-golang/lager"
)

type TaskDiffer interface {
	Diff(lager.Logger, <-chan []cc_messages.CCTaskState, map[string]struct{}, <-chan struct{})
	UnknownToDiego() <-chan []cc_messages.CCTaskState
}

type taskDiffer struct {
	unknownToDiego chan []cc_messages.CCTaskState
}

func NewTaskDiffer() TaskDiffer {
	return &taskDiffer{
		unknownToDiego: make(chan []cc_messages.CCTaskState, 1),
	}
}

func (t *taskDiffer) Diff(logger lager.Logger, ccTasks <-chan []cc_messages.CCTaskState, bbsTasks map[string]struct{}, cancelCh <-chan struct{}) {
	logger = logger.Session("task_diff")

	go func() {
		defer func() {
			close(t.unknownToDiego)
		}()

		for {
			select {
			case <-cancelCh:
				return

			case batch, open := <-ccTasks:
				if !open {
					return
				}

				batchUnknownToDiego := []cc_messages.CCTaskState{}
				for _, taskState := range batch {
					_, exists := bbsTasks[taskState.TaskGuid]
					if !exists && taskState.State == cc_messages.TaskStateRunning {
						batchUnknownToDiego = append(batchUnknownToDiego, taskState)

						logger.Info("found-unkown-to-diego-task", lager.Data{
							"guid": taskState.TaskGuid,
						})
					}
				}

				if len(batchUnknownToDiego) > 0 {
					t.unknownToDiego <- batchUnknownToDiego
				}
			}
		}
	}()
}

func (t *taskDiffer) UnknownToDiego() <-chan []cc_messages.CCTaskState {
	return t.unknownToDiego
}
