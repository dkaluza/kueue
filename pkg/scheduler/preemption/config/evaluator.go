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

	"github.com/go-logr/logr"
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

// TieredCandidates holds the candidates selected by the rules of a PreemptionConfig,
// grouped by the trigger of the rule that selected them. The candidates of a tier are
// only considered once the candidates of the preceding tiers, along with the candidates
// of the classical or Fair Sharing preemption, are not enough to admit the preemptor.
type TieredCandidates struct {
	// Always holds the candidates that are always considered.
	Always []*workload.Info
	// InsufficientQuota holds the candidates that are only considered if there is not
	// enough quota to admit the preemptor.
	InsufficientQuota []*workload.Info
	// QuotaFeasibleButTopologyBlocked holds the candidates that are only considered if
	// there is enough quota to admit the preemptor, but no topology assignment can be
	// found.
	QuotaFeasibleButTopologyBlocked []*workload.Info
}

// Empty returns true if no rule selected any candidate.
func (t TieredCandidates) Empty() bool {
	return len(t.Always) == 0 && t.ConditionalTiersEmpty()
}

// ConditionalTiersEmpty returns true if no rule of the tiers which are conditionally
// reached, so every tier but Always, selected any candidate.
func (t TieredCandidates) ConditionalTiersEmpty() bool {
	return len(t.InsufficientQuota) == 0 && len(t.QuotaFeasibleButTopologyBlocked) == 0
}

// Candidates returns the workloads selected as preemption candidates by the rules of
// the PreemptionConfig, grouped by the trigger of the rule that selected them.
// A workload selected by rules of several tiers is only returned for the first tier
// selecting it, as preempting it in an earlier tier makes it unavailable for the
// following ones.
func (p *preemptionEvaluator) Candidates(
	snapshot *schdcache.Snapshot,
	preemptor *workload.Info,
	flavorsNeedPreemption sets.Set[resources.FlavorResource],
) (TieredCandidates, error) {
	// A workload selected by rules of several tiers is only kept in the first tier
	// selecting it, so the tiers have to be evaluated in the order in which their
	// candidates are considered.
	seen := sets.New[workload.Reference]()
	forTier := func(trigger kueue.PreemptionRuleTrigger) ([]*workload.Info, error) {
		return p.candidatesForTier(snapshot, preemptor, flavorsNeedPreemption, trigger, seen)
	}
	var tieredCandidates TieredCandidates
	var err error
	if tieredCandidates.Always, err = forTier(kueue.Always); err != nil {
		return TieredCandidates{}, err
	}
	if tieredCandidates.InsufficientQuota, err = forTier(kueue.InsufficientQuota); err != nil {
		return TieredCandidates{}, err
	}
	if tieredCandidates.QuotaFeasibleButTopologyBlocked, err = forTier(kueue.QuotaFeasibleButTopologyBlocked); err != nil {
		return TieredCandidates{}, err
	}
	return tieredCandidates, nil
}

// candidatesForTier returns the candidates selected by the rules activated by the
// given trigger. The workloads already selected for a preceding tier, tracked in seen,
// are skipped, and the returned ones are added to it.
func (p *preemptionEvaluator) candidatesForTier(
	snapshot *schdcache.Snapshot,
	preemptor *workload.Info,
	flavorsNeedPreemption sets.Set[resources.FlavorResource],
	tier kueue.PreemptionRuleTrigger,
	seen sets.Set[workload.Reference],
) ([]*workload.Info, error) {
	var candidates []*workload.Info
	for _, rule := range p.config.Spec.Rules {
		if rule.ActivationPolicy.Trigger != tier {
			continue
		}
		matches, err := p.matchesPreemptor(rule, preemptor)
		if err != nil {
			return nil, err
		}
		if !matches {
			continue
		}

		for _, selector := range rule.Candidates {
			filter, rejectAll := filters.NewCandidateFilters(p.log, &selector, preemptor, snapshot)
			if rejectAll {
				continue
			}

			for _, targetCq := range snapshot.ClusterQueues() {
				if !matchesClusterQueue(&filter, targetCq) {
					continue
				}

				for _, wlInfo := range targetCq.Workloads {
					key := workload.Key(wlInfo.Obj)
					if !seen.Has(key) && matchesWorkload(&filter, wlInfo) && classical.WorkloadUsesResources(wlInfo, flavorsNeedPreemption) {
						seen.Insert(key)
						candidates = append(candidates, wlInfo)
					}
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

// matchesPreemptor returns whether the rule can be used for the given preemptor.
// Whether the tier of the rule is reached is decided by the preemption algorithm, as
// it depends on the candidates preempted for the preceding tiers.
func (p *preemptionEvaluator) matchesPreemptor(rule kueue.PreemptionRule, wlInfo *workload.Info) (bool, error) {
	selector, err := metav1.LabelSelectorAsSelector(&rule.MatchingPreemptorWorkloads)
	if err != nil {
		return false, err
	}

	return selector.Matches(labels.Set(wlInfo.Obj.Labels)), nil
}
