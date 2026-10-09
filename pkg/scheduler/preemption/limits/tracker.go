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

	"sigs.k8s.io/kueue/pkg/scheduler/preemption/policy"
)

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
func (t *PreemptionLimitTracker) Record(ts time.Time, consumedLimits map[policy.PreemptionLimitReference]policy.PreemptionLimitScopeValue) {
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
