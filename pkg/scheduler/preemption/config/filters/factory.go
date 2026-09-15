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

package filters

import (
	"github.com/go-logr/logr"

	kueue "sigs.k8s.io/kueue/apis/kueue/v1beta2"
	schdcache "sigs.k8s.io/kueue/pkg/cache/scheduler"
	"sigs.k8s.io/kueue/pkg/workload"
)

// NewCandidateFilters compiles PreemptionCandidateSelector rules into CandidateFilters & RejectAll boolean (if preemptor doesn't pass).
// It returns (CandidateFilters{}, true) if the selector fails to compile and all the candidates should be rejected.
func NewCandidateFilters(
	log logr.Logger,
	selector *kueue.PreemptionCandidateSelector,
	preemptor *workload.Info,
	snapshot *schdcache.Snapshot,
) (CandidateFilters, bool) {
	if selector == nil {
		return CandidateFilters{}, false
	}

	cqRelationFilters, wlRelationFilters, ok := buildRelationFilters(log, selector.RelationRequirement, preemptor, snapshot)
	if !ok {
		return CandidateFilters{}, true
	}
	wlNumericFilters := buildNumericLabelFilters(log, selector.NumericLabels, preemptor)
	wlPriorityFilters := buildPriorityFilters(log, selector, preemptor)

	var wlFilters []WorkloadFilter
	wlFilters = append(wlFilters, wlRelationFilters...)
	wlFilters = append(wlFilters, wlNumericFilters...)
	wlFilters = append(wlFilters, wlPriorityFilters...)

	return CandidateFilters{
		CQFilters: cqRelationFilters,
		WLFilters: wlFilters,
	}, false
}

func buildRelationFilters(
	log logr.Logger,
	relation kueue.PreemptionRelationConstraint,
	preemptor *workload.Info,
	snapshot *schdcache.Snapshot,
) ([]ClusterQueueFilter, []WorkloadFilter, bool) {
	switch relation {
	case kueue.SameLocalQueue:
		// CQ Level: Prune all other ClusterQueues
		// WL Level: Narrow down workloads to those matching exactly same LocalQueue
		return []ClusterQueueFilter{NewSameClusterQueueFilter(preemptor.ClusterQueue)},
			[]WorkloadFilter{NewSameLocalQueueFilter(preemptor.Obj.Namespace, preemptor.Obj.Spec.QueueName)}, true

	case kueue.SameClusterQueue:
		return []ClusterQueueFilter{NewSameClusterQueueFilter(preemptor.ClusterQueue)}, nil, true

	case kueue.SameCohort:
		return []ClusterQueueFilter{NewSameCohortFilter(preemptor.ClusterQueue, snapshot)}, nil, true

	case kueue.SameCohortTree:
		return []ClusterQueueFilter{NewSameCohortTreeFilter(preemptor.ClusterQueue, snapshot)}, nil, true

	case kueue.AnyClusterQueue:
		return nil, nil, true

	default:
		log.V(3).Info("Unsupported or unhandled relation constraint evaluated; 0 candidates permitted", "relation", relation)
		return nil, nil, false
	}
}

func buildNumericLabelFilters(
	log logr.Logger,
	labels []kueue.NumericLabelConstraint,
	preemptor *workload.Info,
) []WorkloadFilter {
	if len(labels) == 0 {
		return nil
	}
	filters := make([]WorkloadFilter, 0, len(labels))
	for _, numConstraint := range labels {
		filters = append(filters, NewNumericLabelFilter(log, numConstraint, preemptor))
	}
	return filters
}

func buildPriorityFilters(
	log logr.Logger,
	selector *kueue.PreemptionCandidateSelector,
	preemptor *workload.Info,
) []WorkloadFilter {
	if selector == nil {
		return nil
	}
	var filters []WorkloadFilter
	if selector.RelativeWorkloadPriority != nil {
		filters = append(filters, NewRelativeWorkloadPriorityFilter(log, *selector.RelativeWorkloadPriority, preemptor))
	}
	return filters
}
