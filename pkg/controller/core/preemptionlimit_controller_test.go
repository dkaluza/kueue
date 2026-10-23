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
	"reflect"
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
	expired := now.Add(-time.Hour)
	errTest := errors.New("test error")
	limit := utiltestingalpha.MakePreemptionLimit("limit", kueuealpha.PreemptingCQLimitScope).UID("uid").LimitWindowSeconds(60).Obj()
	otherLimit := utiltestingalpha.MakePreemptionLimit("other-limit", kueuealpha.GlobalPreemptionLimitScope).UID("other-uid").LimitWindowSeconds(60).Obj()
	consumed := func(l *kueuealpha.PreemptionLimit, v policy.PreemptionLimitScopeValue) policy.ConsumedLimit {
		return policy.ConsumedLimit{UID: l.UID, Scope: l.Spec.Scope, ScopeValue: v}
	}
	// recorded holds the preemption events of the tracker, by timestamp.
	type recorded map[time.Time]policy.ConsumedLimits
	// newTracker syncs the given limits, as only synced limits are recorded.
	// Outside the limits package a tracker can only be built through its
	// methods, which the limits package tests cover.
	newTracker := func(synced []*kueuealpha.PreemptionLimit, r recorded) *limits.PreemptionLimitTracker {
		tracker := limits.NewPreemptionLimitTracker()
		for _, l := range synced {
			tracker.Sync(now, l)
		}
		for ts, consumedLimits := range r {
			tracker.Record(ts, consumedLimits)
		}
		return tracker
	}
	limitsPkg := reflect.TypeFor[limits.PreemptionLimitTracker]().PkgPath()
	seeded := recorded{
		expired: {"limit": consumed(limit, "cq-a"), "other-limit": consumed(otherLimit, "Global")},
		now:     {"limit": consumed(limit, "cq-b"), "other-limit": consumed(otherLimit, "Global")},
	}

	cases := map[string]struct {
		limit        *kueuealpha.PreemptionLimit
		clientFuncs  interceptor.Funcs
		wantLimits   []*kueuealpha.PreemptionLimit
		wantRecorded recorded
		wantResult   reconcile.Result
		wantErr      error
	}{
		"existing limit: expired events are pruned, other limits are untouched": {
			limit:      limit,
			wantLimits: []*kueuealpha.PreemptionLimit{limit, otherLimit},
			wantRecorded: recorded{
				expired: {"other-limit": consumed(otherLimit, "Global")},
				now:     {"limit": consumed(limit, "cq-b"), "other-limit": consumed(otherLimit, "Global")},
			},
			wantResult: reconcile.Result{RequeueAfter: time.Minute},
		},
		"deleted limit is forgotten, other limits are untouched": {
			wantLimits: []*kueuealpha.PreemptionLimit{otherLimit},
			wantRecorded: recorded{
				expired: {"other-limit": consumed(otherLimit, "Global")},
				now:     {"other-limit": consumed(otherLimit, "Global")},
			},
		},
		"get error other than NotFound is returned and the tracker is unchanged": {
			limit: limit,
			clientFuncs: interceptor.Funcs{
				Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
					return errTest
				},
			},
			wantLimits:   []*kueuealpha.PreemptionLimit{limit, otherLimit},
			wantRecorded: seeded,
			wantErr:      errTest,
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			ctx, _ := utiltesting.ContextWithLog(t)
			builder := utiltesting.NewClientBuilder().WithInterceptorFuncs(tc.clientFuncs)
			if tc.limit != nil {
				builder = builder.WithObjects(tc.limit)
			}
			tracker := newTracker([]*kueuealpha.PreemptionLimit{limit, otherLimit}, seeded)
			reconciler := NewPreemptionLimitReconciler(builder.Build(), tracker, nil)
			reconciler.clock = testingclock.NewFakeClock(now)

			gotResult, gotErr := reconciler.Reconcile(ctx, reconcile.Request{Name: "limit"})
			if diff := cmp.Diff(tc.wantErr, gotErr, cmpopts.EquateErrors()); diff != "" {
				t.Errorf("Unexpected Reconcile() error (-want,+got):\n%s", diff)
			}
			if diff := cmp.Diff(tc.wantResult, gotResult); diff != "" {
				t.Errorf("Unexpected Reconcile() result (-want,+got):\n%s", diff)
			}
			if diff := cmp.Diff(newTracker(tc.wantLimits, tc.wantRecorded), tracker,
				cmp.Exporter(func(t reflect.Type) bool { return t.PkgPath() == limitsPkg }),
				cmpopts.IgnoreTypes(sync.RWMutex{}),
				cmpopts.EquateEmpty(),
			); diff != "" {
				t.Errorf("Unexpected tracked preemptions (-want,+got):\n%s", diff)
			}
		})
	}
}
