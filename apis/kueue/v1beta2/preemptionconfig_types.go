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

package v1beta2

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// RelativeConstraint defines how a specified numeric property (e.g., a label value) of the preemptor compares to the candidate.
// Possible values are:
// - "Lower": permits preemption if candidate < preemptor
// - "Greater": permits preemption if candidate > preemptor
// - "LowerOrEqual": permits preemption if candidate <= preemptor
// - "GreaterOrEqual": permits preemption if candidate >= preemptor
//
// +kubebuilder:validation:Enum=Lower;Greater;LowerOrEqual;GreaterOrEqual
type RelativeConstraint string

const (
	// Lower permits preemption if candidate < preemptor
	Lower RelativeConstraint = "Lower"
	// Greater permits preemption if candidate > preemptor
	Greater RelativeConstraint = "Greater"
	// LowerOrEqual permits preemption if candidate <= preemptor
	LowerOrEqual RelativeConstraint = "LowerOrEqual"
	// GreaterOrEqual permits preemption if candidate >= preemptor
	GreaterOrEqual RelativeConstraint = "GreaterOrEqual"
)

// NumericLabelConstraint describes the configurations for filtering a numerical label.
// For example, this can be used to filter candidates based on topology domains, such as the
// "number of TPUs". If a preemptor requires a large topology, you can set key="tpu-size"
// and relation="Lower", allowing it to preempt smaller workloads rather than disrupting
// other large topology workloads.
// Please note that you should remember to append the designated label to the list of labels
// copied to the workload via the Kueue main configuration.
// If neither Relation, MinValue, nor MaxValue are specified, the constraint checks only that
// candidate workloads possess the designated label key with a valid integer.
type NumericLabelConstraint struct {
	// Key is the label key that stores the integer value.
	Key string `json:"key"`
	// DefaultValue is used when a workload does not have the label key
	// or value under the key cannot be parsed as an integer.
	// If not specified workloads without the label or
	// with label value not parsable as int are treated as incomparable by relation (if specified),
	// and therefore excluded from preemption candidates.
	// +optional
	DefaultValue *int32 `json:"defaultValue,omitempty"`
	// Relation defines how the preemptor compares to the candidate.
	// +optional
	Relation *RelativeConstraint `json:"relation,omitempty"`
	// MinValue specifies the lowest label value a workload must have to be considered for preemption.
	// If not specified, no lower bound is enforced.
	// +optional
	MinValue *int32 `json:"minValue,omitempty"`
	// MaxValue specifies the highest label value a workload must have to be considered for preemption.
	// If not specified, no upper bound is enforced.
	// +optional
	MaxValue *int32 `json:"maxValue,omitempty"`
}

// +genclient
// +genclient:nonNamespaced
// +kubebuilder:object:root=true
// +kubebuilder:storageversion
// +kubebuilder:resource:scope=Cluster,shortName={preempcfg}
type PreemptionConfig struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              PreemptionConfigSpec `json:"spec,omitempty"`
}

// +kubebuilder:object:root=true

// PreemptionConfigList contains a list of PreemptionConfig
type PreemptionConfigList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []PreemptionConfig `json:"items"`
}

type PreemptionConfigSpec struct {
	// Rules to select preemption candidates.
	Rules []PreemptionRule `json:"rules,omitempty"`
	// Ordering of preemption candidates evaluated sequentially as a multi-key comparator chain.
	// The order is always deterministic, as the Workload UID is used as the final tie-breaker.
	// If not set, candidates will be ordered by default like this:
	// 1. Priority (Ascending: lowest priority first)
	// 2. AdmissionTimestamp (Descending: most recently admitted first, protecting long-running workloads)
	// 3. UID (Ascending: deterministic tie-breaker)
	// +optional
	Ordering []Order `json:"ordering,omitempty"`
}

type PreemptionRuleTrigger string

const (
	InsufficientQuota    PreemptionRuleTrigger = "InsufficientQuota"
	QuotaReclaimRequired PreemptionRuleTrigger = "QuotaReclaimRequired"
	InsufficientTopology PreemptionRuleTrigger = "InsufficientTopology"
)

type PreemptionRule struct {
	Name string `json:"name,omitempty"`

	// Label Selector indicating which workloads can trigger preemptions
	// using this rule.
	MatchingPreemptorWorkloads metav1.LabelSelector `json:"matchingPreemptorWorkloads,omitempty"`

	Trigger PreemptionRuleTrigger `json:"trigger,omitempty"`

	// How long the trigger has to occur to start preempting workloads specified by candidates. 0s indicates that preemptions can be started immediately. Default is 0s.
	MinTriggerRequiredDuration metav1.Duration `json:"minTriggerRequiredDuration,omitempty"`

	// Selection rules for workloads that are candidates for preemption.
	// Candidates resulting from multiple selectors are summed into one set. No selectors result in empty candidate set, thereby disallowing any preemptions with this rule.
	Candidates []PreemptionCandidateSelector `json:"candidates,omitempty"`
}

// PreemptionRelationConstraint specifies the relational boundary between
// the preempting workload's queue and candidate workloads' queues.
// Possible values are:
// - "SameLocalQueue": restricts preemption candidates to workloads submitted to the exact same LocalQueue (matching name and namespace).
// - "SameClusterQueue": restricts preemption candidates to workloads submitted to the same ClusterQueue as the preemptor.
// - "SameCohort": restricts preemption candidates to workloads in ClusterQueues that share the exact same immediate direct Cohort, as well as workloads in the preemptor's own ClusterQueue (even if standalone).
// - "SameCohortTree": restricts preemption candidates to workloads in ClusterQueues that belong to the same Cohort Tree (sharing the same root ancestor Cohort), as well as workloads in the preemptor's own ClusterQueue (even if standalone).
// - "AnyClusterQueue": places no relationship restrictions on preemption candidates.
//
// +kubebuilder:validation:Enum=SameLocalQueue;SameClusterQueue;SameCohort;SameCohortTree;AnyClusterQueue
type PreemptionRelationConstraint string

const (
	// SameLocalQueue restricts preemption candidates to workloads submitted
	// to the exact same LocalQueue (matching name and namespace).
	SameLocalQueue PreemptionRelationConstraint = "SameLocalQueue"

	// SameClusterQueue restricts preemption candidates to workloads submitted
	// to the same ClusterQueue as the preemptor.
	SameClusterQueue PreemptionRelationConstraint = "SameClusterQueue"

	// SameCohort restricts preemption candidates to workloads in ClusterQueues
	// that share the exact same immediate direct Cohort, as well as workloads in the
	// preemptor's own ClusterQueue (even if standalone and lacking a parent cohort).
	SameCohort PreemptionRelationConstraint = "SameCohort"

	// SameCohortTree restricts preemption candidates to workloads in ClusterQueues
	// that belong to the same Cohort Tree (sharing the same root ancestor Cohort),
	// as well as workloads in the preemptor's own ClusterQueue (even if standalone and lacking a parent cohort).
	SameCohortTree PreemptionRelationConstraint = "SameCohortTree"

	// AnyClusterQueue places no relationship restrictions on preemption candidates.
	AnyClusterQueue PreemptionRelationConstraint = "AnyClusterQueue"
)

// PreemptionCandidateSelector defines the selection criteria for workloads that are candidates for preemption.
type PreemptionCandidateSelector struct {
	// RelationRequirement specifies the queue or cohort relation boundary to the preemptor workload.
	//
	// +kubebuilder:validation:Required
	RelationRequirement PreemptionRelationConstraint `json:"relationRequirement"`

	// NumericLabels defines rules for filtering candidates using custom numeric labels on the Workload resource.
	// Multiple numeric label constraints are joined using logical AND (all must be satisfied).
	// If not set does not add any additional candidate filtering.
	// +optional
	NumericLabels []NumericLabelConstraint `json:"numericLabels,omitempty"`

	// PreemptingWorkloadPrioritySelector specifies a label selector matching labels
	// on the preemptor workload's PriorityClass or WorkloadPriorityClass.
	// Workloads whose priority class matches the selector can trigger preemption of candidates defined by this selector.
	// If not specified or empty, all preemptor priority classes are accepted.
	// +optional
	PreemptingWorkloadPrioritySelector *metav1.LabelSelector `json:"preemptingWorkloadPrioritySelector,omitempty"`

	// CandidateWorkloadPrioritySelector specifies a label selector matching labels
	// on the candidate workload's PriorityClass or WorkloadPriorityClass.
	// Workloads whose priority class matches the selector are permitted as preemption candidates.
	// If not specified or empty, all candidate priority classes are accepted.
	// +optional
	CandidateWorkloadPrioritySelector *metav1.LabelSelector `json:"candidateWorkloadPrioritySelector,omitempty"`

	// RelativeWorkloadPriority defines how the preemptor's priority compares to the candidate's priority.
	// For example "Lower" means that only workloads with lower priority will be allowed as preemption candidates.
	// The comparison is made using effective priority (accounting for priority boost if enabled).
	// If nil, no relative priority check is enforced.
	// +optional
	RelativeWorkloadPriority *RelativeConstraint `json:"relativeWorkloadPriority,omitempty"`
}

// OrderingField specifies the property of candidate workloads to sort by during preemption evaluation.
// Supported values are:
// - "Priority": orders workloads by effective priority (accounting for priority boost if enabled).
//   - Ascending (default): lowest priority first.
//   - Descending: highest priority first.
//
// - "AdmissionTimestamp": orders workloads by the timestamp when quota was reserved (admitted).
//   - Ascending (default): oldest admitted workloads first and most recently admitted last.
//   - Descending: most recently admitted workloads first and oldest admitted last.
//
// - "IsOtherCQ": orders workloads based on whether they belong to a different ClusterQueue than the preemptor.
//   - Ascending (default): workloads from the same ClusterQueue first, followed by other ClusterQueues.
//   - Descending: workloads from other ClusterQueues first, followed by the same ClusterQueue.
//
// - "IsOtherCohort": orders workloads based on whether they belong to a different Cohort than the preemptor.
//   - Ascending (default): workloads from the same Cohort first, followed by other Cohorts.
//   - Descending: workloads from other Cohorts first, followed by the same Cohort.
//
// +kubebuilder:validation:Enum=Priority;AdmissionTimestamp;IsOtherCQ;IsOtherCohort
type OrderingField string

const (
	// Priority orders candidates by effective priority (accounting for priority boost if enabled).
	// Ascending order places lowest priority candidates first.
	Priority OrderingField = "Priority"

	// AdmissionTimestamp orders candidates by the time quota was reserved.
	// Ascending order places oldest admitted candidates first and most recently admitted last.
	AdmissionTimestamp OrderingField = "AdmissionTimestamp"

	// IsOtherCQ orders candidates based on whether their ClusterQueue differs from the preemptor.
	// Ascending order places workloads from the same ClusterQueue first.
	IsOtherCQ OrderingField = "IsOtherCQ"

	// IsOtherCohort orders candidates based on whether their direct Cohort differs from the preemptor.
	// Ascending order places workloads from the same Cohort first.
	IsOtherCohort OrderingField = "IsOtherCohort"
)

// OrderingDirection specifies the sort direction for a candidate ordering criterion.
// Possible values are:
// - "Ascending": sort in natural ascending order (default).
// - "Descending": sort in reverse/descending order.
//
// +kubebuilder:validation:Enum=Ascending;Descending
type OrderingDirection string

const (
	// Ascending sorts candidate workloads in natural order (e.g., lowest priority first, oldest admission first, or same CQ/Cohort first).
	Ascending OrderingDirection = "Ascending"

	// Descending sorts candidate workloads in reverse order (e.g., highest priority first, newest admission first, or other CQ/Cohort first).
	Descending OrderingDirection = "Descending"
)

// Order specifies a single sorting criterion and direction for ordering preemption candidates.
// Multiple Order criteria are evaluated sequentially as a multi-key comparator chain,
// with ties broken by Workload UID for deterministic ordering.
type Order struct {
	// OrderingField specifies the field to sort preemption candidates by.
	//
	// +kubebuilder:validation:Required
	OrderingField OrderingField `json:"orderingField"`

	// Direction specifies the sorting direction (Ascending or Descending).
	// Defaults to Ascending if not specified.
	//
	// +kubebuilder:default=Ascending
	// +optional
	Direction OrderingDirection `json:"direction,omitempty"`
}
