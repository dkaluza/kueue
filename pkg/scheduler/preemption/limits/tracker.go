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
	"time"

	kueuealpha "sigs.k8s.io/kueue/apis/kueue/v1alpha1"
	"sigs.k8s.io/kueue/pkg/scheduler/preemption/policy"
)

// pruneGracePeriod delays pruning past the window, so that readers whose
// "now" lags slightly behind still find every event they count.
const pruneGracePeriod = time.Minute

// preemptionCounts holds a number of preemptions per PreemptionLimit and scope value.
// A missing entry counts as 0.
type preemptionCounts map[policy.PreemptionLimitReference]map[policy.PreemptionLimitScopeValue]int32

// add adds n preemptions for limit and scopeValue.
func (c preemptionCounts) add(limit policy.PreemptionLimitReference, scopeValue policy.PreemptionLimitScopeValue, n int32) {
	if c[limit] == nil {
		c[limit] = make(map[policy.PreemptionLimitScopeValue]int32)
	}
	c[limit][scopeValue] += n
}

// events holds preemption timestamps in ascending order.
type events []time.Time

// insert adds ts while keeping the events sorted. Timestamps usually arrive in
// order, so the insertion point is searched from the tail.
func (e events) insert(ts time.Time) events {
	i := len(e)
	for i > 0 && e[i-1].After(ts) {
		i--
	}
	return slices.Insert(e, i, ts)
}

// countSince returns the number of events at or after cutoff.
func (e events) countSince(cutoff time.Time) int {
	i, _ := slices.BinarySearchFunc(e, cutoff, time.Time.Compare) // first event >= cutoff
	return len(e) - i
}

// pruneBefore reslices e to drop events before cutoff without copying. Dropped
// elements stay in memory until a later insert reallocates the slice.
func (e events) pruneBefore(cutoff time.Time) events {
	i, _ := slices.BinarySearchFunc(e, cutoff, time.Time.Compare) // first event >= cutoff
	return e[i:]
}

// PreemptionLimitTracker records the timestamps of confirmed configurable preemptions for
// each PreemptionLimit and scope value. It is safe for concurrent use.
type PreemptionLimitTracker struct {
	mu     sync.RWMutex
	events map[policy.PreemptionLimitReference]map[policy.PreemptionLimitScopeValue]events
}

// NewPreemptionLimitTracker creates an empty PreemptionLimitTracker.
func NewPreemptionLimitTracker() *PreemptionLimitTracker {
	return &PreemptionLimitTracker{
		events: make(map[policy.PreemptionLimitReference]map[policy.PreemptionLimitScopeValue]events),
	}
}

// Record records a preemption at ts for every PreemptionLimit and scope value
// pair in consumedLimits.
func (t *PreemptionLimitTracker) Record(ts time.Time, consumedLimits policy.ConsumedLimits) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for limit, scopeValue := range consumedLimits {
		scopes, found := t.events[limit]
		if !found {
			scopes = make(map[policy.PreemptionLimitScopeValue]events)
			t.events[limit] = scopes
		}
		scopes[scopeValue] = scopes[scopeValue].insert(ts)
	}
}

// windowStart returns the start of the window of limit ending at now.
func windowStart(now time.Time, limit *kueuealpha.PreemptionLimit) time.Time {
	return now.Add(-time.Duration(limit.Spec.LimitWindowSeconds) * time.Second)
}

// Snapshot returns a snapshot of preemptionLimits with the preemptions recorded
// within their windows ending at now. The caller must not modify preemptionLimits.
func (t *PreemptionLimitTracker) Snapshot(now time.Time, preemptionLimits []kueuealpha.PreemptionLimit) *PreemptionLimitsSnapshot {
	limits := make([]*kueuealpha.PreemptionLimit, len(preemptionLimits))
	limitByName := make(map[policy.PreemptionLimitReference]*kueuealpha.PreemptionLimit, len(preemptionLimits))
	for i := range preemptionLimits {
		limits[i] = &preemptionLimits[i]
		limitByName[policy.PreemptionLimitReference(preemptionLimits[i].Name)] = &preemptionLimits[i]
	}
	return &PreemptionLimitsSnapshot{limits: limits, limitByName: limitByName, recorded: t.Counts(now, limits)}
}

// Counts returns a consistent snapshot of the preemptions recorded for limits
// within their windows [windowStart, now]. Scope values with 0 events are omitted.
func (t *PreemptionLimitTracker) Counts(now time.Time, limits []*kueuealpha.PreemptionLimit) preemptionCounts {
	counts := make(preemptionCounts, len(limits))
	t.mu.RLock()
	defer t.mu.RUnlock()
	for _, limit := range limits {
		ref := policy.PreemptionLimitReference(limit.Name)
		cutoff := windowStart(now, limit)
		for scopeValue, e := range t.events[ref] {
			if count := e.countSince(cutoff); count > 0 {
				counts.add(ref, scopeValue, int32(count))
			}
		}
	}
	return counts
}

// Prune drops the events of limit recorded before now minus its window and
// pruneGracePeriod, and forgets the scope values (and the limit) left without events.
func (t *PreemptionLimitTracker) Prune(now time.Time, limit *kueuealpha.PreemptionLimit) {
	ref := policy.PreemptionLimitReference(limit.Name)
	cutoff := windowStart(now, limit).Add(-pruneGracePeriod)
	t.mu.Lock()
	defer t.mu.Unlock()
	scopes := t.events[ref]
	for scopeValue, e := range scopes {
		if e = e.pruneBefore(cutoff); len(e) == 0 {
			delete(scopes, scopeValue)
		} else {
			scopes[scopeValue] = e
		}
	}
	if len(scopes) == 0 {
		delete(t.events, ref)
	}
}

// Forget drops all the events of limit.
func (t *PreemptionLimitTracker) Forget(limit policy.PreemptionLimitReference) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.events, limit)
}
