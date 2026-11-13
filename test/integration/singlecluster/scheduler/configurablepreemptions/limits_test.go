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

package configurablepreemptions

import (
	"slices"
	"time"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kueuealpha "sigs.k8s.io/kueue/apis/kueue/v1alpha1"
	kueue "sigs.k8s.io/kueue/apis/kueue/v1beta2"
	"sigs.k8s.io/kueue/pkg/features"
	"sigs.k8s.io/kueue/pkg/scheduler/preemption/policy"
	kueuetestalpha1 "sigs.k8s.io/kueue/pkg/util/testing/v1alpha1"
	utiltestingapi "sigs.k8s.io/kueue/pkg/util/testing/v1beta2"
	"sigs.k8s.io/kueue/test/util/behavioral"
	"sigs.k8s.io/kueue/test/util/behavioral/integration"
)

var _ = ginkgo.Describe("PreemptionLimits", ginkgo.Label("feature:configurablepreemptions"), func() {
	var (
		ns     *corev1.Namespace
		flavor *kueue.ResourceFlavor
	)

	createWorkload := func(name, queue string, priority int32, cpu string) *kueue.Workload {
		ginkgo.GinkgoHelper()
		wl := utiltestingapi.MakeWorkload(name, ns.Name).
			Queue(kueue.LocalQueueName(queue)).
			Priority(priority).
			Request(corev1.ResourceCPU, cpu).
			Obj()
		behavioral.MustCreate(ctx, k8sClient, wl)
		return wl
	}

	createPreemptionLimitAndWaitForSync := func(limit *kueuealpha.PreemptionLimit) {
		ginkgo.GinkgoHelper()
		behavioral.MustCreate(ctx, k8sClient, limit)
		gomega.Eventually(func(g gomega.Gomega) {
			var got kueuealpha.PreemptionLimit
			g.Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(limit), &got)).To(gomega.Succeed())
			g.Expect(got.Status.LastUpdateTime).NotTo(gomega.BeNil())
		}, behavioral.Timeout, behavioral.ShortInterval).Should(gomega.Succeed())
	}

	updatePreemptionLimit := func(limit *kueuealpha.PreemptionLimit, mutate func(*kueuealpha.PreemptionLimit)) {
		ginkgo.GinkgoHelper()
		gomega.Eventually(func(g gomega.Gomega) {
			g.Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(limit), limit)).To(gomega.Succeed())
			mutate(limit)
			g.Expect(k8sClient.Update(ctx, limit)).To(gomega.Succeed())
		}, behavioral.Timeout, behavioral.ShortInterval).Should(gomega.Succeed())
	}

	waitForLimitAllowance := func(limitName string, scopeValue policy.PreemptionLimitScopeValue, count int) {
		ginkgo.GinkgoHelper()
		probe := slices.Repeat([]policy.ConsumedLimits{
			{policy.PreemptionLimitReference(limitName): {ScopeValue: scopeValue}},
		}, count)
		gomega.Eventually(func() bool {
			return preemptionLimitTracker.Snapshot(time.Now()).WithinLimits(probe)
		}, behavioral.Timeout, behavioral.ShortInterval).Should(gomega.BeTrue())
	}

	// recreateBlockedPreemptor recreates a pending preemptor that was parked in inadmissibleWorkloads
	// while a PreemptionLimit blocked preemption. Once PreemptionLimit reconciliation requeues
	// inadmissible workloads on spec updates and window expiration, callers can drop this call and
	// assert directly that the existing pending workload gets admitted.
	recreateBlockedPreemptor := func(wl *kueue.Workload) *kueue.Workload {
		ginkgo.GinkgoHelper()
		replacement := utiltestingapi.MakeWorkload(wl.Name, wl.Namespace).
			Queue(wl.Spec.QueueName).
			Priority(*wl.Spec.Priority).
			PodSets(*wl.Spec.PodSets[0].DeepCopy()).
			Obj()
		behavioral.ExpectObjectToBeDeleted(ctx, k8sClient, wl, true)
		behavioral.MustCreate(ctx, k8sClient, replacement)
		return replacement
	}

	expectWorkloadsNotEvictedConsistently := func(wls ...*kueue.Workload) {
		ginkgo.GinkgoHelper()
		gomega.Consistently(func(g gomega.Gomega) {
			for _, wl := range wls {
				var got kueue.Workload
				g.Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(wl), &got)).To(gomega.Succeed())
				g.Expect(got.Status.Admission).NotTo(gomega.BeNil())
				g.Expect(meta.IsStatusConditionTrue(got.Status.Conditions, kueue.WorkloadEvicted)).To(gomega.BeFalse())
			}
		}, behavioral.ConsistentDuration, behavioral.ShortInterval).Should(gomega.Succeed())
	}

	expectPreemptionLimitCounts := func(limit *kueuealpha.PreemptionLimit, want []kueuealpha.PreemptionLimitCount) {
		ginkgo.GinkgoHelper()
		gomega.Eventually(func(g gomega.Gomega) {
			var got kueuealpha.PreemptionLimit
			g.Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(limit), &got)).To(gomega.Succeed())
			g.Expect(got.Status.LastUpdateTime).NotTo(gomega.BeNil())
			if len(want) == 0 {
				g.Expect(got.Status.Counts).To(gomega.BeEmpty())
			} else {
				g.Expect(got.Status.Counts).To(gomega.Equal(want))
			}
		}, behavioral.Timeout, behavioral.ShortInterval).Should(gomega.Succeed())
	}

	ginkgo.Context("when ConfigurablePreemptionLimits feature gate is disabled", func() {
		var (
			config *kueuealpha.PreemptionConfig
			limit  *kueuealpha.PreemptionLimit
			cq     *kueue.ClusterQueue
			lq     *kueue.LocalQueue
		)

		ginkgo.BeforeEach(func() {
			startManager()

			ns = behavioral.CreateNamespaceFromPrefixWithLog(ctx, k8sClient, "preemptionlimits-disabled-")
			flavor = utiltestingapi.MakeResourceFlavor("rf-preemptionlimits-disabled").Obj()
			behavioral.MustCreate(ctx, k8sClient, flavor)

			config = kueuetestalpha1.MakePreemptionConfig("disabled-gate-config").
				Rule("within-cq", kueuealpha.Always,
					kueuetestalpha1.MakeCandidateSelector(kueuealpha.WithinClusterQueue).
						Priority(kueuealpha.Boosted, kueuealpha.LessThan).
						Obj(),
				).Obj()
			behavioral.MustCreate(ctx, k8sClient, config)

			cq = utiltestingapi.MakeClusterQueue("cq-limits-disabled").
				ResourceGroup(*utiltestingapi.MakeFlavorQuotas(flavor.Name).
					Resource(corev1.ResourceCPU, "1").Obj()).
				Annotation(kueuealpha.PreemptionConfigNameAnnotation, config.Name).
				Obj()
			behavioral.CreateClusterQueuesAndWaitForActive(ctx, k8sClient, cq)

			lq = utiltestingapi.MakeLocalQueue("lq-limits-disabled", ns.Name).ClusterQueue(cq.Name).Obj()
			behavioral.CreateLocalQueuesAndWaitForActive(ctx, k8sClient, lq)

			limit = kueuetestalpha1.MakePreemptionLimit("ignored-zero-limit", kueuealpha.GlobalPreemptionLimitScope).
				Limit(0).
				LimitWindowSeconds(60).
				Obj()
			behavioral.MustCreate(ctx, k8sClient, limit)
		})

		ginkgo.AfterEach(func() {
			gomega.Expect(behavioral.DeleteWorkloadsInNamespace(ctx, k8sClient, ns)).To(gomega.Succeed())
			behavioral.ExpectObjectToBeDeleted(ctx, k8sClient, lq, true)
			behavioral.ExpectObjectToBeDeleted(ctx, k8sClient, cq, true)
			behavioral.ExpectObjectToBeDeleted(ctx, k8sClient, limit, true)
			behavioral.ExpectObjectToBeDeleted(ctx, k8sClient, config, true)
			gomega.Expect(behavioral.DeleteNamespace(ctx, k8sClient, ns)).To(gomega.Succeed())
			behavioral.ExpectObjectToBeDeleted(ctx, k8sClient, flavor, true)
		})

		ginkgo.It("should ignore PreemptionLimit objects and neither enforce limits nor update status", func() {
			lowWl := createWorkload("low-wl", lq.Name, 10, "1")
			behavioral.ExpectWorkloadsToBeAdmitted(ctx, k8sClient, lowWl)

			highWl := createWorkload("high-wl", lq.Name, 100, "1")
			behavioral.FinishEvictionForWorkloads(ctx, k8sClient, lowWl)
			behavioral.ExpectWorkloadsToBeAdmitted(ctx, k8sClient, highWl)
			behavioral.ExpectWorkloadsToBePending(ctx, k8sClient, lowWl)

			gomega.Consistently(func(g gomega.Gomega) {
				var got kueuealpha.PreemptionLimit
				g.Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(limit), &got)).To(gomega.Succeed())
				g.Expect(got.Status.LastUpdateTime).To(gomega.BeNil())
				g.Expect(got.Status.Counts).To(gomega.BeEmpty())
			}, behavioral.ConsistentDuration, behavioral.ShortInterval).Should(gomega.Succeed())
		})
	})

	ginkgo.Context("when ConfigurablePreemptionLimits feature gate is enabled", func() {
		ginkgo.BeforeEach(func() {
			features.SetFeatureGateDuringTest(ginkgo.GinkgoTB(), features.ConfigurablePreemptionLimits, true)
			startManager()

			ns = behavioral.CreateNamespaceFromPrefixWithLog(ctx, k8sClient, "preemptionlimits-")
			flavor = utiltestingapi.MakeResourceFlavor("rf-preemptionlimits").Obj()
			behavioral.MustCreate(ctx, k8sClient, flavor)
		})

		ginkgo.AfterEach(func() {
			gomega.Expect(behavioral.DeleteNamespace(ctx, k8sClient, ns)).To(gomega.Succeed())
			behavioral.ExpectObjectToBeDeleted(ctx, k8sClient, flavor, true)
		})

		ginkgo.When("rolling out a PreemptionConfig safely", func() {
			var (
				config *kueuealpha.PreemptionConfig
				limit  *kueuealpha.PreemptionLimit
				cq     *kueue.ClusterQueue
				lq     *kueue.LocalQueue
			)

			ginkgo.BeforeEach(func() {
				config = kueuetestalpha1.MakePreemptionConfig("canary-rollout-config").
					Label("rollout", "canary").
					Rule("within-cq", kueuealpha.Always,
						kueuetestalpha1.MakeCandidateSelector(kueuealpha.WithinClusterQueue).
							Priority(kueuealpha.Boosted, kueuealpha.LessThan).
							Obj(),
					).Obj()
				behavioral.MustCreate(ctx, k8sClient, config)

				cq = utiltestingapi.MakeClusterQueue("cq-rollout").
					ResourceGroup(*utiltestingapi.MakeFlavorQuotas(flavor.Name).
						Resource(corev1.ResourceCPU, "2").Obj()).
					Annotation(kueuealpha.PreemptionConfigNameAnnotation, config.Name).
					Obj()
				behavioral.CreateClusterQueuesAndWaitForActive(ctx, k8sClient, cq)

				lq = utiltestingapi.MakeLocalQueue("lq-rollout", ns.Name).ClusterQueue(cq.Name).Obj()
				behavioral.CreateLocalQueuesAndWaitForActive(ctx, k8sClient, lq)

				limit = kueuetestalpha1.MakePreemptionLimit("canary-rollout-limit", kueuealpha.GlobalPreemptionLimitScope).
					ConfigSelector(&metav1.LabelSelector{MatchLabels: map[string]string{"rollout": "canary"}}).
					Limit(0).
					LimitWindowSeconds(60).
					Obj()
				createPreemptionLimitAndWaitForSync(limit)
			})

			ginkgo.AfterEach(func() {
				gomega.Expect(behavioral.DeleteWorkloadsInNamespace(ctx, k8sClient, ns)).To(gomega.Succeed())
				behavioral.ExpectObjectToBeDeleted(ctx, k8sClient, lq, true)
				behavioral.ExpectObjectToBeDeleted(ctx, k8sClient, cq, true)
				behavioral.ExpectObjectToBeDeleted(ctx, k8sClient, limit, true)
				behavioral.ExpectObjectToBeDeleted(ctx, k8sClient, config, true)
			})

			ginkgo.It("should allow increasing preemptions step by step while preserving recorded counts", func() {
				var lowWl1, lowWl2, highWl1, highWl2 *kueue.Workload

				ginkgo.By("Admitting two low-priority workloads that saturate the ClusterQueue", func() {
					lowWl1 = createWorkload("low-1", lq.Name, 10, "1")
					lowWl2 = createWorkload("low-2", lq.Name, 20, "1")
					behavioral.ExpectWorkloadsToBeAdmitted(ctx, k8sClient, lowWl1, lowWl2)
				})

				ginkgo.By("Blocking all preemptions while limit is 0", func() {
					highWl1 = createWorkload("high-1", lq.Name, 100, "1")
					behavioral.ExpectWorkloadsToBePending(ctx, k8sClient, highWl1)
					expectWorkloadsNotEvictedConsistently(lowWl1, lowWl2)
					expectPreemptionLimitCounts(limit, nil)
				})

				ginkgo.By("Increasing limit to 1 so exactly one preemption proceeds and a second stays blocked", func() {
					updatePreemptionLimit(limit, func(pl *kueuealpha.PreemptionLimit) {
						pl.Spec.Limit = 1
					})
					waitForLimitAllowance(limit.Name, policy.GlobalScopeValue, 1)
					highWl1 = recreateBlockedPreemptor(highWl1)

					behavioral.FinishEvictionForWorkloads(ctx, k8sClient, lowWl1)
					behavioral.ExpectWorkloadsToBeAdmitted(ctx, k8sClient, highWl1)
					behavioral.ExpectWorkloadsToBePending(ctx, k8sClient, lowWl1)
					expectPreemptionLimitCounts(limit, []kueuealpha.PreemptionLimitCount{
						{Name: string(policy.GlobalScopeValue), Value: 1},
					})

					highWl2 = createWorkload("high-2", lq.Name, 100, "1")
					behavioral.ExpectWorkloadsToBePending(ctx, k8sClient, highWl2)
					expectWorkloadsNotEvictedConsistently(lowWl2)
				})

				ginkgo.By("Increasing limit to 2 so one additional preemption proceeds and prior count is preserved", func() {
					updatePreemptionLimit(limit, func(pl *kueuealpha.PreemptionLimit) {
						pl.Spec.Limit = 2
					})
					waitForLimitAllowance(limit.Name, policy.GlobalScopeValue, 1)
					highWl2 = recreateBlockedPreemptor(highWl2)

					behavioral.FinishEvictionForWorkloads(ctx, k8sClient, lowWl2)
					behavioral.ExpectWorkloadsToBeAdmitted(ctx, k8sClient, highWl2)
					behavioral.ExpectWorkloadsToBePending(ctx, k8sClient, lowWl2)
					expectPreemptionLimitCounts(limit, []kueuealpha.PreemptionLimitCount{
						{Name: string(policy.GlobalScopeValue), Value: 2},
					})
				})
			})
		})

		ginkgo.When("protecting a workload from too many disruptions", func() {
			var (
				config *kueuealpha.PreemptionConfig
				limit  *kueuealpha.PreemptionLimit
				cq     *kueue.ClusterQueue
				lq     *kueue.LocalQueue
			)

			ginkgo.BeforeEach(func() {
				config = kueuetestalpha1.MakePreemptionConfig("workload-disruption-config").
					Rule("within-cq", kueuealpha.Always,
						kueuetestalpha1.MakeCandidateSelector(kueuealpha.WithinClusterQueue).
							Priority(kueuealpha.Boosted, kueuealpha.LessThan).
							Obj(),
					).Obj()
				behavioral.MustCreate(ctx, k8sClient, config)

				cq = utiltestingapi.MakeClusterQueue("cq-disruption").
					ResourceGroup(*utiltestingapi.MakeFlavorQuotas(flavor.Name).
						Resource(corev1.ResourceCPU, "2").Obj()).
					Annotation(kueuealpha.PreemptionConfigNameAnnotation, config.Name).
					Obj()
				behavioral.CreateClusterQueuesAndWaitForActive(ctx, k8sClient, cq)

				lq = utiltestingapi.MakeLocalQueue("lq-disruption", ns.Name).ClusterQueue(cq.Name).Obj()
				behavioral.CreateLocalQueuesAndWaitForActive(ctx, k8sClient, lq)

				limit = kueuetestalpha1.MakePreemptionLimit("per-workload-disruption-limit", kueuealpha.PreemptedWorkloadLimitScope).
					Limit(1).
					LimitWindowSeconds(60).
					Obj()
				createPreemptionLimitAndWaitForSync(limit)
			})

			ginkgo.AfterEach(func() {
				gomega.Expect(behavioral.DeleteWorkloadsInNamespace(ctx, k8sClient, ns)).To(gomega.Succeed())
				behavioral.ExpectObjectToBeDeleted(ctx, k8sClient, lq, true)
				behavioral.ExpectObjectToBeDeleted(ctx, k8sClient, cq, true)
				behavioral.ExpectObjectToBeDeleted(ctx, k8sClient, limit, true)
				behavioral.ExpectObjectToBeDeleted(ctx, k8sClient, config, true)
			})

			ginkgo.It("should cap disruptions per workload and redirect subsequent preemptions to other candidates", func() {
				var protectedWl, otherVictimWl, fillerWl, preemptor1, preemptor2, preemptor3 *kueue.Workload

				ginkgo.By("Preempting protectedWl once and requeuing it as pending", func() {
					protectedWl = createWorkload("protected-wl", lq.Name, 10, "1")
					fillerWl = createWorkload("filler-wl", lq.Name, 100, "1")
					behavioral.ExpectWorkloadsToBeAdmitted(ctx, k8sClient, protectedWl, fillerWl)

					preemptor1 = createWorkload("preemptor-1", lq.Name, 100, "1")
					behavioral.FinishEvictionForWorkloads(ctx, k8sClient, protectedWl)
					behavioral.ExpectWorkloadsToBeAdmitted(ctx, k8sClient, preemptor1)
					behavioral.ExpectWorkloadsToBePending(ctx, k8sClient, protectedWl)
					expectPreemptionLimitCounts(limit, []kueuealpha.PreemptionLimitCount{
						{Name: ns.Name + "/" + protectedWl.Name, Value: 1},
					})
				})

				ginkgo.By("Re-admitting protectedWl after quota frees up and redirecting the next preemption to otherVictimWl", func() {
					integration.FinishWorkloads(ctx, k8sClient, preemptor1, fillerWl)
					behavioral.ExpectWorkloadsToBeAdmitted(ctx, k8sClient, protectedWl)

					otherVictimWl = createWorkload("other-victim", lq.Name, 20, "1")
					behavioral.ExpectWorkloadsToBeAdmitted(ctx, k8sClient, otherVictimWl)

					// protectedWl has lower priority (10 < 20) so it would normally be preempted first,
					// but its per-workload disruption limit (1/1) is already exhausted.
					preemptor2 = createWorkload("preemptor-2", lq.Name, 100, "1")
					behavioral.FinishEvictionForWorkloads(ctx, k8sClient, otherVictimWl)
					behavioral.ExpectWorkloadsToBeAdmitted(ctx, k8sClient, preemptor2)
					expectWorkloadsNotEvictedConsistently(protectedWl)
					expectPreemptionLimitCounts(limit, []kueuealpha.PreemptionLimitCount{
						{Name: ns.Name + "/" + otherVictimWl.Name, Value: 1},
						{Name: ns.Name + "/" + protectedWl.Name, Value: 1},
					})
				})

				ginkgo.By("Blocking preemption when protectedWl is the only lower-priority candidate left", func() {
					preemptor3 = createWorkload("preemptor-3", lq.Name, 100, "1")
					behavioral.ExpectWorkloadsToBePending(ctx, k8sClient, preemptor3)
					expectWorkloadsNotEvictedConsistently(protectedWl, preemptor2)
				})
			})
		})

		ginkgo.When("enforcing global preemption rate limiting", func() {
			var (
				configAlpha *kueuealpha.PreemptionConfig
				configBeta  *kueuealpha.PreemptionConfig
				limit       *kueuealpha.PreemptionLimit
				cqAlpha     *kueue.ClusterQueue
				cqBeta      *kueue.ClusterQueue
				lqAlpha     *kueue.LocalQueue
				lqBeta      *kueue.LocalQueue
			)

			ginkgo.BeforeEach(func() {
				withinCQSelector := kueuetestalpha1.MakeCandidateSelector(kueuealpha.WithinClusterQueue).
					Priority(kueuealpha.Boosted, kueuealpha.LessThan).
					Obj()
				configAlpha = kueuetestalpha1.MakePreemptionConfig("global-cfg-alpha").
					Rule("within-cq", kueuealpha.Always, withinCQSelector).
					Obj()
				configBeta = kueuetestalpha1.MakePreemptionConfig("global-cfg-beta").
					Rule("within-cq", kueuealpha.Always, withinCQSelector).
					Obj()
				behavioral.MustCreate(ctx, k8sClient, configAlpha)
				behavioral.MustCreate(ctx, k8sClient, configBeta)

				cqAlpha = utiltestingapi.MakeClusterQueue("cq-global-alpha").
					ResourceGroup(*utiltestingapi.MakeFlavorQuotas(flavor.Name).
						Resource(corev1.ResourceCPU, "1").Obj()).
					Annotation(kueuealpha.PreemptionConfigNameAnnotation, configAlpha.Name).
					Obj()
				cqBeta = utiltestingapi.MakeClusterQueue("cq-global-beta").
					ResourceGroup(*utiltestingapi.MakeFlavorQuotas(flavor.Name).
						Resource(corev1.ResourceCPU, "2").Obj()).
					Annotation(kueuealpha.PreemptionConfigNameAnnotation, configBeta.Name).
					Obj()
				behavioral.CreateClusterQueuesAndWaitForActive(ctx, k8sClient, cqAlpha, cqBeta)

				lqAlpha = utiltestingapi.MakeLocalQueue("lq-global-alpha", ns.Name).ClusterQueue(cqAlpha.Name).Obj()
				lqBeta = utiltestingapi.MakeLocalQueue("lq-global-beta", ns.Name).ClusterQueue(cqBeta.Name).Obj()
				behavioral.CreateLocalQueuesAndWaitForActive(ctx, k8sClient, lqAlpha, lqBeta)

				limit = kueuetestalpha1.MakePreemptionLimit("global-rate-limit", kueuealpha.GlobalPreemptionLimitScope).
					Limit(2).
					LimitWindowSeconds(3).
					Obj()
				createPreemptionLimitAndWaitForSync(limit)
			})

			ginkgo.AfterEach(func() {
				gomega.Expect(behavioral.DeleteWorkloadsInNamespace(ctx, k8sClient, ns)).To(gomega.Succeed())
				behavioral.ExpectObjectToBeDeleted(ctx, k8sClient, lqAlpha, true)
				behavioral.ExpectObjectToBeDeleted(ctx, k8sClient, lqBeta, true)
				behavioral.ExpectObjectToBeDeleted(ctx, k8sClient, cqAlpha, true)
				behavioral.ExpectObjectToBeDeleted(ctx, k8sClient, cqBeta, true)
				behavioral.ExpectObjectToBeDeleted(ctx, k8sClient, limit, true)
				behavioral.ExpectObjectToBeDeleted(ctx, k8sClient, configAlpha, true)
				behavioral.ExpectObjectToBeDeleted(ctx, k8sClient, configBeta, true)
			})

			ginkgo.It("should throttle preemptions across ClusterQueues and recover budget once the window elapses", func() {
				lowAlpha := createWorkload("low-alpha", lqAlpha.Name, 10, "1")
				lowBeta1 := createWorkload("low-beta-1", lqBeta.Name, 10, "1")
				lowBeta2 := createWorkload("low-beta-2", lqBeta.Name, 20, "1")
				behavioral.ExpectWorkloadsToBeAdmitted(ctx, k8sClient, lowAlpha, lowBeta1, lowBeta2)

				var highBeta2 *kueue.Workload
				ginkgo.By("Allowing two preemptions across ClusterQueues and throttling the third within the window", func() {
					highAlpha := createWorkload("high-alpha", lqAlpha.Name, 100, "1")
					behavioral.FinishEvictionForWorkloads(ctx, k8sClient, lowAlpha)
					behavioral.ExpectWorkloadsToBeAdmitted(ctx, k8sClient, highAlpha)

					highBeta1 := createWorkload("high-beta-1", lqBeta.Name, 100, "1")
					behavioral.FinishEvictionForWorkloads(ctx, k8sClient, lowBeta1)
					behavioral.ExpectWorkloadsToBeAdmitted(ctx, k8sClient, highBeta1)

					highBeta2 = createWorkload("high-beta-2", lqBeta.Name, 100, "1")
					behavioral.ExpectWorkloadsToBePending(ctx, k8sClient, highBeta2)
					expectWorkloadsNotEvictedConsistently(lowBeta2)
					expectPreemptionLimitCounts(limit, []kueuealpha.PreemptionLimitCount{
						{Name: string(policy.GlobalScopeValue), Value: 2},
					})
				})

				ginkgo.By("Allowing the third preemption after the sliding window expires", func() {
					expectPreemptionLimitCounts(limit, nil)
					waitForLimitAllowance(limit.Name, policy.GlobalScopeValue, 2)
					highBeta2 = recreateBlockedPreemptor(highBeta2)

					behavioral.FinishEvictionForWorkloads(ctx, k8sClient, lowBeta2)
					behavioral.ExpectWorkloadsToBeAdmitted(ctx, k8sClient, highBeta2)
					expectPreemptionLimitCounts(limit, []kueuealpha.PreemptionLimitCount{
						{Name: string(policy.GlobalScopeValue), Value: 1},
					})
				})
			})
		})

		ginkgo.When("protecting a mission-critical ClusterQueue from preemption", func() {
			var (
				config     *kueuealpha.PreemptionConfig
				limit      *kueuealpha.PreemptionLimit
				cqCritical *kueue.ClusterQueue
				cqStandard *kueue.ClusterQueue
				cqOwner    *kueue.ClusterQueue
				lqCritical *kueue.LocalQueue
				lqStandard *kueue.LocalQueue
				lqOwner    *kueue.LocalQueue
			)

			ginkgo.BeforeEach(func() {
				config = kueuetestalpha1.MakePreemptionConfig("reclaim-cohort-config").
					Rule("reclaim-cohort", kueuealpha.Always,
						kueuetestalpha1.MakeCandidateSelector(kueuealpha.WithinCohortTree).
							Priority(kueuealpha.Boosted, kueuealpha.LessThan).
							Obj(),
					).Obj()
				behavioral.MustCreate(ctx, k8sClient, config)

				cqCritical = utiltestingapi.MakeClusterQueue("cq-mission-critical").
					Cohort("mission-cohort").
					Label("tier", "mission-critical").
					ResourceGroup(*utiltestingapi.MakeFlavorQuotas(flavor.Name).
						Resource(corev1.ResourceCPU, "0").Obj()).
					Obj()
				cqStandard = utiltestingapi.MakeClusterQueue("cq-standard").
					Cohort("mission-cohort").
					Label("tier", "standard").
					ResourceGroup(*utiltestingapi.MakeFlavorQuotas(flavor.Name).
						Resource(corev1.ResourceCPU, "0").Obj()).
					Obj()
				cqOwner = utiltestingapi.MakeClusterQueue("cq-owner").
					Cohort("mission-cohort").
					ResourceGroup(*utiltestingapi.MakeFlavorQuotas(flavor.Name).
						Resource(corev1.ResourceCPU, "2").Obj()).
					Annotation(kueuealpha.PreemptionConfigNameAnnotation, config.Name).
					Obj()
				behavioral.CreateClusterQueuesAndWaitForActive(ctx, k8sClient, cqCritical, cqStandard, cqOwner)

				lqCritical = utiltestingapi.MakeLocalQueue("lq-critical", ns.Name).ClusterQueue(cqCritical.Name).Obj()
				lqStandard = utiltestingapi.MakeLocalQueue("lq-standard", ns.Name).ClusterQueue(cqStandard.Name).Obj()
				lqOwner = utiltestingapi.MakeLocalQueue("lq-owner", ns.Name).ClusterQueue(cqOwner.Name).Obj()
				behavioral.CreateLocalQueuesAndWaitForActive(ctx, k8sClient, lqCritical, lqStandard, lqOwner)

				limit = kueuetestalpha1.MakePreemptionLimit("protect-mission-critical-cq", kueuealpha.PreemptedCQLimitScope).
					PreemptedClusterQueueSelector(&metav1.LabelSelector{MatchLabels: map[string]string{"tier": "mission-critical"}}).
					Limit(0).
					LimitWindowSeconds(60).
					Obj()
				createPreemptionLimitAndWaitForSync(limit)
			})

			ginkgo.AfterEach(func() {
				gomega.Expect(behavioral.DeleteWorkloadsInNamespace(ctx, k8sClient, ns)).To(gomega.Succeed())
				behavioral.ExpectObjectToBeDeleted(ctx, k8sClient, lqCritical, true)
				behavioral.ExpectObjectToBeDeleted(ctx, k8sClient, lqStandard, true)
				behavioral.ExpectObjectToBeDeleted(ctx, k8sClient, lqOwner, true)
				behavioral.ExpectObjectToBeDeleted(ctx, k8sClient, cqCritical, true)
				behavioral.ExpectObjectToBeDeleted(ctx, k8sClient, cqStandard, true)
				behavioral.ExpectObjectToBeDeleted(ctx, k8sClient, cqOwner, true)
				behavioral.ExpectObjectToBeDeleted(ctx, k8sClient, limit, true)
				behavioral.ExpectObjectToBeDeleted(ctx, k8sClient, config, true)
			})

			ginkgo.It("should never evict workloads from the mission-critical ClusterQueue while allowing preemption of other ClusterQueues", func() {
				// Give criticalWl lower priority (10 < 20) so it would be chosen first without the PreemptionLimit.
				criticalWl := createWorkload("critical-wl", lqCritical.Name, 10, "1")
				standardWl := createWorkload("standard-wl", lqStandard.Name, 20, "1")
				behavioral.ExpectWorkloadsToBeAdmitted(ctx, k8sClient, criticalWl, standardWl)

				ginkgo.By("Preempting the standard ClusterQueue workload while sparing the mission-critical one", func() {
					ownerWl1 := createWorkload("owner-1", lqOwner.Name, 100, "1")
					behavioral.FinishEvictionForWorkloads(ctx, k8sClient, standardWl)
					behavioral.ExpectWorkloadsToBeAdmitted(ctx, k8sClient, ownerWl1)
					expectWorkloadsNotEvictedConsistently(criticalWl)
				})

				ginkgo.By("Keeping a second owner workload pending rather than evicting from the mission-critical ClusterQueue", func() {
					ownerWl2 := createWorkload("owner-2", lqOwner.Name, 100, "1")
					behavioral.ExpectWorkloadsToBePending(ctx, k8sClient, ownerWl2)
					expectWorkloadsNotEvictedConsistently(criticalWl)
					expectPreemptionLimitCounts(limit, nil)
				})
			})
		})

		ginkgo.When("enforcing fair preemption budgets per tenant in a shared cohort", func() {
			var (
				config          *kueuealpha.PreemptionConfig
				limit           *kueuealpha.PreemptionLimit
				cqTenantA       *kueue.ClusterQueue
				cqTenantB       *kueue.ClusterQueue
				cqOpportunistic *kueue.ClusterQueue
				lqTenantA       *kueue.LocalQueue
				lqTenantB       *kueue.LocalQueue
				lqOpportunistic *kueue.LocalQueue
			)

			ginkgo.BeforeEach(func() {
				config = kueuetestalpha1.MakePreemptionConfig("tenant-cohort-config").
					Rule("within-cohort", kueuealpha.Always,
						kueuetestalpha1.MakeCandidateSelector(kueuealpha.WithinCohortTree).
							Priority(kueuealpha.Boosted, kueuealpha.LessThan).
							Obj(),
					).Obj()
				behavioral.MustCreate(ctx, k8sClient, config)

				cqTenantA = utiltestingapi.MakeClusterQueue("cq-tenant-a").
					Cohort("tenants-cohort").
					Label("role", "tenant").
					ResourceGroup(*utiltestingapi.MakeFlavorQuotas(flavor.Name).
						Resource(corev1.ResourceCPU, "2").Obj()).
					Annotation(kueuealpha.PreemptionConfigNameAnnotation, config.Name).
					Obj()
				cqTenantB = utiltestingapi.MakeClusterQueue("cq-tenant-b").
					Cohort("tenants-cohort").
					Label("role", "tenant").
					ResourceGroup(*utiltestingapi.MakeFlavorQuotas(flavor.Name).
						Resource(corev1.ResourceCPU, "1").Obj()).
					Annotation(kueuealpha.PreemptionConfigNameAnnotation, config.Name).
					Obj()
				cqOpportunistic = utiltestingapi.MakeClusterQueue("cq-opportunistic").
					Cohort("tenants-cohort").
					ResourceGroup(*utiltestingapi.MakeFlavorQuotas(flavor.Name).
						Resource(corev1.ResourceCPU, "0").Obj()).
					Obj()
				behavioral.CreateClusterQueuesAndWaitForActive(ctx, k8sClient, cqTenantA, cqTenantB, cqOpportunistic)

				lqTenantA = utiltestingapi.MakeLocalQueue("lq-tenant-a", ns.Name).ClusterQueue(cqTenantA.Name).Obj()
				lqTenantB = utiltestingapi.MakeLocalQueue("lq-tenant-b", ns.Name).ClusterQueue(cqTenantB.Name).Obj()
				lqOpportunistic = utiltestingapi.MakeLocalQueue("lq-opportunistic", ns.Name).ClusterQueue(cqOpportunistic.Name).Obj()
				behavioral.CreateLocalQueuesAndWaitForActive(ctx, k8sClient, lqTenantA, lqTenantB, lqOpportunistic)

				limit = kueuetestalpha1.MakePreemptionLimit("per-tenant-budget", kueuealpha.PreemptorCQLimitScope).
					PreemptorClusterQueueSelector(&metav1.LabelSelector{MatchLabels: map[string]string{"role": "tenant"}}).
					Limit(1).
					LimitWindowSeconds(60).
					Obj()
				createPreemptionLimitAndWaitForSync(limit)
			})

			ginkgo.AfterEach(func() {
				gomega.Expect(behavioral.DeleteWorkloadsInNamespace(ctx, k8sClient, ns)).To(gomega.Succeed())
				behavioral.ExpectObjectToBeDeleted(ctx, k8sClient, lqTenantA, true)
				behavioral.ExpectObjectToBeDeleted(ctx, k8sClient, lqTenantB, true)
				behavioral.ExpectObjectToBeDeleted(ctx, k8sClient, lqOpportunistic, true)
				behavioral.ExpectObjectToBeDeleted(ctx, k8sClient, cqTenantA, true)
				behavioral.ExpectObjectToBeDeleted(ctx, k8sClient, cqTenantB, true)
				behavioral.ExpectObjectToBeDeleted(ctx, k8sClient, cqOpportunistic, true)
				behavioral.ExpectObjectToBeDeleted(ctx, k8sClient, limit, true)
				behavioral.ExpectObjectToBeDeleted(ctx, k8sClient, config, true)
			})

			ginkgo.It("should isolate preemption budgets per preemptor ClusterQueue", func() {
				oppWl1 := createWorkload("opp-1", lqOpportunistic.Name, 10, "1")
				oppWl2 := createWorkload("opp-2", lqOpportunistic.Name, 20, "1")
				oppWl3 := createWorkload("opp-3", lqOpportunistic.Name, 30, "1")
				behavioral.ExpectWorkloadsToBeAdmitted(ctx, k8sClient, oppWl1, oppWl2, oppWl3)

				var tenantAWl2 *kueue.Workload
				ginkgo.By("Allowing Tenant A one preemption and throttling its second preemption", func() {
					tenantAWl1 := createWorkload("tenant-a-1", lqTenantA.Name, 100, "1")
					behavioral.FinishEvictionForWorkloads(ctx, k8sClient, oppWl1)
					behavioral.ExpectWorkloadsToBeAdmitted(ctx, k8sClient, tenantAWl1)

					tenantAWl2 = createWorkload("tenant-a-2", lqTenantA.Name, 100, "1")
					behavioral.ExpectWorkloadsToBePending(ctx, k8sClient, tenantAWl2)
					expectWorkloadsNotEvictedConsistently(oppWl2, oppWl3)
					expectPreemptionLimitCounts(limit, []kueuealpha.PreemptionLimitCount{
						{Name: cqTenantA.Name, Value: 1},
					})
				})

				ginkgo.By("Allowing Tenant B to use its own independent preemption budget", func() {
					tenantBWl1 := createWorkload("tenant-b-1", lqTenantB.Name, 100, "1")
					behavioral.FinishEvictionForWorkloads(ctx, k8sClient, oppWl2)
					behavioral.ExpectWorkloadsToBeAdmitted(ctx, k8sClient, tenantBWl1)

					behavioral.ExpectWorkloadsToBePending(ctx, k8sClient, tenantAWl2)
					expectWorkloadsNotEvictedConsistently(oppWl3)
					expectPreemptionLimitCounts(limit, []kueuealpha.PreemptionLimitCount{
						{Name: cqTenantA.Name, Value: 1},
						{Name: cqTenantB.Name, Value: 1},
					})
				})
			})
		})

		ginkgo.When("reporting PreemptionLimitStatus across preemptions, spec changes, and window expiration", func() {
			var (
				config           *kueuealpha.PreemptionConfig
				preemptedCQLimit *kueuealpha.PreemptionLimit
				shortGlobalLimit *kueuealpha.PreemptionLimit
				cqVictimA        *kueue.ClusterQueue
				cqVictimB        *kueue.ClusterQueue
				cqPreemptor      *kueue.ClusterQueue
				lqVictimA        *kueue.LocalQueue
				lqVictimB        *kueue.LocalQueue
				lqPreemptor      *kueue.LocalQueue
			)

			ginkgo.BeforeEach(func() {
				config = kueuetestalpha1.MakePreemptionConfig("status-tracking-config").
					Label("env", "prod").
					Rule("within-cohort", kueuealpha.Always,
						kueuetestalpha1.MakeCandidateSelector(kueuealpha.WithinCohortTree).
							Priority(kueuealpha.Boosted, kueuealpha.LessThan).
							Obj(),
					).Obj()
				behavioral.MustCreate(ctx, k8sClient, config)

				cqVictimA = utiltestingapi.MakeClusterQueue("cq-status-victim-a").
					Cohort("status-cohort").
					ResourceGroup(*utiltestingapi.MakeFlavorQuotas(flavor.Name).
						Resource(corev1.ResourceCPU, "0").Obj()).
					Obj()
				cqVictimB = utiltestingapi.MakeClusterQueue("cq-status-victim-b").
					Cohort("status-cohort").
					ResourceGroup(*utiltestingapi.MakeFlavorQuotas(flavor.Name).
						Resource(corev1.ResourceCPU, "0").Obj()).
					Obj()
				cqPreemptor = utiltestingapi.MakeClusterQueue("cq-status-preemptor").
					Cohort("status-cohort").
					ResourceGroup(*utiltestingapi.MakeFlavorQuotas(flavor.Name).
						Resource(corev1.ResourceCPU, "3").Obj()).
					Annotation(kueuealpha.PreemptionConfigNameAnnotation, config.Name).
					Obj()
				behavioral.CreateClusterQueuesAndWaitForActive(ctx, k8sClient, cqVictimA, cqVictimB, cqPreemptor)

				lqVictimA = utiltestingapi.MakeLocalQueue("lq-status-victim-a", ns.Name).ClusterQueue(cqVictimA.Name).Obj()
				lqVictimB = utiltestingapi.MakeLocalQueue("lq-status-victim-b", ns.Name).ClusterQueue(cqVictimB.Name).Obj()
				lqPreemptor = utiltestingapi.MakeLocalQueue("lq-status-preemptor", ns.Name).ClusterQueue(cqPreemptor.Name).Obj()
				behavioral.CreateLocalQueuesAndWaitForActive(ctx, k8sClient, lqVictimA, lqVictimB, lqPreemptor)

				prodConfigSelector := &metav1.LabelSelector{MatchLabels: map[string]string{"env": "prod"}}
				preemptedCQLimit = kueuetestalpha1.MakePreemptionLimit("status-preempted-cq-limit", kueuealpha.PreemptedCQLimitScope).
					ConfigSelector(prodConfigSelector).
					Limit(5).
					LimitWindowSeconds(60).
					Obj()
				shortGlobalLimit = kueuetestalpha1.MakePreemptionLimit("status-short-global-limit", kueuealpha.GlobalPreemptionLimitScope).
					ConfigSelector(prodConfigSelector).
					Limit(5).
					LimitWindowSeconds(2).
					Obj()
				createPreemptionLimitAndWaitForSync(preemptedCQLimit)
				createPreemptionLimitAndWaitForSync(shortGlobalLimit)
			})

			ginkgo.AfterEach(func() {
				gomega.Expect(behavioral.DeleteWorkloadsInNamespace(ctx, k8sClient, ns)).To(gomega.Succeed())
				behavioral.ExpectObjectToBeDeleted(ctx, k8sClient, lqVictimA, true)
				behavioral.ExpectObjectToBeDeleted(ctx, k8sClient, lqVictimB, true)
				behavioral.ExpectObjectToBeDeleted(ctx, k8sClient, lqPreemptor, true)
				behavioral.ExpectObjectToBeDeleted(ctx, k8sClient, cqVictimA, true)
				behavioral.ExpectObjectToBeDeleted(ctx, k8sClient, cqVictimB, true)
				behavioral.ExpectObjectToBeDeleted(ctx, k8sClient, cqPreemptor, true)
				behavioral.ExpectObjectToBeDeleted(ctx, k8sClient, preemptedCQLimit, true)
				behavioral.ExpectObjectToBeDeleted(ctx, k8sClient, shortGlobalLimit, true)
				behavioral.ExpectObjectToBeDeleted(ctx, k8sClient, config, true)
			})

			ginkgo.It("should update, sort, preserve, and prune status counts accurately", func() {
				victimB1 := createWorkload("victim-b-1", lqVictimB.Name, 10, "1")
				victimB2 := createWorkload("victim-b-2", lqVictimB.Name, 20, "1")
				victimA1 := createWorkload("victim-a-1", lqVictimA.Name, 30, "1")
				behavioral.ExpectWorkloadsToBeAdmitted(ctx, k8sClient, victimB1, victimB2, victimA1)

				wantCQCounts := []kueuealpha.PreemptionLimitCount{
					{Name: cqVictimB.Name, Value: 2},
					{Name: cqVictimA.Name, Value: 1},
				}

				ginkgo.By("Recording preemptions and reporting counts sorted by descending value", func() {
					preemptor := createWorkload("preemptor-all", lqPreemptor.Name, 100, "3")
					behavioral.FinishEvictionForWorkloads(ctx, k8sClient, victimB1, victimB2, victimA1)
					behavioral.ExpectWorkloadsToBeAdmitted(ctx, k8sClient, preemptor)

					expectPreemptionLimitCounts(preemptedCQLimit, wantCQCounts)
					expectPreemptionLimitCounts(shortGlobalLimit, []kueuealpha.PreemptionLimitCount{
						{Name: string(policy.GlobalScopeValue), Value: 3},
					})
				})

				ginkgo.By("Pruning expired counts from status once the short sliding window elapses", func() {
					expectPreemptionLimitCounts(shortGlobalLimit, nil)
					expectPreemptionLimitCounts(preemptedCQLimit, wantCQCounts)
				})

				ginkgo.By("Preserving status counts when only limit changes", func() {
					updatePreemptionLimit(preemptedCQLimit, func(pl *kueuealpha.PreemptionLimit) {
						pl.Spec.Limit = 10
					})
					// With 2 recorded preemptions on cqVictimB, allowance for 4 more only becomes true once Limit=10 is synced.
					waitForLimitAllowance(preemptedCQLimit.Name, policy.PreemptionLimitScopeValue(cqVictimB.Name), 4)
					gomega.Consistently(func(g gomega.Gomega) {
						var got kueuealpha.PreemptionLimit
						g.Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(preemptedCQLimit), &got)).To(gomega.Succeed())
						g.Expect(got.Status.Counts).To(gomega.Equal(wantCQCounts))
					}, behavioral.ConsistentDuration, behavioral.ShortInterval).Should(gomega.Succeed())
				})
			})
		})

		ginkgo.When("resetting or deleting a blocking PreemptionLimit", func() {
			var (
				config *kueuealpha.PreemptionConfig
				limit  *kueuealpha.PreemptionLimit
				cq     *kueue.ClusterQueue
				lq     *kueue.LocalQueue
			)

			ginkgo.BeforeEach(func() {
				config = kueuetestalpha1.MakePreemptionConfig("reset-delete-config").
					Rule("within-cq", kueuealpha.Always,
						kueuetestalpha1.MakeCandidateSelector(kueuealpha.WithinClusterQueue).
							Priority(kueuealpha.Boosted, kueuealpha.LessThan).
							Obj(),
					).Obj()
				behavioral.MustCreate(ctx, k8sClient, config)

				cq = utiltestingapi.MakeClusterQueue("cq-reset-delete").
					ResourceGroup(*utiltestingapi.MakeFlavorQuotas(flavor.Name).
						Resource(corev1.ResourceCPU, "3").Obj()).
					Annotation(kueuealpha.PreemptionConfigNameAnnotation, config.Name).
					Obj()
				behavioral.CreateClusterQueuesAndWaitForActive(ctx, k8sClient, cq)

				lq = utiltestingapi.MakeLocalQueue("lq-reset-delete", ns.Name).ClusterQueue(cq.Name).Obj()
				behavioral.CreateLocalQueuesAndWaitForActive(ctx, k8sClient, lq)

				limit = kueuetestalpha1.MakePreemptionLimit("reset-delete-limit", kueuealpha.GlobalPreemptionLimitScope).
					Limit(1).
					LimitWindowSeconds(60).
					Obj()
				createPreemptionLimitAndWaitForSync(limit)
			})

			ginkgo.AfterEach(func() {
				gomega.Expect(behavioral.DeleteWorkloadsInNamespace(ctx, k8sClient, ns)).To(gomega.Succeed())
				behavioral.ExpectObjectToBeDeleted(ctx, k8sClient, lq, true)
				behavioral.ExpectObjectToBeDeleted(ctx, k8sClient, cq, true)
				behavioral.ExpectObjectToBeDeleted(ctx, k8sClient, limit, true)
				behavioral.ExpectObjectToBeDeleted(ctx, k8sClient, config, true)
			})

			ginkgo.It("should reset enforcement on scope change and unblock preemption when deleted", func() {
				lowWl1 := createWorkload("low-1", lq.Name, 10, "1")
				lowWl2 := createWorkload("low-2", lq.Name, 20, "1")
				lowWl3 := createWorkload("low-3", lq.Name, 30, "1")
				behavioral.ExpectWorkloadsToBeAdmitted(ctx, k8sClient, lowWl1, lowWl2, lowWl3)

				var highWl2, highWl3 *kueue.Workload
				ginkgo.By("Exhausting Limit(1) and verifying a second preemption is blocked", func() {
					highWl1 := createWorkload("high-1", lq.Name, 100, "1")
					behavioral.FinishEvictionForWorkloads(ctx, k8sClient, lowWl1)
					behavioral.ExpectWorkloadsToBeAdmitted(ctx, k8sClient, highWl1)
					expectPreemptionLimitCounts(limit, []kueuealpha.PreemptionLimitCount{
						{Name: string(policy.GlobalScopeValue), Value: 1},
					})

					highWl2 = createWorkload("high-2", lq.Name, 100, "1")
					behavioral.ExpectWorkloadsToBePending(ctx, k8sClient, highWl2)
					expectWorkloadsNotEvictedConsistently(lowWl2, lowWl3)
				})

				ginkgo.By("Updating Scope so counts clear, a new preemption proceeds, and counts record under the new scope", func() {
					updatePreemptionLimit(limit, func(pl *kueuealpha.PreemptionLimit) {
						pl.Spec.Scope = kueuealpha.PreemptorCQLimitScope
					})
					expectPreemptionLimitCounts(limit, nil)
					waitForLimitAllowance(limit.Name, policy.PreemptionLimitScopeValue(cq.Name), 1)

					highWl2 = recreateBlockedPreemptor(highWl2)
					behavioral.FinishEvictionForWorkloads(ctx, k8sClient, lowWl2)
					behavioral.ExpectWorkloadsToBeAdmitted(ctx, k8sClient, highWl2)
					expectPreemptionLimitCounts(limit, []kueuealpha.PreemptionLimitCount{
						{Name: cq.Name, Value: 1},
					})

					highWl3 = createWorkload("high-3", lq.Name, 100, "1")
					behavioral.ExpectWorkloadsToBePending(ctx, k8sClient, highWl3)
					expectWorkloadsNotEvictedConsistently(lowWl3)
				})

				ginkgo.By("Deleting the blocking PreemptionLimit so preemption is allowed again", func() {
					behavioral.ExpectObjectToBeDeleted(ctx, k8sClient, limit, true)
					gomega.Eventually(func() policy.PreemptionLimitScopeCounts {
						return preemptionLimitTracker.Counts(time.Now(), policy.PreemptionLimitReference(limit.Name))
					}, behavioral.Timeout, behavioral.ShortInterval).Should(gomega.BeEmpty())

					highWl3 = recreateBlockedPreemptor(highWl3)
					behavioral.FinishEvictionForWorkloads(ctx, k8sClient, lowWl3)
					behavioral.ExpectWorkloadsToBeAdmitted(ctx, k8sClient, highWl3)
				})
			})
		})
	})
})
