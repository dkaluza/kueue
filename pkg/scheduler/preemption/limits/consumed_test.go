/*
Copyright The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package limits

import (
	"testing"

	"github.com/google/go-cmp/cmp"

	kueuealpha "sigs.k8s.io/kueue/apis/kueue/v1alpha1"
	"sigs.k8s.io/kueue/pkg/scheduler/preemption/policy"
	utiltestingalpha "sigs.k8s.io/kueue/pkg/util/testing/v1alpha1"
	utiltestingapi "sigs.k8s.io/kueue/pkg/util/testing/v1beta2"
	"sigs.k8s.io/kueue/pkg/workload"
)

func TestConsumedLimits(t *testing.T) {
	preemptor := &workload.Info{
		Obj:          utiltestingapi.MakeWorkload("preemptor", "ns").Obj(),
		ClusterQueue: "preempting-cq",
	}
	target := &workload.Info{
		Obj:          utiltestingapi.MakeWorkload("target", "ns").Obj(),
		ClusterQueue: "preempted-cq",
	}

	cases := map[string]struct {
		preemptionLimits []kueuealpha.PreemptionLimit
		want             map[policy.PreemptionLimitReference]policy.PreemptionLimitScopeValue
	}{
		"no limits": {},
		"each scope resolves its scope value": {
			preemptionLimits: []kueuealpha.PreemptionLimit{
				*utiltestingalpha.MakePreemptionLimit("global", kueuealpha.GlobalPreemptionLimitScope).Obj(),
				*utiltestingalpha.MakePreemptionLimit("preempting-cq", kueuealpha.PreemptingCQLimitScope).Obj(),
				*utiltestingalpha.MakePreemptionLimit("preempted-cq", kueuealpha.PreemptedCQLimitScope).Obj(),
				*utiltestingalpha.MakePreemptionLimit("preempted-workload", kueuealpha.PreemptedWorkloadLimitScope).Obj(),
			},
			want: map[policy.PreemptionLimitReference]policy.PreemptionLimitScopeValue{
				"global":             "Global",
				"preempting-cq":      "preempting-cq",
				"preempted-cq":       "preempted-cq",
				"preempted-workload": "ns/target",
			},
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			got := ConsumedLimits(tc.preemptionLimits, preemptor, target)
			if diff := cmp.Diff(tc.want, got); diff != "" {
				t.Errorf("Unexpected consumed limits (-want,+got):\n%s", diff)
			}
		})
	}
}
