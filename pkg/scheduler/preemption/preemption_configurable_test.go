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

package preemption

import (
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	corev1 "k8s.io/api/core/v1"
	schedulingv1 "k8s.io/api/scheduling/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	clocktesting "k8s.io/utils/clock/testing"
	"k8s.io/utils/ptr"

	config "sigs.k8s.io/kueue/apis/config/v1beta2"
	kueue "sigs.k8s.io/kueue/apis/kueue/v1beta2"
	schdcache "sigs.k8s.io/kueue/pkg/cache/scheduler"
	tasindexer "sigs.k8s.io/kueue/pkg/controller/tas/indexer"
	"sigs.k8s.io/kueue/pkg/features"
	"sigs.k8s.io/kueue/pkg/scheduler/flavorassigner"
	preemptexpectations "sigs.k8s.io/kueue/pkg/scheduler/preemption/expectations"
	utilslices "sigs.k8s.io/kueue/pkg/util/slices"
	utiltas "sigs.k8s.io/kueue/pkg/util/tas"
	utiltesting "sigs.k8s.io/kueue/pkg/util/testing"
	utiltestingapi "sigs.k8s.io/kueue/pkg/util/testing/v1beta2"
	testingnode "sigs.k8s.io/kueue/pkg/util/testingjobs/node"
	"sigs.k8s.io/kueue/pkg/workload"
)

// configurableSnapCmpOpts extends snapCmpOpts for the TAS-enabled cases of this
// table. TASFlavorSnapshot holds an unexported logr.Logger (and other unexported
// helper types) which cmp cannot traverse, so the whole field is skipped; the TAS
// state is exercised through the preemption targets instead.
var configurableSnapCmpOpts = append(
	append(cmp.Options{}, snapCmpOpts...),
	cmpopts.IgnoreFields(schdcache.ClusterQueueSnapshot{}, "TASFlavors"),
)

func TestConfigurablePreemptions(t *testing.T) {
	now := time.Now()
	defaultConfigName := "default-config"
	baseCQs := []*kueue.ClusterQueue{
		utiltestingapi.MakeClusterQueue("a").
			Cohort("all").
			ResourceGroup(*utiltestingapi.MakeFlavorQuotas("default").
				Resource(corev1.ResourceCPU, "2").Obj()).
			Annotation(kueue.PreemptionConfigAnnotation, defaultConfigName).
			Obj(),
	}

	baseConfig := kueue.PreemptionConfig{
		ObjectMeta: metav1.ObjectMeta{
			Name: defaultConfigName,
		},
		Spec: kueue.PreemptionConfigSpec{
			Rules: []kueue.PreemptionRule{
				{
					Name:             "test-rule-one",
					ActivationPolicy: kueue.PreemptionRuleActivationPolicy{Trigger: kueue.Always},
					Candidates: []kueue.PreemptionCandidateSelector{
						{
							RelationRequirement: kueue.SameClusterQueue,
						},
					},
				},
			},
		},
	}

	// configWithTrigger returns baseConfig with a single rule with the given trigger
	// and selector.
	configWithTrigger := func(trigger kueue.PreemptionRuleTrigger, selector kueue.PreemptionCandidateSelector) kueue.PreemptionConfig {
		return kueue.PreemptionConfig{
			ObjectMeta: metav1.ObjectMeta{
				Name: defaultConfigName,
			},
			Spec: kueue.PreemptionConfigSpec{
				Rules: []kueue.PreemptionRule{
					{
						Name:             "test-rule-one",
						ActivationPolicy: kueue.PreemptionRuleActivationPolicy{Trigger: trigger},
						Candidates:       []kueue.PreemptionCandidateSelector{selector},
					},
				},
			},
		}
	}

	// configWithSelector returns baseConfig with a single rule of the Always tier
	// using the given selector.
	configWithSelector := func(selector kueue.PreemptionCandidateSelector) kueue.PreemptionConfig {
		return configWithTrigger(kueue.Always, selector)
	}

	lowerTierConstraint := []kueue.NumericLabelConstraint{
		{
			Key:      "preemption-tier",
			Relation: ptr.To(kueue.Lower),
		},
	}
	sameCohortConfig := configWithSelector(kueue.PreemptionCandidateSelector{
		RelationRequirement: kueue.SameCohort,
	})
	sameCohortTierConfig := configWithSelector(kueue.PreemptionCandidateSelector{
		RelationRequirement: kueue.SameCohort,
		NumericLabels:       lowerTierConstraint,
	})
	anyClusterQueueConfig := configWithSelector(kueue.PreemptionCandidateSelector{
		RelationRequirement: kueue.AnyClusterQueue,
	})
	tierConfig := configWithSelector(kueue.PreemptionCandidateSelector{
		RelationRequirement: kueue.SameClusterQueue,
		NumericLabels:       lowerTierConstraint,
	})
	// quotaTierConfig only allows preempting the workloads of the ClusterQueue when
	// the quota is not sufficient to admit the preemptor.
	quotaTierConfig := configWithTrigger(kueue.InsufficientQuota, kueue.PreemptionCandidateSelector{
		RelationRequirement: kueue.SameClusterQueue,
	})
	// tieredConfig allows preempting the workloads of a lower tier unconditionally,
	// and the remaining workloads of the ClusterQueue only if those are not enough to
	// free the quota needed by the preemptor.
	tieredConfig := kueue.PreemptionConfig{
		ObjectMeta: metav1.ObjectMeta{
			Name: defaultConfigName,
		},
		Spec: kueue.PreemptionConfigSpec{
			Rules: []kueue.PreemptionRule{
				tierConfig.Spec.Rules[0],
				quotaTierConfig.Spec.Rules[0],
			},
		},
	}

	unitWl := *utiltestingapi.MakeWorkload("unit", "").Request(corev1.ResourceCPU, "1")
	// defaultAssignment requests cpu from the default flavor, in preemption mode.
	defaultAssignment := singlePodSetAssignment(flavorassigner.ResourceAssignment{
		corev1.ResourceCPU: &flavorassigner.FlavorAssignment{
			Name: "default", Mode: flavorassigner.Preempt,
		},
	})

	// TopologyAwareScheduling fixtures, used by the QuotaFeasibleButTopologyBlocked
	// tier: a hostname topology over two nodes of 2 CPUs each.
	tasTopology := utiltestingapi.MakeDefaultOneLevelTopology("tas-single-level")
	tasFlavor := utiltestingapi.MakeResourceFlavor("tas-default").
		NodeLabel("tas-node", "true").
		TopologyName("tas-single-level").
		Obj()
	tasNode := func(name string) corev1.Node {
		return *testingnode.MakeNode(name).
			Label("tas-node", "true").
			Label(corev1.LabelHostname, name).
			StatusAllocatable(corev1.ResourceList{
				corev1.ResourceCPU:  resource.MustParse("2"),
				corev1.ResourcePods: resource.MustParse("10"),
			}).
			Ready().
			Obj()
	}
	tasNodes := []corev1.Node{tasNode("x1"), tasNode("x2")}
	tasCQs := func(nominalCPU string) []*kueue.ClusterQueue {
		return []*kueue.ClusterQueue{
			utiltestingapi.MakeClusterQueue("a").
				Cohort("all").
				ResourceGroup(*utiltestingapi.MakeFlavorQuotas("tas-default").
					Resource(corev1.ResourceCPU, nominalCPU).Obj()).
				Annotation(kueue.PreemptionConfigAnnotation, defaultConfigName).
				Obj(),
		}
	}
	// tasAdmittedWl occupies one of the two CPUs of the given node.
	tasAdmittedWl := func(name, node string) kueue.Workload {
		return *utiltestingapi.MakeWorkload(name, "").
			PodSets(*utiltestingapi.MakePodSet(kueue.DefaultPodSetName, 1).
				RequiredTopologyRequest(corev1.LabelHostname).
				Request(corev1.ResourceCPU, "1").Obj()).
			ReserveQuotaAt(utiltestingapi.MakeAdmission("a").
				PodSets(utiltestingapi.MakePodSetAssignment(kueue.DefaultPodSetName).
					Assignment(corev1.ResourceCPU, "tas-default", "1").
					Count(1).
					TopologyAssignment(utiltestingapi.MakeTopologyAssignment(utiltas.Levels(tasTopology)).
						Domain(utiltestingapi.MakeTopologyDomainAssignment([]string{node}, 1).Obj()).
						Obj()).
					Obj()).
				Obj(), now).
			Obj()
	}
	// tasIncomingWl requires both of its pods to land on the same node, which is only
	// possible once one of the admitted workloads frees its node.
	tasIncomingWl := utiltestingapi.MakeWorkload("a_incoming", "").
		PodSets(*utiltestingapi.MakePodSet(kueue.DefaultPodSetName, 2).
			RequiredTopologyRequest(corev1.LabelHostname).
			Request(corev1.ResourceCPU, "1").Obj()).
		Obj()
	tasAssignment := flavorassigner.Assignment{
		PodSets: []flavorassigner.PodSetAssignment{{
			Name: kueue.DefaultPodSetName,
			Flavors: flavorassigner.ResourceAssignment{
				corev1.ResourceCPU: &flavorassigner.FlavorAssignment{
					Name: "tas-default", Mode: flavorassigner.Preempt,
				},
			},
			Count: 2,
		}},
	}
	topologyTierConfig := configWithTrigger(kueue.QuotaFeasibleButTopologyBlocked, kueue.PreemptionCandidateSelector{
		RelationRequirement: kueue.SameClusterQueue,
	})
	cases := map[string]struct {
		clusterQueues []*kueue.ClusterQueue
		cohorts       []*kueue.Cohort
		config        kueue.PreemptionConfig
		// resourceFlavors are added to the cache on top of the default flavor.
		resourceFlavors         []*kueue.ResourceFlavor
		topologies              []*kueue.Topology
		nodes                   []corev1.Node
		workloadPriorityClasses []kueue.WorkloadPriorityClass
		priorityClasses         []schedulingv1.PriorityClass
		admitted                []kueue.Workload
		incoming                *kueue.Workload
		targetCQ                kueue.ClusterQueueReference
		// assignment is the flavor assignment of the incoming workload. It defaults
		// to defaultAssignment, requesting cpu from the default flavor.
		assignment flavorassigner.Assignment
		// fairSharing enables the Fair Sharing algorithm, instead of the classical one.
		fairSharing *config.FairSharing
		// configurablePreemptionDisabled turns the ConfigurablePreemption feature gate
		// off, which is the default for the alpha feature.
		configurablePreemptionDisabled bool
		wantPreempted                  sets.Set[string]
		wantReasons                    map[string]string
	}{
		"no candidates for CQ without config": {
			clusterQueues: []*kueue.ClusterQueue{
				utiltestingapi.MakeClusterQueue("a").
					ResourceGroup(*utiltestingapi.MakeFlavorQuotas("default").
						Resource(corev1.ResourceCPU, "2").Obj()).
					Obj(),
			},
			config: baseConfig,
			admitted: []kueue.Workload{
				*unitWl.Clone().Name("a1").SimpleReserveQuota("a", "default", now).Obj(),
				*unitWl.Clone().Name("a2").SimpleReserveQuota("a", "default", now).Obj(),
			},
			incoming:      unitWl.Clone().Name("a_incoming").Obj(),
			targetCQ:      "a",
			wantPreempted: sets.New[string](),
		},
		"no candidates when the ConfigurablePreemption feature is disabled": {
			// Same setup as the case below, which preempts a1.
			configurablePreemptionDisabled: true,
			clusterQueues:                  baseCQs,
			config:                         baseConfig,
			admitted: []kueue.Workload{
				*unitWl.Clone().Name("a1").SimpleReserveQuota("a", "default", now).Obj(),
				*unitWl.Clone().Name("a2").SimpleReserveQuota("a", "default", now).Obj(),
			},
			incoming:      unitWl.Clone().Name("a_incoming").Obj(),
			targetCQ:      "a",
			wantPreempted: sets.New[string](),
		},
		"one workload should be preempted to fit incoming workload": {
			clusterQueues: baseCQs,
			config:        baseConfig,
			admitted: []kueue.Workload{
				*unitWl.Clone().Name("a1").SimpleReserveQuota("a", "default", now).Obj(),
				*unitWl.Clone().Name("a2").SimpleReserveQuota("a", "default", now).Obj(),
			},
			incoming:      unitWl.Clone().Name("a_incoming").Obj(),
			targetCQ:      "a",
			wantPreempted: sets.New("/a1"),
		},
		"multiple workloads should be preempted to fit incoming workload": {
			clusterQueues: baseCQs,
			config:        baseConfig,
			admitted: []kueue.Workload{
				*unitWl.Clone().Name("a1").SimpleReserveQuota("a", "default", now).Obj(),
				*unitWl.Clone().Name("a2").SimpleReserveQuota("a", "default", now).Obj(),
			},
			incoming:      unitWl.Clone().Name("a_incoming").Request(corev1.ResourceCPU, "2").Obj(),
			targetCQ:      "a",
			wantPreempted: sets.New("/a1", "/a2"),
		},
		"incoming workload cannot fit because it doesn't match any rule": {
			clusterQueues: baseCQs,
			config: kueue.PreemptionConfig{
				ObjectMeta: metav1.ObjectMeta{
					Name: defaultConfigName,
				},
				Spec: kueue.PreemptionConfigSpec{
					Rules: []kueue.PreemptionRule{
						{
							Name:             "test-rule-one",
							ActivationPolicy: kueue.PreemptionRuleActivationPolicy{Trigger: kueue.Always},
							MatchingPreemptorWorkloads: metav1.LabelSelector{
								MatchLabels: map[string]string{"team": "research"},
							},
							Candidates: []kueue.PreemptionCandidateSelector{
								{
									RelationRequirement: kueue.SameClusterQueue,
								},
							},
						},
					},
				},
			},
			admitted: []kueue.Workload{
				*unitWl.Clone().Name("a1").SimpleReserveQuota("a", "default", now).Obj(),
				*unitWl.Clone().Name("a2").SimpleReserveQuota("a", "default", now).Obj(),
			},
			incoming:      unitWl.Clone().Name("a_incoming").Label("team", "batch").Obj(),
			targetCQ:      "a",
			wantPreempted: sets.New[string](),
		},
		"incoming workload cannot fit because configuration doesn't provide enough candidates": {
			clusterQueues: baseCQs,
			config: kueue.PreemptionConfig{
				ObjectMeta: metav1.ObjectMeta{
					Name: defaultConfigName,
				},
				Spec: kueue.PreemptionConfigSpec{
					Rules: []kueue.PreemptionRule{
						{
							Name:             "test-rule-one",
							ActivationPolicy: kueue.PreemptionRuleActivationPolicy{Trigger: kueue.Always},
							Candidates: []kueue.PreemptionCandidateSelector{
								{
									RelationRequirement: kueue.SameClusterQueue,
									NumericLabels: []kueue.NumericLabelConstraint{
										{
											Key:      "test-label",
											Relation: ptr.To(kueue.Lower),
										},
									},
								},
							},
						},
					},
				},
			},
			admitted: []kueue.Workload{
				*unitWl.Clone().Name("a1").SimpleReserveQuota("a", "default", now).Label("test-label", "9").Obj(),
				*unitWl.Clone().Name("a2").SimpleReserveQuota("a", "default", now).Label("test-label", "1").Obj(),
			},
			incoming:      unitWl.Clone().Name("a_incoming").Request(corev1.ResourceCPU, "2").Label("test-label", "5").Obj(),
			targetCQ:      "a",
			wantPreempted: sets.New[string](),
		},
		"returns no candidates when requested config not found by name": {
			clusterQueues: []*kueue.ClusterQueue{
				utiltestingapi.MakeClusterQueue("a").
					Cohort("all").
					ResourceGroup(*utiltestingapi.MakeFlavorQuotas("default").
						Resource(corev1.ResourceCPU, "2").Obj()).
					Annotation(kueue.PreemptionConfigAnnotation, "unknown-name").
					Obj(),
			},
			config: baseConfig,
			admitted: []kueue.Workload{
				*unitWl.Clone().Name("a1").SimpleReserveQuota("a", "default", now).Obj(),
				*unitWl.Clone().Name("a2").SimpleReserveQuota("a", "default", now).Obj(),
			},
			incoming:      unitWl.Clone().Name("a_incoming").Obj(),
			targetCQ:      "a",
			wantPreempted: sets.New[string](),
		},
		"returns no candidates when requested config has incorrect parameters": {
			clusterQueues: baseCQs,
			config: kueue.PreemptionConfig{
				ObjectMeta: metav1.ObjectMeta{
					Name: defaultConfigName,
				},
				Spec: kueue.PreemptionConfigSpec{
					Rules: []kueue.PreemptionRule{
						{
							Name:             "test-rule-one",
							ActivationPolicy: kueue.PreemptionRuleActivationPolicy{Trigger: kueue.Always},
							MatchingPreemptorWorkloads: metav1.LabelSelector{
								MatchExpressions: []metav1.LabelSelectorRequirement{
									{
										Key:      "test",
										Operator: "invalid",
									},
								},
							},
							Candidates: []kueue.PreemptionCandidateSelector{
								{
									RelationRequirement: kueue.SameClusterQueue,
								},
							},
						},
					},
				},
			},
			admitted: []kueue.Workload{
				*unitWl.Clone().Name("a1").SimpleReserveQuota("a", "default", now).Obj(),
				*unitWl.Clone().Name("a2").SimpleReserveQuota("a", "default", now).Obj(),
			},
			incoming:      unitWl.Clone().Name("a_incoming").Obj(),
			targetCQ:      "a",
			wantPreempted: sets.New[string](),
		},
		"RelativeWorkloadPriority: only candidates with lower priority are preempted": {
			clusterQueues: baseCQs,
			config: kueue.PreemptionConfig{
				ObjectMeta: metav1.ObjectMeta{
					Name: defaultConfigName,
				},
				Spec: kueue.PreemptionConfigSpec{
					Rules: []kueue.PreemptionRule{
						{
							Name:             "relative-priority-rule",
							ActivationPolicy: kueue.PreemptionRuleActivationPolicy{Trigger: kueue.Always},
							Candidates: []kueue.PreemptionCandidateSelector{
								{
									RelationRequirement:      kueue.SameClusterQueue,
									RelativeWorkloadPriority: ptr.To(kueue.Lower),
								},
							},
						},
					},
				},
			},
			admitted: []kueue.Workload{
				*unitWl.Clone().Name("a1").
					Priority(20).
					SimpleReserveQuota("a", "default", now).Obj(),
				*unitWl.Clone().Name("a2").
					Priority(120).
					SimpleReserveQuota("a", "default", now).Obj(),
			},
			incoming: unitWl.Clone().Name("a_incoming").
				Priority(100).
				Obj(),
			targetCQ:      "a",
			wantPreempted: sets.New("/a1"),
		},
		"RelativeWorkloadPriority with priority boost annotation modifies preemption ordering": {
			clusterQueues: baseCQs,
			config: kueue.PreemptionConfig{
				ObjectMeta: metav1.ObjectMeta{
					Name: defaultConfigName,
				},
				Spec: kueue.PreemptionConfigSpec{
					Rules: []kueue.PreemptionRule{
						{
							Name:             "boost-priority-rule",
							ActivationPolicy: kueue.PreemptionRuleActivationPolicy{Trigger: kueue.Always},
							Candidates: []kueue.PreemptionCandidateSelector{
								{
									RelationRequirement:      kueue.SameClusterQueue,
									RelativeWorkloadPriority: ptr.To(kueue.Lower),
								},
							},
						},
					},
				},
			},
			admitted: []kueue.Workload{
				*unitWl.Clone().Name("a1").
					Priority(100).
					Annotation("kueue.x-k8s.io/priority-boost", "-60").
					SimpleReserveQuota("a", "default", now).Obj(),
				*unitWl.Clone().Name("a2").
					Priority(60).
					SimpleReserveQuota("a", "default", now).Obj(),
			},
			incoming: unitWl.Clone().Name("a_incoming").
				Priority(50).
				Obj(),
			targetCQ:      "a",
			wantPreempted: sets.New("/a1"),
		},
		"candidates from both classical and configurable preemption algorithms are merged": {
			clusterQueues: []*kueue.ClusterQueue{
				utiltestingapi.MakeClusterQueue("a").
					Cohort("all").
					ResourceGroup(*utiltestingapi.MakeFlavorQuotas("default").
						Resource(corev1.ResourceCPU, "3").Obj()).
					Preemption(kueue.ClusterQueuePreemption{
						WithinClusterQueue: kueue.PreemptionPolicyLowerPriority,
					}).
					Annotation(kueue.PreemptionConfigAnnotation, defaultConfigName).
					Obj(),
			},
			config: kueue.PreemptionConfig{
				ObjectMeta: metav1.ObjectMeta{
					Name: defaultConfigName,
				},
				Spec: kueue.PreemptionConfigSpec{
					Rules: []kueue.PreemptionRule{
						{
							Name:             "candidate-tier-rule",
							ActivationPolicy: kueue.PreemptionRuleActivationPolicy{Trigger: kueue.Always},
							Candidates: []kueue.PreemptionCandidateSelector{
								{
									RelationRequirement: kueue.SameClusterQueue,
									NumericLabels: []kueue.NumericLabelConstraint{
										{
											Key:      "preemption-tier",
											Relation: ptr.To(kueue.Lower),
										},
									},
								},
							},
						},
					},
				},
			},
			admitted: []kueue.Workload{
				// a1 has no tier label, so it is a candidate for the classical algorithm only.
				*unitWl.Clone().Name("a1").
					Priority(10).
					SimpleReserveQuota("a", "default", now).Obj(),
				// a2 is a candidate for both algorithms.
				*unitWl.Clone().Name("a2").
					Priority(20).
					Label("preemption-tier", "1").
					SimpleReserveQuota("a", "default", now).Obj(),
				// a3 has a higher priority than the incoming workload, so it is a
				// candidate for the configurable algorithm only.
				*unitWl.Clone().Name("a3").
					Priority(200).
					Label("preemption-tier", "2").
					SimpleReserveQuota("a", "default", now).Obj(),
			},
			incoming: unitWl.Clone().Name("a_incoming").
				Priority(100).
				Label("preemption-tier", "5").
				Request(corev1.ResourceCPU, "2").
				Obj(),
			targetCQ: "a",
			// Candidates selected by the configurable rules are considered before the
			// in-ClusterQueue ones, and only as many targets as needed are preempted.
			wantPreempted: sets.New("/a1", "/a3"),
			wantReasons: map[string]string{
				"/a1": kueue.InClusterQueueReason,
				"/a3": "ConfigurablePreemption",
			},
		},
		"configurable candidate in a ClusterQueue within nominal quota is preempted": {
			clusterQueues: []*kueue.ClusterQueue{
				utiltestingapi.MakeClusterQueue("a").
					Cohort("all").
					ResourceGroup(*utiltestingapi.MakeFlavorQuotas("default").
						Resource(corev1.ResourceCPU, "1").Obj()).
					Annotation(kueue.PreemptionConfigAnnotation, defaultConfigName).
					Obj(),
				utiltestingapi.MakeClusterQueue("b").
					Cohort("all").
					ResourceGroup(*utiltestingapi.MakeFlavorQuotas("default").
						Resource(corev1.ResourceCPU, "1").Obj()).
					Obj(),
			},
			config: sameCohortConfig,
			admitted: []kueue.Workload{
				*unitWl.Clone().Name("a1").SimpleReserveQuota("a", "default", now).Obj(),
				// b is not borrowing, so b1 would be rejected by the classical
				// reclamation rules.
				*unitWl.Clone().Name("b1").SimpleReserveQuota("b", "default", now).Obj(),
			},
			incoming:      unitWl.Clone().Name("a_incoming").Obj(),
			targetCQ:      "a",
			wantPreempted: sets.New("/b1"),
			wantReasons: map[string]string{
				"/b1": "ConfigurablePreemption",
			},
		},
		"candidate selected by both algorithms is preempted once, with the classical reason": {
			clusterQueues: []*kueue.ClusterQueue{
				utiltestingapi.MakeClusterQueue("a").
					Cohort("all").
					ResourceGroup(*utiltestingapi.MakeFlavorQuotas("default").
						Resource(corev1.ResourceCPU, "1").Obj()).
					Preemption(kueue.ClusterQueuePreemption{
						WithinClusterQueue: kueue.PreemptionPolicyLowerPriority,
					}).
					Annotation(kueue.PreemptionConfigAnnotation, defaultConfigName).
					Obj(),
			},
			config: baseConfig,
			admitted: []kueue.Workload{
				*unitWl.Clone().Name("a1").Priority(10).SimpleReserveQuota("a", "default", now).Obj(),
			},
			incoming:      unitWl.Clone().Name("a_incoming").Priority(100).Obj(),
			targetCQ:      "a",
			wantPreempted: sets.New("/a1"),
			wantReasons: map[string]string{
				"/a1": kueue.InClusterQueueReason,
			},
		},
		"configurable target not needed anymore is given back": {
			clusterQueues: []*kueue.ClusterQueue{
				utiltestingapi.MakeClusterQueue("a").
					Cohort("all").
					ResourceGroup(*utiltestingapi.MakeFlavorQuotas("default").
						Resource(corev1.ResourceCPU, "3").Obj()).
					Preemption(kueue.ClusterQueuePreemption{
						WithinClusterQueue: kueue.PreemptionPolicyLowerPriority,
					}).
					Annotation(kueue.PreemptionConfigAnnotation, defaultConfigName).
					Obj(),
			},
			config: tierConfig,
			admitted: []kueue.Workload{
				// c1 is considered first, as a configurable candidate, but freeing 1 CPU
				// is not enough, and it is given back once s1 is preempted.
				*unitWl.Clone().Name("c1").
					Priority(500).
					Label("preemption-tier", "1").
					SimpleReserveQuota("a", "default", now).Obj(),
				*utiltestingapi.MakeWorkload("s1", "").Request(corev1.ResourceCPU, "2").
					Priority(10).
					SimpleReserveQuota("a", "default", now).Obj(),
			},
			incoming: utiltestingapi.MakeWorkload("a_incoming", "").Request(corev1.ResourceCPU, "2").
				Priority(100).
				Label("preemption-tier", "5").
				Obj(),
			targetCQ:      "a",
			wantPreempted: sets.New("/s1"),
			wantReasons: map[string]string{
				"/s1": kueue.InClusterQueueReason,
			},
		},
		"candidate from another Cohort is given back when it doesn't help": {
			clusterQueues: []*kueue.ClusterQueue{
				utiltestingapi.MakeClusterQueue("a").
					Cohort("one").
					ResourceGroup(*utiltestingapi.MakeFlavorQuotas("default").
						Resource(corev1.ResourceCPU, "1").Obj()).
					Annotation(kueue.PreemptionConfigAnnotation, defaultConfigName).
					Obj(),
				utiltestingapi.MakeClusterQueue("b").
					Cohort("two").
					ResourceGroup(*utiltestingapi.MakeFlavorQuotas("default").
						Resource(corev1.ResourceCPU, "1").Obj()).
					Obj(),
			},
			config: anyClusterQueueConfig,
			admitted: []kueue.Workload{
				*unitWl.Clone().Name("a1").SimpleReserveQuota("a", "default", now).Obj(),
				// b belongs to another Cohort, so preempting b1 doesn't free any quota
				// for the incoming workload.
				*unitWl.Clone().Name("b1").SimpleReserveQuota("b", "default", now).Obj(),
			},
			incoming:      unitWl.Clone().Name("a_incoming").Obj(),
			targetCQ:      "a",
			wantPreempted: sets.New("/a1"),
			wantReasons: map[string]string{
				"/a1": "ConfigurablePreemption",
			},
		},
		"already evicted configurable candidate is preempted first": {
			clusterQueues: baseCQs,
			config:        baseConfig,
			admitted: []kueue.Workload{
				*unitWl.Clone().Name("a1").SimpleReserveQuota("a", "default", now).Obj(),
				// Despite sorting after a1 by UID, z1 comes first as it is already evicted.
				*unitWl.Clone().Name("z1").SimpleReserveQuota("a", "default", now).
					Condition(metav1.Condition{
						Type:               kueue.WorkloadEvicted,
						Status:             metav1.ConditionTrue,
						Reason:             kueue.WorkloadEvictedByPreemption,
						LastTransitionTime: metav1.NewTime(now),
					}).Obj(),
			},
			incoming:      unitWl.Clone().Name("a_incoming").Obj(),
			targetCQ:      "a",
			wantPreempted: sets.New("/z1"),
		},
		"fair sharing: configurable candidate in a ClusterQueue within nominal quota is preempted": {
			clusterQueues: []*kueue.ClusterQueue{
				utiltestingapi.MakeClusterQueue("a").
					Cohort("all").
					ResourceGroup(*utiltestingapi.MakeFlavorQuotas("default").
						Resource(corev1.ResourceCPU, "1").Obj()).
					Annotation(kueue.PreemptionConfigAnnotation, defaultConfigName).
					Obj(),
				utiltestingapi.MakeClusterQueue("b").
					Cohort("all").
					ResourceGroup(*utiltestingapi.MakeFlavorQuotas("default").
						Resource(corev1.ResourceCPU, "1").Obj()).
					Obj(),
			},
			config:      sameCohortConfig,
			fairSharing: &config.FairSharing{},
			admitted: []kueue.Workload{
				*unitWl.Clone().Name("a1").SimpleReserveQuota("a", "default", now).Obj(),
				// b is not borrowing, so the Fair Sharing ordering prunes it.
				*unitWl.Clone().Name("b1").SimpleReserveQuota("b", "default", now).Obj(),
			},
			incoming:      unitWl.Clone().Name("a_incoming").Obj(),
			targetCQ:      "a",
			wantPreempted: sets.New("/b1"),
			wantReasons: map[string]string{
				"/b1": "ConfigurablePreemption",
			},
		},
		"fair sharing: strategies are preferred over the configurable candidates": {
			clusterQueues: []*kueue.ClusterQueue{
				utiltestingapi.MakeClusterQueue("a").
					Cohort("all").
					ResourceGroup(*utiltestingapi.MakeFlavorQuotas("default").
						Resource(corev1.ResourceCPU, "1").Obj()).
					Preemption(kueue.ClusterQueuePreemption{
						ReclaimWithinCohort: kueue.PreemptionPolicyAny,
					}).
					Annotation(kueue.PreemptionConfigAnnotation, defaultConfigName).
					Obj(),
				utiltestingapi.MakeClusterQueue("b").
					Cohort("all").
					ResourceGroup(*utiltestingapi.MakeFlavorQuotas("default").
						Resource(corev1.ResourceCPU, "1").Obj()).
					Obj(),
			},
			config:      sameCohortTierConfig,
			fairSharing: &config.FairSharing{},
			admitted: []kueue.Workload{
				// b is borrowing, so both b1 and b2 are Fair Sharing candidates, but only
				// b2 is selected by the configurable rules.
				*unitWl.Clone().Name("b1").SimpleReserveQuota("b", "default", now).Obj(),
				*unitWl.Clone().Name("b2").
					Label("preemption-tier", "1").
					SimpleReserveQuota("b", "default", now).Obj(),
			},
			incoming: unitWl.Clone().Name("a_incoming").
				Label("preemption-tier", "5").Obj(),
			targetCQ: "a",
			// The Fair Sharing strategy admits the workload on its own, so the
			// candidates of the Always tier are never considered.
			wantPreempted: sets.New("/b1"),
			wantReasons: map[string]string{
				"/b1": kueue.InCohortReclamationReason,
			},
		},
		"fair sharing: strategies preempt the remaining targets": {
			clusterQueues: []*kueue.ClusterQueue{
				utiltestingapi.MakeClusterQueue("a").
					Cohort("all").
					ResourceGroup(*utiltestingapi.MakeFlavorQuotas("default").
						Resource(corev1.ResourceCPU, "2").Obj()).
					Preemption(kueue.ClusterQueuePreemption{
						WithinClusterQueue: kueue.PreemptionPolicyLowerPriority,
					}).
					Annotation(kueue.PreemptionConfigAnnotation, defaultConfigName).
					Obj(),
			},
			config:      tierConfig,
			fairSharing: &config.FairSharing{},
			admitted: []kueue.Workload{
				// x1 has a higher priority than the incoming workload, so it is only
				// preemptible through the configurable rules.
				*unitWl.Clone().Name("x1").
					Priority(500).
					Label("preemption-tier", "1").
					SimpleReserveQuota("a", "default", now).Obj(),
				*unitWl.Clone().Name("x2").
					Priority(10).
					SimpleReserveQuota("a", "default", now).Obj(),
			},
			incoming: utiltestingapi.MakeWorkload("a_incoming", "").Request(corev1.ResourceCPU, "2").
				Priority(100).
				Label("preemption-tier", "5").
				Obj(),
			targetCQ:      "a",
			wantPreempted: sets.New("/x1", "/x2"),
			wantReasons: map[string]string{
				"/x1": "ConfigurablePreemption",
				"/x2": kueue.InClusterQueueReason,
			},
		},
		"InsufficientQuota tier extends the candidates of the Always tier": {
			clusterQueues: baseCQs,
			config:        tieredConfig,
			admitted: []kueue.Workload{
				// a1 belongs to a lower tier, so it is selected by the Always rule,
				// while a2 is only selected by the InsufficientQuota rule. The candidate
				// ordering prefers a2, as it has a lower priority, so it would be
				// preempted first if the tiers were considered at once.
				*unitWl.Clone().Name("a1").
					Priority(100).
					Label("preemption-tier", "1").
					SimpleReserveQuota("a", "default", now).Obj(),
				*unitWl.Clone().Name("a2").
					Priority(10).
					SimpleReserveQuota("a", "default", now).Obj(),
			},
			incoming: unitWl.Clone().Name("a_incoming").
				Request(corev1.ResourceCPU, "2").
				Label("preemption-tier", "5").Obj(),
			targetCQ:      "a",
			wantPreempted: sets.New("/a1", "/a2"),
			wantReasons: map[string]string{
				"/a1": "ConfigurablePreemption",
				"/a2": "ConfigurablePreemption",
			},
		},
		"InsufficientQuota tier is not used when the Always tier is enough": {
			clusterQueues: baseCQs,
			config:        tieredConfig,
			admitted: []kueue.Workload{
				*unitWl.Clone().Name("a1").
					Priority(100).
					Label("preemption-tier", "1").
					SimpleReserveQuota("a", "default", now).Obj(),
				*unitWl.Clone().Name("a2").
					Priority(10).
					SimpleReserveQuota("a", "default", now).Obj(),
			},
			incoming: unitWl.Clone().Name("a_incoming").
				Label("preemption-tier", "5").Obj(),
			targetCQ: "a",
			// a2 comes first in the candidate ordering, but it belongs to the
			// InsufficientQuota tier, which is not reached as the Always tier frees
			// enough quota on its own.
			wantPreempted: sets.New("/a1"),
		},
		"InsufficientQuota tier is not used when the classical algorithm is enough": {
			clusterQueues: []*kueue.ClusterQueue{
				utiltestingapi.MakeClusterQueue("a").
					Cohort("all").
					ResourceGroup(*utiltestingapi.MakeFlavorQuotas("default").
						Resource(corev1.ResourceCPU, "2").Obj()).
					Preemption(kueue.ClusterQueuePreemption{
						WithinClusterQueue: kueue.PreemptionPolicyLowerPriority,
					}).
					Annotation(kueue.PreemptionConfigAnnotation, defaultConfigName).
					Obj(),
			},
			config: quotaTierConfig,
			admitted: []kueue.Workload{
				*unitWl.Clone().Name("a1").Priority(10).SimpleReserveQuota("a", "default", now).Obj(),
				*unitWl.Clone().Name("a2").Priority(10).SimpleReserveQuota("a", "default", now).Obj(),
			},
			incoming:      unitWl.Clone().Name("a_incoming").Priority(100).Obj(),
			targetCQ:      "a",
			wantPreempted: sets.New("/a1"),
			// The classical algorithm admits the workload on its own, so the
			// InsufficientQuota tier is never reached.
			wantReasons: map[string]string{
				"/a1": kueue.InClusterQueueReason,
			},
		},
		"fair sharing: InsufficientQuota tier is used when the strategies are not enough": {
			clusterQueues: baseCQs,
			config:        quotaTierConfig,
			fairSharing:   &config.FairSharing{},
			admitted: []kueue.Workload{
				// The ClusterQueue doesn't allow preemption, so the Fair Sharing
				// algorithm has no candidate of its own.
				*unitWl.Clone().Name("a1").SimpleReserveQuota("a", "default", now).Obj(),
				*unitWl.Clone().Name("a2").SimpleReserveQuota("a", "default", now).Obj(),
			},
			incoming:      unitWl.Clone().Name("a_incoming").Obj(),
			targetCQ:      "a",
			wantPreempted: sets.New("/a1"),
			wantReasons: map[string]string{
				"/a1": "ConfigurablePreemption",
			},
		},
		"QuotaFeasibleButTopologyBlocked tier is used when the quota fits but no topology assignment is found": {
			clusterQueues:   tasCQs("4"),
			resourceFlavors: []*kueue.ResourceFlavor{tasFlavor},
			topologies:      []*kueue.Topology{tasTopology},
			nodes:           tasNodes,
			config:          topologyTierConfig,
			admitted: []kueue.Workload{
				tasAdmittedWl("a1", "x1"),
				tasAdmittedWl("a2", "x2"),
			},
			// The incoming workload fits in the quota of the ClusterQueue (2 out of
			// the 4 CPUs are used), but its 2 pods require the same node, and both
			// nodes have only 1 of their 2 CPUs free.
			incoming:      tasIncomingWl,
			assignment:    tasAssignment,
			targetCQ:      "a",
			wantPreempted: sets.New("/a1"),
			wantReasons: map[string]string{
				"/a1": "ConfigurablePreemption",
			},
		},
		"QuotaFeasibleButTopologyBlocked tier is not used when the quota is insufficient": {
			// The nominal quota only covers the admitted workloads, so the preemptor
			// is blocked by the quota rather than by the topology, and the rule of the
			// QuotaFeasibleButTopologyBlocked tier must not be applied.
			clusterQueues:   tasCQs("2"),
			resourceFlavors: []*kueue.ResourceFlavor{tasFlavor},
			topologies:      []*kueue.Topology{tasTopology},
			nodes:           tasNodes,
			config:          topologyTierConfig,
			admitted: []kueue.Workload{
				tasAdmittedWl("a1", "x1"),
				tasAdmittedWl("a2", "x2"),
			},
			incoming:      tasIncomingWl,
			assignment:    tasAssignment,
			targetCQ:      "a",
			wantPreempted: sets.New[string](),
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			features.SetFeatureGateDuringTest(t, features.ConfigurablePreemption, !tc.configurablePreemptionDisabled)
			features.SetFeatureGateDuringTest(t, features.PriorityBoost, true)
			// Only the cases exercising the QuotaFeasibleButTopologyBlocked tier need
			// TAS; the others keep running without it, as most deployments do.
			features.SetFeatureGateDuringTest(t, features.TopologyAwareScheduling, len(tc.topologies) > 0)
			ctx, log := utiltesting.ContextWithLog(t)
			// Set name as UID so that candidates sorting is predictable.
			for i := range tc.admitted {
				tc.admitted[i].UID = types.UID(tc.admitted[i].Name)
			}
			clientBuilder := utiltesting.NewClientBuilder().
				WithLists(&kueue.WorkloadList{Items: tc.admitted}).
				WithLists(&kueue.PreemptionConfigList{Items: []kueue.PreemptionConfig{tc.config}}).
				WithLists(&kueue.WorkloadPriorityClassList{Items: tc.workloadPriorityClasses}).
				WithLists(&schedulingv1.PriorityClassList{Items: tc.priorityClasses}).
				WithLists(&corev1.NodeList{Items: tc.nodes})
			if err := tasindexer.SetupIndexes(ctx, utiltesting.AsIndexer(clientBuilder)); err != nil {
				t.Fatalf("Couldn't set up the TAS indexes: %v", err)
			}
			cl := clientBuilder.Build()

			cqCache := schdcache.New(cl)
			cqCache.AddOrUpdateResourceFlavor(log, utiltestingapi.MakeResourceFlavor("default").Obj())
			for _, flavor := range tc.resourceFlavors {
				cqCache.AddOrUpdateResourceFlavor(log, flavor)
			}
			for _, topology := range tc.topologies {
				cqCache.AddOrUpdateTopology(log, topology)
			}
			for _, node := range tc.nodes {
				cqCache.TASCache().SyncNode(&node)
			}
			for _, cq := range tc.clusterQueues {
				if err := cqCache.AddClusterQueue(ctx, cq); err != nil {
					t.Fatalf("Couldn't add ClusterQueue to cache: %v", err)
				}
			}
			for _, cohort := range tc.cohorts {
				if err := cqCache.AddOrUpdateCohort(cohort); err != nil {
					t.Fatalf("Couldn't add Cohort to cache: %v", err)
				}
			}

			recorder := &utiltesting.EventRecorder{}
			preemptor := New(cl, workload.Ordering{}, recorder, tc.fairSharing, false, clocktesting.NewFakeClock(now), nil, preemptexpectations.New(), nil)

			beforeSnapshot, err := cqCache.Snapshot(ctx)
			if err != nil {
				t.Fatalf("unexpected error while building snapshot: %v", err)
			}
			snapshotWorkingCopy, err := cqCache.Snapshot(ctx)
			if err != nil {
				t.Fatalf("unexpected error while building snapshot: %v", err)
			}
			wlInfo := workload.NewInfo(tc.incoming)
			wlInfo.ClusterQueue = tc.targetCQ
			assignment := tc.assignment
			if len(assignment.PodSets) == 0 {
				assignment = defaultAssignment
			}
			targets := preemptor.GetTargets(ctx, *wlInfo, assignment, snapshotWorkingCopy)
			gotTargetsList := utilslices.Map(targets, func(t **Target) string {
				return string(workload.Key((*t).WorkloadInfo.Obj))
			})
			gotTargets := sets.New(gotTargetsList...)
			if len(targets) != len(gotTargets) {
				t.Errorf("Targets contain duplicates: %v", gotTargetsList)
			}
			if diff := cmp.Diff(tc.wantPreempted, gotTargets, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("Issued preemptions (-want,+got):\n%s", diff)
			}
			if tc.wantReasons != nil {
				gotReasons := make(map[string]string, len(targets))
				for _, target := range targets {
					gotReasons[string(workload.Key(target.WorkloadInfo.Obj))] = target.Reason
				}
				if diff := cmp.Diff(tc.wantReasons, gotReasons); diff != "" {
					t.Errorf("Preemption reasons (-want,+got):\n%s", diff)
				}
			}

			if diff := cmp.Diff(beforeSnapshot, snapshotWorkingCopy, configurableSnapCmpOpts); diff != "" {
				t.Errorf("Snapshot was modified (-initial,+end):\n%s", diff)
			}
		})
	}
}
