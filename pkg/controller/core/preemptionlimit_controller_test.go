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

package core

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	testingclock "k8s.io/utils/clock/testing"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	kueuealpha "sigs.k8s.io/kueue/apis/kueue/v1alpha1"
	"sigs.k8s.io/kueue/pkg/scheduler/preemption/limits"
	"sigs.k8s.io/kueue/pkg/scheduler/preemption/policy"
	utiltesting "sigs.k8s.io/kueue/pkg/util/testing"
	utiltestingalpha "sigs.k8s.io/kueue/pkg/util/testing/v1alpha1"
)

func TestPreemptionLimitReconcile(t *testing.T) {
	now := time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)
	lastUpdateTime := new(metav1.NewTime(now))
	errTest := errors.New("test error")
	limit := utiltestingalpha.MakePreemptionLimit("limit", kueuealpha.PreemptorCQLimitScope).Generation(1).Limit(10).LimitWindowSeconds(60).Obj()
	otherLimit := utiltestingalpha.MakePreemptionLimit("other-limit", kueuealpha.GlobalPreemptionLimitScope).Generation(1).LimitWindowSeconds(60).Obj()
	// recorded holds the preemptions of "limit" recorded before it is
	// reconciled, by timestamp.
	type recorded map[time.Time]policy.ConsumedLimits
	limitConsumed := func(v policy.PreemptionLimitScopeValue) policy.ConsumedLimits {
		return policy.ConsumedLimits{"limit": policy.NewConsumedLimit(limit, v)}
	}
	seeded := recorded{
		now.Add(-time.Hour):        limitConsumed("cq-a"),
		now.Add(-30 * time.Second): limitConsumed("cq-a"),
		now:                        limitConsumed("cq-b"),
	}
	multiplePreemptions := recorded{
		now.Add(-7 * time.Second): limitConsumed("cq-b"),
		now.Add(-6 * time.Second): limitConsumed("cq-d"),
		now.Add(-5 * time.Second): limitConsumed("cq-a"),
		now.Add(-4 * time.Second): limitConsumed("cq-c"),
		now.Add(-3 * time.Second): limitConsumed("cq-d"),
		now.Add(-2 * time.Second): limitConsumed("cq-a"),
		now.Add(-1 * time.Second): limitConsumed("cq-c"),
		now:                       limitConsumed("cq-c"),
	}
	overMaxPreemptions := make(recorded, preemptionLimitCountMaxItems+2)
	overMaxPreemptions[now] = limitConsumed("high")
	overMaxPreemptions[now.Add(-time.Millisecond)] = limitConsumed("high")
	overMaxCounts := map[policy.PreemptionLimitScopeValue]int32{"high": 2}
	for i := range preemptionLimitCountMaxItems {
		scopeValue := policy.PreemptionLimitScopeValue(fmt.Sprintf("cq-%04d", i))
		overMaxPreemptions[now.Add(-time.Duration(i+2)*time.Millisecond)] = limitConsumed(scopeValue)
		overMaxCounts[scopeValue] = 1
	}
	wantTopCounts := make([]kueuealpha.PreemptionLimitCount, 0, preemptionLimitCountMaxItems)
	wantTopCounts = append(wantTopCounts, kueuealpha.PreemptionLimitCount{Name: "high", Value: 2})
	for i := range preemptionLimitCountMaxItems - 1 {
		wantTopCounts = append(wantTopCounts, kueuealpha.PreemptionLimitCount{
			Name:  fmt.Sprintf("cq-%04d", i),
			Value: 1,
		})
	}

	cases := map[string]struct {
		// limit is "limit" when it is reconciled, or nil once deleted.
		limit       *kueuealpha.PreemptionLimit
		recorded    recorded
		clientFuncs interceptor.Funcs
		wantResult  reconcile.Result
		wantErr     error
		// wantCounts are the preemptions within the window of "limit".
		wantCounts map[policy.PreemptionLimitScopeValue]int32
		// wantLimitStatus, if set, is the status of "limit" after the reconcile.
		wantLimitStatus *kueuealpha.PreemptionLimitStatus
	}{
		"unchanged limit keeps its events and reports them in its status": {
			limit:      limit,
			recorded:   seeded,
			wantResult: reconcile.Result{RequeueAfter: time.Minute},
			wantCounts: map[policy.PreemptionLimitScopeValue]int32{"cq-a": 1, "cq-b": 1},
			wantLimitStatus: &kueuealpha.PreemptionLimitStatus{
				LastUpdateTime: lastUpdateTime,
				Counts: []kueuealpha.PreemptionLimitCount{
					{Name: "cq-a", Value: 1},
					{Name: "cq-b", Value: 1},
				},
			},
		},
		"limit and window change keep the events": {
			limit:      utiltestingalpha.MakePreemptionLimit("limit", kueuealpha.PreemptorCQLimitScope).Generation(2).Limit(20).LimitWindowSeconds(2 * 3600).Obj(),
			recorded:   seeded,
			wantResult: reconcile.Result{RequeueAfter: time.Minute},
			wantCounts: map[policy.PreemptionLimitScopeValue]int32{"cq-a": 2, "cq-b": 1},
			wantLimitStatus: &kueuealpha.PreemptionLimitStatus{
				LastUpdateTime: lastUpdateTime,
				Counts: []kueuealpha.PreemptionLimitCount{
					{Name: "cq-a", Value: 2},
					{Name: "cq-b", Value: 1},
				},
			},
		},
		"no events within the window produce empty status counts": {
			limit:      limit,
			recorded:   recorded{now.Add(-time.Hour): limitConsumed("cq-a")},
			wantResult: reconcile.Result{RequeueAfter: time.Minute},
			wantLimitStatus: &kueuealpha.PreemptionLimitStatus{
				LastUpdateTime: lastUpdateTime,
			},
		},
		"status counts are sorted by value descending and then by name ascending": {
			limit:      limit,
			recorded:   multiplePreemptions,
			wantResult: reconcile.Result{RequeueAfter: time.Minute},
			wantCounts: map[policy.PreemptionLimitScopeValue]int32{"cq-a": 2, "cq-b": 1, "cq-c": 3, "cq-d": 2},
			wantLimitStatus: &kueuealpha.PreemptionLimitStatus{
				LastUpdateTime: lastUpdateTime,
				Counts: []kueuealpha.PreemptionLimitCount{
					{Name: "cq-c", Value: 3},
					{Name: "cq-a", Value: 2},
					{Name: "cq-d", Value: 2},
					{Name: "cq-b", Value: 1},
				},
			},
		},
		"status counts exceeding max items are truncated to top 1000 after sorting": {
			limit:      limit,
			recorded:   overMaxPreemptions,
			wantResult: reconcile.Result{RequeueAfter: time.Minute},
			wantCounts: overMaxCounts,
			wantLimitStatus: &kueuealpha.PreemptionLimitStatus{
				LastUpdateTime: lastUpdateTime,
				Counts:         wantTopCounts,
			},
		},
		"deleted limit is forgotten": {
			recorded: seeded,
		},
		"get error other than NotFound is returned and the events are kept": {
			limit:    limit,
			recorded: seeded,
			clientFuncs: interceptor.Funcs{
				Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
					return errTest
				},
			},
			wantErr:    errTest,
			wantCounts: map[policy.PreemptionLimitScopeValue]int32{"cq-a": 1, "cq-b": 1},
		},
		"status update error is returned and the events are kept": {
			limit:    limit,
			recorded: seeded,
			clientFuncs: interceptor.Funcs{
				SubResourceUpdate: func(context.Context, client.Client, string, client.Object, ...client.SubResourceUpdateOption) error {
					return errTest
				},
			},
			wantErr:    errTest,
			wantCounts: map[policy.PreemptionLimitScopeValue]int32{"cq-a": 1, "cq-b": 1},
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			ctx, _ := utiltesting.ContextWithLog(t)
			tracker := limits.NewPreemptionLimitTracker()
			reconciler := NewPreemptionLimitReconciler(utiltesting.NewClientBuilder().
				WithObjects(limit.DeepCopy(), otherLimit.DeepCopy()).
				WithStatusSubresource(&kueuealpha.PreemptionLimit{}).
				Build(), tracker, nil)
			reconciler.clock = testingclock.NewFakeClock(now)
			// Preemptions are only recorded for the reconciled PreemptionLimits.
			for _, l := range []*kueuealpha.PreemptionLimit{limit, otherLimit} {
				if _, err := reconciler.Reconcile(ctx, reconcile.Request{Name: l.Name}); err != nil {
					t.Fatalf("Reconcile() of PreemptionLimit %q failed: %v", l.Name, err)
				}
			}
			for ts, consumedLimits := range tc.recorded {
				tracker.Record(ts, consumedLimits)
			}
			tracker.Record(now, policy.ConsumedLimits{"other-limit": policy.NewConsumedLimit(otherLimit, "Global")})

			builder := utiltesting.NewClientBuilder().
				WithInterceptorFuncs(tc.clientFuncs).
				WithStatusSubresource(&kueuealpha.PreemptionLimit{})
			if tc.limit != nil {
				builder = builder.WithObjects(tc.limit.DeepCopy())
			}
			reconciler.client = builder.Build()
			gotResult, gotErr := reconciler.Reconcile(ctx, reconcile.Request{Name: "limit"})
			if diff := cmp.Diff(tc.wantErr, gotErr, cmpopts.EquateErrors()); diff != "" {
				t.Errorf("Unexpected Reconcile() error (-want,+got):\n%s", diff)
			}
			if diff := cmp.Diff(tc.wantResult, gotResult); diff != "" {
				t.Errorf("Unexpected Reconcile() result (-want,+got):\n%s", diff)
			}
			if diff := cmp.Diff(tc.wantCounts, tracker.Counts(now, "limit"), cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("Unexpected counts of the reconciled limit (-want,+got):\n%s", diff)
			}
			wantOtherCounts := map[policy.PreemptionLimitScopeValue]int32{"Global": 1}
			if diff := cmp.Diff(wantOtherCounts, tracker.Counts(now, "other-limit")); diff != "" {
				t.Errorf("Unexpected counts of the other limit (-want,+got):\n%s", diff)
			}
			if tc.wantLimitStatus != nil {
				var got kueuealpha.PreemptionLimit
				if err := reconciler.client.Get(ctx, client.ObjectKey{Name: "limit"}, &got); err != nil {
					t.Fatalf("Failed to get PreemptionLimit: %v", err)
				}
				if diff := cmp.Diff(*tc.wantLimitStatus, got.Status); diff != "" {
					t.Errorf("Unexpected PreemptionLimitStatus (-want,+got):\n%s", diff)
				}
			}
		})
	}
}
