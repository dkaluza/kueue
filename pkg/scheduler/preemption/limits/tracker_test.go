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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	kueuealpha "sigs.k8s.io/kueue/apis/kueue/v1alpha1"
	"sigs.k8s.io/kueue/pkg/scheduler/preemption/policy"
	utiltestingalpha "sigs.k8s.io/kueue/pkg/util/testing/v1alpha1"
)

type trackedLimits = map[policy.PreemptionLimitReference]limitState

var cmpTrackedLimits = []cmp.Option{
	cmp.AllowUnexported(limitState{}),
	cmpopts.EquateEmpty(),
}

// trackedLimit returns the state of limit as synced, with the given events.
func trackedLimit(limit *kueuealpha.PreemptionLimit, events ScopeEvents) limitState {
	ls := newLimitState(limit)
	if events != nil {
		ls.eventsByScopeValue = events
	}
	return ls
}

// tracked returns the given states by the name of their PreemptionLimit.
func tracked(states ...limitState) trackedLimits {
	limits := make(trackedLimits, len(states))
	for _, ls := range states {
		limits[policy.PreemptionLimitReference(ls.limit.Name)] = ls
	}
	return limits
}

// withResetGeneration sets the generation of the PreemptionLimit when the
// events of ls were last reset.
func withResetGeneration(ls limitState, generation int64) limitState {
	ls.resetGeneration = generation
	return ls
}

// selector returns a LabelSelector matching the label key=value.
func selector(key, value string) *metav1.LabelSelector {
	return &metav1.LabelSelector{MatchLabels: map[string]string{key: value}}
}

func TestPreemptionLimitTrackerSnapshot(t *testing.T) {
	now := time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)
	cutoff := now.Add(-60 * time.Second)
	perCQLimit := utiltestingalpha.MakePreemptionLimit("per-cq", kueuealpha.PreemptorCQLimitScope).
		Generation(1).LimitWindowSeconds(60).Obj()
	globalLimit := utiltestingalpha.MakePreemptionLimit("global", kueuealpha.GlobalPreemptionLimitScope).
		Generation(1).LimitWindowSeconds(3600).Obj()

	cases := map[string]struct {
		initialLimits trackedLimits
		wantLimits    []*kueuealpha.PreemptionLimit
		wantRecorded  preemptionCounts
	}{
		"empty tracker gives an empty snapshot": {},
		"events before the window are not counted": {
			initialLimits: tracked(trackedLimit(perCQLimit, ScopeEvents{"cq-a": {cutoff.Add(-time.Hour), cutoff.Add(-time.Nanosecond), cutoff.Add(time.Nanosecond), now}})),
			wantLimits:    []*kueuealpha.PreemptionLimit{perCQLimit},
			wantRecorded:  preemptionCounts{"per-cq": {"cq-a": 2}},
		},
		"event at the start of the window is counted": {
			initialLimits: tracked(trackedLimit(perCQLimit, ScopeEvents{"cq-a": {cutoff}})),
			wantLimits:    []*kueuealpha.PreemptionLimit{perCQLimit},
			wantRecorded:  preemptionCounts{"per-cq": {"cq-a": 1}},
		},
		"every scope value of a limit is counted": {
			initialLimits: tracked(trackedLimit(perCQLimit, ScopeEvents{"cq-a": {now}, "cq-b": {now, now}})),
			wantLimits:    []*kueuealpha.PreemptionLimit{perCQLimit},
			wantRecorded:  preemptionCounts{"per-cq": {"cq-a": 1, "cq-b": 2}},
		},
		"each limit counts within its own window": {
			initialLimits: tracked(
				trackedLimit(perCQLimit, ScopeEvents{"cq-a": {now.Add(-30 * time.Minute)}}),
				trackedLimit(globalLimit, ScopeEvents{policy.GlobalScopeValue: {now.Add(-2 * time.Hour), now.Add(-30 * time.Minute)}}),
			),
			wantLimits:   []*kueuealpha.PreemptionLimit{globalLimit, perCQLimit},
			wantRecorded: preemptionCounts{"global": {policy.GlobalScopeValue: 1}},
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			tracker := NewPreemptionLimitTracker()
			if tc.initialLimits != nil {
				tracker.limits = tc.initialLimits
			}
			got := tracker.Snapshot(now)
			wantLimitByName := make(map[policy.PreemptionLimitReference]*kueuealpha.PreemptionLimit, len(tc.wantLimits))
			for _, l := range tc.wantLimits {
				wantLimitByName[policy.PreemptionLimitReference(l.Name)] = trackedCopy(l)
			}
			want := &PreemptionLimitsSnapshot{limitByName: wantLimitByName, recorded: tc.wantRecorded}
			if diff := cmp.Diff(want, got, cmp.AllowUnexported(PreemptionLimitsSnapshot{}), cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("Unexpected snapshot (-want,+got):\n%s", diff)
			}
		})
	}
}

func TestPreemptionLimitTrackerCounts(t *testing.T) {
	now := time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)
	cutoff := now.Add(-60 * time.Second)
	perCQLimit := utiltestingalpha.MakePreemptionLimit("per-cq", kueuealpha.PreemptorCQLimitScope).
		Generation(1).LimitWindowSeconds(60).Obj()
	globalLimit := utiltestingalpha.MakePreemptionLimit("global", kueuealpha.GlobalPreemptionLimitScope).
		Generation(1).LimitWindowSeconds(60).Obj()

	cases := map[string]struct {
		initialLimits trackedLimits
		want          policy.PreemptionLimitScopeCounts
	}{
		"only the counts of the given limit within its window are returned": {
			initialLimits: tracked(
				trackedLimit(perCQLimit, ScopeEvents{
					"cq-a": {cutoff.Add(-time.Hour), cutoff.Add(-time.Nanosecond), cutoff, now},
					"cq-b": {now},
				}),
				trackedLimit(globalLimit, ScopeEvents{policy.GlobalScopeValue: {now}}),
			),
			want: policy.PreemptionLimitScopeCounts{"cq-a": 2, "cq-b": 1},
		},
		"tracked limit without events has no counts": {
			initialLimits: tracked(
				trackedLimit(perCQLimit, nil),
				trackedLimit(globalLimit, ScopeEvents{policy.GlobalScopeValue: {now}}),
			),
		},
		"untracked limit has no counts": {
			initialLimits: tracked(trackedLimit(globalLimit, ScopeEvents{policy.GlobalScopeValue: {now}})),
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			tracker := NewPreemptionLimitTracker()
			tracker.limits = tc.initialLimits
			got := tracker.Counts(now, policy.PreemptionLimitReference(perCQLimit.Name))
			if diff := cmp.Diff(tc.want, got, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("Unexpected counts (-want,+got):\n%s", diff)
			}
		})
	}
}

func TestPreemptionLimitTrackerRecord(t *testing.T) {
	t1 := time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)
	t2 := t1.Add(time.Second)
	t3 := t2.Add(time.Second)
	globalLimitBase := utiltestingalpha.MakePreemptionLimit("global", kueuealpha.GlobalPreemptionLimitScope).
		Generation(1).LimitWindowSeconds(60)
	globalLimit := globalLimitBase.Clone().Obj()
	perCQLimitBase := utiltestingalpha.MakePreemptionLimit("per-cq", kueuealpha.PreemptorCQLimitScope).
		Generation(1).LimitWindowSeconds(60)
	perCQLimit := perCQLimitBase.Clone().Obj()
	oldPerCQLimit := perCQLimitBase.Clone().UID("old-uid").Obj()

	cases := map[string]struct {
		initialLimits trackedLimits
		ts            time.Time
		consumed      policy.ConsumedLimits
		want          trackedLimits
	}{
		"nil consumed limits is a no-op": {
			initialLimits: tracked(trackedLimit(globalLimit, ScopeEvents{policy.GlobalScopeValue: {t1}})),
			ts:            t2,
			want:          tracked(trackedLimit(globalLimit, ScopeEvents{policy.GlobalScopeValue: {t1}})),
		},
		"every consumed limit is recorded": {
			initialLimits: tracked(trackedLimit(globalLimit, nil), trackedLimit(perCQLimit, nil)),
			ts:            t1,
			consumed: policy.ConsumedLimits{
				"global": policy.NewConsumedLimit(globalLimit, policy.GlobalScopeValue),
				"per-cq": policy.NewConsumedLimit(perCQLimit, "cq-a"),
			},
			want: tracked(
				trackedLimit(globalLimit, ScopeEvents{policy.GlobalScopeValue: {t1}}),
				trackedLimit(perCQLimit, ScopeEvents{"cq-a": {t1}}),
			),
		},
		"untracked limit is skipped": {
			initialLimits: tracked(trackedLimit(globalLimit, nil)),
			ts:            t1,
			consumed: policy.ConsumedLimits{
				"global": policy.NewConsumedLimit(globalLimit, policy.GlobalScopeValue),
				"per-cq": policy.NewConsumedLimit(perCQLimit, "cq-a"),
			},
			want: tracked(trackedLimit(globalLimit, ScopeEvents{policy.GlobalScopeValue: {t1}})),
		},
		"limit resolved before it was recreated is skipped": {
			initialLimits: tracked(trackedLimit(globalLimit, nil), trackedLimit(perCQLimit, ScopeEvents{"cq-a": {t1}})),
			ts:            t2,
			consumed: policy.ConsumedLimits{
				"global": policy.NewConsumedLimit(globalLimit, policy.GlobalScopeValue),
				"per-cq": policy.NewConsumedLimit(oldPerCQLimit, "cq-a"),
			},
			want: tracked(
				trackedLimit(globalLimit, ScopeEvents{policy.GlobalScopeValue: {t2}}),
				trackedLimit(perCQLimit, ScopeEvents{"cq-a": {t1}}),
			),
		},
		"limit resolved before its last reset is skipped": {
			initialLimits: tracked(trackedLimit(globalLimit, nil), trackedLimit(perCQLimitBase.Clone().Generation(2).Obj(), ScopeEvents{"cq-a": {t1}})),
			ts:            t2,
			consumed: policy.ConsumedLimits{
				"global": policy.NewConsumedLimit(globalLimit, policy.GlobalScopeValue),
				"per-cq": policy.NewConsumedLimit(perCQLimit, "cq-a"),
			},
			want: tracked(
				trackedLimit(globalLimit, ScopeEvents{policy.GlobalScopeValue: {t2}}),
				trackedLimit(perCQLimitBase.Clone().Generation(2).Obj(), ScopeEvents{"cq-a": {t1}}),
			),
		},
		"limit resolved at or after its last reset is recorded": {
			initialLimits: tracked(
				// Reset at generation 2, then updated at generation 3, e.g. by a window change.
				withResetGeneration(trackedLimit(globalLimitBase.Clone().Generation(3).Obj(), nil), 2),
				withResetGeneration(trackedLimit(perCQLimitBase.Clone().Generation(3).Obj(), nil), 2),
			),
			ts: t1,
			consumed: policy.ConsumedLimits{
				"global": policy.NewConsumedLimit(globalLimitBase.Clone().Generation(3).Obj(), policy.GlobalScopeValue),
				"per-cq": policy.NewConsumedLimit(perCQLimitBase.Clone().Generation(2).Obj(), "cq-a"),
			},
			want: tracked(
				withResetGeneration(trackedLimit(globalLimitBase.Clone().Generation(3).Obj(), ScopeEvents{policy.GlobalScopeValue: {t1}}), 2),
				withResetGeneration(trackedLimit(perCQLimitBase.Clone().Generation(3).Obj(), ScopeEvents{"cq-a": {t1}}), 2),
			),
		},
		"new scope value of a recorded limit": {
			initialLimits: tracked(trackedLimit(perCQLimit, ScopeEvents{"cq-a": {t1}})),
			ts:            t2,
			consumed:      policy.ConsumedLimits{"per-cq": policy.NewConsumedLimit(perCQLimit, "cq-b")},
			want:          tracked(trackedLimit(perCQLimit, ScopeEvents{"cq-a": {t1}, "cq-b": {t2}})),
		},
		"later event is appended": {
			initialLimits: tracked(trackedLimit(globalLimit, ScopeEvents{policy.GlobalScopeValue: {t1, t2}})),
			ts:            t3,
			consumed:      policy.ConsumedLimits{"global": policy.NewConsumedLimit(globalLimit, policy.GlobalScopeValue)},
			want:          tracked(trackedLimit(globalLimit, ScopeEvents{policy.GlobalScopeValue: {t1, t2, t3}})),
		},
		"earlier event is inserted in order": {
			initialLimits: tracked(trackedLimit(globalLimit, ScopeEvents{policy.GlobalScopeValue: {t1, t3}})),
			ts:            t2,
			consumed:      policy.ConsumedLimits{"global": policy.NewConsumedLimit(globalLimit, policy.GlobalScopeValue)},
			want:          tracked(trackedLimit(globalLimit, ScopeEvents{policy.GlobalScopeValue: {t1, t2, t3}})),
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			tracker := NewPreemptionLimitTracker()
			tracker.limits = tc.initialLimits
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
	globalLimit := utiltestingalpha.MakePreemptionLimit("global", kueuealpha.GlobalPreemptionLimitScope).
		Generation(1).LimitWindowSeconds(60).Obj()
	globalRef := policy.PreemptionLimitReference(globalLimit.Name)
	consumed := policy.ConsumedLimits{globalRef: policy.NewConsumedLimit(globalLimit, policy.GlobalScopeValue)}

	tracker := NewPreemptionLimitTracker()
	tracker.Sync(globalLimit)
	var wg sync.WaitGroup
	for g := range goroutines {
		wg.Go(func() {
			for i := range recordsPerGoroutine {
				tracker.Record(start.Add(time.Duration(g*recordsPerGoroutine+i)*time.Second), consumed)
			}
		})
	}
	wg.Wait()

	got := tracker.limits[globalRef].eventsByScopeValue[policy.GlobalScopeValue]
	if len(got) != goroutines*recordsPerGoroutine {
		t.Errorf("Recorded %d events, want %d", len(got), goroutines*recordsPerGoroutine)
	}
	if !slices.IsSortedFunc(got, time.Time.Compare) {
		t.Errorf("Events are not sorted: %v", got)
	}
}

func TestPreemptionLimitTrackerSync(t *testing.T) {
	const windowSeconds = 60
	now := time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)
	expired := now.Add(-time.Hour)
	limitBase := utiltestingalpha.MakePreemptionLimit("per-cq", kueuealpha.PreemptorCQLimitScope).
		Generation(1).LimitWindowSeconds(windowSeconds)
	limit := limitBase.Clone().Obj()
	otherLimit := utiltestingalpha.MakePreemptionLimit("global", kueuealpha.GlobalPreemptionLimitScope).
		Generation(1).LimitWindowSeconds(windowSeconds).Obj()
	recreatedLimit := limitBase.Clone().UID("old-uid").Obj()
	allSelectors := limitBase.Clone().
		ConfigSelector(selector("tier", "gold")).
		PreemptorClusterQueueSelector(selector("side", "preemptor")).
		PreemptedClusterQueueSelector(selector("side", "preempted")).
		Obj()
	limitAndWindowIncreased := limitBase.Clone().Generation(2).Limit(10).LimitWindowSeconds(2 * windowSeconds).Obj()
	windowHalved := limitBase.Clone().Generation(2).LimitWindowSeconds(windowSeconds / 2).Obj()
	rescoped := limitBase.Clone().Generation(2).Scope(kueuealpha.PreemptedCQLimitScope).Obj()

	cases := map[string]struct {
		initialLimits trackedLimits
		// synced, if set, is passed to Sync instead of limit.
		synced *kueuealpha.PreemptionLimit
		want   trackedLimits
	}{
		"untracked limit starts being tracked": {
			want: tracked(trackedLimit(limit, nil)),
		},
		"untracked limit starts being tracked with its selectors": {
			synced: allSelectors,
			want:   tracked(trackedLimit(allSelectors, nil)),
		},
		"other limits are untouched": {
			initialLimits: tracked(
				trackedLimit(limit, ScopeEvents{"cq-a": {now}}),
				trackedLimit(otherLimit, ScopeEvents{policy.GlobalScopeValue: {now}}),
			),
			synced: limitAndWindowIncreased,
			want: tracked(
				withResetGeneration(trackedLimit(limitAndWindowIncreased, ScopeEvents{"cq-a": {now}}), 1),
				trackedLimit(otherLimit, ScopeEvents{policy.GlobalScopeValue: {now}}),
			),
		},
		"limit and window increase update the limit, and keep the events and the reset generation": {
			initialLimits: tracked(trackedLimit(limit, ScopeEvents{"cq-a": {expired, now}})),
			synced:        limitAndWindowIncreased,
			// The expired event is kept because the limit and window increased, and the reset generation is updated to the new limit's generation.
			want: tracked(withResetGeneration(trackedLimit(limitAndWindowIncreased, ScopeEvents{"cq-a": {expired, now}}), 1)),
		},
		"window decrease updates the limit, and keeps the events and the reset generation": {
			initialLimits: tracked(trackedLimit(limit, ScopeEvents{"cq-a": {expired, now}})),
			synced:        windowHalved,
			want:          tracked(withResetGeneration(trackedLimit(windowHalved, ScopeEvents{"cq-a": {expired, now}}), 1)),
		},
		"unchanged selectors keep the events": {
			initialLimits: tracked(trackedLimit(allSelectors, ScopeEvents{"cq-a": {now}})),
			synced:        allSelectors,
			want:          tracked(trackedLimit(allSelectors, ScopeEvents{"cq-a": {now}})),
		},
		"limit recreated with the same name resets the events": {
			initialLimits: tracked(trackedLimit(recreatedLimit, ScopeEvents{"cq-a": {now}})),
			want:          tracked(trackedLimit(limit, nil)),
		},
		"scope change resets the events and updates the reset generation": {
			initialLimits: tracked(trackedLimit(limit, ScopeEvents{"cq-a": {now}})),
			synced:        rescoped,
			want:          tracked(trackedLimit(rescoped, nil)),
		},
		"config selector change resets the events": {
			initialLimits: tracked(trackedLimit(limit, ScopeEvents{"cq-a": {now}})),
			synced:        limitBase.Clone().Generation(2).ConfigSelector(selector("tier", "gold")).Obj(),
			want:          tracked(trackedLimit(limitBase.Clone().Generation(2).ConfigSelector(selector("tier", "gold")).Obj(), nil)),
		},
		"preemptor ClusterQueue selector change resets the events": {
			initialLimits: tracked(trackedLimit(limitBase.Clone().PreemptorClusterQueueSelector(selector("side", "preemptor")).Obj(), ScopeEvents{"cq-a": {now}})),
			synced:        limitBase.Clone().Generation(2).PreemptorClusterQueueSelector(selector("side", "other")).Obj(),
			want:          tracked(trackedLimit(limitBase.Clone().Generation(2).PreemptorClusterQueueSelector(selector("side", "other")).Obj(), nil)),
		},
		"preempted ClusterQueue selector change resets the events": {
			initialLimits: tracked(trackedLimit(limitBase.Clone().PreemptedClusterQueueSelector(selector("side", "preempted")).Obj(), ScopeEvents{"cq-a": {now}})),
			synced:        limitBase.Clone().Generation(2).Obj(),
			want:          tracked(trackedLimit(limitBase.Clone().Generation(2).Obj(), nil)),
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			tracker := NewPreemptionLimitTracker()
			if tc.initialLimits != nil {
				tracker.limits = tc.initialLimits
			}
			synced := limit
			if tc.synced != nil {
				synced = tc.synced
			}
			tracker.Sync(synced)
			if diff := cmp.Diff(tc.want, tracker.limits, cmpTrackedLimits...); diff != "" {
				t.Errorf("Unexpected tracked limits (-want,+got):\n%s", diff)
			}
		})
	}
}

func TestPreemptionLimitTrackerPrune(t *testing.T) {
	const windowSeconds = 60
	now := time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)
	window := windowSeconds * time.Second
	cutoff := now.Add(-window - pruneGracePeriod)
	expired := cutoff.Add(-time.Nanosecond)
	withinGracePeriod := now.Add(-window - pruneGracePeriod/2)
	limitBase := utiltestingalpha.MakePreemptionLimit("per-cq", kueuealpha.PreemptorCQLimitScope).
		Generation(1).LimitWindowSeconds(windowSeconds)
	limit := limitBase.Clone().Obj()
	otherLimit := utiltestingalpha.MakePreemptionLimit("global", kueuealpha.GlobalPreemptionLimitScope).
		Generation(1).LimitWindowSeconds(windowSeconds).Obj()
	doubledWindowLimit := limitBase.Clone().Generation(2).LimitWindowSeconds(2 * windowSeconds).Obj()
	halvedWindowLimit := limitBase.Clone().Generation(2).LimitWindowSeconds(windowSeconds / 2).Obj()
	expiredWithDoubledWindow := now.Add(-2*window - pruneGracePeriod - time.Nanosecond)
	withinDoubledWindow := cutoff.Add(-time.Second)
	expiredOnlyWithHalvedWindow := now.Add(-window/2 - pruneGracePeriod - time.Second)

	cases := map[string]struct {
		initialLimits trackedLimits
		want          trackedLimits
	}{
		"events before the cutoff are dropped, events at or after it are kept": {
			initialLimits: tracked(trackedLimit(limit, ScopeEvents{"cq-a": {cutoff.Add(-time.Hour), expired, cutoff, now}})),
			want:          tracked(trackedLimit(limit, ScopeEvents{"cq-a": {cutoff, now}})),
		},
		"events within the grace period are kept": {
			initialLimits: tracked(trackedLimit(limit, ScopeEvents{"cq-a": {withinGracePeriod}})),
			want:          tracked(trackedLimit(limit, ScopeEvents{"cq-a": {withinGracePeriod}})),
		},
		"scope value left without events is forgotten": {
			initialLimits: tracked(trackedLimit(limit, ScopeEvents{"cq-a": {expired}, "cq-b": {now}})),
			want:          tracked(trackedLimit(limit, ScopeEvents{"cq-b": {now}})),
		},
		"limit left without events is still tracked": {
			initialLimits: tracked(trackedLimit(limit, ScopeEvents{"cq-a": {expired}, "cq-b": {expired}})),
			want:          tracked(trackedLimit(limit, nil)),
		},
		"other limits are untouched": {
			initialLimits: tracked(
				trackedLimit(limit, ScopeEvents{"cq-a": {expired, now}}),
				trackedLimit(otherLimit, ScopeEvents{policy.GlobalScopeValue: {expired}}),
			),
			want: tracked(
				trackedLimit(limit, ScopeEvents{"cq-a": {now}}),
				trackedLimit(otherLimit, ScopeEvents{policy.GlobalScopeValue: {expired}}),
			),
		},
		"untracked limit is a no-op": {
			initialLimits: tracked(trackedLimit(otherLimit, ScopeEvents{policy.GlobalScopeValue: {expired}})),
			want:          tracked(trackedLimit(otherLimit, ScopeEvents{policy.GlobalScopeValue: {expired}})),
		},
		"doubled window keeps events within the wider cutoff": {
			initialLimits: tracked(trackedLimit(doubledWindowLimit, ScopeEvents{"cq-a": {expiredWithDoubledWindow, withinDoubledWindow, now}})),
			want:          tracked(trackedLimit(doubledWindowLimit, ScopeEvents{"cq-a": {withinDoubledWindow, now}})),
		},
		"halved window drops events older than the shorter cutoff": {
			initialLimits: tracked(trackedLimit(halvedWindowLimit, ScopeEvents{"cq-a": {expiredOnlyWithHalvedWindow, now}})),
			want:          tracked(trackedLimit(halvedWindowLimit, ScopeEvents{"cq-a": {now}})),
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			tracker := NewPreemptionLimitTracker()
			tracker.limits = tc.initialLimits
			tracker.Prune(now, policy.PreemptionLimitReference(limit.Name))
			if diff := cmp.Diff(tc.want, tracker.limits, cmpTrackedLimits...); diff != "" {
				t.Errorf("Unexpected tracked limits (-want,+got):\n%s", diff)
			}
		})
	}
}

func TestPreemptionLimitTrackerSyncAndPruneConcurrent(t *testing.T) {
	const goroutines, recordsPerGoroutine = 10, 100
	const records = goroutines * recordsPerGoroutine
	const windowSeconds = 60
	start := time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)
	window := windowSeconds * time.Second
	limit := utiltestingalpha.MakePreemptionLimit("per-cq", kueuealpha.PreemptorCQLimitScope).
		Generation(1).LimitWindowSeconds(windowSeconds).Obj()
	otherLimit := utiltestingalpha.MakePreemptionLimit("global", kueuealpha.GlobalPreemptionLimitScope).
		Generation(1).LimitWindowSeconds(windowSeconds).Obj()
	limitRef := policy.PreemptionLimitReference(limit.Name)
	otherLimitRef := policy.PreemptionLimitReference(otherLimit.Name)
	consumed := policy.ConsumedLimits{
		limitRef:      policy.NewConsumedLimit(limit, "cq-a"),
		otherLimitRef: policy.NewConsumedLimit(otherLimit, policy.GlobalScopeValue),
	}
	// Every recorded event is already expired, so Prune keeps dropping events from
	// limit while Sync updates its state and Record keeps adding to it.
	now := start.Add(records*time.Second + window + pruneGracePeriod)

	tracker := NewPreemptionLimitTracker()
	tracker.Sync(otherLimit)
	var wg sync.WaitGroup
	for g := range goroutines {
		wg.Go(func() {
			for i := range recordsPerGoroutine {
				tracker.Sync(limit)
				tracker.Prune(now, limitRef)
				tracker.Record(start.Add(time.Duration(g*recordsPerGoroutine+i)*time.Second), consumed)
			}
		})
	}
	wg.Wait()

	got := tracker.limits[otherLimitRef].eventsByScopeValue[policy.GlobalScopeValue]
	if len(got) != records {
		t.Errorf("Recorded %d events of the other limit, want %d", len(got), records)
	}
	if !slices.IsSortedFunc(got, time.Time.Compare) {
		t.Errorf("Events of the other limit are not sorted: %v", got)
	}
}

func TestPreemptionLimitTrackerForget(t *testing.T) {
	now := time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)
	forgottenLimit := utiltestingalpha.MakePreemptionLimit("per-cq", kueuealpha.PreemptorCQLimitScope).
		Generation(1).LimitWindowSeconds(60).Obj()
	otherLimit := utiltestingalpha.MakePreemptionLimit("global", kueuealpha.GlobalPreemptionLimitScope).
		Generation(1).LimitWindowSeconds(60).Obj()

	cases := map[string]struct {
		initialLimits trackedLimits
		want          trackedLimits
	}{
		"only the given limit is forgotten": {
			initialLimits: tracked(
				trackedLimit(forgottenLimit, ScopeEvents{"cq-a": {now}, "cq-b": {now}}),
				trackedLimit(otherLimit, ScopeEvents{policy.GlobalScopeValue: {now}}),
			),
			want: tracked(trackedLimit(otherLimit, ScopeEvents{policy.GlobalScopeValue: {now}})),
		},
		"unknown limit is a no-op": {
			initialLimits: tracked(trackedLimit(otherLimit, ScopeEvents{policy.GlobalScopeValue: {now}})),
			want:          tracked(trackedLimit(otherLimit, ScopeEvents{policy.GlobalScopeValue: {now}})),
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			tracker := NewPreemptionLimitTracker()
			tracker.limits = tc.initialLimits
			tracker.Forget(policy.PreemptionLimitReference(forgottenLimit.Name))
			if diff := cmp.Diff(tc.want, tracker.limits, cmpTrackedLimits...); diff != "" {
				t.Errorf("Unexpected tracked limits (-want,+got):\n%s", diff)
			}
		})
	}
}
