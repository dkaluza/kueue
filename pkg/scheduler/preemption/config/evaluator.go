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
	"cmp"
	"iter"
	"slices"

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/utils/clock"
	"sigs.k8s.io/kueue/apis/kueue/v1beta2"
	schdcache "sigs.k8s.io/kueue/pkg/cache/scheduler"
	"sigs.k8s.io/kueue/pkg/resources"
	"sigs.k8s.io/kueue/pkg/scheduler/preemption/classical"
	"sigs.k8s.io/kueue/pkg/scheduler/preemption/config/filters"
	"sigs.k8s.io/kueue/pkg/workload"
)

type CandidateFilters func(log logr.Logger, selector *v1beta2.PreemptionCandidateSelector, preemptor *workload.Info, snapshot *schdcache.Snapshot) filters.CandidateFilters

type preemptionEvaluator struct {
	log              logr.Logger
	clock            clock.Clock
	config           v1beta2.PreemptionConfig
	candidateFilters CandidateFilters
}

func NewPreemptionEvaluator(log logr.Logger, clock clock.Clock, config v1beta2.PreemptionConfig, candidateFilters CandidateFilters) *preemptionEvaluator {
	return &preemptionEvaluator{
		log:              log,
		clock:            clock,
		config:           config,
		candidateFilters: candidateFilters,
	}
}

func (p *preemptionEvaluator) Iter(snapshot *schdcache.Snapshot, preemptor *workload.Info, flavorsNeedPreemption sets.Set[resources.FlavorResource]) (iter.Seq[*workload.Info], error) {
	candidates, err := p.findCandidates(snapshot, preemptor, flavorsNeedPreemption)
	if err != nil {
		return nil, err
	}

	slices.SortFunc(candidates, func(a, b *workload.Info) int {
		return cmp.Compare(a.Obj.UID, b.Obj.UID)
	})

	return func(yield func(*workload.Info) bool) {
		for _, candidate := range candidates {
			if !yield(candidate) {
				return
			}
		}
	}, nil
}

func (p *preemptionEvaluator) findCandidates(snapshot *schdcache.Snapshot, preemptor *workload.Info, flavorsNeedPreemption sets.Set[resources.FlavorResource]) ([]*workload.Info, error) {
	candidateFilters := []filters.CandidateFilters{}
	for _, rule := range p.config.Spec.Rules {
		isActive, err := p.isActiveTrigger(rule, preemptor)
		if err != nil {
			return nil, err
		}

		if !isActive {
			continue
		}

		for _, selector := range rule.Candidates {
			candidateFilters = append(candidateFilters, p.candidateFilters(p.log, &selector, preemptor, snapshot))
		}
	}

	if len(candidateFilters) == 0 {
		return nil, nil
	}

	candidateSet := sets.New[*workload.Info]()
	for _, targetCq := range snapshot.ClusterQueues() {
		for _, filter := range candidateFilters {
			if !matchesClusterQueue(&filter, targetCq) {
				continue
			}

			for _, wlInfo := range targetCq.Workloads {
				if matchesWorkload(&filter, wlInfo) && classical.WorkloadUsesResources(wlInfo, flavorsNeedPreemption) {
					candidateSet.Insert(wlInfo)
				}
			}
		}
	}

	return candidateSet.UnsortedList(), nil
}

func matchesClusterQueue(filter *filters.CandidateFilters, cq *schdcache.ClusterQueueSnapshot) bool {
	for _, cqFilter := range filter.CQFilters {
		if !cqFilter.Matches(cq) {
			return false
		}
	}
	return true
}

func matchesWorkload(filter *filters.CandidateFilters, wl *workload.Info) bool {
	for _, wlFilter := range filter.WLFilters {
		if !wlFilter.Matches(wl) {
			return false
		}
	}
	return true
}

func (p *preemptionEvaluator) isActiveTrigger(rule v1beta2.PreemptionRule, wlInfo *workload.Info) (bool, error) {
	condition := meta.FindStatusCondition(wlInfo.Obj.Status.Conditions, string(rule.Trigger))
	if condition == nil || condition.Status == metav1.ConditionFalse {
		return false, nil
	}

	if p.clock.Since(condition.LastTransitionTime.Time) < rule.MinTriggerRequiredDuration.Duration {
		return false, nil
	}

	selector, err := metav1.LabelSelectorAsSelector(&rule.MatchingPreemptorWorkloads)
	if err != nil {
		return false, err
	}

	return selector.Matches(labels.Set(wlInfo.Obj.Labels)), nil
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
