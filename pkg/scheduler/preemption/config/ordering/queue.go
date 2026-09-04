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

	kueue "sigs.k8s.io/kueue/apis/kueue/v1beta2"
	"sigs.k8s.io/kueue/pkg/workload"
)

// CandidateQueue represents a queue of preemption candidate workloads
// partitioned by (CandidateSelector, ClusterQueue), sorted once at initialization.
type CandidateQueue struct {
	ruleName      string
	selectorIndex int
	clusterQueue  kueue.ClusterQueueReference
	candidates    []*workload.Info
	cursor        int
}

// NewCandidateQueue creates and initializes a CandidateQueue, sorting candidates in place
// using the provided comparator.
func NewCandidateQueue(
	ruleName string,
	selectorIndex int,
	clusterQueue kueue.ClusterQueueReference,
	candidates []*workload.Info,
	cmp func(a, b *workload.Info) int,
) *CandidateQueue {
	if cmp == nil {
		cmp = compareUID
	}
	slices.SortFunc(candidates, cmp)
	return &CandidateQueue{
		ruleName:      ruleName,
		selectorIndex: selectorIndex,
		clusterQueue:  clusterQueue,
		candidates:    candidates,
		cursor:        0,
	}
}

// RuleName returns the name of the rule this queue belongs to.
func (q *CandidateQueue) RuleName() string {
	return q.ruleName
}

// SelectorIndex returns the index of the selector within the rule.
func (q *CandidateQueue) SelectorIndex() int {
	return q.selectorIndex
}

// Peek returns the workload at the front of the queue without removing it.
// Returns nil if the queue is empty.
func (q *CandidateQueue) Peek() *workload.Info {
	if q.cursor >= len(q.candidates) {
		return nil
	}
	return q.candidates[q.cursor]
}

// Pop removes and returns the workload at the front of the queue.
// Returns nil if the queue is empty.
func (q *CandidateQueue) Pop() *workload.Info {
	if q.cursor >= len(q.candidates) {
		return nil
	}
	wl := q.candidates[q.cursor]
	q.cursor++
	return wl
}

// isEmpty returns true if the queue has no more candidates.
func (q *CandidateQueue) isEmpty() bool {
	return q.cursor >= len(q.candidates)
}

// len returns the number of remaining candidates in the queue.
func (q *CandidateQueue) len() int {
	if q.cursor >= len(q.candidates) {
		return 0
	}
	return len(q.candidates) - q.cursor
}
