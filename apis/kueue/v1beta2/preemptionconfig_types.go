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
