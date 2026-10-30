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

	"github.com/go-logr/logr"

	kueuealpha "sigs.k8s.io/kueue/apis/kueue/v1alpha1"
	preemptexpectations "sigs.k8s.io/kueue/pkg/scheduler/preemption/expectations"
	"sigs.k8s.io/kueue/pkg/scheduler/preemption/policy"
	utiltestingalpha "sigs.k8s.io/kueue/pkg/util/testing/v1alpha1"
)

func TestPreemptionLimitsSnapshotRecord(t *testing.T) {
	now := time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)
	globalLimit := utiltestingalpha.MakePreemptionLimit("global", global).
		Limit(5).LimitWindowSeconds(60).Obj()
	perCQLimit := utiltestingalpha.MakePreemptionLimit("per-cq", kueuealpha.PreemptedCQLimitScope).
		Limit(3).LimitWindowSeconds(60).Obj()
	globalConsumed := policy.NewConsumedLimit(globalLimit, "Global")
	perCQConsumed := func(cq policy.PreemptionLimitScopeValue) policy.ConsumedLimit {
		return policy.NewConsumedLimit(perCQLimit, cq)
	}

	cases := map[string]struct {
		limits trackedLimits
		// recorded are the preemptions recorded in the snapshot after it was taken.
		recorded      []policy.ConsumedLimits
		wantAllowance map[policy.PreemptionLimitReference]map[policy.PreemptionLimitScopeValue]int32
	}{
		"preemptions recorded in the snapshot are counted in allowance along with tracker events": {
			limits: tracked(
				trackedLimit(globalLimit, nil),
				trackedLimit(perCQLimit, scopeEvents{"cq-a": {now.Add(-30 * time.Second)}}),
			),
			recorded: []policy.ConsumedLimits{{"per-cq": perCQConsumed("cq-a")}},
			wantAllowance: map[policy.PreemptionLimitReference]map[policy.PreemptionLimitScopeValue]int32{
				"per-cq": {"cq-a": 1},
			},
		},
		"preemptions recorded in the snapshot are counted only for their scope value": {
			limits:   tracked(trackedLimit(globalLimit, nil), trackedLimit(perCQLimit, nil)),
			recorded: []policy.ConsumedLimits{{"per-cq": perCQConsumed("cq-a")}},
			wantAllowance: map[policy.PreemptionLimitReference]map[policy.PreemptionLimitScopeValue]int32{
				"per-cq": {"cq-b": 3},
			},
		},
		"multiple consumed limits are all counted in allowance": {
			limits: tracked(
				trackedLimit(globalLimit, scopeEvents{"Global": {now.Add(-30 * time.Second)}}),
				trackedLimit(perCQLimit, nil),
			),
			recorded: []policy.ConsumedLimits{
				{"global": globalConsumed, "per-cq": perCQConsumed("cq-a")},
				{"global": globalConsumed, "per-cq": perCQConsumed("cq-b")},
			},
			wantAllowance: map[policy.PreemptionLimitReference]map[policy.PreemptionLimitScopeValue]int32{
				"global": {"Global": 2},
				"per-cq": {"cq-a": 2, "cq-b": 2},
			},
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			snapshot := snapshotOf(now, tc.limits)
			for _, consumed := range tc.recorded {
				snapshot.Record(consumed)
			}
			limiter, err := NewPreemptionLimiter(logr.Discard(), snapshot, utiltestingalpha.MakePreemptionConfig("config").Obj(), preemptexpectations.New())
			if err != nil {
				t.Fatalf("NewPreemptionLimiter() unexpected error: %v", err)
			}
			for limit, scopes := range tc.wantAllowance {
				for scope, want := range scopes {
					if got := limiter.allowance(limit, scope); got != want {
						t.Errorf("allowance(%q, %q) = %d, want %d", limit, scope, got, want)
					}
				}
			}
		})
	}
}
