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

	"k8s.io/apimachinery/pkg/types"

	kueuealpha "sigs.k8s.io/kueue/apis/kueue/v1alpha1"
	"sigs.k8s.io/kueue/pkg/scheduler/preemption/policy"
	"sigs.k8s.io/kueue/pkg/workload"
)

// Resolve returns, for each PreemptionLimit, the scope value consumed by
// preempting target to admit preemptor.
func Resolve(preemptionLimits []kueuealpha.PreemptionLimit, preemptor, target *workload.Info) policy.ConsumedLimits {
	if len(preemptionLimits) == 0 {
		return nil
	}
	consumed := make(policy.ConsumedLimits, len(preemptionLimits))
	for i := range preemptionLimits {
		limit := &preemptionLimits[i]
		consumed[policy.PreemptionLimitReference(limit.Name)] = policy.ConsumedLimit{
			UID:        limit.UID,
			Scope:      limit.Spec.Scope,
			ScopeValue: scopeValue(limit.Spec.Scope, preemptor, target),
		}
	}
	return consumed
}

func scopeValue(scope kueuealpha.PreemptionLimitScope, preemptor, target *workload.Info) policy.PreemptionLimitScopeValue {
	switch scope {
	case kueuealpha.PreemptingCQLimitScope:
		return policy.PreemptionLimitScopeValue(preemptor.ClusterQueue)
	case kueuealpha.PreemptedCQLimitScope:
		return policy.PreemptionLimitScopeValue(target.ClusterQueue)
	case kueuealpha.PreemptedWorkloadLimitScope:
		return policy.PreemptionLimitScopeValue(workload.Key(target.Obj))
	default:
		// GlobalPreemptionLimitScope, as the API admits no other scope.
		return policy.PreemptionLimitScopeValue(kueuealpha.GlobalPreemptionLimitScope)
	}
}

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

// pruneBefore drops the events older than cutoff by reslicing, so the kept
// events are not copied. The dropped events stay in the backing array until a
// later insert grows it into a new one, or the bucket is deleted, only then
// memory is cleaned.
func (e events) pruneBefore(cutoff time.Time) events {
	i, _ := slices.BinarySearchFunc(e, cutoff, time.Time.Compare) // first event >= cutoff
	return e[i:]
}

// limitState holds the preemption events of a single PreemptionLimit, by
// scope value, along with the UID and scope of the PreemptionLimit they count
// for.
type limitState struct {
	uid    types.UID
	scope  kueuealpha.PreemptionLimitScope
	events map[policy.PreemptionLimitScopeValue]events
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
// pair in consumedLimits. Pairs resolved against a PreemptionLimit not tracked
// with the same UID and scope (see Sync) are skipped, so that late records
// neither resurrect a deleted PreemptionLimit nor count against a recreated or
// rescoped one. As a tradeoff, preemptions before the first Sync of a new
// PreemptionLimit are lost.
func (t *PreemptionLimitTracker) Record(ts time.Time, consumedLimits policy.ConsumedLimits) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for limit, consumed := range consumedLimits {
		state, found := t.limits[limit]
		if !found || state.uid != consumed.UID || state.scope != consumed.Scope {
			continue
		}
		state.events[consumed.ScopeValue] = state.events[consumed.ScopeValue].insert(ts)
	}
}

// Sync starts tracking limit, or resets its events if it was recreated or its
// scope changed. Otherwise, it prunes the events older than the window plus
// pruneGracePeriod; events pruned under a shorter window are not restored.
func (t *PreemptionLimitTracker) Sync(now time.Time, limit *kueuealpha.PreemptionLimit) {
	ref := policy.PreemptionLimitReference(limit.Name)
	t.mu.Lock()
	defer t.mu.Unlock()
	state, found := t.limits[ref]
	if !found || state.uid != limit.UID || state.scope != limit.Spec.Scope {
		t.limits[ref] = &limitState{
			uid:    limit.UID,
			scope:  limit.Spec.Scope,
			events: make(map[policy.PreemptionLimitScopeValue]events),
		}
		return
	}
	window := time.Duration(limit.Spec.LimitWindowSeconds) * time.Second
	cutoff := now.Add(-window - pruneGracePeriod)
	for scopeValue, e := range state.events {
		if e = e.pruneBefore(cutoff); len(e) == 0 {
			delete(state.events, scopeValue)
		} else {
			state.events[scopeValue] = e
		}
	}
}

// Forget stops tracking limit, dropping all its events.
func (t *PreemptionLimitTracker) Forget(limit policy.PreemptionLimitReference) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.limits, limit)
}
