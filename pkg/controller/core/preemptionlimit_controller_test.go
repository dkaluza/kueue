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
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
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
	errTest := errors.New("test error")
	limit := utiltestingalpha.MakePreemptionLimit("limit", kueuealpha.PreemptingCQLimitScope).
		LimitWindowSeconds(60).
		Obj()
	// recorded holds the preemption events of the tracker, by timestamp.
	type recorded map[time.Time]policy.ConsumedLimits
	seeded := recorded{
		now.Add(-time.Hour): {"limit": "cq-a", "other-limit": "Global"},
		now:                 {"limit": "cq-b", "other-limit": "Global"},
	}

	cases := map[string]struct {
		limit       *kueuealpha.PreemptionLimit
		clientFuncs interceptor.Funcs
		want        recorded
		wantResult  reconcile.Result
		wantErr     error
	}{
		"existing limit: expired events are pruned, other limits are untouched": {
			limit: limit,
			want: recorded{
				now.Add(-time.Hour): {"other-limit": "Global"},
				now:                 {"limit": "cq-b", "other-limit": "Global"},
			},
			wantResult: reconcile.Result{RequeueAfter: time.Minute},
		},
		"deleted limit is forgotten, other limits are untouched": {
			want: recorded{
				now.Add(-time.Hour): {"other-limit": "Global"},
				now:                 {"other-limit": "Global"},
			},
		},
		"get error other than NotFound is returned and the tracker is unchanged": {
			limit: limit,
			clientFuncs: interceptor.Funcs{
				Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
					return errTest
				},
			},
			want:    seeded,
			wantErr: errTest,
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			ctx, _ := utiltesting.ContextWithLog(t)
			builder := utiltesting.NewClientBuilder().WithInterceptorFuncs(tc.clientFuncs)
			if tc.limit != nil {
				builder = builder.WithObjects(tc.limit)
			}
			// Pruning changes no count, only the stored events, so the whole tracker
			// is compared. Outside the limits package a tracker can only be filled
			// through Record, which the limits package tests cover.
			tracker := limits.NewPreemptionLimitTracker()
			for ts, consumedLimits := range seeded {
				tracker.Record(ts, consumedLimits)
			}
			reconciler := NewPreemptionLimitReconciler(builder.Build(), tracker, nil)
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
		})
	}
}
