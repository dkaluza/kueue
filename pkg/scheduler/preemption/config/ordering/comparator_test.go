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
	controllerconstants "sigs.k8s.io/kueue/pkg/controller/constants"
	"sigs.k8s.io/kueue/pkg/features"
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

	const (
		wantLess    = -1 // a comes before b
		wantEqual   = 0  // a and b have equal precedence
		wantGreater = 1  // a comes after b
	)

	preemptor := baseWorkload("preemptor", "p-uid", "cq-a1")

	tests := map[string]struct {
		a    *workload.Info
		b    *workload.Info
		want int
	}{
		"same workload identity comparison returns 0": {
			a:    preemptor,
			b:    preemptor,
			want: wantEqual,
		},
		"Priority Ascending: lower priority comes first (a < b)": {
			a:    withPriority(baseWorkload("a", "same-uid", "cq-a1"), 10),
			b:    withPriority(baseWorkload("b", "same-uid", "cq-a1"), 20),
			want: wantLess,
		},
		"Priority Ascending: lower priority comes first (a > b)": {
			a:    withPriority(baseWorkload("a", "same-uid", "cq-a1"), 100),
			b:    withPriority(baseWorkload("b", "same-uid", "cq-a1"), 50),
			want: wantGreater,
		},
		"AdmissionTimestamp Descending: more recently admitted comes first (a < b)": {
			a:    withReservationTime(withPriority(baseWorkload("a", "same-uid", "cq-a1"), 10), t3),
			b:    withReservationTime(withPriority(baseWorkload("b", "same-uid", "cq-a1"), 10), t1),
			want: wantLess,
		},
		"AdmissionTimestamp Descending: more recently admitted comes first (a > b)": {
			a:    withReservationTime(withPriority(baseWorkload("a", "same-uid", "cq-a1"), 10), t1),
			b:    withReservationTime(withPriority(baseWorkload("b", "same-uid", "cq-a1"), 10), t3),
			want: wantGreater,
		},
		"UID Ascending tie-breaker (a < b)": {
			a:    withReservationTime(withPriority(baseWorkload("a", "uid-1", "cq-a1"), 10), t2),
			b:    withReservationTime(withPriority(baseWorkload("b", "uid-2", "cq-a1"), 10), t2),
			want: wantLess,
		},
		"UID Ascending tie-breaker (a > b)": {
			a:    withReservationTime(withPriority(baseWorkload("a", "uid-2", "cq-a1"), 10), t2),
			b:    withReservationTime(withPriority(baseWorkload("b", "uid-1", "cq-a1"), 10), t2),
			want: wantGreater,
		},
		"PriorityBoost raises effective priority": {
			a:    withPriorityBoost(withPriority(baseWorkload("a", "same-uid", "cq-a1"), 10), "100"),
			b:    withPriority(baseWorkload("b", "same-uid", "cq-a1"), 50),
			want: wantGreater, // effective priority: 110 > 50 -> comes after (wantGreater)
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			got := NewComparator(log, now)(tc.a, tc.b)
			if got != tc.want {
				t.Errorf("NewComparator() = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestCandidateSorting(t *testing.T) {
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

	// Candidate ordering: Priority (Ascending) -> AdmissionTimestamp (Descending) -> UID (Ascending)
	cmpFunc := NewComparator(log, now)

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
		t.Errorf("Sorted order mismatch (-want +got):\n%s", diff)
	}
}
