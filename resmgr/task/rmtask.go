package task

import (
	"sync"
	"time"

	"code.uber.internal/infra/peloton/.gen/peloton/api/v0/peloton"
	"code.uber.internal/infra/peloton/.gen/peloton/api/v0/task"
	"code.uber.internal/infra/peloton/.gen/peloton/private/resmgr"
	"code.uber.internal/infra/peloton/.gen/peloton/private/resmgrsvc"

	"code.uber.internal/infra/peloton/common/eventstream"
	state "code.uber.internal/infra/peloton/common/statemachine"
	"code.uber.internal/infra/peloton/resmgr/respool"
	"code.uber.internal/infra/peloton/resmgr/scalar"

	"github.com/pkg/errors"
	log "github.com/sirupsen/logrus"
)

// RunTimeStats is the container for run time stats of the res mgr task
type RunTimeStats struct {
	StartTime time.Time
}

// RMTask is the wrapper around resmgr.task for state machine
type RMTask struct {
	sync.Mutex // Mutex for synchronization

	task         *resmgr.Task       // resmgr task
	stateMachine state.StateMachine // state machine for the task

	respool             respool.ResPool      // ResPool in which this tasks belongs to
	statusUpdateHandler *eventstream.Handler // Event handler for updates

	config *Config // resmgr config object
	policy Policy  // placement retry backoff policy

	runTimeStats *RunTimeStats // run time stats for resmgr task

	// observes the state transitions of the rm task
	transitionObserver TransitionObserver
}

// CreateRMTask creates the RM task from resmgr.task
func CreateRMTask(
	t *resmgr.Task,
	handler *eventstream.Handler,
	respool respool.ResPool,
	transitionObserver TransitionObserver,
	config *Config) (*RMTask, error) {

	stateMachine, err := initStateMachine(t.GetId().GetValue(), config)
	if err != nil {
		return nil, err
	}

	r := &RMTask{
		task:                t,
		stateMachine:        stateMachine,
		statusUpdateHandler: handler,
		respool:             respool,
		config:              config,
		runTimeStats: &RunTimeStats{
			StartTime: time.Time{},
		},
		transitionObserver: transitionObserver,
	}

	// As this is when task is being created , retry should be 0
	r.Task().PlacementRetryCount = 0
	// Placement timeout should be equal to placing timeout by default
	r.Task().PlacementTimeoutSeconds = config.PlacingTimeout.Seconds()
	// Checking if placement backoff is enabled
	if !config.EnablePlacementBackoff {
		return r, nil
	}

	// Creating the backoff policy specified in config
	// and will be used for further backoff calculations.
	r.policy, err = GetFactory().CreateBackOffPolicy(config)
	if err != nil {
		return nil, err
	}
	return r, nil
}

// initStateMachine initializes the resource manager task state machine
func initStateMachine(id string, config *Config) (
	state.StateMachine, error) {

	stateMachine, err :=
		state.NewBuilder().
			WithName(id).
			WithCurrentState(state.State(task.TaskState_INITIALIZED.String())).
			WithTransitionCallback(transitionCallBack).
			AddRule(
				&state.Rule{
					From: state.State(task.TaskState_INITIALIZED.String()),
					To: []state.State{
						state.State(task.TaskState_PENDING.String()),
						// We need this transition when we want to place
						// running/launched task back to resmgr
						// as running/launching
						state.State(task.TaskState_RUNNING.String()),
						state.State(task.TaskState_LAUNCHING.String()),
						state.State(task.TaskState_LAUNCHED.String()),
					},
					Callback: nil,
				}).
			AddRule(
				&state.Rule{
					From: state.State(task.TaskState_PENDING.String()),
					To: []state.State{
						state.State(task.TaskState_READY.String()),
						state.State(task.TaskState_KILLED.String()),
						// It may happen that placement engine returns
						// just after resmgr recovery and task is still
						// in pending
						state.State(task.TaskState_PLACED.String()),
					},
					Callback: nil,
				}).
			AddRule(
				&state.Rule{
					From: state.State(task.TaskState_READY.String()),
					To: []state.State{
						state.State(task.TaskState_PLACING.String()),
						// It may happen that placement engine returns
						// just after resmgr timeout and task is still
						// in ready
						state.State(task.TaskState_PLACED.String()),
						state.State(task.TaskState_KILLED.String()),
						// This transition we need, to put back ready
						// state to pending state for in transitions
						// tasks which could not reach to ready queue
						state.State(task.TaskState_PENDING.String()),
					},
					Callback: nil,
				}).
			AddRule(
				&state.Rule{
					From: state.State(task.TaskState_PLACING.String()),
					To: []state.State{
						state.State(task.TaskState_READY.String()),
						state.State(task.TaskState_PLACED.String()),
						state.State(task.TaskState_KILLED.String()),
						// This transition is required when the task is
						// preempted while its being placed by the Placement
						// engine. If preempted it'll go back to PENDING
						// state and relinquish its resource allocation from
						// the resource pool.
						state.State(task.TaskState_PENDING.String()),
					},
					Callback: nil,
				}).
			AddRule(
				&state.Rule{
					From: state.State(task.TaskState_PLACED.String()),
					To: []state.State{
						state.State(task.TaskState_LAUNCHING.String()),
						state.State(task.TaskState_KILLED.String()),
					},
					Callback: nil,
				}).
			AddRule(
				&state.Rule{
					From: state.State(task.TaskState_LAUNCHING.String()),
					To: []state.State{
						state.State(task.TaskState_RUNNING.String()),
						state.State(task.TaskState_READY.String()),
						state.State(task.TaskState_KILLED.String()),
						state.State(task.TaskState_LAUNCHED.String()),
					},
					Callback: nil,
				}).
			AddRule(
				&state.Rule{
					From: state.State(task.TaskState_LAUNCHED.String()),
					To: []state.State{
						state.State(task.TaskState_RUNNING.String()),
						// The launch of the task may time out in job manager,
						// which will then regenerate the mesos task id, and then
						// enqueue the task again into resource manager. Since, the
						// task has already passed admission control, it will be
						// moved to the READY state.
						state.State(task.TaskState_READY.String()),
						state.State(task.TaskState_KILLED.String()),
						state.State(task.TaskState_LAUNCHED.String()),
					},
					Callback: nil,
				}).
			AddRule(
				&state.Rule{
					From: state.State(task.TaskState_RUNNING.String()),
					To: []state.State{
						state.State(task.TaskState_SUCCEEDED.String()),
						state.State(task.TaskState_LOST.String()),
						state.State(task.TaskState_PREEMPTING.String()),
						state.State(task.TaskState_KILLING.String()),
						state.State(task.TaskState_FAILED.String()),
						state.State(task.TaskState_KILLED.String()),
						state.State(task.TaskState_READY.String()),
					},
					Callback: nil,
				}).
			AddRule(
				&state.Rule{
					From: state.State(task.TaskState_FAILED.String()),
					To: []state.State{
						state.State(task.TaskState_READY.String()),
					},
					Callback: nil,
				}).
			AddRule(
				&state.Rule{
					From: state.State(task.TaskState_KILLED.String()),
					To: []state.State{
						state.State(task.TaskState_PENDING.String()),
					},
					Callback: nil,
				}).
			AddTimeoutRule(
				&state.TimeoutRule{
					From: state.State(task.TaskState_PLACING.String()),
					To: []state.State{
						state.State(task.TaskState_READY.String()),
						state.State(task.TaskState_PENDING.String()),
					},
					Timeout:     config.PlacingTimeout,
					Callback:    timeoutCallbackFromPlacing,
					PreCallback: preTimeoutCallback,
				}).
			AddTimeoutRule(
				&state.TimeoutRule{
					From: state.State(task.TaskState_LAUNCHING.String()),
					To: []state.State{
						state.State(task.TaskState_READY.String()),
					},
					Timeout:  config.LaunchingTimeout,
					Callback: timeoutCallbackFromLaunching,
				}).
			Build()
	if err != nil {
		return nil, err
	}
	return stateMachine, nil
}

// Task returns the task of the RMTask.
func (rmTask *RMTask) Task() *resmgr.Task {
	return rmTask.task
}

// StateMachine returns the state machine of the RMTask.
func (rmTask *RMTask) StateMachine() state.StateMachine {
	return rmTask.stateMachine
}

// GetCurrentState returns the current state
func (rmTask *RMTask) GetCurrentState() task.TaskState {
	return task.TaskState(
		task.TaskState_value[string(
			rmTask.stateMachine.GetCurrentState())])
}

// Respool returns the respool of the RMTask.
func (rmTask *RMTask) Respool() respool.ResPool {
	return rmTask.respool
}

// RunTimeStats returns the runtime stats of the RMTask
func (rmTask *RMTask) RunTimeStats() *RunTimeStats {
	return rmTask.runTimeStats
}

// UpdateStartTime updates the start time of the RMTask
func (rmTask *RMTask) UpdateStartTime(startTime time.Time) {
	rmTask.runTimeStats.StartTime = startTime
}

// AddBackoff adds the backoff to the RMtask based on backoff policy
func (rmTask *RMTask) AddBackoff() error {
	rmTask.Lock()
	defer rmTask.Unlock()

	// Check if policy is nil then we should return back
	if rmTask.policy == nil {
		return errors.Errorf("backoff policy is disabled %s", rmTask.Task().Id.GetValue())
	}

	// Adding the placement timeout values based on policy
	rmTask.Task().PlacementTimeoutSeconds = rmTask.config.PlacingTimeout.Seconds() +
		rmTask.policy.GetNextBackoffDuration(rmTask.Task(), rmTask.config)
	// Adding the placement retry count based on backoff policy
	rmTask.Task().PlacementRetryCount++

	// If there is no Timeout rule for PLACING state we should error out
	rule, ok := rmTask.stateMachine.GetTimeOutRules()[state.State(task.TaskState_PLACING.String())]
	if !ok {
		return errors.Errorf("could not add backoff to task %s", rmTask.Task().Id.GetValue())
	}

	// Updating the timeout rule by that next timer will start with the new time out value.
	rule.Timeout = time.Duration(rmTask.Task().PlacementTimeoutSeconds) * time.Second
	log.WithFields(log.Fields{
		"task_id":           rmTask.Task().Id.Value,
		"retry_count":       rmTask.Task().PlacementRetryCount,
		"placement_timeout": rmTask.Task().PlacementTimeoutSeconds,
	}).Info("Adding backoff to task")
	return nil
}

// IsFailedEnoughPlacement returns true if one placement cycle is completed
// otherwise false
func (rmTask *RMTask) IsFailedEnoughPlacement() bool {
	rmTask.Lock()
	defer rmTask.Unlock()
	// Checking if placement backoff is enabled
	if !rmTask.config.EnablePlacementBackoff {
		return false
	}
	return rmTask.policy.IsCycleCompleted(rmTask.Task(), rmTask.config)
}

// PushTaskForReadmission pushes the task for readmission to pending queue
func (rmTask *RMTask) PushTaskForReadmission() error {
	var tasks []*resmgr.Task
	gang := &resmgrsvc.Gang{
		Tasks: append(tasks, rmTask.task),
	}

	// pushing it to pending queue
	if err := rmTask.Respool().EnqueueGang(gang); err != nil {
		return errors.Wrapf(err, "failed to enqueue gang")
	}

	// remove allocation
	if err := rmTask.Respool().SubtractFromAllocation(scalar.GetGangAllocation(
		gang)); err != nil {
		return errors.Wrapf(err, "failed to remove allocation from respool")
	}

	// add to demand
	if err := rmTask.Respool().AddToDemand(scalar.GetGangResources(
		gang)); err != nil {
		return errors.Wrapf(err, "failed to to add to demand for respool")
	}

	return nil
}

// TransitTo transitions to the target state
func (rmTask *RMTask) TransitTo(stateTo string, options ...state.Option) error {
	if err := rmTask.stateMachine.TransitTo(state.State(stateTo),
		options...); err != nil {
		return err
	}

	GetTracker().UpdateCounters(rmTask.GetCurrentState(),
		task.TaskState(task.TaskState_value[stateTo]))
	return nil
}

// pushTaskForPlacementAgain pushes the task to ready queue as the placement cycle is not
// completed for this task.
func (rmTask *RMTask) pushTaskForPlacementAgain() error {
	var tasks []*resmgr.Task
	gang := &resmgrsvc.Gang{
		Tasks: append(tasks, rmTask.task),
	}

	err := GetScheduler().EnqueueGang(gang)
	if err != nil {
		return errors.Wrapf(err, "failed to enqueue gang")
	}

	return nil
}

// transitionCallBack is the global callback for the resource manager task
func transitionCallBack(t *state.Transition) error {
	// Sending State change event to Ready
	rmTask := GetTracker().GetTask(&peloton.TaskID{
		Value: t.StateMachine.GetName()},
	)
	rmTask.transitionObserver.Observe(t.To)

	// we only care about running state here
	if t.To == state.State(task.TaskState_RUNNING.String()) {
		// update the start time
		rmTask.UpdateStartTime(time.Now().UTC())
	}

	return nil
}

// timeoutCallback is the callback for the resource manager task
// which moving after timeout from placing/launching state to ready state
func timeoutCallbackFromPlacing(t *state.Transition) error {
	pTaskID := &peloton.TaskID{Value: t.StateMachine.GetName()}
	rmTask := GetTracker().GetTask(pTaskID)
	if rmTask == nil {
		return errors.Errorf("task is not present in the tracker %s",
			t.StateMachine.GetName())
	}

	if t.To == state.State(task.TaskState_PENDING.String()) {
		log.WithFields(log.Fields{
			"task_id":    pTaskID.Value,
			"from_state": t.From,
			"to_state":   t.To,
		}).Info("Task is pushed back to pending queue")
		// we need to push it if pending
		err := rmTask.PushTaskForReadmission()
		if err != nil {
			return err
		}
		return nil
	}

	log.WithFields(log.Fields{
		"task_id":    pTaskID.GetValue(),
		"from_state": t.From,
		"to_state":   t.To,
	}).Info("Task is pushed back to ready queue")

	err := rmTask.pushTaskForPlacementAgain()
	if err != nil {
		return err
	}

	log.WithField("task_id", pTaskID.GetValue()).
		Debug("Enqueue again due to timeout")
	return nil
}

func timeoutCallbackFromLaunching(t *state.Transition) error {
	pTaskID := &peloton.TaskID{Value: t.StateMachine.GetName()}

	rmTask := GetTracker().GetTask(pTaskID)
	if rmTask == nil {
		return errors.Errorf("task is not present in the tracker %s",
			t.StateMachine.GetName())
	}

	err := rmTask.pushTaskForPlacementAgain()
	if err != nil {
		return err
	}

	log.WithField("task_id", pTaskID.GetValue()).
		Debug("Enqueue again due to timeout")
	return nil
}

func preTimeoutCallback(t *state.Transition) error {
	pTaskID := &peloton.TaskID{Value: t.StateMachine.GetName()}
	rmTask := GetTracker().GetTask(pTaskID)
	if rmTask == nil {
		return errors.Errorf("task is not present in the tracker %s",
			t.StateMachine.GetName())
	}

	if rmTask.IsFailedEnoughPlacement() {
		t.To = state.State(task.TaskState_PENDING.String())
		return nil
	}
	t.To = state.State(task.TaskState_READY.String())
	return nil
}

// TODO : Commenting it for now to not publish yet, Until we have solution for
// event race : T936171
// updateStatus creates and send the task event to event stream
//func (rmTask *RMTask) updateStatus(status string) {
//
//
//
//
//	//t := time.Now()
//	//// Create Peloton task event
//	//taskEvent := &task.TaskEvent{
//	//	Source:    task.TaskEvent_SOURCE_RESMGR,
//	//	State:     task.TaskState(task.TaskState_value[status]),
//	//	TaskId:    rmTask.task.Id,
//	//	Timestamp: t.Format(time.RFC3339),
//	//}
//	//
//	//event := &pb_eventstream.Event{
//	//	PelotonTaskEvent: taskEvent,
//	//	Type:             pb_eventstream.Event_PELOTON_TASK_EVENT,
//	//}
//	//
//	//err := rmTask.statusUpdateHandler.AddEvent(event)
//	//if err != nil {
//	//	log.WithError(err).WithField("Event", event).
//	//		Error("Cannot add status update")
//	//}
//}
