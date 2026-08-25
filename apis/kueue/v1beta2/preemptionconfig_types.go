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
	// GreaterOrEquals permits preemption if candidate >= preemptor
	GreaterOrEquals RelativeConstraint = "GreaterOrEqual"
)

// NumericLabelConstraint describes the configurations for filtering a numerical label.
// For example, this can be used to filter candidates based on topology domains, such as the
// "number of TPUs". If a preemptor requires a large topology, you can set key="tpu-size"
// and relation="Lower", allowing it to preempt smaller workloads rather than disrupting
// other large topology workloads.
// Please note that you should remember to append the designated label to the list of labels
// copied to the workload via the Kueue main configuration.
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

type PreemptionConfig struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              PreemptionConfigSpec `json:"spec,omitempty"`
}

type PreemptionConfigSpec struct {
	// Rules to select preemption candidates.
	Rules []PreemptionRule
	// Ordering of the preemption candidates.
	// The order will be always deterministic, as UID
	// of the workloads is used to break the ties
	// If not set workloads will be just ordered by UID.
	Ordering []OrderingField
}

type PreemptionRuleTrigger string

const (
	InsufficientQuota    PreemptionRuleTrigger = "InsufficientQuota"
	QuotaReclaimRequired PreemptionRuleTrigger = "QuotaReclaimRequired"
	InsufficientTopology PreemptionRuleTrigger = "InsufficientTopology"
)

type PreemptionRule struct {
	Name string

	// Label Selector indicating which workloads can trigger preemptions
	// using this rule.
	MatchingPreemptorWorkloads metav1.LabelSelector

	Trigger PreemptionRuleTrigger
	// How long the trigger has to occur to start preempting workloads specified by candidates. 0 indicates that preemptions can be started immediately.
	MinTriggerRequiredDurationSeconds int

	// Selection rules for workloads that are candidates for preemption.
	// Candidates resulting from multiple selectors are summed into one set. No selectors result in empty candidate set, thereby disallowing any preemptions with this rule.
	Candidates []PreemptionCandidateSelector
}

type PreemptionRelationConstraint string

const (
	SameLocalQueue   PreemptionRelationConstraint = "SameLocalQueue"
	SameClusterQueue PreemptionRelationConstraint = "SameClusterQueue"
	SameCohort       PreemptionRelationConstraint = "SameCohort"
	SameCohortTree   PreemptionRelationConstraint = "SameCohortTree"
	AnyClusterQueue  PreemptionRelationConstraint = "AnyClusterQueue"
)

type PreemptionCandidateSelector struct {
	// Required.
	RelationRequirement PreemptionRelationConstraint

	// Accepts all if not set
	// Filter candidate workloads using custom numeric labels from the workload
	// resource.
	// Multiple numeric labels are joined using AND-rule (all have to be satisfied).
	NumericLabels []NumericLabelConstraint

	// The comparison is made against the preempting workload.
	// Lower means that the candidate
	// has lower priority than the preemptor and so on. No check is made
	// if the field is nil.
	RelativeWorkloadPriority *RelativeConstraint
}

type OrderingField string

const (
	Priority           OrderingField = "Priority"
	AdmissionTimestamp OrderingField = "AdmissionTimestamp"
)
