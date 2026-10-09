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
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	"sigs.k8s.io/kueue/pkg/scheduler/preemption/policy"
)

func TestPreemptionLimitTrackerRecord(t *testing.T) {
	t1 := time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)
	t2 := t1.Add(time.Second)
	t3 := t2.Add(time.Second)

	type record struct {
		ts       time.Time
		consumed map[policy.PreemptionLimitReference]policy.PreemptionLimitScopeValue
	}
	cases := map[string]struct {
		records []record
		want    map[policy.PreemptionLimitReference]map[policy.PreemptionLimitScopeValue]events
	}{
		"nil consumed limits is a no-op": {
			records: []record{{ts: t1}},
		},
		"multiple limits and scope values": {
			records: []record{
				{ts: t1, consumed: map[policy.PreemptionLimitReference]policy.PreemptionLimitScopeValue{
					"global":       "Global",
					"per-cq":       "cq-a",
					"per-workload": "ns/wl-1",
				}},
				{ts: t2, consumed: map[policy.PreemptionLimitReference]policy.PreemptionLimitScopeValue{
					"global":       "Global",
					"per-cq":       "cq-b",
					"per-workload": "ns/wl-2",
				}},
				{ts: t3, consumed: map[policy.PreemptionLimitReference]policy.PreemptionLimitScopeValue{
					"per-cq": "cq-a",
				}},
			},
			want: map[policy.PreemptionLimitReference]map[policy.PreemptionLimitScopeValue]events{
				"global":       {"Global": {t1, t2}},
				"per-cq":       {"cq-a": {t1, t3}, "cq-b": {t2}},
				"per-workload": {"ns/wl-1": {t1}, "ns/wl-2": {t2}},
			},
		},
		"out-of-order inserts are kept sorted": {
			records: []record{
				{ts: t3, consumed: map[policy.PreemptionLimitReference]policy.PreemptionLimitScopeValue{"global": "Global"}},
				{ts: t1, consumed: map[policy.PreemptionLimitReference]policy.PreemptionLimitScopeValue{"global": "Global"}},
				{ts: t2, consumed: map[policy.PreemptionLimitReference]policy.PreemptionLimitScopeValue{"global": "Global"}},
				{ts: t1, consumed: map[policy.PreemptionLimitReference]policy.PreemptionLimitScopeValue{"global": "Global"}},
			},
			want: map[policy.PreemptionLimitReference]map[policy.PreemptionLimitScopeValue]events{
				"global": {"Global": {t1, t1, t2, t3}},
			},
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			tracker := NewPreemptionLimitTracker()
			for _, r := range tc.records {
				tracker.Record(r.ts, r.consumed)
			}
			if diff := cmp.Diff(tc.want, tracker.events, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("Unexpected events (-want,+got):\n%s", diff)
			}
		})
	}
}

func TestPreemptionLimitTrackerRecordConcurrent(t *testing.T) {
	const records = 100
	start := time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)
	consumed := map[policy.PreemptionLimitReference]policy.PreemptionLimitScopeValue{"global": "Global"}

	tracker := NewPreemptionLimitTracker()
	var wg sync.WaitGroup
	for i := range records {
		wg.Go(func() {
			tracker.Record(start.Add(time.Duration(i)*time.Second), consumed)
		})
	}
	wg.Wait()

	got := tracker.events["global"]["Global"]
	if len(got) != records {
		t.Errorf("Recorded %d events, want %d", len(got), records)
	}
	if !slices.IsSortedFunc(got, time.Time.Compare) {
		t.Errorf("Events are not sorted: %v", got)
	}
}
