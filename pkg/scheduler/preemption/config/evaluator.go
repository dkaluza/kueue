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

	"sigs.k8s.io/kueue/apis/kueue/v1beta2"
	schdcache "sigs.k8s.io/kueue/pkg/cache/scheduler"
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

func findCandidates(config v1beta2.PreemptionConfig, preemptor *workload.Info, preemptorCq *schdcache.ClusterQueueSnapshot) []*workload.Info {
	activeRules := []v1beta2.PreemptionRule{}
	for _, rule := range config.Spec.Rules {
		if isActiveTrigger(rule, preemptor) {
			activeRules = append(activeRules, rule)
		}
	}

	candidates := []*workload.Info{}
	for _, targetCq := range preemptorCq.Parent().Root().SubtreeClusterQueues() {
		for _, wlInfo := range targetCq.Workloads {
			if isActiveCandidate(activeRules, preemptorCq, preemptor, targetCq, wlInfo) {
				candidates = append(candidates, wlInfo)
			}
		}
	}
	return candidates
}

func isActiveTrigger(rule v1beta2.PreemptionRule, _ *workload.Info) bool {
	// Will be implemented later
	return true
}

func isActiveCandidate(_ []v1beta2.PreemptionRule, _ *schdcache.ClusterQueueSnapshot, _ *workload.Info, _ *schdcache.ClusterQueueSnapshot, _ *workload.Info) bool {
	// Will be implemented later
	return true
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
