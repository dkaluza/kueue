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

// PreemptionLimitBudget holds the number of preemptions the PreemptionLimits still allow during one
// preemption search. The budget of each PreemptionLimit and scope value is read from
// the tracker on first use, then reduced by every preemption taken from it. It is only
// accurate while no preemption is recorded, which holds during a scheduling attempt.
type PreemptionLimitBudget struct {
	tracker   *PreemptionLimitTracker
	now       time.Time
	limits    map[policy.PreemptionLimitReference]*kueuealpha.PreemptionLimit
	remaining map[policy.PreemptionLimitReference]map[policy.PreemptionLimitScopeValue]int32
}

// NewPreemptionLimitBudget returns the PreemptionLimitBudget of preemptionLimits, counting the preemptions recorded
// by tracker within their windows ending at now.
func NewPreemptionLimitBudget(tracker *PreemptionLimitTracker, now time.Time, preemptionLimits []kueuealpha.PreemptionLimit) *PreemptionLimitBudget {
	limits := make(map[policy.PreemptionLimitReference]*kueuealpha.PreemptionLimit, len(preemptionLimits))
	for i := range preemptionLimits {
		limits[policy.PreemptionLimitReference(preemptionLimits[i].Name)] = &preemptionLimits[i]
	}
	return &PreemptionLimitBudget{
		tracker:   tracker,
		now:       now,
		limits:    limits,
		remaining: make(map[policy.PreemptionLimitReference]map[policy.PreemptionLimitScopeValue]int32),
	}
}

// TryConsume takes one preemption from the budget of every PreemptionLimit and scope
// value in consumedLimits if all of them have budget left. Otherwise, it takes nothing
// and returns a PreemptionLimit without budget left.
func (b *PreemptionLimitBudget) TryConsume(consumedLimits policy.ConsumedLimits) (policy.PreemptionLimitReference, bool) {
	for limit, scopeValue := range consumedLimits {
		if b.remainingFor(limit, scopeValue) <= 0 {
			return limit, false
		}
	}
	for limit, scopeValue := range consumedLimits {
		b.remaining[limit][scopeValue]--
	}
	return "", true
}

// remainingFor returns the budget left for limit and scopeValue, reading it from the
// tracker on first use.
func (b *PreemptionLimitBudget) remainingFor(limit policy.PreemptionLimitReference, scopeValue policy.PreemptionLimitScopeValue) int32 {
	scopes, found := b.remaining[limit]
	if !found {
		scopes = make(map[policy.PreemptionLimitScopeValue]int32)
		b.remaining[limit] = scopes
	}
	remaining, found := scopes[scopeValue]
	if !found {
		preemptionLimit := b.limits[limit]
		remaining = preemptionLimit.Spec.Limit - b.tracker.Count(b.now, preemptionLimit, scopeValue)
		scopes[scopeValue] = remaining
	}
	return remaining
}
