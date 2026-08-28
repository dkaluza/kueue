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
	"sigs.k8s.io/kueue/apis/kueue/v1beta2"
	kueue "sigs.k8s.io/kueue/apis/kueue/v1beta2"
	schdcache "sigs.k8s.io/kueue/pkg/cache/scheduler"
	"sigs.k8s.io/kueue/pkg/resources"
	"sigs.k8s.io/kueue/pkg/scheduler/preemption/config/filters"
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
		Type:   string(v1beta2.InsufficientQuota),
		Status: metav1.ConditionTrue,
	}
	quotaReclaimRequiredCond := metav1.Condition{
		Type:   string(v1beta2.QuotaReclaimRequired),
		Status: metav1.ConditionTrue,
	}
	insufficientTopologyCond := metav1.Condition{
		Type:   string(v1beta2.InsufficientTopology),
		Status: metav1.ConditionTrue,
	}

	tests := map[string]struct {
		cohorts       []*kueue.Cohort
		clusterQueues []*kueue.ClusterQueue
		config        v1beta2.PreemptionConfig
		admitted      []kueue.Workload
		preemptorWl   *kueue.Workload
		preemptorCq   kueue.ClusterQueueReference
		wantWlOrder   []string
		wantError     string

		// rare options for full testing coverage
		haltIterationAfterNWorkloads int
	}{
		"no candidates for empty config": {
			clusterQueues: baseCqs,
			config: v1beta2.PreemptionConfig{
				Spec: v1beta2.PreemptionConfigSpec{
					Rules: []v1beta2.PreemptionRule{},
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
			config: v1beta2.PreemptionConfig{
				Spec: v1beta2.PreemptionConfigSpec{
					Rules: []v1beta2.PreemptionRule{
						{
							Name:    "test",
							Trigger: v1beta2.InsufficientQuota,
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
			config: v1beta2.PreemptionConfig{
				Spec: v1beta2.PreemptionConfigSpec{
					Rules: []v1beta2.PreemptionRule{
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
							Trigger: v1beta2.InsufficientQuota,
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
			config: v1beta2.PreemptionConfig{
				Spec: v1beta2.PreemptionConfigSpec{
					Rules: []v1beta2.PreemptionRule{
						{
							Name:    "test",
							Trigger: v1beta2.InsufficientQuota,
							Candidates: []v1beta2.PreemptionCandidateSelector{
								{
									RelationRequirement: v1beta2.SameCohortTree,
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
			config: v1beta2.PreemptionConfig{
				Spec: v1beta2.PreemptionConfigSpec{
					Rules: []v1beta2.PreemptionRule{
						{
							Name:    "test",
							Trigger: v1beta2.InsufficientQuota,
							Candidates: []v1beta2.PreemptionCandidateSelector{
								{
									RelationRequirement: v1beta2.SameCohortTree,
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
			config: v1beta2.PreemptionConfig{
				Spec: v1beta2.PreemptionConfigSpec{
					Rules: []v1beta2.PreemptionRule{
						{
							Name:    "test",
							Trigger: v1beta2.QuotaReclaimRequired,
							Candidates: []v1beta2.PreemptionCandidateSelector{
								{
									RelationRequirement: v1beta2.SameCohortTree,
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
			config: v1beta2.PreemptionConfig{
				Spec: v1beta2.PreemptionConfigSpec{
					Rules: []v1beta2.PreemptionRule{
						{
							Name:    "test",
							Trigger: v1beta2.InsufficientTopology,
							Candidates: []v1beta2.PreemptionCandidateSelector{
								{
									RelationRequirement: v1beta2.SameCohortTree,
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
			config: v1beta2.PreemptionConfig{
				Spec: v1beta2.PreemptionConfigSpec{
					Rules: []v1beta2.PreemptionRule{
						{
							Name:                       "test",
							Trigger:                    v1beta2.InsufficientTopology,
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
			config: v1beta2.PreemptionConfig{
				Spec: v1beta2.PreemptionConfigSpec{
					Rules: []v1beta2.PreemptionRule{
						{
							Name:    "test",
							Trigger: v1beta2.InsufficientTopology,
							MatchingPreemptorWorkloads: metav1.LabelSelector{
								MatchLabels: map[string]string{"active": "true"},
							},
							Candidates: []v1beta2.PreemptionCandidateSelector{
								{
									RelationRequirement: v1beta2.SameCohortTree,
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
			config: v1beta2.PreemptionConfig{
				Spec: v1beta2.PreemptionConfigSpec{
					Rules: []v1beta2.PreemptionRule{
						{
							Name:    "test",
							Trigger: v1beta2.InsufficientTopology,
							MatchingPreemptorWorkloads: metav1.LabelSelector{
								MatchLabels: map[string]string{"active": "true"},
							},
							Candidates: []v1beta2.PreemptionCandidateSelector{
								{
									RelationRequirement: v1beta2.SameCohortTree,
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
			config: v1beta2.PreemptionConfig{
				Spec: v1beta2.PreemptionConfigSpec{
					Rules: []v1beta2.PreemptionRule{
						{
							Name:    "test",
							Trigger: v1beta2.InsufficientTopology,
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
			config: v1beta2.PreemptionConfig{
				Spec: v1beta2.PreemptionConfigSpec{
					Rules: []v1beta2.PreemptionRule{
						{
							Name:    "test",
							Trigger: v1beta2.InsufficientTopology,
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
			config: v1beta2.PreemptionConfig{
				Spec: v1beta2.PreemptionConfigSpec{
					Rules: []v1beta2.PreemptionRule{
						{
							Name:    "test",
							Trigger: v1beta2.InsufficientTopology,
							Candidates: []v1beta2.PreemptionCandidateSelector{
								{
									RelationRequirement: v1beta2.AnyClusterQueue,
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
			config: v1beta2.PreemptionConfig{
				Spec: v1beta2.PreemptionConfigSpec{
					Rules: []v1beta2.PreemptionRule{
						{
							Name:    "topology rule",
							Trigger: v1beta2.InsufficientTopology,
							Candidates: []v1beta2.PreemptionCandidateSelector{
								{
									RelationRequirement: v1beta2.SameClusterQueue,
								},
							},
						},
						{
							Name:    "quota rule",
							Trigger: v1beta2.InsufficientQuota,
							Candidates: []v1beta2.PreemptionCandidateSelector{
								{
									RelationRequirement: v1beta2.SameCohortTree,
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
			config: v1beta2.PreemptionConfig{
				Spec: v1beta2.PreemptionConfigSpec{
					Rules: []v1beta2.PreemptionRule{
						{
							Name:    "topology rule",
							Trigger: v1beta2.InsufficientTopology,
							Candidates: []v1beta2.PreemptionCandidateSelector{
								{
									RelationRequirement: v1beta2.SameCohortTree,
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
			config: v1beta2.PreemptionConfig{
				Spec: v1beta2.PreemptionConfigSpec{
					Rules: []v1beta2.PreemptionRule{
						{
							Name:    "first rule",
							Trigger: v1beta2.InsufficientTopology,
							Candidates: []v1beta2.PreemptionCandidateSelector{
								{
									RelationRequirement: v1beta2.SameClusterQueue,
								},
							},
						},
						{
							Name:    "second rule",
							Trigger: v1beta2.InsufficientTopology,
							Candidates: []v1beta2.PreemptionCandidateSelector{
								{
									RelationRequirement: v1beta2.SameCohortTree,
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

			evaluator := NewPreemptionEvaluator(log, clock.RealClock{}, tc.config, filters.NewCandidateFilters)

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
		Type:   string(v1beta2.InsufficientQuota),
		Status: metav1.ConditionTrue,
	}

	tests := map[string]struct {
		config   kueue.PreemptionConfig
		workload *kueue.Workload
		want     bool
		wantErr  bool
	}{
		"empty config": {
			config: v1beta2.PreemptionConfig{
				Spec: v1beta2.PreemptionConfigSpec{
					Rules: []v1beta2.PreemptionRule{},
				},
			},
			workload: unitWl.Clone().Name("a-incoming").Condition(insufficientQuotaCond).Obj(),
			want:     false,
		},
		"active trigger": {
			config: v1beta2.PreemptionConfig{
				Spec: v1beta2.PreemptionConfigSpec{
					Rules: []v1beta2.PreemptionRule{
						{
							Name:    "test",
							Trigger: v1beta2.InsufficientQuota,
						},
					},
				},
			},
			workload: unitWl.Clone().Name("a-incoming").Condition(insufficientQuotaCond).Obj(),
			want:     true,
		},
		"invalid labels configuration": {
			config: v1beta2.PreemptionConfig{
				Spec: v1beta2.PreemptionConfigSpec{
					Rules: []v1beta2.PreemptionRule{
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
							Trigger: v1beta2.InsufficientQuota,
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
			_, log := utiltesting.ContextWithLog(t)
			p := NewPreemptionEvaluator(log, clock.RealClock{}, tc.config, filters.NewCandidateFilters)

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
