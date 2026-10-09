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
	kueuealpha "sigs.k8s.io/kueue/apis/kueue/v1alpha1"
	"sigs.k8s.io/kueue/pkg/scheduler/preemption/policy"
	"sigs.k8s.io/kueue/pkg/workload"
)

// ConsumedLimits returns, for each PreemptionLimit, the scope value consumed by
// preempting target to admit preemptor.
func ConsumedLimits(preemptionLimits []kueuealpha.PreemptionLimit, preemptor, target *workload.Info) map[policy.PreemptionLimitReference]policy.PreemptionLimitScopeValue {
	if len(preemptionLimits) == 0 {
		return nil
	}
	consumed := make(map[policy.PreemptionLimitReference]policy.PreemptionLimitScopeValue, len(preemptionLimits))
	for i := range preemptionLimits {
		limit := &preemptionLimits[i]
		consumed[policy.PreemptionLimitReference(limit.Name)] = scopeValue(limit.Spec.Scope, preemptor, target)
	}
	return consumed
}

func scopeValue(scope kueuealpha.PreemptionLimitScope, preemptor, target *workload.Info) policy.PreemptionLimitScopeValue {
	switch scope {
	case kueuealpha.PreemptingCQLimitScope:
		return policy.PreemptionLimitScopeValue(preemptor.ClusterQueue)
	case kueuealpha.PreemptedCQLimitScope:
		return policy.PreemptionLimitScopeValue(target.ClusterQueue)
	case kueuealpha.PreemptedWorkloadLimitScope:
		return policy.PreemptionLimitScopeValue(workload.Key(target.Obj))
	default:
		// GlobalPreemptionLimitScope, as the API admits no other scope.
		return policy.PreemptionLimitScopeValue(kueuealpha.GlobalPreemptionLimitScope)
	}
}
