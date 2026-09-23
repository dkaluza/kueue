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

package preemption

import (
	"slices"

	"sigs.k8s.io/controller-runtime/pkg/client"

	kueue "sigs.k8s.io/kueue/apis/kueue/v1beta2"
	"sigs.k8s.io/kueue/pkg/features"
	preemptioncommon "sigs.k8s.io/kueue/pkg/scheduler/preemption/common"
	configurable "sigs.k8s.io/kueue/pkg/scheduler/preemption/config"
	"sigs.k8s.io/kueue/pkg/workload"
)

// This file provides helpers for evaluating configurable preemption rules:
// resolving the PreemptionConfig, checking which triggers are configured, and
// evaluating candidate preemption across triggers.
//
// Triggers are applied as a fallback in the order the API defines them: the Always
// trigger first, as a baseline, then InsufficientQuota while the quota is not sufficient,
// and finally QuotaFeasibleAndInsufficientTopology once the quota is sufficient but no
// topology assignment can be found.
//
// TODO(#15893): delete this file once ConfigurablePreemption becomes an algorithm of its
// own, mutually exclusive with the classical and Fair Sharing preemption.

// newConfigurableEvaluator returns the evaluator for the PreemptionConfig referenced by
// the preemptor's ClusterQueue, or nil if the ConfigurablePreemption feature is
// disabled, the ClusterQueue references no PreemptionConfig, or it cannot be read.
func newConfigurableEvaluator(cl client.Client, preemptionCtx *preemptionCtx) *configurable.PreemptionEvaluator {
	if !features.Enabled(features.ConfigurablePreemption) || preemptionCtx.preemptorCQ.PreemptionAnnotation == nil {
		return nil
	}
	preemptionConfig := &kueue.PreemptionConfig{}
	preemptionConfigName := *preemptionCtx.preemptorCQ.PreemptionAnnotation
	if err := cl.Get(preemptionCtx.ctx, client.ObjectKey{Name: preemptionConfigName}, preemptionConfig); err != nil {
		preemptionCtx.log.Error(err, "Failed to get PreemptionConfig", "preemptionConfigName", preemptionConfigName)
		return nil
	}
	return configurable.NewPreemptionEvaluator(preemptionCtx.ctx, preemptionCtx.log, preemptionCtx.clock, *preemptionConfig, cl)
}

// configurableCandidates returns the candidates selected by the rules of the
// PreemptionConfig activated by the given trigger, ordered from the most to the least
// preferred one, or no candidate if the ClusterQueue uses no PreemptionConfig.
// Only the candidates still admitted in the snapshot are returned, so a trigger
// evaluated after some workloads have been preempted never returns those again.
func configurableCandidates(preemptionCtx *preemptionCtx, candidatesOrdering func(a, b *workload.Info) int, trigger kueue.PreemptionConfigActivationTrigger) []*workload.Info {
	if preemptionCtx.configurableEvaluator == nil {
		return nil
	}
	candidates, err := preemptionCtx.configurableEvaluator.Candidates(preemptionCtx.snapshot, &preemptionCtx.preemptor, preemptionCtx.frsNeedPreemption, trigger)
	if err != nil {
		preemptionCtx.log.Error(err, "Failed to get candidates for preemption", "trigger", trigger)
		return nil
	}
	slices.SortFunc(candidates, candidatesOrdering)
	return candidates
}

// hasConfigurableRules returns whether the PreemptionConfig holds any rule at all. It
// inspects the configuration only, which lets the callers keep going without evaluating
// the candidates of a trigger before the phase actually reached it.
func hasConfigurableRules(preemptionCtx *preemptionCtx) bool {
	return preemptionCtx.configurableEvaluator != nil &&
		preemptionCtx.configurableEvaluator.HasRulesFor(kueue.Always, kueue.InsufficientQuota, kueue.QuotaFeasibleAndInsufficientTopology)
}

// hasConditionalConfigurableRules returns whether the PreemptionConfig holds any rule of
// a trigger which is only reached once the preceding ones are not enough. It inspects
// the configuration only, and therefore lets the callers skip the fit checks guarding
// the evaluation of those triggers.
func hasConditionalConfigurableRules(preemptionCtx *preemptionCtx) bool {
	return preemptionCtx.configurableEvaluator != nil &&
		preemptionCtx.configurableEvaluator.HasRulesFor(kueue.InsufficientQuota, kueue.QuotaFeasibleAndInsufficientTopology)
}

// mergeConfigurableCandidatesWithFitCheck evaluates candidates across applicable
// PreemptionConfig triggers and returns (fits, configurableTargets).
//
// Because candidates are removed from the snapshot as they are evaluated, subsequent
// fit checks observe the updated snapshot state, and the evaluator only returns
// candidates still admitted in the snapshot.
func mergeConfigurableCandidatesWithFitCheck(preemptionCtx *preemptionCtx, candidatesOrdering func(a, b *workload.Info) int, allowBorrowing bool) (bool, []*Target) {
	if workloadFits(preemptionCtx, allowBorrowing) {
		return true, nil
	}
	if !hasConfigurableRules(preemptionCtx) {
		return false, nil
	}
	fits, targets := simulateConfigurableCandidatesPreemption(preemptionCtx, candidatesOrdering, kueue.Always, allowBorrowing)
	if !fits && hasConditionalConfigurableRules(preemptionCtx) {
		if !workloadQuotaFits(preemptionCtx, allowBorrowing) {
			var moreTargets []*Target
			fits, moreTargets = simulateConfigurableCandidatesPreemption(preemptionCtx, candidatesOrdering, kueue.InsufficientQuota, allowBorrowing)
			targets = append(targets, moreTargets...)
		}
		if !fits && workloadQuotaFits(preemptionCtx, allowBorrowing) {
			// The topology trigger requires a feasible quota, so it is only applied once
			// the quota fits while the workload still doesn't fit (meaning topology is
			// what keeps the workload out).
			var moreTargets []*Target
			fits, moreTargets = simulateConfigurableCandidatesPreemption(preemptionCtx, candidatesOrdering, kueue.QuotaFeasibleAndInsufficientTopology, allowBorrowing)
			targets = append(targets, moreTargets...)
		}
	}
	return fits, targets
}

// simulateConfigurableCandidatesPreemption removes the candidates selected by the rules of the given
// trigger from the snapshot and returns them, from the most to the least preferred one,
// stopping as soon as workloadFits returns true.
// The candidates are preempted regardless of what the classical or Fair Sharing rules
// allow, as the PreemptionConfig selects them explicitly, and are thus reported with the
// ConfigurablePreemption reason.
func simulateConfigurableCandidatesPreemption(preemptionCtx *preemptionCtx, candidatesOrdering func(a, b *workload.Info) int, trigger kueue.PreemptionConfigActivationTrigger, allowBorrowing bool) (bool, []*Target) {
	var targets []*Target
	for _, candidate := range configurableCandidates(preemptionCtx, candidatesOrdering, trigger) {
		preemptionCtx.snapshot.RemoveWorkload(candidate)
		targets = append(targets, &Target{
			WorkloadInfo: candidate,
			Reason:       preemptioncommon.ConfigurablePreemptionReason,
			WorkloadCq:   preemptionCtx.snapshot.ClusterQueue(candidate.ClusterQueue),
		})
		if workloadFits(preemptionCtx, allowBorrowing) {
			return true, targets
		}
	}
	return false, targets
}
