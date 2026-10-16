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

	kueuealpha "sigs.k8s.io/kueue/apis/kueue/v1alpha1"
	"sigs.k8s.io/kueue/pkg/scheduler/preemption/policy"
	utiltestingalpha "sigs.k8s.io/kueue/pkg/util/testing/v1alpha1"
)

func TestPreemptionLimitTrackerCount(t *testing.T) {
	const windowSeconds = 60
	now := time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)
	cutoff := now.Add(-windowSeconds * time.Second)
	limit := utiltestingalpha.MakePreemptionLimit("per-cq", kueuealpha.PreemptingCQLimitScope).LimitWindowSeconds(windowSeconds).Obj()

	cases := map[string]struct {
		events     map[policy.PreemptionLimitReference]map[policy.PreemptionLimitScopeValue]events
		limit      *kueuealpha.PreemptionLimit
		scopeValue policy.PreemptionLimitScopeValue
		want       int32
	}{
		"events before the window are not counted": {
			events: map[policy.PreemptionLimitReference]map[policy.PreemptionLimitScopeValue]events{
				"per-cq": {"cq-a": {cutoff.Add(-time.Hour), cutoff.Add(-time.Nanosecond), cutoff.Add(time.Nanosecond), now}},
			},
			limit:      limit,
			scopeValue: "cq-a",
			want:       2,
		},
		"event at the start of the window is counted": {
			events: map[policy.PreemptionLimitReference]map[policy.PreemptionLimitScopeValue]events{
				"per-cq": {"cq-a": {cutoff}},
			},
			limit:      limit,
			scopeValue: "cq-a",
			want:       1,
		},
		"window comes from the limit spec": {
			events: map[policy.PreemptionLimitReference]map[policy.PreemptionLimitScopeValue]events{
				"per-cq": {"cq-a": {now.Add(-2 * time.Hour), now.Add(-30 * time.Minute)}},
			},
			limit:      utiltestingalpha.MakePreemptionLimit("per-cq", kueuealpha.PreemptingCQLimitScope).LimitWindowSeconds(3600).Obj(),
			scopeValue: "cq-a",
			want:       1,
		},
		"unknown limit counts nothing": {
			events: map[policy.PreemptionLimitReference]map[policy.PreemptionLimitScopeValue]events{
				"other": {"cq-a": {now}},
			},
			limit:      limit,
			scopeValue: "cq-a",
			want:       0,
		},
		"unknown scope value counts nothing": {
			events: map[policy.PreemptionLimitReference]map[policy.PreemptionLimitScopeValue]events{
				"per-cq": {"cq-b": {now}},
			},
			limit:      limit,
			scopeValue: "cq-a",
			want:       0,
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			tracker := NewPreemptionLimitTracker()
			tracker.events = tc.events
			if got := tracker.Count(now, tc.limit, tc.scopeValue); got != tc.want {
				t.Errorf("Count() = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestPreemptionLimitTrackerRecord(t *testing.T) {
	t1 := time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)
	t2 := t1.Add(time.Second)
	t3 := t2.Add(time.Second)

	cases := map[string]struct {
		events   map[policy.PreemptionLimitReference]map[policy.PreemptionLimitScopeValue]events
		ts       time.Time
		consumed policy.ConsumedLimits
		want     map[policy.PreemptionLimitReference]map[policy.PreemptionLimitScopeValue]events
	}{
		"nil consumed limits is a no-op": {
			events: map[policy.PreemptionLimitReference]map[policy.PreemptionLimitScopeValue]events{
				"global": {"Global": {t1}},
			},
			ts: t2,
			want: map[policy.PreemptionLimitReference]map[policy.PreemptionLimitScopeValue]events{
				"global": {"Global": {t1}},
			},
		},
		"every consumed limit is recorded": {
			ts:       t1,
			consumed: policy.ConsumedLimits{"global": "Global", "per-cq": "cq-a"},
			want: map[policy.PreemptionLimitReference]map[policy.PreemptionLimitScopeValue]events{
				"global": {"Global": {t1}},
				"per-cq": {"cq-a": {t1}},
			},
		},
		"new scope value of a recorded limit": {
			events: map[policy.PreemptionLimitReference]map[policy.PreemptionLimitScopeValue]events{
				"per-cq": {"cq-a": {t1}},
			},
			ts:       t2,
			consumed: policy.ConsumedLimits{"per-cq": "cq-b"},
			want: map[policy.PreemptionLimitReference]map[policy.PreemptionLimitScopeValue]events{
				"per-cq": {"cq-a": {t1}, "cq-b": {t2}},
			},
		},
		"later event is appended": {
			events: map[policy.PreemptionLimitReference]map[policy.PreemptionLimitScopeValue]events{
				"global": {"Global": {t1, t2}},
			},
			ts:       t3,
			consumed: policy.ConsumedLimits{"global": "Global"},
			want: map[policy.PreemptionLimitReference]map[policy.PreemptionLimitScopeValue]events{
				"global": {"Global": {t1, t2, t3}},
			},
		},
		"earlier event is inserted in order": {
			events: map[policy.PreemptionLimitReference]map[policy.PreemptionLimitScopeValue]events{
				"global": {"Global": {t1, t3}},
			},
			ts:       t2,
			consumed: policy.ConsumedLimits{"global": "Global"},
			want: map[policy.PreemptionLimitReference]map[policy.PreemptionLimitScopeValue]events{
				"global": {"Global": {t1, t2, t3}},
			},
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			tracker := NewPreemptionLimitTracker()
			if tc.events != nil {
				tracker.events = tc.events
			}
			tracker.Record(tc.ts, tc.consumed)
			if diff := cmp.Diff(tc.want, tracker.events, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("Unexpected events (-want,+got):\n%s", diff)
			}
		})
	}
}

func TestPreemptionLimitTrackerRecordConcurrent(t *testing.T) {
	const goroutines, recordsPerGoroutine = 10, 100
	start := time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)
	consumed := policy.ConsumedLimits{"global": "Global"}

	tracker := NewPreemptionLimitTracker()
	var wg sync.WaitGroup
	for g := range goroutines {
		wg.Go(func() {
			for i := range recordsPerGoroutine {
				tracker.Record(start.Add(time.Duration(g*recordsPerGoroutine+i)*time.Second), consumed)
			}
		})
	}
	wg.Wait()

	got := tracker.events["global"]["Global"]
	if len(got) != goroutines*recordsPerGoroutine {
		t.Errorf("Recorded %d events, want %d", len(got), goroutines*recordsPerGoroutine)
	}
	if !slices.IsSortedFunc(got, time.Time.Compare) {
		t.Errorf("Events are not sorted: %v", got)
	}
}

func TestPreemptionLimitTrackerPrune(t *testing.T) {
	const windowSeconds = 60
	const prunedLimit, otherLimit = "per-cq", "global"
	now := time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)
	window := windowSeconds * time.Second
	cutoff := now.Add(-window - pruneGracePeriod)
	expired := cutoff.Add(-time.Nanosecond)
	withinGracePeriod := now.Add(-window - pruneGracePeriod/2)
	limit := utiltestingalpha.MakePreemptionLimit(prunedLimit, kueuealpha.PreemptingCQLimitScope).LimitWindowSeconds(windowSeconds).Obj()

	cases := map[string]struct {
		events map[policy.PreemptionLimitReference]map[policy.PreemptionLimitScopeValue]events
		want   map[policy.PreemptionLimitReference]map[policy.PreemptionLimitScopeValue]events
	}{
		"events before the cutoff are dropped, events at or after it are kept": {
			events: map[policy.PreemptionLimitReference]map[policy.PreemptionLimitScopeValue]events{
				prunedLimit: {"cq-a": {cutoff.Add(-time.Hour), expired, cutoff, now}},
			},
			want: map[policy.PreemptionLimitReference]map[policy.PreemptionLimitScopeValue]events{
				prunedLimit: {"cq-a": {cutoff, now}},
			},
		},
		"events within the grace period are kept": {
			events: map[policy.PreemptionLimitReference]map[policy.PreemptionLimitScopeValue]events{
				prunedLimit: {"cq-a": {withinGracePeriod}},
			},
			want: map[policy.PreemptionLimitReference]map[policy.PreemptionLimitScopeValue]events{
				prunedLimit: {"cq-a": {withinGracePeriod}},
			},
		},
		"scope value left without events is forgotten": {
			events: map[policy.PreemptionLimitReference]map[policy.PreemptionLimitScopeValue]events{
				prunedLimit: {"cq-a": {expired}, "cq-b": {now}},
			},
			want: map[policy.PreemptionLimitReference]map[policy.PreemptionLimitScopeValue]events{
				prunedLimit: {"cq-b": {now}},
			},
		},
		"limit left without events is forgotten": {
			events: map[policy.PreemptionLimitReference]map[policy.PreemptionLimitScopeValue]events{
				prunedLimit: {"cq-a": {expired}, "cq-b": {expired}},
			},
			want: map[policy.PreemptionLimitReference]map[policy.PreemptionLimitScopeValue]events{},
		},
		"other limits are untouched": {
			events: map[policy.PreemptionLimitReference]map[policy.PreemptionLimitScopeValue]events{
				prunedLimit: {"cq-a": {expired, now}},
				otherLimit:  {"Global": {expired}},
			},
			want: map[policy.PreemptionLimitReference]map[policy.PreemptionLimitScopeValue]events{
				prunedLimit: {"cq-a": {now}},
				otherLimit:  {"Global": {expired}},
			},
		},
		"unknown limit is a no-op": {
			events: map[policy.PreemptionLimitReference]map[policy.PreemptionLimitScopeValue]events{
				otherLimit: {"Global": {expired}},
			},
			want: map[policy.PreemptionLimitReference]map[policy.PreemptionLimitScopeValue]events{
				otherLimit: {"Global": {expired}},
			},
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			tracker := NewPreemptionLimitTracker()
			tracker.events = tc.events
			tracker.Prune(now, limit)
			if diff := cmp.Diff(tc.want, tracker.events, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("Unexpected events (-want,+got):\n%s", diff)
			}
		})
	}
}

func TestPreemptionLimitTrackerPruneConcurrent(t *testing.T) {
	const goroutines, recordsPerGoroutine = 10, 100
	const records = goroutines * recordsPerGoroutine
	const windowSeconds = 60
	const prunedLimit, otherLimit = "per-cq", "global"
	start := time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)
	window := windowSeconds * time.Second
	limit := utiltestingalpha.MakePreemptionLimit(prunedLimit, kueuealpha.PreemptingCQLimitScope).LimitWindowSeconds(windowSeconds).Obj()
	consumed := policy.ConsumedLimits{prunedLimit: "cq-a", otherLimit: "Global"}
	// Every recorded event is already expired, so the prunes keep deleting the
	// pruned limit while the records keep recreating it.
	now := start.Add(records*time.Second + window + pruneGracePeriod)

	tracker := NewPreemptionLimitTracker()
	var wg sync.WaitGroup
	for g := range goroutines {
		wg.Go(func() {
			for i := range recordsPerGoroutine {
				tracker.Record(start.Add(time.Duration(g*recordsPerGoroutine+i)*time.Second), consumed)
				tracker.Prune(now, limit)
			}
		})
	}
	wg.Wait()

	got := tracker.events[otherLimit]["Global"]
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
		events map[policy.PreemptionLimitReference]map[policy.PreemptionLimitScopeValue]events
		want   map[policy.PreemptionLimitReference]map[policy.PreemptionLimitScopeValue]events
	}{
		"only the given limit is forgotten": {
			events: map[policy.PreemptionLimitReference]map[policy.PreemptionLimitScopeValue]events{
				forgottenLimit: {"cq-a": {now}, "cq-b": {now}},
				otherLimit:     {"Global": {now}},
			},
			want: map[policy.PreemptionLimitReference]map[policy.PreemptionLimitScopeValue]events{
				otherLimit: {"Global": {now}},
			},
		},
		"unknown limit is a no-op": {
			events: map[policy.PreemptionLimitReference]map[policy.PreemptionLimitScopeValue]events{
				otherLimit: {"Global": {now}},
			},
			want: map[policy.PreemptionLimitReference]map[policy.PreemptionLimitScopeValue]events{
				otherLimit: {"Global": {now}},
			},
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			tracker := NewPreemptionLimitTracker()
			tracker.events = tc.events
			tracker.Forget(forgottenLimit)
			if diff := cmp.Diff(tc.want, tracker.events, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("Unexpected events (-want,+got):\n%s", diff)
			}
		})
	}
}
