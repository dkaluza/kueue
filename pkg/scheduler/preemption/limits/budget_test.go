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
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	kueuealpha "sigs.k8s.io/kueue/apis/kueue/v1alpha1"
	"sigs.k8s.io/kueue/pkg/scheduler/preemption/policy"
	utiltestingalpha "sigs.k8s.io/kueue/pkg/util/testing/v1alpha1"
)

func TestPreemptionLimitBudgetTryConsume(t *testing.T) {
	now := time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)
	global := utiltestingalpha.MakePreemptionLimit("global", kueuealpha.GlobalPreemptionLimitScope).
		Limit(3).LimitWindowSeconds(60)
	perCQ := utiltestingalpha.MakePreemptionLimit("per-cq", kueuealpha.PreemptedCQLimitScope).
		Limit(1).LimitWindowSeconds(60)

	cases := map[string]struct {
		preemptionLimits []kueuealpha.PreemptionLimit
		events           map[policy.PreemptionLimitReference]map[policy.PreemptionLimitScopeValue]events
		remaining        map[policy.PreemptionLimitReference]map[policy.PreemptionLimitScopeValue]int32
		consumedLimits   policy.ConsumedLimits
		wantBlockedBy    policy.PreemptionLimitReference
		wantRemaining    map[policy.PreemptionLimitReference]map[policy.PreemptionLimitScopeValue]int32
	}{
		"budget is read from the tracker on first use": {
			preemptionLimits: []kueuealpha.PreemptionLimit{*global.Obj()},
			events: map[policy.PreemptionLimitReference]map[policy.PreemptionLimitScopeValue]events{
				"global": {"Global": {now.Add(-2 * time.Minute), now.Add(-30 * time.Second)}},
			},
			consumedLimits: policy.ConsumedLimits{"global": "Global"},
			wantRemaining: map[policy.PreemptionLimitReference]map[policy.PreemptionLimitScopeValue]int32{
				"global": {"Global": 1},
			},
		},
		"budget already read is not read from the tracker again": {
			preemptionLimits: []kueuealpha.PreemptionLimit{*global.Obj()},
			events: map[policy.PreemptionLimitReference]map[policy.PreemptionLimitScopeValue]events{
				"global": {"Global": {now, now, now}},
			},
			remaining: map[policy.PreemptionLimitReference]map[policy.PreemptionLimitScopeValue]int32{
				"global": {"Global": 2},
			},
			consumedLimits: policy.ConsumedLimits{"global": "Global"},
			wantRemaining: map[policy.PreemptionLimitReference]map[policy.PreemptionLimitScopeValue]int32{
				"global": {"Global": 1},
			},
		},
		"limit of 0 has no budget": {
			preemptionLimits: []kueuealpha.PreemptionLimit{*global.Clone().Limit(0).Obj()},
			consumedLimits:   policy.ConsumedLimits{"global": "Global"},
			wantBlockedBy:    "global",
			wantRemaining: map[policy.PreemptionLimitReference]map[policy.PreemptionLimitScopeValue]int32{
				"global": {"Global": 0},
			},
		},
		"every consumed limit is taken from": {
			preemptionLimits: []kueuealpha.PreemptionLimit{*global.Obj(), *perCQ.Obj()},
			consumedLimits:   policy.ConsumedLimits{"global": "Global", "per-cq": "cq-a"},
			wantRemaining: map[policy.PreemptionLimitReference]map[policy.PreemptionLimitScopeValue]int32{
				"global": {"Global": 2},
				"per-cq": {"cq-a": 0},
			},
		},
		"limit without budget left blocks, and nothing is taken from the others": {
			preemptionLimits: []kueuealpha.PreemptionLimit{*global.Obj(), *perCQ.Obj()},
			remaining: map[policy.PreemptionLimitReference]map[policy.PreemptionLimitScopeValue]int32{
				"global": {"Global": 3},
				"per-cq": {"cq-a": 0},
			},
			consumedLimits: policy.ConsumedLimits{"global": "Global", "per-cq": "cq-a"},
			wantBlockedBy:  "per-cq",
			wantRemaining: map[policy.PreemptionLimitReference]map[policy.PreemptionLimitScopeValue]int32{
				"global": {"Global": 3},
				"per-cq": {"cq-a": 0},
			},
		},
		"scope values of a limit have independent budgets": {
			preemptionLimits: []kueuealpha.PreemptionLimit{*perCQ.Obj()},
			remaining: map[policy.PreemptionLimitReference]map[policy.PreemptionLimitScopeValue]int32{
				"per-cq": {"cq-a": 0},
			},
			consumedLimits: policy.ConsumedLimits{"per-cq": "cq-b"},
			wantRemaining: map[policy.PreemptionLimitReference]map[policy.PreemptionLimitScopeValue]int32{
				"per-cq": {"cq-a": 0, "cq-b": 0},
			},
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			tracker := NewPreemptionLimitTracker()
			if tc.events != nil {
				tracker.events = tc.events
			}
			budget := NewPreemptionLimitBudget(tracker, now, tc.preemptionLimits)
			if tc.remaining != nil {
				budget.remaining = tc.remaining
			}

			blockedBy, ok := budget.TryConsume(tc.consumedLimits)

			if blockedBy != tc.wantBlockedBy || ok != (tc.wantBlockedBy == "") {
				t.Errorf("TryConsume() = (%q, %v), want (%q, %v)", blockedBy, ok, tc.wantBlockedBy, tc.wantBlockedBy == "")
			}
			if diff := cmp.Diff(tc.wantRemaining, budget.remaining, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("Unexpected remaining budget (-want,+got):\n%s", diff)
			}
		})
	}
}
