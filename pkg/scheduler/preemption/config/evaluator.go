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
	"maps"
	"slices"

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/utils/clock"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kueue "sigs.k8s.io/kueue/apis/kueue/v1beta2"
	schdcache "sigs.k8s.io/kueue/pkg/cache/scheduler"
	"sigs.k8s.io/kueue/pkg/resources"
	"sigs.k8s.io/kueue/pkg/scheduler/preemption/classical"
	"sigs.k8s.io/kueue/pkg/scheduler/preemption/config/filters"
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

func (p *preemptionEvaluator) Candidates(
	snapshot *schdcache.Snapshot,
	preemptor *workload.Info,
	flavorsNeedPreemption sets.Set[resources.FlavorResource],
) ([]*workload.Info, error) {
	// Sorting CQ names ensures deterministic candidate collection across scheduling cycles.
	cqNames := slices.Sorted(maps.Keys(snapshot.ClusterQueues()))

	var candidates []*workload.Info
	seen := sets.New[types.UID]()
	for _, rule := range p.config.Spec.Rules {
		isActive, err := p.isActiveTrigger(rule, preemptor)
		if err != nil {
			return nil, err
		}

		if !isActive {
			continue
		}

		for _, selector := range rule.Candidates {
			filter, rejectAll := filters.NewCandidateFilters(p.ctx, p.log, &selector, preemptor, snapshot, p.reader)
			if rejectAll {
				continue
			}

			for _, cqName := range cqNames {
				targetCq := snapshot.ClusterQueue(cqName)
				if !matchesClusterQueue(&filter, targetCq) {
					continue
				}

				// Sorting the workload keys ensures deterministic candidate collection
				// across scheduling cycles. It carries no preemption ordering semantics.
				for _, wlKey := range slices.Sorted(maps.Keys(targetCq.Workloads)) {
					wlInfo := targetCq.Workloads[wlKey]
					if !matchesWorkload(&filter, wlInfo) || !classical.WorkloadUsesResources(wlInfo, flavorsNeedPreemption) {
						continue
					}
					if seen.Has(wlInfo.Obj.UID) {
						continue
					}
					seen.Insert(wlInfo.Obj.UID)
					candidates = append(candidates, wlInfo)
				}
			}
		}
	}

	return candidates, nil
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
