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

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/kueue/apis/kueue/v1beta2"
	kueue "sigs.k8s.io/kueue/apis/kueue/v1beta2"
	schdcache "sigs.k8s.io/kueue/pkg/cache/scheduler"
	utiltesting "sigs.k8s.io/kueue/pkg/util/testing"
	utiltestingapi "sigs.k8s.io/kueue/pkg/util/testing/v1beta2"
	"sigs.k8s.io/kueue/pkg/workload"
)

func TestPreemptionEvaluatorIterationOrder(t *testing.T) {
	tests := map[string]struct {
		candidates    []*kueue.Workload
		wantNameOrder []string
	}{
		"empty order for empty candidates": {
			candidates:    []*kueue.Workload{},
			wantNameOrder: []string{},
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			workloadInfos := []*workload.Info{}
			for _, wl := range tc.candidates {
				workloadInfos = append(workloadInfos, workload.NewInfo(wl))
			}

			p := newPreemptionEvaluator(workloadInfos)

			var gotNameOrder []string
			for wlInfo := range p.Iter() {
				gotNameOrder = append(gotNameOrder, wlInfo.Obj.Name)
			}

			if diff := cmp.Diff(tc.wantNameOrder, gotNameOrder, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("preemptionEvaluator.Iter() (-want,+got):\n%s", diff)
			}
		})
	}
}

func TestFindCandidates(t *testing.T) {
	flavors := []*kueue.ResourceFlavor{
		utiltestingapi.MakeResourceFlavor("default").Obj(),
	}

	tests := map[string]struct {
		config             v1beta2.PreemptionConfig
		cohorts            []*kueue.Cohort
		clusterQueues      []*kueue.ClusterQueue
		admitted           []kueue.Workload
		targetCQ           kueue.ClusterQueueReference
		wantCandidateNames []string
	}{
		"no candidates for empty input": {
			clusterQueues: []*kueue.ClusterQueue{
				utiltestingapi.MakeClusterQueue("a").
					Cohort("all").
					ResourceGroup(*utiltestingapi.MakeFlavorQuotas("default").
						Resource(corev1.ResourceCPU, "3").Obj()).
					Obj(),
			},
			config: v1beta2.PreemptionConfig{
				Spec: v1beta2.PreemptionConfigSpec{
					Rules: []v1beta2.PreemptionRule{},
				},
			},
			admitted:           []kueue.Workload{},
			targetCQ:           "a",
			wantCandidateNames: []string{},
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
			for _, flv := range flavors {
				cqCache.AddOrUpdateResourceFlavor(log, flv)
			}
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

			got := findCandidates(tc.config, snapshot.ClusterQueue(tc.targetCQ))
			gotNames := []string{}
			for _, wlInfo := range got {
				gotNames = append(gotNames, wlInfo.Obj.Name)
			}
			if diff := cmp.Diff(tc.wantCandidateNames, gotNames, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("findCandidates() = %v, want %v", got, tc.wantCandidateNames)
			}
		})
	}
}
