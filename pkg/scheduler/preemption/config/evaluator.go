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
	"maps"
	"slices"

	"github.com/go-logr/logr"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/utils/clock"
	"sigs.k8s.io/kueue/apis/kueue/v1beta2"
	schdcache "sigs.k8s.io/kueue/pkg/cache/scheduler"
	"sigs.k8s.io/kueue/pkg/scheduler/preemption/config/selectors"
	"sigs.k8s.io/kueue/pkg/workload"
)

type preemptionEvaluator struct {
	candidates []*workload.Info
}

func NewPreemptionEvaluator(candidates []*workload.Info) *preemptionEvaluator {
	return &preemptionEvaluator{
		candidates: candidates,
	}
}

func findCandidates(log logr.Logger, clock clock.Clock, config v1beta2.PreemptionConfig, preemptor *workload.Info, preemptorCq *schdcache.ClusterQueueSnapshot) ([]*workload.Info, error) {
	activeRules := []v1beta2.PreemptionRule{}
	for _, rule := range config.Spec.Rules {
		isActive, err := isActiveTrigger(clock, rule, preemptor)
		if err != nil {
			return nil, err
		}

		if isActive {
			activeRules = append(activeRules, rule)
		}
	}

	candidates := []*workload.Info{}
	// TODO: implement once more selectors available
	selectors := []selectors.CandidateSelector{}
	for _, targetCq := range preemptorCq.Parent().Root().SubtreeClusterQueues() {
		// TODO: too many potential allocations, maybe change interface?
		targetCandidates := slices.Collect(maps.Values(targetCq.Workloads))
		for _, selector := range selectors {
			targetCandidates = selector.Filter(log, preemptor, targetCandidates)
		}
		candidates = append(candidates, targetCandidates...)
	}

	return candidates, nil
}

func isActiveTrigger(clock clock.Clock, rule v1beta2.PreemptionRule, wlInfo *workload.Info) (bool, error) {
	for _, condition := range wlInfo.Obj.Status.Conditions {
		if condition.Status == metav1.ConditionTrue &&
			condition.Type == string(rule.Trigger) &&
			int(clock.Since(condition.LastTransitionTime.Time).Seconds()) >= rule.MinTriggerRequiredDurationSeconds {

			selector, err := metav1.LabelSelectorAsSelector(&rule.MatchingPreemptorWorkloads)
			if err != nil {
				return false, err
			}

			if selector.Matches(labels.Set(wlInfo.Obj.Labels)) {
				return true, nil
			}
		}
	}
	return false, nil
}

// TODO: make into a method later
func IsAnyTriggerActive(clock clock.Clock, rules []v1beta2.PreemptionRule, wlInfo *workload.Info) (bool, error) {
	for _, rule := range rules {
		isActive, err := isActiveTrigger(clock, rule, wlInfo)
		if err != nil {
			return false, err
		}

		if isActive {
			return true, nil
		}
	}
	return false, nil
}

func (p *preemptionEvaluator) Iter() iter.Seq[*workload.Info] {
	return func(yield func(*workload.Info) bool) {
		for _, candidate := range p.candidates {
			if !yield(candidate) {
				return
			}
		}
	}
}
