package configurablepreemptions

import (
	"slices"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	"k8s.io/apimachinery/pkg/api/resource"
	"sigs.k8s.io/controller-runtime/pkg/client"
	kueue "sigs.k8s.io/kueue/apis/kueue/v1beta2"
	"sigs.k8s.io/kueue/pkg/util/tas"
	utiltestingapi "sigs.k8s.io/kueue/pkg/util/testing/v1beta2"
	testingnode "sigs.k8s.io/kueue/pkg/util/testingjobs/node"
	"sigs.k8s.io/kueue/pkg/workload"
	"sigs.k8s.io/kueue/test/util"
)

var _ = ginkgo.Describe("ConfigurablePreemptions", ginkgo.Label("feature:configurablepreemptions"), func() {
	var (
		ns *corev1.Namespace
	)

	var createWorkloadWithPriority = func(queue string, extraResourceRequests string, priority int32, nodeSelector map[string]string) *kueue.Workload {
		wl := utiltestingapi.MakeWorkloadWithGeneratedName("workload-", ns.Name).
			Priority(priority).
			Queue(kueue.LocalQueueName(queue)).
			Label(extraResource, extraResourceRequests).
			PodSets(*utiltestingapi.MakePodSet("worker", 1).
				RequiredTopologyRequest(corev1.LabelHostname).
				Request(extraResource, extraResourceRequests).
				NodeSelector(nodeSelector).Obj()).
			Obj()
		util.MustCreate(ctx, k8sClient, wl)
		return wl
	}

	var createWorkload = func(queue string, extraResourceRequests string, nodeSelector map[string]string) *kueue.Workload {
		return createWorkloadWithPriority(queue, extraResourceRequests, 0, nodeSelector)
	}

	ginkgo.BeforeEach(func() {
		fwk.StartManager(ctx, cfg, managerAndSchedulerSetup())
		ns = util.CreateNamespaceFromPrefixWithLog(ctx, k8sClient, "configurablepreemptions-")
	})

	ginkgo.AfterEach(func() {
		gomega.Expect(util.DeleteNamespace(ctx, k8sClient, ns)).To(gomega.Succeed())
		fwk.StopManager(ctx)
	})

	ginkgo.When("Defragmentation is configured", func() {
		var (
			flavor   *kueue.ResourceFlavor
			topology *kueue.Topology
			cqA      *kueue.ClusterQueue
			cqB      *kueue.ClusterQueue
			lqA      *kueue.LocalQueue
			lqB      *kueue.LocalQueue
			config   *kueue.PreemptionConfig
		)

		ginkgo.BeforeEach(func() {
			nodes := []corev1.Node{
				*testingnode.MakeNode("node-a").
					Label(commonLabelKey, commonLabelValue).
					Label(corev1.LabelHostname, "host-a").
					StatusAllocatable(corev1.ResourceList{
						extraResource:       resource.MustParse("2"),
						corev1.ResourcePods: resource.MustParse("2"),
					}).
					Ready().Obj(),
				*testingnode.MakeNode("node-b").
					Label(commonLabelKey, commonLabelValue).
					Label(corev1.LabelHostname, "host-b").
					StatusAllocatable(corev1.ResourceList{
						extraResource:       resource.MustParse("2"),
						corev1.ResourcePods: resource.MustParse("2"),
					}).
					Ready().Obj(),
			}
			util.CreateNodesWithStatus(ctx, k8sClient, nodes)

			defragPreemptionConfigName := "preemption-configuration"
			config = &kueue.PreemptionConfig{
				ObjectMeta: metav1.ObjectMeta{
					Name: defragPreemptionConfigName,
				},
				Spec: kueue.PreemptionConfigSpec{
					Rules: []kueue.PreemptionRule{
						{
							Name:    "defrag-smaller-tpu-workloads",
							Trigger: kueue.InsufficientTopology,
							Candidates: []kueue.PreemptionCandidateSelector{
								{
									RelationRequirement: kueue.AnyClusterQueue,
									NumericLabels: []kueue.NumericLabelConstraint{
										{
											Key:          extraResource,
											Relation:     ptr.To(kueue.Lower),
											DefaultValue: ptr.To(int32(0)),
										},
									},
								},
							},
						},
					},
				},
			}
			util.MustCreate(ctx, k8sClient, config)

			topology = utiltestingapi.MakeDefaultOneLevelTopology("defrag-topology")
			util.MustCreate(ctx, k8sClient, topology)

			flavor = utiltestingapi.MakeResourceFlavor("rf-defrag").
				// NodeLabel is required when TopologyName exists
				NodeLabel(commonLabelKey, commonLabelValue).
				TopologyName(topology.Name).
				Obj()
			util.MustCreate(ctx, k8sClient, flavor)

			cqA = utiltestingapi.MakeClusterQueue("cq-defrag-a").
				ResourceGroup(*utiltestingapi.MakeFlavorQuotas(flavor.Name).
					Resource(extraResource, "2").
					Obj()).
				PreemptionConfigName(defragPreemptionConfigName).
				Obj()
			util.CreateClusterQueuesAndWaitForActive(ctx, k8sClient, cqA)
			lqA = utiltestingapi.MakeLocalQueue("lq-a", ns.Name).ClusterQueue(cqA.Name).Obj()
			util.CreateLocalQueuesAndWaitForActive(ctx, k8sClient, lqA)

			cqB = utiltestingapi.MakeClusterQueue("cq-defrag-b").
				ResourceGroup(*utiltestingapi.MakeFlavorQuotas(flavor.Name).
					Resource(extraResource, "2").
					Obj()).
				PreemptionConfigName(defragPreemptionConfigName).
				Obj()
			util.CreateClusterQueuesAndWaitForActive(ctx, k8sClient, cqB)
			lqB = utiltestingapi.MakeLocalQueue("lq-b", ns.Name).ClusterQueue(cqB.Name).Obj()
			util.CreateLocalQueuesAndWaitForActive(ctx, k8sClient, lqB)
		})

		ginkgo.AfterEach(func() {
			gomega.Expect(util.DeleteWorkloadsInNamespace(ctx, k8sClient, ns)).Should(gomega.Succeed())
			util.ExpectObjectToBeDeleted(ctx, k8sClient, lqA, true)
			util.ExpectObjectToBeDeleted(ctx, k8sClient, lqB, true)
			util.ExpectObjectToBeDeleted(ctx, k8sClient, cqA, true)
			util.ExpectObjectToBeDeleted(ctx, k8sClient, cqB, true)
			util.ExpectObjectToBeDeleted(ctx, k8sClient, flavor, true)
			util.ExpectObjectToBeDeleted(ctx, k8sClient, topology, true)
			util.ExpectObjectToBeDeleted(ctx, k8sClient, config, true)
		})

		ginkgo.It("Should reschedule running workload and schedule incoming", func() {
			wlA := createWorkload("lq-a", "1", map[string]string{})
			util.ExpectWorkloadsToHaveQuotaReservation(ctx, k8sClient, cqA.Name, wlA)
			util.ExpectWorkloadsToBeAdmitted(ctx, k8sClient, wlA)

			gomega.Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(wlA), wlA)).Should(gomega.Succeed())
			nodesA := slices.Collect(tas.LowestLevelValues(wlA.Status.Admission.PodSetAssignments[0].TopologyAssignment))
			gomega.Expect(len(nodesA)).To(gomega.Equal(1))
			wlAHostnameBeforeReschedule := nodesA[0]

			wlB := createWorkload("lq-b", "2", map[string]string{corev1.LabelHostname: wlAHostnameBeforeReschedule})
			// TODO: inadmissible tweaks will not be needed after rebase, remove.
			util.ExpectWorkloadToHaveConditions(ctx, k8sClient, client.ObjectKeyFromObject(wlB), metav1.Condition{
				Type:   kueue.WorkloadInsufficientTopology,
				Status: metav1.ConditionTrue,
				Reason: kueue.WorkloadInsufficientTopology,
			})
			qManager.QueueAssociatedInadmissibleWorkloadsAfter(ctx, workload.NewReference(wlB.Namespace, wlB.Name), nil)

			util.FinishEvictionForWorkloads(ctx, k8sClient, wlA)
			util.ExpectWorkloadsToHaveQuotaReservation(ctx, k8sClient, cqB.Name, wlB)
			util.ExpectWorkloadsToBeAdmitted(ctx, k8sClient, wlB)
			util.ExpectWorkloadsToBeAdmitted(ctx, k8sClient, wlA)

			gomega.Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(wlA), wlA)).Should(gomega.Succeed())
			nodesA = slices.Collect(tas.LowestLevelValues(wlA.Status.Admission.PodSetAssignments[0].TopologyAssignment))
			gomega.Expect(len(nodesA)).To(gomega.Equal(1))
			wlAHostnameAfterReschedule := nodesA[0]

			gomega.Expect(wlAHostnameAfterReschedule).ShouldNot(gomega.Equal(wlAHostnameBeforeReschedule))

			// TODO: add third job for the lq-a with 2 units of extra resource, it should be blocked.
		})
	})
})
