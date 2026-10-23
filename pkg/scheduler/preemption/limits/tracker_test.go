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
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"k8s.io/apimachinery/pkg/types"

	kueuealpha "sigs.k8s.io/kueue/apis/kueue/v1alpha1"
	"sigs.k8s.io/kueue/pkg/scheduler/preemption/policy"
	utiltestingalpha "sigs.k8s.io/kueue/pkg/util/testing/v1alpha1"
	utiltestingapi "sigs.k8s.io/kueue/pkg/util/testing/v1beta2"
	"sigs.k8s.io/kueue/pkg/workload"
)

func TestResolve(t *testing.T) {
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
		want             policy.ConsumedLimits
	}{
		"no limits": {},
		"single limit resolves its scope value": {
			preemptionLimits: []kueuealpha.PreemptionLimit{
				*utiltestingalpha.MakePreemptionLimit("preempted-workload", kueuealpha.PreemptedWorkloadLimitScope).UID("uid").Obj(),
			},
			want: policy.ConsumedLimits{
				"preempted-workload": consumedLimit("uid", kueuealpha.PreemptedWorkloadLimitScope, "ns/target"),
			},
		},
		"each scope resolves its scope value": {
			preemptionLimits: []kueuealpha.PreemptionLimit{
				*utiltestingalpha.MakePreemptionLimit("global", kueuealpha.GlobalPreemptionLimitScope).UID("uid-a").Obj(),
				*utiltestingalpha.MakePreemptionLimit("preempting-cq", kueuealpha.PreemptingCQLimitScope).UID("uid-b").Obj(),
				*utiltestingalpha.MakePreemptionLimit("preempted-cq", kueuealpha.PreemptedCQLimitScope).UID("uid-c").Obj(),
				*utiltestingalpha.MakePreemptionLimit("preempted-workload", kueuealpha.PreemptedWorkloadLimitScope).UID("uid-d").Obj(),
			},
			want: policy.ConsumedLimits{
				"global":             consumedLimit("uid-a", kueuealpha.GlobalPreemptionLimitScope, "Global"),
				"preempting-cq":      consumedLimit("uid-b", kueuealpha.PreemptingCQLimitScope, "preempting-cq"),
				"preempted-cq":       consumedLimit("uid-c", kueuealpha.PreemptedCQLimitScope, "preempted-cq"),
				"preempted-workload": consumedLimit("uid-d", kueuealpha.PreemptedWorkloadLimitScope, "ns/target"),
			},
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			got := Resolve(tc.preemptionLimits, preemptor, target)
			if diff := cmp.Diff(tc.want, got); diff != "" {
				t.Errorf("Unexpected consumed limits (-want,+got):\n%s", diff)
			}
		})
	}
}

// Scopes of the PreemptionLimits used across the tracker tests.
const (
	global = kueuealpha.GlobalPreemptionLimitScope
	perCQ  = kueuealpha.PreemptingCQLimitScope
)

type scopeEvents = map[policy.PreemptionLimitScopeValue]events

type trackedLimits = map[policy.PreemptionLimitReference]*limitState

var cmpTrackedLimits = []cmp.Option{
	cmp.AllowUnexported(limitState{}),
	cmpopts.EquateEmpty(),
}

func trackedLimit(uid types.UID, scope kueuealpha.PreemptionLimitScope, events scopeEvents) *limitState {
	if events == nil {
		// As allocated by Sync.
		events = scopeEvents{}
	}
	return &limitState{uid: uid, scope: scope, events: events}
}

func consumedLimit(uid types.UID, scope kueuealpha.PreemptionLimitScope, scopeValue policy.PreemptionLimitScopeValue) policy.ConsumedLimit {
	return policy.ConsumedLimit{UID: uid, Scope: scope, ScopeValue: scopeValue}
}

func TestPreemptionLimitTrackerRecord(t *testing.T) {
	t1 := time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)
	t2 := t1.Add(time.Second)
	t3 := t2.Add(time.Second)

	cases := map[string]struct {
		limits   trackedLimits
		ts       time.Time
		consumed policy.ConsumedLimits
		want     trackedLimits
	}{
		"nil consumed limits is a no-op": {
			limits: trackedLimits{
				"global": trackedLimit("uid", global, scopeEvents{"Global": {t1}}),
			},
			ts: t2,
			want: trackedLimits{
				"global": trackedLimit("uid", global, scopeEvents{"Global": {t1}}),
			},
		},
		"every consumed limit is recorded": {
			limits: trackedLimits{
				"global": trackedLimit("uid-a", global, nil),
				"per-cq": trackedLimit("uid-b", perCQ, nil),
			},
			ts: t1,
			consumed: policy.ConsumedLimits{
				"global": consumedLimit("uid-a", global, "Global"),
				"per-cq": consumedLimit("uid-b", perCQ, "cq-a"),
			},
			want: trackedLimits{
				"global": trackedLimit("uid-a", global, scopeEvents{"Global": {t1}}),
				"per-cq": trackedLimit("uid-b", perCQ, scopeEvents{"cq-a": {t1}}),
			},
		},
		"untracked limit is skipped": {
			limits: trackedLimits{
				"global": trackedLimit("uid", global, nil),
			},
			ts: t1,
			consumed: policy.ConsumedLimits{
				"global": consumedLimit("uid", global, "Global"),
				"per-cq": consumedLimit("uid-b", perCQ, "cq-a"),
			},
			want: trackedLimits{
				"global": trackedLimit("uid", global, scopeEvents{"Global": {t1}}),
			},
		},
		"limit resolved before it was recreated is skipped": {
			limits: trackedLimits{
				"global": trackedLimit("uid", global, nil),
				"per-cq": trackedLimit("new-uid", perCQ, scopeEvents{"cq-a": {t1}}),
			},
			ts: t2,
			consumed: policy.ConsumedLimits{
				"global": consumedLimit("uid", global, "Global"),
				"per-cq": consumedLimit("old-uid", perCQ, "cq-a"),
			},
			want: trackedLimits{
				"global": trackedLimit("uid", global, scopeEvents{"Global": {t2}}),
				"per-cq": trackedLimit("new-uid", perCQ, scopeEvents{"cq-a": {t1}}),
			},
		},
		"limit resolved before its scope changed is skipped": {
			limits: trackedLimits{
				"global": trackedLimit("uid", global, nil),
				"per-cq": trackedLimit("uid-b", kueuealpha.PreemptedCQLimitScope, scopeEvents{"cq-a": {t1}}),
			},
			ts: t2,
			consumed: policy.ConsumedLimits{
				"global": consumedLimit("uid", global, "Global"),
				"per-cq": consumedLimit("uid-b", perCQ, "cq-a"),
			},
			want: trackedLimits{
				"global": trackedLimit("uid", global, scopeEvents{"Global": {t2}}),
				"per-cq": trackedLimit("uid-b", kueuealpha.PreemptedCQLimitScope, scopeEvents{"cq-a": {t1}}),
			},
		},
		"new scope value of a recorded limit": {
			limits: trackedLimits{
				"per-cq": trackedLimit("uid", perCQ, scopeEvents{"cq-a": {t1}}),
			},
			ts:       t2,
			consumed: policy.ConsumedLimits{"per-cq": consumedLimit("uid", perCQ, "cq-b")},
			want: trackedLimits{
				"per-cq": trackedLimit("uid", perCQ, scopeEvents{"cq-a": {t1}, "cq-b": {t2}}),
			},
		},
		"later event is appended": {
			limits: trackedLimits{
				"global": trackedLimit("uid", global, scopeEvents{"Global": {t1, t2}}),
			},
			ts:       t3,
			consumed: policy.ConsumedLimits{"global": consumedLimit("uid", global, "Global")},
			want: trackedLimits{
				"global": trackedLimit("uid", global, scopeEvents{"Global": {t1, t2, t3}}),
			},
		},
		"earlier event is inserted in order": {
			limits: trackedLimits{
				"global": trackedLimit("uid", global, scopeEvents{"Global": {t1, t3}}),
			},
			ts:       t2,
			consumed: policy.ConsumedLimits{"global": consumedLimit("uid", global, "Global")},
			want: trackedLimits{
				"global": trackedLimit("uid", global, scopeEvents{"Global": {t1, t2, t3}}),
			},
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			tracker := NewPreemptionLimitTracker()
			tracker.limits = tc.limits
			tracker.Record(tc.ts, tc.consumed)
			if diff := cmp.Diff(tc.want, tracker.limits, cmpTrackedLimits...); diff != "" {
				t.Errorf("Unexpected tracked limits (-want,+got):\n%s", diff)
			}
		})
	}
}

func TestPreemptionLimitTrackerRecordConcurrent(t *testing.T) {
	const goroutines, recordsPerGoroutine = 10, 100
	start := time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)
	consumed := policy.ConsumedLimits{"global": consumedLimit("uid", global, "Global")}

	tracker := NewPreemptionLimitTracker()
	tracker.Sync(start, utiltestingalpha.MakePreemptionLimit("global", global).UID("uid").Obj())
	var wg sync.WaitGroup
	for g := range goroutines {
		wg.Go(func() {
			for i := range recordsPerGoroutine {
				tracker.Record(start.Add(time.Duration(g*recordsPerGoroutine+i)*time.Second), consumed)
			}
		})
	}
	wg.Wait()

	got := tracker.limits["global"].events["Global"]
	if len(got) != goroutines*recordsPerGoroutine {
		t.Errorf("Recorded %d events, want %d", len(got), goroutines*recordsPerGoroutine)
	}
	if !slices.IsSortedFunc(got, time.Time.Compare) {
		t.Errorf("Events are not sorted: %v", got)
	}
}

func TestPreemptionLimitTrackerSync(t *testing.T) {
	const windowSeconds = 60
	const syncedLimit, otherLimit = "per-cq", "global"
	now := time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)
	window := windowSeconds * time.Second
	cutoff := now.Add(-window - pruneGracePeriod)
	expired := cutoff.Add(-time.Nanosecond)
	withinGracePeriod := now.Add(-window - pruneGracePeriod/2)
	// prunedOnlyWithHalfWindow is kept with a window of windowSeconds, but pruned
	// once the window is halved, as it is 1s older than the halved window's cutoff.
	prunedOnlyWithHalfWindow := now.Add(-window/2 - pruneGracePeriod - time.Second)
	// limit is the PreemptionLimit passed to Sync, unless updatedLimit is set.
	limit := utiltestingalpha.MakePreemptionLimit(syncedLimit, perCQ).UID("uid").LimitWindowSeconds(windowSeconds).Obj()

	cases := map[string]struct {
		// limits is the state of the tracker before Sync.
		limits trackedLimits
		// updatedLimit, if set, is passed to Sync instead of limit. It is an
		// updated version of the same PreemptionLimit, not a new one.
		updatedLimit *kueuealpha.PreemptionLimit
		// want is the state of the tracker after Sync.
		want trackedLimits
	}{
		"events before the cutoff are dropped, events at or after it are kept": {
			limits: trackedLimits{
				syncedLimit: trackedLimit("uid", perCQ, scopeEvents{"cq-a": {cutoff.Add(-time.Hour), expired, cutoff, now}}),
			},
			want: trackedLimits{
				syncedLimit: trackedLimit("uid", perCQ, scopeEvents{"cq-a": {cutoff, now}}),
			},
		},
		"events within the grace period are kept": {
			limits: trackedLimits{
				syncedLimit: trackedLimit("uid", perCQ, scopeEvents{"cq-a": {withinGracePeriod}}),
			},
			want: trackedLimits{
				syncedLimit: trackedLimit("uid", perCQ, scopeEvents{"cq-a": {withinGracePeriod}}),
			},
		},
		"scope value left without events is forgotten": {
			limits: trackedLimits{
				syncedLimit: trackedLimit("uid", perCQ, scopeEvents{"cq-a": {expired}, "cq-b": {now}}),
			},
			want: trackedLimits{
				syncedLimit: trackedLimit("uid", perCQ, scopeEvents{"cq-b": {now}}),
			},
		},
		"limit left without events is still tracked": {
			limits: trackedLimits{
				syncedLimit: trackedLimit("uid", perCQ, scopeEvents{"cq-a": {expired}, "cq-b": {expired}}),
			},
			want: trackedLimits{
				syncedLimit: trackedLimit("uid", perCQ, nil),
			},
		},
		"untracked limit starts being tracked": {
			want: trackedLimits{
				syncedLimit: trackedLimit("uid", perCQ, nil),
			},
		},
		"other limits are untouched": {
			limits: trackedLimits{
				syncedLimit: trackedLimit("uid", perCQ, scopeEvents{"cq-a": {expired, now}}),
				otherLimit:  trackedLimit("other-uid", global, scopeEvents{"Global": {expired}}),
			},
			want: trackedLimits{
				syncedLimit: trackedLimit("uid", perCQ, scopeEvents{"cq-a": {now}}),
				otherLimit:  trackedLimit("other-uid", global, scopeEvents{"Global": {expired}}),
			},
		},
		"limit and window increase keep the events": {
			limits: trackedLimits{
				syncedLimit: trackedLimit("uid", perCQ, scopeEvents{"cq-a": {expired, now}}),
			},
			updatedLimit: utiltestingalpha.MakePreemptionLimit(syncedLimit, perCQ).UID("uid").Limit(10).LimitWindowSeconds(2 * windowSeconds).Obj(),
			want: trackedLimits{
				syncedLimit: trackedLimit("uid", perCQ, scopeEvents{"cq-a": {expired, now}}),
			},
		},
		"window decrease keeps the events within the new window": {
			limits: trackedLimits{
				syncedLimit: trackedLimit("uid", perCQ, scopeEvents{"cq-a": {prunedOnlyWithHalfWindow, now}}),
			},
			updatedLimit: utiltestingalpha.MakePreemptionLimit(syncedLimit, perCQ).UID("uid").LimitWindowSeconds(windowSeconds / 2).Obj(),
			want: trackedLimits{
				syncedLimit: trackedLimit("uid", perCQ, scopeEvents{"cq-a": {now}}),
			},
		},
		"scope change resets the events": {
			limits: trackedLimits{
				syncedLimit: trackedLimit("uid", kueuealpha.PreemptedCQLimitScope, scopeEvents{"cq-a": {now}}),
			},
			want: trackedLimits{
				syncedLimit: trackedLimit("uid", perCQ, nil),
			},
		},
		"limit recreated with the same name resets the events": {
			limits: trackedLimits{
				syncedLimit: trackedLimit("old-uid", perCQ, scopeEvents{"cq-a": {now}}),
			},
			want: trackedLimits{
				syncedLimit: trackedLimit("uid", perCQ, nil),
			},
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			tracker := NewPreemptionLimitTracker()
			if tc.limits != nil {
				tracker.limits = tc.limits
			}
			limitToSync := limit
			if tc.updatedLimit != nil {
				limitToSync = tc.updatedLimit
			}
			tracker.Sync(now, limitToSync)
			if diff := cmp.Diff(tc.want, tracker.limits, cmpTrackedLimits...); diff != "" {
				t.Errorf("Unexpected tracked limits (-want,+got):\n%s", diff)
			}
		})
	}
}

func TestPreemptionLimitTrackerSyncConcurrent(t *testing.T) {
	const goroutines, recordsPerGoroutine = 10, 100
	const records = goroutines * recordsPerGoroutine
	const windowSeconds = 60
	const syncedLimit, otherLimit = "per-cq", "global"
	start := time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)
	window := windowSeconds * time.Second
	limit := utiltestingalpha.MakePreemptionLimit(syncedLimit, perCQ).UID("uid").LimitWindowSeconds(windowSeconds).Obj()
	consumed := policy.ConsumedLimits{
		syncedLimit: consumedLimit("uid", perCQ, "cq-a"),
		otherLimit:  consumedLimit("other-uid", global, "Global"),
	}
	// Every recorded event is already expired, so the syncs keep pruning the
	// synced limit while the records keep adding to it.
	now := start.Add(records*time.Second + window + pruneGracePeriod)

	tracker := NewPreemptionLimitTracker()
	tracker.Sync(now, utiltestingalpha.MakePreemptionLimit(otherLimit, global).UID("other-uid").Obj())
	var wg sync.WaitGroup
	for g := range goroutines {
		wg.Go(func() {
			for i := range recordsPerGoroutine {
				tracker.Sync(now, limit)
				tracker.Record(start.Add(time.Duration(g*recordsPerGoroutine+i)*time.Second), consumed)
			}
		})
	}
	wg.Wait()

	got := tracker.limits[otherLimit].events["Global"]
	if len(got) != records {
		t.Errorf("Recorded %d events of the other limit, want %d", len(got), records)
	}
	if !slices.IsSortedFunc(got, time.Time.Compare) {
		t.Errorf("Events of the other limit are not sorted: %v", got)
	}
}

func TestPreemptionLimitTrackerForget(t *testing.T) {
	const forgottenLimit, otherLimit = "per-cq", "global"
	now := time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)

	cases := map[string]struct {
		limits trackedLimits
		want   trackedLimits
	}{
		"only the given limit is forgotten": {
			limits: trackedLimits{
				forgottenLimit: trackedLimit("uid", perCQ, scopeEvents{"cq-a": {now}, "cq-b": {now}}),
				otherLimit:     trackedLimit("other-uid", global, scopeEvents{"Global": {now}}),
			},
			want: trackedLimits{
				otherLimit: trackedLimit("other-uid", global, scopeEvents{"Global": {now}}),
			},
		},
		"unknown limit is a no-op": {
			limits: trackedLimits{
				otherLimit: trackedLimit("other-uid", global, scopeEvents{"Global": {now}}),
			},
			want: trackedLimits{
				otherLimit: trackedLimit("other-uid", global, scopeEvents{"Global": {now}}),
			},
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			tracker := NewPreemptionLimitTracker()
			tracker.limits = tc.limits
			tracker.Forget(forgottenLimit)
			if diff := cmp.Diff(tc.want, tracker.limits, cmpTrackedLimits...); diff != "" {
				t.Errorf("Unexpected tracked limits (-want,+got):\n%s", diff)
			}
		})
	}
}
