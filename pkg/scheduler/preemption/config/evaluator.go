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
	"context"
	"iter"
	"maps"
	"slices"

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/utils/clock"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kueue "sigs.k8s.io/kueue/apis/kueue/v1beta2"
	schdcache "sigs.k8s.io/kueue/pkg/cache/scheduler"
	"sigs.k8s.io/kueue/pkg/resources"
	"sigs.k8s.io/kueue/pkg/scheduler/preemption/classical"
	"sigs.k8s.io/kueue/pkg/scheduler/preemption/config/filters"
	"sigs.k8s.io/kueue/pkg/scheduler/preemption/config/ordering"
	"sigs.k8s.io/kueue/pkg/workload"
)

type preemptionEvaluator struct {
	ctx    context.Context
	log    logr.Logger
	clock  clock.Clock
	config kueue.PreemptionConfig
	reader client.Reader
}

func NewPreemptionEvaluator(
	ctx context.Context,
	log logr.Logger,
	clock clock.Clock,
	config kueue.PreemptionConfig,
	reader client.Reader,
) *preemptionEvaluator {
	return &preemptionEvaluator{
		ctx:    ctx,
		log:    log,
		clock:  clock,
		config: config,
		reader: reader,
	}
}

func (p *preemptionEvaluator) Iter(snapshot *schdcache.Snapshot, preemptor *workload.Info, flavorsNeedPreemption sets.Set[resources.FlavorResource]) (iter.Seq[*workload.Info], error) {
	queues, cmpFunc, err := p.buildCandidateQueues(snapshot, preemptor, flavorsNeedPreemption)
	if err != nil {
		return nil, err
	}
	if len(queues) == 0 {
		return func(yield func(*workload.Info) bool) {}, nil
	}

	iterator := ordering.NewMultiQueueCandidateIterator(queues, cmpFunc)
	return iterator.Seq(), nil
}

func (p *preemptionEvaluator) buildCandidateQueues(
	snapshot *schdcache.Snapshot,
	preemptor *workload.Info,
	flavorsNeedPreemption sets.Set[resources.FlavorResource],
) ([]*ordering.CandidateQueue, func(a, b *workload.Info) int, error) {
	cmpFunc := ordering.NewComparator(p.log, p.config.Spec.Ordering, preemptor, snapshot, p.clock.Now())
	// Sorting CQ names ensures deterministic queue instantiation across scheduling cycles.
	cqNames := slices.Sorted(maps.Keys(snapshot.ClusterQueues()))

	var queues []*ordering.CandidateQueue
	for _, rule := range p.config.Spec.Rules {
		isActive, err := p.isActiveTrigger(rule, preemptor)
		if err != nil {
			return nil, nil, err
		}

		if !isActive {
			continue
		}

		for selectorIdx, selector := range rule.Candidates {
			filter, rejectAll := filters.NewCandidateFilters(p.ctx, p.log, &selector, preemptor, snapshot, p.reader)
			if rejectAll {
				continue
			}

			for _, cqName := range cqNames {
				targetCq := snapshot.ClusterQueue(cqName)
				if !matchesClusterQueue(&filter, targetCq) {
					continue
				}

				var candidates []*workload.Info
				for _, wlInfo := range targetCq.Workloads {
					if matchesWorkload(&filter, wlInfo) && classical.WorkloadUsesResources(wlInfo, flavorsNeedPreemption) {
						candidates = append(candidates, wlInfo)
					}
				}

				if len(candidates) > 0 {
					q := ordering.NewCandidateQueue(rule.Name, selectorIdx, targetCq.Name, candidates, cmpFunc)
					queues = append(queues, q)
				}
			}
		}
	}

	return queues, cmpFunc, nil
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

func (p *preemptionEvaluator) isActiveTrigger(rule kueue.PreemptionRule, wlInfo *workload.Info) (bool, error) {
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
