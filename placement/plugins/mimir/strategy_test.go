package mimir

import (
	"testing"
	"time"

	"code.uber.internal/infra/peloton/.gen/peloton/private/resmgr"
	lib_mimir "code.uber.internal/infra/peloton/mimir-lib"
	"code.uber.internal/infra/peloton/mimir-lib/model/metrics"
	"code.uber.internal/infra/peloton/placement/config"
	"code.uber.internal/infra/peloton/placement/models"
	"code.uber.internal/infra/peloton/placement/testutil"
	"github.com/stretchr/testify/assert"
)

func setupStrategy() *mimir {
	config := &config.PlacementConfig{
		TaskDequeueLimit:     10,
		OfferDequeueLimit:    10,
		MaxPlacementDuration: 30 * time.Second,
		TaskDequeueTimeOut:   100,
		TaskType:             resmgr.TaskType_BATCH,
		FetchOfferTasks:      false,
	}
	deriver := metrics.NewDeriver([]metrics.FreeMetricTuple{
		{metrics.CPUFree, metrics.CPUUsed, metrics.CPUTotal},
		{metrics.MemoryFree, metrics.MemoryUsed, metrics.MemoryTotal},
		{metrics.DiskFree, metrics.DiskUsed, metrics.DiskTotal},
		{metrics.GPUFree, metrics.GPUUsed, metrics.GPUTotal},
		{metrics.PortsFree, metrics.PortsUsed, metrics.PortsTotal},
	})
	placer := lib_mimir.NewPlacer(deriver)
	return New(placer, config).(*mimir)
}

func TestMimirPlace(t *testing.T) {
	assignments := []*models.Assignment{
		testutil.SetupAssignment(time.Now().Add(10*time.Second), 1),
		testutil.SetupAssignment(time.Now().Add(10*time.Second), 1),
	}
	offers := []*models.Host{
		testutil.SetupHost(),
	}
	strategy := setupStrategy()
	strategy.PlaceOnce(assignments, offers)

	assert.Equal(t, offers[0], assignments[0].GetHost())
	assert.Nil(t, assignments[1].GetHost())
}

func TestMimirFilters(t *testing.T) {
	strategy := setupStrategy()

	deadline := time.Now().Add(30 * time.Second)
	assignments := []*models.Assignment{
		testutil.SetupAssignment(deadline, 1),
	}
	taskTypeToExpectedMaxHosts := map[resmgr.TaskType]uint32{
		resmgr.TaskType_BATCH:     2,
		resmgr.TaskType_STATELESS: 3,
		resmgr.TaskType_DAEMON:    3,
		resmgr.TaskType_STATEFUL:  3,
	}
	for taskType, expectedMaxHosts := range taskTypeToExpectedMaxHosts {
		strategy.config.TaskType = taskType
		for filter := range strategy.Filters(assignments) {
			assert.NotNil(t, filter)
			assert.Equal(t, expectedMaxHosts, filter.GetQuantity().GetMaxHosts())
			assert.Equal(t, uint32(3), filter.GetResourceConstraint().GetNumPorts())
			assert.Equal(t, 32.0, filter.GetResourceConstraint().GetMinimum().GetCpuLimit())
			assert.Equal(t, 10.0, filter.GetResourceConstraint().GetMinimum().GetGpuLimit())
			assert.Equal(t, 4096.0, filter.GetResourceConstraint().GetMinimum().GetMemLimitMb())
			assert.Equal(t, 1024.0, filter.GetResourceConstraint().GetMinimum().GetDiskLimitMb())
		}
	}
}
