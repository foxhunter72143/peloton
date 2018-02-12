package mimir

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"code.uber.internal/infra/peloton/mimir-lib/model/labels"
	"code.uber.internal/infra/peloton/mimir-lib/model/metrics"
	"code.uber.internal/infra/peloton/mimir-lib/model/requirements"
	"code.uber.internal/infra/peloton/placement/testutil"
)

func TestEntityMapper_Convert(t *testing.T) {
	task := testutil.SetupAssignment(time.Now(), 1).GetTask().GetTask()
	entity := TaskToEntity(task, false)
	assert.Equal(t, "id", entity.Name)
	assert.Equal(t, 1, entity.Relations.Count(labels.NewLabel("relationKey", "relationValue")))
	assert.NotNil(t, entity.Ordering)

	assert.Equal(t, 3200.0, entity.Metrics.Get(CPUReserved))
	assert.Equal(t, 1000.0, entity.Metrics.Get(GPUReserved))
	assert.Equal(t, 4096.0*metrics.MiB, entity.Metrics.Get(MemoryReserved))
	assert.Equal(t, 1024.0*metrics.MiB, entity.Metrics.Get(DiskReserved))
	assert.Equal(t, 3.0, entity.Metrics.Get(PortsReserved))

	and1, ok := entity.Requirement.(*requirements.AndRequirement)
	assert.True(t, ok)
	assert.NotNil(t, and1)
	assert.Equal(t, 6, len(and1.Requirements))

	or, ok := and1.Requirements[0].(*requirements.OrRequirement)
	assert.True(t, ok)
	assert.NotNil(t, or)
	assert.Equal(t, 1, len(or.Requirements))
	and2, ok := or.Requirements[0].(*requirements.AndRequirement)
	assert.True(t, ok)
	assert.NotNil(t, and2)
	assert.Equal(t, 2, len(and2.Requirements))

	label, ok := and2.Requirements[0].(*requirements.LabelRequirement)
	assert.True(t, ok)
	assert.NotNil(t, label)
	assert.Nil(t, label.Scope)
	assert.Equal(t, labels.NewLabel("key1", "value1"), label.Label)
	assert.Equal(t, requirements.LessThan, label.Comparison)
	assert.Equal(t, 1, label.Occurrences)

	relation, ok := and2.Requirements[1].(*requirements.RelationRequirement)
	assert.True(t, ok)
	assert.NotNil(t, relation)
	assert.Nil(t, relation.Scope)
	assert.Equal(t, labels.NewLabel("key2", "value2"), relation.Relation)
	assert.Equal(t, requirements.LessThan, relation.Comparison)
	assert.Equal(t, 1, relation.Occurrences)

	for _, r := range and1.Requirements[1:] {
		requirement, ok := r.(*requirements.MetricRequirement)
		assert.True(t, ok)
		assert.NotNil(t, requirement)
		assert.Equal(t, requirements.GreaterThanEqual, requirement.Comparison)
		switch requirement.MetricType {
		case CPUFree:
			assert.Equal(t, 3200.0, requirement.Value)
		case GPUFree:
			assert.Equal(t, 1000.0, requirement.Value)
		case MemoryFree:
			assert.Equal(t, 4096.0*metrics.MiB, requirement.Value)
		case DiskFree:
			assert.Equal(t, 1024.0*metrics.MiB, requirement.Value)
		case PortsFree:
			assert.Equal(t, 3.0, requirement.Value)
		}
	}
}