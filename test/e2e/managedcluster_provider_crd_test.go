package e2e

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/stolostron/mtv-integrations/test/utils"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

var _ = Describe("Test crd controller", Ordered, func() {
	const (
		path               string = "../resources/managedcluster_provider_crd"
		providerCrdPath    string = path + "/provider_crd.yaml"
		managedclusterPath string = path + "/managedcluster.yaml"
		namespace          string = "mtv-integrations"
	)

	AfterEach(func() {
		By("Clean log file")
		err := utils.EmptyLogFile()
		Expect(err).ToNot(HaveOccurred(), "Failed to empty log file")
	})

	BeforeAll(func() {
		utils.Kubectl("apply", "-f", managedclusterPath)
		utils.Kubectl("create", "ns", "open-cluster-management-agent-addon")
		_, err := clientHub.AppsV1().Deployments("open-cluster-management-agent-addon").
			Create(context.TODO(), &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "managed-serviceaccount-addon-agent",
					Namespace: "open-cluster-management-agent-addon",
				},
				Spec: appsv1.DeploymentSpec{
					Replicas: func(i int32) *int32 { return &i }(1),
					Selector: &metav1.LabelSelector{
						MatchLabels: map[string]string{
							"app": "managed-serviceaccount-addon-agent",
						},
					},
					Template: corev1.PodTemplateSpec{
						ObjectMeta: metav1.ObjectMeta{
							Labels: map[string]string{
								"app": "managed-serviceaccount-addon-agent",
							},
						},
						Spec: corev1.PodSpec{
							Containers: []corev1.Container{{
								Name:    "pause",
								Image:   "registry.k8s.io/pause:3.9",
								Command: []string{"sleep", "infinity"},
							}},
						},
					},
				},
			}, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
	})

	AfterAll(func() {
		utils.Kubectl("delete", "-f", managedclusterPath, "--ignore-not-found")
		utils.Kubectl("delete", "ns", "open-cluster-management-agent-addon", "--ignore-not-found")
	})
	It("Should not start managedcluster controller", func() {
		Consistently(func() bool {
			return utils.FindLogMessage("Reconciling ManagedCluster")
		}, 10).Should(BeFalse(),
			"ManagedCluster controller should not reconcile without Provider CRD")
	})

	It("Should reconcile managedcluster after Provider CRD provided", func() {
		utils.Kubectl("apply", "-f", providerCrdPath)
		DeferCleanup(func() {
			By("Clean up the Provider CRD")
			utils.Kubectl("delete", "-f", providerCrdPath, "--ignore-not-found")
		})

		Eventually(func() bool {
			return utils.FindLogMessage("Reconciling ManagedCluster")
		}, 30).Should(BeTrue(),
			"ManagedCluster controller continues with Provider CRD")
	})

	It("Should not continue managedcluster controller after Provider CRD deleted", func() {
		utils.Kubectl("delete", "-f", providerCrdPath, "--ignore-not-found")
		DeferCleanup(func() {
			By("Clean up the Provider CRD")
			utils.Kubectl("delete", "-f", providerCrdPath, "--ignore-not-found")
		})

		Eventually(func() bool {
			return utils.FindLogMessage("Provider CRD is not established, skipping reconciliation")
		}, 30).Should(BeTrue(),
			"ManagedCluster controller should skip with Provider CRD removed")
	})

	It("Should start again managedcluster controller after Provider CRD provided", func() {
		utils.Kubectl("apply", "-f", providerCrdPath)
		DeferCleanup(func() {
			By("Clean up the Provider CRD")
			utils.Kubectl("delete", "-f", providerCrdPath, "--ignore-not-found")
		})

		Eventually(func() bool {
			return utils.FindLogMessage("Reconciling ManagedCluster")
		}, 30).Should(BeTrue(),
			"ManagedCluster controller should continue with Provider CRD")
	})
})
