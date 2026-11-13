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
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	kueuealpha "sigs.k8s.io/kueue/apis/kueue/v1alpha1"
)

func TestPreemptionLimitBlockMessage(t *testing.T) {
	utcPlus2 := time.FixedZone("UTC+2", 2*60*60)
	cases := map[string]struct {
		block PreemptionLimitBlock
		want  string
	}{
		"non-expiring limit omits until timestamp": {
			block: PreemptionLimitBlock{
				Limit: "blocked",
				Scope: kueuealpha.GlobalPreemptionLimitScope,
			},
			want: "preemptions blocked by PreemptionLimit blocked (aggregation Global)",
		},
		"expiring limit formats timestamp as UTC RFC3339": {
			block: PreemptionLimitBlock{
				Limit:   "per-cq",
				Scope:   kueuealpha.PreemptedCQLimitScope,
				RetryAt: new(time.Date(2026, time.January, 1, 0, 1, 0, 0, time.UTC)),
			},
			want: "preemptions blocked by PreemptionLimit per-cq (aggregation PreemptedClusterQueue) until 2026-01-01T00:01:00Z",
		},
		"non-UTC timestamp is normalized to UTC RFC3339": {
			block: PreemptionLimitBlock{
				Limit:   "per-cq",
				Scope:   kueuealpha.PreemptorCQLimitScope,
				RetryAt: new(time.Date(2026, time.January, 1, 2, 1, 0, 0, utcPlus2)),
			},
			want: "preemptions blocked by PreemptionLimit per-cq (aggregation PreemptorClusterQueue) until 2026-01-01T00:01:00Z",
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if diff := cmp.Diff(tc.want, tc.block.Message()); diff != "" {
				t.Errorf("Message() (-want,+got):\n%s", diff)
			}
		})
	}
}

func TestEarlierPreemptionLimitBlock(t *testing.T) {
	t1 := time.Date(2026, time.January, 1, 0, 0, 10, 0, time.UTC)
	t2 := time.Date(2026, time.January, 1, 0, 0, 20, 0, time.UTC)

	cases := map[string]struct {
		a    *PreemptionLimitBlock
		b    *PreemptionLimitBlock
		want *PreemptionLimitBlock
	}{
		"both nil": {},
		"first nil returns second": {
			b:    &PreemptionLimitBlock{Limit: "b", RetryAt: &t1},
			want: &PreemptionLimitBlock{Limit: "b", RetryAt: &t1},
		},
		"second nil returns first": {
			a:    &PreemptionLimitBlock{Limit: "a", RetryAt: &t1},
			want: &PreemptionLimitBlock{Limit: "a", RetryAt: &t1},
		},
		"finite RetryAt beats nil RetryAt on second": {
			a:    &PreemptionLimitBlock{Limit: "z", RetryAt: &t2},
			b:    &PreemptionLimitBlock{Limit: "a"},
			want: &PreemptionLimitBlock{Limit: "z", RetryAt: &t2},
		},
		"finite RetryAt beats nil RetryAt on first": {
			a:    &PreemptionLimitBlock{Limit: "a"},
			b:    &PreemptionLimitBlock{Limit: "z", RetryAt: &t2},
			want: &PreemptionLimitBlock{Limit: "z", RetryAt: &t2},
		},
		"earlier RetryAt wins when first is earlier": {
			a:    &PreemptionLimitBlock{Limit: "z", RetryAt: &t1},
			b:    &PreemptionLimitBlock{Limit: "a", RetryAt: &t2},
			want: &PreemptionLimitBlock{Limit: "z", RetryAt: &t1},
		},
		"earlier RetryAt wins when second is earlier": {
			a:    &PreemptionLimitBlock{Limit: "a", RetryAt: &t2},
			b:    &PreemptionLimitBlock{Limit: "z", RetryAt: &t1},
			want: &PreemptionLimitBlock{Limit: "z", RetryAt: &t1},
		},
		"equal RetryAt breaks tie by Limit name (a <= b)": {
			a:    &PreemptionLimitBlock{Limit: "alpha", RetryAt: &t1},
			b:    &PreemptionLimitBlock{Limit: "beta", RetryAt: &t1},
			want: &PreemptionLimitBlock{Limit: "alpha", RetryAt: &t1},
		},
		"equal RetryAt breaks tie by Limit name (b < a)": {
			a:    &PreemptionLimitBlock{Limit: "beta", RetryAt: &t1},
			b:    &PreemptionLimitBlock{Limit: "alpha", RetryAt: &t1},
			want: &PreemptionLimitBlock{Limit: "alpha", RetryAt: &t1},
		},
		"both nil RetryAt breaks tie by Limit name (a <= b)": {
			a:    &PreemptionLimitBlock{Limit: "alpha"},
			b:    &PreemptionLimitBlock{Limit: "beta"},
			want: &PreemptionLimitBlock{Limit: "alpha"},
		},
		"both nil RetryAt breaks tie by Limit name (b < a)": {
			a:    &PreemptionLimitBlock{Limit: "beta"},
			b:    &PreemptionLimitBlock{Limit: "alpha"},
			want: &PreemptionLimitBlock{Limit: "alpha"},
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if diff := cmp.Diff(tc.want, EarlierPreemptionLimitBlock(tc.a, tc.b)); diff != "" {
				t.Errorf("EarlierPreemptionLimitBlock() (-want,+got):\n%s", diff)
			}
		})
	}
}
