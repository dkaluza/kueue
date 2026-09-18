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
	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	kueue "sigs.k8s.io/kueue/apis/kueue/v1beta2"
	kueueclientset "sigs.k8s.io/kueue/client-go/clientset/versioned"
	utiltesting "sigs.k8s.io/kueue/pkg/util/testing"
	"sigs.k8s.io/kueue/test/util"
)

const (
	preemptionConfigAdminUser  = "preemptionconfig-test-admin"
	preemptionConfigBatchUser  = "preemptionconfig-test-user"
	preemptionConfigNoRoleUser = "preemptionconfig-test-nobody"
)

var _ = ginkgo.Describe("PreemptionConfig RBAC", ginkgo.Label("area:singlecluster", "feature:rbac"), func() {
	ginkgo.When("A subject is bound to kueue-batch-admin-role", func() {
		var (
			adminClient        kueueclientset.Interface
			clusterRoleBinding *rbacv1.ClusterRoleBinding
		)

		ginkgo.BeforeEach(func() {
			adminClient, clusterRoleBinding = bindUserToClusterRole(preemptionConfigAdminUser, "kueue-batch-admin-role")
		})

		ginkgo.AfterEach(func() {
			util.ExpectObjectToBeDeleted(ctx, k8sClient, clusterRoleBinding, true)
		})

		ginkgo.It("Should allow the full PreemptionConfig lifecycle", func() {
			preemptionConfig := &kueue.PreemptionConfig{
				ObjectMeta: metav1.ObjectMeta{Name: "preemptionconfig-rbac-admin"},
				Spec: kueue.PreemptionConfigSpec{
					Rules: []kueue.PreemptionRule{{
						Name:       "rule",
						Trigger:    kueue.InsufficientQuota,
						Candidates: []kueue.PreemptionCandidateSelector{{RelationRequirement: kueue.SameClusterQueue}},
					}},
				},
			}
			// Also removed by the last step; this covers a failure before then.
			ginkgo.DeferCleanup(func() {
				util.ExpectObjectToBeDeleted(ctx, k8sClient, preemptionConfig, true)
			})

			ginkgo.By("Creating a PreemptionConfig", func() {
				created, err := adminClient.KueueV1beta2().PreemptionConfigs().Create(ctx, preemptionConfig, metav1.CreateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				preemptionConfig = created
			})

			ginkgo.By("Getting the PreemptionConfig", func() {
				got, err := adminClient.KueueV1beta2().PreemptionConfigs().Get(ctx, preemptionConfig.Name, metav1.GetOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				gomega.Expect(got.Spec.Rules).Should(gomega.HaveLen(1))
			})

			ginkgo.By("Listing the PreemptionConfigs", func() {
				list, err := adminClient.KueueV1beta2().PreemptionConfigs().List(ctx, metav1.ListOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				gomega.Expect(list.Items).Should(gomega.ContainElement(
					gomega.HaveField("ObjectMeta.Name", preemptionConfig.Name)))
			})

			ginkgo.By("Updating the PreemptionConfig", func() {
				// No Eventually: nothing else writes this object, so a retry would only hide a
				// missing update verb behind a timeout.
				preemptionConfig.Spec.Rules[0].Candidates[0].RelationRequirement = kueue.SameCohort
				updated, err := adminClient.KueueV1beta2().PreemptionConfigs().Update(ctx, preemptionConfig, metav1.UpdateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				gomega.Expect(updated.Spec.Rules[0].Candidates[0].RelationRequirement).Should(gomega.Equal(kueue.SameCohort))
				preemptionConfig = updated
			})

			ginkgo.By("Deleting the PreemptionConfig", func() {
				err := adminClient.KueueV1beta2().PreemptionConfigs().Delete(ctx, preemptionConfig.Name, metav1.DeleteOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
			})
		})
	})

	ginkgo.When("A subject is bound to kueue-batch-user-role", func() {
		var (
			userClient         kueueclientset.Interface
			clusterRoleBinding *rbacv1.ClusterRoleBinding
		)

		ginkgo.BeforeEach(func() {
			userClient, clusterRoleBinding = bindUserToClusterRole(preemptionConfigBatchUser, "kueue-batch-user-role")
		})

		ginkgo.AfterEach(func() {
			util.ExpectObjectToBeDeleted(ctx, k8sClient, clusterRoleBinding, true)
		})

		ginkgo.It("Should be Forbidden from accessing PreemptionConfigs", func() {
			expectPreemptionConfigAccessForbidden(userClient, "preemptionconfig-rbac-user")
		})
	})

	ginkgo.When("A subject is bound to neither kueue-batch-admin-role nor kueue-batch-user-role", func() {
		ginkgo.It("Should be Forbidden from accessing PreemptionConfigs", func() {
			expectPreemptionConfigAccessForbidden(
				util.CreateKueueClientset(preemptionConfigNoRoleUser), "preemptionconfig-rbac-nobody")
		})
	})
})

// bindUserToClusterRole binds user to clusterRole and returns a clientset acting as that user.
// The binding is cluster-wide because PreemptionConfig is cluster-scoped: a RoleBinding would deny
// access on scope alone, making the Forbidden assertions pass for the wrong reason.
func bindUserToClusterRole(user, clusterRole string) (kueueclientset.Interface, *rbacv1.ClusterRoleBinding) {
	ginkgo.GinkgoHelper()

	binding := &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{GenerateName: "preemptionconfig-rbac-"},
		RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: clusterRole},
		Subjects:   []rbacv1.Subject{{Name: user, APIGroup: rbacv1.GroupName, Kind: rbacv1.UserKind}},
	}
	util.MustCreate(ctx, k8sClient, binding)
	clientset := util.CreateKueueClientset(user)

	ginkgo.By("Wait for an already granted request to succeed to make sure the role binding is in effect", func() {
		// Probe LocalQueues, which both roles grant, rather than PreemptionConfigs: gating on the
		// permission under test would report a real regression as an opaque BeforeEach timeout.
		gomega.Eventually(func(g gomega.Gomega) {
			_, err := clientset.KueueV1beta2().LocalQueues(metav1.NamespaceAll).List(ctx, metav1.ListOptions{})
			g.Expect(err).NotTo(gomega.HaveOccurred())
		}, util.Timeout, util.Interval).Should(gomega.Succeed())
	})
	return clientset, binding
}

// expectPreemptionConfigAccessForbidden asserts that c is denied every verb on PreemptionConfigs.
// RBAC is checked before the object is looked up, so name does not need to exist.
func expectPreemptionConfigAccessForbidden(c kueueclientset.Interface, name string) {
	ginkgo.GinkgoHelper()

	preemptionConfigs := c.KueueV1beta2().PreemptionConfigs()
	preemptionConfig := &kueue.PreemptionConfig{ObjectMeta: metav1.ObjectMeta{Name: name}}

	// Only needed if a create unexpectedly succeeds.
	ginkgo.DeferCleanup(func() {
		util.ExpectObjectToBeDeleted(ctx, k8sClient, preemptionConfig, true)
	})

	ginkgo.By("Returning a Forbidden error for a create request", func() {
		_, err := preemptionConfigs.Create(ctx, preemptionConfig, metav1.CreateOptions{})
		gomega.Expect(err).Should(utiltesting.BeForbiddenError())
	})

	ginkgo.By("Returning a Forbidden error for a get request", func() {
		_, err := preemptionConfigs.Get(ctx, name, metav1.GetOptions{})
		gomega.Expect(err).Should(utiltesting.BeForbiddenError())
	})

	ginkgo.By("Returning a Forbidden error for a list request", func() {
		_, err := preemptionConfigs.List(ctx, metav1.ListOptions{})
		gomega.Expect(err).Should(utiltesting.BeForbiddenError())
	})

	ginkgo.By("Returning a Forbidden error for an update request", func() {
		_, err := preemptionConfigs.Update(ctx, preemptionConfig, metav1.UpdateOptions{})
		gomega.Expect(err).Should(utiltesting.BeForbiddenError())
	})

	ginkgo.By("Returning a Forbidden error for a delete request", func() {
		err := preemptionConfigs.Delete(ctx, name, metav1.DeleteOptions{})
		gomega.Expect(err).Should(utiltesting.BeForbiddenError())
	})
}
