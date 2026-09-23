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
	"slices"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/utils/clock"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kueue "sigs.k8s.io/kueue/apis/kueue/v1beta2"
	schdcache "sigs.k8s.io/kueue/pkg/cache/scheduler"
	"sigs.k8s.io/kueue/pkg/resources"
	utilslices "sigs.k8s.io/kueue/pkg/util/slices"
	utiltesting "sigs.k8s.io/kueue/pkg/util/testing"
	utiltestingapi "sigs.k8s.io/kueue/pkg/util/testing/v1beta2"
	"sigs.k8s.io/kueue/pkg/workload"
)

func TestPreemptionEvaluatorCandidates(t *testing.T) {
	now := time.Now()

	baseCqs := []*kueue.ClusterQueue{
		utiltestingapi.MakeClusterQueue("a").
			Cohort("all").
			ResourceGroup(*utiltestingapi.MakeFlavorQuotas("default").
				Resource(corev1.ResourceCPU, "1").Obj()).
			Obj(),
		utiltestingapi.MakeClusterQueue("b").
			Cohort("all").
			ResourceGroup(*utiltestingapi.MakeFlavorQuotas("default").
				Resource(corev1.ResourceCPU, "1").Obj()).
			Obj(),
	}

	unitWl := *utiltestingapi.MakeWorkload("unit", "").Request(corev1.ResourceCPU, "1")

	clientReader := utiltesting.NewFakeClient(
		utiltestingapi.MakeWorkloadPriorityClass("critical-tier").Label("tier", "critical").Obj(),
		utiltestingapi.MakeWorkloadPriorityClass("batch-tier").Label("tier", "batch").Obj(),
	)

	tests := map[string]struct {
		cohorts       []*kueue.Cohort
		clusterQueues []*kueue.ClusterQueue
		config        kueue.PreemptionConfig
		admitted      []kueue.Workload
		preemptorWl   *kueue.Workload
		preemptorCq   kueue.ClusterQueueReference
		// trigger defaults to kueue.Always when empty.
		trigger        kueue.PreemptionConfigActivationTrigger
		client         client.Reader
		wantCandidates []string
		wantError      string
	}{
		"no candidates for empty config": {
			clusterQueues: baseCqs,
			config: kueue.PreemptionConfig{
				Spec: kueue.PreemptionConfigSpec{
					Rules: []kueue.PreemptionConfigPreemptionRule{},
				},
			},
			admitted: []kueue.Workload{
				*unitWl.Clone().Name("a1").SimpleReserveQuota("a", "default", now).Obj(),
				*unitWl.Clone().Name("a2").SimpleReserveQuota("a", "default", now).Obj(),
			},
			preemptorWl:    unitWl.Clone().Name("a-incoming").Obj(),
			preemptorCq:    "a",
			wantCandidates: []string{},
		},
		"no candidates for rule without selectors": {
			clusterQueues: baseCqs,
			config: kueue.PreemptionConfig{
				Spec: kueue.PreemptionConfigSpec{
					Rules: []kueue.PreemptionConfigPreemptionRule{
						{
							Name:             "test",
							ActivationPolicy: kueue.PreemptionConfigActivationPolicy{Trigger: kueue.Always},
						},
					},
				},
			},
			admitted: []kueue.Workload{
				*unitWl.Clone().Name("a1").SimpleReserveQuota("a", "default", now).Obj(),
				*unitWl.Clone().Name("a2").SimpleReserveQuota("a", "default", now).Obj(),
			},
			preemptorWl:    unitWl.Clone().Name("a-incoming").Obj(),
			preemptorCq:    "a",
			wantCandidates: []string{},
		},
		"returns error for invalid labels selector": {
			clusterQueues: baseCqs,
			config: kueue.PreemptionConfig{
				Spec: kueue.PreemptionConfigSpec{
					Rules: []kueue.PreemptionConfigPreemptionRule{
						{
							Name: "test",
							PreemptorSelector: &metav1.LabelSelector{
								MatchExpressions: []metav1.LabelSelectorRequirement{
									{
										Key:      "test",
										Operator: "invalid",
									},
								},
							},
							ActivationPolicy: kueue.PreemptionConfigActivationPolicy{Trigger: kueue.Always},
						},
					},
				},
			},
			admitted: []kueue.Workload{
				*unitWl.Clone().Name("a1").SimpleReserveQuota("a", "default", now).Obj(),
				*unitWl.Clone().Name("a2").SimpleReserveQuota("a", "default", now).Obj(),
			},
			preemptorWl: unitWl.Clone().Name("a-incoming").Obj(),
			preemptorCq: "a",
			wantError:   "\"invalid\" is not a valid label selector operator",
		},
		"selects candidates for CQ without cohort": {
			clusterQueues: []*kueue.ClusterQueue{
				utiltestingapi.MakeClusterQueue("a").
					ResourceGroup(*utiltestingapi.MakeFlavorQuotas("default").
						Resource(corev1.ResourceCPU, "2").Obj()).
					Obj(),
			},
			config: kueue.PreemptionConfig{
				Spec: kueue.PreemptionConfigSpec{
					Rules: []kueue.PreemptionConfigPreemptionRule{
						{
							Name:             "test",
							ActivationPolicy: kueue.PreemptionConfigActivationPolicy{Trigger: kueue.Always},
							CandidateSelectors: []kueue.PreemptionConfigPreemptionCandidateSelector{
								{
									Scope: kueue.WithinCohortTree,
								},
							},
						},
					},
				},
			},
			admitted: []kueue.Workload{
				*unitWl.Clone().Name("a1").SimpleReserveQuota("a", "default", now).Obj(),
				*unitWl.Clone().Name("a2").SimpleReserveQuota("a", "default", now).Obj(),
			},
			preemptorWl:    unitWl.Clone().Name("a-incoming").Obj(),
			preemptorCq:    "a",
			wantCandidates: []string{"a1", "a2"},
		},
		"selects candidates for the Always trigger": {
			clusterQueues: baseCqs,
			config: kueue.PreemptionConfig{
				Spec: kueue.PreemptionConfigSpec{
					Rules: []kueue.PreemptionConfigPreemptionRule{
						{
							Name:             "test",
							ActivationPolicy: kueue.PreemptionConfigActivationPolicy{Trigger: kueue.Always},
							CandidateSelectors: []kueue.PreemptionConfigPreemptionCandidateSelector{
								{
									Scope: kueue.WithinCohortTree,
								},
							},
						},
					},
				},
			},
			admitted: []kueue.Workload{
				*unitWl.Clone().Name("a1").SimpleReserveQuota("a", "default", now).Obj(),
				*unitWl.Clone().Name("a2").SimpleReserveQuota("a", "default", now).Obj(),
			},
			preemptorWl:    unitWl.Clone().Name("a-incoming").Obj(),
			preemptorCq:    "a",
			wantCandidates: []string{"a1", "a2"},
		},
		"selects candidates for the InsufficientQuota trigger": {
			clusterQueues: baseCqs,
			config: kueue.PreemptionConfig{
				Spec: kueue.PreemptionConfigSpec{
					Rules: []kueue.PreemptionConfigPreemptionRule{
						{
							Name:             "test",
							ActivationPolicy: kueue.PreemptionConfigActivationPolicy{Trigger: kueue.InsufficientQuota},
							CandidateSelectors: []kueue.PreemptionConfigPreemptionCandidateSelector{
								{
									Scope: kueue.WithinCohortTree,
								},
							},
						},
					},
				},
			},
			admitted: []kueue.Workload{
				*unitWl.Clone().Name("a1").SimpleReserveQuota("a", "default", now).Obj(),
				*unitWl.Clone().Name("a2").SimpleReserveQuota("a", "default", now).Obj(),
			},
			preemptorWl:    unitWl.Clone().Name("a-incoming").Obj(),
			preemptorCq:    "a",
			trigger:        kueue.InsufficientQuota,
			wantCandidates: []string{"a1", "a2"},
		},
		"selects candidates for the QuotaFeasibleAndInsufficientTopology trigger": {
			clusterQueues: baseCqs,
			config: kueue.PreemptionConfig{
				Spec: kueue.PreemptionConfigSpec{
					Rules: []kueue.PreemptionConfigPreemptionRule{
						{
							Name:             "test",
							ActivationPolicy: kueue.PreemptionConfigActivationPolicy{Trigger: kueue.QuotaFeasibleAndInsufficientTopology},
							CandidateSelectors: []kueue.PreemptionConfigPreemptionCandidateSelector{
								{
									Scope: kueue.WithinCohortTree,
								},
							},
						},
					},
				},
			},
			admitted: []kueue.Workload{
				*unitWl.Clone().Name("a1").SimpleReserveQuota("a", "default", now).Obj(),
				*unitWl.Clone().Name("a2").SimpleReserveQuota("a", "default", now).Obj(),
			},
			preemptorWl:    unitWl.Clone().Name("a-incoming").Obj(),
			preemptorCq:    "a",
			trigger:        kueue.QuotaFeasibleAndInsufficientTopology,
			wantCandidates: []string{"a1", "a2"},
		},
		"rule with matching preemptor labels selector is triggered for matching workload": {
			clusterQueues: baseCqs,
			config: kueue.PreemptionConfig{
				Spec: kueue.PreemptionConfigSpec{
					Rules: []kueue.PreemptionConfigPreemptionRule{
						{
							Name:             "test",
							ActivationPolicy: kueue.PreemptionConfigActivationPolicy{Trigger: kueue.Always},
							PreemptorSelector: &metav1.LabelSelector{
								MatchLabels: map[string]string{"active": "true"},
							},
							CandidateSelectors: []kueue.PreemptionConfigPreemptionCandidateSelector{
								{
									Scope: kueue.WithinCohortTree,
								},
							},
						},
					},
				},
			},
			admitted: []kueue.Workload{
				*unitWl.Clone().Name("a1").SimpleReserveQuota("a", "default", now).Obj(),
				*unitWl.Clone().Name("a2").SimpleReserveQuota("a", "default", now).Obj(),
			},
			preemptorWl:    unitWl.Clone().Name("a-incoming").Label("active", "true").Obj(),
			preemptorCq:    "a",
			wantCandidates: []string{"a1", "a2"},
		},
		"rule does not apply because of not matching preemptor labels selector": {
			clusterQueues: baseCqs,
			config: kueue.PreemptionConfig{
				Spec: kueue.PreemptionConfigSpec{
					Rules: []kueue.PreemptionConfigPreemptionRule{
						{
							Name:             "test",
							ActivationPolicy: kueue.PreemptionConfigActivationPolicy{Trigger: kueue.Always},
							PreemptorSelector: &metav1.LabelSelector{
								MatchLabels: map[string]string{"active": "true"},
							},
						},
					},
				},
			},
			admitted: []kueue.Workload{
				*unitWl.Clone().Name("a1").SimpleReserveQuota("a", "default", now).Obj(),
				*unitWl.Clone().Name("a2").SimpleReserveQuota("a", "default", now).Obj(),
			},
			preemptorWl:    unitWl.Clone().Name("a-incoming").Obj(),
			preemptorCq:    "a",
			wantCandidates: []string{},
		},
		"returns candidates from ClusterQueues not under the same root": {
			clusterQueues: []*kueue.ClusterQueue{
				utiltestingapi.MakeClusterQueue("a").
					Cohort("a-cohort").
					ResourceGroup(*utiltestingapi.MakeFlavorQuotas("default").
						Resource(corev1.ResourceCPU, "1").Obj()).
					Obj(),
				utiltestingapi.MakeClusterQueue("b").
					Cohort("b-cohort").
					ResourceGroup(*utiltestingapi.MakeFlavorQuotas("default").
						Resource(corev1.ResourceCPU, "1").Obj()).
					Obj(),
				utiltestingapi.MakeClusterQueue("c").
					ResourceGroup(*utiltestingapi.MakeFlavorQuotas("default").
						Resource(corev1.ResourceCPU, "1").Obj()).
					Obj(),
			},
			config: kueue.PreemptionConfig{
				Spec: kueue.PreemptionConfigSpec{
					Rules: []kueue.PreemptionConfigPreemptionRule{
						{
							Name:             "test",
							ActivationPolicy: kueue.PreemptionConfigActivationPolicy{Trigger: kueue.Always},
							CandidateSelectors: []kueue.PreemptionConfigPreemptionCandidateSelector{
								{
									Scope: kueue.AnyClusterQueue,
								},
							},
						},
					},
				},
			},
			admitted: []kueue.Workload{
				*unitWl.Clone().Name("a1").SimpleReserveQuota("a", "default", now).Obj(),
				*unitWl.Clone().Name("b1").SimpleReserveQuota("b", "default", now).Obj(),
				*unitWl.Clone().Name("c1").SimpleReserveQuota("c", "default", now).Obj(),
			},
			preemptorWl:    unitWl.Clone().Name("a-incoming").Obj(),
			preemptorCq:    "a",
			wantCandidates: []string{"a1", "b1", "c1"},
		},
		"returns candidates grouped by the trigger of the rule selecting them": {
			clusterQueues: baseCqs,
			config: kueue.PreemptionConfig{
				Spec: kueue.PreemptionConfigSpec{
					Rules: []kueue.PreemptionConfigPreemptionRule{
						{
							Name:             "same-cluster-queue-rule",
							ActivationPolicy: kueue.PreemptionConfigActivationPolicy{Trigger: kueue.Always},
							CandidateSelectors: []kueue.PreemptionConfigPreemptionCandidateSelector{
								{
									Scope: kueue.WithinClusterQueue,
								},
							},
						},
						{
							Name:             "cohort-rule",
							ActivationPolicy: kueue.PreemptionConfigActivationPolicy{Trigger: kueue.InsufficientQuota},
							CandidateSelectors: []kueue.PreemptionConfigPreemptionCandidateSelector{
								{
									Scope: kueue.WithinCohortTree,
								},
							},
						},
					},
				},
			},
			admitted: []kueue.Workload{
				*unitWl.Clone().Name("a1").SimpleReserveQuota("a", "default", now).Obj(),
				*unitWl.Clone().Name("b1").SimpleReserveQuota("b", "default", now).Obj(),
			},
			preemptorWl:    unitWl.Clone().Name("a-incoming").Obj(),
			preemptorCq:    "a",
			trigger:        kueue.Always,
			wantCandidates: []string{"a1"},
		},
		"returns candidates which use preemptable resource": {
			clusterQueues: baseCqs,
			config: kueue.PreemptionConfig{
				Spec: kueue.PreemptionConfigSpec{
					Rules: []kueue.PreemptionConfigPreemptionRule{
						{
							Name:             "topology-rule",
							ActivationPolicy: kueue.PreemptionConfigActivationPolicy{Trigger: kueue.Always},
							CandidateSelectors: []kueue.PreemptionConfigPreemptionCandidateSelector{
								{
									Scope: kueue.WithinCohortTree,
								},
							},
						},
					},
				},
			},
			admitted: []kueue.Workload{
				*unitWl.Clone().Name("a1").SimpleReserveQuota("a", "default", now).Obj(),
				*unitWl.Clone().Name("a2").SimpleReserveQuota("a", "other-flavor", now).Obj(),
			},
			preemptorWl:    unitWl.Clone().Name("a-incoming").Obj(),
			preemptorCq:    "a",
			wantCandidates: []string{"a1"},
		},
		"returns non repeating candidates even when the same candidates are matched by several rules of a trigger": {
			clusterQueues: baseCqs,
			config: kueue.PreemptionConfig{
				Spec: kueue.PreemptionConfigSpec{
					Rules: []kueue.PreemptionConfigPreemptionRule{
						{
							Name:             "first-rule",
							ActivationPolicy: kueue.PreemptionConfigActivationPolicy{Trigger: kueue.Always},
							CandidateSelectors: []kueue.PreemptionConfigPreemptionCandidateSelector{
								{
									Scope: kueue.WithinClusterQueue,
								},
							},
						},
						{
							Name:             "second-rule",
							ActivationPolicy: kueue.PreemptionConfigActivationPolicy{Trigger: kueue.Always},
							CandidateSelectors: []kueue.PreemptionConfigPreemptionCandidateSelector{
								{
									Scope: kueue.WithinCohortTree,
								},
							},
						},
					},
				},
			},
			admitted: []kueue.Workload{
				*unitWl.Clone().Name("a1").SimpleReserveQuota("a", "default", now).Obj(),
				*unitWl.Clone().Name("b1").SimpleReserveQuota("b", "default", now).Obj(),
			},
			preemptorWl:    unitWl.Clone().Name("a-incoming").Obj(),
			preemptorCq:    "a",
			wantCandidates: []string{"a1", "b1"},
		},
		"Priority filters candidates with higher priority": {
			clusterQueues: baseCqs,
			config: kueue.PreemptionConfig{
				Spec: kueue.PreemptionConfigSpec{
					Rules: []kueue.PreemptionConfigPreemptionRule{
						{
							Name:             "priority rule",
							ActivationPolicy: kueue.PreemptionConfigActivationPolicy{Trigger: kueue.Always},
							CandidateSelectors: []kueue.PreemptionConfigPreemptionCandidateSelector{
								{
									Scope: kueue.WithinClusterQueue,
									Priority: &kueue.PreemptionConfigPriorityConstraint{
										Mode:       kueue.Base,
										Comparison: kueue.LessThan,
									},
								},
							},
						},
					},
				},
			},
			admitted: []kueue.Workload{
				*unitWl.Clone().Name("a1").Priority(50).SimpleReserveQuota("a", "default", now).Obj(),
				*unitWl.Clone().Name("a2").Priority(150).SimpleReserveQuota("a", "default", now).Obj(),
			},
			preemptorWl:    unitWl.Clone().Name("a-incoming").Priority(100).Obj(),
			preemptorCq:    "a",
			wantCandidates: []string{"a1"},
		},
		"LabelSelector filters candidate workloads matching label selector": {
			clusterQueues: baseCqs,
			config: kueue.PreemptionConfig{
				Spec: kueue.PreemptionConfigSpec{
					Rules: []kueue.PreemptionConfigPreemptionRule{
						{
							Name:             "label-selector rule",
							ActivationPolicy: kueue.PreemptionConfigActivationPolicy{Trigger: kueue.InsufficientQuota},
							CandidateSelectors: []kueue.PreemptionConfigPreemptionCandidateSelector{
								{
									Scope: kueue.WithinClusterQueue,
									LabelSelector: &metav1.LabelSelector{
										MatchLabels: map[string]string{"env": "preemptible"},
									},
								},
							},
						},
					},
				},
			},
			admitted: []kueue.Workload{
				*unitWl.Clone().Name("a1").
					Label("env", "preemptible").
					SimpleReserveQuota("a", "default", now).Obj(),
				*unitWl.Clone().Name("a2").
					Label("env", "guaranteed").
					SimpleReserveQuota("a", "default", now).Obj(),
				*unitWl.Clone().Name("a3").
					SimpleReserveQuota("a", "default", now).Obj(),
			},
			preemptorWl:    unitWl.Clone().Name("a-incoming").Obj(),
			preemptorCq:    "a",
			trigger:        kueue.InsufficientQuota,
			wantCandidates: []string{"a1"},
		},
		"ClusterQueueSelector filters candidates by matching ClusterQueue labels": {
			clusterQueues: []*kueue.ClusterQueue{
				utiltestingapi.MakeClusterQueue("a").
					Cohort("all").
					Label("tier", "preemptible").
					ResourceGroup(*utiltestingapi.MakeFlavorQuotas("default").
						Resource(corev1.ResourceCPU, "1").Obj()).
					Obj(),
				utiltestingapi.MakeClusterQueue("b").
					Cohort("all").
					Label("tier", "protected").
					ResourceGroup(*utiltestingapi.MakeFlavorQuotas("default").
						Resource(corev1.ResourceCPU, "1").Obj()).
					Obj(),
			},
			config: kueue.PreemptionConfig{
				Spec: kueue.PreemptionConfigSpec{
					Rules: []kueue.PreemptionConfigPreemptionRule{
						{
							Name:             "cq-selector-rule",
							ActivationPolicy: kueue.PreemptionConfigActivationPolicy{Trigger: kueue.InsufficientQuota},
							CandidateSelectors: []kueue.PreemptionConfigPreemptionCandidateSelector{
								{
									Scope: kueue.WithinParentCohort,
									ClusterQueueSelector: &metav1.LabelSelector{
										MatchLabels: map[string]string{"tier": "preemptible"},
									},
								},
							},
						},
					},
				},
			},
			admitted: []kueue.Workload{
				*unitWl.Clone().Name("a1").SimpleReserveQuota("a", "default", now).Obj(),
				*unitWl.Clone().Name("b1").SimpleReserveQuota("b", "default", now).Obj(),
			},
			preemptorWl:    unitWl.Clone().Name("a-incoming").Obj(),
			preemptorCq:    "a",
			trigger:        kueue.InsufficientQuota,
			wantCandidates: []string{"a1"},
		},
		"multi-selector deduplication": {
			clusterQueues: baseCqs,
			config: kueue.PreemptionConfig{
				Spec: kueue.PreemptionConfigSpec{
					Rules: []kueue.PreemptionConfigPreemptionRule{
						{
							Name:             "rule-1",
							ActivationPolicy: kueue.PreemptionConfigActivationPolicy{Trigger: kueue.Always},
							CandidateSelectors: []kueue.PreemptionConfigPreemptionCandidateSelector{
								{
									Scope: kueue.WithinClusterQueue,
								},
								{
									Scope: kueue.WithinCohortTree,
								},
							},
						},
						{
							Name:             "rule-2",
							ActivationPolicy: kueue.PreemptionConfigActivationPolicy{Trigger: kueue.Always},
							CandidateSelectors: []kueue.PreemptionConfigPreemptionCandidateSelector{
								{
									Scope: kueue.WithinCohortTree,
								},
							},
						},
					},
				},
			},
			admitted: []kueue.Workload{
				*unitWl.Clone().Name("a1-same-cq").Priority(10).SimpleReserveQuota("a", "default", now).Obj(),
				*unitWl.Clone().Name("b1-sibling-cq").Priority(20).SimpleReserveQuota("b", "default", now).Obj(),
			},
			preemptorWl:    unitWl.Clone().Name("a-incoming").Obj(),
			preemptorCq:    "a",
			wantCandidates: []string{"a1-same-cq", "b1-sibling-cq"},
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			ctx, log := utiltesting.ContextWithLog(t)
			// Set name as UID so that candidates sorting is predictable.
			for i := range tc.admitted {
				tc.admitted[i].UID = types.UID(tc.admitted[i].Name)
			}

			cl := utiltesting.NewClientBuilder().
				WithLists(&kueue.WorkloadList{Items: tc.admitted}).
				Build()

			cqCache := schdcache.New(cl)
			cqCache.AddOrUpdateResourceFlavor(log, utiltestingapi.MakeResourceFlavor("default").Obj())
			cqCache.AddOrUpdateResourceFlavor(log, utiltestingapi.MakeResourceFlavor("other-flavour").Obj())

			for _, cq := range tc.clusterQueues {
				if err := cqCache.AddClusterQueue(ctx, cq); err != nil {
					t.Fatalf("Couldn't add ClusterQueue to cache: %v", err)
				}
			}
			for _, cohort := range tc.cohorts {
				if err := cqCache.AddOrUpdateCohort(cohort); err != nil {
					t.Fatalf("Couldn't add Cohort to cache: %v", err)
				}
			}

			snapshot, err := cqCache.Snapshot(ctx)
			if err != nil {
				t.Fatalf("unexpected error while building snapshot: %v", err)
			}

			evaluator := NewPreemptionEvaluator(ctx, log, clock.RealClock{}, tc.config, clientReader)

			wlInfo := workload.NewInfo(tc.preemptorWl)
			wlInfo.ClusterQueue = tc.preemptorCq

			trigger := tc.trigger
			if trigger == "" {
				trigger = kueue.Always
			}
			frsNeedPreemption := sets.New(resources.FlavorResource{Flavor: "default", Resource: corev1.ResourceCPU})
			candidates, err := evaluator.Candidates(snapshot, wlInfo, frsNeedPreemption, trigger)
			if err != nil || tc.wantError != "" {
				gotError := ""
				if err != nil {
					gotError = err.Error()
				}
				if diff := cmp.Diff(tc.wantError, gotError, cmpopts.EquateEmpty()); diff != "" {
					t.Errorf("Candidates() error (-want +got):\n%s", diff)
				}
				return
			}

			// Candidates are not ordered, so compare them as sorted lists.
			gotCandidates := slices.Sorted(slices.Values(utilslices.Map(candidates, func(wlInfo **workload.Info) string {
				return (*wlInfo).Obj.Name
			})))
			wantCandidates := slices.Sorted(slices.Values(tc.wantCandidates))
			if diff := cmp.Diff(wantCandidates, gotCandidates, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("Selected candidates (-want,+got):\n%s", diff)
			}
		})
	}
}
