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

	schdcache "sigs.k8s.io/kueue/pkg/cache/scheduler"
	configtesting "sigs.k8s.io/kueue/pkg/scheduler/preemption/config/testing"
	utiltestingapi "sigs.k8s.io/kueue/pkg/util/testing/v1beta2"
	"sigs.k8s.io/kueue/pkg/workload"
)

func TestClusterQueueRelationFilters(t *testing.T) {
	// Hierarchy topology:
	//                 rootA (Root Cohort)                     rootB (Root Cohort)
	//              /           |             \                          |
	// cqDirectRootA          subA1          subA2                     subB
	//                      /   |     \        |                         |
	//              cq1SubA1 cq2SubA1 subSubA cq3SubA2                cq4SubB
	//                                  |
	//                            cqDeepSubSubA
	//
	// Standalone CQs (no cohort):
	// - cqStandalone1
	// - cqStandalone2
	snapshot := configtesting.NewSnapshotBuilder().
		Cohort("rootA", "").
		Cohort("subA1", "rootA").
		Cohort("subSubA", "subA1").
		Cohort("subA2", "rootA").
		Cohort("rootB", "").
		Cohort("subB", "rootB").
		ClusterQueue("cqDirectRootA", "rootA").
		ClusterQueue("cq1SubA1", "subA1").
		ClusterQueue("cq2SubA1", "subA1").
		ClusterQueue("cqDeepSubSubA", "subSubA").
		ClusterQueue("cq3SubA2", "subA2").
		ClusterQueue("cq4SubB", "subB").
		ClusterQueue("cqStandalone1", "").
		ClusterQueue("cqStandalone2", "").
		Build()

	cq1SubA1 := snapshot.ClusterQueue("cq1SubA1")
	cq2SubA1 := snapshot.ClusterQueue("cq2SubA1")
	cqDeepSubSubA := snapshot.ClusterQueue("cqDeepSubSubA")
	cqDirectRootA := snapshot.ClusterQueue("cqDirectRootA")
	cq3SubA2 := snapshot.ClusterQueue("cq3SubA2")
	cq4SubB := snapshot.ClusterQueue("cq4SubB")
	cqStandalone1 := snapshot.ClusterQueue("cqStandalone1")
	cqStandalone2 := snapshot.ClusterQueue("cqStandalone2")

	cases := map[string]struct {
		filter      ClusterQueueFilter
		candidateCQ *schdcache.ClusterQueueSnapshot
		wantMatch   bool
	}{
		// 1. SameClusterQueue Filter Tests
		"SameClusterQueue: matching target CQ": {
			filter:      NewSameClusterQueueFilter("cq1SubA1"),
			candidateCQ: cq1SubA1,
			wantMatch:   true,
		},
		"SameClusterQueue: different sibling CQ in same cohort rejected": {
			filter:      NewSameClusterQueueFilter("cq1SubA1"),
			candidateCQ: cq2SubA1,
			wantMatch:   false,
		},
		"SameClusterQueue: different CQ in disjoint tree rejected": {
			filter:      NewSameClusterQueueFilter("cq1SubA1"),
			candidateCQ: cq4SubB,
			wantMatch:   false,
		},
		"SameClusterQueue: standalone CQ matches itself": {
			filter:      NewSameClusterQueueFilter("cqStandalone1"),
			candidateCQ: cqStandalone1,
			wantMatch:   true,
		},
		"SameClusterQueue: standalone CQ rejects another standalone CQ": {
			filter:      NewSameClusterQueueFilter("cqStandalone1"),
			candidateCQ: cqStandalone2,
			wantMatch:   false,
		},

		// 2. SameCohort Filter Tests
		"SameCohort: sibling CQ in same immediate parent cohort matches": {
			filter:      NewSameCohortFilter("cq1SubA1", snapshot),
			candidateCQ: cq2SubA1,
			wantMatch:   true,
		},
		"SameCohort: candidate in exact same CQ as preemptor matches": {
			filter:      NewSameCohortFilter("cq1SubA1", snapshot),
			candidateCQ: cq1SubA1,
			wantMatch:   true,
		},
		"SameCohort: sub-cohort child under same immediate parent rejected": {
			filter:      NewSameCohortFilter("cq1SubA1", snapshot),
			candidateCQ: cqDeepSubSubA,
			wantMatch:   false,
		},
		"SameCohort: sibling sub-cohort under same root rejected": {
			filter:      NewSameCohortFilter("cq1SubA1", snapshot),
			candidateCQ: cq3SubA2,
			wantMatch:   false,
		},
		"SameCohort: direct child of root cohort rejected when preemptor is in sub-cohort": {
			filter:      NewSameCohortFilter("cq1SubA1", snapshot),
			candidateCQ: cqDirectRootA,
			wantMatch:   false,
		},
		"SameCohort: candidate in disjoint cohort tree rejected": {
			filter:      NewSameCohortFilter("cq1SubA1", snapshot),
			candidateCQ: cq4SubB,
			wantMatch:   false,
		},
		"SameCohort: standalone candidate rejected for preemptor with cohort": {
			filter:      NewSameCohortFilter("cq1SubA1", snapshot),
			candidateCQ: cqStandalone1,
			wantMatch:   false,
		},
		"SameCohort: preemptor directly under root matches itself": {
			filter:      NewSameCohortFilter("cqDirectRootA", snapshot),
			candidateCQ: cqDirectRootA,
			wantMatch:   true,
		},
		"SameCohort: preemptor directly under root rejects sub-cohort child": {
			filter:      NewSameCohortFilter("cqDirectRootA", snapshot),
			candidateCQ: cq1SubA1,
			wantMatch:   false,
		},
		"SameCohort: standalone preemptor matches candidate in its own CQ": {
			filter:      NewSameCohortFilter("cqStandalone1", snapshot),
			candidateCQ: cqStandalone1,
			wantMatch:   true,
		},
		"SameCohort: standalone preemptor rejects candidate in another standalone CQ": {
			filter:      NewSameCohortFilter("cqStandalone1", snapshot),
			candidateCQ: cqStandalone2,
			wantMatch:   false,
		},
		"SameCohort: standalone preemptor rejects candidate in a cohort CQ": {
			filter:      NewSameCohortFilter("cqStandalone1", snapshot),
			candidateCQ: cq1SubA1,
			wantMatch:   false,
		},

		// 3. SameCohortTree Filter Tests
		"SameCohortTree: sibling CQ in same immediate cohort matches": {
			filter:      NewSameCohortTreeFilter("cq1SubA1", snapshot),
			candidateCQ: cq2SubA1,
			wantMatch:   true,
		},
		"SameCohortTree: candidate in sibling sub-cohort sharing root matches": {
			filter:      NewSameCohortTreeFilter("cq1SubA1", snapshot),
			candidateCQ: cq3SubA2,
			wantMatch:   true,
		},
		"SameCohortTree: candidate in 3-level deep sub-cohort sharing root matches": {
			filter:      NewSameCohortTreeFilter("cq1SubA1", snapshot),
			candidateCQ: cqDeepSubSubA,
			wantMatch:   true,
		},
		"SameCohortTree: candidate directly under root cohort matches": {
			filter:      NewSameCohortTreeFilter("cq1SubA1", snapshot),
			candidateCQ: cqDirectRootA,
			wantMatch:   true,
		},
		"SameCohortTree: candidate in exact same CQ as preemptor matches": {
			filter:      NewSameCohortTreeFilter("cq1SubA1", snapshot),
			candidateCQ: cq1SubA1,
			wantMatch:   true,
		},
		"SameCohortTree: candidate in disjoint cohort tree rejected": {
			filter:      NewSameCohortTreeFilter("cq1SubA1", snapshot),
			candidateCQ: cq4SubB,
			wantMatch:   false,
		},
		"SameCohortTree: preemptor in rootB matches itself": {
			filter:      NewSameCohortTreeFilter("cq4SubB", snapshot),
			candidateCQ: cq4SubB,
			wantMatch:   true,
		},
		"SameCohortTree: preemptor in rootB rejects candidate in rootA": {
			filter:      NewSameCohortTreeFilter("cq4SubB", snapshot),
			candidateCQ: cq1SubA1,
			wantMatch:   false,
		},
		"SameCohortTree: standalone candidate rejected for preemptor with cohort tree": {
			filter:      NewSameCohortTreeFilter("cq1SubA1", snapshot),
			candidateCQ: cqStandalone1,
			wantMatch:   false,
		},
		"SameCohortTree: standalone preemptor matches candidate in its own CQ": {
			filter:      NewSameCohortTreeFilter("cqStandalone1", snapshot),
			candidateCQ: cqStandalone1,
			wantMatch:   true,
		},
		"SameCohortTree: standalone preemptor rejects candidate in another standalone CQ": {
			filter:      NewSameCohortTreeFilter("cqStandalone1", snapshot),
			candidateCQ: cqStandalone2,
			wantMatch:   false,
		},
		"SameCohortTree: standalone preemptor rejects candidate in cohort tree": {
			filter:      NewSameCohortTreeFilter("cqStandalone1", snapshot),
			candidateCQ: cq1SubA1,
			wantMatch:   false,
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			gotMatch := tc.filter.Matches(tc.candidateCQ)
			if gotMatch != tc.wantMatch {
				t.Errorf("Matches() = %v, want %v", gotMatch, tc.wantMatch)
			}
		})
	}
}

func TestSameLocalQueueFilter(t *testing.T) {
	filter := NewSameLocalQueueFilter("ns1", "lq1")

	cases := map[string]struct {
		candidate *workload.Info
		wantMatch bool
	}{
		"matching namespace and queue name": {
			candidate: workload.NewInfo(utiltestingapi.MakeWorkload("c-exact", "ns1").Queue("lq1").Obj()),
			wantMatch: true,
		},
		"different local queue name rejected": {
			candidate: workload.NewInfo(utiltestingapi.MakeWorkload("c-diff-lq", "ns1").Queue("lq2").Obj()),
			wantMatch: false,
		},
		"different namespace rejected": {
			candidate: workload.NewInfo(utiltestingapi.MakeWorkload("c-diff-ns", "ns2").Queue("lq1").Obj()),
			wantMatch: false,
		},
		"different namespace and queue name rejected": {
			candidate: workload.NewInfo(utiltestingapi.MakeWorkload("c-diff-both", "ns2").Queue("lq2").Obj()),
			wantMatch: false,
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			gotMatch := filter.Matches(tc.candidate)
			if gotMatch != tc.wantMatch {
				t.Errorf("Matches() = %v, want %v", gotMatch, tc.wantMatch)
			}
		})
	}
}
