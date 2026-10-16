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

	"github.com/google/go-cmp/cmp"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	kueuealpha "sigs.k8s.io/kueue/apis/kueue/v1alpha1"
	schdcache "sigs.k8s.io/kueue/pkg/cache/scheduler"
	"sigs.k8s.io/kueue/pkg/scheduler/preemption/policy"
	utiltestingalpha "sigs.k8s.io/kueue/pkg/util/testing/v1alpha1"
	utiltestingapi "sigs.k8s.io/kueue/pkg/util/testing/v1beta2"
	"sigs.k8s.io/kueue/pkg/workload"
)

func TestNewPreemptionLimitMatchers(t *testing.T) {
	config := utiltestingalpha.MakePreemptionConfig("config").Label("tier", "gold").Obj()
	invalidSelector := &metav1.LabelSelector{
		MatchExpressions: []metav1.LabelSelectorRequirement{{Key: "tier", Operator: "invalid"}},
	}

	cases := map[string]struct {
		preemptionLimits []kueuealpha.PreemptionLimit
		want             []string
		// wantErr, when set, is the expected prefix of the error message.
		wantErr string
	}{
		"limit without configSelector applies": {
			preemptionLimits: []kueuealpha.PreemptionLimit{
				*utiltestingalpha.MakePreemptionLimit("any-config", kueuealpha.GlobalPreemptionLimitScope).Obj(),
			},
			want: []string{"any-config"},
		},
		"limit whose configSelector selects the config applies, other limits are dropped": {
			preemptionLimits: []kueuealpha.PreemptionLimit{
				*utiltestingalpha.MakePreemptionLimit("gold", kueuealpha.GlobalPreemptionLimitScope).
					ConfigSelector(&metav1.LabelSelector{MatchLabels: map[string]string{"tier": "gold"}}).Obj(),
				*utiltestingalpha.MakePreemptionLimit("silver", kueuealpha.GlobalPreemptionLimitScope).
					ConfigSelector(&metav1.LabelSelector{MatchLabels: map[string]string{"tier": "silver"}}).Obj(),
			},
			want: []string{"gold"},
		},
		"invalid configSelector is an error": {
			preemptionLimits: []kueuealpha.PreemptionLimit{
				*utiltestingalpha.MakePreemptionLimit("invalid", kueuealpha.GlobalPreemptionLimitScope).
					ConfigSelector(invalidSelector).Obj(),
			},
			wantErr: `preemptionLimit "invalid" configSelector: `,
		},
		"invalid clusterQueueSelector is an error": {
			preemptionLimits: []kueuealpha.PreemptionLimit{
				*utiltestingalpha.MakePreemptionLimit("invalid", kueuealpha.GlobalPreemptionLimitScope).
					ClusterQueueSelector(invalidSelector).Obj(),
			},
			wantErr: `preemptionLimit "invalid" clusterQueueSelector: `,
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			matchers, err := NewPreemptionLimitMatchers(tc.preemptionLimits, config)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("NewPreemptionLimitMatchers() unexpected error: %v", err)
				}
			} else if err == nil || !strings.HasPrefix(err.Error(), tc.wantErr) {
				t.Fatalf("NewPreemptionLimitMatchers() error = %v, want error starting with %q", err, tc.wantErr)
			}
			var got []string
			for _, matcher := range matchers {
				got = append(got, matcher.limit.Name)
			}
			if diff := cmp.Diff(tc.want, got); diff != "" {
				t.Errorf("Unexpected matched limits (-want,+got):\n%s", diff)
			}
		})
	}
}

func TestResolve(t *testing.T) {
	preemptorCQ := &schdcache.ClusterQueueSnapshot{
		Name:   "preempting-cq",
		Labels: map[string]string{"side": "preempting"},
	}
	targetCQ := &schdcache.ClusterQueueSnapshot{
		Name:   "preempted-cq",
		Labels: map[string]string{"side": "preempted"},
	}
	target := &workload.Info{
		Obj:          utiltestingapi.MakeWorkload("target", "ns").Obj(),
		ClusterQueue: "preempted-cq",
	}
	selectsPreempting := &metav1.LabelSelector{MatchLabels: map[string]string{"side": "preempting"}}
	selectsPreempted := &metav1.LabelSelector{MatchLabels: map[string]string{"side": "preempted"}}
	selectsOther := &metav1.LabelSelector{MatchLabels: map[string]string{"side": "other"}}

	cases := map[string]struct {
		preemptionLimits []kueuealpha.PreemptionLimit
		want             policy.ConsumedLimits
	}{
		"no limits": {},
		"each scope resolves its scope value": {
			preemptionLimits: []kueuealpha.PreemptionLimit{
				*utiltestingalpha.MakePreemptionLimit("global", kueuealpha.GlobalPreemptionLimitScope).Obj(),
				*utiltestingalpha.MakePreemptionLimit("preempting-cq", kueuealpha.PreemptingCQLimitScope).Obj(),
				*utiltestingalpha.MakePreemptionLimit("preempted-cq", kueuealpha.PreemptedCQLimitScope).Obj(),
				*utiltestingalpha.MakePreemptionLimit("preempted-workload", kueuealpha.PreemptedWorkloadLimitScope).Obj(),
			},
			want: policy.ConsumedLimits{
				"global":             "Global",
				"preempting-cq":      "preempting-cq",
				"preempted-cq":       "preempted-cq",
				"preempted-workload": "ns/target",
			},
		},
		"preempting ClusterQueue limit applies only when its clusterQueueSelector selects the preemptor's ClusterQueue": {
			preemptionLimits: []kueuealpha.PreemptionLimit{
				*utiltestingalpha.MakePreemptionLimit("selects-preempting", kueuealpha.PreemptingCQLimitScope).ClusterQueueSelector(selectsPreempting).Obj(),
				*utiltestingalpha.MakePreemptionLimit("selects-preempted", kueuealpha.PreemptingCQLimitScope).ClusterQueueSelector(selectsPreempted).Obj(),
			},
			want: policy.ConsumedLimits{"selects-preempting": "preempting-cq"},
		},
		"preempted ClusterQueue limit applies only when its clusterQueueSelector selects the target's ClusterQueue": {
			preemptionLimits: []kueuealpha.PreemptionLimit{
				*utiltestingalpha.MakePreemptionLimit("selects-preempting", kueuealpha.PreemptedCQLimitScope).ClusterQueueSelector(selectsPreempting).Obj(),
				*utiltestingalpha.MakePreemptionLimit("selects-preempted", kueuealpha.PreemptedCQLimitScope).ClusterQueueSelector(selectsPreempted).Obj(),
			},
			want: policy.ConsumedLimits{"selects-preempted": "preempted-cq"},
		},
		"preempted workload limit applies only when its clusterQueueSelector selects the target's ClusterQueue": {
			preemptionLimits: []kueuealpha.PreemptionLimit{
				*utiltestingalpha.MakePreemptionLimit("selects-preempting", kueuealpha.PreemptedWorkloadLimitScope).ClusterQueueSelector(selectsPreempting).Obj(),
				*utiltestingalpha.MakePreemptionLimit("selects-preempted", kueuealpha.PreemptedWorkloadLimitScope).ClusterQueueSelector(selectsPreempted).Obj(),
			},
			want: policy.ConsumedLimits{"selects-preempted": "ns/target"},
		},
		"global limit applies when its clusterQueueSelector selects the preemptor's ClusterQueue": {
			preemptionLimits: []kueuealpha.PreemptionLimit{
				*utiltestingalpha.MakePreemptionLimit("selects-preempting", kueuealpha.GlobalPreemptionLimitScope).ClusterQueueSelector(selectsPreempting).Obj(),
			},
			want: policy.ConsumedLimits{"selects-preempting": "Global"},
		},
		"global limit applies when its clusterQueueSelector selects the target's ClusterQueue": {
			preemptionLimits: []kueuealpha.PreemptionLimit{
				*utiltestingalpha.MakePreemptionLimit("selects-preempted", kueuealpha.GlobalPreemptionLimitScope).ClusterQueueSelector(selectsPreempted).Obj(),
			},
			want: policy.ConsumedLimits{"selects-preempted": "Global"},
		},
		"global limit does not apply when its clusterQueueSelector selects neither ClusterQueue": {
			preemptionLimits: []kueuealpha.PreemptionLimit{
				*utiltestingalpha.MakePreemptionLimit("selects-neither", kueuealpha.GlobalPreemptionLimitScope).ClusterQueueSelector(selectsOther).Obj(),
			},
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			matchers, err := NewPreemptionLimitMatchers(tc.preemptionLimits, utiltestingalpha.MakePreemptionConfig("config").Obj())
			if err != nil {
				t.Fatalf("NewPreemptionLimitMatchers() error = %v", err)
			}
			got := Resolve(matchers, preemptorCQ, targetCQ, target)
			if diff := cmp.Diff(tc.want, got); diff != "" {
				t.Errorf("Unexpected consumed limits (-want,+got):\n%s", diff)
			}
		})
	}
}
