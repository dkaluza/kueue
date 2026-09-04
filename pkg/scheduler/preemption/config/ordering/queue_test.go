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
	"testing"

	gocmp "github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"k8s.io/apimachinery/pkg/types"

	utiltestingapi "sigs.k8s.io/kueue/pkg/util/testing/v1beta2"
	"sigs.k8s.io/kueue/pkg/workload"
)

func TestCandidateQueue(t *testing.T) {
	createWl := func(name string, prio int32) *workload.Info {
		obj := utiltestingapi.MakeWorkload(name, "ns").
			UID(types.UID(name)).
			Priority(prio).
			Obj()
		info := workload.NewInfo(obj)
		info.ClusterQueue = "cq-test"
		return info
	}

	w1 := createWl("w1", 10)
	w2 := createWl("w2", 20)
	w3 := createWl("w3", 30)

	prioCmp := func(a, b *workload.Info) int {
		return cmp.Compare(*a.Obj.Spec.Priority, *b.Obj.Spec.Priority)
	}

	t.Run("sorts candidates once at initialization", func(t *testing.T) {
		// Input out of order: w3 (30), w1 (10), w2 (20)
		q := NewCandidateQueue("rule-1", 0, "cq-test", []*workload.Info{w3, w1, w2}, prioCmp)

		if q.RuleName() != "rule-1" {
			t.Errorf("RuleName() = %v, want rule-1", q.RuleName())
		}
		if q.SelectorIndex() != 0 {
			t.Errorf("SelectorIndex() = %v, want 0", q.SelectorIndex())
		}
		if q.clusterQueue != "cq-test" {
			t.Errorf("clusterQueue = %v, want cq-test", q.clusterQueue)
		}
		if q.len() != 3 {
			t.Errorf("len() = %d, want 3", q.len())
		}
		if q.isEmpty() {
			t.Errorf("isEmpty() = true, want false")
		}

		// Peek should see lowest priority first (w1)
		if got := q.Peek(); got != w1 {
			t.Errorf("Peek() = %v, want w1", got)
		}
		// Second Peek should still see w1 (non-destructive)
		if got := q.Peek(); got != w1 {
			t.Errorf("Peek() second time = %v, want w1", got)
		}

		// Pop w1
		if got := q.Pop(); got != w1 {
			t.Errorf("Pop() = %v, want w1", got)
		}
		if q.len() != 2 {
			t.Errorf("len() after 1 pop = %d, want 2", q.len())
		}

		// Pop w2
		if got := q.Pop(); got != w2 {
			t.Errorf("Pop() = %v, want w2", got)
		}

		// Pop w3
		if got := q.Pop(); got != w3 {
			t.Errorf("Pop() = %v, want w3", got)
		}

		// Now empty
		if !q.isEmpty() {
			t.Errorf("isEmpty() after draining = false, want true")
		}
		if q.len() != 0 {
			t.Errorf("len() after draining = %d, want 0", q.len())
		}
		if got := q.Peek(); got != nil {
			t.Errorf("Peek() on empty queue = %v, want nil", got)
		}
		if got := q.Pop(); got != nil {
			t.Errorf("Pop() on empty queue = %v, want nil", got)
		}
	})

	t.Run("empty queue handling", func(t *testing.T) {
		q := NewCandidateQueue("rule-1", 1, "cq-empty", nil, prioCmp)
		if !q.isEmpty() {
			t.Errorf("isEmpty() = false, want true")
		}
		if q.len() != 0 {
			t.Errorf("len() = %d, want 0", q.len())
		}
		if got := q.Peek(); got != nil {
			t.Errorf("Peek() = %v, want nil", got)
		}
		if got := q.Pop(); got != nil {
			t.Errorf("Pop() = %v, want nil", got)
		}
		if len(q.candidates[q.cursor:]) != 0 {
			t.Errorf("candidates len = %v, want 0", len(q.candidates[q.cursor:]))
		}
	})

	t.Run("nil comparator falls back to UID sorting", func(t *testing.T) {
		q := NewCandidateQueue("rule-1", 0, "cq-test", []*workload.Info{w3, w1, w2}, nil)
		if got := q.Pop(); got != w1 {
			t.Errorf("Pop() 1 with nil cmp = %v, want w1", got)
		}
		if got := q.Pop(); got != w2 {
			t.Errorf("Pop() 2 with nil cmp = %v, want w2", got)
		}
		if got := q.Pop(); got != w3 {
			t.Errorf("Pop() 3 with nil cmp = %v, want w3", got)
		}
	})

	t.Run("candidates slice returns remaining items", func(t *testing.T) {
		q := NewCandidateQueue("rule-1", 0, "cq-test", []*workload.Info{w3, w1, w2}, prioCmp)
		if diff := gocmp.Diff([]*workload.Info{w1, w2, w3}, q.candidates[q.cursor:], cmpopts.EquateEmpty()); diff != "" {
			t.Errorf("candidates mismatch (-want +got):\n%s", diff)
		}
		q.Pop()
		if diff := gocmp.Diff([]*workload.Info{w2, w3}, q.candidates[q.cursor:], cmpopts.EquateEmpty()); diff != "" {
			t.Errorf("candidates after pop mismatch (-want +got):\n%s", diff)
		}
		q.Pop()
		q.Pop()
		if len(q.candidates[q.cursor:]) != 0 {
			t.Errorf("candidates after draining = %v, want empty", q.candidates[q.cursor:])
		}
	})
}
