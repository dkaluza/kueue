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
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/utils/clock"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	kueuealpha "sigs.k8s.io/kueue/apis/kueue/v1alpha1"
	"sigs.k8s.io/kueue/pkg/scheduler/preemption/limits"
	"sigs.k8s.io/kueue/pkg/scheduler/preemption/policy"
	"sigs.k8s.io/kueue/pkg/util/roletracker"
)

// preemptionLimitPruneInterval is how often the expired preemption events of
// a PreemptionLimit are pruned.
const preemptionLimitPruneInterval = time.Minute

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

// Reconcile prunes the preemption events of the PreemptionLimit that fell out
// of its window, every preemptionLimitPruneInterval, and forgets all of them
// once the PreemptionLimit is deleted.
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
	r.tracker.Prune(r.clock.Now(), &limit)
	return ctrl.Result{RequeueAfter: preemptionLimitPruneInterval}, nil
}

// SetupWithManager sets up the controller with the Manager. It keeps the default
// leader election, so the controller runs only on the leader, where the scheduler
// records the preemption events.
func (r *PreemptionLimitReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		Named("preemptionlimit_controller").
		For(&kueuealpha.PreemptionLimit{}).
		WithOptions(controller.Options{
			LogConstructor: roletracker.NewLogConstructor(r.roleTracker, "preemptionlimit-reconciler"),
		}).
		Complete(r)
}
