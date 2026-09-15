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

package ordering

import (
	"cmp"
	"time"

	"github.com/go-logr/logr"

	"sigs.k8s.io/kueue/pkg/scheduler/preemption/common"
	"sigs.k8s.io/kueue/pkg/util/priority"
	"sigs.k8s.io/kueue/pkg/workload"
)

// NewComparator returns a comparator function that compares two candidate workloads
// according to candidate ordering rules and UID tie-breaking:
// 1. Priority (Ascending: lowest priority first)
// 2. AdmissionTimestamp (Descending: most recently admitted first, protecting long-running workloads)
// 3. UID (Ascending: deterministic tie-breaker)
func NewComparator(
	log logr.Logger,
	now time.Time,
) func(a, b *workload.Info) int {
	return func(a, b *workload.Info) int {
		if a == b {
			return 0
		}

		if res := comparePriority(log, a, b); res != 0 {
			return res
		}

		if res := compareAdmissionTimestamp(a, b, now); res != 0 {
			// Descending: most recently admitted first
			return -res
		}

		return compareUID(a, b)
	}
}

func comparePriority(log logr.Logger, a, b *workload.Info) int {
	prioA := priority.EffectivePriority(log, a.Obj)
	prioB := priority.EffectivePriority(log, b.Obj)
	return cmp.Compare(prioA, prioB)
}

func compareAdmissionTimestamp(a, b *workload.Info, now time.Time) int {
	timestampA := common.QuotaReservationTime(a.Obj, now)
	timestampB := common.QuotaReservationTime(b.Obj, now)
	return timestampA.Compare(timestampB)
}

func compareUID(a, b *workload.Info) int {
	return cmp.Compare(a.Obj.UID, b.Obj.UID)
}
