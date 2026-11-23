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

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// +genclient
// +genclient:nonNamespaced
// +kubebuilder:object:root=true
// +kubebuilder:storageversion
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster,shortName={preemplim}

// PreemptionLimit is the Schema for the preemptionlimits API
type PreemptionLimit struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is the standard object metadata.
	// +optional
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// spec defines the desired state of the PreemptionLimit.
	// +optional
	Spec PreemptionLimitSpec `json:"spec"`

	// status defines the observed state of the PreemptionLimit.
	// +optional
	Status PreemptionLimitStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// PreemptionLimitList contains a list of PreemptionLimit
type PreemptionLimitList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []PreemptionLimit `json:"items"`
}

// PreemptionLimitAggregation specifies the grouping level for counting preemption events.
// Possible values are:
// - "Global": restricts the total number of preemption events across the entire cluster within the sliding time window.
// - "ByPreemptorClusterQueue": restricts the number of preemption events triggered by workloads originating from a specific ClusterQueue within the sliding time window.
// - "ByPreempteeClusterQueue": restricts the number of preemption events targeting workloads belonging to a specific ClusterQueue within the sliding time window.
// - "ByPreempteeWorkload": restricts how many times an individual workload can be preempted within the sliding time window.
//
// +kubebuilder:validation:Enum=Global;ByPreemptorClusterQueue;ByPreempteeClusterQueue;ByPreempteeWorkload
type PreemptionLimitAggregation string

const (
	// GlobalLimitAggregation restricts the total number of preemption events
	// across the entire cluster within the sliding time window.
	GlobalLimitAggregation PreemptionLimitAggregation = "Global"

	// ByPreemptorCQLimitAggregation restricts the number of preemption events triggered by workloads
	// originating from a specific ClusterQueue within the sliding time window.
	ByPreemptorCQLimitAggregation PreemptionLimitAggregation = "ByPreemptorClusterQueue"

	// ByPreempteeCQLimitAggregation restricts the number of preemption events targeting workloads
	// belonging to a specific ClusterQueue within the sliding time window.
	ByPreempteeCQLimitAggregation PreemptionLimitAggregation = "ByPreempteeClusterQueue"

	// ByPreempteeWorkloadLimitAggregation restricts how many times an individual workload
	// can be preempted within the sliding time window.
	ByPreempteeWorkloadLimitAggregation PreemptionLimitAggregation = "ByPreempteeWorkload"
)

// PreemptionLimitSpec defines the desired state of PreemptionLimit
type PreemptionLimitSpec struct {
	// aggregation specifies the grouping level for counting preemption events under this limit.
	//
	// +required
	Aggregation PreemptionLimitAggregation `json:"aggregation,omitempty"`

	// configSelector selects PreemptionConfigs to which this limit applies.
	// An empty selector ({}) matches all PreemptionConfigs.
	//
	// +required
	ConfigSelector metav1.LabelSelector `json:"configSelector"`

	// preemptorClusterQueueSelector selects ClusterQueues of preempting workloads to which this limit applies.
	// If not set, this limit applies to preempting workloads from all ClusterQueues.
	//
	// +optional
	PreemptorClusterQueueSelector *metav1.LabelSelector `json:"preemptorClusterQueueSelector,omitempty"`

	// preempteeClusterQueueSelector selects ClusterQueues of preemptee workloads to which this limit applies.
	// If not set, this limit applies to preemptee workloads from all ClusterQueues.
	//
	// +optional
	PreempteeClusterQueueSelector *metav1.LabelSelector `json:"preempteeClusterQueueSelector,omitempty"`

	// limit defines how many preemption events can occur within the given time window.
	// An event is defined as a confirmed (preemptor, preemptee) eviction pair.
	// Setting limit to 0 blocks all matching preemptions under this limit.
	//
	// +required
	// +kubebuilder:validation:Minimum=0
	Limit int32 `json:"limit"`

	// limitWindowSeconds specifies the sliding time window duration, in seconds.
	// Must be greater than or equal to 1 to prevent sub-second thrashing.
	//
	// +required
	// +kubebuilder:validation:Minimum=1
	LimitWindowSeconds int32 `json:"limitWindowSeconds,omitempty"`
}

// PreemptionLimitStatus defines the observed state of PreemptionLimit
type PreemptionLimitStatus struct {
	// lastUpdateTime is the time when the status was last updated.
	//
	// +kubebuilder:validation:Type=string
	// +kubebuilder:validation:Format=date-time
	// +optional
	LastUpdateTime *metav1.Time `json:"lastUpdateTime,omitempty"`

	// counts is periodically updated, for reference only.
	// Restricted to the top 1000 counts to fit within CRD size limits.
	//
	// +optional
	// +listType=atomic
	// +kubebuilder:validation:MaxItems=1000
	Counts []PreemptionLimitCount `json:"counts,omitempty"`
}

// PreemptionLimitCount tracks the number of preemptions for a single aggregation unit.
type PreemptionLimitCount struct {
	// name identifies the aggregation unit.
	// For Global aggregation it is "Global".
	// For ByPreemptorClusterQueue and ByPreempteeClusterQueue aggregations it is the ClusterQueue name.
	// For ByPreempteeWorkload aggregation it is "<namespace>/<workload-name>".
	//
	// +required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=317
	Name string `json:"name"`

	// value is the number of preemptions recorded within the sliding time window.
	//
	// +required
	// +kubebuilder:validation:Minimum=0
	Value int32 `json:"value"`
}
