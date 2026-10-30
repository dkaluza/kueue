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
	"strings"
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

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

// limitState tracks preemption events grouped by scope value for a single
// PreemptionLimit, alongside the PreemptionLimit they count for and its
// generation when they were last reset.
type limitState struct {
	// limit is the PreemptionLimit as last synced. It is replaced, never
	// modified, so that snapshots can share it.
	limit              *kueuealpha.PreemptionLimit
	resetGeneration    int64
	eventsByScopeValue map[policy.PreemptionLimitScopeValue]events
}

// newLimitState returns the state of limit, without events.
func newLimitState(limit *kueuealpha.PreemptionLimit) *limitState {
	return &limitState{
		limit:              trackedCopy(limit),
		resetGeneration:    limit.Generation,
		eventsByScopeValue: make(map[policy.PreemptionLimitScopeValue]events),
	}
}

// trackedCopy returns a copy of the parts of limit the tracker relies on,
// leaving out the status and the other metadata.
func trackedCopy(limit *kueuealpha.PreemptionLimit) *kueuealpha.PreemptionLimit {
	return &kueuealpha.PreemptionLimit{
		ObjectMeta: metav1.ObjectMeta{Name: limit.Name, UID: limit.UID, Generation: limit.Generation},
		Spec:       *limit.Spec.DeepCopy(),
	}
}

// isCompatibleWith reports whether the tracked state can continue to be used
// for limit, i.e. whether limit has the same UID, scope, and selectors that
// this state was initialized with.
func (ls *limitState) isCompatibleWith(limit *kueuealpha.PreemptionLimit) bool {
	return ls.limit.UID == limit.UID &&
		ls.limit.Spec.Scope == limit.Spec.Scope &&
		equality.Semantic.DeepEqual(ls.limit.Spec.ConfigSelector, limit.Spec.ConfigSelector) &&
		equality.Semantic.DeepEqual(ls.limit.Spec.PreemptorClusterQueueSelector, limit.Spec.PreemptorClusterQueueSelector) &&
		equality.Semantic.DeepEqual(ls.limit.Spec.PreemptedClusterQueueSelector, limit.Spec.PreemptedClusterQueueSelector)
}

// counts returns the number of events within the window of the tracked
// PreemptionLimit ending at now, by scope value. Scope values with 0 events
// are omitted.
func (ls *limitState) counts(now time.Time) map[policy.PreemptionLimitScopeValue]int32 {
	counts := make(map[policy.PreemptionLimitScopeValue]int32)
	cutoff := windowStart(now, ls.limit)
	for scopeValue, e := range ls.eventsByScopeValue {
		if count := e.countSince(cutoff); count > 0 {
			counts[scopeValue] = int32(count)
		}
	}
	return counts
}

// PreemptionLimitTracker records the timestamps of confirmed configurable preemptions for
// each PreemptionLimit and scope value. It is safe for concurrent use.
type PreemptionLimitTracker struct {
	mu     sync.RWMutex
	limits map[policy.PreemptionLimitReference]*limitState
}

// NewPreemptionLimitTracker creates an empty PreemptionLimitTracker.
func NewPreemptionLimitTracker() *PreemptionLimitTracker {
	return &PreemptionLimitTracker{
		limits: make(map[policy.PreemptionLimitReference]*limitState),
	}
}

// Record records a preemption at ts for every PreemptionLimit and scope value
// pair in consumedLimits. Pairs that are stale for the tracked PreemptionLimit
// (see ConsumedLimit.IsStaleFor) are skipped, so that late records neither
// resurrect a deleted PreemptionLimit nor count against a recreated one or one
// whose scope or selectors changed. As a tradeoff, preemptions before the
// first Sync of a new PreemptionLimit are lost.
func (t *PreemptionLimitTracker) Record(ts time.Time, consumedLimits policy.ConsumedLimits) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for limit, consumed := range consumedLimits {
		ls, found := t.limits[limit]
		if !found || consumed.IsStaleFor(ls.limit.UID, ls.resetGeneration) {
			continue
		}
		ls.eventsByScopeValue[consumed.ScopeValue] = ls.eventsByScopeValue[consumed.ScopeValue].insert(ts)
	}
}

// windowStart returns the start of the window of limit ending at now.
func windowStart(now time.Time, limit *kueuealpha.PreemptionLimit) time.Time {
	return now.Add(-time.Duration(limit.Spec.LimitWindowSeconds) * time.Second)
}

// Snapshot returns a snapshot of the tracked PreemptionLimits, as last synced,
// with the preemptions recorded within their windows ending at now. As it is
// taken from the tracker only, the PreemptionLimits are always consistent with
// their preemptions, including while an update is not synced yet.
func (t *PreemptionLimitTracker) Snapshot(now time.Time) *PreemptionLimitsSnapshot {
	t.mu.RLock()
	defer t.mu.RUnlock()
	snapshot := &PreemptionLimitsSnapshot{
		limits:   make([]*kueuealpha.PreemptionLimit, 0, len(t.limits)),
		recorded: make(preemptionCounts, len(t.limits)),
	}
	for ref, ls := range t.limits {
		snapshot.limits = append(snapshot.limits, ls.limit)
		if counts := ls.counts(now); len(counts) > 0 {
			snapshot.recorded[ref] = counts
		}
	}
	// Sorted, so that errors are deterministic.
	slices.SortFunc(snapshot.limits, func(a, b *kueuealpha.PreemptionLimit) int {
		return strings.Compare(a.Name, b.Name)
	})
	return snapshot
}

// Counts returns the number of preemptions recorded for limit within its
// window ending at now, by scope value. Scope values with 0 events are
// omitted, and an untracked limit has no counts.
func (t *PreemptionLimitTracker) Counts(now time.Time, limit policy.PreemptionLimitReference) map[policy.PreemptionLimitScopeValue]int32 {
	t.mu.RLock()
	defer t.mu.RUnlock()
	ls, found := t.limits[limit]
	if !found {
		return nil
	}
	return ls.counts(now)
}

// Sync starts tracking limit, or resets its events if it was recreated or its
// scope or selectors changed, saving its generation as the last reset.
// Otherwise, it takes the new spec into account (e.g. a limit or window
// change), and prunes the events older than the window plus pruneGracePeriod;
// events pruned under a shorter window are not restored.
func (t *PreemptionLimitTracker) Sync(now time.Time, limit *kueuealpha.PreemptionLimit) {
	ref := policy.PreemptionLimitReference(limit.Name)
	t.mu.Lock()
	defer t.mu.Unlock()
	ls, found := t.limits[ref]
	if !found || !ls.isCompatibleWith(limit) {
		t.limits[ref] = newLimitState(limit)
		return
	}
	ls.limit = trackedCopy(limit)
	cutoff := windowStart(now, ls.limit).Add(-pruneGracePeriod)
	for scopeValue, e := range ls.eventsByScopeValue {
		if e = e.pruneBefore(cutoff); len(e) == 0 {
			delete(ls.eventsByScopeValue, scopeValue)
		} else {
			ls.eventsByScopeValue[scopeValue] = e
		}
	}
}

// Forget stops tracking limit, dropping all its events.
func (t *PreemptionLimitTracker) Forget(limit policy.PreemptionLimitReference) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.limits, limit)
}
