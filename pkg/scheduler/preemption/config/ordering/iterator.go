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
	"iter"

	"k8s.io/apimachinery/pkg/types"

	"sigs.k8s.io/kueue/pkg/workload"
)

// RuleSelectorOrigin identifies the preemption rule and candidate selector index
// that matched a candidate workload.
type RuleSelectorOrigin struct {
	RuleName      string
	SelectorIndex int
}

// MultiQueueCandidateIterator coordinates iteration across multiple per-selector,
// per-ClusterQueue priority queues, selecting the globally minimal candidate across
// all queue heads, popping matching duplicates simultaneously, and recording provenance.
type MultiQueueCandidateIterator struct {
	queues     []*CandidateQueue
	cmp        func(a, b *workload.Info) int
	provenance map[types.UID][]RuleSelectorOrigin
}

// NewMultiQueueCandidateIterator creates a new MultiQueueCandidateIterator.
func NewMultiQueueCandidateIterator(
	queues []*CandidateQueue,
	cmp func(a, b *workload.Info) int,
) *MultiQueueCandidateIterator {
	if cmp == nil {
		cmp = compareUID
	}
	return &MultiQueueCandidateIterator{
		queues:     queues,
		cmp:        cmp,
		provenance: make(map[types.UID][]RuleSelectorOrigin),
	}
}

// next selects and returns the globally minimal candidate across all active queue heads,
// popping it simultaneously from all matching queue heads and recording provenance.
// Returns (nil, nil, false) if no candidates remain in any active queue.
func (it *MultiQueueCandidateIterator) next() (*workload.Info, []RuleSelectorOrigin, bool) {
	var minWl *workload.Info

	for _, q := range it.queues {
		head := q.Peek()
		if head == nil {
			continue
		}
		if minWl == nil || it.cmp(head, minWl) < 0 {
			minWl = head
		}
	}

	if minWl == nil {
		return nil, nil, false
	}

	// Simultaneously pop minWl from all queue heads where it appears and collect provenance.
	var origins []RuleSelectorOrigin
	for _, q := range it.queues {
		head := q.Peek()
		if head == nil {
			continue
		}
		if isSameWorkload(head, minWl) {
			q.Pop()
			origins = append(origins, RuleSelectorOrigin{
				RuleName:      q.RuleName(),
				SelectorIndex: q.SelectorIndex(),
			})
		}
	}

	it.provenance[minWl.Obj.UID] = origins

	return minWl, origins, true
}

func isSameWorkload(a, b *workload.Info) bool {
	return a == b || a.Obj.UID == b.Obj.UID
}

// Seq returns an iterator sequence yielding preemption candidates in configured order.
func (it *MultiQueueCandidateIterator) Seq() iter.Seq[*workload.Info] {
	return func(yield func(*workload.Info) bool) {
		for {
			wl, _, ok := it.next()
			if !ok {
				return
			}
			if !yield(wl) {
				return
			}
		}
	}
}

// Provenance returns all captured provenance mappings.
func (it *MultiQueueCandidateIterator) Provenance() map[types.UID][]RuleSelectorOrigin {
	return it.provenance
}

// Queues returns the slice of managed CandidateQueues.
func (it *MultiQueueCandidateIterator) Queues() []*CandidateQueue {
	return it.queues
}
