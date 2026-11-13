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
// consumes, and returns those limits.
//
// If any limit has no budget left, it takes nothing and returns false along
// with the strictest PreemptionLimitBlock across the exhausted limits on target.
func (b *PreemptionLimitBudget) TryConsume(
	preemptor, target *workload.Info,
	ruleReference policy.PreemptionConfigRuleReference,
	preemptorCQ, targetCQ *schdcache.ClusterQueueSnapshot,
) (policy.ConsumedLimits, *policy.PreemptionLimitBlock, bool) {
	consumedLimits := b.limiter.consumedLimits(preemptor, target, ruleReference, preemptorCQ, targetCQ)
	if len(consumedLimits) == 0 {
		return nil, nil, true
	}
	var blocking *policy.PreemptionLimitBlock
	for limit, consumed := range consumedLimits {
		if b.remaining(limit, consumed.ScopeValue) > 0 {
			continue
		}
		slotTime := b.limiter.snapshot.nextSlotTime(limit, consumed.ScopeValue)
		if stricterTargetBlock(limit, slotTime, blocking) {
			blocking = &policy.PreemptionLimitBlock{
				Limit:   limit,
				Scope:   b.limiter.snapshot.scope(limit),
				RetryAt: slotTime,
			}
		}
	}
	if blocking != nil {
		if blocking.RetryAt != nil {
			b.limiter.log.V(5).Info("Skipping preemption candidate exceeding a PreemptionLimit",
				"candidate", klog.KObj(target.Obj), "preemptionLimit", blocking.Limit, "scopeValue", consumedLimits[blocking.Limit].ScopeValue, "nextSlotTime", *blocking.RetryAt)
		} else {
			b.limiter.log.V(5).Info("Skipping preemption candidate exceeding a PreemptionLimit",
				"candidate", klog.KObj(target.Obj), "preemptionLimit", blocking.Limit, "scopeValue", consumedLimits[blocking.Limit].ScopeValue)
		}
		return nil, blocking, false
	}
	for limit, consumed := range consumedLimits {
		b.taken.add(limit, consumed.ScopeValue, 1)
	}
	return consumedLimits, nil, true
}

// stricterTargetBlock reports whether (limit, slotTime) clears later than current
// on the same candidate (where nil means waiting alone never clears it), breaking
// ties deterministically by Limit name.
func stricterTargetBlock(limit policy.PreemptionLimitReference, slotTime *time.Time, current *policy.PreemptionLimitBlock) bool {
	switch {
	case current == nil:
		return true
	case slotTime == nil && current.RetryAt == nil:
		return limit < current.Limit
	case slotTime == nil:
		return true
	case current.RetryAt == nil:
		return false
	case slotTime.After(*current.RetryAt):
		return true
	case current.RetryAt.After(*slotTime):
		return false
	default:
		return limit < current.Limit
	}
}

// remaining returns how many more preemptions can be taken from this budget for limit and scopeValue.
func (b *PreemptionLimitBudget) remaining(limit policy.PreemptionLimitReference, scopeValue policy.PreemptionLimitScopeValue) int32 {
	return b.limiter.snapshot.allowance(limit, scopeValue) - b.taken[limit][scopeValue]
}
