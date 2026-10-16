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
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"

	kueuealpha "sigs.k8s.io/kueue/apis/kueue/v1alpha1"
	schdcache "sigs.k8s.io/kueue/pkg/cache/scheduler"
	"sigs.k8s.io/kueue/pkg/scheduler/preemption/policy"
	"sigs.k8s.io/kueue/pkg/workload"
)

// PreemptionLimitMatcher is a PreemptionLimit which applies to the PreemptionConfig
// of a preemptor, with its ClusterQueue selector parsed.
type PreemptionLimitMatcher struct {
	limit                *kueuealpha.PreemptionLimit
	clusterQueueSelector labels.Selector
}

// NewPreemptionLimitMatchers returns the matchers of the PreemptionLimits whose
// configSelector selects config. It returns an error if the configSelector or the
// clusterQueueSelector of any PreemptionLimit is invalid.
func NewPreemptionLimitMatchers(preemptionLimits []kueuealpha.PreemptionLimit, config *kueuealpha.PreemptionConfig) ([]PreemptionLimitMatcher, error) {
	var matchers []PreemptionLimitMatcher
	for i := range preemptionLimits {
		limit := &preemptionLimits[i]
		configSelector, err := selectorOrEverything(limit.Spec.ConfigSelector)
		if err != nil {
			return nil, fmt.Errorf("preemptionLimit %q configSelector: %w", limit.Name, err)
		}
		clusterQueueSelector, err := selectorOrEverything(limit.Spec.ClusterQueueSelector)
		if err != nil {
			return nil, fmt.Errorf("preemptionLimit %q clusterQueueSelector: %w", limit.Name, err)
		}
		if configSelector.Matches(labels.Set(config.Labels)) {
			matchers = append(matchers, PreemptionLimitMatcher{limit: limit, clusterQueueSelector: clusterQueueSelector})
		}
	}
	return matchers, nil
}

// selectorOrEverything returns the selector of ls. A nil ls selects everything,
// unlike metav1.LabelSelectorAsSelector(nil), which selects nothing.
func selectorOrEverything(ls *metav1.LabelSelector) (labels.Selector, error) {
	if ls == nil {
		return labels.Everything(), nil
	}
	return metav1.LabelSelectorAsSelector(ls)
}

// Resolve returns the PreemptionLimits consumed by preempting target, admitted in
// targetCQ, to admit a workload of preemptorCQ, with the scope value each consumes.
func Resolve(matchers []PreemptionLimitMatcher, preemptorCQ, targetCQ *schdcache.ClusterQueueSnapshot, target *workload.Info) policy.ConsumedLimits {
	var consumedLimits policy.ConsumedLimits
	for i := range matchers {
		matcher := &matchers[i]
		if !matcher.matchesClusterQueues(preemptorCQ, targetCQ) {
			continue
		}
		if consumedLimits == nil {
			consumedLimits = make(policy.ConsumedLimits)
		}
		consumedLimits[policy.PreemptionLimitReference(matcher.limit.Name)] = scopeValue(matcher.limit.Spec.Scope, preemptorCQ, targetCQ, target)
	}
	return consumedLimits
}

// matchesClusterQueues returns whether the clusterQueueSelector of the limit selects
// the ClusterQueue of its scope: the preemptor's one for PreemptingClusterQueue, the
// target's one for PreemptedClusterQueue and PreemptedWorkload, and either of them
// for Global.
func (m *PreemptionLimitMatcher) matchesClusterQueues(preemptorCQ, targetCQ *schdcache.ClusterQueueSnapshot) bool {
	switch m.limit.Spec.Scope {
	case kueuealpha.PreemptingCQLimitScope:
		return m.clusterQueueSelector.Matches(labels.Set(preemptorCQ.Labels))
	case kueuealpha.PreemptedCQLimitScope, kueuealpha.PreemptedWorkloadLimitScope:
		return m.clusterQueueSelector.Matches(labels.Set(targetCQ.Labels))
	default:
		// GlobalPreemptionLimitScope, as the API admits no other scope.
		return m.clusterQueueSelector.Matches(labels.Set(preemptorCQ.Labels)) ||
			m.clusterQueueSelector.Matches(labels.Set(targetCQ.Labels))
	}
}

func scopeValue(scope kueuealpha.PreemptionLimitScope, preemptorCQ, targetCQ *schdcache.ClusterQueueSnapshot, target *workload.Info) policy.PreemptionLimitScopeValue {
	switch scope {
	case kueuealpha.PreemptingCQLimitScope:
		return policy.PreemptionLimitScopeValue(preemptorCQ.Name)
	case kueuealpha.PreemptedCQLimitScope:
		return policy.PreemptionLimitScopeValue(targetCQ.Name)
	case kueuealpha.PreemptedWorkloadLimitScope:
		return policy.PreemptionLimitScopeValue(workload.Key(target.Obj))
	default:
		// GlobalPreemptionLimitScope, as the API admits no other scope.
		return policy.PreemptionLimitScopeValue(kueuealpha.GlobalPreemptionLimitScope)
	}
}
