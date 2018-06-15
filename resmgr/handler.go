package resmgr

import (
	"context"
	"fmt"
	"reflect"
	"sync/atomic"
	"time"

	"code.uber.internal/infra/peloton/.gen/peloton/api/v0/peloton"
	t "code.uber.internal/infra/peloton/.gen/peloton/api/v0/task"
	pb_eventstream "code.uber.internal/infra/peloton/.gen/peloton/private/eventstream"
	"code.uber.internal/infra/peloton/.gen/peloton/private/resmgr"
	"code.uber.internal/infra/peloton/.gen/peloton/private/resmgrsvc"

	"code.uber.internal/infra/peloton/common"
	"code.uber.internal/infra/peloton/common/eventstream"
	"code.uber.internal/infra/peloton/common/queue"
	"code.uber.internal/infra/peloton/common/statemachine"
	"code.uber.internal/infra/peloton/resmgr/preemption"
	r_queue "code.uber.internal/infra/peloton/resmgr/queue"
	"code.uber.internal/infra/peloton/resmgr/respool"
	"code.uber.internal/infra/peloton/resmgr/scalar"
	rmtask "code.uber.internal/infra/peloton/resmgr/task"
	"code.uber.internal/infra/peloton/util"

	"github.com/pkg/errors"
	log "github.com/sirupsen/logrus"
	"github.com/uber-go/tally"
	"go.uber.org/yarpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

var (
	errFailingGangMemberTask    = errors.New("task fail because other gang member failed")
	errSameTaskPresent          = errors.New("same task present in tracker, Ignoring new task")
	errGangNotEnqueued          = errors.New("could not enqueue gang to ready after retry")
	errEnqueuedAgain            = errors.New("enqueued again after retry")
	errUnplacedTaskInWrongState = errors.New("unplaced task should be in state placing")
	errRequeueTaskFailed        = errors.New("requeue exisiting task to resmgr failed")
	errGangEnqueuedPending      = errors.New("could not enqueue gang to pending after multiple placement retry")
)

// ServiceHandler implements peloton.private.resmgr.ResourceManagerService
type ServiceHandler struct {
	metrics            *Metrics
	resPoolTree        respool.Tree
	placements         queue.Queue
	eventStreamHandler *eventstream.Handler
	rmTracker          rmtask.Tracker
	preemptor          preemption.Preemptor
	maxOffset          *uint64
	config             Config
}

// InitServiceHandler initializes the handler for ResourceManagerService
func InitServiceHandler(
	d *yarpc.Dispatcher,
	parent tally.Scope,
	rmTracker rmtask.Tracker,
	preemptor preemption.Preemptor,
	conf Config) *ServiceHandler {

	var maxOffset uint64
	handler := &ServiceHandler{
		metrics:     NewMetrics(parent.SubScope("resmgr")),
		resPoolTree: respool.GetTree(),
		placements: queue.NewQueue(
			"placement-queue",
			reflect.TypeOf(resmgr.Placement{}),
			maxPlacementQueueSize,
		),
		rmTracker: rmTracker,
		preemptor: preemptor,
		maxOffset: &maxOffset,
		config:    conf,
	}
	// TODO: move eventStreamHandler buffer size into config
	handler.eventStreamHandler = initEventStreamHandler(d, 1000, parent.SubScope("resmgr"))

	d.Register(resmgrsvc.BuildResourceManagerServiceYARPCProcedures(handler))
	return handler
}

func initEventStreamHandler(d *yarpc.Dispatcher, bufferSize int, parentScope tally.Scope) *eventstream.Handler {
	eventStreamHandler := eventstream.NewEventStreamHandler(
		bufferSize,
		[]string{
			common.PelotonJobManager,
			common.PelotonResourceManager,
		},
		nil,
		parentScope)

	d.Register(pb_eventstream.BuildEventStreamServiceYARPCProcedures(eventStreamHandler))

	return eventStreamHandler
}

// GetStreamHandler returns the stream handler
func (h *ServiceHandler) GetStreamHandler() *eventstream.Handler {
	return h.eventStreamHandler
}

// EnqueueGangs implements ResourceManagerService.EnqueueGangs
func (h *ServiceHandler) EnqueueGangs(
	ctx context.Context,
	req *resmgrsvc.EnqueueGangsRequest,
) (*resmgrsvc.EnqueueGangsResponse, error) {

	log.WithField("request", req).Info("EnqueueGangs called.")
	h.metrics.APIEnqueueGangs.Inc(1)

	// Lookup respool from the resource pool tree
	var err error
	var resourcePool respool.ResPool
	respoolID := req.GetResPool()
	if respoolID != nil {
		resourcePool, err = respool.GetTree().Get(respoolID)
		if err != nil {
			h.metrics.EnqueueGangFail.Inc(1)
			return &resmgrsvc.EnqueueGangsResponse{
				Error: &resmgrsvc.EnqueueGangsResponse_Error{
					NotFound: &resmgrsvc.ResourcePoolNotFound{
						Id:      respoolID,
						Message: err.Error(),
					},
				},
			}, nil
		}
	}

	// Enqueue the gangs sent in an API call to the pending queue of the respool.
	// For each gang, add its tasks to the state machine, enqueue the gang, and
	// return per-task success/failure.
	var failed []*resmgrsvc.EnqueueGangsFailure_FailedTask
	for _, gang := range req.GetGangs() {
		// Here we are checking if the respool is nil that means
		// These gangs are failed in placement engine and been returned
		// to resmgr for enqueuing again.
		if resourcePool == nil {
			failed, err = h.returnExistingTasks(gang, req.GetReason())
		} else {
			failed, err = h.enqueueGang(gang, resourcePool)
		}
		if err != nil {
			h.metrics.EnqueueGangFail.Inc(1)
			continue
		}
		h.metrics.EnqueueGangSuccess.Inc(1)
	}

	if len(failed) > 0 {
		return &resmgrsvc.EnqueueGangsResponse{
			Error: &resmgrsvc.EnqueueGangsResponse_Error{
				Failure: &resmgrsvc.EnqueueGangsFailure{
					Failed: failed,
				},
			},
		}, nil
	}

	response := resmgrsvc.EnqueueGangsResponse{}
	log.Debug("Enqueue Returned")
	return &response, nil
}

func (h *ServiceHandler) returnExistingTasks(gang *resmgrsvc.Gang, reason string) (
	[]*resmgrsvc.EnqueueGangsFailure_FailedTask, error) {
	allTasksExist := true
	failedTasks := make(map[string]bool)
	var failed []*resmgrsvc.EnqueueGangsFailure_FailedTask
	for _, task := range gang.GetTasks() {
		task := h.rmTracker.GetTask(task.GetId())
		if task == nil {
			allTasksExist = false
			break
		}
	}

	if !allTasksExist {
		// Making the whole gang failed as there are not all tasks present
		err := fmt.Errorf("not all tasks for the gang exists in the resource manager")
		log.WithError(err).Error("failing gang to requeue again ")
		failed = append(failed, h.markingTasksFailInGang(gang, failedTasks, err)...)
		return failed, err
	}

	for _, task := range gang.GetTasks() {
		err := h.requeueUnplacedTask(task, reason)
		if err != nil {
			failed = append(
				failed,
				&resmgrsvc.EnqueueGangsFailure_FailedTask{
					Task:      task,
					Message:   err.Error(),
					Errorcode: resmgrsvc.EnqueueGangsFailure_ENQUEUE_GANGS_FAILURE_ERROR_CODE_INTERNAL,
				},
			)
			failedTasks[task.Id.Value] = true
		}
	}
	var err error
	if len(failed) > 0 {
		// If there are some tasks in this gang been failed to enqueue
		// we are making all the tasks in this gang to be failed
		// as we can enqueue the full gang or full gang will be failed.
		failed = append(failed, h.markingTasksFailInGang(gang, failedTasks, errFailingGangMemberTask)...)
		err = fmt.Errorf("some tasks failed to be re-enqueued")
	}
	return failed, err
}

// enqueueGang adds the new gangs to pending queue or
// requeue the gang if the tasks have different mesos
// taskid.
func (h *ServiceHandler) enqueueGang(
	gang *resmgrsvc.Gang,
	respool respool.ResPool) (
	[]*resmgrsvc.EnqueueGangsFailure_FailedTask,
	error) {
	var failed []*resmgrsvc.EnqueueGangsFailure_FailedTask
	var failedTask *resmgrsvc.EnqueueGangsFailure_FailedTask
	var err error
	failedTasks := make(map[string]bool)
	isGangRequeued := false
	for _, task := range gang.GetTasks() {
		if !(h.isTaskPresent(task)) {
			// If the task is not present in the tracker
			// this means its a new task and needs to be
			// added to tracker
			failedTask, err = h.addTask(task, respool)
		} else {
			// This is the already present task,
			// We need to check if it has same mesos
			// id or different mesos task id.
			failedTask, err = h.requeueTask(task)
			isGangRequeued = true
		}

		// If there is any failure we need to add those tasks to
		// failed list of task by that we can remove the gang later
		if err != nil {

			failed = append(failed, failedTask)
			failedTasks[task.Id.Value] = true
		}
	}

	if len(failed) == 0 {
		if isGangRequeued {
			return nil, nil
		}
		err = h.addingGangToPendingQueue(gang, respool)
		// if there is error , we need to mark all tasks in gang failed.
		if err != nil {
			failed = append(failed, h.markingTasksFailInGang(gang, failedTasks, errFailingGangMemberTask)...)
			err = errGangNotEnqueued
		}
		return failed, err
	}
	// we need to fail the other tasks which are not failed
	// as we have to enqueue whole gang or not
	// here we are assuming that all the tasks in gang whether
	// be enqueued or requeued.
	failed = append(failed, h.markingTasksFailInGang(gang, failedTasks, errFailingGangMemberTask)...)
	err = errGangNotEnqueued

	return failed, err
}

// isTaskPresent checks if the task is present in the tracker, Returns
// True if present otherwise False
func (h *ServiceHandler) isTaskPresent(requeuedTask *resmgr.Task) bool {
	return h.rmTracker.GetTask(requeuedTask.Id) != nil
}

// removeGangFromTracker removes the  task from the tracker
func (h *ServiceHandler) removeGangFromTracker(gang *resmgrsvc.Gang) {
	for _, task := range gang.Tasks {
		h.rmTracker.DeleteTask(task.Id)
	}
}

// addingGangToPendingQueue transit all tasks of gang to PENDING state
// and add them to pending queue by that they can be scheduled for
// next scheduling cycle
func (h *ServiceHandler) addingGangToPendingQueue(gang *resmgrsvc.Gang,
	respool respool.ResPool) error {
	totalGangResources := &scalar.Resources{}
	taskFailtoTransit := false
	for _, task := range gang.GetTasks() {
		if h.rmTracker.GetTask(task.Id) != nil {
			// transiting the task from INITIALIZED State to PENDING State
			err := h.rmTracker.GetTask(task.Id).TransitTo(
				t.TaskState_PENDING.String(), statemachine.WithReason("enqueue gangs called"),
				statemachine.WithInfo(mesosTaskID, *task.GetTaskId().Value))
			if err != nil {
				taskFailtoTransit = true
				log.WithError(err).WithField("task", task.Id.Value).
					Error("not able to transit task to PENDING")
				break
			}
		}
		// Adding to gang resources as this task is been
		// successfully added to tracker
		totalGangResources = totalGangResources.Add(
			scalar.ConvertToResmgrResource(
				task.GetResource()))
	}
	// Removing the gang from the tracker if some tasks fail to transit
	if taskFailtoTransit {
		h.removeGangFromTracker(gang)
		return errGangNotEnqueued
	}
	// Adding gang to pending queue
	err := respool.EnqueueGang(gang)

	if err == nil {
		err = respool.AddToDemand(totalGangResources)
		log.WithFields(log.Fields{
			"respool_id":           respool.ID(),
			"total_gang_resources": totalGangResources,
		}).Debug("Resources added for Gang")
		if err != nil {
			log.Error(err)
		}
		return err
	}

	// We need to remove gang tasks from tracker
	h.removeGangFromTracker(gang)
	return errGangNotEnqueued
}

// markingTasksFailInGang marks all other tasks fail which are not
// part of failedTasks map and return the failed list.
func (h *ServiceHandler) markingTasksFailInGang(gang *resmgrsvc.Gang,
	failedTasks map[string]bool,
	err error,
) []*resmgrsvc.EnqueueGangsFailure_FailedTask {
	var failed []*resmgrsvc.EnqueueGangsFailure_FailedTask
	for _, task := range gang.GetTasks() {
		if _, ok := failedTasks[task.Id.Value]; !ok {
			failed = append(failed,
				&resmgrsvc.EnqueueGangsFailure_FailedTask{
					Task:      task,
					Message:   err.Error(),
					Errorcode: resmgrsvc.EnqueueGangsFailure_ENQUEUE_GANGS_FAILURE_ERROR_CODE_FAILED_DUE_TO_GANG_FAILED,
				})
		}
	}
	return failed
}

// requeueUnplacedTask is going to requeue tasks from placement engine if they could not be
// placed in prior placement attempt. The two possible paths from here is
// 1. Put this task again to Ready queue
// 2. Put this to Pending queue
// Paths will be decided based on how many ateemps is already been made for placement
func (h *ServiceHandler) requeueUnplacedTask(requeuedTask *resmgr.Task, reason string) error {
	rmTask := h.rmTracker.GetTask(requeuedTask.Id)
	if rmTask == nil {
		return nil
	}
	currentTaskState := rmTask.GetCurrentState()
	// If the task is in READY state we dont need to do anything
	if currentTaskState == t.TaskState_READY {
		return nil
	}

	// If task is not in PLACING state, it should return errUnplacedTaskInWrongState error
	if currentTaskState != t.TaskState_PLACING {
		return errUnplacedTaskInWrongState
	}
	// If task is in PLACING state we need to determine which STATE it will
	// transition to based on retry attempts

	// Checking if this task is been failed enough times
	// put this task to PENDING queue.
	if rmTask.IsFailedEnoughPlacement() {
		err := h.moveTaskForAdmission(requeuedTask, reason)
		if err != nil {
			return err
		}
		return nil
	}

	log.WithFields(log.Fields{
		"task_id":    rmTask.Task().Id.Value,
		"from_state": t.TaskState_PLACING.String(),
		"to_state":   t.TaskState_PENDING.String(),
	}).Info("Task is pushed back to pending queue from placement engine requeue")

	// Transitioning back to Ready State
	err := rmTask.TransitTo(t.TaskState_READY.String(),
		statemachine.WithReason("Previous placement timed out due to "+reason))
	if err != nil {
		log.WithField("task", rmTask.Task().Id.Value).Error(errGangNotEnqueued.Error())
		return err
	}
	// Adding to ready Queue
	gang := &resmgrsvc.Gang{
		Tasks: []*resmgr.Task{rmTask.Task()},
	}
	err = rmtask.GetScheduler().EnqueueGang(gang)
	if err != nil {
		log.WithField("gang", gang).Error(errGangNotEnqueued.Error())
		return err
	}
	return nil
}

// moveTaskForAdmission transits task to PENDING state and push it back to Pending queue
func (h *ServiceHandler) moveTaskForAdmission(requeuedTask *resmgr.Task, reason string) error {
	rmTask := h.rmTracker.GetTask(requeuedTask.Id)
	if rmTask == nil {
		return nil
	}
	// Transitioning task state to PENDING
	err := rmTask.TransitTo(t.TaskState_PENDING.String(),
		statemachine.WithReason("multiple placement timed out, putting back for readmission"+reason))
	if err != nil {
		log.WithField("task", rmTask.Task().Id.Value).Error(errGangEnqueuedPending.Error())
		return err
	}
	log.WithFields(log.Fields{
		"task_id":    rmTask.Task().Id.Value,
		"from_state": t.TaskState_PLACING.String(),
		"to_state":   t.TaskState_PENDING.String(),
	}).Info("Task is pushed back to pending queue from placement engine requeue")
	// Pushing task to PENDING queue
	err = rmTask.PushTaskForReadmission()
	if err != nil {
		return err
	}
	return nil
}

// addTask adds the task to RMTracker based on the respool
func (h *ServiceHandler) addTask(newTask *resmgr.Task, respool respool.ResPool,
) (*resmgrsvc.EnqueueGangsFailure_FailedTask, error) {
	// Adding task to state machine
	err := h.rmTracker.AddTask(
		newTask,
		h.eventStreamHandler,
		respool,
		h.config.RmTaskConfig,
	)
	if err != nil {
		return &resmgrsvc.EnqueueGangsFailure_FailedTask{
			Task:      newTask,
			Message:   err.Error(),
			Errorcode: resmgrsvc.EnqueueGangsFailure_ENQUEUE_GANGS_FAILURE_ERROR_CODE_INTERNAL,
		}, err
	}
	return nil, nil
}

// requeueTask validates the enqueued task has the same mesos task id or not
// If task has same mesos task id => return error
// If task has different mesos task id then check state and based on the state
// act accordingly
func (h *ServiceHandler) requeueTask(requeuedTask *resmgr.Task) (*resmgrsvc.EnqueueGangsFailure_FailedTask, error) {
	rmTask := h.rmTracker.GetTask(requeuedTask.Id)
	if rmTask == nil {
		return &resmgrsvc.EnqueueGangsFailure_FailedTask{
			Task:      requeuedTask,
			Message:   errRequeueTaskFailed.Error(),
			Errorcode: resmgrsvc.EnqueueGangsFailure_ENQUEUE_GANGS_FAILURE_ERROR_CODE_INTERNAL,
		}, errRequeueTaskFailed
	}
	if *requeuedTask.TaskId.Value == *rmTask.Task().TaskId.Value {
		return &resmgrsvc.EnqueueGangsFailure_FailedTask{
			Task:      requeuedTask,
			Message:   errSameTaskPresent.Error(),
			Errorcode: resmgrsvc.EnqueueGangsFailure_ENQUEUE_GANGS_FAILURE_ERROR_CODE_ALREADY_EXIST,
		}, errSameTaskPresent
	}
	currentTaskState := rmTask.GetCurrentState()

	// If state is Launching, Launched or Running then only
	// put task to ready queue with update of
	// mesos task id otherwise ignore
	if h.isTaskInTransitRunning(rmTask.GetCurrentState()) {
		// Updating the New Mesos Task ID
		rmTask.Task().TaskId = requeuedTask.TaskId
		// Transitioning back to Ready State
		rmTask.TransitTo(t.TaskState_READY.String(),
			statemachine.WithReason("waiting for placement (task updated with new mesos task id)"),
			statemachine.WithInfo(mesosTaskID, *requeuedTask.TaskId.Value))
		// Adding to ready Queue
		gang := &resmgrsvc.Gang{
			Tasks: []*resmgr.Task{rmTask.Task()},
		}
		err := rmtask.GetScheduler().EnqueueGang(gang)
		if err != nil {
			log.WithField("gang", gang).Error(errGangNotEnqueued.Error())
			return &resmgrsvc.EnqueueGangsFailure_FailedTask{
				Task:      requeuedTask,
				Message:   errSameTaskPresent.Error(),
				Errorcode: resmgrsvc.EnqueueGangsFailure_ENQUEUE_GANGS_FAILURE_ERROR_CODE_INTERNAL,
			}, err
		}
		log.WithField("gang", gang).Debug(errEnqueuedAgain.Error())
		return nil, nil

	}
	// TASK should not be in any other state other then
	// LAUNCHING, RUNNING or LAUNCHED
	// Logging error if this happens.
	log.WithFields(log.Fields{
		"task":              rmTask.Task().Id.Value,
		"current_state":     currentTaskState.String(),
		"old_mesos_task_id": *rmTask.Task().TaskId.Value,
		"new_mesos_task_id": *requeuedTask.TaskId.Value,
	}).Error("task should not be requeued with different mesos taskid at this state")

	return &resmgrsvc.EnqueueGangsFailure_FailedTask{
		Task:      requeuedTask,
		Message:   errSameTaskPresent.Error(),
		Errorcode: resmgrsvc.EnqueueGangsFailure_ENQUEUE_GANGS_FAILURE_ERROR_CODE_INTERNAL,
	}, errRequeueTaskFailed

}

// isTaskInTransitRunning return TRUE if the task state is in
// LAUNCHING, RUNNING or LAUNCHED state else it returns FALSE
func (h *ServiceHandler) isTaskInTransitRunning(state t.TaskState) bool {
	if state == t.TaskState_LAUNCHING ||
		state == t.TaskState_LAUNCHED ||
		state == t.TaskState_RUNNING {
		return true
	}
	return false
}

// DequeueGangs implements ResourceManagerService.DequeueGangs
func (h *ServiceHandler) DequeueGangs(
	ctx context.Context,
	req *resmgrsvc.DequeueGangsRequest,
) (*resmgrsvc.DequeueGangsResponse, error) {

	h.metrics.APIDequeueGangs.Inc(1)

	limit := req.GetLimit()
	timeout := time.Duration(req.GetTimeout())
	sched := rmtask.GetScheduler()

	var gangs []*resmgrsvc.Gang
	for i := uint32(0); i < limit; i++ {
		gang, err := sched.DequeueGang(timeout*time.Millisecond, req.Type)
		if err != nil {
			log.WithField("task_type", req.Type).
				Debug("Timeout to dequeue gang from ready queue")
			h.metrics.DequeueGangTimeout.Inc(1)
			break
		}
		tasksToRemove := make(map[string]*resmgr.Task)
		for _, task := range gang.GetTasks() {
			h.metrics.DequeueGangSuccess.Inc(1)

			// Moving task to Placing state
			if h.rmTracker.GetTask(task.Id) != nil {
				// Checking if placement backoff is enabled if yes add the
				// backoff otherwise just dot he transition
				if h.config.RmTaskConfig.EnablePlacementBackoff {
					//Adding backoff
					h.rmTracker.GetTask(task.Id).AddBackoff()
				}
				err = h.rmTracker.GetTask(task.Id).TransitTo(
					t.TaskState_PLACING.String())
				if err != nil {
					log.WithError(err).WithField(
						"task_id", task.Id.Value).
						Error("Failed to transit state " +
							"for task")
				}

			} else {
				tasksToRemove[task.Id.Value] = task
			}
		}
		gang = h.removeFromGang(gang, tasksToRemove)
		gangs = append(gangs, gang)
	}
	// TODO: handle the dequeue errors better
	response := resmgrsvc.DequeueGangsResponse{Gangs: gangs}
	log.WithField("response", response).Debug("DequeueGangs succeeded")
	return &response, nil
}

func (h *ServiceHandler) removeFromGang(
	gang *resmgrsvc.Gang,
	tasksToRemove map[string]*resmgr.Task) *resmgrsvc.Gang {
	if len(tasksToRemove) == 0 {
		return gang
	}
	var newTasks []*resmgr.Task
	for _, gt := range gang.GetTasks() {
		if _, ok := tasksToRemove[gt.Id.Value]; !ok {
			newTasks = append(newTasks, gt)
		}
	}
	gang.Tasks = newTasks
	return gang
}

// SetPlacements implements ResourceManagerService.SetPlacements
func (h *ServiceHandler) SetPlacements(
	ctx context.Context,
	req *resmgrsvc.SetPlacementsRequest,
) (*resmgrsvc.SetPlacementsResponse, error) {

	log.WithField("request", req).Debug("SetPlacements called.")
	h.metrics.APISetPlacements.Inc(1)

	var failed []*resmgrsvc.SetPlacementsFailure_FailedPlacement
	var err error
	for _, placement := range req.GetPlacements() {
		newplacement := h.transitTasksInPlacement(placement,
			t.TaskState_PLACING,
			t.TaskState_PLACED,
			"placement received")
		h.rmTracker.SetPlacementHost(newplacement, newplacement.Hostname)
		err = h.placements.Enqueue(newplacement)
		if err != nil {
			log.WithField("placement", newplacement).
				WithError(err).Error("Failed to enqueue placement")
			failed = append(
				failed,
				&resmgrsvc.SetPlacementsFailure_FailedPlacement{
					Placement: newplacement,
					Message:   err.Error(),
				},
			)
			h.metrics.SetPlacementFail.Inc(1)
		} else {
			h.metrics.SetPlacementSuccess.Inc(1)
		}
	}

	if len(failed) > 0 {
		return &resmgrsvc.SetPlacementsResponse{
			Error: &resmgrsvc.SetPlacementsResponse_Error{
				Failure: &resmgrsvc.SetPlacementsFailure{
					Failed: failed,
				},
			},
		}, nil
	}
	response := resmgrsvc.SetPlacementsResponse{}
	h.metrics.PlacementQueueLen.Update(float64(h.placements.Length()))
	log.Debug("Set Placement Returned")
	return &response, nil
}

// GetTasksByHosts returns all tasks of the given task type running on the given list of hosts.
func (h *ServiceHandler) GetTasksByHosts(ctx context.Context,
	req *resmgrsvc.GetTasksByHostsRequest) (*resmgrsvc.GetTasksByHostsResponse, error) {
	hostTasksMap := map[string]*resmgrsvc.TaskList{}
	for hostname, tasks := range h.rmTracker.TasksByHosts(req.Hostnames, req.Type) {
		if _, exists := hostTasksMap[hostname]; !exists {
			hostTasksMap[hostname] = &resmgrsvc.TaskList{
				Tasks: make([]*resmgr.Task, 0, len(tasks)),
			}
		}
		for _, task := range tasks {
			hostTasksMap[hostname].Tasks = append(hostTasksMap[hostname].Tasks, task.Task())
		}
	}
	res := &resmgrsvc.GetTasksByHostsResponse{
		HostTasksMap: hostTasksMap,
	}
	return res, nil
}

func (h *ServiceHandler) removeTasksFromPlacements(
	placement *resmgr.Placement,
	tasks map[string]*peloton.TaskID,
) *resmgr.Placement {
	if len(tasks) == 0 {
		return placement
	}
	var newTasks []*peloton.TaskID

	log.WithFields(log.Fields{
		"tasks_to_remove": tasks,
		"orig_tasks":      placement.GetTasks(),
	}).Debug("Removing Tasks")

	for _, pt := range placement.GetTasks() {
		if _, ok := tasks[pt.Value]; !ok {
			newTasks = append(newTasks, pt)
		}
	}
	placement.Tasks = newTasks
	return placement
}

// GetPlacements implements ResourceManagerService.GetPlacements
func (h *ServiceHandler) GetPlacements(
	ctx context.Context,
	req *resmgrsvc.GetPlacementsRequest,
) (*resmgrsvc.GetPlacementsResponse, error) {

	log.WithField("request", req).Debug("GetPlacements called.")
	h.metrics.APIGetPlacements.Inc(1)

	limit := req.GetLimit()
	timeout := time.Duration(req.GetTimeout())

	h.metrics.APIGetPlacements.Inc(1)
	var placements []*resmgr.Placement
	for i := 0; i < int(limit); i++ {
		item, err := h.placements.Dequeue(timeout * time.Millisecond)

		if err != nil {
			h.metrics.GetPlacementFail.Inc(1)
			break
		}
		placement := item.(*resmgr.Placement)
		newPlacement := h.transitTasksInPlacement(placement,
			t.TaskState_PLACED,
			t.TaskState_LAUNCHING,
			"placement dequeued, waiting for launch")
		placements = append(placements, newPlacement)
		h.metrics.GetPlacementSuccess.Inc(1)
	}

	response := resmgrsvc.GetPlacementsResponse{Placements: placements}
	h.metrics.PlacementQueueLen.Update(float64(h.placements.Length()))
	log.Debug("Get Placement Returned")

	return &response, nil
}

// transitTasksInPlacement transition to Launching upon getplacement
// or remove tasks from placement which are not in placed state.
func (h *ServiceHandler) transitTasksInPlacement(
	placement *resmgr.Placement,
	expectedState t.TaskState,
	newState t.TaskState,
	reason string) *resmgr.Placement {
	invalidTasks := make(map[string]*peloton.TaskID)
	for _, taskID := range placement.Tasks {
		rmTask := h.rmTracker.GetTask(taskID)
		if rmTask == nil {
			invalidTasks[taskID.Value] = taskID
			log.WithFields(log.Fields{
				"task_id": taskID.Value,
			}).Debug("Task is not present in tracker, " +
				"Removing it from placement")
			continue
		}
		state := rmTask.GetCurrentState()
		log.WithFields(log.Fields{
			"task_id":       taskID.Value,
			"current_state": state.String(),
		}).Debug("Get Placement for task")
		if state != expectedState {
			log.WithFields(log.Fields{
				"task_id":        taskID.GetValue(),
				"expected_state": expectedState.String(),
				"actual_state":   state.String(),
			}).Error("Unable to transit tasks in placement: " +
				"task is not in expected state")
			invalidTasks[taskID.Value] = taskID

		} else {
			err := rmTask.TransitTo(newState.String(), statemachine.WithReason(reason))
			if err != nil {
				log.WithError(errors.WithStack(err)).
					WithField("task_id", taskID.GetValue()).
					Info("not able to transition to launching for task")
				invalidTasks[taskID.Value] = taskID
			}
		}
		log.WithFields(log.Fields{
			"task_id":       taskID.Value,
			"current_state": state.String(),
		}).Debug("Latest state in Get Placement")
	}
	return h.removeTasksFromPlacements(placement, invalidTasks)
}

// NotifyTaskUpdates is called by HM to notify task updates
func (h *ServiceHandler) NotifyTaskUpdates(
	ctx context.Context,
	req *resmgrsvc.NotifyTaskUpdatesRequest) (*resmgrsvc.NotifyTaskUpdatesResponse, error) {
	var response resmgrsvc.NotifyTaskUpdatesResponse

	if len(req.Events) == 0 {
		log.Warn("Empty events received by resource manager")
		return &response, nil
	}

	for _, e := range req.Events {
		h.handleEvent(e)
	}
	response.PurgeOffset = atomic.LoadUint64(h.maxOffset)
	return &response, nil
}

func (h *ServiceHandler) handleEvent(event *pb_eventstream.Event) {
	defer h.acknowledgeEvent(event.Offset)

	taskState := util.MesosStateToPelotonState(
		event.MesosTaskStatus.GetState())
	if taskState != t.TaskState_RUNNING &&
		!util.IsPelotonStateTerminal(taskState) {
		return
	}

	ptID, err := util.ParseTaskIDFromMesosTaskID(
		*(event.MesosTaskStatus.TaskId.Value))
	if err != nil {
		log.WithFields(log.Fields{
			"event":         event,
			"mesos_task_id": *(event.MesosTaskStatus.TaskId.Value),
		}).Error("Could not parse mesos task ID")
		return
	}

	taskID := &peloton.TaskID{
		Value: ptID,
	}
	rmTask := h.rmTracker.GetTask(taskID)
	if rmTask == nil {
		return
	}

	if *(rmTask.Task().TaskId.Value) !=
		*(event.MesosTaskStatus.TaskId.Value) {
		log.WithFields(log.Fields{
			"task_id": rmTask.Task().TaskId.Value,
			"event":   event,
		}).Info("could not be updated due to" +
			"different mesos task ID")
		return
	}

	if taskState == t.TaskState_RUNNING {
		err = rmTask.TransitTo(taskState.String(), statemachine.WithReason("task running"))
		if err != nil {
			log.WithError(errors.WithStack(err)).
				WithField("task_id", ptID).
				Info("Not able to transition to RUNNING for task")
		}
		return
	}

	// TODO: We probably want to terminate all the tasks in gang
	err = rmtask.GetTracker().MarkItDone(taskID, *event.MesosTaskStatus.TaskId.Value)
	if err != nil {
		log.WithField("event", event).WithError(err).Error(
			"Could not be updated")
		return
	}

	log.WithFields(log.Fields{
		"task_id":       ptID,
		"current_state": taskState.String(),
		"mesos_task_id": rmTask.Task().TaskId.Value,
	}).Info("Task is completed and removed from tracker")
	rmtask.GetTracker().UpdateCounters(
		t.TaskState_RUNNING, taskState)

}

func (h *ServiceHandler) acknowledgeEvent(offset uint64) {
	log.WithField("offset", offset).
		Debug("Event received by resource manager")
	if offset > atomic.LoadUint64(h.maxOffset) {
		atomic.StoreUint64(h.maxOffset, offset)
	}
}

func (h *ServiceHandler) fillTaskEntry(task *rmtask.RMTask) *resmgrsvc.GetActiveTasksResponse_TaskEntry {
	taskEntry := &resmgrsvc.GetActiveTasksResponse_TaskEntry{
		TaskID:         task.Task().GetId().GetValue(),
		TaskState:      task.GetCurrentState().String(),
		Reason:         task.StateMachine().GetReason(),
		LastUpdateTime: task.StateMachine().GetLastUpdateTime().String(),
	}
	return taskEntry
}

// GetActiveTasks returns state to task entry map.
// The map can be filtered based on job id, respool id and task states.
func (h *ServiceHandler) GetActiveTasks(
	ctx context.Context,
	req *resmgrsvc.GetActiveTasksRequest,
) (*resmgrsvc.GetActiveTasksResponse, error) {
	var taskStates = map[string]*resmgrsvc.GetActiveTasksResponse_TaskEntries{}

	taskStateMap := h.rmTracker.GetActiveTasks(req.GetJobID(), req.GetRespoolID(), req.GetStates())
	for state, tasks := range taskStateMap {
		for _, task := range tasks {
			taskEntry := h.fillTaskEntry(task)
			if _, ok := taskStates[state]; !ok {
				var taskList resmgrsvc.GetActiveTasksResponse_TaskEntries
				taskStates[state] = &taskList
			}
			taskStates[state].TaskEntry = append(taskStates[state].GetTaskEntry(), taskEntry)
		}
	}

	return &resmgrsvc.GetActiveTasksResponse{TasksByState: taskStates}, nil
}

// GetPendingTasks returns the pending tasks from a resource pool in the
// order in which they were added up to a max limit number of gangs.
// Eg specifying a limit of 10 would return pending tasks from the first 10
// gangs in the queue.
// The tasks are grouped according to their gang membership since a gang is the
// unit of admission.
func (h *ServiceHandler) GetPendingTasks(
	ctx context.Context,
	req *resmgrsvc.GetPendingTasksRequest,
) (*resmgrsvc.GetPendingTasksResponse, error) {

	respoolID := req.GetRespoolID()
	limit := req.GetLimit()

	log.WithFields(log.Fields{
		"respool_id": respoolID,
		"limit":      limit,
	}).Info("GetPendingTasks called")

	if respoolID == nil {
		return &resmgrsvc.GetPendingTasksResponse{},
			status.Errorf(codes.InvalidArgument,
				"resource pool ID can't be nil")
	}

	node, err := h.resPoolTree.Get(&peloton.ResourcePoolID{
		Value: respoolID.GetValue()})
	if err != nil {
		return &resmgrsvc.GetPendingTasksResponse{},
			status.Errorf(codes.NotFound,
				"resource pool ID not found:%s", respoolID)
	}

	if !node.IsLeaf() {
		return &resmgrsvc.GetPendingTasksResponse{},
			status.Errorf(codes.InvalidArgument,
				"resource pool:%s is not a leaf node", respoolID)
	}

	// returns a list of pending resmgr.gangs for each queue
	gangsInQueue, err := h.getPendingGangs(node, limit)
	if err != nil {
		return &resmgrsvc.GetPendingTasksResponse{},
			status.Errorf(codes.Internal,
				"failed to return pending tasks, err:%s", err.Error())
	}

	// marshall the response since we only care about task ID's
	pendingGangs := make(map[string]*resmgrsvc.GetPendingTasksResponse_PendingGangs)
	for q, gangs := range gangsInQueue {
		var pendingGang []*resmgrsvc.GetPendingTasksResponse_PendingGang
		for _, gang := range gangs {
			var taskIDs []string
			for _, task := range gang.GetTasks() {
				taskIDs = append(taskIDs, task.GetId().GetValue())
			}
			pendingGang = append(pendingGang,
				&resmgrsvc.GetPendingTasksResponse_PendingGang{
					TaskIDs: taskIDs})
		}
		pendingGangs[q.String()] = &resmgrsvc.GetPendingTasksResponse_PendingGangs{
			PendingGangs: pendingGang,
		}
	}

	log.WithFields(log.Fields{
		"respool_id":    respoolID,
		"limit":         limit,
		"pending_gangs": pendingGangs,
	}).Debug("GetPendingTasks returned")

	return &resmgrsvc.GetPendingTasksResponse{
		PendingGangsByQueue: pendingGangs,
	}, nil
}

func (h *ServiceHandler) getPendingGangs(node respool.ResPool,
	limit uint32) (map[respool.QueueType][]*resmgrsvc.Gang,
	error) {

	var gangs []*resmgrsvc.Gang
	var err error

	gangsInQueue := make(map[respool.QueueType][]*resmgrsvc.Gang)

	for _, q := range []respool.QueueType{
		respool.PendingQueue,
		respool.NonPreemptibleQueue,
		respool.ControllerQueue} {
		gangs, err = node.PeekGangs(q, limit)

		if err != nil {
			if _, ok := err.(r_queue.ErrorQueueEmpty); ok {
				// queue is empty, move to the next one
				continue
			}
			return gangsInQueue, errors.Wrap(err, "failed to peek pending gangs")
		}

		gangsInQueue[q] = gangs
	}

	return gangsInQueue, nil
}

// KillTasks kills the task
func (h *ServiceHandler) KillTasks(
	ctx context.Context,
	req *resmgrsvc.KillTasksRequest,
) (*resmgrsvc.KillTasksResponse, error) {
	listTasks := req.GetTasks()
	if len(listTasks) == 0 {
		return &resmgrsvc.KillTasksResponse{
			Error: []*resmgrsvc.KillTasksResponse_Error{
				{
					NotFound: &resmgrsvc.TasksNotFound{
						Message: "Kill tasks called with no tasks",
					},
				},
			},
		}, nil
	}

	log.WithField("tasks", listTasks).Info("tasks to be killed")

	var tasksNotFound []*peloton.TaskID
	var tasksNotKilled []*peloton.TaskID
	for _, taskTobeKilled := range listTasks {
		killedRmTask := h.rmTracker.GetTask(taskTobeKilled)

		if killedRmTask == nil {
			tasksNotFound = append(tasksNotFound, taskTobeKilled)
			continue
		}

		err := h.rmTracker.MarkItInvalid(taskTobeKilled, *(killedRmTask.Task().TaskId.Value))
		if err != nil {
			tasksNotKilled = append(tasksNotKilled, taskTobeKilled)
			continue
		}
		log.WithFields(log.Fields{
			"task_id":       taskTobeKilled.Value,
			"current_state": killedRmTask.GetCurrentState().String(),
		}).Info("Task is Killed and removed from tracker")
		h.rmTracker.UpdateCounters(
			killedRmTask.GetCurrentState(),
			t.TaskState_KILLED,
		)
	}
	if len(tasksNotKilled) == 0 && len(tasksNotFound) == 0 {
		return &resmgrsvc.KillTasksResponse{}, nil
	}

	var killResponseErr []*resmgrsvc.KillTasksResponse_Error

	if len(tasksNotFound) != 0 {
		log.WithField("tasks", tasksNotFound).Error("tasks can't be found")
		for _, task := range tasksNotFound {
			killResponseErr = append(killResponseErr,
				&resmgrsvc.KillTasksResponse_Error{
					NotFound: &resmgrsvc.TasksNotFound{
						Message: "Tasks Not Found",
						Task:    task,
					},
				})
		}
	} else {
		log.WithField("tasks", tasksNotKilled).Error("tasks can't be killed")
		for _, task := range tasksNotKilled {
			killResponseErr = append(killResponseErr,
				&resmgrsvc.KillTasksResponse_Error{
					KillError: &resmgrsvc.KillTasksError{
						Message: "Tasks can't be killed",
						Task:    task,
					},
				})
		}
	}

	return &resmgrsvc.KillTasksResponse{
		Error: killResponseErr,
	}, nil
}

// GetPreemptibleTasks returns tasks which need to be preempted from the resource pool
func (h *ServiceHandler) GetPreemptibleTasks(
	ctx context.Context,
	req *resmgrsvc.GetPreemptibleTasksRequest) (*resmgrsvc.GetPreemptibleTasksResponse, error) {

	log.WithField("request", req).Debug("GetPreemptibleTasks called.")
	h.metrics.APIGetPreemptibleTasks.Inc(1)

	limit := req.GetLimit()
	timeout := time.Duration(req.GetTimeout())
	var preemptionCandidates []*resmgr.PreemptionCandidate
	for i := 0; i < int(limit); i++ {
		preemptionCandidate, err := h.preemptor.DequeueTask(timeout * time.Millisecond)
		if err != nil {
			// no more tasks
			h.metrics.GetPreemptibleTasksTimeout.Inc(1)
			break
		}

		// Transit task state machine to PREEMPTING
		if rmTask := h.rmTracker.GetTask(preemptionCandidate.Id); rmTask != nil {
			err = rmTask.TransitTo(
				t.TaskState_PREEMPTING.String(), statemachine.WithReason("preemption triggered"))
			if err != nil {
				// the task could have moved from RUNNING state
				log.WithError(err).
					WithField("task_id", preemptionCandidate.Id.Value).
					Error("failed to transit state for task")
				continue
			}
		} else {
			log.WithError(err).
				WithField("task_id", preemptionCandidate.Id.Value).
				Error("failed to find task in the tracker")
			continue
		}
		preemptionCandidates = append(preemptionCandidates, preemptionCandidate)
	}

	log.WithField("preemptible_tasks", preemptionCandidates).
		Info("GetPreemptibleTasks returned")
	h.metrics.GetPreemptibleTasksSuccess.Inc(1)
	return &resmgrsvc.GetPreemptibleTasksResponse{
		PreemptionCandidates: preemptionCandidates,
	}, nil
}

// UpdateTasksState will be called to notify the resource manager about the tasks
// which have been moved to cooresponding state , by that resource manager
// can take appropriate actions for those tasks. As an example if the tasks been
// launched then job manager will call resource manager to notify it is launched
// by that resource manager can stop timer for launching state. Similarly if
// task is been failed to be launched in host manager due to valid failure then
// job manager will tell resource manager about the task to be killed by that
// can be removed from resource manager and relevant resource accounting can be done.
func (h *ServiceHandler) UpdateTasksState(
	ctx context.Context,
	req *resmgrsvc.UpdateTasksStateRequest) (*resmgrsvc.UpdateTasksStateResponse, error) {

	taskStateList := req.GetTaskStates()
	if len(taskStateList) == 0 {
		return &resmgrsvc.UpdateTasksStateResponse{}, nil
	}

	log.WithField("task_state_list", taskStateList).
		Debug("tasks called with states")
	h.metrics.APILaunchedTasks.Inc(1)

	for _, updateEntry := range taskStateList {
		ID := updateEntry.GetTask()
		// Checking if the task is present in tracker, if not
		// drop that task to be updated
		task := h.rmTracker.GetTask(ID)
		if task == nil {
			continue
		}

		// Checking if the state for the task is in terminal state
		if util.IsPelotonStateTerminal(updateEntry.GetState()) {
			err := h.rmTracker.MarkItDone(ID, *updateEntry.MesosTaskId.Value)
			if err != nil {
				log.WithError(err).WithFields(log.Fields{
					"task_id":      ID,
					"update_entry": updateEntry,
				}).Error("could not update task")
			}
			continue
		}

		// Checking if the mesos task is same
		// otherwise drop the task
		if *task.Task().TaskId.Value != *updateEntry.GetMesosTaskId().Value {
			continue
		}
		err := task.TransitTo(updateEntry.GetState().String(),
			statemachine.WithReason(
				fmt.Sprintf("task moved to %s",
					updateEntry.GetState().String())))
		if err != nil {
			log.WithError(err).
				WithFields(log.Fields{
					"task_id":  ID,
					"to_state": updateEntry.GetState().String(),
				}).Info("failed to transit")
			continue
		}
	}
	return &resmgrsvc.UpdateTasksStateResponse{}, nil
}
