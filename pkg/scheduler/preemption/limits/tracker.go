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

// pruneBefore drops the events older than cutoff by reslicing, so the kept
// events are not copied. The dropped events stay in the backing array until a
// later insert grows it into a new one, or the bucket is deleted, only then
// memory is cleaned.
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

// Count returns the number of preemptions recorded for limit and scopeValue within
// the window of limit ending at now, including those recorded exactly at its start.
func (t *PreemptionLimitTracker) Count(now time.Time, limit *kueuealpha.PreemptionLimit, scopeValue policy.PreemptionLimitScopeValue) int32 {
	cutoff := windowStart(now, limit)
	t.mu.RLock()
	defer t.mu.RUnlock()
	return int32(t.events[policy.PreemptionLimitReference(limit.Name)][scopeValue].countSince(cutoff))
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
