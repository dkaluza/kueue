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

package baseline

import (
	"slices"

	"github.com/google/go-cmp/cmp/cmpopts"
	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	v1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"

	"sigs.k8s.io/controller-runtime/pkg/client"
	configapi "sigs.k8s.io/kueue/apis/config/v1beta2"
	kueue "sigs.k8s.io/kueue/apis/kueue/v1beta2"
	workloadjob "sigs.k8s.io/kueue/pkg/controller/jobs/job"
	"sigs.k8s.io/kueue/pkg/features"
	clientutil "sigs.k8s.io/kueue/pkg/util/client"
	"sigs.k8s.io/kueue/pkg/util/tas"
	utiltestingapi "sigs.k8s.io/kueue/pkg/util/testing/v1beta2"
	jobtesting "sigs.k8s.io/kueue/pkg/util/testingjobs/job"
	testingjob "sigs.k8s.io/kueue/pkg/util/testingjobs/job"
	"sigs.k8s.io/kueue/test/util"
)

var _ = ginkgo.Describe("Configuration Preemptions", ginkgo.Label("feature:configurablepreemption"), ginkgo.Ordered, ginkgo.ContinueOnFailure, func() {
	const (
		extraResource    = "example.com/extraResource"
		commonLabelKey   = "commonTestingKey"
		commonLabelValue = "commonTestingValue"
	)

	var (
		ns *corev1.Namespace
		rf *kueue.ResourceFlavor
		cq *kueue.ClusterQueue
		lq *kueue.LocalQueue
	)

	preemptionConfigName := "preemption-config"
	priorityLabel := "test-priority-label"
	preemptionConfig := kueue.PreemptionConfig{
		ObjectMeta: metav1.ObjectMeta{
			Name: preemptionConfigName,
		},
		Spec: kueue.PreemptionConfigSpec{
			Rules: []kueue.PreemptionRule{
				{
					Name:    "test-rule-one",
					Trigger: kueue.InsufficientQuota,
					Candidates: []kueue.PreemptionCandidateSelector{
						{
							RelationRequirement: kueue.SameClusterQueue,
							NumericLabels: []kueue.NumericLabelConstraint{
								{
									Key:          priorityLabel,
									DefaultValue: ptr.To[int32](0),
									Relation:     ptr.To(kueue.Lower),
								},
							},
						},
					},
				},
			},
		},
	}

	ginkgo.BeforeAll(func() {
		util.UpdateKueueConfigurationAndRestart(ctx, k8sClient, defaultKueueCfg, kindClusterName, func(cfg *configapi.Configuration) {
			cfg.FeatureGates = map[string]bool{
				string(features.ConfigurablePreemption):  true,
				string(features.TopologyAwareScheduling): true,
			}
			cfg.Integrations = &configapi.Integrations{
				LabelKeysToCopy: []string{priorityLabel},
			}
		})

		nodes := &corev1.NodeList{}
		requiredLabelKeys := client.HasLabels{"instance-type"}
		err := k8sClient.List(ctx, nodes, requiredLabelKeys)
		gomega.Expect(err).NotTo(gomega.HaveOccurred(), "failed to list nodes for TAS")

		// Assign common label and add extra resource to every node.
		for _, n := range nodes.Items {
			gomega.Eventually(func(g gomega.Gomega) {
				node := &corev1.Node{}
				g.Expect(k8sClient.Get(ctx, client.ObjectKey{Name: n.Name}, node)).To(gomega.Succeed())
				err := clientutil.PatchStatus(ctx, k8sClient, node, func() (bool, error) {
					node.Labels[commonLabelKey] = commonLabelValue
					node.Status.Capacity[extraResource] = resource.MustParse("1")
					node.Status.Allocatable[extraResource] = resource.MustParse("1")
					return true, nil
				})
				g.Expect(err).NotTo(gomega.HaveOccurred())
			}, util.Timeout, util.Interval).Should(gomega.Succeed())
		}
	})

	ginkgo.AfterAll(func() {
		nodes := &corev1.NodeList{}
		requiredLabelKeys := client.HasLabels{"instance-type"}
		err := k8sClient.List(ctx, nodes, requiredLabelKeys)
		gomega.Expect(err).NotTo(gomega.HaveOccurred(), "failed to list nodes for TAS")

		// Remove common label and extra resource from every node.
		for _, n := range nodes.Items {
			gomega.Eventually(func(g gomega.Gomega) {
				node := &corev1.Node{}
				g.Expect(k8sClient.Get(ctx, client.ObjectKey{Name: n.Name}, node)).To(gomega.Succeed())
				err := clientutil.PatchStatus(ctx, k8sClient, node, func() (bool, error) {
					delete(node.Labels, commonLabelKey)
					delete(node.Status.Capacity, extraResource)
					delete(node.Status.Allocatable, extraResource)
					return true, nil
				})
				g.Expect(err).NotTo(gomega.HaveOccurred())
			}, util.Timeout, util.Interval).Should(gomega.Succeed())
		}
	})

	ginkgo.BeforeEach(func() {
		ns = util.CreateNamespaceFromPrefixWithLog(ctx, k8sClient, "ns-")

		util.MustCreate(ctx, k8sClient, &preemptionConfig)

		rf = utiltestingapi.MakeResourceFlavor("rf-" + ns.Name).Obj()
		util.MustCreate(ctx, k8sClient, rf)

		cohort := kueue.CohortReference("cohort-" + ns.Name)

		cq = utiltestingapi.MakeClusterQueue("cq-" + ns.Name).
			Cohort(cohort).
			ResourceGroup(*utiltestingapi.MakeFlavorQuotas(rf.Name).
				Resource(corev1.ResourceCPU, "2").
				Resource(corev1.ResourceMemory, "2G").
				Obj()).
			PreemptionConfigName(preemptionConfigName).
			Obj()
		util.CreateClusterQueuesAndWaitForActive(ctx, k8sClient, cq)

		lq = utiltestingapi.MakeLocalQueue("lq", ns.Name).ClusterQueue(cq.Name).Obj()
		util.CreateLocalQueuesAndWaitForActive(ctx, k8sClient, lq)
	})

	ginkgo.AfterEach(func() {
		gomega.Expect(util.DeleteNamespace(ctx, k8sClient, ns)).To(gomega.Succeed())
		util.ExpectObjectToBeDeleted(ctx, k8sClient, cq, true)
		util.ExpectObjectToBeDeleted(ctx, k8sClient, rf, true)
		util.ExpectObjectToBeDeleted(ctx, k8sClient, &preemptionConfig, true)
		util.ExpectAllPodsInNamespaceDeleted(ctx, k8sClient, ns)
	})

	ginkgo.When("Configurable preemption enabled", func() {
		ginkgo.It("Should preempt in the same LQ with lower priority", func() {
			ginkgo.By("Create jobs for admission")
			lowPriorityJob := jobtesting.MakeJob("low-priority-job", ns.Name).
				Queue(kueue.LocalQueueName(lq.Name)).
				Image(util.GetAgnHostImage(), util.BehaviorWaitForDeletion).
				RequestAndLimit(corev1.ResourceCPU, "1").
				RequestAndLimit(corev1.ResourceMemory, "200Mi").
				Label(priorityLabel, "1").
				TerminationGracePeriod(1).
				Obj()
			util.MustCreate(ctx, k8sClient, lowPriorityJob)

			highPriorityJob := jobtesting.MakeJob("high-priority-job", ns.Name).
				Queue(kueue.LocalQueueName(lq.Name)).
				Image(util.GetAgnHostImage(), util.BehaviorWaitForDeletion).
				RequestAndLimit(corev1.ResourceCPU, "1").
				RequestAndLimit(corev1.ResourceMemory, "200Mi").
				Label(priorityLabel, "9").
				TerminationGracePeriod(1).
				Obj()
			util.MustCreate(ctx, k8sClient, highPriorityJob)

			ginkgo.By("Waiting for workloads to be admitted")
			gomega.Eventually(func(g gomega.Gomega) {
				util.ExpectJobUnsuspended(ctx, k8sClient, client.ObjectKeyFromObject(lowPriorityJob))
				util.ExpectJobUnsuspended(ctx, k8sClient, client.ObjectKeyFromObject(highPriorityJob))
			}, util.Timeout, util.Interval).Should(gomega.Succeed())

			ginkgo.By("Create preempting job")
			preemptingJob := jobtesting.MakeJob("preempting-job", ns.Name).
				Queue(kueue.LocalQueueName(lq.Name)).
				Image(util.GetAgnHostImage(), util.BehaviorWaitForDeletion).
				RequestAndLimit(corev1.ResourceCPU, "1").
				RequestAndLimit(corev1.ResourceMemory, "200Mi").
				Label(priorityLabel, "5").
				TerminationGracePeriod(1).
				Obj()
			util.MustCreate(ctx, k8sClient, preemptingJob)

			ginkgo.By("Verify preemption")
			gomega.Eventually(func(g gomega.Gomega) {
				util.ExpectJobUnsuspended(ctx, k8sClient, client.ObjectKeyFromObject(preemptingJob))
				util.ExpectJobUnsuspended(ctx, k8sClient, client.ObjectKeyFromObject(highPriorityJob))

				g.Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(lowPriorityJob), lowPriorityJob)).Should(gomega.Succeed())
				g.Expect(lowPriorityJob.Spec.Suspend).Should(gomega.Equal(new(true)))

				wlLookupKey := types.NamespacedName{Name: workloadjob.GetWorkloadNameForJob(lowPriorityJob.Name, lowPriorityJob.UID), Namespace: ns.Name}
				lowPriorityWorkload := &kueue.Workload{}
				g.Expect(k8sClient.Get(ctx, wlLookupKey, lowPriorityWorkload)).Should(gomega.Succeed())
				g.Expect(lowPriorityWorkload.Status.Conditions).Should(gomega.ContainElement(gomega.BeComparableTo(
					metav1.Condition{
						Type:   kueue.WorkloadPreempted,
						Status: metav1.ConditionTrue,
						Reason: "ConfigurablePreemption",
					},
					cmpopts.IgnoreFields(metav1.Condition{}, "LastTransitionTime", "Message", "ObservedGeneration"),
				)))
			}, util.Timeout, util.Interval).Should(gomega.Succeed())
		})
	})

	ginkgo.When("Defragmentation preemption is configured", func() {
		var (
			preemptionConfig kueue.PreemptionConfig

			rf       *kueue.ResourceFlavor
			topology *kueue.Topology
			cq       *kueue.ClusterQueue
			lq       *kueue.LocalQueue
		)

		ginkgo.AfterEach(func() {
			gomega.Expect(util.DeleteNamespace(ctx, k8sClient, ns)).To(gomega.Succeed())
			util.ExpectObjectToBeDeleted(ctx, k8sClient, cq, true)
			util.ExpectObjectToBeDeleted(ctx, k8sClient, rf, true)
			util.ExpectObjectToBeDeleted(ctx, k8sClient, topology, true)
			util.ExpectObjectToBeDeleted(ctx, k8sClient, &preemptionConfig, true)
			util.ExpectAllPodsInNamespaceDeleted(ctx, k8sClient, ns)
		})

		ginkgo.It("Should reschedule running workload which and schedule incoming", func() {
			defragPreemptionConfigName := "defrag-preemption-config"
			preemptionConfig = kueue.PreemptionConfig{
				ObjectMeta: metav1.ObjectMeta{
					Name: defragPreemptionConfigName,
				},
				Spec: kueue.PreemptionConfigSpec{
					Rules: []kueue.PreemptionRule{
						{
							Name:    "defrag-config",
							Trigger: kueue.InsufficientTopology,
							Candidates: []kueue.PreemptionCandidateSelector{
								{
									RelationRequirement: kueue.SameClusterQueue,
								},
							},
						},
					},
				},
			}
			util.MustCreate(ctx, k8sClient, &preemptionConfig)

			topology = utiltestingapi.MakeDefaultOneLevelTopology("defrag-topology")
			util.MustCreate(ctx, k8sClient, topology)

			rf = utiltestingapi.MakeResourceFlavor("rf-defrag").
				// NodeLabel is required when TopologyName exists
				NodeLabel(commonLabelKey, commonLabelValue).
				TopologyName(topology.Name).
				Obj()
			util.MustCreate(ctx, k8sClient, rf)

			cq = utiltestingapi.MakeClusterQueue("cq-defrag").
				ResourceGroup(*utiltestingapi.MakeFlavorQuotas(rf.Name).
					Resource(extraResource, "2").
					Obj()).
				PreemptionConfigName(defragPreemptionConfigName).
				Obj()
			util.CreateClusterQueuesAndWaitForActive(ctx, k8sClient, cq)

			lq = utiltestingapi.MakeLocalQueue("lq-defrag", ns.Name).ClusterQueue(cq.Name).Obj()
			util.CreateLocalQueuesAndWaitForActive(ctx, k8sClient, lq)

			var jobA *v1.Job
			ginkgo.By("Schedule first job which requires whole extraResource available for node", func() {
				jobA = testingjob.MakeJob("job-a", ns.Name).
					Queue(kueue.LocalQueueName(lq.Name)).
					RequestAndLimit(extraResource, "1").
					PodAnnotation(kueue.PodSetRequiredTopologyAnnotation, corev1.LabelHostname).
					Image(util.GetAgnHostImage(), util.BehaviorWaitForDeletion).
					TerminationGracePeriod(1).
					Obj()
				util.MustCreate(ctx, k8sClient, jobA)
				util.ExpectJobToBeRunning(ctx, k8sClient, jobA)
			})

			var nodeName string
			wlA := &kueue.Workload{}
			ginkgo.By("Get workload and hostname for first job", func() {
				gomega.Expect(k8sClient.Get(ctx, types.NamespacedName{
					Name:      workloadjob.GetWorkloadNameForJob(jobA.Name, jobA.UID),
					Namespace: ns.Name,
				}, wlA)).To(gomega.Succeed())

				nodesA := slices.Collect(tas.LowestLevelValues(wlA.Status.Admission.PodSetAssignments[0].TopologyAssignment))
				gomega.Expect(len(nodesA)).To(gomega.Equal(1))
				nodeName = nodesA[0]
			})

			var jobB *v1.Job
			ginkgo.By("Schedule second job which requires whole extraResource and nodes are limited to nodes of first job", func() {
				jobB = testingjob.MakeJob("job-b", ns.Name).
					Queue(kueue.LocalQueueName(lq.Name)).
					RequestAndLimit(extraResource, "1").
					NodeSelector(corev1.LabelHostname, nodeName).
					PodAnnotation(kueue.PodSetRequiredTopologyAnnotation, corev1.LabelHostname).
					Image(util.GetAgnHostImage(), util.BehaviorWaitForDeletion).
					TerminationGracePeriod(1).
					Obj()
				util.MustCreate(ctx, k8sClient, jobB)
				util.ExpectJobToBeRunning(ctx, k8sClient, jobB)
			})

			ginkgo.By("Verify first job got preempted and rescheduled", func() {
				util.ExpectJobToBeRunning(ctx, k8sClient, jobA)
				util.ExpectJobToBeRunning(ctx, k8sClient, jobB)

				gomega.Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(wlA), wlA)).Should(gomega.Succeed())
				gomega.Expect(meta.FindStatusCondition(wlA.Status.Conditions, "Preempted")).Should(gomega.Not(gomega.BeNil()))
			})
		})
	})
})
