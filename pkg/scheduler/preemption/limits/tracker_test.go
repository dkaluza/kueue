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

// Scopes of the PreemptionLimits used across the tracker tests.
const (
	global = kueuealpha.GlobalPreemptionLimitScope
	perCQ  = kueuealpha.PreemptorCQLimitScope
)

type scopeEvents = map[policy.PreemptionLimitScopeValue]events

type trackedLimits = map[policy.PreemptionLimitReference]*limitState

var cmpTrackedLimits = []cmp.Option{
	cmp.AllowUnexported(limitState{}),
	cmpopts.EquateEmpty(),
}

// trackedLimit returns the state of limit as synced, with the given events.
func trackedLimit(limit *kueuealpha.PreemptionLimit, events scopeEvents) *limitState {
	ls := newLimitState(limit)
	if events != nil {
		ls.eventsByScopeValue = events
	}
	return ls
}

// tracked returns the given states by the name of their PreemptionLimit.
func tracked(states ...*limitState) trackedLimits {
	limits := make(trackedLimits, len(states))
	for _, ls := range states {
		limits[policy.PreemptionLimitReference(ls.limit.Name)] = ls
	}
	return limits
}

// snapshotOf returns the snapshot at now of a tracker holding limits.
func snapshotOf(now time.Time, limits trackedLimits) *PreemptionLimitsSnapshot {
	tracker := NewPreemptionLimitTracker()
	if limits != nil {
		tracker.limits = limits
	}
	return tracker.Snapshot(now)
}

// withResetGeneration sets the generation of the PreemptionLimit when the
// events of ls were last reset.
func withResetGeneration(ls *limitState, generation int64) *limitState {
	ls.resetGeneration = generation
	return ls
}

// atGeneration returns a copy of limit at the given generation.
func atGeneration(limit *kueuealpha.PreemptionLimit, generation int64) *kueuealpha.PreemptionLimit {
	limit = limit.DeepCopy()
	limit.Generation = generation
	return limit
}

// selector returns a LabelSelector matching the label key=value.
func selector(key, value string) *metav1.LabelSelector {
	return &metav1.LabelSelector{MatchLabels: map[string]string{key: value}}
}

func TestPreemptionLimitTrackerSnapshot(t *testing.T) {
	now := time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)
	cutoff := now.Add(-60 * time.Second)
	perCQLimit := utiltestingalpha.MakePreemptionLimit("per-cq", perCQ).LimitWindowSeconds(60).Obj()
	globalLimit := utiltestingalpha.MakePreemptionLimit("global", global).LimitWindowSeconds(3600).Obj()

	cases := map[string]struct {
		limits       trackedLimits
		wantLimits   []*kueuealpha.PreemptionLimit
		wantRecorded preemptionCounts
	}{
		"empty tracker gives an empty snapshot": {},
		"events before the window are not counted": {
			limits:       tracked(trackedLimit(perCQLimit, scopeEvents{"cq-a": {cutoff.Add(-time.Hour), cutoff.Add(-time.Nanosecond), cutoff.Add(time.Nanosecond), now}})),
			wantLimits:   []*kueuealpha.PreemptionLimit{perCQLimit},
			wantRecorded: preemptionCounts{"per-cq": {"cq-a": 2}},
		},
		"event at the start of the window is counted": {
			limits:       tracked(trackedLimit(perCQLimit, scopeEvents{"cq-a": {cutoff}})),
			wantLimits:   []*kueuealpha.PreemptionLimit{perCQLimit},
			wantRecorded: preemptionCounts{"per-cq": {"cq-a": 1}},
		},
		"every scope value of a limit is counted": {
			limits:       tracked(trackedLimit(perCQLimit, scopeEvents{"cq-a": {now}, "cq-b": {now, now}})),
			wantLimits:   []*kueuealpha.PreemptionLimit{perCQLimit},
			wantRecorded: preemptionCounts{"per-cq": {"cq-a": 1, "cq-b": 2}},
		},
		"each limit counts within its own window, and limits are sorted by name": {
			limits: tracked(
				trackedLimit(perCQLimit, scopeEvents{"cq-a": {now.Add(-30 * time.Minute)}}),
				trackedLimit(globalLimit, scopeEvents{"Global": {now.Add(-2 * time.Hour), now.Add(-30 * time.Minute)}}),
			),
			wantLimits:   []*kueuealpha.PreemptionLimit{globalLimit, perCQLimit},
			wantRecorded: preemptionCounts{"global": {"Global": 1}},
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			tracker := NewPreemptionLimitTracker()
			if tc.limits != nil {
				tracker.limits = tc.limits
			}
			got := tracker.Snapshot(now)
			wantLimits := make([]*kueuealpha.PreemptionLimit, 0, len(tc.wantLimits))
			for _, l := range tc.wantLimits {
				wantLimits = append(wantLimits, trackedCopy(l))
			}
			want := &PreemptionLimitsSnapshot{limits: wantLimits, recorded: tc.wantRecorded}
			if diff := cmp.Diff(want, got, cmp.AllowUnexported(PreemptionLimitsSnapshot{}), cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("Unexpected snapshot (-want,+got):\n%s", diff)
			}
		})
	}
}

func TestPreemptionLimitTrackerCounts(t *testing.T) {
	now := time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)
	perCQLimit := utiltestingalpha.MakePreemptionLimit("per-cq", perCQ).LimitWindowSeconds(60).Obj()
	globalLimit := utiltestingalpha.MakePreemptionLimit("global", global).LimitWindowSeconds(60).Obj()

	cases := map[string]struct {
		limits trackedLimits
		want   map[policy.PreemptionLimitScopeValue]int32
	}{
		"only the counts of the given limit within its window are returned": {
			limits: tracked(
				trackedLimit(perCQLimit, scopeEvents{"cq-a": {now.Add(-time.Hour), now}, "cq-b": {now}}),
				trackedLimit(globalLimit, scopeEvents{"Global": {now}}),
			),
			want: map[policy.PreemptionLimitScopeValue]int32{"cq-a": 1, "cq-b": 1},
		},
		"untracked limit has no counts": {
			limits: tracked(trackedLimit(globalLimit, scopeEvents{"Global": {now}})),
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			tracker := NewPreemptionLimitTracker()
			tracker.limits = tc.limits
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
	globalLimit := utiltestingalpha.MakePreemptionLimit("global", global).Obj()
	perCQLimit := utiltestingalpha.MakePreemptionLimit("per-cq", perCQ).Obj()
	oldPerCQLimit := perCQLimit.DeepCopy()
	oldPerCQLimit.UID = "old-uid"

	cases := map[string]struct {
		limits   trackedLimits
		ts       time.Time
		consumed policy.ConsumedLimits
		want     trackedLimits
	}{
		"nil consumed limits is a no-op": {
			limits: tracked(trackedLimit(globalLimit, scopeEvents{"Global": {t1}})),
			ts:     t2,
			want:   tracked(trackedLimit(globalLimit, scopeEvents{"Global": {t1}})),
		},
		"every consumed limit is recorded": {
			limits: tracked(trackedLimit(globalLimit, nil), trackedLimit(perCQLimit, nil)),
			ts:     t1,
			consumed: policy.ConsumedLimits{
				"global": policy.NewConsumedLimit(globalLimit, "Global"),
				"per-cq": policy.NewConsumedLimit(perCQLimit, "cq-a"),
			},
			want: tracked(
				trackedLimit(globalLimit, scopeEvents{"Global": {t1}}),
				trackedLimit(perCQLimit, scopeEvents{"cq-a": {t1}}),
			),
		},
		"untracked limit is skipped": {
			limits: tracked(trackedLimit(globalLimit, nil)),
			ts:     t1,
			consumed: policy.ConsumedLimits{
				"global": policy.NewConsumedLimit(globalLimit, "Global"),
				"per-cq": policy.NewConsumedLimit(perCQLimit, "cq-a"),
			},
			want: tracked(trackedLimit(globalLimit, scopeEvents{"Global": {t1}})),
		},
		"limit resolved before it was recreated is skipped": {
			limits: tracked(trackedLimit(globalLimit, nil), trackedLimit(perCQLimit, scopeEvents{"cq-a": {t1}})),
			ts:     t2,
			consumed: policy.ConsumedLimits{
				"global": policy.NewConsumedLimit(globalLimit, "Global"),
				"per-cq": policy.NewConsumedLimit(oldPerCQLimit, "cq-a"),
			},
			want: tracked(
				trackedLimit(globalLimit, scopeEvents{"Global": {t2}}),
				trackedLimit(perCQLimit, scopeEvents{"cq-a": {t1}}),
			),
		},
		"limit resolved before its last reset is skipped": {
			limits: tracked(trackedLimit(globalLimit, nil), trackedLimit(atGeneration(perCQLimit, 2), scopeEvents{"cq-a": {t1}})),
			ts:     t2,
			consumed: policy.ConsumedLimits{
				"global": policy.NewConsumedLimit(globalLimit, "Global"),
				"per-cq": policy.NewConsumedLimit(atGeneration(perCQLimit, 1), "cq-a"),
			},
			want: tracked(
				trackedLimit(globalLimit, scopeEvents{"Global": {t2}}),
				trackedLimit(atGeneration(perCQLimit, 2), scopeEvents{"cq-a": {t1}}),
			),
		},
		"limit resolved at or after its last reset is recorded": {
			limits: tracked(
				trackedLimit(atGeneration(globalLimit, 2), nil),
				// Reset at generation 2, then updated at generation 3, e.g. by a window change.
				withResetGeneration(trackedLimit(atGeneration(perCQLimit, 3), nil), 2),
			),
			ts: t1,
			consumed: policy.ConsumedLimits{
				"global": policy.NewConsumedLimit(atGeneration(globalLimit, 2), "Global"),
				"per-cq": policy.NewConsumedLimit(atGeneration(perCQLimit, 2), "cq-a"),
			},
			want: tracked(
				trackedLimit(atGeneration(globalLimit, 2), scopeEvents{"Global": {t1}}),
				withResetGeneration(trackedLimit(atGeneration(perCQLimit, 3), scopeEvents{"cq-a": {t1}}), 2),
			),
		},
		"new scope value of a recorded limit": {
			limits:   tracked(trackedLimit(perCQLimit, scopeEvents{"cq-a": {t1}})),
			ts:       t2,
			consumed: policy.ConsumedLimits{"per-cq": policy.NewConsumedLimit(perCQLimit, "cq-b")},
			want:     tracked(trackedLimit(perCQLimit, scopeEvents{"cq-a": {t1}, "cq-b": {t2}})),
		},
		"later event is appended": {
			limits:   tracked(trackedLimit(globalLimit, scopeEvents{"Global": {t1, t2}})),
			ts:       t3,
			consumed: policy.ConsumedLimits{"global": policy.NewConsumedLimit(globalLimit, "Global")},
			want:     tracked(trackedLimit(globalLimit, scopeEvents{"Global": {t1, t2, t3}})),
		},
		"earlier event is inserted in order": {
			limits:   tracked(trackedLimit(globalLimit, scopeEvents{"Global": {t1, t3}})),
			ts:       t2,
			consumed: policy.ConsumedLimits{"global": policy.NewConsumedLimit(globalLimit, "Global")},
			want:     tracked(trackedLimit(globalLimit, scopeEvents{"Global": {t1, t2, t3}})),
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
	globalLimit := utiltestingalpha.MakePreemptionLimit("global", global).Obj()
	consumed := policy.ConsumedLimits{"global": policy.NewConsumedLimit(globalLimit, "Global")}

	tracker := NewPreemptionLimitTracker()
	tracker.Sync(start, globalLimit)
	var wg sync.WaitGroup
	for g := range goroutines {
		wg.Go(func() {
			for i := range recordsPerGoroutine {
				tracker.Record(start.Add(time.Duration(g*recordsPerGoroutine+i)*time.Second), consumed)
			}
		})
	}
	wg.Wait()

	got := tracker.limits["global"].eventsByScopeValue["Global"]
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
	window := windowSeconds * time.Second
	cutoff := now.Add(-window - pruneGracePeriod)
	expired := cutoff.Add(-time.Nanosecond)
	withinGracePeriod := now.Add(-window - pruneGracePeriod/2)
	// prunedOnlyWithHalfWindow is kept with a window of windowSeconds, but pruned
	// once the window is halved, as it is 1s older than the halved window's cutoff.
	prunedOnlyWithHalfWindow := now.Add(-window/2 - pruneGracePeriod - time.Second)
	limit := utiltestingalpha.MakePreemptionLimit("per-cq", perCQ).LimitWindowSeconds(windowSeconds).Obj()
	otherLimit := utiltestingalpha.MakePreemptionLimit("global", global).LimitWindowSeconds(windowSeconds).Obj()
	recreatedLimit := limit.DeepCopy()
	recreatedLimit.UID = "old-uid"
	withSelectors := func(l *kueuealpha.PreemptionLimit, config, preemptorClusterQueue, preemptedClusterQueue *metav1.LabelSelector) *kueuealpha.PreemptionLimit {
		l = l.DeepCopy()
		l.Spec.ConfigSelector = config
		l.Spec.PreemptorClusterQueueSelector = preemptorClusterQueue
		l.Spec.PreemptedClusterQueueSelector = preemptedClusterQueue
		return l
	}
	allSelectors := withSelectors(limit, selector("tier", "gold"), selector("side", "preemptor"), selector("side", "preempted"))
	limitAndWindowIncreased := utiltestingalpha.MakePreemptionLimit("per-cq", perCQ).Generation(2).Limit(10).LimitWindowSeconds(2 * windowSeconds).Obj()
	windowHalved := utiltestingalpha.MakePreemptionLimit("per-cq", perCQ).LimitWindowSeconds(windowSeconds / 2).Obj()
	rescoped := utiltestingalpha.MakePreemptionLimit("per-cq", kueuealpha.PreemptedCQLimitScope).Generation(1).LimitWindowSeconds(windowSeconds).Obj()

	cases := map[string]struct {
		// limits is the state of the tracker before Sync.
		limits trackedLimits
		// synced, if set, is passed to Sync instead of limit.
		synced *kueuealpha.PreemptionLimit
		// want is the state of the tracker after Sync.
		want trackedLimits
	}{
		"events before the cutoff are dropped, events at or after it are kept": {
			limits: tracked(trackedLimit(limit, scopeEvents{"cq-a": {cutoff.Add(-time.Hour), expired, cutoff, now}})),
			want:   tracked(trackedLimit(limit, scopeEvents{"cq-a": {cutoff, now}})),
		},
		"events within the grace period are kept": {
			limits: tracked(trackedLimit(limit, scopeEvents{"cq-a": {withinGracePeriod}})),
			want:   tracked(trackedLimit(limit, scopeEvents{"cq-a": {withinGracePeriod}})),
		},
		"scope value left without events is forgotten": {
			limits: tracked(trackedLimit(limit, scopeEvents{"cq-a": {expired}, "cq-b": {now}})),
			want:   tracked(trackedLimit(limit, scopeEvents{"cq-b": {now}})),
		},
		"limit left without events is still tracked": {
			limits: tracked(trackedLimit(limit, scopeEvents{"cq-a": {expired}, "cq-b": {expired}})),
			want:   tracked(trackedLimit(limit, nil)),
		},
		"untracked limit starts being tracked": {
			want: tracked(trackedLimit(limit, nil)),
		},
		"untracked limit starts being tracked with its selectors": {
			synced: allSelectors,
			want:   tracked(trackedLimit(allSelectors, nil)),
		},
		"other limits are untouched": {
			limits: tracked(
				trackedLimit(limit, scopeEvents{"cq-a": {expired, now}}),
				trackedLimit(otherLimit, scopeEvents{"Global": {expired}}),
			),
			want: tracked(
				trackedLimit(limit, scopeEvents{"cq-a": {now}}),
				trackedLimit(otherLimit, scopeEvents{"Global": {expired}}),
			),
		},
		"limit and window increase update the limit, and keep the events and the reset generation": {
			limits: tracked(trackedLimit(atGeneration(limit, 1), scopeEvents{"cq-a": {expired, now}})),
			synced: limitAndWindowIncreased,
			want:   tracked(withResetGeneration(trackedLimit(limitAndWindowIncreased, scopeEvents{"cq-a": {expired, now}}), 1)),
		},
		"window decrease keeps the events within the new window": {
			limits: tracked(trackedLimit(limit, scopeEvents{"cq-a": {prunedOnlyWithHalfWindow, now}})),
			synced: windowHalved,
			want:   tracked(trackedLimit(windowHalved, scopeEvents{"cq-a": {now}})),
		},
		"unchanged selectors keep the events": {
			limits: tracked(trackedLimit(allSelectors, scopeEvents{"cq-a": {now}})),
			synced: allSelectors,
			want:   tracked(trackedLimit(allSelectors, scopeEvents{"cq-a": {now}})),
		},
		"limit recreated with the same name resets the events": {
			limits: tracked(trackedLimit(recreatedLimit, scopeEvents{"cq-a": {now}})),
			want:   tracked(trackedLimit(limit, nil)),
		},
		"scope change resets the events and the reset generation": {
			limits: tracked(trackedLimit(limit, scopeEvents{"cq-a": {now}})),
			synced: rescoped,
			want:   tracked(trackedLimit(rescoped, nil)),
		},
		"config selector change resets the events": {
			limits: tracked(trackedLimit(limit, scopeEvents{"cq-a": {now}})),
			synced: withSelectors(limit, selector("tier", "gold"), nil, nil),
			want:   tracked(trackedLimit(withSelectors(limit, selector("tier", "gold"), nil, nil), nil)),
		},
		"preemptor ClusterQueue selector change resets the events": {
			limits: tracked(trackedLimit(withSelectors(limit, nil, selector("side", "preemptor"), nil), scopeEvents{"cq-a": {now}})),
			synced: withSelectors(limit, nil, selector("side", "other"), nil),
			want:   tracked(trackedLimit(withSelectors(limit, nil, selector("side", "other"), nil), nil)),
		},
		"preempted ClusterQueue selector removal resets the events": {
			limits: tracked(trackedLimit(withSelectors(limit, nil, nil, selector("side", "preempted")), scopeEvents{"cq-a": {now}})),
			want:   tracked(trackedLimit(limit, nil)),
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			tracker := NewPreemptionLimitTracker()
			if tc.limits != nil {
				tracker.limits = tc.limits
			}
			synced := limit
			if tc.synced != nil {
				synced = tc.synced
			}
			tracker.Sync(now, synced)
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
	start := time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)
	window := windowSeconds * time.Second
	limit := utiltestingalpha.MakePreemptionLimit("per-cq", perCQ).LimitWindowSeconds(windowSeconds).Obj()
	otherLimit := utiltestingalpha.MakePreemptionLimit("global", global).Obj()
	consumed := policy.ConsumedLimits{
		"per-cq": policy.NewConsumedLimit(limit, "cq-a"),
		"global": policy.NewConsumedLimit(otherLimit, "Global"),
	}
	// Every recorded event is already expired, so the syncs keep pruning the
	// synced limit while the records keep adding to it.
	now := start.Add(records*time.Second + window + pruneGracePeriod)

	tracker := NewPreemptionLimitTracker()
	tracker.Sync(now, otherLimit)
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

	got := tracker.limits["global"].eventsByScopeValue["Global"]
	if len(got) != records {
		t.Errorf("Recorded %d events of the other limit, want %d", len(got), records)
	}
	if !slices.IsSortedFunc(got, time.Time.Compare) {
		t.Errorf("Events of the other limit are not sorted: %v", got)
	}
}

func TestPreemptionLimitTrackerForget(t *testing.T) {
	now := time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)
	forgottenLimit := utiltestingalpha.MakePreemptionLimit("per-cq", perCQ).Obj()
	otherLimit := utiltestingalpha.MakePreemptionLimit("global", global).Obj()

	cases := map[string]struct {
		limits trackedLimits
		want   trackedLimits
	}{
		"only the given limit is forgotten": {
			limits: tracked(
				trackedLimit(forgottenLimit, scopeEvents{"cq-a": {now}, "cq-b": {now}}),
				trackedLimit(otherLimit, scopeEvents{"Global": {now}}),
			),
			want: tracked(trackedLimit(otherLimit, scopeEvents{"Global": {now}})),
		},
		"unknown limit is a no-op": {
			limits: tracked(trackedLimit(otherLimit, scopeEvents{"Global": {now}})),
			want:   tracked(trackedLimit(otherLimit, scopeEvents{"Global": {now}})),
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			tracker := NewPreemptionLimitTracker()
			tracker.limits = tc.limits
			tracker.Forget(policy.PreemptionLimitReference(forgottenLimit.Name))
			if diff := cmp.Diff(tc.want, tracker.limits, cmpTrackedLimits...); diff != "" {
				t.Errorf("Unexpected tracked limits (-want,+got):\n%s", diff)
			}
		})
	}
}
