// Copyright (c) 2019 Uber Technologies, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//    http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package ptoa

import (
	"github.com/uber/peloton/.gen/peloton/api/v1alpha/job/stateless"
	"github.com/uber/peloton/.gen/thrift/aurora/api"
)

// NewJobUpdateInstructions gets new job update instructions
// Since Aggregator ignores task config, it is not set.
func NewJobUpdateInstructions(
	w *stateless.WorkflowInfo,
) *api.JobUpdateInstructions {

	return &api.JobUpdateInstructions{
		InitialState: []*api.InstanceTaskConfig{
			&api.InstanceTaskConfig{
				Instances: NewRange(append(w.GetInstancesUpdated(),
					w.GetInstancesRemoved()...)),
			},
		},
		DesiredState: &api.InstanceTaskConfig{
			Instances: NewRange(append(w.GetInstancesUpdated(),
				w.GetInstancesAdded()...)),
		},
	}
}
