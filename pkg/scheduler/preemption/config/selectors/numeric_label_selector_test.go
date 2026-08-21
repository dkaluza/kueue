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
	"slices"
	"testing"

	ctrlLog "sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/google/go-cmp/cmp"
	"k8s.io/utils/ptr"

	kueuev1beta2 "sigs.k8s.io/kueue/apis/kueue/v1beta2"
	utiltesting "sigs.k8s.io/kueue/pkg/util/testing/v1beta2"
	"sigs.k8s.io/kueue/pkg/workload"
)

func TestNumericLabelFilter(t *testing.T) {
	cases := map[string]struct {
		config     kueuev1beta2.NumericLabelConstraint
		preemptor  *workload.Info
		candidates []*workload.Info
		wantNames  []string
	}{
		"candidate less than or equal to preemptor": {
			config: kueuev1beta2.NumericLabelConstraint{
				Key:          "tpu-size",
				DefaultValue: ptr.To[int32](1),
				Relation:     ptr.To(kueuev1beta2.LowerOrEqual),
			},
			preemptor: workload.NewInfo(utiltesting.MakeWorkload("p", "").Labels(map[string]string{"tpu-size": "8"}).Obj()),
			candidates: []*workload.Info{
				workload.NewInfo(utiltesting.MakeWorkload("c1", "").Labels(map[string]string{"tpu-size": "4"}).Obj()),
				workload.NewInfo(utiltesting.MakeWorkload("c2", "").Labels(map[string]string{"tpu-size": "8"}).Obj()),
				workload.NewInfo(utiltesting.MakeWorkload("c3", "").Labels(map[string]string{"tpu-size": "16"}).Obj()),
			},
			wantNames: []string{"c1", "c2"},
		},
		"candidate less than preemptor": {
			config: kueuev1beta2.NumericLabelConstraint{
				Key:          "tpu-size",
				DefaultValue: ptr.To[int32](1),
				Relation:     ptr.To(kueuev1beta2.Lower),
			},
			preemptor: workload.NewInfo(utiltesting.MakeWorkload("p", "").Labels(map[string]string{"tpu-size": "8"}).Obj()),
			candidates: []*workload.Info{
				workload.NewInfo(utiltesting.MakeWorkload("c1", "").Labels(map[string]string{"tpu-size": "4"}).Obj()),
				workload.NewInfo(utiltesting.MakeWorkload("c2", "").Labels(map[string]string{"tpu-size": "8"}).Obj()),
				workload.NewInfo(utiltesting.MakeWorkload("c3", "").Labels(map[string]string{"tpu-size": "16"}).Obj()),
			},
			wantNames: []string{"c1"},
		},
		"candidate greater than preemptor": {
			config: kueuev1beta2.NumericLabelConstraint{
				Key:          "tpu-size",
				DefaultValue: ptr.To[int32](1),
				Relation:     ptr.To(kueuev1beta2.Greater),
			},
			preemptor: workload.NewInfo(utiltesting.MakeWorkload("p", "").Labels(map[string]string{"tpu-size": "8"}).Obj()),
			candidates: []*workload.Info{
				workload.NewInfo(utiltesting.MakeWorkload("c1", "").Labels(map[string]string{"tpu-size": "4"}).Obj()),
				workload.NewInfo(utiltesting.MakeWorkload("c2", "").Labels(map[string]string{"tpu-size": "8"}).Obj()),
				workload.NewInfo(utiltesting.MakeWorkload("c3", "").Labels(map[string]string{"tpu-size": "16"}).Obj()),
			},
			wantNames: []string{"c3"},
		},
		"candidate greater than or equal to preemptor": {
			config: kueuev1beta2.NumericLabelConstraint{
				Key:          "tpu-size",
				DefaultValue: ptr.To[int32](1),
				Relation:     ptr.To(kueuev1beta2.GreaterOrEquals),
			},
			preemptor: workload.NewInfo(utiltesting.MakeWorkload("p", "").Labels(map[string]string{"tpu-size": "8"}).Obj()),
			candidates: []*workload.Info{
				workload.NewInfo(utiltesting.MakeWorkload("c1", "").Labels(map[string]string{"tpu-size": "4"}).Obj()),
				workload.NewInfo(utiltesting.MakeWorkload("c2", "").Labels(map[string]string{"tpu-size": "8"}).Obj()),
				workload.NewInfo(utiltesting.MakeWorkload("c3", "").Labels(map[string]string{"tpu-size": "16"}).Obj()),
			},
			wantNames: []string{"c2", "c3"},
		},
		"candidate uses default value when label key is missing": {
			config: kueuev1beta2.NumericLabelConstraint{
				Key:          "size",
				DefaultValue: ptr.To[int32](4),
				Relation:     ptr.To(kueuev1beta2.Greater),
			},
			preemptor: workload.NewInfo(utiltesting.MakeWorkload("p", "").Labels(map[string]string{"size": "2"}).Obj()),
			candidates: []*workload.Info{
				workload.NewInfo(utiltesting.MakeWorkload("c1", "").Labels(map[string]string{"other-key": "123"}).Obj()),
				workload.NewInfo(utiltesting.MakeWorkload("c2", "").Labels(map[string]string{"size": "1"}).Obj()),
				workload.NewInfo(utiltesting.MakeWorkload("c3", "").Labels(map[string]string{"other-key": "1"}).Obj()),
			},
			wantNames: []string{"c1", "c3"},
		},
		"candidate uses default value safely filtering candidates not satisfying relation": {
			config: kueuev1beta2.NumericLabelConstraint{
				Key:          "size",
				DefaultValue: ptr.To[int32](1),
				Relation:     ptr.To(kueuev1beta2.Greater),
			},
			preemptor: workload.NewInfo(utiltesting.MakeWorkload("p", "").Labels(map[string]string{"size": "8"}).Obj()),
			candidates: []*workload.Info{
				workload.NewInfo(utiltesting.MakeWorkload("c1", "").Labels(map[string]string{"other-key": "123"}).Obj()), // c1 is 1 <= 8 (rejected)
				workload.NewInfo(utiltesting.MakeWorkload("c2", "").Labels(map[string]string{"size": "16"}).Obj()),
			},
			wantNames: []string{"c2"},
		},
		"candidate skips preemption entirely when label is missing and default is nil": {
			config: kueuev1beta2.NumericLabelConstraint{
				Key:      "size",
				Relation: ptr.To(kueuev1beta2.LowerOrEqual),
			},
			preemptor: workload.NewInfo(utiltesting.MakeWorkload("p", "").Labels(map[string]string{"size": "8"}).Obj()),
			candidates: []*workload.Info{
				workload.NewInfo(utiltesting.MakeWorkload("c1", "").Labels(map[string]string{"other-key": "123"}).Obj()), // Skips because no default!
				workload.NewInfo(utiltesting.MakeWorkload("c2", "").Labels(map[string]string{"size": "4"}).Obj()),
			},
			wantNames: []string{"c2"},
		},
		"preemptor cannot preempt when label is missing and default is nil": {
			config: kueuev1beta2.NumericLabelConstraint{
				Key:      "size",
				Relation: ptr.To(kueuev1beta2.LowerOrEqual),
			},
			preemptor: workload.NewInfo(utiltesting.MakeWorkload("p", "").Labels(map[string]string{"other-key": "123"}).Obj()),
			candidates: []*workload.Info{
				workload.NewInfo(utiltesting.MakeWorkload("c1", "").Labels(map[string]string{"size": "2"}).Obj()),
				workload.NewInfo(utiltesting.MakeWorkload("c2", "").Labels(map[string]string{"size": "8"}).Obj()),
			},
			wantNames: []string{}, // neither candidate is permitted
		},
		"malformed labels fallback to default": {
			config: kueuev1beta2.NumericLabelConstraint{
				Key:          "size",
				DefaultValue: ptr.To[int32](2),
				Relation:     ptr.To(kueuev1beta2.LowerOrEqual),
			},
			preemptor: workload.NewInfo(utiltesting.MakeWorkload("p", "").Labels(map[string]string{"size": "invalid"}).Obj()),
			candidates: []*workload.Info{
				workload.NewInfo(utiltesting.MakeWorkload("c1", "").Labels(map[string]string{"size": "invalid-candidate"}).Obj()),
				workload.NewInfo(utiltesting.MakeWorkload("c2", "").Labels(map[string]string{"size": "4"}).Obj()),
			},
			wantNames: []string{"c1"},
		},
		"reject candidates below min value": {
			config: kueuev1beta2.NumericLabelConstraint{
				Key:      "size",
				MinValue: ptr.To[int32](4),
			},
			preemptor: workload.NewInfo(utiltesting.MakeWorkload("p", "").Labels(map[string]string{"size": "16"}).Obj()),
			candidates: []*workload.Info{
				workload.NewInfo(utiltesting.MakeWorkload("c1", "").Labels(map[string]string{"size": "2"}).Obj()),
				workload.NewInfo(utiltesting.MakeWorkload("c2", "").Labels(map[string]string{"size": "8"}).Obj()),
			},
			wantNames: []string{"c2"},
		},
		"candidate directly at min value edge case is permitted": {
			config: kueuev1beta2.NumericLabelConstraint{
				Key:      "size",
				MinValue: ptr.To[int32](8), // MinValue exactly matches candidate!
			},
			preemptor: workload.NewInfo(utiltesting.MakeWorkload("p", "").Labels(map[string]string{"size": "16"}).Obj()),
			candidates: []*workload.Info{
				workload.NewInfo(utiltesting.MakeWorkload("c1", "").Labels(map[string]string{"size": "4"}).Obj()),
				workload.NewInfo(utiltesting.MakeWorkload("c2", "").Labels(map[string]string{"size": "8"}).Obj()), // Survives
			},
			wantNames: []string{"c2"},
		},
		"candidate directly at max value edge case is permitted": {
			config: kueuev1beta2.NumericLabelConstraint{
				Key:      "size",
				MaxValue: ptr.To[int32](32), // MaxValue exactly matches candidate!
			},
			preemptor: workload.NewInfo(utiltesting.MakeWorkload("p", "").Labels(map[string]string{"size": "64"}).Obj()),
			candidates: []*workload.Info{
				workload.NewInfo(utiltesting.MakeWorkload("c1", "").Labels(map[string]string{"size": "64"}).Obj()), // Rejected by MaxValue
				workload.NewInfo(utiltesting.MakeWorkload("c2", "").Labels(map[string]string{"size": "32"}).Obj()), // Survives exactly at MaxValue
			},
			wantNames: []string{"c2"},
		},
		"reject candidates above max value": {
			config: kueuev1beta2.NumericLabelConstraint{
				Key:      "size",
				MaxValue: ptr.To[int32](10),
			},
			preemptor: workload.NewInfo(utiltesting.MakeWorkload("p", "").Labels(map[string]string{"size": "32"}).Obj()),
			candidates: []*workload.Info{
				workload.NewInfo(utiltesting.MakeWorkload("c1", "").Labels(map[string]string{"size": "8"}).Obj()),
				workload.NewInfo(utiltesting.MakeWorkload("c2", "").Labels(map[string]string{"size": "16"}).Obj()),
			},
			wantNames: []string{"c1"},
		},
		"unsupported relation constraint permits all falling back": {
			config: kueuev1beta2.NumericLabelConstraint{
				Key:          "size",
				DefaultValue: ptr.To[int32](1),
				Relation:     ptr.To[kueuev1beta2.RelativeConstraint]("Unsupported"),
			},
			preemptor: workload.NewInfo(utiltesting.MakeWorkload("p", "").Labels(map[string]string{"size": "2"}).Obj()),
			candidates: []*workload.Info{
				workload.NewInfo(utiltesting.MakeWorkload("c1", "").Labels(map[string]string{"size": "4"}).Obj()),
				workload.NewInfo(utiltesting.MakeWorkload("c2", "").Labels(map[string]string{"size": "8"}).Obj()),
			},
			wantNames: []string{"c1", "c2"},
		},
		"full rejection resulting in empty slice": {
			config: kueuev1beta2.NumericLabelConstraint{
				Key:          "size",
				DefaultValue: ptr.To[int32](1),
				Relation:     ptr.To(kueuev1beta2.Greater),
			},
			preemptor: workload.NewInfo(utiltesting.MakeWorkload("p", "").Labels(map[string]string{"size": "32"}).Obj()),
			candidates: []*workload.Info{
				workload.NewInfo(utiltesting.MakeWorkload("c1", "").Labels(map[string]string{"size": "8"}).Obj()),
				workload.NewInfo(utiltesting.MakeWorkload("c2", "").Labels(map[string]string{"size": "16"}).Obj()),
			},
			wantNames: []string{},
		},
		"absolute bounds effectively filter candidates without relation defined": {
			config: kueuev1beta2.NumericLabelConstraint{
				Key:          "size",
				DefaultValue: ptr.To[int32](1),
				MinValue:     ptr.To[int32](4),
			},
			preemptor: workload.NewInfo(utiltesting.MakeWorkload("p", "").Labels(map[string]string{"size": "2"}).Obj()),
			candidates: []*workload.Info{
				workload.NewInfo(utiltesting.MakeWorkload("c1", "").Labels(map[string]string{"size": "2"}).Obj()),
				workload.NewInfo(utiltesting.MakeWorkload("c2", "").Labels(map[string]string{"size": "8"}).Obj()),
			},
			wantNames: []string{"c2"},
		},
		"complex scenario combining bounds, relations, defaults, and distraction labels": {
			config: kueuev1beta2.NumericLabelConstraint{
				Key:          "size",
				DefaultValue: ptr.To[int32](4),
				Relation:     ptr.To(kueuev1beta2.LowerOrEqual),
				MinValue:     ptr.To[int32](2),
				MaxValue:     ptr.To[int32](16),
			},
			preemptor: workload.NewInfo(utiltesting.MakeWorkload("p", "").Labels(map[string]string{"size": "8"}).Obj()),
			candidates: []*workload.Info{
				workload.NewInfo(utiltesting.MakeWorkload("c1", "").Labels(map[string]string{"size": "1"}).Obj()),        // Rejected by MinValue
				workload.NewInfo(utiltesting.MakeWorkload("c2", "").Labels(map[string]string{"size": "10"}).Obj()),       // Rejected by Relation (> 8)
				workload.NewInfo(utiltesting.MakeWorkload("c3", "").Labels(map[string]string{"size": "24"}).Obj()),       // Rejected by MaxValue and Relation
				workload.NewInfo(utiltesting.MakeWorkload("c4", "").Labels(map[string]string{"size": "4"}).Obj()),        // Survives (2 <= 4 <= 16 and 4 <= 8)
				workload.NewInfo(utiltesting.MakeWorkload("c5", "").Labels(map[string]string{"other-key": "123"}).Obj()), // Survives (default 4 fits boundaries and relation)
				workload.NewInfo(utiltesting.MakeWorkload("c6", "").Labels(map[string]string{"size": "invalid"}).Obj()),  // Survives (default 4 fits all rules)
				workload.NewInfo(utiltesting.MakeWorkload("c7", "").Labels(map[string]string{"size": "8"}).Obj()),        // Survives (matches Preemptor exactly)
			},
			wantNames: []string{"c4", "c5", "c6", "c7"},
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			filter := NewNumericLabelFilter(tc.config)
			got := filter.Filter(ctrlLog.Log, tc.preemptor, tc.candidates)

			var wantCandidates []*workload.Info
			for _, c := range tc.candidates {
				if slices.Contains(tc.wantNames, c.Obj.Name) {
					wantCandidates = append(wantCandidates, c)
				}
			}

			if diff := cmp.Diff(wantCandidates, got); diff != "" {
				t.Errorf("Unexpected filtered candidates (-want +got):\n%s", diff)
			}
		})
	}
}
