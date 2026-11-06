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
	global := utiltestingalpha.MakePreemptionLimit("global", kueuealpha.GlobalPreemptionLimitScope).
		Limit(3).LimitWindowSeconds(60)
	globalOne := global.Clone().Limit(1)
	perCQ := utiltestingalpha.MakePreemptionLimit("per-cq", kueuealpha.PreemptedCQLimitScope).
		Limit(1).LimitWindowSeconds(60)
	globalConsumed := policy.NewConsumedLimit(global.Obj(), policy.GlobalScopeValue)
	perCQConsumed := policy.NewConsumedLimit(perCQ.Obj(), "preempted-cq")
	twoRecordedGlobalEvents := EventsByLimit{
		"global": {policy.GlobalScopeValue: {now.Add(-2 * time.Minute), now.Add(-30 * time.Second), now}},
	}
	preemptorCQ := &schdcache.ClusterQueueSnapshot{Name: "preempting-cq"}
	targetCQ := &schdcache.ClusterQueueSnapshot{Name: "preempted-cq"}
	preemptor := &workload.Info{Obj: utiltestingapi.MakeWorkload("preemptor", "ns").Obj(), ClusterQueue: "preempting-cq"}
	targetWl := utiltestingapi.MakeWorkload("target", "ns").UID("target")
	target := &workload.Info{Obj: targetWl.Clone().Obj(), ClusterQueue: "preempted-cq"}

	cases := map[string]struct {
		preemptionLimits []kueuealpha.PreemptionLimit
		events           EventsByLimit
		taken            preemptionCounts
		// target, when set, replaces the default target.
		target       *workload.Info
		wantConsumed policy.ConsumedLimits
		wantOK       bool
		wantTaken    preemptionCounts
		wantRetryAt  *time.Time
	}{
		"target consuming no limit is allowed and takes nothing": {
			wantOK: true,
		},
		"consumed limit is taken from the budget": {
			preemptionLimits: []kueuealpha.PreemptionLimit{*global.Obj()},
			wantConsumed:     policy.ConsumedLimits{"global": globalConsumed},
			wantOK:           true,
			wantTaken:        preemptionCounts{"global": {policy.GlobalScopeValue: 1}},
		},
		"consumed limit is added to preemptions already taken from the budget": {
			preemptionLimits: []kueuealpha.PreemptionLimit{*global.Obj()},
			taken:            preemptionCounts{"global": {policy.GlobalScopeValue: 2}},
			wantConsumed:     policy.ConsumedLimits{"global": globalConsumed},
			wantOK:           true,
			wantTaken:        preemptionCounts{"global": {policy.GlobalScopeValue: 3}},
		},
		"budget allows preemption if there are fewer recorded events than the limit": {
			preemptionLimits: []kueuealpha.PreemptionLimit{*global.Obj()},
			events:           twoRecordedGlobalEvents,
			wantConsumed:     policy.ConsumedLimits{"global": globalConsumed},
			wantOK:           true,
			wantTaken:        preemptionCounts{"global": {policy.GlobalScopeValue: 1}},
		},
		"target is blocked when recorded and taken preemptions reach the limit": {
			preemptionLimits: []kueuealpha.PreemptionLimit{*global.Obj()},
			events:           twoRecordedGlobalEvents,
			taken:            preemptionCounts{"global": {policy.GlobalScopeValue: 1}},
			wantOK:           false,
			wantTaken:        preemptionCounts{"global": {policy.GlobalScopeValue: 1}},
			wantRetryAt:      new(now.Add(30 * time.Second)),
		},
		"limit of 0 blocks the target": {
			preemptionLimits: []kueuealpha.PreemptionLimit{*global.Clone().Limit(0).Obj()},
			wantOK:           false,
		},
		"multiple applicable limits are all consumed and taken from the budget": {
			preemptionLimits: []kueuealpha.PreemptionLimit{*global.Obj(), *perCQ.Obj()},
			wantConsumed:     policy.ConsumedLimits{"global": globalConsumed, "per-cq": perCQConsumed},
			wantOK:           true,
			wantTaken: preemptionCounts{
				"global": {policy.GlobalScopeValue: 1},
				"per-cq": {"preempted-cq": 1},
			},
		},
		"exhausted limit blocks the target and takes nothing from other limits": {
			preemptionLimits: []kueuealpha.PreemptionLimit{*global.Obj(), *perCQ.Obj()},
			taken:            preemptionCounts{"per-cq": {"preempted-cq": 1}},
			wantOK:           false,
			wantTaken:        preemptionCounts{"per-cq": {"preempted-cq": 1}},
		},
		"scope values of a limit have independent budgets": {
			preemptionLimits: []kueuealpha.PreemptionLimit{*perCQ.Obj()},
			taken:            preemptionCounts{"per-cq": {"other-cq": 1}},
			wantConsumed:     policy.ConsumedLimits{"per-cq": perCQConsumed},
			wantOK:           true,
			wantTaken:        preemptionCounts{"per-cq": {"other-cq": 1, "preempted-cq": 1}},
		},
		"target with preemption in progress is allowed even when budget is exhausted": {
			preemptionLimits: []kueuealpha.PreemptionLimit{*global.Obj()},
			taken:            preemptionCounts{"global": {policy.GlobalScopeValue: 3}},
			target:           &workload.Info{Obj: targetWl.Clone().EvictedAt(now).Obj(), ClusterQueue: "preempted-cq"},
			wantOK:           true,
			wantTaken:        preemptionCounts{"global": {policy.GlobalScopeValue: 3}},
		},
		"event exactly one window old no longer blocks the target": {
			preemptionLimits: []kueuealpha.PreemptionLimit{*globalOne.Obj()},
			events:           EventsByLimit{"global": {policy.GlobalScopeValue: {now.Add(-60 * time.Second)}}},
			wantConsumed:     policy.ConsumedLimits{"global": globalConsumed},
			wantOK:           true,
			wantTaken:        preemptionCounts{"global": {policy.GlobalScopeValue: 1}},
		},
		"multiple blocking limits on a target return the latest retry time": {
			preemptionLimits: []kueuealpha.PreemptionLimit{*globalOne.Obj(), *perCQ.Obj()},
			events: EventsByLimit{
				"global": {policy.GlobalScopeValue: {now.Add(-50 * time.Second)}},
				"per-cq": {"preempted-cq": {now.Add(-15 * time.Second)}},
			},
			wantOK:      false,
			wantRetryAt: new(now.Add(45 * time.Second)),
		},
		"non-expiring blocking limit overrides expiring limit on the same target": {
			preemptionLimits: []kueuealpha.PreemptionLimit{*global.Clone().Limit(0).Obj(), *perCQ.Obj()},
			events: EventsByLimit{
				"per-cq": {"preempted-cq": {now.Add(-50 * time.Second)}},
			},
			wantOK: false,
		},
		"taken preemptions alone reaching the limit return nil retry time": {
			preemptionLimits: []kueuealpha.PreemptionLimit{*global.Clone().Limit(2).Obj()},
			events:           EventsByLimit{"global": {policy.GlobalScopeValue: {now.Add(-30 * time.Second)}}},
			taken:            preemptionCounts{"global": {policy.GlobalScopeValue: 2}},
			wantOK:           false,
			wantTaken:        preemptionCounts{"global": {policy.GlobalScopeValue: 2}},
		},
		"recorded and taken preemptions together wait for enough recorded events to expire": {
			preemptionLimits: []kueuealpha.PreemptionLimit{*global.Clone().Limit(2).Obj()},
			events:           EventsByLimit{"global": {policy.GlobalScopeValue: {now.Add(-40 * time.Second), now.Add(-10 * time.Second)}}},
			taken:            preemptionCounts{"global": {policy.GlobalScopeValue: 1}},
			wantOK:           false,
			wantTaken:        preemptionCounts{"global": {policy.GlobalScopeValue: 1}},
			wantRetryAt:      new(now.Add(50 * time.Second)),
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			limiter, err := NewPreemptionLimiter(logr.Discard(), SnapshotForTest(now, tc.preemptionLimits, tc.events), utiltestingalpha.MakePreemptionConfig("config").Obj(),
				preemptexpectations.New())
			if err != nil {
				t.Fatalf("NewPreemptionLimiter() unexpected error: %v", err)
			}
			budget := limiter.NewBudget()
			if tc.taken != nil {
				budget.taken = tc.taken
			}

			gotConsumed, gotRetryAt, ok := budget.TryConsume(preemptor, cmp.Or(tc.target, target), preemptorCQ, targetCQ)

			if ok != tc.wantOK {
				t.Errorf("TryConsume() ok = %v, want %v", ok, tc.wantOK)
			}
			if diff := gocmp.Diff(tc.wantConsumed, gotConsumed, gocmp.AllowUnexported(policy.ConsumedLimit{})); diff != "" {
				t.Errorf("Unexpected consumed limits (-want,+got):\n%s", diff)
			}
			if diff := gocmp.Diff(tc.wantTaken, budget.taken, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("Unexpected taken preemptions (-want,+got):\n%s", diff)
			}
			if diff := gocmp.Diff(tc.wantRetryAt, gotRetryAt); diff != "" {
				t.Errorf("Unexpected retryAt (-want,+got):\n%s", diff)
			}
		})
	}
}
