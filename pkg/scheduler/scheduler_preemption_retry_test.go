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

package scheduler

import (
	"context"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/component-base/featuregate"
	testingclock "k8s.io/utils/clock/testing"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kueuealpha "sigs.k8s.io/kueue/apis/kueue/v1alpha1"
	kueue "sigs.k8s.io/kueue/apis/kueue/v1beta2"
	qcache "sigs.k8s.io/kueue/pkg/cache/queue"
	schdcache "sigs.k8s.io/kueue/pkg/cache/scheduler"
	"sigs.k8s.io/kueue/pkg/features"
	"sigs.k8s.io/kueue/pkg/scheduler/flavorassigner"
	preemptexpectations "sigs.k8s.io/kueue/pkg/scheduler/preemption/expectations"
	"sigs.k8s.io/kueue/pkg/scheduler/preemption/limits"
	"sigs.k8s.io/kueue/pkg/scheduler/preemption/policy"
	utiltas "sigs.k8s.io/kueue/pkg/util/tas"
	utiltesting "sigs.k8s.io/kueue/pkg/util/testing"
	utiltestingalpha "sigs.k8s.io/kueue/pkg/util/testing/v1alpha1"
	utiltestingapi "sigs.k8s.io/kueue/pkg/util/testing/v1beta2"
	testingnode "sigs.k8s.io/kueue/pkg/util/testingjobs/node"
	"sigs.k8s.io/kueue/pkg/workload"
)

func TestRequeueAndUpdateRetryAt(t *testing.T) {
	now := time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)

	cases := map[string]struct {
		cohortName             kueue.CohortReference
		requeueReason          qcache.RequeueReason
		retryAt                *time.Time
		beforeRequeue          func(context.Context, client.Client, *kueue.Workload) error
		wantMoved              int
		wantClusterQueueDelays map[kueue.ClusterQueueReference]time.Duration
		wantCohortDelays       map[kueue.CohortReference]time.Duration
	}{
		"PreemptionNoCandidates with retryAt on standalone ClusterQueue arms ClusterQueue delay": {
			requeueReason:          qcache.RequeueReasonPreemptionNoCandidates,
			retryAt:                new(now.Add(10 * time.Second)),
			wantMoved:              1,
			wantClusterQueueDelays: map[kueue.ClusterQueueReference]time.Duration{"cq": 10 * time.Second},
		},
		"PreemptionNoCandidates with retryAt on cohort ClusterQueue arms root Cohort delay": {
			cohortName:       "cohort",
			requeueReason:    qcache.RequeueReasonPreemptionNoCandidates,
			retryAt:          new(now.Add(15 * time.Second)),
			wantMoved:        1,
			wantCohortDelays: map[kueue.CohortReference]time.Duration{"cohort": 15 * time.Second},
		},
		"PreemptionNoCandidates with non-future retryAt still notifies requeuer": {
			requeueReason: qcache.RequeueReasonPreemptionNoCandidates,
			retryAt:       new(now.Add(-time.Second)),
			wantMoved:     1,
		},
		"PreemptionNoCandidates without retryAt arms no delay": {
			requeueReason: qcache.RequeueReasonPreemptionNoCandidates,
		},
		"NoFit with retryAt arms ClusterQueue delay": {
			requeueReason:          qcache.RequeueReasonNoFit,
			retryAt:                new(now.Add(10 * time.Second)),
			wantMoved:              1,
			wantClusterQueueDelays: map[kueue.ClusterQueueReference]time.Duration{"cq": 10 * time.Second},
		},
		"PendingPreemption with retryAt arms no delay": {
			requeueReason: qcache.RequeueReasonPendingPreemption,
			retryAt:       new(now.Add(10 * time.Second)),
		},
		"deleted workload with retryAt arms no delay": {
			requeueReason: qcache.RequeueReasonPreemptionNoCandidates,
			retryAt:       new(now.Add(10 * time.Second)),
			beforeRequeue: func(ctx context.Context, cl client.Client, wl *kueue.Workload) error {
				return cl.Delete(ctx, wl)
			},
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			ctx, _ := utiltesting.ContextWithLog(t)
			wl := utiltestingapi.MakeWorkload("wl", "ns").Queue("lq").Request(corev1.ResourceCPU, "1").Obj()
			cq := utiltestingapi.MakeClusterQueue("cq").Cohort(tc.cohortName).Obj()
			lq := utiltestingapi.MakeLocalQueue("lq", "ns").ClusterQueue("cq").Obj()

			cl := utiltesting.NewClientBuilder().
				WithLists(&kueue.WorkloadList{Items: []kueue.Workload{*wl}}, &kueue.LocalQueueList{Items: []kueue.LocalQueue{*lq}}).
				WithObjects(utiltesting.MakeNamespace("ns"), cq).
				WithStatusSubresource(&kueue.Workload{}).
				Build()
			cqCache := schdcache.New(cl)
			if tc.cohortName != "" {
				if err := cqCache.AddOrUpdateCohort(utiltestingapi.MakeCohort(tc.cohortName).Obj()); err != nil {
					t.Fatalf("adding Cohort to cache: %v", err)
				}
			}
			if err := cqCache.AddClusterQueue(ctx, cq); err != nil {
				t.Fatalf("adding ClusterQueue to cache: %v", err)
			}
			preemptionExpectations := preemptexpectations.New()
			qManager, requeuer := qcache.NewManagerForUnitTestsWithRequeuer(cl, cqCache, qcache.WithPreemptionExpectations(preemptionExpectations))
			if tc.cohortName != "" {
				qManager.AddOrUpdateCohort(ctx, utiltestingapi.MakeCohort(tc.cohortName).Obj())
			}
			if err := qManager.AddLocalQueue(ctx, lq); err != nil {
				t.Fatalf("adding LocalQueue to manager: %v", err)
			}
			if err := qManager.AddClusterQueue(ctx, cq); err != nil {
				t.Fatalf("adding ClusterQueue to manager: %v", err)
			}
			requeuer.ProcessRequeues(ctx)

			heads := qManager.Heads(ctx)
			if len(heads) != 1 {
				t.Fatalf("Heads() returned %d workloads, want 1", len(heads))
			}

			scheduler := New(
				qManager, cqCache, cl, &utiltesting.EventRecorder{},
				WithClock(t, testingclock.NewFakeClock(now)),
				WithPreemptionExpectations(preemptionExpectations),
			)

			e := entry{
				Head:          heads[0],
				status:        nominated,
				requeueReason: tc.requeueReason,
			}
			e.recordAssignment(flavorassigner.Assignment{}, nil, tc.retryAt, nil)
			if tc.beforeRequeue != nil {
				if err := tc.beforeRequeue(ctx, cl, wl); err != nil {
					t.Fatalf("beforeRequeue(): %v", err)
				}
			}
			scheduler.requeueAndUpdate(ctx, e)

			if diff := cmp.Diff(tc.wantClusterQueueDelays, requeuer.ClusterQueueDelays(), cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("ClusterQueueDelays() (-want,+got):\n%s", diff)
			}
			if diff := cmp.Diff(tc.wantCohortDelays, requeuer.CohortDelays(), cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("CohortDelays() (-want,+got):\n%s", diff)
			}
			if got := requeuer.ProcessRequeues(ctx); got != tc.wantMoved {
				t.Errorf("ProcessRequeues() = %d, want %d", got, tc.wantMoved)
			}
		})
	}
}

func TestGetAssignmentsRetryAt(t *testing.T) {
	now := time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)
	tasTopology := utiltestingapi.MakeDefaultOneLevelTopology("tas-single-level")
	rfSpot := utiltestingapi.MakeResourceFlavor("spot").Obj()
	preemptionCfg := utiltestingalpha.MakePreemptionConfig("limits-cfg").
		Rule("within-cq", kueuealpha.InsufficientQuota,
			utiltestingalpha.MakeCandidateSelector(kueuealpha.WithinClusterQueue).Obj(),
		).
		Obj()
	preemptionLimit := utiltestingalpha.MakePreemptionLimit("per-workload", kueuealpha.PreemptedWorkloadLimitScope).
		Limit(1).LimitWindowSeconds(60).Obj()
	victim := utiltestingapi.MakeWorkload("v-od", "default").
		Queue("limits-lq").
		Request(corev1.ResourceCPU, "1").
		SimpleReserveQuota("limits-cq", "on-demand", now).
		AdmittedAt(true, now).
		Obj()
	memVictim := utiltestingapi.MakeWorkload("v-mem", "default").
		Queue("limits-lq").
		Request(corev1.ResourceMemory, "1").
		SimpleReserveQuota("limits-cq", "on-demand", now).
		AdmittedAt(true, now).
		Obj()
	defaultIncoming := utiltestingapi.MakeWorkload("incoming", "default").
		Queue("limits-lq").
		Request(corev1.ResourceCPU, "1").
		Obj()

	cases := map[string]struct {
		enableTAS         bool
		spotQuota         string
		onDemandCPU       string
		incoming          *kueue.Workload
		extraWorkloads    []*kueue.Workload
		recordedAgoByName map[policy.PreemptionLimitScopeValue]time.Duration
		nodes             []corev1.Node
		wantMode          flavorassigner.FlavorAssignmentMode
		wantRetryAt       *time.Time
		wantBlocked       *policy.PreemptionLimitBlock
	}{
		"returns retryAt and blockedLimit when all flavors are blocked by expiring limit": {
			spotQuota:   "0",
			onDemandCPU: "1",
			incoming:    defaultIncoming,
			wantMode:    flavorassigner.Preempt,
			wantRetryAt: new(now.Add(10 * time.Second)),
			wantBlocked: &policy.PreemptionLimitBlock{
				Limit:   "per-workload",
				Scope:   kueuealpha.PreemptedWorkloadLimitScope,
				RetryAt: new(now.Add(10 * time.Second)),
			},
		},
		"returns nil retryAt and blockedLimit and leaves preemptor drained when fallback flavor fits": {
			spotQuota:   "1",
			onDemandCPU: "1",
			incoming:    defaultIncoming,
			wantMode:    flavorassigner.Fit,
		},
		"ignores retryAt and blockedLimit when plan fails with NoFit due to impossible topology": {
			enableTAS:   true,
			spotQuota:   "0",
			onDemandCPU: "2",
			incoming: utiltestingapi.MakeWorkload("incoming", "default").
				Queue("limits-lq").
				PodSets(*utiltestingapi.MakePodSet(kueue.DefaultPodSetName, 2).
					Request(corev1.ResourceCPU, "1").
					RequiredTopologyRequest(corev1.LabelHostname).
					Obj()).
				Obj(),
			nodes: []corev1.Node{
				*testingnode.MakeNode("x1").
					Label("tas-node", "true").
					Label(corev1.LabelHostname, "x1").
					StatusAllocatable(corev1.ResourceList{
						corev1.ResourceCPU:  resource.MustParse("1"),
						corev1.ResourcePods: resource.MustParse("10"),
					}).
					Ready().
					Obj(),
			},
			wantMode: flavorassigner.NoFit,
		},
		"keeps earliest retryAt and blockedLimit across two reduced counts blocked until different times": {
			spotQuota:      "0",
			onDemandCPU:    "1",
			extraWorkloads: []*kueue.Workload{memVictim},
			recordedAgoByName: map[policy.PreemptionLimitScopeValue]time.Duration{
				"default/v-mem": 50 * time.Second,
				"default/v-od":  30 * time.Second,
			},
			incoming: utiltestingapi.MakeWorkload("incoming", "default").
				Queue("limits-lq").
				PodSets(
					*utiltestingapi.MakePodSet("mem-ps", 2).
						SetMinimumCount(1).
						Request(corev1.ResourceMemory, "1").
						Obj(),
					*utiltestingapi.MakePodSet("cpu-ps", 2).
						SetMinimumCount(1).
						Request(corev1.ResourceCPU, "1").
						Obj(),
				).
				Obj(),
			wantMode:    flavorassigner.NoFit,
			wantRetryAt: new(now.Add(10 * time.Second)),
			wantBlocked: &policy.PreemptionLimitBlock{
				Limit:   "per-workload",
				Scope:   kueuealpha.PreemptedWorkloadLimitScope,
				RetryAt: new(now.Add(10 * time.Second)),
			},
		},
		"clears retryAt and blockedLimit when partial reduction fits before probing larger blocked count": {
			spotQuota:   "0",
			onDemandCPU: "3",
			incoming: utiltestingapi.MakeWorkload("incoming", "default").
				Queue("limits-lq").
				PodSets(
					*utiltestingapi.MakePodSet(kueue.DefaultPodSetName, 4).
						SetMinimumCount(1).
						Request(corev1.ResourceCPU, "1").
						Obj(),
				).
				Obj(),
			wantMode: flavorassigner.Fit,
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			features.SetFeatureGatesDuringTest(t, map[featuregate.Feature]bool{
				features.PartialAdmission:             true,
				features.ConfigurablePreemptions:      true,
				features.ConfigurablePreemptionLimits: true,
				features.TopologyAwareScheduling:      tc.enableTAS,
			})
			ctx, log := utiltesting.ContextWithLog(t)
			rfOnDemandBuilder := utiltestingapi.MakeResourceFlavor("on-demand")
			if tc.enableTAS {
				rfOnDemandBuilder = rfOnDemandBuilder.NodeLabel("tas-node", "true").TopologyName("tas-single-level")
			}
			rfOnDemand := rfOnDemandBuilder.Obj()
			cq := utiltestingapi.MakeClusterQueue("limits-cq").
				Annotation(kueuealpha.PreemptionConfigNameAnnotation, "limits-cfg").
				ResourceGroup(
					*utiltestingapi.MakeFlavorQuotas("on-demand").
						Resource(corev1.ResourceCPU, tc.onDemandCPU).
						Resource(corev1.ResourceMemory, "2").
						Obj(),
					*utiltestingapi.MakeFlavorQuotas("spot").
						Resource(corev1.ResourceCPU, tc.spotQuota).
						Resource(corev1.ResourceMemory, "0").
						Obj(),
				).
				Obj()
			lq := utiltestingapi.MakeLocalQueue("limits-lq", "default").ClusterQueue("limits-cq").Obj()
			objs := []client.Object{
				utiltesting.MakeNamespace("default"),
				rfOnDemand, rfSpot, cq, lq, preemptionCfg, preemptionLimit, victim, tc.incoming,
			}
			for _, extra := range tc.extraWorkloads {
				objs = append(objs, extra)
			}
			cl := utiltesting.NewClientBuilder().
				WithObjects(objs...).
				WithLists(&corev1.NodeList{Items: tc.nodes}).
				Build()

			cqCache := schdcache.New(cl)
			cqCache.AddOrUpdateResourceFlavor(log, rfOnDemand)
			cqCache.AddOrUpdateResourceFlavor(log, rfSpot)
			if tc.enableTAS {
				cqCache.AddOrUpdateTopology(log, tasTopology)
				for i := range tc.nodes {
					cqCache.TASCache().SyncNode(&tc.nodes[i])
				}
			}
			if err := cqCache.AddClusterQueue(ctx, cq); err != nil {
				t.Fatalf("adding ClusterQueue to cache: %v", err)
			}

			limitTracker := limits.NewPreemptionLimitTracker()
			limitTracker.Sync(preemptionLimit)
			recordedAgo := tc.recordedAgoByName
			if recordedAgo == nil {
				recordedAgo = map[policy.PreemptionLimitScopeValue]time.Duration{
					"default/v-od": 50 * time.Second,
				}
			}
			for wlKey, ago := range recordedAgo {
				limitTracker.Record(now.Add(-ago), policy.ConsumedLimits{
					"per-workload": policy.NewConsumedLimit(preemptionLimit, wlKey),
				})
			}

			qManager := qcache.NewManagerForUnitTests(cl, cqCache)
			sched := New(
				qManager, cqCache, cl, &utiltesting.EventRecorder{},
				WithClock(t, testingclock.NewFakeClock(now)),
				WithPreemptionExpectations(preemptexpectations.New()),
				WithPreemptionLimitTracker(limitTracker),
			)
			sched.preemptor.SnapshotPreemptionLimits()
			snap, err := cqCache.Snapshot(ctx)
			if err != nil {
				t.Fatalf("Snapshot: %v", err)
			}

			wlInfo := workload.NewInfo(log, tc.incoming)
			wlInfo.ClusterQueue = "limits-cq"
			gotAssignment, _, gotRetryAt, gotBlocked := sched.getAssignments(ctx, wlInfo, snap)
			if gotAssignment.RepresentativeMode() != tc.wantMode {
				t.Errorf("getAssignments mode = %v, want %v", gotAssignment.RepresentativeMode(), tc.wantMode)
			}
			if diff := cmp.Diff(tc.wantRetryAt, gotRetryAt); diff != "" {
				t.Errorf("getAssignments retryAt (-want,+got):\n%s", diff)
			}
			if diff := cmp.Diff(tc.wantBlocked, gotBlocked); diff != "" {
				t.Errorf("getAssignments blockedLimit (-want,+got):\n%s", diff)
			}
			if leakedRetry := sched.preemptor.TakeRetryAt(); leakedRetry != nil {
				t.Errorf("preemptor.TakeRetryAt() = %v after getAssignments, want nil", leakedRetry)
			}
			if leakedBlocked := sched.preemptor.TakeBlockedLimit(); leakedBlocked != nil {
				t.Errorf("preemptor.TakeBlockedLimit() = %v after getAssignments, want nil", leakedBlocked)
			}
		})
	}
}

func TestSchedulePreemptionRetry(t *testing.T) {
	now := time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)
	fakeClock := testingclock.NewFakeClock(now)
	ignoreEventMessageCmpOpts := cmp.Options{cmpopts.IgnoreFields(utiltesting.EventRecord{}, "Message")}

	resourceFlavors := []*kueue.ResourceFlavor{
		utiltestingapi.MakeResourceFlavor("on-demand").Obj(),
		utiltestingapi.MakeResourceFlavor("spot").Obj(),
	}
	preemptionCfg := utiltestingalpha.MakePreemptionConfig("limits-cfg").
		Rule("within-cq", kueuealpha.InsufficientQuota,
			utiltestingalpha.MakeCandidateSelector(kueuealpha.WithinClusterQueue).Obj(),
		).
		Obj()
	tasPreemptionCfg := utiltestingalpha.MakePreemptionConfig("tas-limits-cfg").
		Rule("within-cq-topology", kueuealpha.QuotaFeasibleAndInsufficientTopology,
			utiltestingalpha.MakeCandidateSelector(kueuealpha.WithinClusterQueue).Obj(),
		).
		Obj()
	perWorkloadLimit := utiltestingalpha.MakePreemptionLimit("per-workload", kueuealpha.PreemptedWorkloadLimitScope).
		Limit(1).LimitWindowSeconds(60).Obj()
	zeroLimit := utiltestingalpha.MakePreemptionLimit("zero-limit", kueuealpha.GlobalPreemptionLimitScope).
		Limit(0).LimitWindowSeconds(60).Obj()
	perWorkloadConsumed := func(wl policy.PreemptionLimitScopeValue) policy.ConsumedLimits {
		return policy.ConsumedLimits{
			"per-workload": policy.NewConsumedLimit(perWorkloadLimit, wl),
		}
	}
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
	tasAdmittedWorkload := func(name, node string) kueue.Workload {
		return *utiltestingapi.MakeWorkload(name, "default").
			UID(types.UID(name)).
			Queue("tas-lq").
			PodSets(*utiltestingapi.MakePodSet(kueue.DefaultPodSetName, 1).
				Request(corev1.ResourceCPU, "1").
				PreferredTopologyRequest(corev1.LabelHostname).
				Obj()).
			ReserveQuotaAt(
				utiltestingapi.MakeAdmission("tas-cq").
					PodSets(utiltestingapi.MakePodSetAssignment(kueue.DefaultPodSetName).
						Assignment(corev1.ResourceCPU, "tas-flavor", "1").
						TopologyAssignment(utiltestingapi.MakeTopologyAssignment([]string{corev1.LabelHostname}).
							Domain(utiltas.TopologyDomainAssignment{Count: 1, Values: []string{node}}).
							Obj()).
						Obj()).
					Obj(),
				now,
			).
			AdmittedAt(true, now).
			Obj()
	}

	cases := map[string]scheduleTestCase{
		"standalone ClusterQueue requeues workload after earliest PreemptionLimit expiry across flavors": {
			featureGates: map[featuregate.Feature]bool{
				features.ConfigurablePreemptions:      true,
				features.ConfigurablePreemptionLimits: true,
			},
			additionalClusterQueues: []kueue.ClusterQueue{
				*utiltestingapi.MakeClusterQueue("limits-cq").
					Annotation(kueuealpha.PreemptionConfigNameAnnotation, "limits-cfg").
					ResourceGroup(
						*utiltestingapi.MakeFlavorQuotas("on-demand").Resource(corev1.ResourceCPU, "1").Obj(),
						*utiltestingapi.MakeFlavorQuotas("spot").Resource(corev1.ResourceCPU, "1").Obj(),
					).
					Obj(),
			},
			additionalLocalQueues: []kueue.LocalQueue{
				*utiltestingapi.MakeLocalQueue("limits-lq", "default").ClusterQueue("limits-cq").Obj(),
			},
			objects: []client.Object{
				preemptionCfg,
				perWorkloadLimit.DeepCopy(),
			},
			recordedPreemptions: []recordedPreemption{
				{at: now.Add(-35 * time.Second), consumed: perWorkloadConsumed("default/v-od")},
				{at: now.Add(-50 * time.Second), consumed: perWorkloadConsumed("default/v-spot")},
			},
			workloads: []kueue.Workload{
				*utiltestingapi.MakeWorkload("v-od", "default").
					Queue("limits-lq").
					Request(corev1.ResourceCPU, "1").
					SimpleReserveQuota("limits-cq", "on-demand", now).
					AdmittedAt(true, now).
					Obj(),
				*utiltestingapi.MakeWorkload("v-spot", "default").
					Queue("limits-lq").
					Request(corev1.ResourceCPU, "1").
					SimpleReserveQuota("limits-cq", "spot", now).
					AdmittedAt(true, now).
					Obj(),
				*utiltestingapi.MakeWorkload("incoming", "default").
					Queue("limits-lq").
					Request(corev1.ResourceCPU, "1").
					Creation(now).
					Obj(),
			},
			wantWorkloads: []kueue.Workload{
				*utiltestingapi.MakeWorkload("incoming", "default").
					Queue("limits-lq").
					Request(corev1.ResourceCPU, "1").
					Creation(now).
					Condition(metav1.Condition{
						Type:               kueue.WorkloadQuotaReserved,
						Status:             metav1.ConditionFalse,
						Reason:             kueue.WorkloadQuotaReservedReasonWaitingForQuota,
						Message:            "couldn't assign flavors to pod set main: insufficient unused quota for cpu in flavor on-demand, 1 more needed, insufficient unused quota for cpu in flavor spot, 1 more needed; preemptions blocked by PreemptionLimit per-workload (aggregation PreemptedWorkload) until 2026-01-01T00:00:10Z",
						LastTransitionTime: metav1.NewTime(now),
					}).
					Condition(metav1.Condition{
						Type:               kueue.WorkloadAdmitted,
						Status:             metav1.ConditionFalse,
						Reason:             kueue.WorkloadAdmittedReasonNoReservation,
						Message:            "The workload has no reservation",
						LastTransitionTime: metav1.NewTime(now),
					}).
					ResourceRequests(kueue.PodSetRequest{
						Name:      kueue.DefaultPodSetName,
						Resources: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1")},
					}).
					Obj(),
				*utiltestingapi.MakeWorkload("v-od", "default").
					Queue("limits-lq").
					Request(corev1.ResourceCPU, "1").
					SimpleReserveQuota("limits-cq", "on-demand", now).
					AdmittedAt(true, now).
					Obj(),
				*utiltestingapi.MakeWorkload("v-spot", "default").
					Queue("limits-lq").
					Request(corev1.ResourceCPU, "1").
					SimpleReserveQuota("limits-cq", "spot", now).
					AdmittedAt(true, now).
					Obj(),
			},
			wantAssignments: map[workload.Reference]kueue.Admission{
				"default/v-od": *utiltestingapi.MakeAdmission("limits-cq").
					PodSets(utiltestingapi.MakePodSetAssignment(kueue.DefaultPodSetName).
						Assignment(corev1.ResourceCPU, "on-demand", "1").Obj()).
					Obj(),
				"default/v-spot": *utiltestingapi.MakeAdmission("limits-cq").
					PodSets(utiltestingapi.MakePodSetAssignment(kueue.DefaultPodSetName).
						Assignment(corev1.ResourceCPU, "spot", "1").Obj()).
					Obj(),
			},
			wantInadmissibleLeft: map[kueue.ClusterQueueReference][]workload.Reference{
				"limits-cq": {"default/incoming"},
			},
			wantClusterQueueRequeueDelays: map[kueue.ClusterQueueReference]time.Duration{
				"limits-cq": 10 * time.Second,
			},
			wantEvents: []utiltesting.EventRecord{
				{
					Key:       client.ObjectKey{Namespace: "default", Name: "incoming"},
					Reason:    kueue.WorkloadQuotaReservedReasonWaitingForQuota,
					EventType: corev1.EventTypeWarning,
					Message:   "couldn't assign flavors to pod set main: insufficient unused quota for cpu in flavor on-demand, 1 more needed, insufficient unused quota for cpu in flavor spot, 1 more needed; preemptions blocked by PreemptionLimit per-workload (aggregation PreemptedWorkload) until 2026-01-01T00:00:10Z",
				},
			},
		},
		"reduced count of partially admissible workload arms PreemptionLimit retry delay": {
			featureGates: map[featuregate.Feature]bool{
				features.PartialAdmission:             true,
				features.ConfigurablePreemptions:      true,
				features.ConfigurablePreemptionLimits: true,
			},
			additionalClusterQueues: []kueue.ClusterQueue{
				*utiltestingapi.MakeClusterQueue("limits-cq").
					Annotation(kueuealpha.PreemptionConfigNameAnnotation, "limits-cfg").
					ResourceGroup(
						*utiltestingapi.MakeFlavorQuotas("on-demand").Resource(corev1.ResourceCPU, "2").Obj(),
					).
					Obj(),
			},
			additionalLocalQueues: []kueue.LocalQueue{
				*utiltestingapi.MakeLocalQueue("limits-lq", "default").ClusterQueue("limits-cq").Obj(),
			},
			objects: []client.Object{
				preemptionCfg,
				perWorkloadLimit.DeepCopy(),
			},
			recordedPreemptions: []recordedPreemption{
				{at: now.Add(-50 * time.Second), consumed: perWorkloadConsumed("default/v-od")},
			},
			workloads: []kueue.Workload{
				*utiltestingapi.MakeWorkload("v-od", "default").
					Queue("limits-lq").
					Request(corev1.ResourceCPU, "2").
					SimpleReserveQuota("limits-cq", "on-demand", now).
					AdmittedAt(true, now).
					Obj(),
				*utiltestingapi.MakeWorkload("incoming", "default").
					Queue("limits-lq").
					PodSets(
						*utiltestingapi.MakePodSet(kueue.DefaultPodSetName, 4).
							SetMinimumCount(1).
							Request(corev1.ResourceCPU, "1").
							Obj(),
					).
					Creation(now).
					Obj(),
			},
			wantWorkloads: []kueue.Workload{
				*utiltestingapi.MakeWorkload("incoming", "default").
					Queue("limits-lq").
					PodSets(
						*utiltestingapi.MakePodSet(kueue.DefaultPodSetName, 4).
							SetMinimumCount(1).
							Request(corev1.ResourceCPU, "1").
							Obj(),
					).
					Creation(now).
					Condition(metav1.Condition{
						Type:               kueue.WorkloadQuotaReserved,
						Status:             metav1.ConditionFalse,
						Reason:             kueue.WorkloadQuotaReservedReasonExceedsMaxQuota,
						Message:            "couldn't assign flavors to pod set main: insufficient quota for cpu in flavor on-demand, previously considered podsets requests (0) + current podset request (4) > maximum capacity (2); preemptions blocked by PreemptionLimit per-workload (aggregation PreemptedWorkload) until 2026-01-01T00:00:10Z",
						LastTransitionTime: metav1.NewTime(now),
					}).
					Condition(metav1.Condition{
						Type:               kueue.WorkloadAdmitted,
						Status:             metav1.ConditionFalse,
						Reason:             kueue.WorkloadAdmittedReasonNoReservation,
						Message:            "The workload has no reservation",
						LastTransitionTime: metav1.NewTime(now),
					}).
					ResourceRequests(kueue.PodSetRequest{
						Name:      kueue.DefaultPodSetName,
						Resources: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("4")},
					}).
					Obj(),
				*utiltestingapi.MakeWorkload("v-od", "default").
					Queue("limits-lq").
					Request(corev1.ResourceCPU, "2").
					SimpleReserveQuota("limits-cq", "on-demand", now).
					AdmittedAt(true, now).
					Obj(),
			},
			wantAssignments: map[workload.Reference]kueue.Admission{
				"default/v-od": *utiltestingapi.MakeAdmission("limits-cq").
					PodSets(utiltestingapi.MakePodSetAssignment(kueue.DefaultPodSetName).
						Assignment(corev1.ResourceCPU, "on-demand", "2").Obj()).
					Obj(),
			},
			wantInadmissibleLeft: map[kueue.ClusterQueueReference][]workload.Reference{
				"limits-cq": {"default/incoming"},
			},
			wantClusterQueueRequeueDelays: map[kueue.ClusterQueueReference]time.Duration{
				"limits-cq": 10 * time.Second,
			},
			eventCmpOpts: ignoreEventMessageCmpOpts,
			wantEvents: []utiltesting.EventRecord{
				utiltesting.MakeEventRecord("default", "incoming", kueue.WorkloadQuotaReservedReasonExceedsMaxQuota, corev1.EventTypeWarning).Obj(),
			},
		},
		"PreemptionLimit with limit of 0 omits until timestamp and arms no requeue delay": {
			featureGates: map[featuregate.Feature]bool{
				features.ConfigurablePreemptions:      true,
				features.ConfigurablePreemptionLimits: true,
			},
			additionalClusterQueues: []kueue.ClusterQueue{
				*utiltestingapi.MakeClusterQueue("limits-cq").
					Annotation(kueuealpha.PreemptionConfigNameAnnotation, "limits-cfg").
					ResourceGroup(
						*utiltestingapi.MakeFlavorQuotas("on-demand").Resource(corev1.ResourceCPU, "1").Obj(),
					).
					Obj(),
			},
			additionalLocalQueues: []kueue.LocalQueue{
				*utiltestingapi.MakeLocalQueue("limits-lq", "default").ClusterQueue("limits-cq").Obj(),
			},
			objects: []client.Object{
				preemptionCfg,
				zeroLimit.DeepCopy(),
			},
			workloads: []kueue.Workload{
				*utiltestingapi.MakeWorkload("v-od", "default").
					Queue("limits-lq").
					Request(corev1.ResourceCPU, "1").
					SimpleReserveQuota("limits-cq", "on-demand", now).
					AdmittedAt(true, now).
					Obj(),
				*utiltestingapi.MakeWorkload("incoming", "default").
					Queue("limits-lq").
					Request(corev1.ResourceCPU, "1").
					Creation(now).
					Obj(),
			},
			wantWorkloads: []kueue.Workload{
				*utiltestingapi.MakeWorkload("incoming", "default").
					Queue("limits-lq").
					Request(corev1.ResourceCPU, "1").
					Creation(now).
					Condition(metav1.Condition{
						Type:               kueue.WorkloadQuotaReserved,
						Status:             metav1.ConditionFalse,
						Reason:             kueue.WorkloadQuotaReservedReasonWaitingForQuota,
						Message:            "couldn't assign flavors to pod set main: insufficient unused quota for cpu in flavor on-demand, 1 more needed; preemptions blocked by PreemptionLimit zero-limit (aggregation Global)",
						LastTransitionTime: metav1.NewTime(now),
					}).
					Condition(metav1.Condition{
						Type:               kueue.WorkloadAdmitted,
						Status:             metav1.ConditionFalse,
						Reason:             kueue.WorkloadAdmittedReasonNoReservation,
						Message:            "The workload has no reservation",
						LastTransitionTime: metav1.NewTime(now),
					}).
					ResourceRequests(kueue.PodSetRequest{
						Name:      kueue.DefaultPodSetName,
						Resources: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1")},
					}).
					Obj(),
				*utiltestingapi.MakeWorkload("v-od", "default").
					Queue("limits-lq").
					Request(corev1.ResourceCPU, "1").
					SimpleReserveQuota("limits-cq", "on-demand", now).
					AdmittedAt(true, now).
					Obj(),
			},
			wantAssignments: map[workload.Reference]kueue.Admission{
				"default/v-od": *utiltestingapi.MakeAdmission("limits-cq").
					PodSets(utiltestingapi.MakePodSetAssignment(kueue.DefaultPodSetName).
						Assignment(corev1.ResourceCPU, "on-demand", "1").Obj()).
					Obj(),
			},
			wantInadmissibleLeft: map[kueue.ClusterQueueReference][]workload.Reference{
				"limits-cq": {"default/incoming"},
			},
			wantEvents: []utiltesting.EventRecord{
				{
					Key:       client.ObjectKey{Namespace: "default", Name: "incoming"},
					Reason:    kueue.WorkloadQuotaReservedReasonWaitingForQuota,
					EventType: corev1.EventTypeWarning,
					Message:   "couldn't assign flavors to pod set main: insufficient unused quota for cpu in flavor on-demand, 1 more needed; preemptions blocked by PreemptionLimit zero-limit (aggregation Global)",
				},
			},
		},
		"TAS QuotaFeasibleAndInsufficientTopology preemption blocked by PreemptionLimit extends QuotaReserved message": {
			featureGates: map[featuregate.Feature]bool{
				features.TopologyAwareScheduling:      true,
				features.ConfigurablePreemptions:      true,
				features.ConfigurablePreemptionLimits: true,
			},
			additionalResourceFlavors: []kueue.ResourceFlavor{
				*utiltestingapi.MakeResourceFlavor("tas-flavor").
					NodeLabel("tas-node", "true").
					TopologyName("tas-topology").
					Obj(),
			},
			topologies: []kueue.Topology{
				*utiltestingapi.MakeDefaultOneLevelTopology("tas-topology"),
			},
			nodes: []corev1.Node{tasNode("x1"), tasNode("x2")},
			additionalClusterQueues: []kueue.ClusterQueue{
				*utiltestingapi.MakeClusterQueue("tas-cq").
					Annotation(kueuealpha.PreemptionConfigNameAnnotation, "tas-limits-cfg").
					ResourceGroup(
						*utiltestingapi.MakeFlavorQuotas("tas-flavor").Resource(corev1.ResourceCPU, "4").Obj(),
					).
					Obj(),
			},
			additionalLocalQueues: []kueue.LocalQueue{
				*utiltestingapi.MakeLocalQueue("tas-lq", "default").ClusterQueue("tas-cq").Obj(),
			},
			objects: []client.Object{
				tasPreemptionCfg,
				perWorkloadLimit.DeepCopy(),
			},
			recordedPreemptions: []recordedPreemption{
				{at: now.Add(-35 * time.Second), consumed: perWorkloadConsumed("default/v1")},
				{at: now.Add(-50 * time.Second), consumed: perWorkloadConsumed("default/v2")},
			},
			workloads: []kueue.Workload{
				tasAdmittedWorkload("v1", "x1"),
				tasAdmittedWorkload("v2", "x2"),
				*utiltestingapi.MakeWorkload("incoming", "default").
					Queue("tas-lq").
					PodSets(*utiltestingapi.MakePodSet(kueue.DefaultPodSetName, 2).
						Request(corev1.ResourceCPU, "1").
						RequiredTopologyRequest(corev1.LabelHostname).
						Obj()).
					Creation(now).
					Obj(),
			},
			wantWorkloads: []kueue.Workload{
				*utiltestingapi.MakeWorkload("incoming", "default").
					Queue("tas-lq").
					PodSets(*utiltestingapi.MakePodSet(kueue.DefaultPodSetName, 2).
						Request(corev1.ResourceCPU, "1").
						RequiredTopologyRequest(corev1.LabelHostname).
						Obj()).
					Creation(now).
					Condition(metav1.Condition{
						Type:               kueue.WorkloadQuotaReserved,
						Status:             metav1.ConditionFalse,
						Reason:             kueue.WorkloadQuotaReservedReasonWaitingForQuota,
						Message:            `couldn't assign flavors to pod set main: topology "tas-topology" allows to fit only 1 out of 2 pod(s); preemptions blocked by PreemptionLimit per-workload (aggregation PreemptedWorkload) until 2026-01-01T00:00:10Z`,
						LastTransitionTime: metav1.NewTime(now),
					}).
					Condition(metav1.Condition{
						Type:               kueue.WorkloadAdmitted,
						Status:             metav1.ConditionFalse,
						Reason:             kueue.WorkloadAdmittedReasonNoReservation,
						Message:            "The workload has no reservation",
						LastTransitionTime: metav1.NewTime(now),
					}).
					ResourceRequests(kueue.PodSetRequest{
						Name:      kueue.DefaultPodSetName,
						Resources: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("2")},
					}).
					Obj(),
				tasAdmittedWorkload("v1", "x1"),
				tasAdmittedWorkload("v2", "x2"),
			},
			wantAssignments: map[workload.Reference]kueue.Admission{
				"default/v1": *tasAdmittedWorkload("v1", "x1").Status.Admission,
				"default/v2": *tasAdmittedWorkload("v2", "x2").Status.Admission,
			},
			wantInadmissibleLeft: map[kueue.ClusterQueueReference][]workload.Reference{
				"tas-cq": {"default/incoming"},
			},
			wantClusterQueueRequeueDelays: map[kueue.ClusterQueueReference]time.Duration{
				"tas-cq": 10 * time.Second,
			},
			eventCmpOpts: ignoreEventMessageCmpOpts,
			wantEvents: []utiltesting.EventRecord{
				utiltesting.MakeEventRecord("default", "incoming", kueue.WorkloadQuotaReservedReasonWaitingForQuota, corev1.EventTypeWarning).Obj(),
			},
		},
		"ConfigurablePreemptionLimits disabled ignores PreemptionLimit and preempts normally": {
			featureGates: map[featuregate.Feature]bool{
				features.ConfigurablePreemptions:      true,
				features.ConfigurablePreemptionLimits: false,
			},
			additionalClusterQueues: []kueue.ClusterQueue{
				*utiltestingapi.MakeClusterQueue("limits-cq").
					Annotation(kueuealpha.PreemptionConfigNameAnnotation, "limits-cfg").
					ResourceGroup(
						*utiltestingapi.MakeFlavorQuotas("on-demand").Resource(corev1.ResourceCPU, "1").Obj(),
					).
					Obj(),
			},
			additionalLocalQueues: []kueue.LocalQueue{
				*utiltestingapi.MakeLocalQueue("limits-lq", "default").ClusterQueue("limits-cq").Obj(),
			},
			objects: []client.Object{
				preemptionCfg,
				zeroLimit.DeepCopy(),
			},
			workloads: []kueue.Workload{
				*utiltestingapi.MakeWorkload("v-od", "default").
					Queue("limits-lq").
					Request(corev1.ResourceCPU, "1").
					SimpleReserveQuota("limits-cq", "on-demand", now).
					AdmittedAt(true, now).
					Obj(),
				*utiltestingapi.MakeWorkload("incoming", "default").
					Queue("limits-lq").
					Request(corev1.ResourceCPU, "1").
					Creation(now).
					Obj(),
			},
			wantWorkloads: []kueue.Workload{
				*utiltestingapi.MakeWorkload("incoming", "default").
					Queue("limits-lq").
					Request(corev1.ResourceCPU, "1").
					Creation(now).
					Condition(metav1.Condition{
						Type:               kueue.WorkloadQuotaReserved,
						Status:             metav1.ConditionFalse,
						Reason:             kueue.WorkloadQuotaReservedReasonWaitingForPreemptedWorkloads,
						Message:            "couldn't assign flavors to pod set main: insufficient unused quota for cpu in flavor on-demand, 1 more needed. Pending the preemption of 1 workload(s)",
						LastTransitionTime: metav1.NewTime(now),
					}).
					Condition(metav1.Condition{
						Type:               kueue.WorkloadAdmitted,
						Status:             metav1.ConditionFalse,
						Reason:             kueue.WorkloadAdmittedReasonNoReservation,
						Message:            "The workload has no reservation",
						LastTransitionTime: metav1.NewTime(now),
					}).
					ResourceRequests(kueue.PodSetRequest{
						Name:      kueue.DefaultPodSetName,
						Resources: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1")},
					}).
					Obj(),
				*utiltestingapi.MakeWorkload("v-od", "default").
					Queue("limits-lq").
					Request(corev1.ResourceCPU, "1").
					SimpleReserveQuota("limits-cq", "on-demand", now).
					AdmittedAt(true, now).
					Condition(metav1.Condition{
						Type:               kueue.WorkloadEvicted,
						Status:             metav1.ConditionTrue,
						Reason:             kueue.WorkloadEvictedByPreemption,
						Message:            "Preempted by default/incoming because of preemption config limits-cfg rule within-cq/0",
						LastTransitionTime: metav1.NewTime(now),
					}).
					Condition(metav1.Condition{
						Type:               kueue.WorkloadPreempted,
						Status:             metav1.ConditionTrue,
						Reason:             kueue.ConfigurablePreemptionReason,
						Message:            "Preempted by default/incoming because of preemption config limits-cfg rule within-cq/0",
						LastTransitionTime: metav1.NewTime(now),
					}).
					SchedulingStatsEviction(kueue.WorkloadSchedulingStatsEviction{
						Reason:          kueue.WorkloadEvictedByPreemption,
						UnderlyingCause: "limits-cfg",
						Count:           1,
					}).
					Obj(),
			},
			wantAssignments: map[workload.Reference]kueue.Admission{
				"default/v-od": *utiltestingapi.MakeAdmission("limits-cq").
					PodSets(utiltestingapi.MakePodSetAssignment(kueue.DefaultPodSetName).
						Assignment(corev1.ResourceCPU, "on-demand", "1").Obj()).
					Obj(),
			},
			wantLeft: map[kueue.ClusterQueueReference][]workload.Reference{
				"limits-cq": {"default/incoming"},
			},
		},
	}

	runScheduleTestCases(t, scheduleTestConfig{
		resourceFlavors: resourceFlavors,
		fakeClock:       fakeClock,
	}, cases)
}
