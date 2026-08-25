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

package config

import (
	"iter"

	"github.com/go-logr/logr"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/utils/clock"
	"sigs.k8s.io/kueue/apis/kueue/v1beta2"
	schdcache "sigs.k8s.io/kueue/pkg/cache/scheduler"
	"sigs.k8s.io/kueue/pkg/resources"
	"sigs.k8s.io/kueue/pkg/scheduler/preemption/classical"
	"sigs.k8s.io/kueue/pkg/scheduler/preemption/config/selectors"
	"sigs.k8s.io/kueue/pkg/workload"
)

type SelectorFactory func(rules []v1beta2.PreemptionRule) selectors.CandidateSelector

type preemptionEvaluator struct {
	log             logr.Logger
	clock           clock.Clock
	config          v1beta2.PreemptionConfig
	selectorFactory SelectorFactory
}

func NewPreemptionEvaluator(log logr.Logger, clock clock.Clock, config v1beta2.PreemptionConfig, selectorFactory SelectorFactory) *preemptionEvaluator {
	return &preemptionEvaluator{
		log:             log,
		clock:           clock,
		config:          config,
		selectorFactory: selectorFactory,
	}
}

func (p *preemptionEvaluator) Iter(preemptor *workload.Info, preemptorCq *schdcache.ClusterQueueSnapshot, frsNeedPreemption sets.Set[resources.FlavorResource]) (iter.Seq[*workload.Info], error) {
	candidates, err := p.findCandidates(preemptor, preemptorCq, frsNeedPreemption)
	if err != nil {
		return nil, err
	}

	return func(yield func(*workload.Info) bool) {
		for _, candidate := range candidates {
			if !yield(candidate) {
				return
			}
		}
	}, nil
}

func (p *preemptionEvaluator) findCandidates(preemptor *workload.Info, preemptorCq *schdcache.ClusterQueueSnapshot, frsNeedPreemption sets.Set[resources.FlavorResource]) ([]*workload.Info, error) {
	activeRules := []v1beta2.PreemptionRule{}
	for _, rule := range p.config.Spec.Rules {
		isActive, err := p.isActiveTrigger(rule, preemptor)
		if err != nil {
			return nil, err
		}

		if isActive {
			activeRules = append(activeRules, rule)
		}
	}

	selector := p.selectorFactory(activeRules)
	candidates := []*workload.Info{}

	for _, targetCq := range preemptorCq.Parent().Root().SubtreeClusterQueues() {
		if !cqIsBorrowing(targetCq, frsNeedPreemption) {
			continue
		}

		targetCandidates := []*workload.Info{}
		for _, wlInfo := range targetCq.Workloads {
			if classical.WorkloadUsesResources(wlInfo, frsNeedPreemption) {
				targetCandidates = append(targetCandidates, wlInfo)
			}
		}

		targetCandidates = selector.Filter(p.log, preemptor, targetCandidates)
		candidates = append(candidates, targetCandidates...)
	}

	return candidates, nil
}

func cqIsBorrowing(cq *schdcache.ClusterQueueSnapshot, frsNeedPreemption sets.Set[resources.FlavorResource]) bool {
	if !cq.HasParent() {
		return false
	}
	for fr := range frsNeedPreemption {
		if cq.Borrowing(fr) {
			return true
		}
	}
	return false
}

func (p *preemptionEvaluator) isActiveTrigger(rule v1beta2.PreemptionRule, wlInfo *workload.Info) (bool, error) {
	for _, condition := range wlInfo.Obj.Status.Conditions {
		if condition.Status == metav1.ConditionTrue && condition.Type == string(rule.Trigger) {
			if p.clock.Since(condition.LastTransitionTime.Time) < rule.MinTriggerRequiredDuration.Duration {
				return false, nil
			}

			selector, err := metav1.LabelSelectorAsSelector(rule.MatchingPreemptorWorkloads)
			if err != nil {
				return false, err
			}

			if !selector.Matches(labels.Set(wlInfo.Obj.Labels)) {
				return false, nil
			}

			return true, nil
		}
	}
	return false, nil
}

func (p *preemptionEvaluator) IsAnyTriggerActive(wlInfo *workload.Info) (bool, error) {
	for _, rule := range p.config.Spec.Rules {
		isActive, err := p.isActiveTrigger(rule, wlInfo)
		if err != nil {
			return false, err
		}

		if isActive {
			return true, nil
		}
	}
	return false, nil
}
