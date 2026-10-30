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
	"k8s.io/klog/v2"

	schdcache "sigs.k8s.io/kueue/pkg/cache/scheduler"
	"sigs.k8s.io/kueue/pkg/scheduler/preemption/policy"
	"sigs.k8s.io/kueue/pkg/workload"
)

// PreemptionLimitBudget counts the preemptions taken from the PreemptionLimits
// during one pass of candidate selection.
type PreemptionLimitBudget struct {
	limiter *PreemptionLimiter
	// taken holds the preemptions taken from this budget.
	taken preemptionCounts
}

// NewBudget returns an empty budget for one pass of candidate selection.
// A budget from a nil PreemptionLimiter imposes no limits.
func (l *PreemptionLimiter) NewBudget() *PreemptionLimitBudget {
	return &PreemptionLimitBudget{
		limiter: l,
		taken:   preemptionCounts{},
	}
}

// TryConsume takes one preemption from every limit that preempting target
// consumes, and returns those limits. If any limit has no budget left, it takes
// nothing and returns false.
func (b *PreemptionLimitBudget) TryConsume(
	preemptor, target *workload.Info,
	preemptorCQ, targetCQ *schdcache.ClusterQueueSnapshot,
) (policy.ConsumedLimits, bool) {
	consumedLimits := b.limiter.consumedLimits(preemptor, target, preemptorCQ, targetCQ)
	if len(consumedLimits) == 0 {
		return nil, true
	}
	if limit, ok := b.take(consumedLimits); !ok {
		b.limiter.log.V(5).Info("Skipping preemption candidate exceeding a PreemptionLimit",
			"candidate", klog.KObj(target.Obj), "preemptionLimit", limit, "scopeValue", consumedLimits[limit])
		return nil, false
	}
	return consumedLimits, true
}

// take consumes one preemption from each limit in consumedLimits, or takes
// nothing and returns a limit that has no budget left.
func (b *PreemptionLimitBudget) take(consumedLimits policy.ConsumedLimits) (policy.PreemptionLimitReference, bool) {
	for limit, scopeValue := range consumedLimits {
		if b.remaining(limit, scopeValue) <= 0 {
			return limit, false
		}
	}
	for limit, scopeValue := range consumedLimits {
		b.taken.add(limit, scopeValue, 1)
	}
	return "", true
}

// remaining returns how many more preemptions can be taken from this budget for limit and scopeValue.
func (b *PreemptionLimitBudget) remaining(limit policy.PreemptionLimitReference, scopeValue policy.PreemptionLimitScopeValue) int32 {
	return b.limiter.snapshot.allowance(limit, scopeValue) - b.taken[limit][scopeValue]
}
