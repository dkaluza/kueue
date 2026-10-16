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
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/go-logr/logr"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kueuealpha "sigs.k8s.io/kueue/apis/kueue/v1alpha1"
	kueue "sigs.k8s.io/kueue/apis/kueue/v1beta2"
	schdcache "sigs.k8s.io/kueue/pkg/cache/scheduler"
	"sigs.k8s.io/kueue/pkg/features"
	"sigs.k8s.io/kueue/pkg/resources"
	"sigs.k8s.io/kueue/pkg/scheduler/preemption/classical"
	"sigs.k8s.io/kueue/pkg/scheduler/preemption/config/filters"
	"sigs.k8s.io/kueue/pkg/scheduler/preemption/limits"
	"sigs.k8s.io/kueue/pkg/scheduler/preemption/policy"
	"sigs.k8s.io/kueue/pkg/util/expectations"
	"sigs.k8s.io/kueue/pkg/workload"
	workloadevict "sigs.k8s.io/kueue/pkg/workload/evict"
)

// PreemptionEvaluator selects the preemption candidates according to the configuration.
//
// FindCandidates is its entry point, yielding the candidates selected by the rules
// of the PreemptionConfig, while HasRules lets callers check upfront whether it may
// yield any candidate at all. A nil PreemptionEvaluator holds no rule, and therefore
// never yields any candidate.
type PreemptionEvaluator struct {
	ctx       context.Context
	log       logr.Logger
	config    kueuealpha.PreemptionConfig
	preemptor *workload.Info
	// candidatesOrdering orders the candidates of a trigger from the most to the
	// least preferred one.
	candidatesOrdering func(a, b *workload.Info) int

	// The fields below are only set when the ConfigurablePreemptionLimits feature
	// is enabled, to enforce the PreemptionLimits applying to the PreemptionConfig.
	preemptionLimits        []kueuealpha.PreemptionLimit
	preemptionLimitMatchers []limits.PreemptionLimitMatcher
	preemptionLimitTracker  *limits.PreemptionLimitTracker
	preemptionExpectations  *expectations.Store
	// now is the time of the preemption attempt, ending the windows of the
	// PreemptionLimits.
	now time.Time
}

// NewPreemptionEvaluator returns the PreemptionEvaluator of the given preemptor for the
// given PreemptionConfig, which yields the candidates of each trigger in the order defined
// by candidatesOrdering, within the budget of the PreemptionLimits of the given matchers.
// The budget counts the preemptions recorded by preemptionLimitTracker within the windows
// of the PreemptionLimits ending at now.
func NewPreemptionEvaluator(
	ctx context.Context,
	log logr.Logger,
	config kueuealpha.PreemptionConfig,
	preemptionLimits []kueuealpha.PreemptionLimit,
	preemptionLimitMatchers []limits.PreemptionLimitMatcher,
	preemptionLimitTracker *limits.PreemptionLimitTracker,
	preemptionExpectations *expectations.Store,
	now time.Time,
	preemptor *workload.Info,
	candidatesOrdering func(a, b *workload.Info) int,
) *PreemptionEvaluator {
	return &PreemptionEvaluator{
		ctx:                     ctx,
		log:                     log,
		config:                  config,
		preemptor:               preemptor,
		candidatesOrdering:      candidatesOrdering,
		preemptionLimits:        preemptionLimits,
		preemptionLimitMatchers: preemptionLimitMatchers,
		preemptionLimitTracker:  preemptionLimitTracker,
		preemptionExpectations:  preemptionExpectations,
		now:                     now,
	}
}

// NewEvaluatorForPreemptionConfig returns the PreemptionEvaluator of the given preemptor for
// the PreemptionConfig with the given name. When the ConfigurablePreemptionLimits feature is
// enabled, the evaluator only yields the candidates within the budget of the PreemptionLimits
// applying to the PreemptionConfig.
// It returns nil if the PreemptionConfig cannot be read, or, when the ConfigurablePreemptionLimits
// feature is enabled, if the PreemptionLimits cannot be listed or any of them has an invalid selector.
func NewEvaluatorForPreemptionConfig(
	ctx context.Context,
	log logr.Logger,
	cl client.Client,
	preemptionLimitTracker *limits.PreemptionLimitTracker,
	preemptionExpectations *expectations.Store,
	now time.Time,
	preemptionConfigName string,
	preemptor *workload.Info,
	candidatesOrdering func(a, b *workload.Info) int,
) *PreemptionEvaluator {
	preemptionConfig := &kueuealpha.PreemptionConfig{}
	if err := cl.Get(ctx, client.ObjectKey{Name: preemptionConfigName}, preemptionConfig); err != nil {
		log.Error(err, "Failed to get PreemptionConfig", "preemptionConfigName", preemptionConfigName)
		return nil
	}
	var (
		preemptionLimits        []kueuealpha.PreemptionLimit
		preemptionLimitMatchers []limits.PreemptionLimitMatcher
	)
	if features.Enabled(features.ConfigurablePreemptionLimits) {
		// Fail fast: without valid PreemptionLimits, the preemptions issued in this
		// attempt could exceed them, and would not be counted against them.
		preemptionLimitList := &kueuealpha.PreemptionLimitList{}
		if err := cl.List(ctx, preemptionLimitList); err != nil {
			log.Error(err, "Failed to list PreemptionLimits, skipping configurable preemptions", "preemptionConfigName", preemptionConfigName)
			return nil
		}
		var err error
		if preemptionLimitMatchers, err = limits.NewPreemptionLimitMatchers(preemptionLimitList.Items, preemptionConfig); err != nil {
			log.Error(err, "Invalid PreemptionLimit, skipping configurable preemptions", "preemptionConfigName", preemptionConfigName)
			return nil
		}
		preemptionLimits = preemptionLimitList.Items
	}
	return NewPreemptionEvaluator(ctx, log, *preemptionConfig, preemptionLimits, preemptionLimitMatchers,
		preemptionLimitTracker, preemptionExpectations, now, preemptor, candidatesOrdering)
}

// HasRules returns whether the PreemptionConfig holds any rule at all. It only
// inspects the configuration, without evaluating any candidate, so callers can
// cheaply tell whether FindCandidates may yield any candidate before running it.
func (p *PreemptionEvaluator) HasRules() bool {
	return p != nil &&
		p.hasRulesFor(kueuealpha.Always, kueuealpha.InsufficientQuota, kueuealpha.QuotaFeasibleAndInsufficientTopology)
}

// FindCandidates evaluates candidates across applicable PreemptionConfig
// triggers and yields them.
// Returns (interrupted = true) if at any point the yield method returns false.
// An evaluation error is logged and stops evaluation of subsequent triggers.
//
// Triggers are applied as a fallback in the order the API defines them: the Always
// trigger first, as a baseline, then InsufficientQuota while the quota is not sufficient,
// and finally QuotaFeasibleAndInsufficientTopology once the quota is sufficient but no
// topology assignment can be found.
//
// Because candidates are removed from the snapshot as they are evaluated, subsequent
// fit checks observe the updated snapshot state, and the evaluator only returns
// candidates still admitted in the snapshot.
//
// Candidates exceeding the budget of a PreemptionLimit are skipped. The budget is
// shared by all the triggers of a call, and starts afresh at each call.
func (p *PreemptionEvaluator) FindCandidates(
	snapshot *schdcache.Snapshot,
	frsNeedPreemption sets.Set[resources.FlavorResource],
	workloadQuotaFits func() bool,
	yield func(*policy.Target) bool,
) (interrupted bool) {
	yield = policy.YieldFromSnapshot(snapshot, yield)

	if !p.HasRules() {
		return
	}

	budget := limits.NewPreemptionLimitBudget(p.preemptionLimitTracker, p.now, p.preemptionLimits)
	candidates, err := p.orderedCandidates(snapshot, frsNeedPreemption, kueuealpha.Always)
	if err != nil {
		return
	}
	if p.iterateOverCandidates(snapshot, candidates, budget, yield) {
		return true
	}

	if !p.hasConditionalRules() {
		// We iterated over all initial candidates.
		// Without conditional rules no more candidates can be yielded.
		return
	}

	if !workloadQuotaFits() {
		candidates, err = p.orderedCandidates(snapshot, frsNeedPreemption, kueuealpha.InsufficientQuota)
		if err != nil {
			return
		}
		if p.iterateOverCandidates(snapshot, candidates, budget, yield) {
			return true
		}
	}

	// The topology trigger requires a feasible quota, so it is only applied once
	// the quota fits while the workload still doesn't fit (meaning topology is
	// what keeps the workload out).
	if workloadQuotaFits() {
		candidates, err = p.orderedCandidates(snapshot, frsNeedPreemption, kueuealpha.QuotaFeasibleAndInsufficientTopology)
		if err != nil {
			return
		}
		if p.iterateOverCandidates(snapshot, candidates, budget, yield) {
			return true
		}
	}

	return
}

func (p *PreemptionEvaluator) iterateOverCandidates(
	snapshot *schdcache.Snapshot,
	candidates []*configurableCandidate,
	budget *limits.PreemptionLimitBudget,
	yield func(*policy.Target) bool,
) (interrupted bool) {
	preemptorCQ := snapshot.ClusterQueue(p.preemptor.ClusterQueue)
	for _, candidate := range candidates {
		targetCQ := snapshot.ClusterQueue(candidate.WlInfo.ClusterQueue)
		consumedLimits, withinLimits := p.consumeLimits(budget, preemptorCQ, targetCQ, candidate.WlInfo)
		if !withinLimits {
			continue
		}
		if !yield(&policy.Target{
			WorkloadInfo: candidate.WlInfo,
			Reason:       kueue.ConfigurablePreemptionReason,
			WorkloadCq:   targetCQ,
			ConfigurablePreemptionReasonData: &policy.ConfigurablePreemptionReasonData{
				ConfigName:                candidate.ConfigName,
				RuleNameToSelectorIndexes: candidate.RuleNameToSelectorIndexes,
				ConsumedLimits:            consumedLimits,
			},
		}) {
			return true
		}
	}
	return
}

// consumeLimits returns the PreemptionLimits consumed by preempting target, and
// takes them from budget. It returns false, taking nothing, if any of them has no
// budget left.
func (p *PreemptionEvaluator) consumeLimits(
	budget *limits.PreemptionLimitBudget,
	preemptorCQ, targetCQ *schdcache.ClusterQueueSnapshot,
	target *workload.Info,
) (policy.ConsumedLimits, bool) {
	consumedLimits := limits.Resolve(p.preemptionLimitMatchers, preemptorCQ, targetCQ, target)
	if len(consumedLimits) == 0 || p.preemptionInProgress(target) {
		return nil, true
	}
	if limit, ok := budget.TryConsume(consumedLimits); !ok {
		p.log.V(5).Info("Skipping preemption candidate exceeding a PreemptionLimit",
			"candidate", klog.KObj(target.Obj), "preemptionLimit", limit, "scopeValue", consumedLimits[limit])
		return nil, false
	}
	return consumedLimits, true
}

// preemptionInProgress returns whether the preemption of target is already in
// progress: target is evicted, or its eviction is not observed yet. These are the
// cases where IssuePreemptions neither evicts target nor records its preemption, so
// target uses no budget and consumes no PreemptionLimit. A preemptor waiting for its
// targets re-selects them every cycle until their quota is released.
func (p *PreemptionEvaluator) preemptionInProgress(target *workload.Info) bool {
	return workloadevict.IsEvicted(target.Obj) ||
		!p.preemptionExpectations.Satisfied(p.log, client.ObjectKeyFromObject(target.Obj))
}

// hasRulesFor returns whether any rule of the PreemptionConfig is activated by one of
// the given triggers. It only inspects the configuration, never the snapshot, and is
// therefore cheap enough to guard the candidate evaluation.
func (p *PreemptionEvaluator) hasRulesFor(triggers ...kueuealpha.PreemptionConfigActivationTrigger) bool {
	for _, rule := range p.config.Spec.Rules {
		if slices.Contains(triggers, rule.ActivationPolicy.Trigger) {
			return true
		}
	}
	return false
}

// hasConditionalRules returns whether the PreemptionConfig holds any rule of a
// trigger which is only reached once the preceding ones are not enough. It inspects
// the configuration only, and therefore lets FindCandidates skip the fit checks
// guarding the evaluation of those triggers.
func (p *PreemptionEvaluator) hasConditionalRules() bool {
	return p.hasRulesFor(kueuealpha.InsufficientQuota, kueuealpha.QuotaFeasibleAndInsufficientTopology)
}

// configurableCandidate represents a workload selected for configurable preemption
// together with the configuration rules and selectors that selected it.
type configurableCandidate struct {
	WlInfo                    *workload.Info
	ConfigName                policy.PreemptionConfigReference
	RuleNameToSelectorIndexes map[policy.PreemptionConfigRuleReference][]int
}

// orderedCandidates returns the candidates selected by the rules of the
// PreemptionConfig activated by the given trigger, ordered from the most to the least
// preferred one.
// Only the candidates still admitted in the snapshot are returned, so a trigger
// evaluated after some workloads have been preempted never returns those again.
// Returns an error if a matching rule cannot be evaluated.
func (p *PreemptionEvaluator) orderedCandidates(
	snapshot *schdcache.Snapshot,
	frsNeedPreemption sets.Set[resources.FlavorResource],
	trigger kueuealpha.PreemptionConfigActivationTrigger,
) ([]*configurableCandidate, error) {
	candidates, err := p.candidatesFor(snapshot, frsNeedPreemption, trigger)
	if err != nil {
		p.log.Error(err, "Failed to get candidates for preemption", "trigger", trigger)
		return nil, err
	}
	slices.SortFunc(candidates, func(a, b *configurableCandidate) int {
		return p.candidatesOrdering(a.WlInfo, b.WlInfo)
	})
	return candidates, nil
}

// candidatesFor returns the workloads selected as preemption candidates by the rules of the
// PreemptionConfig activated by the given trigger, deduplicated across the rules and
// selectors of the trigger.
func (p *PreemptionEvaluator) candidatesFor(
	snapshot *schdcache.Snapshot,
	flavorsNeedPreemption sets.Set[resources.FlavorResource],
	trigger kueuealpha.PreemptionConfigActivationTrigger,
) ([]*configurableCandidate, error) {
	var (
		candidates []*configurableCandidate
		errs       []error
	)
	// Several rules, or several selectors of a rule, can select the same workload.
	// Therefore, we need to keep track of the UIDs of the selected workloads
	// to avoid duplicates. Additionally map's value is used as index of already recorded candidate
	// for effective look up to add another selector's index in case of another match.
	seen := map[types.UID]int{}
	for _, rule := range p.config.Spec.Rules {
		if rule.ActivationPolicy.Trigger != trigger {
			continue
		}
		matches, err := workloadMatchesSelector(rule.PreemptorSelector, p.preemptor)
		if err != nil {
			errs = append(errs, fmt.Errorf("preemptionConfig %q rule %q: %w", p.config.Name, rule.Name, err))
			continue
		}
		if !matches {
			continue
		}

		for selectorIndex, selector := range rule.CandidateSelectors {
			filter, buildErrs := filters.NewCandidateFilters(p.log, &selector, p.preemptor, snapshot)
			if len(buildErrs) > 0 {
				for _, bErr := range buildErrs {
					errs = append(errs, fmt.Errorf("preemptionConfig %q rule %q candidateSelectors[%d]: %w", p.config.Name, rule.Name, selectorIndex, bErr))
				}
				continue
			}

			ruleReference := policy.PreemptionConfigRuleReference(rule.Name)
			p.addMatchingCandidates(&filter, snapshot, flavorsNeedPreemption, ruleReference, seen, &candidates, selectorIndex)
		}
	}

	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}

	return candidates, nil
}

func (p *PreemptionEvaluator) addMatchingCandidates(
	filter *filters.CandidateFilters,
	snapshot *schdcache.Snapshot,
	flavorsNeedPreemption sets.Set[resources.FlavorResource],
	ruleReference policy.PreemptionConfigRuleReference,
	seen map[types.UID]int,
	candidates *[]*configurableCandidate,
	selectorIndex int,
) {
	for _, targetCq := range snapshot.ClusterQueues() {
		if !matchesClusterQueue(filter, targetCq) {
			continue
		}

		for _, wlInfo := range targetCq.Workloads {
			if matchesWorkload(filter, wlInfo) && classical.WorkloadUsesResources(wlInfo, flavorsNeedPreemption) {
				candidate := p.ensureCandidate(seen, candidates, wlInfo)

				indexes := candidate.RuleNameToSelectorIndexes[ruleReference]
				candidate.RuleNameToSelectorIndexes[ruleReference] = append(indexes, selectorIndex)
			}
		}
	}
}

func (p *PreemptionEvaluator) ensureCandidate(
	seen map[types.UID]int,
	candidates *[]*configurableCandidate,
	wlInfo *workload.Info,
) *configurableCandidate {
	if existingIndex, found := seen[wlInfo.Obj.UID]; found {
		return (*candidates)[existingIndex]
	}

	seen[wlInfo.Obj.UID] = len(*candidates)
	candidate := &configurableCandidate{
		WlInfo:                    wlInfo,
		ConfigName:                policy.PreemptionConfigReference(p.config.Name),
		RuleNameToSelectorIndexes: map[policy.PreemptionConfigRuleReference][]int{},
	}

	*candidates = append(*candidates, candidate)
	return candidate
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

// workloadMatchesSelector returns whether the labels of the workload match the
// selector. A nil selector accepts every workload, which differs from
// LabelSelectorAsSelector(nil), matching none.
func workloadMatchesSelector(selector *metav1.LabelSelector, wlInfo *workload.Info) (bool, *filters.FilterBuildError) {
	if selector == nil {
		return true, nil
	}
	labelSelector, err := metav1.LabelSelectorAsSelector(selector)
	if err != nil {
		return false, &filters.FilterBuildError{
			Filter: filters.FilterPreemptorSelector,
			Reason: filters.ReasonInvalidSelector,
			Err:    err,
		}
	}

	return labelSelector.Matches(labels.Set(wlInfo.Obj.Labels)), nil
}
