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

package selectors

import (
	"strconv"

	"github.com/go-logr/logr"
	"k8s.io/klog/v2"

	kueuev1beta2 "sigs.k8s.io/kueue/apis/kueue/v1beta2"
	"sigs.k8s.io/kueue/pkg/workload"
)

type numericLabelFilter struct {
	config kueuev1beta2.NumericLabelConstraint
}

// NewNumericLabelFilter creates a highly reusable CandidateSelector to evaluate candidate workloads
// based on customized integer labels.
func NewNumericLabelFilter(cfg kueuev1beta2.NumericLabelConstraint) CandidateSelector {
	return &numericLabelFilter{
		config: cfg,
	}
}

// Filter evaluates candidates against absolute bounds and relationship boundaries with the preemptor workload.
func (f *numericLabelFilter) Filter(log logr.Logger, preemptor *workload.Info, candidates []*workload.Info) []*workload.Info {
	var matchingWorkloads []*workload.Info

	log = log.WithValues("key", f.config.Key)
	if f.config.DefaultValue != nil {
		log = log.WithValues("default", *f.config.DefaultValue)
	}

	var preemptorVal int32
	if f.config.Relation != nil {
		preemptorLog := log.WithValues("preemptor", klog.KObj(preemptor.Obj))
		pVal, ok := tryGetLabelValue(preemptorLog, preemptor, f.config.Key, f.config.DefaultValue)
		if !ok {
			// If preemptor has no valid label and no default is set, relation restrictions are inherently uncomparable.
			preemptorLog.V(2).Info("Preemptor missing required numeric label for Relation evaluation; 0 candidates permitted.")
			return nil
		}
		preemptorVal = pVal
	}

	for _, candidate := range candidates {
		candidateVal, ok := tryGetLabelValue(log.WithValues("candidate", klog.KObj(candidate.Obj)), candidate, f.config.Key, f.config.DefaultValue)
		if !ok {
			// Exclude the candidate from preemption since it lacks the label & default
			continue
		}

		// 1. Check absolute bounds (MinValue, MaxValue)
		if f.config.MinValue != nil && candidateVal < *f.config.MinValue {
			continue
		}
		if f.config.MaxValue != nil && candidateVal > *f.config.MaxValue {
			continue
		}

		// 2. Check relation constraint compared to preemptor
		if f.config.Relation == nil || matchesRelation(log, f.config.Relation, candidateVal, preemptorVal) {
			matchingWorkloads = append(matchingWorkloads, candidate)
		}
	}

	return matchingWorkloads
}

// matchesRelation dynamically evaluates relation pointers between two workload bounds.
func matchesRelation(log logr.Logger, rel *kueuev1beta2.RelativeConstraint, candidateVal, preemptorVal int32) bool {
	if rel == nil {
		return true // Default behavior when missing
	}
	switch *rel {
	case kueuev1beta2.LowerOrEqual:
		return candidateVal <= preemptorVal
	case kueuev1beta2.Greater:
		return candidateVal > preemptorVal
	case kueuev1beta2.Lower:
		return candidateVal < preemptorVal
	case kueuev1beta2.GreaterOrEquals:
		return candidateVal >= preemptorVal
	default:
		// Fallback logic for unsupported or missing relations
		log.V(3).Info("Unsupported or unhandled relation constraint evaluated", "relation", *rel)
		return true
	}
}

// tryGetLabelValue safely extracts a numeric int32 label from a workload.
// If the label is incorrectly formatted or missing, it evaluates the optionally configured default.
func tryGetLabelValue(log logr.Logger, wl *workload.Info, key string, def *int32) (int32, bool) {
	if wl == nil || wl.Obj.Labels == nil {
		if def != nil {
			return *def, true
		}
		return 0, false
	}
	valStr, exists := wl.Obj.Labels[key]
	if !exists {
		if def != nil {
			return *def, true
		}
		return 0, false
	}
	val, err := strconv.ParseInt(valStr, 10, 32)
	if err != nil {
		log.V(3).Info("Failed to parse label into integer as expected; falling back to default evaluation", "value", valStr, "error", err)
		if def != nil {
			return *def, true
		}
		return 0, false
	}
	return int32(val), true
}
