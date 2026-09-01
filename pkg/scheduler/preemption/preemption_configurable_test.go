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
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	clocktesting "k8s.io/utils/clock/testing"
	"k8s.io/utils/ptr"

	"sigs.k8s.io/kueue/apis/kueue/v1beta2"
	kueue "sigs.k8s.io/kueue/apis/kueue/v1beta2"
	schdcache "sigs.k8s.io/kueue/pkg/cache/scheduler"
	"sigs.k8s.io/kueue/pkg/features"
	"sigs.k8s.io/kueue/pkg/scheduler/flavorassigner"
	preemptexpectations "sigs.k8s.io/kueue/pkg/scheduler/preemption/expectations"
	utilslices "sigs.k8s.io/kueue/pkg/util/slices"
	utiltesting "sigs.k8s.io/kueue/pkg/util/testing"
	utiltestingapi "sigs.k8s.io/kueue/pkg/util/testing/v1beta2"
	"sigs.k8s.io/kueue/pkg/workload"
)

func TestConfigurablePreemptions(t *testing.T) {
	now := time.Now()
	defaultConfigName := "default-config"
	baseCQs := []*kueue.ClusterQueue{
		utiltestingapi.MakeClusterQueue("a").
			Cohort("all").
			ResourceGroup(*utiltestingapi.MakeFlavorQuotas("default").
				Resource(corev1.ResourceCPU, "2").Obj()).
			PreemptionConfigName(defaultConfigName).
			Obj(),
	}

	baseConfig := kueue.PreemptionConfig{
		ObjectMeta: metav1.ObjectMeta{
			Name: defaultConfigName,
		},
		Spec: kueue.PreemptionConfigSpec{
			Rules: []kueue.PreemptionRule{
				{
					Name:    "test-rule-one",
					Trigger: kueue.InsufficientQuota,
					Candidates: []kueue.PreemptionCandidateSelector{
						{
							RelationRequirement: kueue.SameClusterQueue,
						},
					},
				},
			},
		},
	}

	insufficientQuotaCond := metav1.Condition{
		Type:               string(v1beta2.InsufficientQuota),
		Status:             metav1.ConditionTrue,
		LastTransitionTime: metav1.NewTime(now),
	}

	unitWl := *utiltestingapi.MakeWorkload("unit", "").Request(corev1.ResourceCPU, "1")
	cases := map[string]struct {
		clusterQueues []*kueue.ClusterQueue
		cohorts       []*kueue.Cohort
		config        kueue.PreemptionConfig
		admitted      []kueue.Workload
		incoming      *kueue.Workload
		targetCQ      kueue.ClusterQueueReference
		wantPreempted sets.Set[string]
	}{
		"no candidates for CQ without config": {
			clusterQueues: []*kueue.ClusterQueue{
				utiltestingapi.MakeClusterQueue("a").
					ResourceGroup(*utiltestingapi.MakeFlavorQuotas("default").
						Resource(corev1.ResourceCPU, "2").Obj()).
					Obj(),
			},
			config: baseConfig,
			admitted: []kueue.Workload{
				*unitWl.Clone().Name("a1").SimpleReserveQuota("a", "default", now).Obj(),
				*unitWl.Clone().Name("a2").SimpleReserveQuota("a", "default", now).Obj(),
			},
			incoming:      unitWl.Clone().Name("a_incoming").Condition(insufficientQuotaCond).Obj(),
			targetCQ:      "a",
			wantPreempted: sets.New[string](),
		},
		"one workload should be preempted to fit incoming workload": {
			clusterQueues: baseCQs,
			config:        baseConfig,
			admitted: []kueue.Workload{
				*unitWl.Clone().Name("a1").SimpleReserveQuota("a", "default", now).Obj(),
				*unitWl.Clone().Name("a2").SimpleReserveQuota("a", "default", now).Obj(),
			},
			incoming:      unitWl.Clone().Name("a_incoming").Condition(insufficientQuotaCond).Obj(),
			targetCQ:      "a",
			wantPreempted: sets.New("/a1"),
		},
		"multiple workloads should be preempted to fit incoming workload": {
			clusterQueues: baseCQs,
			config:        baseConfig,
			admitted: []kueue.Workload{
				*unitWl.Clone().Name("a1").SimpleReserveQuota("a", "default", now).Obj(),
				*unitWl.Clone().Name("a2").SimpleReserveQuota("a", "default", now).Obj(),
			},
			incoming:      unitWl.Clone().Name("a_incoming").Request(corev1.ResourceCPU, "2").Condition(insufficientQuotaCond).Obj(),
			targetCQ:      "a",
			wantPreempted: sets.New("/a1", "/a2"),
		},
		"incoming workload cannot fit because no matching triggers": {
			clusterQueues: baseCQs,
			config:        baseConfig,
			admitted: []kueue.Workload{
				*unitWl.Clone().Name("a1").SimpleReserveQuota("a", "default", now).Obj(),
				*unitWl.Clone().Name("a2").SimpleReserveQuota("a", "default", now).Obj(),
			},
			incoming: unitWl.Clone().Name("a_incoming").Request(corev1.ResourceCPU, "1").
				Condition(metav1.Condition{
					Type:               string(v1beta2.InsufficientTopology),
					Status:             metav1.ConditionTrue,
					LastTransitionTime: metav1.NewTime(now),
				}).
				Obj(),
			targetCQ:      "a",
			wantPreempted: sets.New[string](),
		},
		"incoming workload cannot fit because configuration doesn't provide enough candidates": {
			clusterQueues: baseCQs,
			config: kueue.PreemptionConfig{
				ObjectMeta: metav1.ObjectMeta{
					Name: defaultConfigName,
				},
				Spec: kueue.PreemptionConfigSpec{
					Rules: []kueue.PreemptionRule{
						{
							Name:    "test-rule-one",
							Trigger: kueue.InsufficientQuota,
							Candidates: []kueue.PreemptionCandidateSelector{
								{
									RelationRequirement: kueue.SameClusterQueue,
									NumericLabels: []kueue.NumericLabelConstraint{
										{
											Key:      "test-label",
											Relation: ptr.To(kueue.Lower),
										},
									},
								},
							},
						},
					},
				},
			},
			admitted: []kueue.Workload{
				*unitWl.Clone().Name("a1").SimpleReserveQuota("a", "default", now).Label("test-label", "9").Obj(),
				*unitWl.Clone().Name("a2").SimpleReserveQuota("a", "default", now).Label("test-label", "1").Obj(),
			},
			incoming:      unitWl.Clone().Name("a_incoming").Request(corev1.ResourceCPU, "2").Label("test-label", "5").Condition(insufficientQuotaCond).Obj(),
			targetCQ:      "a",
			wantPreempted: sets.New[string](),
		},
		"returns no candidates when requested config not found by name": {
			clusterQueues: []*kueue.ClusterQueue{
				utiltestingapi.MakeClusterQueue("a").
					Cohort("all").
					ResourceGroup(*utiltestingapi.MakeFlavorQuotas("default").
						Resource(corev1.ResourceCPU, "2").Obj()).
					PreemptionConfigName("unknown-name").
					Obj(),
			},
			config: baseConfig,
			admitted: []kueue.Workload{
				*unitWl.Clone().Name("a1").SimpleReserveQuota("a", "default", now).Obj(),
				*unitWl.Clone().Name("a2").SimpleReserveQuota("a", "default", now).Obj(),
			},
			incoming:      unitWl.Clone().Name("a_incoming").Condition(insufficientQuotaCond).Obj(),
			targetCQ:      "a",
			wantPreempted: sets.New[string](),
		},
		"returns no candidates when requested config has incorrect parameters": {
			clusterQueues: baseCQs,
			config: kueue.PreemptionConfig{
				ObjectMeta: metav1.ObjectMeta{
					Name: defaultConfigName,
				},
				Spec: kueue.PreemptionConfigSpec{
					Rules: []kueue.PreemptionRule{
						{
							Name:    "test-rule-one",
							Trigger: kueue.InsufficientQuota,
							MatchingPreemptorWorkloads: metav1.LabelSelector{
								MatchExpressions: []metav1.LabelSelectorRequirement{
									{
										Key:      "test",
										Operator: "invalid",
									},
								},
							},
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
				*unitWl.Clone().Name("a1").SimpleReserveQuota("a", "default", now).Obj(),
				*unitWl.Clone().Name("a2").SimpleReserveQuota("a", "default", now).Obj(),
			},
			incoming:      unitWl.Clone().Name("a_incoming").Condition(insufficientQuotaCond).Obj(),
			targetCQ:      "a",
			wantPreempted: sets.New[string](),
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			features.SetFeatureGateDuringTest(t, features.ConfigurablePreemption, true)
			ctx, log := utiltesting.ContextWithLog(t)
			// Set name as UID so that candidates sorting is predictable.
			for i := range tc.admitted {
				tc.admitted[i].UID = types.UID(tc.admitted[i].Name)
			}
			cl := utiltesting.NewClientBuilder().
				WithLists(&kueue.WorkloadList{Items: tc.admitted}).
				WithLists(&kueue.PreemptionConfigList{Items: []kueue.PreemptionConfig{tc.config}}).
				Build()
			cqCache := schdcache.New(cl)
			cqCache.AddOrUpdateResourceFlavor(log, utiltestingapi.MakeResourceFlavor("default").Obj())
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

			recorder := &utiltesting.EventRecorder{}
			preemptor := New(cl, workload.Ordering{}, recorder, nil, false, clocktesting.NewFakeClock(now), nil, preemptexpectations.New(), nil)

			beforeSnapshot, err := cqCache.Snapshot(ctx)
			if err != nil {
				t.Fatalf("unexpected error while building snapshot: %v", err)
			}
			snapshotWorkingCopy, err := cqCache.Snapshot(ctx)
			if err != nil {
				t.Fatalf("unexpected error while building snapshot: %v", err)
			}
			flavorName := kueue.ResourceFlavorReference("default")
			wlInfo := workload.NewInfo(tc.incoming)
			wlInfo.ClusterQueue = tc.targetCQ
			targets := preemptor.GetTargets(ctx, *wlInfo, singlePodSetAssignment(
				flavorassigner.ResourceAssignment{
					corev1.ResourceCPU: &flavorassigner.FlavorAssignment{
						Name: flavorName, Mode: flavorassigner.Preempt,
					},
				},
			), snapshotWorkingCopy)
			gotTargets := sets.New(utilslices.Map(targets, func(t **Target) string {
				return string(workload.Key((*t).WorkloadInfo.Obj))
			})...)
			if diff := cmp.Diff(tc.wantPreempted, gotTargets, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("Issued preemptions (-want,+got):\n%s", diff)
			}

			if diff := cmp.Diff(beforeSnapshot, snapshotWorkingCopy, snapCmpOpts); diff != "" {
				t.Errorf("Snapshot was modified (-initial,+end):\n%s", diff)
			}
		})
	}
}
