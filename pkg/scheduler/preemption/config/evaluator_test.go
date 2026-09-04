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
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/utils/clock"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kueue "sigs.k8s.io/kueue/apis/kueue/v1beta2"
	schdcache "sigs.k8s.io/kueue/pkg/cache/scheduler"
	"sigs.k8s.io/kueue/pkg/resources"
	utiltesting "sigs.k8s.io/kueue/pkg/util/testing"
	utiltestingapi "sigs.k8s.io/kueue/pkg/util/testing/v1beta2"
	"sigs.k8s.io/kueue/pkg/workload"
)

func TestPreemptionEvaluatorIter(t *testing.T) {
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

	insufficientQuotaCond := metav1.Condition{
		Type:   string(kueue.InsufficientQuota),
		Status: metav1.ConditionTrue,
	}
	quotaReclaimRequiredCond := metav1.Condition{
		Type:   string(kueue.QuotaReclaimRequired),
		Status: metav1.ConditionTrue,
	}
	insufficientTopologyCond := metav1.Condition{
		Type:   string(kueue.InsufficientTopology),
		Status: metav1.ConditionTrue,
	}

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
		client        client.Reader
		wantWlOrder   []string
		wantError     string

		// rare options for full testing coverage
		haltIterationAfterNWorkloads int
	}{
		"no candidates for empty config": {
			clusterQueues: baseCqs,
			config: kueue.PreemptionConfig{
				Spec: kueue.PreemptionConfigSpec{
					Rules: []kueue.PreemptionRule{},
				},
			},
			admitted: []kueue.Workload{
				*unitWl.Clone().Name("a1").SimpleReserveQuota("a", "default", now).Obj(),
				*unitWl.Clone().Name("a2").SimpleReserveQuota("a", "default", now).Obj(),
			},
			preemptorWl: unitWl.Clone().Name("a-incoming").Condition(insufficientQuotaCond).Obj(),
			preemptorCq: "a",
			wantWlOrder: []string{},
		},
		"no candidates for workload not matching a trigger": {
			clusterQueues: baseCqs,
			config: kueue.PreemptionConfig{
				Spec: kueue.PreemptionConfigSpec{
					Rules: []kueue.PreemptionRule{
						{
							Name:    "test",
							Trigger: kueue.InsufficientQuota,
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
			wantWlOrder: []string{},
		},
		"returns error for invalid labels selector": {
			clusterQueues: baseCqs,
			config: kueue.PreemptionConfig{
				Spec: kueue.PreemptionConfigSpec{
					Rules: []kueue.PreemptionRule{
						{
							Name: "test",
							MatchingPreemptorWorkloads: metav1.LabelSelector{
								MatchExpressions: []metav1.LabelSelectorRequirement{
									{
										Key:      "test",
										Operator: "invalid",
									},
								},
							},
							Trigger: kueue.InsufficientQuota,
						},
					},
				},
			},
			admitted: []kueue.Workload{
				*unitWl.Clone().Name("a1").SimpleReserveQuota("a", "default", now).Obj(),
				*unitWl.Clone().Name("a2").SimpleReserveQuota("a", "default", now).Obj(),
			},
			preemptorWl: unitWl.Clone().Name("a-incoming").Condition(insufficientQuotaCond).Obj(),
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
					Rules: []kueue.PreemptionRule{
						{
							Name:    "test",
							Trigger: kueue.InsufficientQuota,
							Candidates: []kueue.PreemptionCandidateSelector{
								{
									RelationRequirement: kueue.SameCohortTree,
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
			preemptorWl: unitWl.Clone().Name("a-incoming").Condition(insufficientQuotaCond).Obj(),
			preemptorCq: "a",
			wantWlOrder: []string{"a1", "a2"},
		},
		"selects candidates for InsufficientQuota trigger": {
			clusterQueues: baseCqs,
			config: kueue.PreemptionConfig{
				Spec: kueue.PreemptionConfigSpec{
					Rules: []kueue.PreemptionRule{
						{
							Name:    "test",
							Trigger: kueue.InsufficientQuota,
							Candidates: []kueue.PreemptionCandidateSelector{
								{
									RelationRequirement: kueue.SameCohortTree,
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
			preemptorWl: unitWl.Clone().Name("a-incoming").Condition(insufficientQuotaCond).Obj(),
			preemptorCq: "a",
			wantWlOrder: []string{"a1", "a2"},
		},
		"selects candidates for QuotaReclaimRequired trigger": {
			clusterQueues: baseCqs,
			config: kueue.PreemptionConfig{
				Spec: kueue.PreemptionConfigSpec{
					Rules: []kueue.PreemptionRule{
						{
							Name:    "test",
							Trigger: kueue.QuotaReclaimRequired,
							Candidates: []kueue.PreemptionCandidateSelector{
								{
									RelationRequirement: kueue.SameCohortTree,
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
			preemptorWl: unitWl.Clone().Name("a-incoming").Condition(quotaReclaimRequiredCond).Obj(),
			preemptorCq: "a",
			wantWlOrder: []string{"a1", "a2"},
		},
		"selects candidates for InsufficientTopology trigger": {
			clusterQueues: baseCqs,
			config: kueue.PreemptionConfig{
				Spec: kueue.PreemptionConfigSpec{
					Rules: []kueue.PreemptionRule{
						{
							Name:    "test",
							Trigger: kueue.InsufficientTopology,
							Candidates: []kueue.PreemptionCandidateSelector{
								{
									RelationRequirement: kueue.SameCohortTree,
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
			preemptorWl: unitWl.Clone().Name("a-incoming").Condition(insufficientTopologyCond).Obj(),
			preemptorCq: "a",
			wantWlOrder: []string{"a1", "a2"},
		},
		"trigger isn't active because of min trigger requirement duration ": {
			clusterQueues: baseCqs,
			config: kueue.PreemptionConfig{
				Spec: kueue.PreemptionConfigSpec{
					Rules: []kueue.PreemptionRule{
						{
							Name:                       "test",
							Trigger:                    kueue.InsufficientTopology,
							MinTriggerRequiredDuration: metav1.Duration{Duration: 10 * time.Minute},
						},
					},
				},
			},
			admitted: []kueue.Workload{
				*unitWl.Clone().Name("a1").SimpleReserveQuota("a", "default", now).Obj(),
				*unitWl.Clone().Name("a2").SimpleReserveQuota("a", "default", now).Obj(),
			},
			preemptorWl: unitWl.Clone().Name("a-incoming").Condition(insufficientTopologyCond).Obj(),
			preemptorCq: "a",
			wantWlOrder: []string{},
		},
		"rule with matching preemptor labels selector is triggered for matching workload": {
			clusterQueues: baseCqs,
			config: kueue.PreemptionConfig{
				Spec: kueue.PreemptionConfigSpec{
					Rules: []kueue.PreemptionRule{
						{
							Name:    "test",
							Trigger: kueue.InsufficientTopology,
							MatchingPreemptorWorkloads: metav1.LabelSelector{
								MatchLabels: map[string]string{"active": "true"},
							},
							Candidates: []kueue.PreemptionCandidateSelector{
								{
									RelationRequirement: kueue.SameCohortTree,
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
			preemptorWl: unitWl.Clone().Name("a-incoming").Label("active", "true").Condition(insufficientTopologyCond).Obj(),
			preemptorCq: "a",
			wantWlOrder: []string{"a1", "a2"},
		},
		"trigger matched by labels selector with iteration break": {
			clusterQueues: baseCqs,
			config: kueue.PreemptionConfig{
				Spec: kueue.PreemptionConfigSpec{
					Rules: []kueue.PreemptionRule{
						{
							Name:    "test",
							Trigger: kueue.InsufficientTopology,
							MatchingPreemptorWorkloads: metav1.LabelSelector{
								MatchLabels: map[string]string{"active": "true"},
							},
							Candidates: []kueue.PreemptionCandidateSelector{
								{
									RelationRequirement: kueue.SameCohortTree,
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
			preemptorWl:                  unitWl.Clone().Name("a-incoming").Label("active", "true").Condition(insufficientTopologyCond).Obj(),
			preemptorCq:                  "a",
			haltIterationAfterNWorkloads: 1,
			wantWlOrder:                  []string{"a1"},
		},
		"rule does not apply because of not matching preemptor labels selector": {
			clusterQueues: baseCqs,
			config: kueue.PreemptionConfig{
				Spec: kueue.PreemptionConfigSpec{
					Rules: []kueue.PreemptionRule{
						{
							Name:    "test",
							Trigger: kueue.InsufficientTopology,
							MatchingPreemptorWorkloads: metav1.LabelSelector{
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
			preemptorWl: unitWl.Clone().Name("a-incoming").Condition(insufficientTopologyCond).Obj(),
			preemptorCq: "a",
			wantWlOrder: []string{},
		},
		"rule does not apply because of different condition on preemptor's workload": {
			clusterQueues: baseCqs,
			config: kueue.PreemptionConfig{
				Spec: kueue.PreemptionConfigSpec{
					Rules: []kueue.PreemptionRule{
						{
							Name:    "test",
							Trigger: kueue.InsufficientTopology,
						},
					},
				},
			},
			admitted: []kueue.Workload{
				*unitWl.Clone().Name("a1").SimpleReserveQuota("a", "default", now).Obj(),
				*unitWl.Clone().Name("a2").SimpleReserveQuota("a", "default", now).Obj(),
			},
			preemptorWl: unitWl.Clone().Name("a-incoming").Condition(quotaReclaimRequiredCond).Obj(),
			preemptorCq: "a",
			wantWlOrder: []string{},
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
					Rules: []kueue.PreemptionRule{
						{
							Name:    "test",
							Trigger: kueue.InsufficientTopology,
							Candidates: []kueue.PreemptionCandidateSelector{
								{
									RelationRequirement: kueue.AnyClusterQueue,
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
			preemptorWl: unitWl.Clone().Name("a-incoming").Condition(insufficientTopologyCond).Obj(),
			preemptorCq: "a",
			wantWlOrder: []string{"a1", "b1", "c1"},
		},
		"returns candidates based on selectors from matched trigger": {
			clusterQueues: baseCqs,
			config: kueue.PreemptionConfig{
				Spec: kueue.PreemptionConfigSpec{
					Rules: []kueue.PreemptionRule{
						{
							Name:    "topology rule",
							Trigger: kueue.InsufficientTopology,
							Candidates: []kueue.PreemptionCandidateSelector{
								{
									RelationRequirement: kueue.SameClusterQueue,
								},
							},
						},
						{
							Name:    "quota rule",
							Trigger: kueue.InsufficientQuota,
							Candidates: []kueue.PreemptionCandidateSelector{
								{
									RelationRequirement: kueue.SameCohortTree,
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
			preemptorWl: unitWl.Clone().Name("a-incoming").Condition(insufficientTopologyCond).Obj(),
			preemptorCq: "a",
			wantWlOrder: []string{"a1"},
		},
		"returns candidates which use preemptable resource": {
			clusterQueues: baseCqs,
			config: kueue.PreemptionConfig{
				Spec: kueue.PreemptionConfigSpec{
					Rules: []kueue.PreemptionRule{
						{
							Name:    "topology rule",
							Trigger: kueue.InsufficientTopology,
							Candidates: []kueue.PreemptionCandidateSelector{
								{
									RelationRequirement: kueue.SameCohortTree,
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
			preemptorWl: unitWl.Clone().Name("a-incoming").Condition(insufficientTopologyCond).Obj(),
			preemptorCq: "a",
			wantWlOrder: []string{"a1"},
		},
		"returns non repeating candidates even when same candidates matched by different trigger rules": {
			clusterQueues: baseCqs,
			config: kueue.PreemptionConfig{
				Spec: kueue.PreemptionConfigSpec{
					Rules: []kueue.PreemptionRule{
						{
							Name:    "first rule",
							Trigger: kueue.InsufficientTopology,
							Candidates: []kueue.PreemptionCandidateSelector{
								{
									RelationRequirement: kueue.SameClusterQueue,
								},
							},
						},
						{
							Name:    "second rule",
							Trigger: kueue.InsufficientTopology,
							Candidates: []kueue.PreemptionCandidateSelector{
								{
									RelationRequirement: kueue.SameCohortTree,
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
			preemptorWl: unitWl.Clone().Name("a-incoming").Condition(insufficientTopologyCond).Obj(),
			preemptorCq: "a",
			wantWlOrder: []string{"a1", "b1"},
		},
		"PreemptingWorkloadPrioritySelector rejects when preemptor does not match priority selector": {
			clusterQueues: baseCqs,
			config: kueue.PreemptionConfig{
				Spec: kueue.PreemptionConfigSpec{
					Rules: []kueue.PreemptionRule{
						{
							Name:    "priority-gated rule",
							Trigger: kueue.InsufficientQuota,
							Candidates: []kueue.PreemptionCandidateSelector{
								{
									RelationRequirement: kueue.SameClusterQueue,
									PreemptingWorkloadPrioritySelector: &metav1.LabelSelector{
										MatchLabels: map[string]string{"tier": "critical"},
									},
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
			preemptorWl: unitWl.Clone().Name("a-incoming").
				PriorityClassRef(kueue.NewWorkloadPriorityClassRef("batch-tier")).
				Condition(insufficientQuotaCond).Obj(),
			preemptorCq: "a",
			wantWlOrder: []string{},
		},
		"PreemptingWorkloadPrioritySelector allows when preemptor matches priority selector": {
			clusterQueues: baseCqs,
			config: kueue.PreemptionConfig{
				Spec: kueue.PreemptionConfigSpec{
					Rules: []kueue.PreemptionRule{
						{
							Name:    "priority-gated rule",
							Trigger: kueue.InsufficientQuota,
							Candidates: []kueue.PreemptionCandidateSelector{
								{
									RelationRequirement: kueue.SameClusterQueue,
									PreemptingWorkloadPrioritySelector: &metav1.LabelSelector{
										MatchLabels: map[string]string{"tier": "critical"},
									},
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
			preemptorWl: unitWl.Clone().Name("a-incoming").
				PriorityClassRef(kueue.NewWorkloadPriorityClassRef("critical-tier")).
				Condition(insufficientQuotaCond).Obj(),
			preemptorCq: "a",
			wantWlOrder: []string{"a1", "a2"},
		},
		"CandidateWorkloadPrioritySelector filters candidates matching priority selector": {
			clusterQueues: baseCqs,
			config: kueue.PreemptionConfig{
				Spec: kueue.PreemptionConfigSpec{
					Rules: []kueue.PreemptionRule{
						{
							Name:    "candidate-priority rule",
							Trigger: kueue.InsufficientQuota,
							Candidates: []kueue.PreemptionCandidateSelector{
								{
									RelationRequirement: kueue.SameClusterQueue,
									CandidateWorkloadPrioritySelector: &metav1.LabelSelector{
										MatchLabels: map[string]string{"tier": "batch"},
									},
								},
							},
						},
					},
				},
			},
			admitted: []kueue.Workload{
				*unitWl.Clone().Name("a1").
					PriorityClassRef(kueue.NewWorkloadPriorityClassRef("batch-tier")).
					SimpleReserveQuota("a", "default", now).Obj(),
				*unitWl.Clone().Name("a2").
					PriorityClassRef(kueue.NewWorkloadPriorityClassRef("critical-tier")).
					SimpleReserveQuota("a", "default", now).Obj(),
			},
			preemptorWl: unitWl.Clone().Name("a-incoming").Condition(insufficientQuotaCond).Obj(),
			preemptorCq: "a",
			wantWlOrder: []string{"a1"},
		},
		"Priority selectors combined with RelativeWorkloadPriority": {
			clusterQueues: baseCqs,
			config: kueue.PreemptionConfig{
				Spec: kueue.PreemptionConfigSpec{
					Rules: []kueue.PreemptionRule{
						{
							Name:    "combined-priority rule",
							Trigger: kueue.InsufficientQuota,
							Candidates: []kueue.PreemptionCandidateSelector{
								{
									RelationRequirement: kueue.SameClusterQueue,
									PreemptingWorkloadPrioritySelector: &metav1.LabelSelector{
										MatchLabels: map[string]string{"tier": "critical"},
									},
									CandidateWorkloadPrioritySelector: &metav1.LabelSelector{
										MatchLabels: map[string]string{"tier": "batch"},
									},
									RelativeWorkloadPriority: ptr.To(kueue.Lower),
								},
							},
						},
					},
				},
			},
			admitted: []kueue.Workload{
				*unitWl.Clone().Name("a1").
					PriorityClassRef(kueue.NewWorkloadPriorityClassRef("batch-tier")).
					Priority(50).
					SimpleReserveQuota("a", "default", now).Obj(),
				*unitWl.Clone().Name("a2").
					PriorityClassRef(kueue.NewWorkloadPriorityClassRef("batch-tier")).
					Priority(150).
					SimpleReserveQuota("a", "default", now).Obj(),
				*unitWl.Clone().Name("a3").
					PriorityClassRef(kueue.NewWorkloadPriorityClassRef("critical-tier")).
					Priority(50).
					SimpleReserveQuota("a", "default", now).Obj(),
			},
			preemptorWl: unitWl.Clone().Name("a-incoming").
				PriorityClassRef(kueue.NewWorkloadPriorityClassRef("critical-tier")).
				Priority(100).
				Condition(insufficientQuotaCond).Obj(),
			preemptorCq: "a",
			wantWlOrder: []string{"a1"},
		},
		"multi-key ordering: Priority -> AdmissionTimestamp": {
			clusterQueues: baseCqs,
			config: kueue.PreemptionConfig{
				Spec: kueue.PreemptionConfigSpec{
					Ordering: []kueue.Order{
						{OrderingField: kueue.Priority},
						{OrderingField: kueue.AdmissionTimestamp, Direction: kueue.Descending},
					},
					Rules: []kueue.PreemptionRule{
						{
							Name:    "multi-key ordering rule",
							Trigger: kueue.InsufficientQuota,
							Candidates: []kueue.PreemptionCandidateSelector{
								{
									RelationRequirement: kueue.SameClusterQueue,
								},
							},
						},
					},
				},
			},
			admitted: []kueue.Workload{
				*unitWl.Clone().Name("w1-prio10-old").Priority(10).SimpleReserveQuota("a", "default", now.Add(-10*time.Minute)).Obj(),
				*unitWl.Clone().Name("w2-prio10-new").Priority(10).SimpleReserveQuota("a", "default", now.Add(-1*time.Minute)).Obj(),
				*unitWl.Clone().Name("w3-prio20-new").Priority(20).SimpleReserveQuota("a", "default", now.Add(-1*time.Minute)).Obj(),
				*unitWl.Clone().Name("w4-prio20-old").Priority(20).SimpleReserveQuota("a", "default", now.Add(-10*time.Minute)).Obj(),
			},
			preemptorWl: unitWl.Clone().Name("a-incoming").Condition(insufficientQuotaCond).Obj(),
			preemptorCq: "a",
			wantWlOrder: []string{"w2-prio10-new", "w1-prio10-old", "w3-prio20-new", "w4-prio20-old"},
		},
		"multi-selector deduplication and simultaneous popping": {
			clusterQueues: baseCqs,
			config: kueue.PreemptionConfig{
				Spec: kueue.PreemptionConfigSpec{
					Ordering: []kueue.Order{{OrderingField: kueue.Priority}},
					Rules: []kueue.PreemptionRule{
						{
							Name:    "rule-1",
							Trigger: kueue.InsufficientQuota,
							Candidates: []kueue.PreemptionCandidateSelector{
								{
									RelationRequirement: kueue.SameClusterQueue,
								},
								{
									RelationRequirement: kueue.SameCohortTree,
								},
							},
						},
						{
							Name:    "rule-2",
							Trigger: kueue.InsufficientQuota,
							Candidates: []kueue.PreemptionCandidateSelector{
								{
									RelationRequirement: kueue.SameCohortTree,
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
			preemptorWl: unitWl.Clone().Name("a-incoming").Condition(insufficientQuotaCond).Obj(),
			preemptorCq: "a",
			wantWlOrder: []string{"a1-same-cq", "b1-sibling-cq"},
		},
		"deep hierarchical cohort tree (4 levels) with IsOtherCohort ordering": {
			cohorts: []*kueue.Cohort{
				utiltestingapi.MakeCohort("root").Obj(),
				utiltestingapi.MakeCohort("lvl1").Parent("root").Obj(),
				utiltestingapi.MakeCohort("lvl2").Parent("lvl1").Obj(),
				utiltestingapi.MakeCohort("lvl3").Parent("lvl2").Obj(),
				utiltestingapi.MakeCohort("lvl1-sib").Parent("root").Obj(),
			},
			clusterQueues: []*kueue.ClusterQueue{
				utiltestingapi.MakeClusterQueue("cq-deep-1").Cohort("lvl3").
					ResourceGroup(*utiltestingapi.MakeFlavorQuotas("default").Resource(corev1.ResourceCPU, "10").Obj()).Obj(),
				utiltestingapi.MakeClusterQueue("cq-deep-2").Cohort("lvl3").
					ResourceGroup(*utiltestingapi.MakeFlavorQuotas("default").Resource(corev1.ResourceCPU, "10").Obj()).Obj(),
				utiltestingapi.MakeClusterQueue("cq-root").Cohort("root").
					ResourceGroup(*utiltestingapi.MakeFlavorQuotas("default").Resource(corev1.ResourceCPU, "10").Obj()).Obj(),
				utiltestingapi.MakeClusterQueue("cq-sib-branch").Cohort("lvl1-sib").
					ResourceGroup(*utiltestingapi.MakeFlavorQuotas("default").Resource(corev1.ResourceCPU, "10").Obj()).Obj(),
				utiltestingapi.MakeClusterQueue("cq-standalone").
					ResourceGroup(*utiltestingapi.MakeFlavorQuotas("default").Resource(corev1.ResourceCPU, "10").Obj()).Obj(),
			},
			config: kueue.PreemptionConfig{
				Spec: kueue.PreemptionConfigSpec{
					Ordering: []kueue.Order{{OrderingField: kueue.IsOtherCohort}},
					Rules: []kueue.PreemptionRule{
						{
							Name:    "all-cohorts-rule",
							Trigger: kueue.InsufficientQuota,
							Candidates: []kueue.PreemptionCandidateSelector{
								{
									RelationRequirement: kueue.AnyClusterQueue,
								},
							},
						},
					},
				},
			},
			admitted: []kueue.Workload{
				*unitWl.Clone().Name("w-same-cq").SimpleReserveQuota("cq-deep-1", "default", now).Obj(),
				*unitWl.Clone().Name("w-same-cohort-sib").SimpleReserveQuota("cq-deep-2", "default", now).Obj(),
				*unitWl.Clone().Name("w-root-cq").SimpleReserveQuota("cq-root", "default", now).Obj(),
				*unitWl.Clone().Name("w-sib-branch").SimpleReserveQuota("cq-sib-branch", "default", now).Obj(),
				*unitWl.Clone().Name("w-standalone").SimpleReserveQuota("cq-standalone", "default", now).Obj(),
			},
			preemptorWl: unitWl.Clone().Name("incoming").Condition(insufficientQuotaCond).Obj(),
			preemptorCq: "cq-deep-1",
			wantWlOrder: []string{"w-same-cohort-sib", "w-same-cq", "w-root-cq", "w-sib-branch", "w-standalone"},
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

			frsNeedPreemption := sets.New(resources.FlavorResource{Flavor: "default", Resource: corev1.ResourceCPU})
			iter, err := evaluator.Iter(snapshot, wlInfo, frsNeedPreemption)
			if err != nil || tc.wantError != "" {
				gotError := ""
				if err != nil {
					gotError = err.Error()
				}
				if diff := cmp.Diff(tc.wantError, gotError, cmpopts.EquateEmpty()); diff != "" {
					t.Errorf("Iter() error (-want +got):\n%s", diff)
				}
				return
			}

			gotWlOrder := []string{}
			for wlInfo := range iter {
				gotWlOrder = append(gotWlOrder, wlInfo.Obj.Name)

				if len(gotWlOrder) == tc.haltIterationAfterNWorkloads {
					break
				}
			}

			if diff := cmp.Diff(tc.wantWlOrder, gotWlOrder, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("Issued preemptions (-want,+got):\n%s", diff)
			}
		})
	}
}

func Test_preemptionEvaluator_IsAnyTriggerActive(t *testing.T) {
	unitWl := *utiltestingapi.MakeWorkload("unit", "").Request(corev1.ResourceCPU, "1")
	insufficientQuotaCond := metav1.Condition{
		Type:   string(kueue.InsufficientQuota),
		Status: metav1.ConditionTrue,
	}

	tests := map[string]struct {
		config   kueue.PreemptionConfig
		workload *kueue.Workload
		want     bool
		wantErr  bool
	}{
		"empty config": {
			config: kueue.PreemptionConfig{
				Spec: kueue.PreemptionConfigSpec{
					Rules: []kueue.PreemptionRule{},
				},
			},
			workload: unitWl.Clone().Name("a-incoming").Condition(insufficientQuotaCond).Obj(),
			want:     false,
		},
		"active trigger": {
			config: kueue.PreemptionConfig{
				Spec: kueue.PreemptionConfigSpec{
					Rules: []kueue.PreemptionRule{
						{
							Name:    "test",
							Trigger: kueue.InsufficientQuota,
						},
					},
				},
			},
			workload: unitWl.Clone().Name("a-incoming").Condition(insufficientQuotaCond).Obj(),
			want:     true,
		},
		"invalid labels configuration": {
			config: kueue.PreemptionConfig{
				Spec: kueue.PreemptionConfigSpec{
					Rules: []kueue.PreemptionRule{
						{
							Name: "test",
							MatchingPreemptorWorkloads: metav1.LabelSelector{
								MatchExpressions: []metav1.LabelSelectorRequirement{
									{
										Key:      "test",
										Operator: "invalid",
									},
								},
							},
							Trigger: kueue.InsufficientQuota,
						},
					},
				},
			},
			workload: unitWl.Clone().Name("a-incoming").Condition(insufficientQuotaCond).Obj(),
			wantErr:  true,
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			ctx, log := utiltesting.ContextWithLog(t)
			p := NewPreemptionEvaluator(ctx, log, clock.RealClock{}, tc.config, nil)

			wlInfo := workload.NewInfo(tc.workload)
			wlInfo.ClusterQueue = "test-cq"

			got, gotErr := p.IsAnyTriggerActive(wlInfo)
			if (gotErr != nil) != tc.wantErr {
				t.Errorf("IsAnyTriggerActive() error = %v, want error = %v", gotErr != nil, tc.wantErr)
				return
			}
			if got != tc.want {
				t.Errorf("IsAnyTriggerActive() = %v, want %v", got, tc.want)
			}
		})
	}
}
