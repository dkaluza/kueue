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
	"time"

	kueuealpha "sigs.k8s.io/kueue/apis/kueue/v1alpha1"
	"sigs.k8s.io/kueue/pkg/scheduler/preemption/policy"
)

// PreemptionLimitsSnapshot captures PreemptionLimits and the preemption events
// recorded against them at a single point in time. It allows to reason about
// the current limits state in one scheduling cycle without danger of concurrent
// modification of PreemptionLimits or currently tracked preemptions, caused by
// user actions (e.g. delete of a limit resource).
//
// Moreover, snapshot allows to update the underlying captured state via Record
// method, to take into account additional preemptions that were issued after it
// was captured, for example to track preemptions already issued in the same
// scheduling cycle.
type PreemptionLimitsSnapshot struct {
	limitByName map[policy.PreemptionLimitReference]*kueuealpha.PreemptionLimit
	// events holds the preemption timestamps within the window of each limit when
	// the snapshot was taken, plus ones added via Record.
	events map[policy.PreemptionLimitReference]map[policy.PreemptionLimitScopeValue]events
}

// Record records a preemption at ts in the snapshot for every PreemptionLimit
// and scope value in consumedLimits.
func (s *PreemptionLimitsSnapshot) Record(ts time.Time, consumedLimits policy.ConsumedLimits) {
	for limit, consumed := range consumedLimits {
		scopes, found := s.events[limit]
		if !found {
			scopes = make(map[policy.PreemptionLimitScopeValue]events)
			s.events[limit] = scopes
		}
		scopes[consumed.ScopeValue] = scopes[consumed.ScopeValue].insert(ts)
	}
}

// WithinLimits reports whether preemptions fit together within the remaining
// limits in the snapshot. A nil PreemptionLimitsSnapshot allows no preemptions.
func (s *PreemptionLimitsSnapshot) WithinLimits(preemptions []policy.ConsumedLimits) bool {
	if s == nil {
		return len(preemptions) == 0
	}
	taken := preemptionCounts{}
	for _, consumedLimits := range preemptions {
		for limit, consumed := range consumedLimits {
			taken.add(limit, consumed.ScopeValue, 1)
			if taken[limit][consumed.ScopeValue] > s.allowance(limit, consumed.ScopeValue) {
				return false
			}
		}
	}
	return true
}

// allowance returns how many more preemptions are allowed for limit and
// scopeValue in the snapshot, or 0 if limit is not in the snapshot.
func (s *PreemptionLimitsSnapshot) allowance(limit policy.PreemptionLimitReference, scopeValue policy.PreemptionLimitScopeValue) int32 {
	preemptionLimit, found := s.limitByName[limit]
	if !found {
		return 0
	}
	return preemptionLimit.Spec.Limit - int32(len(s.events[limit][scopeValue]))
}

// nextSlotTime returns the earliest time when enough recorded events expire to
// allow count additional preemptions for limit and scopeValue.
//
// It returns nil if they are already allowed, or if waiting for events to
// expire is never enough.
func (s *PreemptionLimitsSnapshot) nextSlotTime(limit policy.PreemptionLimitReference, scopeValue policy.PreemptionLimitScopeValue, count int32) *time.Time {
	preemptionLimit, found := s.limitByName[limit]
	if !found || count <= 0 || count > preemptionLimit.Spec.Limit {
		return nil
	}
	recorded := s.events[limit][scopeValue]
	index := len(recorded) + int(count-preemptionLimit.Spec.Limit) - 1
	if index < 0 {
		return nil
	}
	return new(recorded[index].Add(time.Duration(preemptionLimit.Spec.LimitWindowSeconds) * time.Second))
}
