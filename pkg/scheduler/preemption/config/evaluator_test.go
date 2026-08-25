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

	"github.com/go-logr/logr"
	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/utils/clock"
	"sigs.k8s.io/kueue/apis/kueue/v1beta2"
	kueue "sigs.k8s.io/kueue/apis/kueue/v1beta2"
	schdcache "sigs.k8s.io/kueue/pkg/cache/scheduler"
	"sigs.k8s.io/kueue/pkg/resources"
	"sigs.k8s.io/kueue/pkg/scheduler/preemption/config/selectors"
	utiltesting "sigs.k8s.io/kueue/pkg/util/testing"
	utiltestingapi "sigs.k8s.io/kueue/pkg/util/testing/v1beta2"
	"sigs.k8s.io/kueue/pkg/workload"
)

func TestPreemptionEvaluatorIter(t *testing.T) {
	now := time.Now()
	unitWl := *utiltestingapi.MakeWorkload("unit", "").Request(corev1.ResourceCPU, "1")

	tests := map[string]struct {
		config             v1beta2.PreemptionConfig
		cohorts            []*kueue.Cohort
		clusterQueues      []*kueue.ClusterQueue
		admitted           []kueue.Workload
		preemptorWl        *kueue.Workload
		preemptorCq        kueue.ClusterQueueReference
		wantCandidateNames []string
	}{
		"no candidates for empty config": {
			clusterQueues: []*kueue.ClusterQueue{
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
			},
			config: v1beta2.PreemptionConfig{
				Spec: v1beta2.PreemptionConfigSpec{
					Rules: []v1beta2.PreemptionRule{},
				},
			},
			admitted: []kueue.Workload{
				*unitWl.Clone().Name("a1").SimpleReserveQuota("a", "default", now).Obj(),
				*unitWl.Clone().Name("a2").SimpleReserveQuota("a", "default", now).Obj(),
			},
			preemptorWl:        unitWl.Clone().Name("wl1").Request(corev1.ResourceCPU, "1").Obj(),
			preemptorCq:        "b",
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

			snapshot, err := cqCache.Snapshot(ctx)
			if err != nil {
				t.Fatalf("unexpected error while building snapshot: %v", err)
			}

			evaluator := NewPreemptionEvaluator(log, clock.RealClock{}, tc.config, func(rules []v1beta2.PreemptionRule) selectors.CandidateSelector {
				return &echoCandidateSelector{}
			})

			preemptorCqSnapshot := snapshot.ClusterQueue(tc.preemptorCq)

			wlInfo := workload.NewInfo(tc.preemptorWl)
			wlInfo.ClusterQueue = tc.preemptorCq

			iter, err := evaluator.Iter(wlInfo, preemptorCqSnapshot, sets.New(resources.FlavorResource{Flavor: "default", Resource: corev1.ResourceCPU}))
			if err != nil {
				t.Fatalf("unexpected error while creating iterator: %v", err)
			}

			gotNames := []string{}
			for wlInfo := range iter {
				gotNames = append(gotNames, wlInfo.Obj.Name)
			}

			if diff := cmp.Diff(tc.wantCandidateNames, gotNames, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("Issued preemptions (-want,+got):\n%s", diff)
			}
		})
	}
}

type echoCandidateSelector struct {
}

func (e *echoCandidateSelector) Filter(_ logr.Logger, _ *workload.Info, candidates []*workload.Info) []*workload.Info {
	return candidates
}
