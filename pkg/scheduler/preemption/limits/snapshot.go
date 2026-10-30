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
	kueuealpha "sigs.k8s.io/kueue/apis/kueue/v1alpha1"
	"sigs.k8s.io/kueue/pkg/scheduler/preemption/policy"
)

// PreemptionLimitsSnapshot captures PreemptionLimits and the current number of
// preemptions against them at a single point in time. It allows to reason about
// the current limits state in one scheduling cycle without danger of concurrent
// modification of PreemptionLimits or currently tracked preemptions, caused by
// user actions (e.g. delete of a limit resource).
//
// Moreover, snapshot allows to update the underlying captured state via Record
// method, to take into account additional preemptions that were issued after it
// was captured, for example to track preemptions already issued in the same
// scheduling cycle.
type PreemptionLimitsSnapshot struct {
	// limits are kept in the given order, so that errors are deterministic.
	limits      []*kueuealpha.PreemptionLimit
	limitByName map[policy.PreemptionLimitReference]*kueuealpha.PreemptionLimit
	// recorded holds the preemptions recorded within the window of each limit when
	// the snapshot was taken, plus ones added with `Record` method.
	recorded preemptionCounts
}

// Record updates the snapshot by incrementing the recorded preemptions for every
// PreemptionLimit and scope value in consumedLimits.
func (s *PreemptionLimitsSnapshot) Record(consumedLimits policy.ConsumedLimits) {
	for limit, scopeValue := range consumedLimits {
		s.recorded.add(limit, scopeValue, 1)
	}
}

// WithinLimits reports whether all preemptions in consumedLimits fit together
// within the remaining limits in the snapshot. A nil PreemptionLimitsSnapshot
// allows no preemptions.
func (s *PreemptionLimitsSnapshot) WithinLimits(consumedLimits []policy.ConsumedLimits) bool {
	if s == nil {
		return len(consumedLimits) == 0
	}
	taken := preemptionCounts{}
	for _, consumed := range consumedLimits {
		for limit, scopeValue := range consumed {
			taken.add(limit, scopeValue, 1)
			if taken[limit][scopeValue] > s.allowance(limit, scopeValue) {
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
	return preemptionLimit.Spec.Limit - s.recorded[limit][scopeValue]
}
