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

package policy

import (
	"fmt"
	"strconv"
	"strings"

	"sigs.k8s.io/controller-runtime/pkg/client"

	kueue "sigs.k8s.io/kueue/apis/kueue/v1beta2"
	"sigs.k8s.io/kueue/pkg/cache/scheduler"
	"sigs.k8s.io/kueue/pkg/util/logging"
	sliceutils "sigs.k8s.io/kueue/pkg/util/slices"
	"sigs.k8s.io/kueue/pkg/workload"
)

// Target represents a workload selected for preemption.
type Target struct {
	WorkloadInfo *workload.Info
	Reason       string
	WorkloadCq   *scheduler.ClusterQueueSnapshot

	// ConfigurablePreemptionReasonData stores data which resulted in eviction.
	// Specified only when eviction is due to configurable preemption.
	ConfigurablePreemptionReasonData *ConfigurablePreemptionReasonData
}

type ConfigurablePreemptionReasonData struct {
	ConfigName PreemptionConfigReference
	// MatchedRule is a rule to which preemption candidate is attributed to.
	// First satisfied rule within preemption limits is picked.
	MatchedRule MatchedRule
	// ConsumedLimits are the PreemptionLimits consumed by preempting this target.
	ConsumedLimits ConsumedLimits
}

type MatchedRule struct {
	RuleReference PreemptionConfigRuleReference
	// SelectorIndexes contains indexes of selectors under the preemption rule
	// which were matched.
	SelectorIndexes []int
}

// PreemptionConfigReference is a dedicated type to reference PreemptionConfig
type PreemptionConfigReference string

// PreemptionConfigRuleReference is a dedicated type to reference PreemptionConfigPreemptionRule
type PreemptionConfigRuleReference string

// PreemptionLimitReference is a dedicated type to reference PreemptionLimit
type PreemptionLimitReference string

// PreemptionLimitScopeValue identifies a scoped entity within a PreemptionLimit
// (e.g. "Global", a ClusterQueue name, or "<namespace>/<workload-name>").
type PreemptionLimitScopeValue string

// ConsumedLimits maps each PreemptionLimit to the scope value consumed by a preemption.
type ConsumedLimits map[PreemptionLimitReference]PreemptionLimitScopeValue

func (d *ConfigurablePreemptionReasonData) EvictionMessage(preemptor *kueue.Workload) string {
	itoa := func(n *int) string { return strconv.Itoa(*n) }

	return fmt.Sprintf("Preempted by %s because of preemption config %s rule %s/%s",
		workload.Key(preemptor),
		d.ConfigName,
		d.MatchedRule.RuleReference,
		strings.Join(sliceutils.Map(d.MatchedRule.SelectorIndexes, itoa), ","))
}

type yieldCandidate = func(*Target) bool

// YieldFromSnapshot wraps a candidate (Target) yielder with
// logic removing the candidate from the provided snapshot.
func YieldFromSnapshot(snapshot *scheduler.Snapshot, yield yieldCandidate) yieldCandidate {
	return func(t *Target) bool {
		snapshot.RemoveWorkload(t.WorkloadInfo)
		return yield(t)
	}
}

// ensures that Target implements ObjectRefProvider interface at compile time
var _ logging.ObjectRefProvider = (*Target)(nil)

// GetObject implements the ObjectRefProvider interface.
func (t *Target) GetObject() client.Object {
	return t.WorkloadInfo.Obj
}

// PreemptionPossibility represents the result
// of a preemption simulation.
type PreemptionPossibility int

const (
	// NoCandidates were found.
	NoCandidates PreemptionPossibility = iota
	// Preemption targets were found.
	Preempt
	// Preemption targets were found, and
	// all of them are outside of preempting
	// ClusterQueue.
	Reclaim
)

func (p PreemptionPossibility) String() string {
	switch p {
	case NoCandidates:
		return "NoCandidates"
	case Preempt:
		return "Preempt"
	case Reclaim:
		return "Reclaim"
	}
	return "Unknown"
}
