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
	"cmp"
	"context"
	"slices"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/clock"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	kueuealpha "sigs.k8s.io/kueue/apis/kueue/v1alpha1"
	"sigs.k8s.io/kueue/pkg/scheduler/preemption/limits"
	"sigs.k8s.io/kueue/pkg/scheduler/preemption/policy"
	"sigs.k8s.io/kueue/pkg/util/roletracker"
)

// preemptionLimitPruneInterval is how often the expired preemption events of
// a PreemptionLimit are pruned.
const preemptionLimitPruneInterval = time.Minute

const preemptionLimitCountMaxItems = 1000

// PreemptionLimitReconciler aligns the in-memory representation of the PreemptionLimits
// (see PreemptionLimitTracker) with their current specs.
type PreemptionLimitReconciler struct {
	client      client.Client
	tracker     *limits.PreemptionLimitTracker
	clock       clock.Clock
	roleTracker *roletracker.RoleTracker
}

var _ reconcile.Reconciler = (*PreemptionLimitReconciler)(nil)

func NewPreemptionLimitReconciler(
	client client.Client,
	tracker *limits.PreemptionLimitTracker,
	roleTracker *roletracker.RoleTracker,
) *PreemptionLimitReconciler {
	return &PreemptionLimitReconciler{
		client:      client,
		tracker:     tracker,
		clock:       realClock,
		roleTracker: roleTracker,
	}
}

// +kubebuilder:rbac:groups=kueue.x-k8s.io,resources=preemptionlimits,verbs=get;list;watch
// +kubebuilder:rbac:groups=kueue.x-k8s.io,resources=preemptionlimits/status,verbs=get;update;patch

// Reconcile prunes the preemption events of the PreemptionLimit that fell out
// of its window, every preemptionLimitPruneInterval, and forgets all of them
// once the PreemptionLimit is deleted.
// Additionaly it updates PreemptionLimit.Status field based on current
// state of preemptions.
func (r *PreemptionLimitReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var limit kueuealpha.PreemptionLimit
	if err := r.client.Get(ctx, req.NamespacedName, &limit); err != nil {
		if apierrors.IsNotFound(err) {
			// A preemption picked before the deletion can still be recorded after
			// it, leaving a few orphaned events until restart, or until a recreated
			// PreemptionLimit with the same name prunes them. Accepted as negligible.
			ctrl.LoggerFrom(ctx).V(2).Info("PreemptionLimit is deleted, forgetting its preemption events")
			r.tracker.Forget(policy.PreemptionLimitReference(req.Name))
		}
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	now := r.clock.Now()
	r.tracker.Prune(now, &limit)

	limit.Status = r.preemptionLimitStatus(now, &limit)
	if err := r.client.Status().Update(ctx, &limit); err != nil {
		// "NotFound" error will be handled on retry.
		return ctrl.Result{}, err
	}

	return ctrl.Result{RequeueAfter: preemptionLimitPruneInterval}, nil
}

func (r *PreemptionLimitReconciler) preemptionLimitStatus(now time.Time, limit *kueuealpha.PreemptionLimit) kueuealpha.PreemptionLimitStatus {
	counts := r.tracker.Counts(now, []*kueuealpha.PreemptionLimit{limit})

	lastUpdateTime := metav1.NewTime(now)
	status := kueuealpha.PreemptionLimitStatus{
		LastUpdateTime: &lastUpdateTime,
	}

	countsForLimit := counts[policy.PreemptionLimitReference(limit.Name)]
	status.Counts = make([]kueuealpha.PreemptionLimitCount, 0, len(countsForLimit))
	for scope, count := range countsForLimit {
		status.Counts = append(status.Counts, kueuealpha.PreemptionLimitCount{
			// No need to additionally validate name length because it is created from
			// existing validated values and maxLength is calculated as combination of
			// existing name length constraints.
			Name:  string(scope),
			Value: count,
		})
	}

	slices.SortFunc(status.Counts, func(a, b kueuealpha.PreemptionLimitCount) int {
		return cmp.Or(-cmp.Compare(a.Value, b.Value), cmp.Compare(a.Name, b.Name))
	})

	if len(status.Counts) > preemptionLimitCountMaxItems {
		status.Counts = status.Counts[:preemptionLimitCountMaxItems]
	}

	return status
}

// SetupWithManager sets up the controller with the Manager. It keeps the default
// leader election, so the controller runs only on the leader, where the scheduler
// records the preemption events.
func (r *PreemptionLimitReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		Named("preemptionlimit_controller").
		// Make sure that update to the status part aren't triggering additional Reconcile.
		For(&kueuealpha.PreemptionLimit{}, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		WithOptions(controller.Options{
			LogConstructor: roletracker.NewLogConstructor(r.roleTracker, "preemptionlimit-reconciler"),
		}).
		Complete(r)
}
