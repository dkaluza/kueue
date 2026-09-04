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
	"slices"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	kueue "sigs.k8s.io/kueue/apis/kueue/v1beta2"
	schdcache "sigs.k8s.io/kueue/pkg/cache/scheduler"
	controllerconstants "sigs.k8s.io/kueue/pkg/controller/constants"
	"sigs.k8s.io/kueue/pkg/features"
	configtesting "sigs.k8s.io/kueue/pkg/scheduler/preemption/config/testing"
	utiltesting "sigs.k8s.io/kueue/pkg/util/testing"
	utiltestingapi "sigs.k8s.io/kueue/pkg/util/testing/v1beta2"
	"sigs.k8s.io/kueue/pkg/workload"
)

func TestCompareCandidates(t *testing.T) {
	features.SetFeatureGateDuringTest(t, features.PriorityBoost, true)

	now := time.Now()
	t1 := now.Add(-10 * time.Minute)
	t2 := now.Add(-5 * time.Minute)
	t3 := now.Add(-1 * time.Minute)

	baseWorkload := func(name, uid, cq string) *workload.Info {
		wl := utiltestingapi.MakeWorkload(name, "ns").
			UID(types.UID(uid)).
			Obj()
		info := workload.NewInfo(wl)
		info.ClusterQueue = kueue.ClusterQueueReference(cq)
		return info
	}

	withPriority := func(info *workload.Info, p int32) *workload.Info {
		info.Obj.Spec.Priority = &p
		return info
	}

	withPriorityBoost := func(info *workload.Info, boost string) *workload.Info {
		if info.Obj.Annotations == nil {
			info.Obj.Annotations = make(map[string]string)
		}
		info.Obj.Annotations[controllerconstants.PriorityBoostAnnotationKey] = boost
		return info
	}

	withReservationTime := func(info *workload.Info, tm time.Time) *workload.Info {
		info.Obj.Status.Conditions = append(info.Obj.Status.Conditions, metav1.Condition{
			Type:               kueue.WorkloadQuotaReserved,
			Status:             metav1.ConditionTrue,
			LastTransitionTime: metav1.Time{Time: tm},
		})
		return info
	}

	_, log := utiltesting.ContextWithLog(t)

	// Cohort structure:
	// rootA
	//  ├── cq-a1
	//  ├── cq-a3 (sibling in rootA)
	//  └── subA
	//       └── cq-a2
	// rootB
	//  └── cq-b1
	// standalone-cq (no cohort)
	snap := configtesting.NewSnapshotBuilder().
		Cohort("rootA", "").
		Cohort("subA", "rootA").
		Cohort("rootB", "").
		ClusterQueue("cq-a1", "rootA").
		ClusterQueue("cq-a3", "rootA").
		ClusterQueue("cq-a2", "subA").
		ClusterQueue("cq-b1", "rootB").
		ClusterQueue("standalone-cq", "").
		Build()

	const (
		wantLess    = -1 // a comes before b
		wantEqual   = 0  // a and b have equal precedence
		wantGreater = 1  // a comes after b
	)

	preemptor := baseWorkload("preemptor", "p-uid", "cq-a1")

	tests := map[string]struct {
		ordering  []kueue.Order
		a         *workload.Info
		b         *workload.Info
		preemptor *workload.Info
		snapshot  *schdcache.Snapshot
		want      int
	}{
		"same workload identity comparison returns 0": {
			ordering:  []kueue.Order{{OrderingField: kueue.Priority}},
			a:         preemptor,
			b:         preemptor,
			preemptor: preemptor,
			snapshot:  snap,
			want:      wantEqual,
		},
		"Priority Ascending (default): lower priority comes first (a < b)": {
			ordering:  []kueue.Order{{OrderingField: kueue.Priority}},
			a:         withPriority(baseWorkload("a", "same-uid", "cq-a1"), 10),
			b:         withPriority(baseWorkload("b", "same-uid", "cq-a1"), 20),
			preemptor: preemptor,
			snapshot:  snap,
			want:      wantLess,
		},
		"Priority Ascending (default): lower priority comes first (a > b)": {
			ordering:  []kueue.Order{{OrderingField: kueue.Priority}},
			a:         withPriority(baseWorkload("a", "same-uid", "cq-a1"), 100),
			b:         withPriority(baseWorkload("b", "same-uid", "cq-a1"), 50),
			preemptor: preemptor,
			snapshot:  snap,
			want:      wantGreater,
		},
		"Priority Ascending (explicit): lower priority comes first": {
			ordering:  []kueue.Order{{OrderingField: kueue.Priority, Direction: kueue.Ascending}},
			a:         withPriority(baseWorkload("a", "same-uid", "cq-a1"), 10),
			b:         withPriority(baseWorkload("b", "same-uid", "cq-a1"), 20),
			preemptor: preemptor,
			snapshot:  snap,
			want:      wantLess,
		},
		"Priority Descending: higher priority comes first (a < b)": {
			ordering:  []kueue.Order{{OrderingField: kueue.Priority, Direction: kueue.Descending}},
			a:         withPriority(baseWorkload("a", "same-uid", "cq-a1"), 10),
			b:         withPriority(baseWorkload("b", "same-uid", "cq-a1"), 20),
			preemptor: preemptor,
			snapshot:  snap,
			want:      wantGreater,
		},
		"Priority Descending: higher priority comes first (a > b)": {
			ordering:  []kueue.Order{{OrderingField: kueue.Priority, Direction: kueue.Descending}},
			a:         withPriority(baseWorkload("a", "same-uid", "cq-a1"), 100),
			b:         withPriority(baseWorkload("b", "same-uid", "cq-a1"), 50),
			preemptor: preemptor,
			snapshot:  snap,
			want:      wantLess,
		},
		"Priority Ascending: priority boost is respected": {
			ordering:  []kueue.Order{{OrderingField: kueue.Priority}},
			a:         withPriorityBoost(withPriority(baseWorkload("a", "same-uid", "cq-a1"), 10), "100"), // effective 110
			b:         withPriority(baseWorkload("b", "same-uid", "cq-a1"), 50),                           // effective 50
			preemptor: preemptor,
			snapshot:  snap,
			want:      wantGreater, // b has lower priority, so b comes first
		},
		"Priority Descending: priority boost is respected": {
			ordering:  []kueue.Order{{OrderingField: kueue.Priority, Direction: kueue.Descending}},
			a:         withPriorityBoost(withPriority(baseWorkload("a", "same-uid", "cq-a1"), 10), "100"), // effective 110
			b:         withPriority(baseWorkload("b", "same-uid", "cq-a1"), 50),                           // effective 50
			preemptor: preemptor,
			snapshot:  snap,
			want:      wantLess, // a has higher priority (110 vs 50), so a comes first
		},
		"AdmissionTimestamp Ascending (default): older admission comes first (t1 < t3)": {
			ordering:  []kueue.Order{{OrderingField: kueue.AdmissionTimestamp}},
			a:         withReservationTime(baseWorkload("a", "same-uid", "cq-a1"), t1), // older
			b:         withReservationTime(baseWorkload("b", "same-uid", "cq-a1"), t3), // more recent
			preemptor: preemptor,
			snapshot:  snap,
			want:      wantLess,
		},
		"AdmissionTimestamp Ascending (explicit): older admission comes first (t3 > t1)": {
			ordering:  []kueue.Order{{OrderingField: kueue.AdmissionTimestamp, Direction: kueue.Ascending}},
			a:         withReservationTime(baseWorkload("a", "same-uid", "cq-a1"), t3), // more recent
			b:         withReservationTime(baseWorkload("b", "same-uid", "cq-a1"), t1), // older
			preemptor: preemptor,
			snapshot:  snap,
			want:      wantGreater,
		},
		"AdmissionTimestamp Descending: more recently admitted comes first (t3 > t1)": {
			ordering:  []kueue.Order{{OrderingField: kueue.AdmissionTimestamp, Direction: kueue.Descending}},
			a:         withReservationTime(baseWorkload("a", "same-uid", "cq-a1"), t3), // more recent
			b:         withReservationTime(baseWorkload("b", "same-uid", "cq-a1"), t1), // older
			preemptor: preemptor,
			snapshot:  snap,
			want:      wantLess,
		},
		"AdmissionTimestamp Descending: older comes after (t1 < t2)": {
			ordering:  []kueue.Order{{OrderingField: kueue.AdmissionTimestamp, Direction: kueue.Descending}},
			a:         withReservationTime(baseWorkload("a", "same-uid", "cq-a1"), t1), // older
			b:         withReservationTime(baseWorkload("b", "same-uid", "cq-a1"), t2), // more recent
			preemptor: preemptor,
			snapshot:  snap,
			want:      wantGreater,
		},
		"AdmissionTimestamp Ascending: missing condition falls back to now (older t1 < now)": {
			ordering:  []kueue.Order{{OrderingField: kueue.AdmissionTimestamp}},
			a:         baseWorkload("a", "same-uid", "cq-a1"),                          // now
			b:         withReservationTime(baseWorkload("b", "same-uid", "cq-a1"), t1), // older
			preemptor: preemptor,
			snapshot:  snap,
			want:      wantGreater, // b is older (t1 < now) -> b comes first (a > b)
		},
		"AdmissionTimestamp Descending: missing condition falls back to now (now > older t1)": {
			ordering:  []kueue.Order{{OrderingField: kueue.AdmissionTimestamp, Direction: kueue.Descending}},
			a:         baseWorkload("a", "same-uid", "cq-a1"),                          // now
			b:         withReservationTime(baseWorkload("b", "same-uid", "cq-a1"), t1), // older
			preemptor: preemptor,
			snapshot:  snap,
			want:      wantLess, // a was just admitted (now > t1) -> a comes first
		},
		"AdmissionTimestamp: ConditionFalse treated as unreserved (now)": {
			ordering: []kueue.Order{{OrderingField: kueue.AdmissionTimestamp, Direction: kueue.Descending}},
			a: func() *workload.Info {
				wl := baseWorkload("a", "same-uid", "cq-a1")
				wl.Obj.Status.Conditions = append(wl.Obj.Status.Conditions, metav1.Condition{
					Type:               kueue.WorkloadQuotaReserved,
					Status:             metav1.ConditionFalse,
					LastTransitionTime: metav1.Time{Time: t1},
				})
				return wl
			}(),
			b:         withReservationTime(baseWorkload("b", "same-uid", "cq-a1"), t1), // True condition at t1
			preemptor: preemptor,
			snapshot:  snap,
			want:      wantLess, // a has False condition -> treated as now (now > t1) -> a comes first in Descending
		},
		"IsOtherCQ Ascending (default): same CQ before other CQ": {
			ordering:  []kueue.Order{{OrderingField: kueue.IsOtherCQ}},
			a:         baseWorkload("a", "same-uid", "cq-a1"), // same CQ as preemptor (false)
			b:         baseWorkload("b", "same-uid", "cq-b1"), // other CQ (true)
			preemptor: preemptor,
			snapshot:  snap,
			want:      wantLess,
		},
		"IsOtherCQ Ascending (default): other CQ after same CQ": {
			ordering:  []kueue.Order{{OrderingField: kueue.IsOtherCQ}},
			a:         baseWorkload("a", "same-uid", "cq-b1"), // other CQ (true)
			b:         baseWorkload("b", "same-uid", "cq-a1"), // same CQ as preemptor (false)
			preemptor: preemptor,
			snapshot:  snap,
			want:      wantGreater,
		},
		"IsOtherCQ Descending: other CQ before same CQ": {
			ordering:  []kueue.Order{{OrderingField: kueue.IsOtherCQ, Direction: kueue.Descending}},
			a:         baseWorkload("a", "same-uid", "cq-b1"), // other CQ (true)
			b:         baseWorkload("b", "same-uid", "cq-a1"), // same CQ as preemptor (false)
			preemptor: preemptor,
			snapshot:  snap,
			want:      wantLess,
		},
		"IsOtherCQ Descending: same CQ after other CQ": {
			ordering:  []kueue.Order{{OrderingField: kueue.IsOtherCQ, Direction: kueue.Descending}},
			a:         baseWorkload("a", "same-uid", "cq-a1"), // same CQ as preemptor (false)
			b:         baseWorkload("b", "same-uid", "cq-b1"), // other CQ (true)
			preemptor: preemptor,
			snapshot:  snap,
			want:      wantGreater,
		},
		"IsOtherCQ: both other CQs tie-break to UID": {
			ordering:  []kueue.Order{{OrderingField: kueue.IsOtherCQ}},
			a:         baseWorkload("a", "uid-1", "cq-b1"), // other CQ
			b:         baseWorkload("b", "uid-2", "cq-a2"), // other CQ
			preemptor: preemptor,
			snapshot:  snap,
			want:      wantLess, // "uid-1" < "uid-2"
		},
		"IsOtherCohort Ascending (default): same cohort before other cohort (rootA vs rootB)": {
			ordering:  []kueue.Order{{OrderingField: kueue.IsOtherCohort}},
			a:         baseWorkload("a", "same-uid", "cq-a1"), // rootA (same cohort as preemptor, false)
			b:         baseWorkload("b", "same-uid", "cq-b1"), // rootB (other cohort, true)
			preemptor: preemptor,
			snapshot:  snap,
			want:      wantLess,
		},
		"IsOtherCohort Ascending (default): other cohort (standalone) after same cohort (cq-a3 under rootA)": {
			ordering:  []kueue.Order{{OrderingField: kueue.IsOtherCohort}},
			a:         baseWorkload("a", "same-uid", "standalone-cq"), // no cohort -> other cohort (true)
			b:         baseWorkload("b", "same-uid", "cq-a3"),         // sibling CQ under rootA -> same cohort (false)
			preemptor: preemptor,
			snapshot:  snap,
			want:      wantGreater, // b is same cohort (false < true) -> b comes first (a > b)
		},
		"IsOtherCohort Descending: other cohort before same cohort (rootB vs rootA)": {
			ordering:  []kueue.Order{{OrderingField: kueue.IsOtherCohort, Direction: kueue.Descending}},
			a:         baseWorkload("a", "same-uid", "cq-b1"), // rootB (other cohort, true)
			b:         baseWorkload("b", "same-uid", "cq-a1"), // rootA (same cohort, false)
			preemptor: preemptor,
			snapshot:  snap,
			want:      wantLess,
		},
		"IsOtherCohort Descending: same cohort after other cohort (cq-a3 vs standalone)": {
			ordering:  []kueue.Order{{OrderingField: kueue.IsOtherCohort, Direction: kueue.Descending}},
			a:         baseWorkload("a", "same-uid", "cq-a3"),         // sibling CQ under rootA -> same cohort (false)
			b:         baseWorkload("b", "same-uid", "standalone-cq"), // no cohort -> other cohort (true)
			preemptor: preemptor,
			snapshot:  snap,
			want:      wantGreater, // b is other cohort -> in Descending, b comes first (a > b)
		},
		"Deterministic Tie-breaker: UID comparison when ordering fields are equal": {
			ordering:  []kueue.Order{{OrderingField: kueue.Priority}},
			a:         withPriority(baseWorkload("a", "uid-aaa", "cq-a1"), 100),
			b:         withPriority(baseWorkload("b", "uid-zzz", "cq-a1"), 100),
			preemptor: preemptor,
			snapshot:  snap,
			want:      wantLess, // "uid-aaa" < "uid-zzz"
		},
		"Default ordering when ordering is empty: lower priority comes first (Priority Ascending)": {
			ordering:  []kueue.Order{},
			a:         withPriority(baseWorkload("a", "uid-9", "cq-a1"), 10),
			b:         withPriority(baseWorkload("b", "uid-1", "cq-a1"), 20),
			preemptor: preemptor,
			snapshot:  snap,
			want:      wantLess, // 10 < 20
		},
		"Default ordering when ordering is nil: lower priority comes first (Priority Ascending)": {
			ordering:  nil,
			a:         withPriority(baseWorkload("a", "uid-9", "cq-a1"), 10),
			b:         withPriority(baseWorkload("b", "uid-1", "cq-a1"), 20),
			preemptor: preemptor,
			snapshot:  snap,
			want:      wantLess, // 10 < 20
		},
		"Default ordering when ordering is empty: equal priority orders more recently admitted first (AdmissionTimestamp Descending)": {
			ordering:  []kueue.Order{},
			a:         withReservationTime(withPriority(baseWorkload("a", "uid-9", "cq-a1"), 10), t3), // more recent
			b:         withReservationTime(withPriority(baseWorkload("b", "uid-1", "cq-a1"), 10), t1), // older
			preemptor: preemptor,
			snapshot:  snap,
			want:      wantLess, // more recent comes first in Descending
		},
		"Default ordering when ordering is empty: equal priority and timestamp tie-breaks to UID (Ascending)": {
			ordering:  []kueue.Order{},
			a:         withReservationTime(withPriority(baseWorkload("a", "uid-2", "cq-a1"), 10), t1),
			b:         withReservationTime(withPriority(baseWorkload("b", "uid-1", "cq-a1"), 10), t1),
			preemptor: preemptor,
			snapshot:  snap,
			want:      wantGreater, // "uid-2" > "uid-1"
		},
		"Multi-key chain: Priority (Ascending) -> AdmissionTimestamp (Descending) -> IsOtherCQ (Descending)": {
			ordering: []kueue.Order{
				{OrderingField: kueue.Priority, Direction: kueue.Ascending},
				{OrderingField: kueue.AdmissionTimestamp, Direction: kueue.Descending},
				{OrderingField: kueue.IsOtherCQ, Direction: kueue.Descending},
			},
			// Equal priority, different admission timestamp -> more recent admission timestamp wins in Descending
			a:         withReservationTime(withPriority(baseWorkload("a", "uid-9", "cq-a1"), 100), t3),
			b:         withReservationTime(withPriority(baseWorkload("b", "uid-1", "cq-b1"), 100), t1),
			preemptor: preemptor,
			snapshot:  snap,
			want:      wantLess, // a is more recently admitted (t3 > t1)
		},
		"Multi-key chain: equal priority and admission time falls back to IsOtherCQ Descending": {
			ordering: []kueue.Order{
				{OrderingField: kueue.Priority},
				{OrderingField: kueue.AdmissionTimestamp, Direction: kueue.Descending},
				{OrderingField: kueue.IsOtherCQ, Direction: kueue.Descending},
			},
			a:         withReservationTime(withPriority(baseWorkload("a", "uid-9", "cq-b1"), 100), t2), // other CQ (true)
			b:         withReservationTime(withPriority(baseWorkload("b", "uid-1", "cq-a1"), 100), t2), // same CQ (false)
			preemptor: preemptor,
			snapshot:  snap,
			want:      wantLess, // a is other CQ (true > false -> in Descending, a comes first)
		},
		"Multi-key chain: Priority Descending -> IsOtherCQ Descending": {
			ordering: []kueue.Order{
				{OrderingField: kueue.Priority, Direction: kueue.Descending},
				{OrderingField: kueue.IsOtherCQ, Direction: kueue.Descending},
			},
			// a has priority 100 (same CQ), b has priority 50 (other CQ) -> a comes first because priority is Descending
			a:         withPriority(baseWorkload("a", "uid-1", "cq-a1"), 100),
			b:         withPriority(baseWorkload("b", "uid-2", "cq-b1"), 50),
			preemptor: preemptor,
			snapshot:  snap,
			want:      wantLess,
		},
		"Unknown ordering field is ignored": {
			ordering: []kueue.Order{
				{OrderingField: "UnknownField"},
				{OrderingField: kueue.Priority},
			},
			a:         withPriority(baseWorkload("a", "same-uid", "cq-a1"), 10),
			b:         withPriority(baseWorkload("b", "same-uid", "cq-a1"), 20),
			preemptor: preemptor,
			snapshot:  snap,
			want:      wantLess,
		},
		"4-key ordering chain: IsOtherCohort (Descending) -> IsOtherCQ (Descending) -> Priority -> AdmissionTimestamp": {
			ordering: []kueue.Order{
				{OrderingField: kueue.IsOtherCohort, Direction: kueue.Descending},
				{OrderingField: kueue.IsOtherCQ, Direction: kueue.Descending},
				{OrderingField: kueue.Priority},
				{OrderingField: kueue.AdmissionTimestamp, Direction: kueue.Descending},
			},
			// a is in same cohort but different CQ, lower priority (10)
			// b is in same cohort, same CQ, higher priority (100)
			a:         withReservationTime(withPriority(baseWorkload("a", "same-uid", "cq-a3"), 10), t1),
			b:         withReservationTime(withPriority(baseWorkload("b", "same-uid", "cq-a1"), 100), t3),
			preemptor: preemptor,
			snapshot:  snap,
			want:      wantLess, // both same cohort (rootA), but a is other CQ (cq-a3 vs cq-a1) -> in Descending IsOtherCQ, a comes first
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			got := NewComparator(log, tc.ordering, tc.preemptor, tc.snapshot, now)(tc.a, tc.b)
			if got != tc.want {
				t.Errorf("NewComparator() = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestCandidateSortingWithMultiKeyComparator(t *testing.T) {
	now := time.Now()
	tOld := now.Add(-10 * time.Minute)
	tNew := now.Add(-1 * time.Minute)

	wl := func(name string, priority int32, tm time.Time, cq string, uid string) *workload.Info {
		obj := utiltestingapi.MakeWorkload(name, "ns").
			UID(types.UID(uid)).
			Priority(priority).
			Obj()
		obj.Status.Conditions = append(obj.Status.Conditions, metav1.Condition{
			Type:               kueue.WorkloadQuotaReserved,
			Status:             metav1.ConditionTrue,
			LastTransitionTime: metav1.Time{Time: tm},
		})
		info := workload.NewInfo(obj)
		info.ClusterQueue = kueue.ClusterQueueReference(cq)
		return info
	}

	w1 := wl("w1-prio10-old", 10, tOld, "cq-a", "uid-1")
	w2 := wl("w2-prio10-new", 10, tNew, "cq-a", "uid-2")
	w3 := wl("w3-prio20-new", 20, tNew, "cq-a", "uid-3")
	w4 := wl("w4-prio20-other-cq", 20, tNew, "cq-b", "uid-4")

	_, log := utiltesting.ContextWithLog(t)
	snap := configtesting.NewSnapshotBuilder().
		ClusterQueue("cq-a", "").
		ClusterQueue("cq-b", "").
		Build()

	preemptor := wl("preemptor", 100, now, "cq-a", "uid-p")

	// Ordering: Priority (Ascending) -> AdmissionTimestamp (Descending) -> IsOtherCQ (Descending)
	cmpFunc := NewComparator(
		log,
		[]kueue.Order{
			{OrderingField: kueue.Priority, Direction: kueue.Ascending},
			{OrderingField: kueue.AdmissionTimestamp, Direction: kueue.Descending},
			{OrderingField: kueue.IsOtherCQ, Direction: kueue.Descending},
		},
		preemptor,
		snap,
		now,
	)

	list := []*workload.Info{w4, w3, w1, w2}
	slices.SortFunc(list, cmpFunc)

	// Expected order:
	// Priority 10 first:
	//   Between w1 and w2: w2 is more recent (tNew > tOld) in Descending timestamp -> w2, then w1
	// Priority 20 next:
	//   Between w3 (same CQ) and w4 (other CQ): both tNew, w4 is other CQ (true) -> in Descending, w4 then w3
	wantNames := []string{"w2-prio10-new", "w1-prio10-old", "w4-prio20-other-cq", "w3-prio20-new"}
	var gotNames []string
	for _, item := range list {
		gotNames = append(gotNames, item.Obj.Name)
	}

	if diff := cmp.Diff(wantNames, gotNames); diff != "" {
		t.Errorf("Sorted order mismatch (-want +got):\n%s", diff)
	}
}

func TestDefaultOrderingCandidateSorting(t *testing.T) {
	now := time.Now()
	tOld := now.Add(-10 * time.Minute)
	tNew := now.Add(-1 * time.Minute)

	wl := func(name string, priority int32, tm time.Time, uid string) *workload.Info {
		obj := utiltestingapi.MakeWorkload(name, "ns").
			UID(types.UID(uid)).
			Priority(priority).
			Obj()
		obj.Status.Conditions = append(obj.Status.Conditions, metav1.Condition{
			Type:               kueue.WorkloadQuotaReserved,
			Status:             metav1.ConditionTrue,
			LastTransitionTime: metav1.Time{Time: tm},
		})
		info := workload.NewInfo(obj)
		info.ClusterQueue = "cq-a"
		return info
	}

	w1 := wl("w1-prio10-old", 10, tOld, "uid-1")
	w2 := wl("w2-prio10-new", 10, tNew, "uid-2")
	w3 := wl("w3-prio20-old", 20, tOld, "uid-3")
	w4 := wl("w4-prio20-new", 20, tNew, "uid-4")

	_, log := utiltesting.ContextWithLog(t)
	snap := configtesting.NewSnapshotBuilder().
		ClusterQueue("cq-a", "").
		Build()

	preemptor := wl("preemptor", 100, now, "uid-p")

	// Empty ordering should fall back to default: Priority (Ascending) -> AdmissionTimestamp (Descending) -> UID (Ascending)
	cmpFunc := NewComparator(log, nil, preemptor, snap, now)

	list := []*workload.Info{w3, w1, w4, w2}
	slices.SortFunc(list, cmpFunc)

	// Expected order:
	// Priority 10 first: w2 (newer, tNew) before w1 (older, tOld)
	// Priority 20 next: w4 (newer, tNew) before w3 (older, tOld)
	wantNames := []string{"w2-prio10-new", "w1-prio10-old", "w4-prio20-new", "w3-prio20-old"}
	var gotNames []string
	for _, item := range list {
		gotNames = append(gotNames, item.Obj.Name)
	}

	if diff := cmp.Diff(wantNames, gotNames); diff != "" {
		t.Errorf("Default sorted order mismatch (-want +got):\n%s", diff)
	}
}

func TestDeepHierarchicalCohortComparator(t *testing.T) {
	_, log := utiltesting.ContextWithLog(t)
	now := time.Now()

	// Cohort Tree: 4 levels deep
	// Level 0: root
	//   └── Level 1: level1
	//         └── Level 2: level2
	//               └── Level 3: level3
	//                     ├── cq-deep-1 (cohort: level3)
	//                     └── cq-deep-2 (cohort: level3, sibling in level3)
	//   ├── cq-root (cohort: root)
	//   └── Level 1: level1-sibling (cohort: root)
	//         └── cq-sibling-branch (cohort: level1-sibling)
	// standalone-cq (no cohort)
	snap := configtesting.NewSnapshotBuilder().
		Cohort("root", "").
		Cohort("level1", "root").
		Cohort("level2", "level1").
		Cohort("level3", "level2").
		Cohort("level1-sibling", "root").
		ClusterQueue("cq-deep-1", "level3").
		ClusterQueue("cq-deep-2", "level3").
		ClusterQueue("cq-root", "root").
		ClusterQueue("cq-sibling-branch", "level1-sibling").
		ClusterQueue("standalone-cq", "").
		Build()

	createWl := func(name, cq, uid string) *workload.Info {
		wl := utiltestingapi.MakeWorkload(name, "ns").
			UID(types.UID(uid)).
			Obj()
		info := workload.NewInfo(wl)
		info.ClusterQueue = kueue.ClusterQueueReference(cq)
		return info
	}

	preemptor := createWl("preemptor", "cq-deep-1", "p-uid")
	wlSameCQ := createWl("wl-same-cq", "cq-deep-1", "uid-1")
	wlSameCohortSibling := createWl("wl-same-cohort-sib", "cq-deep-2", "uid-2")
	wlRootCQ := createWl("wl-root-cq", "cq-root", "uid-3")
	wlSibBranchCQ := createWl("wl-sib-branch-cq", "cq-sibling-branch", "uid-4")
	wlStandalone := createWl("wl-standalone", "standalone-cq", "uid-5")

	t.Run("IsOtherCohort Ascending", func(t *testing.T) {
		cmpFunc := NewComparator(log, []kueue.Order{{OrderingField: kueue.IsOtherCohort, Direction: kueue.Ascending}}, preemptor, snap, now)

		candidates := []*workload.Info{wlSameCQ, wlRootCQ, wlSameCohortSibling, wlStandalone, wlSibBranchCQ}
		slices.SortFunc(candidates, cmpFunc)

		// In IsOtherCohort Ascending (false < true): same cohort comes before other cohort.
		// Same cohort: wlSameCQ, wlSameCohortSibling (tied on IsOtherCohort=false -> ordered by UID)
		// Other cohort: wlRootCQ, wlStandalone, wlSibBranchCQ (tied on IsOtherCohort=true -> ordered by UID)
		// UID order for same cohort: uid-1 (wlSameCQ), uid-2 (wlSameCohortSibling)
		// UID order for other cohort: uid-3 (wlRootCQ), uid-4 (wlSibBranchCQ), uid-5 (wlStandalone)
		wantNames := []string{"wl-same-cq", "wl-same-cohort-sib", "wl-root-cq", "wl-sib-branch-cq", "wl-standalone"}
		var gotNames []string
		for _, c := range candidates {
			gotNames = append(gotNames, c.Obj.Name)
		}

		if diff := cmp.Diff(wantNames, gotNames); diff != "" {
			t.Errorf("Deep cohort hierarchy sorting mismatch (-want +got):\n%s", diff)
		}
	})

	t.Run("IsOtherCohort Descending", func(t *testing.T) {
		cmpFunc := NewComparator(log, []kueue.Order{{OrderingField: kueue.IsOtherCohort, Direction: kueue.Descending}}, preemptor, snap, now)

		candidates := []*workload.Info{wlRootCQ, wlSameCQ, wlStandalone, wlSameCohortSibling, wlSibBranchCQ}
		slices.SortFunc(candidates, cmpFunc)

		// In IsOtherCohort Descending (true > false): other cohort comes before same cohort.
		// Other cohort: wlRootCQ (uid-3), wlSibBranchCQ (uid-4), wlStandalone (uid-5)
		// Same cohort: wlSameCQ (uid-1), wlSameCohortSibling (uid-2)
		wantNames := []string{"wl-root-cq", "wl-sib-branch-cq", "wl-standalone", "wl-same-cq", "wl-same-cohort-sib"}
		var gotNames []string
		for _, c := range candidates {
			gotNames = append(gotNames, c.Obj.Name)
		}

		if diff := cmp.Diff(wantNames, gotNames); diff != "" {
			t.Errorf("Deep cohort hierarchy Descending mismatch (-want +got):\n%s", diff)
		}
	})
}
