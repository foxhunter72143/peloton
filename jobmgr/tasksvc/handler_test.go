package tasksvc

import (
	"context"
	"fmt"
	"math"
	"math/rand"
	"testing"
	"time"

	mesos "code.uber.internal/infra/peloton/.gen/mesos/v1"
	mesos_master "code.uber.internal/infra/peloton/.gen/mesos/v1/master"
	"code.uber.internal/infra/peloton/.gen/peloton/api/v0/job"
	"code.uber.internal/infra/peloton/.gen/peloton/api/v0/peloton"
	"code.uber.internal/infra/peloton/.gen/peloton/api/v0/task"
	"code.uber.internal/infra/peloton/.gen/peloton/private/hostmgr/hostsvc"
	hostmocks "code.uber.internal/infra/peloton/.gen/peloton/private/hostmgr/hostsvc/mocks"
	"code.uber.internal/infra/peloton/.gen/peloton/private/models"
	"code.uber.internal/infra/peloton/.gen/peloton/private/resmgrsvc"

	resmocks "code.uber.internal/infra/peloton/.gen/peloton/private/resmgrsvc/mocks"
	cachedmocks "code.uber.internal/infra/peloton/jobmgr/cached/mocks"
	goalstatemocks "code.uber.internal/infra/peloton/jobmgr/goalstate/mocks"
	logmanagermocks "code.uber.internal/infra/peloton/jobmgr/logmanager/mocks"
	activermtaskmocks "code.uber.internal/infra/peloton/jobmgr/task/activermtask/mocks"
	leadermocks "code.uber.internal/infra/peloton/leader/mocks"
	storemocks "code.uber.internal/infra/peloton/storage/mocks"

	"code.uber.internal/infra/peloton/jobmgr/cached"
	cachedtest "code.uber.internal/infra/peloton/jobmgr/cached/test"
	jobmgrcommon "code.uber.internal/infra/peloton/jobmgr/common"
	"code.uber.internal/infra/peloton/util"

	"github.com/golang/mock/gomock"
	"github.com/pkg/errors"
	log "github.com/sirupsen/logrus"
	"github.com/stretchr/testify/suite"
	"github.com/uber-go/tally"
	"go.uber.org/yarpc/yarpcerrors"
)

const (
	testInstanceCount = 4
	testJob           = "941ff353-ba82-49fe-8f80-fb5bc649b04d"
)

type TaskHandlerTestSuite struct {
	suite.Suite
	handler        *serviceHandler
	testJobID      *peloton.JobID
	testJobConfig  *job.JobConfig
	testJobRuntime *job.RuntimeInfo
	taskInfos      map[uint32]*task.TaskInfo

	ctrl                     *gomock.Controller
	mockedCandidate          *leadermocks.MockCandidate
	mockedResmgrClient       *resmocks.MockResourceManagerServiceYARPCClient
	mockedJobFactory         *cachedmocks.MockJobFactory
	mockedCachedJob          *cachedmocks.MockJob
	mockedCachedTask         *cachedmocks.MockTask
	mockedGoalStateDrive     *goalstatemocks.MockDriver
	mockedJobStore           *storemocks.MockJobStore
	mockedTaskStore          *storemocks.MockTaskStore
	mockedUpdateStore        *storemocks.MockUpdateStore
	mockedFrameworkInfoStore *storemocks.MockFrameworkInfoStore
	mockedLogManager         *logmanagermocks.MockLogManager
	mockedHostMgr            *hostmocks.MockInternalHostServiceYARPCClient
	mockedTask               *cachedmocks.MockTask
	mockedActiveRMTasks      *activermtaskmocks.MockActiveRMTasks
}

func (suite *TaskHandlerTestSuite) SetupTest() {
	mtx := NewMetrics(tally.NoopScope)
	suite.handler = &serviceHandler{
		metrics: mtx,
	}
	suite.testJobID = &peloton.JobID{
		Value: testJob,
	}
	suite.testJobConfig = &job.JobConfig{
		Name:          suite.testJobID.Value,
		InstanceCount: testInstanceCount,
		Type:          job.JobType_BATCH,
	}
	suite.testJobRuntime = &job.RuntimeInfo{
		State:     job.JobState_RUNNING,
		GoalState: job.JobState_SUCCEEDED,
	}
	var taskInfos = make(map[uint32]*task.TaskInfo)
	for i := uint32(0); i < testInstanceCount; i++ {
		taskInfos[i] = suite.createTestTaskInfo(
			task.TaskState_RUNNING, i)
	}
	suite.taskInfos = taskInfos

	suite.ctrl = gomock.NewController(suite.T())
	suite.mockedJobStore = storemocks.NewMockJobStore(suite.ctrl)
	suite.mockedJobFactory = cachedmocks.NewMockJobFactory(suite.ctrl)
	suite.mockedCachedJob = cachedmocks.NewMockJob(suite.ctrl)
	suite.mockedCachedTask = cachedmocks.NewMockTask(suite.ctrl)
	suite.mockedGoalStateDrive = goalstatemocks.NewMockDriver(suite.ctrl)
	suite.mockedResmgrClient = resmocks.NewMockResourceManagerServiceYARPCClient(suite.ctrl)
	suite.mockedCandidate = leadermocks.NewMockCandidate(suite.ctrl)
	suite.mockedTaskStore = storemocks.NewMockTaskStore(suite.ctrl)
	suite.mockedUpdateStore = storemocks.NewMockUpdateStore(suite.ctrl)
	suite.mockedFrameworkInfoStore = storemocks.NewMockFrameworkInfoStore(suite.ctrl)
	suite.mockedLogManager = logmanagermocks.NewMockLogManager(suite.ctrl)
	suite.mockedHostMgr = hostmocks.NewMockInternalHostServiceYARPCClient(suite.ctrl)
	suite.mockedTask = cachedmocks.NewMockTask(suite.ctrl)
	suite.mockedActiveRMTasks = activermtaskmocks.NewMockActiveRMTasks(suite.ctrl)

	suite.handler.jobStore = suite.mockedJobStore
	suite.handler.taskStore = suite.mockedTaskStore
	suite.handler.updateStore = suite.mockedUpdateStore
	suite.handler.jobFactory = suite.mockedJobFactory
	suite.handler.goalStateDriver = suite.mockedGoalStateDrive
	suite.handler.resmgrClient = suite.mockedResmgrClient
	suite.handler.candidate = suite.mockedCandidate
	suite.handler.frameworkInfoStore = suite.mockedFrameworkInfoStore
	suite.handler.logManager = suite.mockedLogManager
	suite.handler.hostMgrClient = suite.mockedHostMgr
	suite.handler.activeRMTasks = suite.mockedActiveRMTasks
}

func (suite *TaskHandlerTestSuite) TearDownTest() {
	log.Debug("tearing down")
	suite.ctrl.Finish()
	suite.handler.mesosAgentWorkDir = ""
}

func TestPelotonTaskHandler(t *testing.T) {
	suite.Run(t, new(TaskHandlerTestSuite))
}

func (suite *TaskHandlerTestSuite) createTestTaskInfo(
	state task.TaskState,
	instanceID uint32) *task.TaskInfo {

	var taskID = fmt.Sprintf("%s-%d-%d", suite.testJobID.Value, instanceID, rand.Int31())
	return &task.TaskInfo{
		Runtime: &task.RuntimeInfo{
			MesosTaskId: &mesos.TaskID{Value: &taskID},
			State:       state,
			GoalState:   task.TaskState_SUCCEEDED,
		},
		Config: &task.TaskConfig{
			RestartPolicy: &task.RestartPolicy{
				MaxFailures: 3,
			},
		},
		InstanceId: instanceID,
		JobId:      suite.testJobID,
	}
}

func (suite *TaskHandlerTestSuite) createTestTaskEvents() []*task.TaskEvent {
	var taskID0 = fmt.Sprintf("%s-%d", suite.testJobID.Value, 0)
	var taskID1 = fmt.Sprintf("%s-%d", suite.testJobID.Value, 1)
	return []*task.TaskEvent{
		{
			TaskId: &peloton.TaskID{
				Value: taskID0,
			},
			State:     task.TaskState_INITIALIZED,
			Message:   "",
			Timestamp: "2017-12-11T22:17:26Z",
			Hostname:  "peloton-test-host",
			Reason:    "",
		},
		{
			TaskId: &peloton.TaskID{
				Value: taskID1,
			},
			State:     task.TaskState_INITIALIZED,
			Message:   "",
			Timestamp: "2017-12-11T22:17:46Z",
			Hostname:  "peloton-test-host-1",
			Reason:    "",
		},
		{
			TaskId: &peloton.TaskID{
				Value: taskID0,
			},
			State:     task.TaskState_FAILED,
			Message:   "",
			Timestamp: "2017-12-11T22:17:36Z",
			Hostname:  "peloton-test-host",
			Reason:    "",
		},
		{
			TaskId: &peloton.TaskID{
				Value: taskID1,
			},
			State:     task.TaskState_LAUNCHED,
			Message:   "",
			Timestamp: "2017-12-11T22:17:50Z",
			Hostname:  "peloton-test-host-1",
			Reason:    "",
		},
		{
			TaskId: &peloton.TaskID{
				Value: taskID1,
			},
			State:     task.TaskState_RUNNING,
			Message:   "",
			Timestamp: "2017-12-11T22:17:56Z",
			Hostname:  "peloton-test-host-1",
			Reason:    "",
		},
	}
}

func (suite *TaskHandlerTestSuite) TestGetTaskInfosByRangesFromDBReturnsError() {
	jobID := &peloton.JobID{}

	suite.mockedTaskStore.EXPECT().GetTasksForJob(gomock.Any(), jobID).Return(nil, errors.New("my-error"))
	_, err := suite.handler.getTaskInfosByRangesFromDB(context.Background(), jobID, nil)

	suite.EqualError(err, "my-error")
}

func (suite *TaskHandlerTestSuite) createTaskEventForGetTasks(instanceID uint32, taskRuns uint32) ([]*task.TaskEvent, []*task.TaskEvent) {
	var events []*task.TaskEvent
	var getReturnEvents []*task.TaskEvent
	taskInfos := make([]*task.TaskInfo, taskRuns)
	for i := uint32(0); i < taskRuns; i++ {
		var prevTaskID *peloton.TaskID
		taskInfos[i] = suite.createTestTaskInfo(task.TaskState_FAILED, instanceID)
		if i > uint32(0) {
			prevTaskID = &peloton.TaskID{
				Value: taskInfos[i-1].GetRuntime().GetMesosTaskId().GetValue(),
			}
		}

		event := &task.TaskEvent{
			TaskId: &peloton.TaskID{
				Value: taskInfos[i].GetRuntime().GetMesosTaskId().GetValue(),
			},
			PrevTaskId: prevTaskID,
			State:      task.TaskState_PENDING,
			Message:    "",
			Timestamp:  "2017-12-11T22:17:26Z",
			Hostname:   "peloton-test-host",
			Reason:     "",
		}
		events = append(events, event)

		event = &task.TaskEvent{
			TaskId: &peloton.TaskID{
				Value: taskInfos[i].GetRuntime().GetMesosTaskId().GetValue(),
			},
			PrevTaskId: prevTaskID,
			State:      task.TaskState_RUNNING,
			Message:    "",
			Timestamp:  "2017-12-11T22:17:26Z",
			Hostname:   "peloton-test-host",
			Reason:     "",
		}
		events = append(events, event)

		if i == uint32(0) {
			event = &task.TaskEvent{
				TaskId: &peloton.TaskID{
					Value: taskInfos[i].GetRuntime().GetMesosTaskId().GetValue(),
				},
				PrevTaskId: prevTaskID,
				State:      task.TaskState_FAILED,
				Message:    "",
				Timestamp:  "2017-12-11T22:17:26Z",
				Hostname:   "peloton-test-host",
				Reason:     "",
			}
		} else {
			event = &task.TaskEvent{
				TaskId: &peloton.TaskID{
					Value: taskInfos[i].GetRuntime().GetMesosTaskId().GetValue(),
				},
				PrevTaskId: prevTaskID,
				State:      task.TaskState_FAILED,
				Message:    "",
				Timestamp:  "2017-12-11T22:17:26Z",
				Hostname:   "peloton-test-host",
				Reason:     "",
				AgentId:    "peloton-test-agent",
			}
		}

		events = append(events, event)
		getReturnEvents = append(getReturnEvents, event)
	}

	return events, getReturnEvents
}

func (suite *TaskHandlerTestSuite) TestGetTasks_Batch_Job() {
	instanceID := uint32(0)
	taskRuns := uint32(3)
	lastTaskInfo := suite.createTestTaskInfo(task.TaskState_FAILED, instanceID)
	taskInfoMap := make(map[uint32]*task.TaskInfo)
	taskInfoMap[instanceID] = lastTaskInfo
	events, _ := suite.createTaskEventForGetTasks(instanceID, taskRuns)
	suite.testJobConfig.Type = job.JobType_BATCH

	gomock.InOrder(
		suite.mockedJobFactory.EXPECT().GetJob(suite.testJobID).Return(suite.mockedCachedJob),
		suite.mockedCachedJob.EXPECT().
			GetConfig(gomock.Any()).
			Return(cachedtest.NewMockJobConfig(suite.ctrl, suite.testJobConfig), nil),
		suite.mockedTaskStore.EXPECT().
			GetTaskForJob(gomock.Any(), suite.testJobID, instanceID).Return(taskInfoMap, nil),
		suite.mockedTaskStore.EXPECT().
			GetTaskEvents(gomock.Any(), suite.testJobID, instanceID).Return(events, nil),
	)

	var req = &task.GetRequest{
		JobId:      suite.testJobID,
		InstanceId: instanceID,
	}

	resp, err := suite.handler.Get(context.Background(), req)
	suite.NoError(err)
	suite.Equal(uint32(len(resp.Results)), taskRuns)
	for _, result := range resp.Results {
		suite.Equal(result.GetRuntime().GetState(), task.TaskState_FAILED)
	}
}

func (suite *TaskHandlerTestSuite) TestGetTasks_Service_Job() {
	instanceID := uint32(0)
	lastTaskInfo := suite.createTestTaskInfo(task.TaskState_FAILED, instanceID)
	taskInfoMap := make(map[uint32]*task.TaskInfo)
	taskInfoMap[instanceID] = lastTaskInfo
	suite.testJobConfig.Type = job.JobType_SERVICE

	gomock.InOrder(
		suite.mockedJobFactory.EXPECT().GetJob(suite.testJobID).Return(suite.mockedCachedJob),
		suite.mockedCachedJob.EXPECT().
			GetConfig(gomock.Any()).
			Return(cachedtest.NewMockJobConfig(suite.ctrl, suite.testJobConfig), nil),
		suite.mockedTaskStore.EXPECT().
			GetTaskForJob(gomock.Any(), suite.testJobID, instanceID).Return(taskInfoMap, nil),
	)

	var req = &task.GetRequest{
		JobId:      suite.testJobID,
		InstanceId: instanceID,
	}

	resp, err := suite.handler.Get(context.Background(), req)
	suite.NoError(err)
	suite.Len(resp.Results, 0)
	for _, result := range resp.Results {
		suite.Equal(result.GetRuntime().GetState(), task.TaskState_FAILED)
	}
}

func (suite *TaskHandlerTestSuite) TestGetTasks_FailToGetConfig() {
	gomock.InOrder(
		suite.mockedJobFactory.EXPECT().GetJob(suite.testJobID).Return(suite.mockedCachedJob),
		suite.mockedCachedJob.EXPECT().
			GetConfig(gomock.Any()).
			Return(nil, fmt.Errorf("test error")),
	)

	var req = &task.GetRequest{
		JobId:      suite.testJobID,
		InstanceId: uint32(0),
	}

	resp, err := suite.handler.Get(context.Background(), req)
	suite.NoError(err)
	suite.NotNil(resp.GetNotFound())
}

func (suite *TaskHandlerTestSuite) TestGetTasks_GetTaskFail() {
	instanceID := uint32(0)
	lastTaskInfo := suite.createTestTaskInfo(task.TaskState_FAILED, instanceID)
	taskInfoMap := make(map[uint32]*task.TaskInfo)
	taskInfoMap[instanceID] = lastTaskInfo
	suite.testJobConfig.Type = job.JobType_SERVICE

	gomock.InOrder(
		suite.mockedJobFactory.EXPECT().GetJob(suite.testJobID).Return(suite.mockedCachedJob),
		suite.mockedCachedJob.EXPECT().
			GetConfig(gomock.Any()).
			Return(cachedtest.NewMockJobConfig(suite.ctrl, suite.testJobConfig), nil),
		suite.mockedTaskStore.EXPECT().
			GetTaskForJob(gomock.Any(), suite.testJobID, instanceID).Return(nil, fmt.Errorf("test err")),
	)

	var req = &task.GetRequest{
		JobId:      suite.testJobID,
		InstanceId: instanceID,
	}

	resp, err := suite.handler.Get(context.Background(), req)
	suite.NoError(err)
	suite.NotNil(resp.GetOutOfRange())
}

func (suite *TaskHandlerTestSuite) TestGetTasks_FailToGetTaskEvents() {
	instanceID := uint32(0)
	lastTaskInfo := suite.createTestTaskInfo(task.TaskState_FAILED, instanceID)
	taskInfoMap := make(map[uint32]*task.TaskInfo)
	taskInfoMap[instanceID] = lastTaskInfo
	suite.testJobConfig.Type = job.JobType_BATCH

	gomock.InOrder(
		suite.mockedJobFactory.EXPECT().GetJob(suite.testJobID).Return(suite.mockedCachedJob),
		suite.mockedCachedJob.EXPECT().
			GetConfig(gomock.Any()).
			Return(cachedtest.NewMockJobConfig(suite.ctrl, suite.testJobConfig), nil),
		suite.mockedTaskStore.EXPECT().
			GetTaskForJob(gomock.Any(), suite.testJobID, instanceID).Return(taskInfoMap, nil),
		suite.mockedTaskStore.EXPECT().
			GetTaskEvents(gomock.Any(), suite.testJobID, instanceID).Return(nil, fmt.Errorf("test error")),
	)

	var req = &task.GetRequest{
		JobId:      suite.testJobID,
		InstanceId: instanceID,
	}

	resp, err := suite.handler.Get(context.Background(), req)
	suite.NoError(err)
	suite.NotNil(resp.GetOutOfRange())
}

func (suite *TaskHandlerTestSuite) TestStopAllTasks() {
	expectedTaskIds := make(map[*mesos.TaskID]bool)
	for _, taskInfo := range suite.taskInfos {
		expectedTaskIds[taskInfo.GetRuntime().GetMesosTaskId()] = true
	}

	expectedJobRuntime := &job.RuntimeInfo{
		GoalState: job.JobState_KILLED,
	}

	expectedJobInfo := &job.JobInfo{
		Runtime: expectedJobRuntime,
	}

	gomock.InOrder(
		suite.mockedCandidate.EXPECT().IsLeader().Return(true),
		suite.mockedJobFactory.EXPECT().
			AddJob(suite.testJobID).
			Return(suite.mockedCachedJob),
		suite.mockedCachedJob.EXPECT().
			GetConfig(gomock.Any()).
			Return(cachedtest.NewMockJobConfig(suite.ctrl, suite.testJobConfig), nil),
		suite.mockedJobFactory.EXPECT().
			AddJob(suite.testJobID).
			Return(suite.mockedCachedJob),
		suite.mockedCachedJob.EXPECT().
			GetRuntime(gomock.Any()).
			Return(suite.testJobRuntime, nil),
		suite.mockedCachedJob.EXPECT().
			Update(gomock.Any(), expectedJobInfo, gomock.Any(), cached.UpdateCacheAndDB).
			Return(nil),
		suite.mockedGoalStateDrive.EXPECT().
			EnqueueJob(suite.testJobID, gomock.Any()).Return(),
	)

	var request = &task.StopRequest{
		JobId: suite.testJobID,
	}
	resp, err := suite.handler.Stop(
		context.Background(),
		request,
	)
	suite.NoError(err)
	suite.Equal(len(resp.GetInvalidInstanceIds()), 0)
	suite.Equal(len(resp.GetStoppedInstanceIds()), testInstanceCount)
}

func (suite *TaskHandlerTestSuite) TestStopTasksWithRanges() {
	singleTaskInfo := make(map[uint32]*task.TaskInfo)
	singleTaskInfo[1] = suite.taskInfos[1]

	taskRanges := []*task.InstanceRange{
		{
			From: 1,
			To:   2,
		},
	}

	gomock.InOrder(
		suite.mockedCandidate.EXPECT().IsLeader().Return(true),
		suite.mockedJobFactory.EXPECT().
			AddJob(suite.testJobID).Return(suite.mockedCachedJob),
		suite.mockedCachedJob.EXPECT().
			GetConfig(gomock.Any()).
			Return(cachedtest.NewMockJobConfig(suite.ctrl, suite.testJobConfig), nil),
		suite.mockedTaskStore.EXPECT().
			GetTasksForJobByRange(gomock.Any(), suite.testJobID, taskRanges[0]).Return(singleTaskInfo, nil),
		suite.mockedCachedJob.EXPECT().
			PatchTasks(gomock.Any(), gomock.Any()).Return(nil),
		suite.mockedGoalStateDrive.EXPECT().
			EnqueueTask(suite.testJobID, uint32(1), gomock.Any()).Return(),
		suite.mockedCachedJob.EXPECT().GetJobType().Return(job.JobType_BATCH),
		suite.mockedGoalStateDrive.EXPECT().
			JobRuntimeDuration(job.JobType_BATCH).
			Return(1*time.Second),
		suite.mockedGoalStateDrive.EXPECT().
			EnqueueJob(suite.testJobID, gomock.Any()).Return(),
	)

	var request = &task.StopRequest{
		JobId:  suite.testJobID,
		Ranges: taskRanges,
	}
	resp, err := suite.handler.Stop(
		context.Background(),
		request,
	)
	suite.NoError(err)
	suite.Equal(len(resp.GetInvalidInstanceIds()), 0)
	suite.Equal(resp.GetStoppedInstanceIds(), []uint32{1})
}

func (suite *TaskHandlerTestSuite) TestStopTasksSkipKillNotRunningTask() {
	taskInfos := make(map[uint32]*task.TaskInfo)
	taskInfos[1] = suite.taskInfos[1]
	taskInfos[2] = suite.createTestTaskInfo(task.TaskState_FAILED, uint32(2))

	taskRanges := []*task.InstanceRange{
		{
			From: 1,
			To:   3,
		},
	}

	gomock.InOrder(
		suite.mockedCandidate.EXPECT().IsLeader().Return(true),
		suite.mockedJobFactory.EXPECT().
			AddJob(suite.testJobID).Return(suite.mockedCachedJob),
		suite.mockedCachedJob.EXPECT().
			GetConfig(gomock.Any()).Return(cachedtest.NewMockJobConfig(suite.ctrl, suite.testJobConfig), nil),
		suite.mockedTaskStore.EXPECT().
			GetTasksForJobByRange(gomock.Any(), suite.testJobID, taskRanges[0]).Return(taskInfos, nil),
		suite.mockedCachedJob.EXPECT().
			PatchTasks(gomock.Any(), gomock.Any()).Return(nil),
	)

	suite.mockedGoalStateDrive.EXPECT().
		EnqueueTask(suite.testJobID, uint32(1), gomock.Any()).Return()

	suite.mockedGoalStateDrive.EXPECT().
		EnqueueTask(suite.testJobID, uint32(2), gomock.Any()).Return()

	suite.mockedCachedJob.EXPECT().GetJobType().Return(job.JobType_BATCH)

	suite.mockedGoalStateDrive.EXPECT().
		JobRuntimeDuration(job.JobType_BATCH).
		Return(1 * time.Second)

	suite.mockedGoalStateDrive.EXPECT().
		EnqueueJob(suite.testJobID, gomock.Any()).Return()

	var request = &task.StopRequest{
		JobId:  suite.testJobID,
		Ranges: taskRanges,
	}
	resp, err := suite.handler.Stop(
		context.Background(),
		request,
	)
	suite.NoError(err)
	suite.Equal(len(resp.GetInvalidInstanceIds()), 0)
	suite.Equal(len(resp.GetStoppedInstanceIds()), 2)
}

func (suite *TaskHandlerTestSuite) TestStopTasksWithInvalidRanges() {
	singleTaskInfo := make(map[uint32]*task.TaskInfo)
	singleTaskInfo[1] = suite.taskInfos[1]
	emptyTaskInfo := make(map[uint32]*task.TaskInfo)

	taskRanges := []*task.InstanceRange{
		{
			From: 1,
			To:   2,
		},
		{
			From: 5,
			To:   math.MaxInt32 + 1, // To should not go beyond MaxInt32
		},
	}
	correctedRange := &task.InstanceRange{
		From: 5,
		To:   math.MaxInt32,
	}

	gomock.InOrder(
		suite.mockedCandidate.EXPECT().IsLeader().Return(true),
		suite.mockedJobFactory.EXPECT().
			AddJob(gomock.Any()).Return(suite.mockedCachedJob),
		suite.mockedCachedJob.EXPECT().
			GetConfig(gomock.Any()).Return(cachedtest.NewMockJobConfig(suite.ctrl, suite.testJobConfig), nil),
		suite.mockedTaskStore.EXPECT().
			GetTasksForJobByRange(gomock.Any(), suite.testJobID, taskRanges[0]).Return(singleTaskInfo, nil),
		suite.mockedTaskStore.EXPECT().
			GetTasksForJobByRange(gomock.Any(), suite.testJobID, correctedRange).
			Return(emptyTaskInfo, errors.New("test error")),
	)

	var request = &task.StopRequest{
		JobId:  suite.testJobID,
		Ranges: taskRanges,
	}
	resp, err := suite.handler.Stop(
		context.Background(),
		request,
	)
	suite.NoError(err)
	suite.Equal(len(resp.GetStoppedInstanceIds()), 0)
	suite.Equal(resp.GetError().GetOutOfRange().GetJobId().GetValue(), testJob)
	suite.Equal(
		resp.GetError().GetOutOfRange().GetInstanceCount(),
		uint32(testInstanceCount))
}

func (suite *TaskHandlerTestSuite) TestStopTasksWithInvalidJobID() {
	singleTaskInfo := make(map[uint32]*task.TaskInfo)
	singleTaskInfo[1] = suite.taskInfos[1]
	gomock.InOrder(
		suite.mockedCandidate.EXPECT().IsLeader().Return(true),
		suite.mockedJobFactory.EXPECT().AddJob(suite.testJobID).Return(suite.mockedCachedJob),
		suite.mockedCachedJob.EXPECT().GetConfig(gomock.Any()).Return(nil, errors.New("test error")),
	)

	var request = &task.StopRequest{
		JobId: suite.testJobID,
	}
	resp, err := suite.handler.Stop(
		context.Background(),
		request,
	)
	suite.NoError(err)
	suite.Equal(resp.GetError().GetNotFound().GetId().GetValue(), testJob)
	suite.Equal(len(resp.GetInvalidInstanceIds()), 0)
	suite.Equal(len(resp.GetStoppedInstanceIds()), 0)
}

func (suite *TaskHandlerTestSuite) TestStopAllTasksWithUpdateFailure() {
	gomock.InOrder(
		suite.mockedCandidate.EXPECT().IsLeader().Return(true),
		suite.mockedJobFactory.EXPECT().
			AddJob(suite.testJobID).Return(suite.mockedCachedJob),
		suite.mockedCachedJob.EXPECT().
			GetConfig(gomock.Any()).Return(cachedtest.NewMockJobConfig(suite.ctrl, suite.testJobConfig), nil),
		suite.mockedJobFactory.EXPECT().
			AddJob(suite.testJobID).Return(suite.mockedCachedJob),
		suite.mockedCachedJob.EXPECT().
			GetRuntime(gomock.Any()).
			Return(suite.testJobRuntime, nil),
		suite.mockedCachedJob.EXPECT().
			Update(gomock.Any(), gomock.Any(), gomock.Any(), cached.UpdateCacheAndDB).Return(fmt.Errorf("db update failure")),
	)

	var request = &task.StopRequest{
		JobId: suite.testJobID,
	}
	resp, err := suite.handler.Stop(
		context.Background(),
		request,
	)
	suite.NoError(err)
	suite.Equal(len(resp.GetInvalidInstanceIds()), testInstanceCount)
	suite.Equal(len(resp.GetStoppedInstanceIds()), 0)
	suite.NotNil(resp.GetError().GetUpdateError())
}

// TestStopTasks_NonLeader tests stop tasks on non-leader node
func (suite *TaskHandlerTestSuite) TestStopTasks_NonLeader() {
	suite.mockedCandidate.EXPECT().IsLeader().Return(false)

	var request = &task.StopRequest{
		JobId: suite.testJobID,
	}
	resp, err := suite.handler.Stop(
		context.Background(),
		request,
	)
	suite.Error(err)
	suite.Nil(resp)
}

// TestStopTasks_PatchFailure tests stop tasks when patch tasks fail
func (suite *TaskHandlerTestSuite) TestStopTasks_PatchFailure() {
	singleTaskInfo := make(map[uint32]*task.TaskInfo)
	singleTaskInfo[1] = suite.taskInfos[1]

	taskRanges := []*task.InstanceRange{
		{
			From: 1,
			To:   2,
		},
	}

	gomock.InOrder(
		suite.mockedCandidate.EXPECT().IsLeader().Return(true),
		suite.mockedJobFactory.EXPECT().
			AddJob(suite.testJobID).Return(suite.mockedCachedJob),
		suite.mockedCachedJob.EXPECT().
			GetConfig(gomock.Any()).
			Return(cachedtest.NewMockJobConfig(suite.ctrl, suite.testJobConfig), nil),
		suite.mockedTaskStore.EXPECT().
			GetTasksForJobByRange(gomock.Any(), suite.testJobID, taskRanges[0]).Return(singleTaskInfo, nil),
		suite.mockedCachedJob.EXPECT().
			PatchTasks(gomock.Any(), gomock.Any()).Return(errors.New("test error")),
		suite.mockedCachedJob.EXPECT().GetJobType().Return(job.JobType_BATCH),
		suite.mockedGoalStateDrive.EXPECT().
			JobRuntimeDuration(job.JobType_BATCH).
			Return(1*time.Second),
		suite.mockedGoalStateDrive.EXPECT().
			EnqueueJob(suite.testJobID, gomock.Any()).Return(),
	)

	var request = &task.StopRequest{
		JobId:  suite.testJobID,
		Ranges: taskRanges,
	}
	resp, err := suite.handler.Stop(
		context.Background(),
		request,
	)
	suite.NoError(err)
	suite.NotNil(resp.GetError())
}

func (suite *TaskHandlerTestSuite) TestStartAllTasks() {
	expectedTaskIds := make(map[*mesos.TaskID]bool)
	runningInstanceID := uint32(3)
	for _, taskInfo := range suite.taskInfos {
		expectedTaskIds[taskInfo.GetRuntime().GetMesosTaskId()] = true
	}

	var taskInfos = make(map[uint32]*task.TaskInfo)
	var tasksInfoList []*task.TaskInfo
	for i := uint32(0); i < testInstanceCount; i++ {
		taskInfos[i] = suite.createTestTaskInfo(
			task.TaskState_FAILED, i)
		taskInfos[i].Runtime.GoalState = task.TaskState_KILLED
		tasksInfoList = append(tasksInfoList, taskInfos[i])
	}
	// one of them is not killed, so it should not get started
	taskInfos[runningInstanceID] = suite.createTestTaskInfo(
		task.TaskState_RUNNING,
		runningInstanceID,
	)

	gomock.InOrder(
		suite.mockedCandidate.EXPECT().IsLeader().Return(true),
		suite.mockedJobFactory.EXPECT().
			AddJob(suite.testJobID).Return(suite.mockedCachedJob),
		suite.mockedCachedJob.EXPECT().
			GetConfig(gomock.Any()).Return(cachedtest.NewMockJobConfig(suite.ctrl, suite.testJobConfig), nil),
		suite.mockedCachedJob.EXPECT().
			GetRuntime(gomock.Any()).
			Return(suite.testJobRuntime, nil),
		suite.mockedCachedJob.EXPECT().
			CompareAndSetRuntime(gomock.Any(), gomock.Any()).
			Do(func(_ context.Context, jobRuntime *job.RuntimeInfo) {
				suite.Equal(job.JobState_PENDING, jobRuntime.GetState())
				suite.Equal(job.JobState_SUCCEEDED, jobRuntime.GetGoalState())
			}).
			Return(&job.RuntimeInfo{}, nil),
		suite.mockedTaskStore.EXPECT().
			GetTasksForJob(gomock.Any(), suite.testJobID).Return(taskInfos, nil),
	)

	for i := uint32(0); i < testInstanceCount; i++ {
		suite.mockedCachedJob.EXPECT().
			AddTask(gomock.Any(), i).
			Return(suite.mockedCachedTask, nil)
		suite.mockedCachedTask.EXPECT().
			GetRunTime(gomock.Any()).
			Return(taskInfos[i].Runtime, nil)
		if i != runningInstanceID {
			suite.mockedCachedTask.EXPECT().
				CompareAndSetRuntime(gomock.Any(), gomock.Any(), gomock.Any()).
				Do(func(_ context.Context, runtime *task.RuntimeInfo, _ job.JobType) {
					suite.Equal(runtime.State, task.TaskState_INITIALIZED)
					suite.Equal(runtime.Healthy, task.HealthState_DISABLED)
					suite.Equal(runtime.GoalState, task.TaskState_SUCCEEDED)
				}).Return(nil, nil)
		}
	}

	suite.mockedGoalStateDrive.EXPECT().
		EnqueueTask(suite.testJobID, gomock.Any(), gomock.Any()).Return().AnyTimes()

	suite.mockedCachedJob.EXPECT().GetJobType().Return(job.JobType_BATCH)

	suite.mockedGoalStateDrive.EXPECT().
		JobRuntimeDuration(job.JobType_BATCH).
		Return(1 * time.Second)

	suite.mockedGoalStateDrive.EXPECT().
		EnqueueJob(suite.testJobID, gomock.Any()).Return()

	var request = &task.StartRequest{
		JobId: suite.testJobID,
	}
	resp, err := suite.handler.Start(
		context.Background(),
		request,
	)

	suite.NoError(err)
	suite.Nil(resp.GetError())
	suite.Equal(len(resp.GetInvalidInstanceIds()), 0)
	suite.Equal(len(resp.GetStartedInstanceIds()), testInstanceCount-1)
}

// TestStartTasks_NonLeader tests call Start on a non leader node
func (suite *TaskHandlerTestSuite) TestStartTasks_NonLeader() {
	suite.mockedCandidate.EXPECT().IsLeader().Return(false)
	var request = &task.StartRequest{
		JobId: suite.testJobID,
	}
	resp, err := suite.handler.Start(
		context.Background(),
		request,
	)

	suite.Error(err)
	suite.Nil(resp)
}

func (suite *TaskHandlerTestSuite) TestStartTasks_GetConfigFailure() {
	gomock.InOrder(
		suite.mockedCandidate.EXPECT().IsLeader().Return(true),
		suite.mockedJobFactory.EXPECT().
			AddJob(suite.testJobID).Return(suite.mockedCachedJob),
		suite.mockedCachedJob.EXPECT().
			GetConfig(gomock.Any()).Return(nil, errors.New("test error")),
	)

	var request = &task.StartRequest{
		JobId: suite.testJobID,
	}
	resp, err := suite.handler.Start(
		context.Background(),
		request,
	)

	suite.NoError(err)
	suite.NotNil(resp.GetError())
}

// TestStartTasksTerminatedJob tests starting tasks from a terminated batch job
func (suite *TaskHandlerTestSuite) TestStartTasksTerminatedJob() {
	suite.testJobRuntime.State = job.JobState_SUCCEEDED
	gomock.InOrder(
		suite.mockedCandidate.EXPECT().IsLeader().Return(true),
		suite.mockedJobFactory.EXPECT().
			AddJob(suite.testJobID).Return(suite.mockedCachedJob),
		suite.mockedCachedJob.EXPECT().
			GetConfig(gomock.Any()).
			Return(cachedtest.NewMockJobConfig(suite.ctrl, suite.testJobConfig), nil),
		suite.mockedCachedJob.EXPECT().
			GetRuntime(gomock.Any()).
			Return(suite.testJobRuntime, nil),
	)

	var request = &task.StartRequest{
		JobId: suite.testJobID,
	}
	resp, err := suite.handler.Start(
		context.Background(),
		request,
	)

	suite.Error(err)
	suite.True(yarpcerrors.IsInvalidArgument(err))
	suite.Equal(yarpcerrors.ErrorMessage(err),
		"cannot start tasks in a terminated job")
	suite.Nil(resp)
}

// TestStartTasksGetRuntimeFailure tests getting a DB error when fetching job runtime
func (suite *TaskHandlerTestSuite) TestStartTasksGetRuntimeFailure() {
	gomock.InOrder(
		suite.mockedCandidate.EXPECT().IsLeader().Return(true),
		suite.mockedJobFactory.EXPECT().
			AddJob(suite.testJobID).Return(suite.mockedCachedJob),
		suite.mockedCachedJob.EXPECT().
			GetConfig(gomock.Any()).
			Return(cachedtest.NewMockJobConfig(suite.ctrl, suite.testJobConfig), nil),
		suite.mockedCachedJob.EXPECT().
			GetRuntime(gomock.Any()).
			Return(nil, fmt.Errorf("fake db error")),
	)

	var request = &task.StartRequest{
		JobId: suite.testJobID,
	}
	resp, err := suite.handler.Start(
		context.Background(),
		request,
	)
	suite.Error(err)
	suite.Nil(resp)
}

// TestStartTasksUpdateFailure tests getting a DB error
// when updating the job runtime
func (suite *TaskHandlerTestSuite) TestStartTasksUpdateFailure() {
	gomock.InOrder(
		suite.mockedCandidate.EXPECT().IsLeader().Return(true),
		suite.mockedJobFactory.EXPECT().
			AddJob(suite.testJobID).Return(suite.mockedCachedJob),
		suite.mockedCachedJob.EXPECT().
			GetConfig(gomock.Any()).Return(cachedtest.NewMockJobConfig(suite.ctrl, suite.testJobConfig), nil),
		suite.mockedCachedJob.EXPECT().
			GetRuntime(gomock.Any()).
			Return(suite.testJobRuntime, nil),
		suite.mockedCachedJob.EXPECT().
			CompareAndSetRuntime(gomock.Any(), gomock.Any()).
			Do(func(_ context.Context, jobRuntime *job.RuntimeInfo) {
				suite.Equal(job.JobState_PENDING, jobRuntime.GetState())
				suite.Equal(job.JobState_SUCCEEDED, jobRuntime.GetGoalState())
			}).
			Return(nil, errors.New("test error")),
	)

	var request = &task.StartRequest{
		JobId: suite.testJobID,
	}
	resp, err := suite.handler.Start(
		context.Background(),
		request,
	)

	suite.NoError(err)
	suite.NotNil(resp.GetError())
}

// TestStartTasksUpdateVersionError tests getting a version error from cache
// when trying to update the job runtime
func (suite *TaskHandlerTestSuite) TestStartTasksUpdateVersionError() {
	suite.mockedCandidate.EXPECT().IsLeader().Return(true)
	suite.mockedJobFactory.EXPECT().
		AddJob(suite.testJobID).Return(suite.mockedCachedJob)
	suite.mockedCachedJob.EXPECT().
		GetConfig(gomock.Any()).Return(cachedtest.NewMockJobConfig(suite.ctrl, suite.testJobConfig), nil)
	suite.mockedCachedJob.EXPECT().
		GetRuntime(gomock.Any()).
		Return(suite.testJobRuntime, nil).
		Times(jobmgrcommon.MaxConcurrencyErrorRetry)
	suite.mockedCachedJob.EXPECT().
		CompareAndSetRuntime(gomock.Any(), gomock.Any()).
		Do(func(_ context.Context, jobRuntime *job.RuntimeInfo) {
			suite.Equal(job.JobState_PENDING, jobRuntime.GetState())
			suite.Equal(job.JobState_SUCCEEDED, jobRuntime.GetGoalState())
		}).
		Return(nil, jobmgrcommon.UnexpectedVersionError).
		Times(jobmgrcommon.MaxConcurrencyErrorRetry)

	var request = &task.StartRequest{
		JobId: suite.testJobID,
	}
	resp, err := suite.handler.Start(
		context.Background(),
		request,
	)

	suite.NoError(err)
	suite.NotNil(resp.GetError())
}

// TestStartTasksCompareAndSetAddFailure tests add task failures during task start
func (suite *TaskHandlerTestSuite) TestStartTasksCompareAndSetAddFailure() {
	expectedTaskIds := make(map[*mesos.TaskID]bool)
	for _, taskInfo := range suite.taskInfos {
		expectedTaskIds[taskInfo.GetRuntime().GetMesosTaskId()] = true
	}

	var taskInfos = make(map[uint32]*task.TaskInfo)
	var tasksInfoList []*task.TaskInfo
	for i := uint32(0); i < testInstanceCount; i++ {
		taskInfos[i] = suite.createTestTaskInfo(
			task.TaskState_FAILED, i)
		taskInfos[i].Runtime.GoalState = task.TaskState_KILLED
		tasksInfoList = append(tasksInfoList, taskInfos[i])
	}

	gomock.InOrder(
		suite.mockedCandidate.EXPECT().IsLeader().Return(true),
		suite.mockedJobFactory.EXPECT().
			AddJob(suite.testJobID).Return(suite.mockedCachedJob),
		suite.mockedCachedJob.EXPECT().
			GetConfig(gomock.Any()).Return(cachedtest.NewMockJobConfig(suite.ctrl, suite.testJobConfig), nil),
		suite.mockedCachedJob.EXPECT().
			GetRuntime(gomock.Any()).
			Return(suite.testJobRuntime, nil),
		suite.mockedCachedJob.EXPECT().
			CompareAndSetRuntime(gomock.Any(), gomock.Any()).
			Do(func(_ context.Context, jobRuntime *job.RuntimeInfo) {
				suite.Equal(job.JobState_PENDING, jobRuntime.GetState())
				suite.Equal(job.JobState_SUCCEEDED, jobRuntime.GetGoalState())
			}).
			Return(&job.RuntimeInfo{}, nil),
		suite.mockedTaskStore.EXPECT().
			GetTasksForJob(gomock.Any(), suite.testJobID).Return(taskInfos, nil),
	)

	suite.mockedCachedJob.EXPECT().
		AddTask(gomock.Any(), gomock.Any()).
		Return(nil, fmt.Errorf("fake DB error")).AnyTimes()

	suite.mockedCachedJob.EXPECT().GetJobType().Return(job.JobType_BATCH)

	suite.mockedGoalStateDrive.EXPECT().
		JobRuntimeDuration(job.JobType_BATCH).
		Return(1 * time.Second)

	suite.mockedGoalStateDrive.EXPECT().
		EnqueueJob(suite.testJobID, gomock.Any()).Return()

	var request = &task.StartRequest{
		JobId: suite.testJobID,
	}
	resp, err := suite.handler.Start(
		context.Background(),
		request,
	)

	suite.NoError(err)
	suite.Nil(resp.GetError())
	suite.Equal(len(resp.GetStartedInstanceIds()), 0)
	suite.Equal(len(resp.GetInvalidInstanceIds()), testInstanceCount)
}

// TestStartTasksCompareAndSetGetRuntimeFailure tests get runtime
// DB failures during task start
func (suite *TaskHandlerTestSuite) TestStartTasksCompareAndSetGetRuntimeFailure() {
	expectedTaskIds := make(map[*mesos.TaskID]bool)
	for _, taskInfo := range suite.taskInfos {
		expectedTaskIds[taskInfo.GetRuntime().GetMesosTaskId()] = true
	}

	var taskInfos = make(map[uint32]*task.TaskInfo)
	var tasksInfoList []*task.TaskInfo
	for i := uint32(0); i < testInstanceCount; i++ {
		taskInfos[i] = suite.createTestTaskInfo(
			task.TaskState_FAILED, i)
		taskInfos[i].Runtime.GoalState = task.TaskState_KILLED
		tasksInfoList = append(tasksInfoList, taskInfos[i])
	}

	gomock.InOrder(
		suite.mockedCandidate.EXPECT().IsLeader().Return(true),
		suite.mockedJobFactory.EXPECT().
			AddJob(suite.testJobID).Return(suite.mockedCachedJob),
		suite.mockedCachedJob.EXPECT().
			GetConfig(gomock.Any()).Return(cachedtest.NewMockJobConfig(suite.ctrl, suite.testJobConfig), nil),
		suite.mockedCachedJob.EXPECT().
			GetRuntime(gomock.Any()).
			Return(suite.testJobRuntime, nil),
		suite.mockedCachedJob.EXPECT().
			CompareAndSetRuntime(gomock.Any(), gomock.Any()).
			Do(func(_ context.Context, jobRuntime *job.RuntimeInfo) {
				suite.Equal(job.JobState_PENDING, jobRuntime.GetState())
				suite.Equal(job.JobState_SUCCEEDED, jobRuntime.GetGoalState())
			}).
			Return(&job.RuntimeInfo{}, nil),
		suite.mockedTaskStore.EXPECT().
			GetTasksForJob(gomock.Any(), suite.testJobID).Return(taskInfos, nil),
	)

	suite.mockedCachedJob.EXPECT().
		AddTask(gomock.Any(), gomock.Any()).
		Return(suite.mockedCachedTask, nil).AnyTimes()
	suite.mockedCachedTask.EXPECT().
		GetRunTime(gomock.Any()).
		Return(nil, fmt.Errorf("fake db error")).AnyTimes()

	suite.mockedCachedJob.EXPECT().GetJobType().Return(job.JobType_BATCH)

	suite.mockedGoalStateDrive.EXPECT().
		JobRuntimeDuration(job.JobType_BATCH).
		Return(1 * time.Second)

	suite.mockedGoalStateDrive.EXPECT().
		EnqueueJob(suite.testJobID, gomock.Any()).Return()

	var request = &task.StartRequest{
		JobId: suite.testJobID,
	}
	resp, err := suite.handler.Start(
		context.Background(),
		request,
	)

	suite.NoError(err)
	suite.Nil(resp.GetError())
	suite.Equal(len(resp.GetStartedInstanceIds()), 0)
	suite.Equal(len(resp.GetInvalidInstanceIds()), testInstanceCount)
}

// TestStartTasksCompareAndSetFailure tests write DB failures during task start
func (suite *TaskHandlerTestSuite) TestStartTasksCompareAndSetFailure() {
	expectedTaskIds := make(map[*mesos.TaskID]bool)
	for _, taskInfo := range suite.taskInfos {
		expectedTaskIds[taskInfo.GetRuntime().GetMesosTaskId()] = true
	}

	var taskInfos = make(map[uint32]*task.TaskInfo)
	var tasksInfoList []*task.TaskInfo
	for i := uint32(0); i < testInstanceCount; i++ {
		taskInfos[i] = suite.createTestTaskInfo(
			task.TaskState_FAILED, i)
		taskInfos[i].Runtime.GoalState = task.TaskState_KILLED
		tasksInfoList = append(tasksInfoList, taskInfos[i])
	}

	gomock.InOrder(
		suite.mockedCandidate.EXPECT().IsLeader().Return(true),
		suite.mockedJobFactory.EXPECT().
			AddJob(suite.testJobID).Return(suite.mockedCachedJob),
		suite.mockedCachedJob.EXPECT().
			GetConfig(gomock.Any()).Return(cachedtest.NewMockJobConfig(suite.ctrl, suite.testJobConfig), nil),
		suite.mockedCachedJob.EXPECT().
			GetRuntime(gomock.Any()).
			Return(suite.testJobRuntime, nil),
		suite.mockedCachedJob.EXPECT().
			CompareAndSetRuntime(gomock.Any(), gomock.Any()).
			Do(func(_ context.Context, jobRuntime *job.RuntimeInfo) {
				suite.Equal(job.JobState_PENDING, jobRuntime.GetState())
				suite.Equal(job.JobState_SUCCEEDED, jobRuntime.GetGoalState())
			}).
			Return(&job.RuntimeInfo{}, nil),
		suite.mockedTaskStore.EXPECT().
			GetTasksForJob(gomock.Any(), suite.testJobID).Return(taskInfos, nil),
	)

	var taskID = fmt.Sprintf("%s-%d-%d", suite.testJobID.Value, 0, rand.Int31())
	for i := uint32(0); i < uint32(testInstanceCount); i++ {
		suite.mockedCachedJob.EXPECT().
			AddTask(gomock.Any(), i).
			Return(suite.mockedCachedTask, nil)
		for l := 0; l < jobmgrcommon.MaxConcurrencyErrorRetry; l++ {
			suite.mockedCachedTask.EXPECT().
				GetRunTime(gomock.Any()).
				Return(&task.RuntimeInfo{
					MesosTaskId: &mesos.TaskID{
						Value: &taskID,
					},
					State:     task.TaskState_FAILED,
					GoalState: task.TaskState_KILLED,
				}, nil)

			suite.mockedCachedTask.EXPECT().
				CompareAndSetRuntime(gomock.Any(), gomock.Any(), gomock.Any()).
				Return(nil, jobmgrcommon.UnexpectedVersionError)
		}
	}

	suite.mockedCachedJob.EXPECT().GetJobType().Return(job.JobType_BATCH)

	suite.mockedGoalStateDrive.EXPECT().
		JobRuntimeDuration(job.JobType_BATCH).
		Return(1 * time.Second)

	suite.mockedGoalStateDrive.EXPECT().
		EnqueueJob(suite.testJobID, gomock.Any()).Return()

	var request = &task.StartRequest{
		JobId: suite.testJobID,
	}
	resp, err := suite.handler.Start(
		context.Background(),
		request,
	)

	suite.NoError(err)
	suite.Nil(resp.GetError())
	suite.Equal(len(resp.GetStartedInstanceIds()), 0)
	suite.Equal(len(resp.GetInvalidInstanceIds()), testInstanceCount)
}

func (suite *TaskHandlerTestSuite) TestStartTasksWithRanges() {
	expectedTaskIds := make(map[*mesos.TaskID]bool)
	for _, taskInfo := range suite.taskInfos {
		expectedTaskIds[taskInfo.GetRuntime().GetMesosTaskId()] = true
	}

	singleTaskInfo := make(map[uint32]*task.TaskInfo)
	singleTaskInfo[1] = suite.createTestTaskInfo(
		task.TaskState_FAILED, 1)
	singleTaskInfo[1].GetRuntime().GoalState = task.TaskState_KILLED

	taskRanges := []*task.InstanceRange{
		{
			From: 1,
			To:   2,
		},
	}

	gomock.InOrder(
		suite.mockedCandidate.EXPECT().IsLeader().Return(true),
		suite.mockedJobFactory.EXPECT().
			AddJob(suite.testJobID).Return(suite.mockedCachedJob),
		suite.mockedCachedJob.EXPECT().
			GetConfig(gomock.Any()).Return(cachedtest.NewMockJobConfig(suite.ctrl, suite.testJobConfig), nil),
		suite.mockedCachedJob.EXPECT().
			GetRuntime(gomock.Any()).
			Return(suite.testJobRuntime, nil),
		suite.mockedCachedJob.EXPECT().
			CompareAndSetRuntime(gomock.Any(), gomock.Any()).
			Do(func(_ context.Context, jobRuntime *job.RuntimeInfo) {
				suite.Equal(job.JobState_PENDING, jobRuntime.GetState())
				suite.Equal(job.JobState_SUCCEEDED, jobRuntime.GetGoalState())
			}).
			Return(&job.RuntimeInfo{}, nil),
		suite.mockedTaskStore.EXPECT().
			GetTasksForJobByRange(gomock.Any(), suite.testJobID, taskRanges[0]).Return(singleTaskInfo, nil),
	)

	suite.mockedCachedJob.EXPECT().
		AddTask(gomock.Any(), gomock.Any()).
		Return(suite.mockedCachedTask, nil)
	suite.mockedCachedTask.EXPECT().
		GetRunTime(gomock.Any()).
		Return(singleTaskInfo[1].Runtime, nil)
	suite.mockedCachedTask.EXPECT().
		CompareAndSetRuntime(gomock.Any(), gomock.Any(), gomock.Any()).
		Do(func(_ context.Context, runtime *task.RuntimeInfo, _ job.JobType) {
			suite.Equal(runtime.State, task.TaskState_INITIALIZED)
			suite.Equal(runtime.Healthy, task.HealthState_DISABLED)
			suite.Equal(runtime.GoalState, task.TaskState_SUCCEEDED)
		}).Return(nil, nil)

	suite.mockedGoalStateDrive.EXPECT().
		EnqueueTask(suite.testJobID, gomock.Any(), gomock.Any()).Return()

	suite.mockedCachedJob.EXPECT().GetJobType().Return(job.JobType_BATCH)

	suite.mockedGoalStateDrive.EXPECT().
		JobRuntimeDuration(job.JobType_BATCH).
		Return(1 * time.Second)

	suite.mockedGoalStateDrive.EXPECT().
		EnqueueJob(suite.testJobID, gomock.Any()).Return()

	var request = &task.StartRequest{
		JobId:  suite.testJobID,
		Ranges: taskRanges,
	}
	resp, err := suite.handler.Start(
		context.Background(),
		request,
	)
	suite.NoError(err)
	suite.Nil(resp.GetError())
	suite.Equal(len(resp.GetInvalidInstanceIds()), 0)
	suite.Equal(resp.GetStartedInstanceIds(), []uint32{1})
}

func (suite *TaskHandlerTestSuite) TestGetEvents() {
	taskEvents := suite.createTestTaskEvents()
	suite.testJobConfig.Type = job.JobType_BATCH

	gomock.InOrder(
		suite.mockedJobFactory.EXPECT().
			GetJob(suite.testJobID).Return(suite.mockedCachedJob),
		suite.mockedCachedJob.EXPECT().
			GetConfig(gomock.Any()).Return(suite.testJobConfig, nil),
		suite.mockedTaskStore.EXPECT().
			GetTaskEvents(gomock.Any(), gomock.Any(), gomock.Any()).Return(taskEvents, nil),
	)
	var request = &task.GetEventsRequest{
		JobId:      suite.testJobID,
		InstanceId: 0,
	}
	resp, err := suite.handler.GetEvents(
		context.Background(),
		request,
	)
	suite.NoError(err)
	suite.Nil(resp.GetError())
	eventsList := resp.GetResult()
	suite.Equal(len(eventsList), 2)
	task0Events := eventsList[0].GetEvent()
	task1Events := eventsList[1].GetEvent()
	suite.Equal(len(task0Events), 2)
	suite.Equal(len(task1Events), 3)
	taskID1 := fmt.Sprintf("%s-%d", suite.testJobID.Value, 1)
	expectedTask1Events := []*task.TaskEvent{
		{
			TaskId: &peloton.TaskID{
				Value: taskID1,
			},
			State:     task.TaskState_INITIALIZED,
			Message:   "",
			Timestamp: "2017-12-11T22:17:46Z",
			Hostname:  "peloton-test-host-1",
			Reason:    "",
		},
		{
			TaskId: &peloton.TaskID{
				Value: taskID1,
			},
			State:     task.TaskState_LAUNCHED,
			Message:   "",
			Timestamp: "2017-12-11T22:17:50Z",
			Hostname:  "peloton-test-host-1",
			Reason:    "",
		},
		{
			TaskId: &peloton.TaskID{
				Value: taskID1,
			},
			State:     task.TaskState_RUNNING,
			Message:   "",
			Timestamp: "2017-12-11T22:17:56Z",
			Hostname:  "peloton-test-host-1",
			Reason:    "",
		},
	}
	suite.Equal(task1Events, expectedTask1Events)
}

func (suite *TaskHandlerTestSuite) TestGetEvents_GetJobConfigFailure() {
	suite.testJobConfig.Type = job.JobType_BATCH

	gomock.InOrder(
		suite.mockedJobFactory.EXPECT().
			GetJob(suite.testJobID).Return(suite.mockedCachedJob),
		suite.mockedCachedJob.EXPECT().
			GetConfig(gomock.Any()).Return(nil, fmt.Errorf("test error")),
	)
	var request = &task.GetEventsRequest{
		JobId:      suite.testJobID,
		InstanceId: 0,
	}
	resp, err := suite.handler.GetEvents(
		context.Background(),
		request,
	)
	suite.NoError(err)
	suite.NotNil(resp.GetError())
}

func (suite *TaskHandlerTestSuite) TestGetEvents_GetEventsFailure() {
	suite.testJobConfig.Type = job.JobType_BATCH

	gomock.InOrder(
		suite.mockedJobFactory.EXPECT().
			GetJob(suite.testJobID).Return(suite.mockedCachedJob),
		suite.mockedCachedJob.EXPECT().
			GetConfig(gomock.Any()).Return(suite.testJobConfig, nil),
		suite.mockedTaskStore.EXPECT().
			GetTaskEvents(gomock.Any(), gomock.Any(), gomock.Any()).Return(nil, fmt.Errorf("test error")),
	)
	var request = &task.GetEventsRequest{
		JobId:      suite.testJobID,
		InstanceId: 0,
	}
	resp, err := suite.handler.GetEvents(
		context.Background(),
		request,
	)
	suite.Nil(err)
	suite.NotNil(resp.GetError())
}

func (suite *TaskHandlerTestSuite) TestGetEvents_Service_Job() {
	suite.testJobConfig.Type = job.JobType_SERVICE

	gomock.InOrder(
		suite.mockedJobFactory.EXPECT().
			GetJob(suite.testJobID).Return(suite.mockedCachedJob),
		suite.mockedCachedJob.EXPECT().
			GetConfig(gomock.Any()).Return(suite.testJobConfig, nil),
	)
	var request = &task.GetEventsRequest{
		JobId:      suite.testJobID,
		InstanceId: 0,
	}
	resp, err := suite.handler.GetEvents(
		context.Background(),
		request,
	)
	suite.NoError(err)
	suite.Nil(resp.GetError())
	eventsList := resp.GetResult()
	suite.Len(eventsList, 0)
}

func (suite *TaskHandlerTestSuite) TestStartTasksWithInvalidRanges() {
	singleTaskInfo := make(map[uint32]*task.TaskInfo)
	singleTaskInfo[1] = suite.taskInfos[1]
	emptyTaskInfo := make(map[uint32]*task.TaskInfo)

	taskRanges := []*task.InstanceRange{
		{
			From: 1,
			To:   2,
		},
		{
			From: 3,
			To:   math.MaxInt32 + 1, // To should not go beyond MaxInt32
		},
	}

	correctRange := &task.InstanceRange{
		From: 3,
		To:   math.MaxInt32,
	}

	gomock.InOrder(
		suite.mockedCandidate.EXPECT().IsLeader().Return(true),
		suite.mockedJobFactory.EXPECT().
			AddJob(gomock.Any()).Return(suite.mockedCachedJob),
		suite.mockedCachedJob.EXPECT().
			GetConfig(gomock.Any()).
			Return(cachedtest.NewMockJobConfig(suite.ctrl, suite.testJobConfig), nil),
		suite.mockedCachedJob.EXPECT().
			GetRuntime(gomock.Any()).
			Return(suite.testJobRuntime, nil),
		suite.mockedCachedJob.EXPECT().
			CompareAndSetRuntime(gomock.Any(), gomock.Any()).
			Do(func(_ context.Context, jobRuntime *job.RuntimeInfo) {
				suite.Equal(job.JobState_PENDING, jobRuntime.GetState())
				suite.Equal(job.JobState_SUCCEEDED, jobRuntime.GetGoalState())
			}).
			Return(&job.RuntimeInfo{}, nil),
		suite.mockedTaskStore.EXPECT().
			GetTasksForJobByRange(gomock.Any(), suite.testJobID, taskRanges[0]).Return(singleTaskInfo, nil),
		suite.mockedTaskStore.EXPECT().
			GetTasksForJobByRange(gomock.Any(), suite.testJobID, correctRange).
			Return(emptyTaskInfo, errors.New("test error")),
	)

	var request = &task.StartRequest{
		JobId:  suite.testJobID,
		Ranges: taskRanges,
	}
	resp, err := suite.handler.Start(
		context.Background(),
		request,
	)
	suite.NoError(err)
	suite.Equal(len(resp.GetStartedInstanceIds()), 0)
	suite.Equal(resp.GetError().GetOutOfRange().GetJobId().GetValue(), testJob)
	suite.Equal(
		resp.GetError().GetOutOfRange().GetInstanceCount(),
		uint32(testInstanceCount))
}

func (suite *TaskHandlerTestSuite) TestBrowseSandboxPreviousTaskRun() {
	instanceID := uint32(0)
	taskRuns := uint32(3)
	events, getReturnEvents := suite.createTaskEventForGetTasks(instanceID, taskRuns)

	gomock.InOrder(
		suite.mockedJobFactory.EXPECT().
			GetJob(suite.testJobID).Return(suite.mockedCachedJob),
		suite.mockedCachedJob.EXPECT().
			GetConfig(gomock.Any()).Return(cachedtest.NewMockJobConfig(suite.ctrl, suite.testJobConfig), nil),
		suite.mockedTaskStore.EXPECT().
			GetTaskEvents(gomock.Any(), suite.testJobID, instanceID).Return(events, nil),
	)

	var req = &task.BrowseSandboxRequest{
		JobId:      suite.testJobID,
		InstanceId: instanceID,
		TaskId:     getReturnEvents[0].GetTaskId().GetValue(),
	}
	resp, err := suite.handler.BrowseSandbox(context.Background(), req)
	suite.NoError(err)
	suite.NotNil(resp.GetError().GetNotRunning())
}

func (suite *TaskHandlerTestSuite) TestBrowseSandboxWithoutHostname() {
	singleTaskInfo := make(map[uint32]*task.TaskInfo)
	singleTaskInfo[0] = suite.taskInfos[0]

	gomock.InOrder(
		suite.mockedJobFactory.EXPECT().
			GetJob(suite.testJobID).Return(suite.mockedCachedJob),
		suite.mockedCachedJob.EXPECT().
			GetConfig(gomock.Any()).Return(cachedtest.NewMockJobConfig(suite.ctrl, suite.testJobConfig), nil),
		suite.mockedTaskStore.EXPECT().
			GetTaskForJob(gomock.Any(), suite.testJobID, uint32(0)).Return(singleTaskInfo, nil),
	)

	var request = &task.BrowseSandboxRequest{
		JobId:      suite.testJobID,
		InstanceId: 0,
	}
	resp, err := suite.handler.BrowseSandbox(context.Background(), request)
	suite.NoError(err)
	suite.NotNil(resp.GetError().GetNotRunning())
}

func (suite *TaskHandlerTestSuite) TestBrowseSandboxWithEmptyFrameworkID() {
	singleTaskInfo := make(map[uint32]*task.TaskInfo)
	singleTaskInfo[0] = suite.taskInfos[0]
	singleTaskInfo[0].GetRuntime().Host = "host-0"
	singleTaskInfo[0].GetRuntime().AgentID = &mesos.AgentID{
		Value: util.PtrPrintf("host-agent-0"),
	}

	gomock.InOrder(
		suite.mockedJobFactory.EXPECT().
			GetJob(suite.testJobID).Return(suite.mockedCachedJob),
		suite.mockedCachedJob.EXPECT().
			GetConfig(gomock.Any()).Return(cachedtest.NewMockJobConfig(suite.ctrl, suite.testJobConfig), nil),
		suite.mockedTaskStore.EXPECT().
			GetTaskForJob(gomock.Any(), suite.testJobID, uint32(0)).Return(singleTaskInfo, nil),
		suite.mockedFrameworkInfoStore.EXPECT().GetFrameworkID(gomock.Any(), _frameworkName).Return("", nil),
	)

	var request = &task.BrowseSandboxRequest{
		JobId:      suite.testJobID,
		InstanceId: 0,
	}
	resp, err := suite.handler.BrowseSandbox(context.Background(), request)
	suite.NoError(err)
	suite.NotNil(resp.GetError().GetFailure())
}

func (suite *TaskHandlerTestSuite) TestBrowseSandboxListSandboxFileFailure() {
	instanceID := uint32(0)
	taskRuns := uint32(3)
	events, getReturnEvents := suite.createTaskEventForGetTasks(
		instanceID, taskRuns)
	hostName := "peloton-test-host"
	agentID := "peloton-test-agent"
	frameworkID := "1234"
	mesosAgentDir := "mesosAgentDir"

	suite.handler.mesosAgentWorkDir = mesosAgentDir

	var req = &task.BrowseSandboxRequest{
		JobId:      suite.testJobID,
		InstanceId: instanceID,
		TaskId:     getReturnEvents[1].GetTaskId().GetValue(),
	}

	gomock.InOrder(
		suite.mockedJobFactory.EXPECT().GetJob(suite.testJobID).
			Return(suite.mockedCachedJob),
		suite.mockedCachedJob.EXPECT().GetConfig(gomock.Any()).
			Return(
				cachedtest.NewMockJobConfig(suite.ctrl, suite.testJobConfig),
				nil),
		suite.mockedTaskStore.EXPECT().
			GetTaskEvents(gomock.Any(), suite.testJobID, instanceID).
			Return(events, nil),
		suite.mockedFrameworkInfoStore.EXPECT().
			GetFrameworkID(gomock.Any(), _frameworkName).
			Return(frameworkID, nil),
		suite.mockedHostMgr.EXPECT().
			GetMesosAgentInfo(gomock.Any(),
				&hostsvc.GetMesosAgentInfoRequest{Hostname: hostName}).
			Return(&hostsvc.GetMesosAgentInfoResponse{}, nil),
		suite.mockedLogManager.EXPECT().
			ListSandboxFilesPaths(mesosAgentDir, frameworkID, hostName,
				"5051", agentID, req.GetTaskId()).
			Return(nil, errors.New(
				"enable to fetch sandbox files from mesos agent")),
	)

	resp, _ := suite.handler.BrowseSandbox(context.Background(), req)
	suite.NotEmpty(resp.GetError().GetFailure())
}

func (suite *TaskHandlerTestSuite) TestBrowseSandboxGetMesosMasterInfoFailure() {
	instanceID := uint32(0)
	taskRuns := uint32(3)
	events, getReturnEvents := suite.createTaskEventForGetTasks(
		instanceID, taskRuns)
	sandboxFilesPaths := []string{"path1", "path2"}
	hostName := "peloton-test-host"
	agentID := "peloton-test-agent"
	frameworkID := "1234"
	mesosAgentDir := "mesosAgentDir"

	suite.handler.mesosAgentWorkDir = mesosAgentDir

	var req = &task.BrowseSandboxRequest{
		JobId:      suite.testJobID,
		InstanceId: instanceID,
		TaskId:     getReturnEvents[1].GetTaskId().GetValue(),
	}

	gomock.InOrder(
		suite.mockedJobFactory.EXPECT().GetJob(suite.testJobID).
			Return(suite.mockedCachedJob),
		suite.mockedCachedJob.EXPECT().GetConfig(gomock.Any()).
			Return(
				cachedtest.NewMockJobConfig(suite.ctrl, suite.testJobConfig),
				nil),
		suite.mockedTaskStore.EXPECT().
			GetTaskEvents(gomock.Any(), suite.testJobID, instanceID).
			Return(events, nil),
		suite.mockedFrameworkInfoStore.EXPECT().
			GetFrameworkID(gomock.Any(), _frameworkName).
			Return(frameworkID, nil),
		suite.mockedHostMgr.EXPECT().
			GetMesosAgentInfo(gomock.Any(),
				&hostsvc.GetMesosAgentInfoRequest{Hostname: hostName}).
			Return(&hostsvc.GetMesosAgentInfoResponse{}, nil),
		suite.mockedLogManager.EXPECT().
			ListSandboxFilesPaths(mesosAgentDir, frameworkID, hostName,
				"5051", agentID, req.GetTaskId()).
			Return(sandboxFilesPaths, nil),
		suite.mockedHostMgr.EXPECT().
			GetMesosMasterHostPort(gomock.Any(),
				&hostsvc.MesosMasterHostPortRequest{}).
			Return(nil, errors.New("unable to fetch mesos master info")),
	)

	resp, _ := suite.handler.BrowseSandbox(context.Background(), req)
	suite.NotEmpty(resp.GetError().GetFailure())
}

func (suite *TaskHandlerTestSuite) TestBrowseSandboxListFilesSuccess() {

	instanceID := uint32(0)
	taskRuns := uint32(3)
	events, getReturnEvents := suite.createTaskEventForGetTasks(
		instanceID, taskRuns)
	sandboxFilesPaths := []string{"path1", "path2"}
	hostName := "peloton-test-host"
	agentID := "peloton-test-agent"
	frameworkID := "1234"
	mesosAgentDir := "mesosAgentDir"

	suite.handler.mesosAgentWorkDir = mesosAgentDir

	var req = &task.BrowseSandboxRequest{
		JobId:      suite.testJobID,
		InstanceId: instanceID,
		TaskId:     getReturnEvents[1].GetTaskId().GetValue(),
	}

	var res = &task.BrowseSandboxResponse{
		Hostname:            hostName,
		Port:                "5051",
		Error:               nil,
		Paths:               sandboxFilesPaths,
		MesosMasterHostname: "master",
		MesosMasterPort:     "5050",
	}

	gomock.InOrder(
		suite.mockedJobFactory.EXPECT().GetJob(suite.testJobID).
			Return(suite.mockedCachedJob),
		suite.mockedCachedJob.EXPECT().GetConfig(gomock.Any()).
			Return(
				cachedtest.NewMockJobConfig(suite.ctrl, suite.testJobConfig),
				nil),
		suite.mockedTaskStore.EXPECT().
			GetTaskEvents(gomock.Any(), suite.testJobID, instanceID).
			Return(events, nil),
		suite.mockedFrameworkInfoStore.EXPECT().
			GetFrameworkID(gomock.Any(), _frameworkName).
			Return(frameworkID, nil),
		suite.mockedHostMgr.EXPECT().
			GetMesosAgentInfo(gomock.Any(),
				&hostsvc.GetMesosAgentInfoRequest{Hostname: hostName}).
			Return(&hostsvc.GetMesosAgentInfoResponse{}, nil),
		suite.mockedLogManager.EXPECT().
			ListSandboxFilesPaths(mesosAgentDir, frameworkID, hostName,
				"5051", agentID, req.GetTaskId()).
			Return(sandboxFilesPaths, nil),
		suite.mockedHostMgr.EXPECT().
			GetMesosMasterHostPort(gomock.Any(),
				&hostsvc.MesosMasterHostPortRequest{}).
			Return(&hostsvc.MesosMasterHostPortResponse{
				Hostname: "master",
				Port:     "5050",
			}, nil),
	)

	resp, err := suite.handler.BrowseSandbox(context.Background(), req)
	suite.NoError(err)
	suite.Equal(resp.Paths, sandboxFilesPaths)
	suite.Equal(resp, res)
}

// Tests BrowseSandbox handler when an IP-address + port is available for the
// Mesos slave agent
func (suite *TaskHandlerTestSuite) TestBrowseSandboxListFilesSuccessAgentIP() {

	instanceID := uint32(0)
	taskRuns := uint32(3)
	events, getReturnEvents := suite.createTaskEventForGetTasks(instanceID, taskRuns)
	sandboxFilesPaths := []string{"path1", "path2"}
	hostName := "peloton-test-host"
	agentIP := "1.2.3.4"
	agentPort := "31000"
	agentPid := "slave(1)@" + agentIP + ":" + agentPort
	agentID := "peloton-test-agent"
	frameworkID := "1234"
	mesosAgentDir := "mesosAgentDir"

	suite.handler.mesosAgentWorkDir = mesosAgentDir

	var req = &task.BrowseSandboxRequest{
		JobId:      suite.testJobID,
		InstanceId: instanceID,
		TaskId:     getReturnEvents[1].GetTaskId().GetValue(),
	}

	var res = &task.BrowseSandboxResponse{
		Hostname:            agentIP,
		Port:                agentPort,
		Error:               nil,
		Paths:               sandboxFilesPaths,
		MesosMasterHostname: "master",
		MesosMasterPort:     "5050",
	}

	agentInfos := make([]*mesos_master.Response_GetAgents_Agent, 1)
	agentInfos[0] = &mesos_master.Response_GetAgents_Agent{
		AgentInfo: &mesos.AgentInfo{
			Id:       &mesos.AgentID{Value: &agentID},
			Hostname: &hostName,
		},
		Pid: &agentPid,
	}
	gomock.InOrder(
		suite.mockedJobFactory.EXPECT().GetJob(suite.testJobID).
			Return(suite.mockedCachedJob),
		suite.mockedCachedJob.EXPECT().GetConfig(gomock.Any()).
			Return(
				cachedtest.NewMockJobConfig(suite.ctrl, suite.testJobConfig),
				nil),
		suite.mockedTaskStore.EXPECT().
			GetTaskEvents(gomock.Any(), suite.testJobID, instanceID).
			Return(events, nil),
		suite.mockedFrameworkInfoStore.EXPECT().
			GetFrameworkID(gomock.Any(), _frameworkName).
			Return(frameworkID, nil),
		suite.mockedHostMgr.EXPECT().
			GetMesosAgentInfo(gomock.Any(),
				&hostsvc.GetMesosAgentInfoRequest{Hostname: hostName}).
			Return(&hostsvc.GetMesosAgentInfoResponse{Agents: agentInfos},
				nil),
		suite.mockedLogManager.EXPECT().
			ListSandboxFilesPaths(mesosAgentDir, frameworkID, agentIP,
				agentPort, agentID, req.GetTaskId()).
			Return(sandboxFilesPaths, nil),
		suite.mockedHostMgr.EXPECT().
			GetMesosMasterHostPort(gomock.Any(),
				&hostsvc.MesosMasterHostPortRequest{}).
			Return(&hostsvc.MesosMasterHostPortResponse{
				Hostname: "master",
				Port:     "5050",
			}, nil),
	)

	resp, err := suite.handler.BrowseSandbox(context.Background(), req)
	suite.NoError(err)
	suite.Equal(resp.Paths, sandboxFilesPaths)
	suite.Equal(resp, res)
}

func (suite *TaskHandlerTestSuite) TestRefreshTask() {
	runtimes := make(map[uint32]*task.RuntimeInfo)
	for instID, taskInfo := range suite.taskInfos {
		runtimes[instID] = taskInfo.GetRuntime()
	}

	suite.mockedCandidate.EXPECT().IsLeader().Return(true)
	suite.mockedJobStore.EXPECT().
		GetJobConfig(gomock.Any(), suite.testJobID).Return(suite.testJobConfig, &models.ConfigAddOn{}, nil)
	suite.mockedTaskStore.EXPECT().
		GetTaskRuntimesForJobByRange(gomock.Any(), suite.testJobID, &task.InstanceRange{
			From: 0,
			To:   suite.testJobConfig.GetInstanceCount(),
		}).Return(runtimes, nil)
	suite.mockedJobFactory.EXPECT().
		AddJob(suite.testJobID).Return(suite.mockedCachedJob)
	suite.mockedCachedJob.EXPECT().
		ReplaceTasks(runtimes, true).Return(nil)
	suite.mockedGoalStateDrive.EXPECT().
		EnqueueTask(suite.testJobID, gomock.Any(), gomock.Any()).Return().Times(int(suite.testJobConfig.GetInstanceCount()))
	suite.mockedCachedJob.EXPECT().GetJobType().Return(job.JobType_BATCH)
	suite.mockedGoalStateDrive.EXPECT().
		JobRuntimeDuration(job.JobType_BATCH).
		Return(1 * time.Second)
	suite.mockedGoalStateDrive.EXPECT().
		EnqueueJob(suite.testJobID, gomock.Any()).Return()

	var request = &task.RefreshRequest{
		JobId: suite.testJobID,
	}
	_, err := suite.handler.Refresh(context.Background(), request)
	suite.NoError(err)

	suite.mockedCandidate.EXPECT().IsLeader().Return(true)
	suite.mockedJobStore.EXPECT().
		GetJobConfig(gomock.Any(), suite.testJobID).Return(nil, nil, fmt.Errorf("fake db error"))
	_, err = suite.handler.Refresh(context.Background(), request)
	suite.Error(err)

	suite.mockedCandidate.EXPECT().IsLeader().Return(true)
	suite.mockedJobStore.EXPECT().
		GetJobConfig(gomock.Any(), suite.testJobID).Return(suite.testJobConfig, &models.ConfigAddOn{}, nil)
	suite.mockedTaskStore.EXPECT().
		GetTaskRuntimesForJobByRange(gomock.Any(), suite.testJobID, &task.InstanceRange{
			From: 0,
			To:   suite.testJobConfig.GetInstanceCount(),
		}).Return(nil, fmt.Errorf("fake db error"))
	_, err = suite.handler.Refresh(context.Background(), request)
	suite.Error(err)

	suite.mockedCandidate.EXPECT().IsLeader().Return(true)
	suite.mockedJobStore.EXPECT().
		GetJobConfig(gomock.Any(), suite.testJobID).
		Return(suite.testJobConfig, &models.ConfigAddOn{}, nil)
	suite.mockedTaskStore.EXPECT().
		GetTaskRuntimesForJobByRange(gomock.Any(), suite.testJobID, &task.InstanceRange{
			From: 0,
			To:   suite.testJobConfig.GetInstanceCount(),
		}).Return(nil, nil)
	_, err = suite.handler.Refresh(context.Background(), request)
	suite.Error(err)
}

func (suite *TaskHandlerTestSuite) initTestTaskInfo(
	runningTasks uint32,
	pendingTasks uint32) map[uint32]*task.TaskInfo {
	taskInfos := make(map[uint32]*task.TaskInfo)
	for i := uint32(0); i < testInstanceCount; i++ {
		if i < runningTasks {
			taskInfos[i] = suite.createTestTaskInfo(
				task.TaskState_RUNNING, i)
		} else {
			taskInfos[i] = suite.createTestTaskInfo(
				task.TaskState_PENDING, i)
		}
	}
	return taskInfos
}

func (suite *TaskHandlerTestSuite) TestListTask() {
	runningTasks := uint32(testInstanceCount) / 2
	pendingTasks := uint32(testInstanceCount) - runningTasks
	taskInfos := suite.initTestTaskInfo(runningTasks, pendingTasks)

	suite.mockedTaskStore.EXPECT().
		GetTasksForJob(gomock.Any(), suite.testJobID).
		Return(taskInfos, nil)
	suite.mockedActiveRMTasks.EXPECT().
		GetActiveTasks(gomock.Any()).
		Return(&resmgrsvc.GetActiveTasksResponse_TaskEntry{
			Reason: "TEST_REASON",
		}).Times(2)

	result, err := suite.handler.List(context.Background(), &task.ListRequest{
		JobId: suite.testJobID,
	})
	suite.NoError(err)
	for _, taskInfo := range result.GetResult().GetValue() {
		if taskInfo.GetRuntime().GetState() == task.TaskState_RUNNING {
			suite.Equal(taskInfo.GetRuntime().GetReason(), "")
			runningTasks--
		}
		if taskInfo.GetRuntime().GetState() == task.TaskState_PENDING {
			suite.Equal(taskInfo.GetRuntime().GetReason(), "TEST_REASON")
			pendingTasks--
		}
	}
	suite.Equal(runningTasks, uint32(0))
	suite.Equal(pendingTasks, uint32(0))
}

func (suite *TaskHandlerTestSuite) TestListTaskQueryByRange() {
	runningTasks := uint32(testInstanceCount) / 2
	pendingTasks := uint32(testInstanceCount) - runningTasks
	taskInfos := suite.initTestTaskInfo(runningTasks, pendingTasks)

	// test Query by range
	suite.mockedTaskStore.EXPECT().
		GetTasksForJobByRange(gomock.Any(), suite.testJobID, gomock.Any()).
		Return(taskInfos, nil)

	suite.mockedActiveRMTasks.EXPECT().
		GetActiveTasks(gomock.Any()).
		Return(&resmgrsvc.GetActiveTasksResponse_TaskEntry{
			Reason: "TEST_REASON",
		}).Times(2)

	_, err := suite.handler.List(context.Background(), &task.ListRequest{
		JobId: suite.testJobID,
		Range: &task.InstanceRange{
			From: 0,
			To:   testInstanceCount + 1,
		},
	})
	suite.NoError(err)
}

func (suite *TaskHandlerTestSuite) TestListTaskNoTaskInDB() {
	emptyTaskInfos := make(map[uint32]*task.TaskInfo)
	suite.mockedTaskStore.EXPECT().
		GetTasksForJobByRange(gomock.Any(), suite.testJobID, gomock.Any()).
		Return(emptyTaskInfos, nil)
	_, err := suite.handler.List(context.Background(), &task.ListRequest{
		JobId: suite.testJobID,
		Range: &task.InstanceRange{
			From: 0,
			To:   testInstanceCount + 1,
		},
	})
	suite.NoError(err)
}

func (suite *TaskHandlerTestSuite) TestListTaskNoTaskInCache() {
	runningTasks := uint32(testInstanceCount) / 2
	pendingTasks := uint32(testInstanceCount) - runningTasks
	taskInfos := suite.initTestTaskInfo(runningTasks, pendingTasks)

	suite.mockedTaskStore.EXPECT().
		GetTasksForJob(gomock.Any(), suite.testJobID).
		Return(taskInfos, nil)
	suite.mockedActiveRMTasks.EXPECT().
		GetActiveTasks(gomock.Any()).
		Return(&resmgrsvc.GetActiveTasksResponse_TaskEntry{
			Reason: "TEST_REASON",
		}).Times(2)

	_, err := suite.handler.List(context.Background(), &task.ListRequest{
		JobId: suite.testJobID,
	})
	suite.NoError(err)
}

func (suite *TaskHandlerTestSuite) TestQueryTask() {
	//testReason := "test reason"
	//var taskEntries []*resmgrsvc.GetActiveTasksResponse_TaskEntry
	taskInfos := make([]*task.TaskInfo, testInstanceCount)
	runningTasks := testInstanceCount / 2
	pendingTasks := testInstanceCount - runningTasks
	for i := 0; i < testInstanceCount; i++ {
		if i < runningTasks {
			taskInfos[i] = suite.createTestTaskInfo(
				task.TaskState_RUNNING, uint32(i))
		} else {
			taskInfos[i] = suite.createTestTaskInfo(
				task.TaskState_PENDING, uint32(i))
			/*taskEntries = append(taskEntries, &resmgrsvc.GetActiveTasksResponse_TaskEntry{
				TaskID: fmt.Sprintf("%s-%d", suite.testJobID.Value, i),
				Reason: testReason,
			})*/
		}
	}

	suite.mockedJobFactory.EXPECT().
		GetJob(suite.testJobID).
		Return(suite.mockedCachedJob)
	suite.mockedCachedJob.EXPECT().
		GetRuntime(gomock.Any()).
		Return(suite.testJobRuntime, nil)
	suite.mockedTaskStore.EXPECT().
		QueryTasks(gomock.Any(), suite.testJobID, nil).
		Return(taskInfos, uint32(testInstanceCount), nil)
	suite.mockedActiveRMTasks.EXPECT().
		GetActiveTasks(gomock.Any()).
		Return(&resmgrsvc.GetActiveTasksResponse_TaskEntry{
			Reason: "TEST_REASON",
		}).Times(2)

	result, err := suite.handler.Query(context.Background(), &task.QueryRequest{
		JobId: suite.testJobID,
	})
	suite.NoError(err)
	for _, taskInfo := range result.Records {
		if taskInfo.GetRuntime().GetState() == task.TaskState_RUNNING {
			suite.Equal(taskInfo.GetRuntime().GetReason(), "")
			runningTasks--
		}
		if taskInfo.GetRuntime().GetState() == task.TaskState_PENDING {
			suite.Equal(taskInfo.GetRuntime().GetReason(), "TEST_REASON")
			pendingTasks--
		}
	}
	suite.Equal(runningTasks, 0)
	suite.Equal(pendingTasks, 0)
}

func (suite *TaskHandlerTestSuite) TestQueryTaskQueryJobErr() {
	suite.mockedJobFactory.EXPECT().
		GetJob(suite.testJobID).
		Return(suite.mockedCachedJob)
	suite.mockedCachedJob.EXPECT().
		GetRuntime(gomock.Any()).
		Return(suite.testJobRuntime, errors.New("test error"))
	suite.mockedTaskStore.EXPECT()
	_, err := suite.handler.Query(context.Background(), &task.QueryRequest{
		JobId: suite.testJobID,
	})
	suite.NoError(err)
}

func (suite *TaskHandlerTestSuite) TestQueryTaskQueryTaskErr() {
	taskInfos := make([]*task.TaskInfo, testInstanceCount)

	suite.mockedJobFactory.EXPECT().
		GetJob(suite.testJobID).
		Return(suite.mockedCachedJob)
	suite.mockedCachedJob.EXPECT().
		GetRuntime(gomock.Any()).
		Return(suite.testJobRuntime, nil)
	suite.mockedTaskStore.EXPECT().
		QueryTasks(gomock.Any(), suite.testJobID, nil).
		Return(taskInfos, uint32(testInstanceCount), errors.New("test error"))
	_, err := suite.handler.Query(context.Background(), &task.QueryRequest{
		JobId: suite.testJobID,
	})
	suite.NoError(err)
}

func (suite *TaskHandlerTestSuite) TestGetCache_JobNotFound() {
	instanceID := uint32(0)

	// Test cannot find job
	suite.mockedJobFactory.EXPECT().
		GetJob(gomock.Any()).Return(nil)
	_, err := suite.handler.GetCache(context.Background(), &task.GetCacheRequest{
		JobId:      suite.testJobID,
		InstanceId: instanceID,
	})
	suite.Error(err)
}

func (suite *TaskHandlerTestSuite) TestGetCache_TaskNotFound() {
	instanceID := uint32(0)

	// Test cannot find task
	suite.mockedJobFactory.EXPECT().
		GetJob(gomock.Any()).Return(suite.mockedCachedJob)
	suite.mockedCachedJob.EXPECT().
		GetTask(instanceID).Return(nil)
	_, err := suite.handler.GetCache(context.Background(), &task.GetCacheRequest{
		JobId:      suite.testJobID,
		InstanceId: instanceID,
	})
	suite.Error(err)
}

func (suite *TaskHandlerTestSuite) TestGetCache_FailToLoadRuntime() {
	instanceID := uint32(0)

	// Test cannot load task runtime
	suite.mockedJobFactory.EXPECT().
		GetJob(gomock.Any()).Return(suite.mockedCachedJob)
	suite.mockedCachedJob.EXPECT().
		GetTask(instanceID).Return(suite.mockedTask)
	suite.mockedTask.EXPECT().
		GetRunTime(gomock.Any()).Return(nil, fmt.Errorf("test err"))
	_, err := suite.handler.GetCache(context.Background(), &task.GetCacheRequest{
		JobId:      suite.testJobID,
		InstanceId: instanceID,
	})
	suite.Error(err)
}

func (suite *TaskHandlerTestSuite) TestGetCache_SUCCESS() {
	instanceID := uint32(0)

	// Test success path
	suite.mockedJobFactory.EXPECT().
		GetJob(gomock.Any()).Return(suite.mockedCachedJob)
	suite.mockedCachedJob.EXPECT().
		GetTask(instanceID).Return(suite.mockedTask)
	suite.mockedTask.EXPECT().
		GetRunTime(gomock.Any()).Return(suite.taskInfos[instanceID].Runtime, nil)
	resp, err := suite.handler.GetCache(context.Background(), &task.GetCacheRequest{
		JobId:      suite.testJobID,
		InstanceId: instanceID,
	})
	suite.NoError(err)
	suite.Equal(resp.Runtime.State, task.TaskState_RUNNING)
}

// TestRestartNonLeader tests restart call on a non leader jobmgr
func (suite *TaskHandlerTestSuite) TestRestartNonLeader() {
	suite.mockedCandidate.EXPECT().IsLeader().Return(false)
	var request = &task.RestartRequest{
		JobId: suite.testJobID,
	}
	resp, err := suite.handler.Restart(
		context.Background(),
		request,
	)

	suite.Error(err)
	suite.Nil(resp)
}

// TestRestartAllTasks tests restart all tasks
func (suite *TaskHandlerTestSuite) TestRestartAllTasks() {
	expectedTaskIds := make(map[*mesos.TaskID]bool)
	for _, taskInfo := range suite.taskInfos {
		expectedTaskIds[taskInfo.GetRuntime().GetMesosTaskId()] = true
	}

	var taskInfos = make(map[uint32]*task.TaskInfo)
	var tasksInfoList []*task.TaskInfo
	for i := uint32(0); i < testInstanceCount; i++ {
		taskInfos[i] = suite.createTestTaskInfo(
			task.TaskState_RUNNING, i)
		tasksInfoList = append(tasksInfoList, taskInfos[i])
	}

	gomock.InOrder(
		suite.mockedCandidate.EXPECT().IsLeader().Return(true),
		suite.mockedJobFactory.EXPECT().
			AddJob(suite.testJobID).Return(suite.mockedCachedJob),
		suite.mockedCachedJob.EXPECT().
			ID().Return(suite.testJobID),
		suite.mockedTaskStore.EXPECT().
			GetTasksForJob(gomock.Any(), suite.testJobID).Return(taskInfos, nil),
		suite.mockedCachedJob.EXPECT().
			ID().Return(suite.testJobID).Times(len(taskInfos)),
		suite.mockedCachedJob.EXPECT().
			PatchTasks(gomock.Any(), gomock.Any()).Return(nil),
		suite.mockedGoalStateDrive.EXPECT().
			EnqueueTask(suite.testJobID, gomock.Any(), gomock.Any()).Return().AnyTimes(),
	)

	var request = &task.RestartRequest{
		JobId: suite.testJobID,
	}
	resp, err := suite.handler.Restart(
		context.Background(),
		request,
	)

	suite.NoError(err)
	suite.NotNil(resp)
}
