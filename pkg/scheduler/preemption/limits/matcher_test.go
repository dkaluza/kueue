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

package limits

import (
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	kueuealpha "sigs.k8s.io/kueue/apis/kueue/v1alpha1"
	schdcache "sigs.k8s.io/kueue/pkg/cache/scheduler"
	"sigs.k8s.io/kueue/pkg/scheduler/preemption/policy"
	utiltestingalpha "sigs.k8s.io/kueue/pkg/util/testing/v1alpha1"
	utiltestingapi "sigs.k8s.io/kueue/pkg/util/testing/v1beta2"
	"sigs.k8s.io/kueue/pkg/workload"
)

func TestNewPreemptionLimitMatcher(t *testing.T) {
	config := utiltestingalpha.MakePreemptionConfig("config").Label("tier", "gold").Obj()
	selectsGold := &metav1.LabelSelector{MatchLabels: map[string]string{"tier": "gold"}}
	selectsSilver := &metav1.LabelSelector{MatchLabels: map[string]string{"tier": "silver"}}
	invalidSelector := &metav1.LabelSelector{
		MatchExpressions: []metav1.LabelSelectorRequirement{{Key: "tier", Operator: "invalid"}},
	}
	limit := utiltestingalpha.MakePreemptionLimit("limit", kueuealpha.GlobalPreemptionLimitScope)

	cases := map[string]struct {
		limit       *kueuealpha.PreemptionLimit
		wantApplies bool
		// wantErr, when set, is the expected prefix of the error message.
		wantErr string
	}{
		"limit without configSelector applies": {
			limit:       limit.Clone().Obj(),
			wantApplies: true,
		},
		"limit whose configSelector selects the config applies": {
			limit:       limit.Clone().ConfigSelector(selectsGold).Obj(),
			wantApplies: true,
		},
		"limit whose configSelector doesn't select the config doesn't apply": {
			limit: limit.Clone().ConfigSelector(selectsSilver).Obj(),
		},
		"invalid configSelector is an error": {
			limit:   limit.Clone().ConfigSelector(invalidSelector).Obj(),
			wantErr: `preemptionLimit "limit" configSelector: `,
		},
		"invalid clusterQueueSelector of a limit applying to the config is an error": {
			limit:   limit.Clone().ClusterQueueSelector(invalidSelector).Obj(),
			wantErr: `preemptionLimit "limit" clusterQueueSelector: `,
		},
		"invalid clusterQueueSelector of a limit not applying to the config is ignored": {
			limit: limit.Clone().ConfigSelector(selectsSilver).ClusterQueueSelector(invalidSelector).Obj(),
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			matcher, err := newPreemptionLimitMatcher(tc.limit, config)
			if tc.wantErr != "" {
				if err == nil || !strings.HasPrefix(err.Error(), tc.wantErr) {
					t.Fatalf("newPreemptionLimitMatcher() error = %v, want error starting with %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("newPreemptionLimitMatcher() unexpected error: %v", err)
			}
			if applies := matcher != nil; applies != tc.wantApplies {
				t.Fatalf("newPreemptionLimitMatcher() = %v, want a matcher: %v", matcher, tc.wantApplies)
			}
			if matcher != nil && matcher.limit != tc.limit {
				t.Errorf("newPreemptionLimitMatcher() limit = %v, want %v", matcher.limit, tc.limit)
			}
		})
	}
}

func TestPreemptionLimitMatcherMatchesClusterQueues(t *testing.T) {
	preemptorCQ := &schdcache.ClusterQueueSnapshot{Name: "preempting-cq", Labels: map[string]string{"side": "preempting"}}
	targetCQ := &schdcache.ClusterQueueSnapshot{Name: "preempted-cq", Labels: map[string]string{"side": "preempted"}}
	selectsPreempting := &metav1.LabelSelector{MatchLabels: map[string]string{"side": "preempting"}}
	selectsPreempted := &metav1.LabelSelector{MatchLabels: map[string]string{"side": "preempted"}}
	selectsOther := &metav1.LabelSelector{MatchLabels: map[string]string{"side": "other"}}

	cases := map[string]struct {
		scope                kueuealpha.PreemptionLimitScope
		clusterQueueSelector *metav1.LabelSelector
		want                 bool
	}{
		"limit without clusterQueueSelector matches any ClusterQueues": {
			scope: kueuealpha.PreemptedCQLimitScope,
			want:  true,
		},
		"preempting ClusterQueue limit matches the preemptor's ClusterQueue": {
			scope:                kueuealpha.PreemptingCQLimitScope,
			clusterQueueSelector: selectsPreempting,
			want:                 true,
		},
		"preempting ClusterQueue limit doesn't match the target's ClusterQueue": {
			scope:                kueuealpha.PreemptingCQLimitScope,
			clusterQueueSelector: selectsPreempted,
		},
		"preempted ClusterQueue limit matches the target's ClusterQueue": {
			scope:                kueuealpha.PreemptedCQLimitScope,
			clusterQueueSelector: selectsPreempted,
			want:                 true,
		},
		"preempted ClusterQueue limit doesn't match the preemptor's ClusterQueue": {
			scope:                kueuealpha.PreemptedCQLimitScope,
			clusterQueueSelector: selectsPreempting,
		},
		"preempted workload limit matches the target's ClusterQueue": {
			scope:                kueuealpha.PreemptedWorkloadLimitScope,
			clusterQueueSelector: selectsPreempted,
			want:                 true,
		},
		"preempted workload limit doesn't match the preemptor's ClusterQueue": {
			scope:                kueuealpha.PreemptedWorkloadLimitScope,
			clusterQueueSelector: selectsPreempting,
		},
		"global limit matches the preemptor's ClusterQueue": {
			scope:                kueuealpha.GlobalPreemptionLimitScope,
			clusterQueueSelector: selectsPreempting,
			want:                 true,
		},
		"global limit matches the target's ClusterQueue": {
			scope:                kueuealpha.GlobalPreemptionLimitScope,
			clusterQueueSelector: selectsPreempted,
			want:                 true,
		},
		"global limit doesn't match when its clusterQueueSelector selects neither ClusterQueue": {
			scope:                kueuealpha.GlobalPreemptionLimitScope,
			clusterQueueSelector: selectsOther,
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			limit := utiltestingalpha.MakePreemptionLimit("limit", tc.scope).ClusterQueueSelector(tc.clusterQueueSelector).Obj()
			matcher, err := newPreemptionLimitMatcher(limit, utiltestingalpha.MakePreemptionConfig("config").Obj())
			if err != nil {
				t.Fatalf("newPreemptionLimitMatcher() unexpected error: %v", err)
			}
			if got := matcher.matchesClusterQueues(preemptorCQ, targetCQ); got != tc.want {
				t.Errorf("matchesClusterQueues() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestPreemptionLimitMatcherScopeValue(t *testing.T) {
	preemptor := &workload.Info{Obj: utiltestingapi.MakeWorkload("preemptor", "ns").Obj(), ClusterQueue: "preempting-cq"}
	target := &workload.Info{Obj: utiltestingapi.MakeWorkload("target", "ns").Obj(), ClusterQueue: "preempted-cq"}

	cases := map[kueuealpha.PreemptionLimitScope]policy.PreemptionLimitScopeValue{
		kueuealpha.GlobalPreemptionLimitScope:  "Global",
		kueuealpha.PreemptingCQLimitScope:      "preempting-cq",
		kueuealpha.PreemptedCQLimitScope:       "preempted-cq",
		kueuealpha.PreemptedWorkloadLimitScope: "ns/target",
	}
	for scope, want := range cases {
		t.Run(string(scope), func(t *testing.T) {
			matcher := &preemptionLimitMatcher{limit: utiltestingalpha.MakePreemptionLimit("limit", scope).Obj()}
			if got := matcher.scopeValue(preemptor, target); got != want {
				t.Errorf("scopeValue() = %q, want %q", got, want)
			}
		})
	}
}
