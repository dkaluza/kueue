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
	"testing"

	"github.com/go-logr/logr"
	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/utils/ptr"

	kueue "sigs.k8s.io/kueue/apis/kueue/v1beta2"
	configtesting "sigs.k8s.io/kueue/pkg/scheduler/preemption/config/testing"
	utiltesting "sigs.k8s.io/kueue/pkg/util/testing"
	utiltestingapi "sigs.k8s.io/kueue/pkg/util/testing/v1beta2"
	"sigs.k8s.io/kueue/pkg/workload"
)

func makeWorkloadInfo(w *kueue.Workload, cq kueue.ClusterQueueReference) *workload.Info {
	info := workload.NewInfo(w)
	info.ClusterQueue = cq
	return info
}

func TestNewCandidateFilters(t *testing.T) {
	// Minimal snapshot required by constructor for resolving preemptor's cohort ancestors:
	// rootA -> subA1 -> cq1
	snapshot := configtesting.NewSnapshotBuilder().
		Cohort("rootA", "").
		Cohort("subA1", "rootA").
		ClusterQueue("cq1", "subA1").
		Build()

	clientReader := utiltesting.NewFakeClient(
		utiltestingapi.MakeWorkloadPriorityClass("wpc-critical").Label("tier", "critical-training").Obj(),
		utiltestingapi.MakeWorkloadPriorityClass("wpc-batch").Label("tier", "batch").Obj(),
	)

	preemptor := makeWorkloadInfo(utiltestingapi.MakeWorkload("preemptor", "ns1").
		Queue("lq1").
		Label("tpu-size", "8").
		Priority(100).
		Obj(), "cq1")

	preemptorWithPC := makeWorkloadInfo(utiltestingapi.MakeWorkload("preemptorWithPC", "ns1").
		Queue("lq1").
		Label("tpu-size", "8").
		Priority(100).
		WorkloadPriorityClassRef("wpc-critical").
		Obj(), "cq1")

	preemptorWithBatchPC := makeWorkloadInfo(utiltestingapi.MakeWorkload("preemptorWithBatchPC", "ns1").
		Queue("lq1").
		WorkloadPriorityClassRef("wpc-batch").
		Obj(), "cq1")

	candSelectorPreemptible, err := metav1.LabelSelectorAsSelector(&metav1.LabelSelector{
		MatchLabels: map[string]string{"preemptible": "true"},
	})
	if err != nil {
		t.Fatalf("Failed to parse label selector: %v", err)
	}

	cases := map[string]struct {
		selector      *kueue.PreemptionCandidateSelector
		preemptor     *workload.Info
		wantFilters   CandidateFilters
		wantRejectAll bool
	}{
		"nil selector returns empty CandidateFilters": {
			selector:    nil,
			preemptor:   preemptor,
			wantFilters: CandidateFilters{},
		},
		"SameLocalQueue instantiates sameClusterQueueFilter and sameLocalQueueFilter": {
			selector: &kueue.PreemptionCandidateSelector{
				RelationRequirement: kueue.SameLocalQueue,
			},
			preemptor: preemptor,
			wantFilters: CandidateFilters{
				CQFilters: []ClusterQueueFilter{
					&sameClusterQueueFilter{preemptorCQ: "cq1"},
				},
				WLFilters: []WorkloadFilter{
					&sameLocalQueueFilter{namespace: "ns1", queueName: "lq1"},
				},
			},
		},
		"SameClusterQueue instantiates sameClusterQueueFilter": {
			selector: &kueue.PreemptionCandidateSelector{
				RelationRequirement: kueue.SameClusterQueue,
			},
			preemptor: preemptor,
			wantFilters: CandidateFilters{
				CQFilters: []ClusterQueueFilter{
					&sameClusterQueueFilter{preemptorCQ: "cq1"},
				},
			},
		},
		"SameCohort resolves immediate parent cohort from snapshot": {
			selector: &kueue.PreemptionCandidateSelector{
				RelationRequirement: kueue.SameCohort,
			},
			preemptor: preemptor,
			wantFilters: CandidateFilters{
				CQFilters: []ClusterQueueFilter{
					&sameCohortFilter{
						preemptorCQ:     "cq1",
						preemptorCohort: "subA1",
						hasCohort:       true,
					},
				},
			},
		},
		"SameCohortTree resolves root ancestor cohort from snapshot": {
			selector: &kueue.PreemptionCandidateSelector{
				RelationRequirement: kueue.SameCohortTree,
			},
			preemptor: preemptor,
			wantFilters: CandidateFilters{
				CQFilters: []ClusterQueueFilter{
					&sameCohortTreeFilter{
						preemptorCQ:         "cq1",
						preemptorRootCohort: "rootA",
						hasCohort:           true,
					},
				},
			},
		},
		"AnyClusterQueue results in empty filters": {
			selector: &kueue.PreemptionCandidateSelector{
				RelationRequirement: kueue.AnyClusterQueue,
			},
			preemptor:   preemptor,
			wantFilters: CandidateFilters{},
		},
		"unrecognized relation requirement returns rejectAll true": {
			selector: &kueue.PreemptionCandidateSelector{
				RelationRequirement: kueue.PreemptionRelationConstraint("UnknownRelation"),
			},
			preemptor:     preemptor,
			wantFilters:   CandidateFilters{},
			wantRejectAll: true,
		},
		"SameClusterQueue with empty NumericLabels produces no WorkloadFilters": {
			selector: &kueue.PreemptionCandidateSelector{
				RelationRequirement: kueue.SameClusterQueue,
				NumericLabels:       []kueue.NumericLabelConstraint{},
			},
			preemptor: preemptor,
			wantFilters: CandidateFilters{
				CQFilters: []ClusterQueueFilter{
					&sameClusterQueueFilter{preemptorCQ: "cq1"},
				},
				WLFilters: nil,
			},
		},
		"Combined SameLocalQueue and NumericLabelConstraints appends both relation and numeric WorkloadFilters": {
			selector: &kueue.PreemptionCandidateSelector{
				RelationRequirement: kueue.SameLocalQueue,
				NumericLabels: []kueue.NumericLabelConstraint{
					{
						Key:          "tpu-size",
						DefaultValue: ptr.To[int32](1),
						Relation:     ptr.To(kueue.LowerOrEqual),
					},
					{
						Key:      "priority-boost",
						MinValue: ptr.To[int32](10),
					},
				},
			},
			preemptor: preemptor,
			wantFilters: CandidateFilters{
				CQFilters: []ClusterQueueFilter{
					&sameClusterQueueFilter{preemptorCQ: "cq1"},
				},
				WLFilters: []WorkloadFilter{
					&sameLocalQueueFilter{
						namespace: "ns1",
						queueName: "lq1",
					},
					&numericLabelFilter{
						constraint: kueue.NumericLabelConstraint{
							Key:          "tpu-size",
							DefaultValue: ptr.To[int32](1),
							Relation:     ptr.To(kueue.LowerOrEqual),
						},
						preemptorVal: ptr.To[int32](8),
					},
					&numericLabelFilter{
						constraint: kueue.NumericLabelConstraint{
							Key:      "priority-boost",
							MinValue: ptr.To[int32](10),
						},
						preemptorVal: nil,
					},
				},
			},
		},
		"PreemptingWorkloadPrioritySelector matching preemptor proceeds with compilation": {
			selector: &kueue.PreemptionCandidateSelector{
				RelationRequirement: kueue.SameClusterQueue,
				PreemptingWorkloadPrioritySelector: &metav1.LabelSelector{
					MatchLabels: map[string]string{"tier": "critical-training"},
				},
			},
			preemptor: preemptorWithPC,
			wantFilters: CandidateFilters{
				CQFilters: []ClusterQueueFilter{
					&sameClusterQueueFilter{preemptorCQ: "cq1"},
				},
			},
		},
		"PreemptingWorkloadPrioritySelector not matching preemptor returns rejectAll true": {
			selector: &kueue.PreemptionCandidateSelector{
				RelationRequirement: kueue.SameClusterQueue,
				PreemptingWorkloadPrioritySelector: &metav1.LabelSelector{
					MatchLabels: map[string]string{"tier": "critical-training"},
				},
			},
			preemptor:     preemptorWithBatchPC,
			wantFilters:   CandidateFilters{},
			wantRejectAll: true,
		},
		"CandidateWorkloadPrioritySelector and RelativeWorkloadPriority are compiled into WLFilters": {
			selector: &kueue.PreemptionCandidateSelector{
				RelationRequirement: kueue.SameClusterQueue,
				CandidateWorkloadPrioritySelector: &metav1.LabelSelector{
					MatchLabels: map[string]string{"preemptible": "true"},
				},
				RelativeWorkloadPriority: ptr.To(kueue.LowerOrEqual),
			},
			preemptor: preemptorWithPC,
			wantFilters: CandidateFilters{
				CQFilters: []ClusterQueueFilter{
					&sameClusterQueueFilter{preemptorCQ: "cq1"},
				},
				WLFilters: []WorkloadFilter{
					&candidateWorkloadPriorityFilter{
						selector: candSelectorPreemptible,
					},
					&relativeWorkloadPriorityFilter{
						relation:          kueue.LowerOrEqual,
						preemptorPriority: 100,
					},
				},
			},
		},
		"CandidateWorkloadPrioritySelector with invalid selector returns rejectAll true": {
			selector: &kueue.PreemptionCandidateSelector{
				RelationRequirement: kueue.SameClusterQueue,
				CandidateWorkloadPrioritySelector: &metav1.LabelSelector{
					MatchExpressions: []metav1.LabelSelectorRequirement{
						{Key: "tier", Operator: metav1.LabelSelectorOperator("InvalidOp")},
					},
				},
			},
			preemptor:     preemptorWithPC,
			wantFilters:   CandidateFilters{},
			wantRejectAll: true,
		},
		"Full combination of all selector criteria compiles into complete CandidateFilters": {
			selector: &kueue.PreemptionCandidateSelector{
				RelationRequirement: kueue.SameCohort,
				PreemptingWorkloadPrioritySelector: &metav1.LabelSelector{
					MatchLabels: map[string]string{"tier": "critical-training"},
				},
				CandidateWorkloadPrioritySelector: &metav1.LabelSelector{
					MatchLabels: map[string]string{"preemptible": "true"},
				},
				RelativeWorkloadPriority: ptr.To(kueue.Lower),
				NumericLabels: []kueue.NumericLabelConstraint{
					{
						Key:      "tpu-size",
						Relation: ptr.To(kueue.Lower),
					},
				},
			},
			preemptor: preemptorWithPC,
			wantFilters: CandidateFilters{
				CQFilters: []ClusterQueueFilter{
					&sameCohortFilter{
						preemptorCQ:     "cq1",
						preemptorCohort: "subA1",
						hasCohort:       true,
					},
				},
				WLFilters: []WorkloadFilter{
					&numericLabelFilter{
						constraint: kueue.NumericLabelConstraint{
							Key:      "tpu-size",
							Relation: ptr.To(kueue.Lower),
						},
						preemptorVal: ptr.To[int32](8),
					},
					&candidateWorkloadPriorityFilter{
						selector: candSelectorPreemptible,
					},
					&relativeWorkloadPriorityFilter{
						relation:          kueue.Lower,
						preemptorPriority: 100,
					},
				},
			},
		},
		"SameClusterQueue with RelativeWorkloadPriority compiles both CQ and WL priority filters": {
			selector: &kueue.PreemptionCandidateSelector{
				RelationRequirement:      kueue.SameClusterQueue,
				RelativeWorkloadPriority: ptr.To(kueue.Lower),
			},
			preemptor: preemptor,
			wantFilters: CandidateFilters{
				CQFilters: []ClusterQueueFilter{
					&sameClusterQueueFilter{preemptorCQ: "cq1"},
				},
				WLFilters: []WorkloadFilter{
					&relativeWorkloadPriorityFilter{
						relation:          kueue.Lower,
						preemptorPriority: 100,
					},
				},
			},
		},
		"Combined SameLocalQueue, NumericLabels, and RelativeWorkloadPriority compiles all filters": {
			selector: &kueue.PreemptionCandidateSelector{
				RelationRequirement: kueue.SameLocalQueue,
				NumericLabels: []kueue.NumericLabelConstraint{
					{
						Key:          "tpu-size",
						DefaultValue: ptr.To[int32](1),
						Relation:     ptr.To(kueue.LowerOrEqual),
					},
				},
				RelativeWorkloadPriority: ptr.To(kueue.LowerOrEqual),
			},
			preemptor: preemptor,
			wantFilters: CandidateFilters{
				CQFilters: []ClusterQueueFilter{
					&sameClusterQueueFilter{preemptorCQ: "cq1"},
				},
				WLFilters: []WorkloadFilter{
					&sameLocalQueueFilter{
						namespace: "ns1",
						queueName: "lq1",
					},
					&numericLabelFilter{
						constraint: kueue.NumericLabelConstraint{
							Key:          "tpu-size",
							DefaultValue: ptr.To[int32](1),
							Relation:     ptr.To(kueue.LowerOrEqual),
						},
						preemptorVal: ptr.To[int32](8),
					},
					&relativeWorkloadPriorityFilter{
						relation:          kueue.LowerOrEqual,
						preemptorPriority: 100,
					},
				},
			},
		},
	}

	cmpOptions := []cmp.Option{
		cmp.AllowUnexported(
			sameClusterQueueFilter{},
			sameCohortFilter{},
			sameCohortTreeFilter{},
			sameLocalQueueFilter{},
			numericLabelFilter{},
			candidateWorkloadPriorityFilter{},
			relativeWorkloadPriorityFilter{},
		),
		cmpopts.IgnoreFields(numericLabelFilter{}, "log"),
		cmpopts.IgnoreFields(candidateWorkloadPriorityFilter{}, "ctx", "log", "reader"),
		cmpopts.IgnoreFields(relativeWorkloadPriorityFilter{}, "log"),
		cmp.Comparer(func(a, b labels.Selector) bool {
			if a == nil && b == nil {
				return true
			}
			if a == nil || b == nil {
				return false
			}
			return a.String() == b.String()
		}),
		cmpopts.EquateEmpty(),
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			gotFilters, gotRejectAll := NewCandidateFilters(t.Context(), logr.Discard(), tc.selector, tc.preemptor, snapshot, clientReader)
			if gotRejectAll != tc.wantRejectAll {
				t.Errorf("NewCandidateFilters() rejectAll = %v, want %v", gotRejectAll, tc.wantRejectAll)
			}
			if diff := cmp.Diff(tc.wantFilters, gotFilters, cmpOptions...); diff != "" {
				t.Errorf("NewCandidateFilters() mismatch (-want +got):\n%s", diff)
			}
		})
	}
}
