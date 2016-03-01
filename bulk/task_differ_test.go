package bulk_test

import (
	"github.com/cloudfoundry-incubator/nsync/bulk"
	"github.com/cloudfoundry-incubator/runtime-schema/cc_messages"
	"github.com/pivotal-golang/lager/lagertest"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
)

var _ = Describe("TaskDiffer", func() {

	var (
		bbsTasks map[string]struct{}
		ccTasks  chan []cc_messages.CCTaskState
		cancelCh chan struct{}
		logger   *lagertest.TestLogger
		differ   bulk.TaskDiffer
	)

	BeforeEach(func() {
		logger = lagertest.NewTestLogger("test")
		cancelCh = make(chan struct{})
		differ = bulk.NewTaskDiffer()
		ccTasks = make(chan []cc_messages.CCTaskState, 1)
	})

	Context("when bbs does not know about a running task", func() {
		expectedTask := cc_messages.CCTaskState{TaskGuid: "task-guid-1", State: cc_messages.TaskStateRunning, CompletionCallbackUrl: "asdf"}

		BeforeEach(func() {
			ccTasks <- []cc_messages.CCTaskState{expectedTask}
			close(ccTasks)
		})

		It("includes it in UnknownToDiego", func() {
			differ.Diff(logger, ccTasks, bbsTasks, cancelCh)

			Eventually(differ.UnknownToDiego()).Should(Receive(ConsistOf(expectedTask)))
		})
	})

	Context("when bbs does not know about a pending task", func() {
		BeforeEach(func() {
			ccTasks <- []cc_messages.CCTaskState{
				{TaskGuid: "task-guid-1", State: cc_messages.TaskStatePending},
			}
			close(ccTasks)
		})

		It("is not included in UnknownToDiego", func() {
			differ.Diff(logger, ccTasks, bbsTasks, cancelCh)

			Consistently(differ.UnknownToDiego()).Should(Not(Receive()))
		})
	})

	Context("when bbs does not know about a completed task", func() {
		BeforeEach(func() {
			ccTasks <- []cc_messages.CCTaskState{
				{TaskGuid: "task-guid-1", State: cc_messages.TaskStateSucceeded},
			}
			close(ccTasks)
		})

		It("is not included in UnknownToDiego", func() {
			differ.Diff(logger, ccTasks, bbsTasks, cancelCh)

			Consistently(differ.UnknownToDiego()).Should(Not(Receive()))
		})
	})

	Context("when bbs knows about a running task", func() {
		BeforeEach(func() {
			bbsTasks = map[string]struct{}{"task-guid-1": struct{}{}}
			ccTasks <- []cc_messages.CCTaskState{
				{TaskGuid: "task-guid-1", State: cc_messages.TaskStateRunning},
			}
			close(ccTasks)
		})

		It("is not included in UnknownToDiego", func() {
			differ.Diff(logger, ccTasks, bbsTasks, cancelCh)

			Consistently(differ.UnknownToDiego()).Should(Not(Receive()))
		})
	})

	Context("canceling", func() {
		Context("when it is receiving tasks", func() {
			BeforeEach(func() {
				ccTasks <- []cc_messages.CCTaskState{
					{TaskGuid: "task-guid-1", State: cc_messages.TaskStateRunning},
				}
				close(ccTasks)
			})

			It("closes the output channels", func() {
				close(cancelCh)
				differ.Diff(logger, ccTasks, bbsTasks, cancelCh)

				Eventually(differ.UnknownToDiego()).Should(BeClosed())
			})
		})

		Context("when it is not receiving tasks", func() {
			It("closes the output channels", func() {
				close(cancelCh)
				differ.Diff(logger, ccTasks, bbsTasks, cancelCh)

				Eventually(differ.UnknownToDiego()).Should(BeClosed())
			})
		})
	})
})
