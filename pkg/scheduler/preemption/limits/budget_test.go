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
	"cmp"
	"testing"
	"time"

	"github.com/go-logr/logr"
	gocmp "github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	kueuealpha "sigs.k8s.io/kueue/apis/kueue/v1alpha1"
	schdcache "sigs.k8s.io/kueue/pkg/cache/scheduler"
	preemptexpectations "sigs.k8s.io/kueue/pkg/scheduler/preemption/expectations"
	"sigs.k8s.io/kueue/pkg/scheduler/preemption/policy"
	utiltestingalpha "sigs.k8s.io/kueue/pkg/util/testing/v1alpha1"
	utiltestingapi "sigs.k8s.io/kueue/pkg/util/testing/v1beta2"
	"sigs.k8s.io/kueue/pkg/workload"
)

func TestPreemptionLimitBudgetTryConsume(t *testing.T) {
	now := time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)
	globalLimit := utiltestingalpha.MakePreemptionLimit("global", kueuealpha.GlobalPreemptionLimitScope).
		Limit(3).LimitWindowSeconds(60).Obj()
	perCQLimit := utiltestingalpha.MakePreemptionLimit("per-cq", kueuealpha.PreemptedCQLimitScope).
		Limit(1).LimitWindowSeconds(60).Obj()
	globalConsumed := policy.NewConsumedLimit(globalLimit, "Global")
	perCQConsumed := policy.NewConsumedLimit(perCQLimit, "preempted-cq")
	globalWithTwoEvents := trackedLimit(globalLimit, scopeEvents{"Global": {now.Add(-2 * time.Minute), now.Add(-30 * time.Second), now}})
	preemptorCQ := &schdcache.ClusterQueueSnapshot{Name: "preempting-cq"}
	targetCQ := &schdcache.ClusterQueueSnapshot{Name: "preempted-cq"}
	preemptor := &workload.Info{Obj: utiltestingapi.MakeWorkload("preemptor", "ns").Obj(), ClusterQueue: "preempting-cq"}
	targetWl := utiltestingapi.MakeWorkload("target", "ns").UID("target")
	target := &workload.Info{Obj: targetWl.Clone().Obj(), ClusterQueue: "preempted-cq"}

	cases := map[string]struct {
		limits trackedLimits
		taken  preemptionCounts
		// target, when set, replaces the default target.
		target       *workload.Info
		wantConsumed policy.ConsumedLimits
		wantOK       bool
		wantTaken    preemptionCounts
	}{
		"target consuming no limit is allowed and takes nothing": {
			wantOK: true,
		},
		"consumed limit is taken from the budget": {
			limits:       tracked(trackedLimit(globalLimit, nil)),
			wantConsumed: policy.ConsumedLimits{"global": globalConsumed},
			wantOK:       true,
			wantTaken:    preemptionCounts{"global": {"Global": 1}},
		},
		"consumed limit is added to preemptions already taken from the budget": {
			limits:       tracked(trackedLimit(globalLimit, nil)),
			taken:        preemptionCounts{"global": {"Global": 2}},
			wantConsumed: policy.ConsumedLimits{"global": globalConsumed},
			wantOK:       true,
			wantTaken:    preemptionCounts{"global": {"Global": 3}},
		},
		"budget allows preemption if there are fewer recorded events than the limit": {
			limits:       tracked(globalWithTwoEvents),
			wantConsumed: policy.ConsumedLimits{"global": globalConsumed},
			wantOK:       true,
			wantTaken:    preemptionCounts{"global": {"Global": 1}},
		},
		"target is blocked when recorded and taken preemptions reach the limit": {
			limits:    tracked(globalWithTwoEvents),
			taken:     preemptionCounts{"global": {"Global": 1}},
			wantOK:    false,
			wantTaken: preemptionCounts{"global": {"Global": 1}},
		},
		"limit of 0 blocks the target": {
			limits: tracked(trackedLimit(utiltestingalpha.MakePreemptionLimit("global", kueuealpha.GlobalPreemptionLimitScope).Limit(0).LimitWindowSeconds(60).Obj(), nil)),
			wantOK: false,
		},
		"multiple applicable limits are all consumed and taken from the budget": {
			limits:       tracked(trackedLimit(globalLimit, nil), trackedLimit(perCQLimit, nil)),
			wantConsumed: policy.ConsumedLimits{"global": globalConsumed, "per-cq": perCQConsumed},
			wantOK:       true,
			wantTaken: preemptionCounts{
				"global": {"Global": 1},
				"per-cq": {"preempted-cq": 1},
			},
		},
		"exhausted limit blocks the target and takes nothing from other limits": {
			limits:    tracked(trackedLimit(globalLimit, nil), trackedLimit(perCQLimit, nil)),
			taken:     preemptionCounts{"per-cq": {"preempted-cq": 1}},
			wantOK:    false,
			wantTaken: preemptionCounts{"per-cq": {"preempted-cq": 1}},
		},
		"scope values of a limit have independent budgets": {
			limits:       tracked(trackedLimit(perCQLimit, nil)),
			taken:        preemptionCounts{"per-cq": {"other-cq": 1}},
			wantConsumed: policy.ConsumedLimits{"per-cq": perCQConsumed},
			wantOK:       true,
			wantTaken:    preemptionCounts{"per-cq": {"other-cq": 1, "preempted-cq": 1}},
		},
		"target with preemption in progress is allowed even when budget is exhausted": {
			limits:    tracked(trackedLimit(globalLimit, nil)),
			taken:     preemptionCounts{"global": {"Global": 3}},
			target:    &workload.Info{Obj: targetWl.Clone().EvictedAt(now).Obj(), ClusterQueue: "preempted-cq"},
			wantOK:    true,
			wantTaken: preemptionCounts{"global": {"Global": 3}},
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			limiter, err := NewPreemptionLimiter(logr.Discard(), snapshotOf(now, tc.limits), utiltestingalpha.MakePreemptionConfig("config").Obj(),
				preemptexpectations.New())
			if err != nil {
				t.Fatalf("NewPreemptionLimiter() unexpected error: %v", err)
			}
			budget := limiter.NewBudget()
			if tc.taken != nil {
				budget.taken = tc.taken
			}

			gotConsumed, ok := budget.TryConsume(preemptor, cmp.Or(tc.target, target), preemptorCQ, targetCQ)

			if ok != tc.wantOK {
				t.Errorf("TryConsume() ok = %v, want %v", ok, tc.wantOK)
			}
			if diff := gocmp.Diff(tc.wantConsumed, gotConsumed, gocmp.AllowUnexported(policy.ConsumedLimit{})); diff != "" {
				t.Errorf("Unexpected consumed limits (-want,+got):\n%s", diff)
			}
			if diff := gocmp.Diff(tc.wantTaken, budget.taken, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("Unexpected taken preemptions (-want,+got):\n%s", diff)
			}
		})
	}
}
