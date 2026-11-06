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
	"slices"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"

	kueuealpha "sigs.k8s.io/kueue/apis/kueue/v1alpha1"
	schdcache "sigs.k8s.io/kueue/pkg/cache/scheduler"
	"sigs.k8s.io/kueue/pkg/scheduler/preemption/policy"
	"sigs.k8s.io/kueue/pkg/workload"
)

// preemptionLimitMatcher determines whether a PreemptionLimit applies to a
// preemption and which scope value it counts against.
type preemptionLimitMatcher struct {
	limit                         *kueuealpha.PreemptionLimit
	preemptorClusterQueueSelector labels.Selector
	preemptedClusterQueueSelector labels.Selector
}

// selectorError reports an invalid selector on a PreemptionLimit.
type selectorError struct {
	PreemptionLimit string
	Selector        string
	Err             error
}

func (e *selectorError) Error() string {
	return fmt.Sprintf("preemptionLimit %q %s: %v", e.PreemptionLimit, e.Selector, e.Err)
}

func (e *selectorError) Unwrap() error {
	return e.Err
}

// newPreemptionLimitMatcher returns the matcher for limit, or nil if limit
// doesn't apply to config. It returns an error if a selector it needs is invalid.
func newPreemptionLimitMatcher(limit *kueuealpha.PreemptionLimit, config *kueuealpha.PreemptionConfig) (*preemptionLimitMatcher, error) {
	configSelector, err := selectorOrEverything(limit.Spec.ConfigSelector)
	if err != nil {
		return nil, &selectorError{PreemptionLimit: limit.Name, Selector: "configSelector", Err: err}
	}
	if !configSelector.Matches(labels.Set(config.Labels)) {
		return nil, nil
	}
	preemptorClusterQueueSelector, err := selectorOrEverything(limit.Spec.PreemptorClusterQueueSelector)
	if err != nil {
		return nil, &selectorError{PreemptionLimit: limit.Name, Selector: "preemptorClusterQueueSelector", Err: err}
	}
	preemptedClusterQueueSelector, err := selectorOrEverything(limit.Spec.PreemptedClusterQueueSelector)
	if err != nil {
		return nil, &selectorError{PreemptionLimit: limit.Name, Selector: "preemptedClusterQueueSelector", Err: err}
	}
	return &preemptionLimitMatcher{
		limit:                         limit,
		preemptorClusterQueueSelector: preemptorClusterQueueSelector,
		preemptedClusterQueueSelector: preemptedClusterQueueSelector,
	}, nil
}

// selectorOrEverything is metav1.LabelSelectorAsSelector, except that nil selects everything.
func selectorOrEverything(ls *metav1.LabelSelector) (labels.Selector, error) {
	if ls == nil {
		return labels.Everything(), nil
	}
	return metav1.LabelSelectorAsSelector(ls)
}

// matches returns true when arguments are passing filters on associated PreemptionLimitSpec.
func (m *preemptionLimitMatcher) matches(preemptorCQ, targetCQ *schdcache.ClusterQueueSnapshot, ruleReference policy.PreemptionConfigRuleReference) bool {
	return m.preemptorClusterQueueSelector.Matches(labels.Set(preemptorCQ.Labels)) &&
		m.preemptedClusterQueueSelector.Matches(labels.Set(targetCQ.Labels)) &&
		m.matchRuleNames(ruleReference)
}

func (m *preemptionLimitMatcher) matchRuleNames(ruleReference policy.PreemptionConfigRuleReference) bool {
	// Match every rule when no ruleNames provided.
	if len(m.limit.Spec.RuleNames) == 0 {
		return true
	}

	return slices.Contains(m.limit.Spec.RuleNames, string(ruleReference))
}

// scopeValue returns the scope value that preempting target for preemptor counts against.
func (m *preemptionLimitMatcher) scopeValue(preemptor, target *workload.Info) policy.PreemptionLimitScopeValue {
	switch m.limit.Spec.Scope {
	case kueuealpha.PreemptorCQLimitScope:
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
