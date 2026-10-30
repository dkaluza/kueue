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
	"cmp"
	"maps"
	"slices"
	"testing"
	"time"

	"github.com/go-logr/logr"
	gocmp "github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kueuealpha "sigs.k8s.io/kueue/apis/kueue/v1alpha1"
	schdcache "sigs.k8s.io/kueue/pkg/cache/scheduler"
	preemptexpectations "sigs.k8s.io/kueue/pkg/scheduler/preemption/expectations"
	"sigs.k8s.io/kueue/pkg/scheduler/preemption/policy"
	utiltestingalpha "sigs.k8s.io/kueue/pkg/util/testing/v1alpha1"
	utiltestingapi "sigs.k8s.io/kueue/pkg/util/testing/v1beta2"
	"sigs.k8s.io/kueue/pkg/workload"
)

func TestNewPreemptionLimiter(t *testing.T) {
	config := utiltestingalpha.MakePreemptionConfig("config").Label("tier", "gold").Obj()

	cases := map[string]struct {
		limits  trackedLimits
		want    []policy.PreemptionLimitReference
		wantErr *selectorError
	}{
		"limits applying to the config are kept by name, the others are dropped": {
			limits: tracked(
				trackedLimit(utiltestingalpha.MakePreemptionLimit("any-config", kueuealpha.GlobalPreemptionLimitScope).LimitWindowSeconds(60).Obj(), nil),
				trackedLimit(utiltestingalpha.MakePreemptionLimit("silver", kueuealpha.GlobalPreemptionLimitScope).LimitWindowSeconds(60).
					ConfigSelector(&metav1.LabelSelector{MatchLabels: map[string]string{"tier": "silver"}}).Obj(), nil),
			),
			want: []policy.PreemptionLimitReference{"any-config"},
		},
		"invalid limit is an error": {
			limits: tracked(
				trackedLimit(utiltestingalpha.MakePreemptionLimit("any-config", kueuealpha.GlobalPreemptionLimitScope).LimitWindowSeconds(60).Obj(), nil),
				trackedLimit(utiltestingalpha.MakePreemptionLimit("invalid", kueuealpha.GlobalPreemptionLimitScope).LimitWindowSeconds(60).
					ConfigSelector(&metav1.LabelSelector{
						MatchExpressions: []metav1.LabelSelectorRequirement{{Key: "tier", Operator: "invalid"}},
					}).Obj(), nil),
			),
			wantErr: &selectorError{PreemptionLimit: "invalid", Selector: "configSelector"},
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			limiter, err := NewPreemptionLimiter(logr.Discard(), snapshotOf(time.Time{}, tc.limits), config, preemptexpectations.New())
			if diff := diffSelectorError(tc.wantErr, err); diff != "" {
				t.Fatalf("NewPreemptionLimiter() unexpected error (-want,+got):\n%s", diff)
			}
			if err != nil {
				return
			}
			got := slices.Sorted(maps.Keys(limiter.matchers))
			if diff := gocmp.Diff(tc.want, got); diff != "" {
				t.Errorf("Unexpected limits (-want,+got):\n%s", diff)
			}
		})
	}
}

func TestPreemptionLimiterConsumedLimits(t *testing.T) {
	preemptorCQ := &schdcache.ClusterQueueSnapshot{Name: "preempting-cq", Labels: map[string]string{"side": "preempting"}}
	targetCQ := &schdcache.ClusterQueueSnapshot{Name: "preempted-cq", Labels: map[string]string{"side": "preempted"}}
	preemptor := &workload.Info{Obj: utiltestingapi.MakeWorkload("preemptor", "ns").Obj(), ClusterQueue: "preempting-cq"}
	target := &workload.Info{Obj: utiltestingapi.MakeWorkload("target", "ns").UID("target").Obj(), ClusterQueue: "preempted-cq"}
	globalLimit := utiltestingalpha.MakePreemptionLimit("global", kueuealpha.GlobalPreemptionLimitScope).LimitWindowSeconds(60).Obj()
	preemptedWorkloadLimit := utiltestingalpha.MakePreemptionLimit("preempted-workload", kueuealpha.PreemptedWorkloadLimitScope).LimitWindowSeconds(60).Obj()

	cases := map[string]struct {
		limits trackedLimits
		// target, when set, replaces the default target.
		target *workload.Info
		// unobservedEviction marks the eviction of the target as issued but not observed yet.
		unobservedEviction bool
		nilLimiter         bool
		want               policy.ConsumedLimits
	}{
		"no limits consume nothing": {},
		"limits matching the ClusterQueues are consumed together, the others are not": {
			limits: tracked(
				trackedLimit(globalLimit, nil),
				trackedLimit(preemptedWorkloadLimit, nil),
				trackedLimit(utiltestingalpha.MakePreemptionLimit("other-cqs", kueuealpha.PreemptedCQLimitScope).LimitWindowSeconds(60).
					PreemptedClusterQueueSelector(&metav1.LabelSelector{MatchLabels: map[string]string{"side": "other"}}).Obj(), nil),
			),
			want: policy.ConsumedLimits{
				"global":             policy.NewConsumedLimit(globalLimit, "Global"),
				"preempted-workload": policy.NewConsumedLimit(preemptedWorkloadLimit, "ns/target"),
			},
		},
		"evicted target consumes nothing": {
			limits: tracked(trackedLimit(globalLimit, nil)),
			target: &workload.Info{
				Obj:          utiltestingapi.MakeWorkload("target", "ns").UID("target").EvictedAt(time.Now()).Obj(),
				ClusterQueue: "preempted-cq",
			},
		},
		"target with an unobserved eviction consumes nothing": {
			limits:             tracked(trackedLimit(globalLimit, nil)),
			unobservedEviction: true,
		},
		"nil limiter consumes nothing": {
			limits:     tracked(trackedLimit(globalLimit, nil)),
			nilLimiter: true,
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			log := logr.Discard()
			preemptionExpectations := preemptexpectations.New()
			if tc.unobservedEviction {
				preemptionExpectations.ExpectUIDs(log, client.ObjectKeyFromObject(target.Obj), []types.UID{target.Obj.UID})
			}
			limiter, err := NewPreemptionLimiter(log, snapshotOf(time.Time{}, tc.limits),
				utiltestingalpha.MakePreemptionConfig("config").Obj(), preemptionExpectations)
			if err != nil {
				t.Fatalf("NewPreemptionLimiter() unexpected error: %v", err)
			}
			if tc.nilLimiter {
				limiter = nil
			}
			got := limiter.consumedLimits(preemptor, cmp.Or(tc.target, target), preemptorCQ, targetCQ)
			if diff := gocmp.Diff(tc.want, got, gocmp.AllowUnexported(policy.ConsumedLimit{}), cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("Unexpected consumed limits (-want,+got):\n%s", diff)
			}
		})
	}
}

func TestPreemptionLimiterAllowance(t *testing.T) {
	now := time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)
	perCQLimit := utiltestingalpha.MakePreemptionLimit("per-cq", kueuealpha.PreemptedCQLimitScope).
		Limit(3).LimitWindowSeconds(60).Obj()
	perCQRef := policy.PreemptionLimitReference(perCQLimit.Name)

	cases := map[string]struct {
		limits trackedLimits
		// changeTracker, when set, changes the tracker after the limiter is created.
		changeTracker func(*PreemptionLimitTracker)
		want          int32
	}{
		"allowance is the limit minus the preemptions within the window": {
			limits: tracked(trackedLimit(perCQLimit, scopeEvents{"cq-a": {now.Add(-2 * time.Minute), now.Add(-30 * time.Second)}})),
			want:   2,
		},
		"allowance returns the current limit for the proper scope value": {
			limits: tracked(trackedLimit(perCQLimit, scopeEvents{"cq-b": {now, now}})),
			want:   3,
		},
		"allowance doesn't change when the tracker forgets the limit after the limiter is created": {
			limits:        tracked(trackedLimit(perCQLimit, scopeEvents{"cq-a": {now, now}})),
			changeTracker: func(tracker *PreemptionLimitTracker) { tracker.Forget(perCQRef) },
			want:          1,
		},
		"allowance doesn't change when the tracker prunes the limit after the limiter is created": {
			limits: tracked(trackedLimit(perCQLimit, scopeEvents{"cq-a": {now.Add(-50 * time.Second), now}})),
			changeTracker: func(tracker *PreemptionLimitTracker) {
				// Drops all the events, as a prune with a shrunk window would.
				shrunk := perCQLimit.DeepCopy()
				shrunk.Spec.LimitWindowSeconds = 1
				tracker.Sync(now.Add(time.Hour), shrunk)
			},
			want: 1,
		},
		"allowance doesn't change when the tracker records preemptions after the limiter is created": {
			limits: tracked(trackedLimit(perCQLimit, nil)),
			changeTracker: func(tracker *PreemptionLimitTracker) {
				tracker.Record(now, policy.ConsumedLimits{perCQRef: policy.NewConsumedLimit(perCQLimit, "cq-a")})
			},
			want: 3,
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			tracker := NewPreemptionLimitTracker()
			if tc.limits != nil {
				tracker.limits = tc.limits
			}
			limiter, err := NewPreemptionLimiter(logr.Discard(), tracker.Snapshot(now), utiltestingalpha.MakePreemptionConfig("config").Obj(), preemptexpectations.New())
			if err != nil {
				t.Fatalf("NewPreemptionLimiter() unexpected error: %v", err)
			}
			if tc.changeTracker != nil {
				tc.changeTracker(tracker)
			}
			if got := limiter.allowance(perCQRef, "cq-a"); got != tc.want {
				t.Errorf("allowance() = %d, want %d", got, tc.want)
			}
		})
	}
}
