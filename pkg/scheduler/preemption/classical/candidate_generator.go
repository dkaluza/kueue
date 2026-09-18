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

package classical

import (
	"slices"
	"sort"
	"time"

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/utils/clock"

	kueue "sigs.k8s.io/kueue/apis/kueue/v1beta2"
	schdcache "sigs.k8s.io/kueue/pkg/cache/scheduler"
	"sigs.k8s.io/kueue/pkg/resources"
	"sigs.k8s.io/kueue/pkg/workload"
	workloadevict "sigs.k8s.io/kueue/pkg/workload/evict"
)

type candidateIterator struct {
	candidates                        []*candidateElem
	runIndex                          int
	frsNeedPreemption                 sets.Set[resources.FlavorResource]
	snapshot                          *schdcache.Snapshot
	NoCandidateFromOtherQueues        bool
	NoCandidateForHierarchicalReclaim bool
	hierarchicalReclaimCtx            *HierarchicalPreemptionCtx
}

type candidateElem struct {
	wl *workload.Info
	// lca of this queue and cq (queue to which the new workload is submitted)
	lca *schdcache.CohortSnapshot
	// candidates above priority threshold cannot be preempted if at the same time
	// cq would borrow from other queues/cohorts
	preemptionVariant preemptionVariant
	// configurable indicates that the candidate was selected by the
	// ConfigurablePreemption rules. Such candidates are not subject to the
	// quota-based restrictions, as they were explicitly selected by the
	// PreemptionConfig. A candidate which the classical algorithm collected as well
	// keeps its preemptionVariant, and is therefore reported with the reason of the
	// classical algorithm.
	// TODO(#13396): remove once ConfigurablePreemption covers the classical
	// preemption and the two become mutually exclusive, as the classical algorithm
	// will then never see a configurable candidate.
	configurable bool
}

func WorkloadUsesResources(wl *workload.Info, frsNeedPreemption sets.Set[resources.FlavorResource]) bool {
	for _, ps := range wl.TotalRequests {
		for res, flv := range ps.Flavors {
			if frsNeedPreemption.Has(resources.FlavorResource{Flavor: flv, Resource: res}) {
				return true
			}
		}
	}
	return false
}

// assume a prefix of the elements has condition WorkloadEvicted = true
func splitEvicted(workloads []*candidateElem) ([]*candidateElem, []*candidateElem) {
	firstFalse := sort.Search(len(workloads), func(i int) bool {
		return !workloadevict.IsEvicted(workloads[i].wl.Obj)
	})
	return workloads[:firstFalse], workloads[firstFalse:]
}

// NewCandidateIterator creates a new iterator that yields candidate workloads for preemption
// The iterator can be used to perform two independent runs over the list of candidates:
// with and without borrowing. The runs are independent which means that the same candidates
// might be returned for both, but note that the candidates with borrowing are a subset of
// candidates without borrowing.
// The configurableCandidates, selected by the ConfigurablePreemption rules, are merged with
// the candidates found by the classical algorithm. They are not subject to the quota-based
// restrictions, see candidateIsValid.
// TODO(#13396): drop the configurableCandidates parameter, and the merging it entails,
// once ConfigurablePreemption covers the classical preemption and the two become
// mutually exclusive.
func NewCandidateIterator(
	hierarchicalReclaimCtx *HierarchicalPreemptionCtx,
	enabledAfs bool,
	frsNeedPreemption sets.Set[resources.FlavorResource],
	snapshot *schdcache.Snapshot,
	clock clock.Clock,
	ordering func(logr.Logger, bool, *workload.Info, *workload.Info, kueue.ClusterQueueReference, time.Time) int,
	configurableCandidates []*workload.Info,
) *candidateIterator {
	sameQueueCandidates := collectSameQueueCandidates(hierarchicalReclaimCtx)
	hierarchyCandidates, priorityCandidates := collectCandidatesForHierarchicalReclaim(hierarchicalReclaimCtx)
	configurableOnlyCandidates := markConfigurableCandidates(configurableCandidates, sameQueueCandidates, hierarchyCandidates, priorityCandidates)
	sortFn := func(a, b *candidateElem) int {
		return ordering(hierarchicalReclaimCtx.Log, enabledAfs, a.wl, b.wl, hierarchicalReclaimCtx.Cq.Name, clock.Now())
	}
	slices.SortFunc(sameQueueCandidates, sortFn)
	slices.SortFunc(priorityCandidates, sortFn)
	slices.SortFunc(hierarchyCandidates, sortFn)
	slices.SortFunc(configurableOnlyCandidates, sortFn)

	evictedHierarchicalReclaimCandidates, nonEvictedHierarchicalReclaimCandidates := splitEvicted(hierarchyCandidates)
	evictedConfigurableCandidates, nonEvictedConfigurableCandidates := splitEvicted(configurableOnlyCandidates)
	evictedSTCandidates, nonEvictedSTCandidates := splitEvicted(priorityCandidates)
	evictedSameQueueCandidates, nonEvictedSameQueueCandidates := splitEvicted(sameQueueCandidates)
	allCandidates := make([]*candidateElem, 0, len(hierarchyCandidates)+len(configurableOnlyCandidates)+len(priorityCandidates)+len(sameQueueCandidates))
	allCandidates = append(allCandidates, evictedHierarchicalReclaimCandidates...)
	allCandidates = append(allCandidates, evictedConfigurableCandidates...)
	allCandidates = append(allCandidates, evictedSTCandidates...)
	allCandidates = append(allCandidates, evictedSameQueueCandidates...)
	allCandidates = append(allCandidates, nonEvictedHierarchicalReclaimCandidates...)
	allCandidates = append(allCandidates, nonEvictedConfigurableCandidates...)
	allCandidates = append(allCandidates, nonEvictedSTCandidates...)
	allCandidates = append(allCandidates, nonEvictedSameQueueCandidates...)
	return &candidateIterator{
		runIndex:                          0,
		frsNeedPreemption:                 frsNeedPreemption,
		snapshot:                          snapshot,
		candidates:                        allCandidates,
		NoCandidateFromOtherQueues:        len(hierarchyCandidates) == 0 && len(priorityCandidates) == 0,
		NoCandidateForHierarchicalReclaim: len(hierarchyCandidates) == 0,
		hierarchicalReclaimCtx:            hierarchicalReclaimCtx,
	}
}

// markConfigurableCandidates flags the candidates which were already collected by the
// classical algorithm, so that they are not rejected by the quota-based restrictions,
// and returns the elements for the candidates which are only selected by the
// ConfigurablePreemption rules. A candidate is never duplicated, as that would lead
// to removing the same workload from the snapshot twice.
// TODO(#13396): remove once ConfigurablePreemption covers the classical preemption and
// the two become mutually exclusive.
func markConfigurableCandidates(configurableCandidates []*workload.Info, collectedCandidates ...[]*candidateElem) []*candidateElem {
	if len(configurableCandidates) == 0 {
		return nil
	}
	configurableKeys := sets.New[workload.Reference]()
	for _, wl := range configurableCandidates {
		configurableKeys.Insert(workload.Key(wl.Obj))
	}
	collectedKeys := sets.New[workload.Reference]()
	for _, candidates := range collectedCandidates {
		for _, candidate := range candidates {
			key := workload.Key(candidate.wl.Obj)
			collectedKeys.Insert(key)
			if configurableKeys.Has(key) {
				candidate.configurable = true
			}
		}
	}
	var configurableOnlyCandidates []*candidateElem
	for _, wl := range configurableCandidates {
		if collectedKeys.Has(workload.Key(wl.Obj)) {
			continue
		}
		configurableOnlyCandidates = append(configurableOnlyCandidates, &candidateElem{
			wl:                wl,
			preemptionVariant: ConfigurablePreemption,
			configurable:      true,
		})
	}
	return configurableOnlyCandidates
}

// Next allows to iterate over the ordered sequence of candidates, with the reason
// for eviction returned together with a candidate.
func (c *candidateIterator) Next(borrow bool) (*workload.Info, string) {
	if c.runIndex >= len(c.candidates) {
		return nil, ""
	}
	candidate := c.candidates[c.runIndex]
	c.runIndex++
	if !c.candidateIsValid(candidate, borrow) {
		return c.Next(borrow)
	}
	return candidate.wl, candidate.preemptionVariant.PreemptionReason()
}

// candidateIsValid checks if candidate is valid,
// as eg. some candidates can only be considered without borrowing
// Also, preemption of candidates might invalidate other candidates
func (c *candidateIterator) candidateIsValid(candidate *candidateElem, borrow bool) bool {
	// Candidates selected by the ConfigurablePreemption rules are preemptible
	// regardless of the quota used by their ClusterQueue.
	// TODO(#13396): remove this bypass once ConfigurablePreemption covers the classical
	// preemption and the two become mutually exclusive.
	if candidate.configurable {
		return true
	}
	if c.hierarchicalReclaimCtx.Cq.Name == candidate.wl.ClusterQueue {
		return true
	}
	if borrow && candidate.preemptionVariant == ReclaimWithoutBorrowing {
		return false
	}
	cq := c.snapshot.ClusterQueue(candidate.wl.ClusterQueue)
	if schdcache.IsWithinNominalInResources(cq, c.frsNeedPreemption) {
		return false
	}
	// we don't go all the way to the root but only to the lca node
	for node := range cq.PathParentToRoot() {
		if node == candidate.lca {
			break
		}
		if schdcache.IsWithinNominalInResources(node, c.frsNeedPreemption) {
			return false
		}
	}
	return true
}

// Reset moves the candidate iterator back to the starting position.
// It is required to reset the iterator before each run.
func (c *candidateIterator) Reset() {
	c.runIndex = 0
}
