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
	"slices"
	"testing"

	gocmp "github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"k8s.io/apimachinery/pkg/types"

	kueue "sigs.k8s.io/kueue/apis/kueue/v1beta2"
	utiltestingapi "sigs.k8s.io/kueue/pkg/util/testing/v1beta2"
	"sigs.k8s.io/kueue/pkg/workload"
)

func TestMultiQueueCandidateIterator(t *testing.T) {
	createWl := func(name string, prio int32, cq string) *workload.Info {
		obj := utiltestingapi.MakeWorkload(name, "ns").
			UID(types.UID(name)).
			Priority(prio).
			Obj()
		info := workload.NewInfo(obj)
		info.ClusterQueue = kueue.ClusterQueueReference(cq)
		return info
	}

	prioCmp := func(a, b *workload.Info) int {
		if a == b {
			return 0
		}
		if a == nil {
			return 1
		}
		if b == nil {
			return -1
		}
		if res := cmp.Compare(*a.Obj.Spec.Priority, *b.Obj.Spec.Priority); res != 0 {
			return res
		}
		return cmp.Compare(a.Obj.UID, b.Obj.UID)
	}

	w1 := createWl("w1", 10, "cq-a")
	w2 := createWl("w2", 20, "cq-b")
	w3 := createWl("w3", 30, "cq-a")
	w4 := createWl("w4", 40, "cq-b")

	cases := map[string]struct {
		queues         []*CandidateQueue
		cmp            func(a, b *workload.Info) int
		wantOrder      []string
		wantProvenance map[types.UID][]RuleSelectorOrigin
	}{
		"single queue iteration": {
			queues: []*CandidateQueue{
				NewCandidateQueue("rule-1", 0, "cq-a", []*workload.Info{w3, w1}, prioCmp),
			},
			cmp:       prioCmp,
			wantOrder: []string{"w1", "w3"},
			wantProvenance: map[types.UID][]RuleSelectorOrigin{
				"w1": {{RuleName: "rule-1", SelectorIndex: 0}},
				"w3": {{RuleName: "rule-1", SelectorIndex: 0}},
			},
		},
		"multi-queue interleaved ordering": {
			queues: []*CandidateQueue{
				NewCandidateQueue("rule-1", 0, "cq-a", []*workload.Info{w1, w3}, prioCmp),
				NewCandidateQueue("rule-1", 0, "cq-b", []*workload.Info{w2, w4}, prioCmp),
			},
			cmp:       prioCmp,
			wantOrder: []string{"w1", "w2", "w3", "w4"},
			wantProvenance: map[types.UID][]RuleSelectorOrigin{
				"w1": {{RuleName: "rule-1", SelectorIndex: 0}},
				"w2": {{RuleName: "rule-1", SelectorIndex: 0}},
				"w3": {{RuleName: "rule-1", SelectorIndex: 0}},
				"w4": {{RuleName: "rule-1", SelectorIndex: 0}},
			},
		},
		"simultaneous duplicate head popping and provenance tracking": {
			queues: []*CandidateQueue{
				// w1 matches Rule 1 Selector 0, Rule 1 Selector 1, and Rule 2 Selector 0
				// w3 matches Rule 1 Selector 0
				// w2 matches Rule 2 Selector 0
				NewCandidateQueue("rule-1", 0, "cq-a", []*workload.Info{w1, w3}, prioCmp),
				NewCandidateQueue("rule-1", 1, "cq-a", []*workload.Info{w1}, prioCmp),
				NewCandidateQueue("rule-2", 0, "cq-a", []*workload.Info{w1, w2}, prioCmp),
			},
			cmp:       prioCmp,
			wantOrder: []string{"w1", "w2", "w3"},
			wantProvenance: map[types.UID][]RuleSelectorOrigin{
				"w1": {
					{RuleName: "rule-1", SelectorIndex: 0},
					{RuleName: "rule-1", SelectorIndex: 1},
					{RuleName: "rule-2", SelectorIndex: 0},
				},
				"w2": {{RuleName: "rule-2", SelectorIndex: 0}},
				"w3": {{RuleName: "rule-1", SelectorIndex: 0}},
			},
		},
		"multi-queue UID tie-breaking across 4 queues": {
			queues: []*CandidateQueue{
				NewCandidateQueue("rule-1", 0, "cq-a", []*workload.Info{createWl("wl-a", 10, "cq-a")}, prioCmp),
				NewCandidateQueue("rule-1", 0, "cq-b", []*workload.Info{createWl("wl-b", 10, "cq-b")}, prioCmp),
				NewCandidateQueue("rule-1", 0, "cq-c", []*workload.Info{createWl("wl-c", 10, "cq-c")}, prioCmp),
				NewCandidateQueue("rule-1", 0, "cq-d", []*workload.Info{createWl("wl-d", 10, "cq-d")}, prioCmp),
			},
			cmp:       prioCmp,
			wantOrder: []string{"wl-a", "wl-b", "wl-c", "wl-d"},
			wantProvenance: map[types.UID][]RuleSelectorOrigin{
				"wl-a": {{RuleName: "rule-1", SelectorIndex: 0}},
				"wl-b": {{RuleName: "rule-1", SelectorIndex: 0}},
				"wl-c": {{RuleName: "rule-1", SelectorIndex: 0}},
				"wl-d": {{RuleName: "rule-1", SelectorIndex: 0}},
			},
		},
		"empty queue handling with nil queues": {
			queues:         nil,
			cmp:            prioCmp,
			wantOrder:      nil,
			wantProvenance: map[types.UID][]RuleSelectorOrigin{},
		},
		"empty queue handling with empty queues list": {
			queues: []*CandidateQueue{
				NewCandidateQueue("rule-1", 0, "cq-a", nil, prioCmp),
				NewCandidateQueue("rule-2", 0, "cq-b", []*workload.Info{}, prioCmp),
			},
			cmp:            prioCmp,
			wantOrder:      nil,
			wantProvenance: map[types.UID][]RuleSelectorOrigin{},
		},
		"nil comparator fallback in iterator": {
			queues: []*CandidateQueue{
				NewCandidateQueue("rule-1", 0, "cq-a", []*workload.Info{w1, w3}, nil),
				NewCandidateQueue("rule-1", 0, "cq-b", []*workload.Info{w2, w4}, nil),
			},
			cmp:       nil,
			wantOrder: []string{"w1", "w2", "w3", "w4"},
			wantProvenance: map[types.UID][]RuleSelectorOrigin{
				"w1": {{RuleName: "rule-1", SelectorIndex: 0}},
				"w2": {{RuleName: "rule-1", SelectorIndex: 0}},
				"w3": {{RuleName: "rule-1", SelectorIndex: 0}},
				"w4": {{RuleName: "rule-1", SelectorIndex: 0}},
			},
		},
		"deduplication across queues by UID with distinct objects": {
			queues: []*CandidateQueue{
				NewCandidateQueue("rule-1", 0, "cq-a", []*workload.Info{createWl("wl-a", 10, "cq-a")}, nil),
				NewCandidateQueue("rule-1", 1, "cq-a", []*workload.Info{createWl("wl-a", 10, "cq-a")}, nil),
			},
			cmp:       nil,
			wantOrder: []string{"wl-a"},
			wantProvenance: map[types.UID][]RuleSelectorOrigin{
				"wl-a": {
					{RuleName: "rule-1", SelectorIndex: 0},
					{RuleName: "rule-1", SelectorIndex: 1},
				},
			},
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			it := NewMultiQueueCandidateIterator(tc.queues, tc.cmp)

			var gotOrder []string
			for wl := range it.Seq() {
				gotOrder = append(gotOrder, wl.Obj.Name)
			}

			if diff := gocmp.Diff(tc.wantOrder, gotOrder, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("Iteration order mismatch (-want +got):\n%s", diff)
			}

			gotProvenance := it.Provenance()
			if diff := gocmp.Diff(tc.wantProvenance, gotProvenance, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("Provenance mismatch (-want +got):\n%s", diff)
			}

			// Also verify Provenance lookup for each workload
			for uid, wantOrigins := range tc.wantProvenance {
				gotOrigins := it.Provenance()[uid]
				if diff := gocmp.Diff(wantOrigins, gotOrigins); diff != "" {
					t.Errorf("Provenance[%s] mismatch (-want +got):\n%s", uid, diff)
				}
			}
		})
	}

	t.Run("next step-by-step queue draining and head state", func(t *testing.T) {
		qR1S0 := NewCandidateQueue("rule-1", 0, "cq-a", []*workload.Info{w1, w3}, prioCmp)
		qR1S1 := NewCandidateQueue("rule-1", 1, "cq-a", []*workload.Info{w1}, prioCmp)
		qR2S0 := NewCandidateQueue("rule-2", 0, "cq-a", []*workload.Info{w1, w2}, prioCmp)

		it := NewMultiQueueCandidateIterator([]*CandidateQueue{qR1S0, qR1S1, qR2S0}, prioCmp)

		// 1. First next() should return w1 and pop it from all 3 queues
		wl1, origins1, ok1 := it.next()
		if !ok1 || wl1 != w1 {
			t.Fatalf("First next() = %v, %v, want w1, true", wl1, ok1)
		}
		wantOrigins1 := []RuleSelectorOrigin{
			{RuleName: "rule-1", SelectorIndex: 0},
			{RuleName: "rule-1", SelectorIndex: 1},
			{RuleName: "rule-2", SelectorIndex: 0},
		}
		if diff := gocmp.Diff(wantOrigins1, origins1); diff != "" {
			t.Errorf("w1 origins mismatch (-want +got):\n%s", diff)
		}
		if qR1S0.Peek() != w3 {
			t.Errorf("qR1S0 head after pop = %v, want w3", qR1S0.Peek())
		}
		if qR1S1.Peek() != nil {
			t.Errorf("qR1S1 head after pop = %v, want nil", qR1S1.Peek())
		}
		if qR2S0.Peek() != w2 {
			t.Errorf("qR2S0 head after pop = %v, want w2", qR2S0.Peek())
		}

		// 2. Second next() should pick w2 (prio 20) over w3 (prio 30)
		wl2, origins2, ok2 := it.next()
		if !ok2 || wl2 != w2 {
			t.Fatalf("Second next() = %v, %v, want w2, true", wl2, ok2)
		}
		wantOrigins2 := []RuleSelectorOrigin{
			{RuleName: "rule-2", SelectorIndex: 0},
		}
		if diff := gocmp.Diff(wantOrigins2, origins2); diff != "" {
			t.Errorf("w2 origins mismatch (-want +got):\n%s", diff)
		}

		// 3. Third next() should pick w3 (prio 30)
		wl3, origins3, ok3 := it.next()
		if !ok3 || wl3 != w3 {
			t.Fatalf("Third next() = %v, %v, want w3, true", wl3, ok3)
		}
		wantOrigins3 := []RuleSelectorOrigin{
			{RuleName: "rule-1", SelectorIndex: 0},
		}
		if diff := gocmp.Diff(wantOrigins3, origins3); diff != "" {
			t.Errorf("w3 origins mismatch (-want +got):\n%s", diff)
		}

		// 4. Fourth next() should return false (all drained)
		wl4, _, ok4 := it.next()
		if ok4 || wl4 != nil {
			t.Errorf("Fourth next() = %v, %v, want nil, false", wl4, ok4)
		}
	})

	t.Run("early halting in Seq", func(t *testing.T) {
		q := NewCandidateQueue("rule-1", 0, "cq-a", []*workload.Info{w1, w2, w3, w4}, prioCmp)
		it := NewMultiQueueCandidateIterator([]*CandidateQueue{q}, prioCmp)

		var count int
		for range it.Seq() {
			count++
			if count == 2 {
				break
			}
		}

		if count != 2 {
			t.Errorf("Halted after %d items, want 2", count)
		}
		if q.len() != 2 {
			t.Errorf("Remaining items in queue = %d, want 2", q.len())
		}
	})

	t.Run("Queues accessor", func(t *testing.T) {
		q1 := NewCandidateQueue("rule-1", 0, "cq-a", []*workload.Info{w1}, prioCmp)
		q2 := NewCandidateQueue("rule-2", 0, "cq-b", []*workload.Info{w2}, prioCmp)
		queues := []*CandidateQueue{q1, q2}
		it := NewMultiQueueCandidateIterator(queues, prioCmp)

		if got := it.Queues(); !slices.Equal(queues, got) {
			t.Errorf("Queues() = %v, want %v", got, queues)
		}
	})
}
