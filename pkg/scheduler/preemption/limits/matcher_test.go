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
	"errors"
	"fmt"
	"testing"

	gocmp "github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
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
	limit := utiltestingalpha.MakePreemptionLimit("limit", kueuealpha.GlobalPreemptionLimitScope).LimitWindowSeconds(60)

	cases := map[string]struct {
		limit       *kueuealpha.PreemptionLimit
		wantApplies bool
		wantErr     *selectorError
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
			wantErr: &selectorError{PreemptionLimit: "limit", Selector: "configSelector"},
		},
		"invalid preemptorClusterQueueSelector of a limit applying to the config is an error": {
			limit:   limit.Clone().PreemptorClusterQueueSelector(invalidSelector).Obj(),
			wantErr: &selectorError{PreemptionLimit: "limit", Selector: "preemptorClusterQueueSelector"},
		},
		"invalid preemptedClusterQueueSelector of a limit applying to the config is an error": {
			limit:   limit.Clone().PreemptedClusterQueueSelector(invalidSelector).Obj(),
			wantErr: &selectorError{PreemptionLimit: "limit", Selector: "preemptedClusterQueueSelector"},
		},
		"invalid ClusterQueue selectors of a limit not applying to the config are ignored": {
			limit: limit.Clone().
				ConfigSelector(selectsSilver).
				PreemptorClusterQueueSelector(invalidSelector).
				PreemptedClusterQueueSelector(invalidSelector).
				Obj(),
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			matcher, err := newPreemptionLimitMatcher(tc.limit, config)
			if diff := diffSelectorError(tc.wantErr, err); diff != "" {
				t.Fatalf("newPreemptionLimitMatcher() unexpected error (-want,+got):\n%s", diff)
			}
			if err != nil {
				return
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

func TestPreemptionLimitMatcherMatches(t *testing.T) {
	preemptorCQ := &schdcache.ClusterQueueSnapshot{Name: "preemptor-cq", Labels: map[string]string{"side": "preemptor"}}
	targetCQ := &schdcache.ClusterQueueSnapshot{Name: "preempted-cq", Labels: map[string]string{"side": "preempted"}}
	selectsPreemptor := &metav1.LabelSelector{MatchLabels: map[string]string{"side": "preemptor"}}
	selectsPreempted := &metav1.LabelSelector{MatchLabels: map[string]string{"side": "preempted"}}
	notMatchingLabels := &metav1.LabelSelector{MatchLabels: map[string]string{"side": "notMatching"}}

	cases := map[string]struct {
		preemptorClusterQueueSelector *metav1.LabelSelector
		preemptedClusterQueueSelector *metav1.LabelSelector
		ruleNames                     []string
		ruleReference                 policy.PreemptionConfigRuleReference
		want                          bool
	}{
		"limit without ClusterQueue selectors matches any ClusterQueues": {
			want: true,
		},
		"limit matches when both ClusterQueue selectors match": {
			preemptorClusterQueueSelector: selectsPreemptor,
			preemptedClusterQueueSelector: selectsPreempted,
			ruleReference:                 "preemptionLimitRuleReference",
			want:                          true,
		},
		"limit doesn't match when preemptorClusterQueueSelector doesn't match": {
			preemptorClusterQueueSelector: selectsPreempted,
			preemptedClusterQueueSelector: selectsPreempted,
			ruleReference:                 "preemptionLimitRuleReference",
			want:                          false,
		},
		"limit doesn't match when preemptedClusterQueueSelector doesn't match": {
			preemptorClusterQueueSelector: selectsPreemptor,
			preemptedClusterQueueSelector: selectsPreemptor,
			ruleReference:                 "preemptionLimitRuleReference",
			want:                          false,
		},
		"multiple ruleNames contain ruleReference for candidate, match": {
			ruleNames:     []string{"unknownRuleName", "preemptionLimitRuleReference"},
			ruleReference: "preemptionLimitRuleReference",
			want:          true,
		},
		"multiple ruleNames don't contain ruleReference for candidate, no match": {
			ruleNames:     []string{"unknownRuleName", "unknownRuleName2"},
			ruleReference: "preemptionLimitRuleReference",
			want:          false,
		},
		"clusterQueue match and ruleNames contain ruleReference, match": {
			preemptorClusterQueueSelector: selectsPreemptor,
			preemptedClusterQueueSelector: selectsPreempted,
			ruleNames:                     []string{"preemptionLimitRuleReference"},
			ruleReference:                 "preemptionLimitRuleReference",
			want:                          true,
		},
		"clusterQueue match but ruleNames don't contain ruleReference, no match": {
			preemptorClusterQueueSelector: selectsPreemptor,
			preemptedClusterQueueSelector: selectsPreempted,
			ruleNames:                     []string{"preemptionLimitRuleReference"},
			ruleReference:                 "unknownRule",
			want:                          false,
		},
		"clusterQueue doesn't match but ruleNames contain ruleReference, no match": {
			preemptorClusterQueueSelector: notMatchingLabels,
			preemptedClusterQueueSelector: notMatchingLabels,
			ruleNames:                     []string{"preemptionLimitRuleReference"},
			ruleReference:                 "preemptionLimitRuleReference",
			want:                          false,
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			limit := utiltestingalpha.MakePreemptionLimit("limit", kueuealpha.GlobalPreemptionLimitScope).
				LimitWindowSeconds(60).
				PreemptorClusterQueueSelector(tc.preemptorClusterQueueSelector).
				PreemptedClusterQueueSelector(tc.preemptedClusterQueueSelector).
				RuleNames(tc.ruleNames...).
				Obj()
			matcher, err := newPreemptionLimitMatcher(limit, utiltestingalpha.MakePreemptionConfig("config").Obj())
			if err != nil {
				t.Fatalf("newPreemptionLimitMatcher() unexpected error: %v", err)
			}
			if got := matcher.matches(preemptorCQ, targetCQ, tc.ruleReference); got != tc.want {
				t.Errorf("matches() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestPreemptionLimitMatcherScopeValue(t *testing.T) {
	preemptor := &workload.Info{Obj: utiltestingapi.MakeWorkload("preemptor", "ns").Obj(), ClusterQueue: "preemptor-cq"}
	target := &workload.Info{Obj: utiltestingapi.MakeWorkload("target", "ns").Obj(), ClusterQueue: "preempted-cq"}

	cases := map[kueuealpha.PreemptionLimitScope]policy.PreemptionLimitScopeValue{
		kueuealpha.GlobalPreemptionLimitScope:  policy.GlobalScopeValue,
		kueuealpha.PreemptorCQLimitScope:       "preemptor-cq",
		kueuealpha.PreemptedCQLimitScope:       "preempted-cq",
		kueuealpha.PreemptedWorkloadLimitScope: "ns/target",
	}
	for scope, want := range cases {
		t.Run(string(scope), func(t *testing.T) {
			matcher := &preemptionLimitMatcher{limit: utiltestingalpha.MakePreemptionLimit("limit", scope).LimitWindowSeconds(60).Obj()}
			if got := matcher.scopeValue(preemptor, target); got != want {
				t.Errorf("scopeValue() = %q, want %q", got, want)
			}
		})
	}
}

// diffSelectorError compares err with want, ignoring the wrapped error.
func diffSelectorError(want *selectorError, err error) string {
	var got *selectorError
	if err != nil && !errors.As(err, &got) {
		return fmt.Sprintf("unexpected error type: %v", err)
	}
	return gocmp.Diff(want, got, cmpopts.IgnoreFields(selectorError{}, "Err"))
}
