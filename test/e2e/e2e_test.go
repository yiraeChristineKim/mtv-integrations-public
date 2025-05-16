package e2e

import (
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"sigs.k8s.io/controller-runtime/pkg/client"
	// import your api and controller packages here
	// "your/module/api/v1"
	// "your/module/controllers"
	corev1 "k8s.io/api/core/v1"
	auth "open-cluster-management.io/managed-serviceaccount/apis/authentication/v1beta1"
)

var _ = Describe("Managed Cluster Controller E2E", func() {

	Context("When deciding whether to reconcile a ManagedCluster", func() {
		It("Should create a label on the cluster to trigger the reconcile and pass the entry gate successfully", func() {

			// Eventually check status or reconciliation
			Eventually(func() bool {
				err := k8sClient.Get(ctx, client.ObjectKey{Name: "test-cluster", Namespace: "default"}, cluster)
				return err == nil //&& cluster.Status
			}, time.Second*10, time.Millisecond*250).Should(BeTrue())

			// Update the resource and check reconciliation
			cluster.ObjectMeta.Labels = map[string]string{"acm/cnv-operator-install": "true"}
			err := k8sClient.Update(ctx, cluster)
			Expect(err).NotTo(HaveOccurred())

			Eventually(func() bool {
				err := k8sClient.Get(ctx, client.ObjectKey{Name: "test-cluster"}, cluster)
				return err == nil && cluster.GetObjectMeta().GetLabels()["acm/cnv-operator-install"] == "true"
			}, time.Second*10, time.Millisecond*250).Should(BeTrue())

		})
	})
})

var _ = Describe("Managed Cluster Controller E2E", func() {

	Context("When a ManagedCluster is reconciled a Provider, ClusterPermission and Provider Secret should be created", func() {
		It("Should create a ManagedServiceAccount and its related secret successfully", func() {

			// Eventually check status or reconciliation
			Eventually(func() bool {
				managedServiceAccount := &auth.ManagedServiceAccount{}

				err := k8sClient.Get(ctx, client.ObjectKey{Name: "test-cluster-mtv", Namespace: "default"}, managedServiceAccount)
				return err == nil //&& cluster.Status
			}, time.Second*10, time.Millisecond*250).Should(BeTrue())

			Eventually(func() bool {
				secret := &corev1.Secret{}
				err := k8sClient.Get(ctx, client.ObjectKey{Name: "test-cluster-mtv", Namespace: "default"}, secret)
				return err == nil
			}, time.Second*10, time.Millisecond*250).Should(BeTrue())

		})
	})
})
