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
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
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
	expired := now.Add(-time.Hour)
	lastUpdateTime := new(metav1.NewTime(now))
	errTest := errors.New("test error")
	limit := utiltestingalpha.MakePreemptionLimit("limit", kueuealpha.PreemptorCQLimitScope).
		Limit(10).
		LimitWindowSeconds(60).
		Obj()
	// recorded holds the preemption events of the tracker, by timestamp.
	type recorded map[time.Time]policy.ConsumedLimits
	seeded := recorded{
		expired: {"limit": "cq-a", "other-limit": "Global"},
		now:     {"limit": "cq-b", "other-limit": "Global"},
	}
	multiplePreemptions := recorded{
		now.Add(-7 * time.Second): {"limit": "cq-b"},
		now.Add(-6 * time.Second): {"limit": "cq-d"},
		now.Add(-5 * time.Second): {"limit": "cq-a"},
		now.Add(-4 * time.Second): {"limit": "cq-c"},
		now.Add(-3 * time.Second): {"limit": "cq-d"},
		now.Add(-2 * time.Second): {"limit": "cq-a"},
		now.Add(-1 * time.Second): {"limit": "cq-c"},
		now:                       {"limit": "cq-c"},
	}
	overMaxPreemptions := make(recorded, preemptionLimitCountMaxItems+2)
	overMaxPreemptions[now] = policy.ConsumedLimits{"limit": "high"}
	overMaxPreemptions[now.Add(-time.Millisecond)] = policy.ConsumedLimits{"limit": "high"}
	for i := range preemptionLimitCountMaxItems {
		ts := now.Add(-time.Duration(i+2) * time.Millisecond)
		overMaxPreemptions[ts] = policy.ConsumedLimits{
			"limit": policy.PreemptionLimitScopeValue(fmt.Sprintf("cq-%04d", i)),
		}
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
		limit           *kueuealpha.PreemptionLimit
		recorded        recorded
		clientFuncs     interceptor.Funcs
		want            recorded
		wantResult      reconcile.Result
		wantLimitStatus *kueuealpha.PreemptionLimitStatus
		wantErr         error
	}{
		"existing limit: expired events are pruned, other limits are untouched": {
			limit:    limit,
			recorded: seeded,
			want: recorded{
				expired: {"other-limit": "Global"},
				now:     {"limit": "cq-b", "other-limit": "Global"},
			},
			wantResult: reconcile.Result{RequeueAfter: time.Minute},
			wantLimitStatus: &kueuealpha.PreemptionLimitStatus{
				LastUpdateTime: lastUpdateTime,
				Counts: []kueuealpha.PreemptionLimitCount{
					{Name: "cq-b", Value: 1},
				},
			},
		},
		"no active events in window produces empty status counts": {
			limit: limit,
			recorded: recorded{
				expired: {"limit": "cq-a"},
			},
			wantResult: reconcile.Result{RequeueAfter: time.Minute},
			wantLimitStatus: &kueuealpha.PreemptionLimitStatus{
				LastUpdateTime: lastUpdateTime,
			},
		},
		"status counts are sorted by value descending and then by name ascending": {
			limit:      limit,
			recorded:   multiplePreemptions,
			want:       multiplePreemptions,
			wantResult: reconcile.Result{RequeueAfter: time.Minute},
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
			want:       overMaxPreemptions,
			wantResult: reconcile.Result{RequeueAfter: time.Minute},
			wantLimitStatus: &kueuealpha.PreemptionLimitStatus{
				LastUpdateTime: lastUpdateTime,
				Counts:         wantTopCounts,
			},
		},
		"deleted limit is forgotten, other limits are untouched": {
			recorded: seeded,
			want: recorded{
				expired: {"other-limit": "Global"},
				now:     {"other-limit": "Global"},
			},
		},
		"get error other than NotFound is returned and the tracker is unchanged": {
			limit:    limit,
			recorded: seeded,
			clientFuncs: interceptor.Funcs{
				Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
					return errTest
				},
			},
			want:    seeded,
			wantErr: errTest,
		},
		"status update error is returned": {
			limit:    limit,
			recorded: seeded,
			clientFuncs: interceptor.Funcs{
				SubResourceUpdate: func(context.Context, client.Client, string, client.Object, ...client.SubResourceUpdateOption) error {
					return errTest
				},
			},
			want: recorded{
				expired: {"other-limit": "Global"},
				now:     {"limit": "cq-b", "other-limit": "Global"},
			},
			wantErr: errTest,
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			ctx, _ := utiltesting.ContextWithLog(t)
			builder := utiltesting.NewClientBuilder().WithInterceptorFuncs(tc.clientFuncs)
			if tc.limit != nil {
				builder = builder.WithObjects(tc.limit).WithStatusSubresource(tc.limit)
			}
			// Pruning changes no count, only the stored events, so the whole tracker
			// is compared. Outside the limits package a tracker can only be filled
			// through Record, which the limits package tests cover.
			tracker := limits.NewPreemptionLimitTracker()
			for ts, consumedLimits := range tc.recorded {
				tracker.Record(ts, consumedLimits)
			}
			client := builder.Build()
			reconciler := NewPreemptionLimitReconciler(client, tracker, nil)
			reconciler.clock = testingclock.NewFakeClock(now)

			gotResult, gotErr := reconciler.Reconcile(ctx, reconcile.Request{Name: "limit"})
			if diff := cmp.Diff(tc.wantErr, gotErr, cmpopts.EquateErrors()); diff != "" {
				t.Errorf("Unexpected Reconcile() error (-want,+got):\n%s", diff)
			}
			if diff := cmp.Diff(tc.wantResult, gotResult); diff != "" {
				t.Errorf("Unexpected Reconcile() result (-want,+got):\n%s", diff)
			}
			wantTracker := limits.NewPreemptionLimitTracker()
			for ts, consumedLimits := range tc.want {
				wantTracker.Record(ts, consumedLimits)
			}
			if diff := cmp.Diff(wantTracker, tracker,
				cmp.AllowUnexported(limits.PreemptionLimitTracker{}),
				cmpopts.IgnoreTypes(sync.RWMutex{}),
			); diff != "" {
				t.Errorf("Unexpected tracked preemptions (-want,+got):\n%s", diff)
			}

			if tc.wantLimitStatus != nil {
				limit := &kueuealpha.PreemptionLimit{}
				if err := client.Get(ctx, types.NamespacedName{Name: "limit"}, limit); err != nil {
					t.Errorf("PreemptionLimit not found but expected: %v", err)
				}

				if diff := cmp.Diff(*tc.wantLimitStatus, limit.Status); diff != "" {
					t.Errorf("Unexpected PreemptionLimitStatus (-want,+got):\n%s", diff)
				}
			}
		})
	}
}
