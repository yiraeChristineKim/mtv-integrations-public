package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/stolostron/mtv-integrations/controllers"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/clientcmd"
	clusterv1 "open-cluster-management.io/api/cluster/v1"
	clusterv1client "open-cluster-management.io/api/client/cluster/clientset/versioned"
)

const (
	// clusterRecommendationAPIPort matches --api-bind-address default in cmd/main.go.
	clusterRecommendationAPIPort = "8082"

	// mtv namespace and deployment name after kustomize namePrefix "mtv-integrations-".
	mtvNamespace      = "open-cluster-management"
	mtvDeploymentName = "mtv-integrations-controller"

	testTimeout  = 120 * time.Second
	testInterval = 5 * time.Second
)

// targetStorageClass is the storage class name passed as targetStorageClass in
// every cluster-recommendation request. Set TEST_TARGET_STORAGE_CLASS in the
// environment to override; defaults to "standard" (kind's built-in class).
var targetStorageClass = func() string {
	if sc := os.Getenv("TEST_TARGET_STORAGE_CLASS"); sc != "" {
		return sc
	}
	return "standard"
}()

var _ = Describe("Cluster Recommendation API Smoke Tests", func() {

	// apiBase is the root URL for the recommendation API, served by the manager
	// process (run-instrument) or by port-forwarding the in-cluster service.
	apiBase := fmt.Sprintf("http://localhost:%s/api/cluster-recommendation", clusterRecommendationAPIPort)

	var clusterClient clusterv1client.Interface

	BeforeEach(func() {
		By("Building cluster client from kubeconfig")
		config, err := clientcmd.BuildConfigFromFlags("", kubeconfigHub)
		Expect(err).ToNot(HaveOccurred())
		clusterClient, err = clusterv1client.NewForConfig(config)
		Expect(err).ToNot(HaveOccurred())

		By("Waiting for mtv-integrations deployment to be ready")
		Eventually(func() error {
			dep, err := clientHub.AppsV1().Deployments(mtvNamespace).Get(
				context.TODO(), mtvDeploymentName, metav1.GetOptions{})
			if err != nil {
				return err
			}
			if dep.Status.ReadyReplicas == 0 {
				return fmt.Errorf("deployment %s/%s has 0 ready replicas", mtvNamespace, mtvDeploymentName)
			}
			return nil
		}, testTimeout, testInterval).Should(Succeed())
	})

	// ─────────────────────────────────────────────────────────────────────────
	// Context 1: HTTP-level validation — no managed clusters required.
	//   These tests exercise the handler's parameter parsing and method routing
	//   and should always pass as long as the manager process is running.
	// ─────────────────────────────────────────────────────────────────────────
	Context("API endpoint validation", func() {

		It("Should return 400 when required query parameters are missing", func() {
			// storageGB and targetStorageClass are both absent.
			url := fmt.Sprintf("%s?cpuCores=2&memoryGiB=4", apiBase)
			Eventually(func() error {
				resp, err := http.Get(url)
				if err != nil {
					return err
				}
				defer resp.Body.Close()
				if resp.StatusCode != http.StatusBadRequest {
					return fmt.Errorf("expected 400, got %d", resp.StatusCode)
				}
				return nil
			}, testTimeout, testInterval).Should(Succeed())
		})

		It("Should return 400 when targetStorageClass is missing from GET", func() {
			url := fmt.Sprintf("%s?cpuCores=2&memoryGiB=4&storageGB=50", apiBase)
			Eventually(func() error {
				resp, err := http.Get(url)
				if err != nil {
					return err
				}
				defer resp.Body.Close()
				if resp.StatusCode != http.StatusBadRequest {
					return fmt.Errorf("expected 400, got %d", resp.StatusCode)
				}
				return nil
			}, testTimeout, testInterval).Should(Succeed())
		})

		It("Should return 400 for an invalid cpuCores value", func() {
			url := fmt.Sprintf("%s?cpuCores=not-a-number&memoryGiB=4&storageGB=50&targetStorageClass=%s",
				apiBase, targetStorageClass)
			Eventually(func() error {
				resp, err := http.Get(url)
				if err != nil {
					return err
				}
				defer resp.Body.Close()
				if resp.StatusCode != http.StatusBadRequest {
					return fmt.Errorf("expected 400, got %d", resp.StatusCode)
				}
				return nil
			}, testTimeout, testInterval).Should(Succeed())
		})

		It("Should return 400 for an invalid memoryGiB value", func() {
			url := fmt.Sprintf("%s?cpuCores=2&memoryGiB=invalid&storageGB=50&targetStorageClass=%s",
				apiBase, targetStorageClass)
			Eventually(func() error {
				resp, err := http.Get(url)
				if err != nil {
					return err
				}
				defer resp.Body.Close()
				if resp.StatusCode != http.StatusBadRequest {
					return fmt.Errorf("expected 400, got %d", resp.StatusCode)
				}
				return nil
			}, testTimeout, testInterval).Should(Succeed())
		})

		It("Should return 400 for POST body with invalid JSON", func() {
			invalidJSON := `{"cpuCores": "not-a-number", "memoryGiB": 8, "storageGB": 100}`
			Eventually(func() error {
				resp, err := http.Post(apiBase, "application/json", strings.NewReader(invalidJSON))
				if err != nil {
					return err
				}
				defer resp.Body.Close()
				if resp.StatusCode != http.StatusBadRequest {
					return fmt.Errorf("expected 400, got %d", resp.StatusCode)
				}
				return nil
			}, testTimeout, testInterval).Should(Succeed())
		})

		It("Should return 400 for POST body missing targetStorageClass", func() {
			body, err := json.Marshal(controllers.VMResourceRequirements{
				CPUCores:  2,
				MemoryGiB: 4,
				StorageGB: 50,
				// TargetStorageClass intentionally omitted
			})
			Expect(err).ToNot(HaveOccurred())

			Eventually(func() error {
				resp, err := http.Post(apiBase, "application/json", strings.NewReader(string(body)))
				if err != nil {
					return err
				}
				defer resp.Body.Close()
				if resp.StatusCode != http.StatusBadRequest {
					return fmt.Errorf("expected 400, got %d", resp.StatusCode)
				}
				return nil
			}, testTimeout, testInterval).Should(Succeed())
		})

		It("Should return 400 for POST body with zero resource values", func() {
			body, err := json.Marshal(controllers.VMResourceRequirements{
				CPUCores:           0, // invalid
				MemoryGiB:          4,
				StorageGB:          50,
				TargetStorageClass: targetStorageClass,
			})
			Expect(err).ToNot(HaveOccurred())

			Eventually(func() error {
				resp, err := http.Post(apiBase, "application/json", strings.NewReader(string(body)))
				if err != nil {
					return err
				}
				defer resp.Body.Close()
				if resp.StatusCode != http.StatusBadRequest {
					return fmt.Errorf("expected 400, got %d", resp.StatusCode)
				}
				return nil
			}, testTimeout, testInterval).Should(Succeed())
		})

		It("Should return 405 for PUT on the cluster-recommendation endpoint", func() {
			req, err := http.NewRequest(http.MethodPut, apiBase, nil)
			Expect(err).ToNot(HaveOccurred())

			Eventually(func() error {
				resp, err := (&http.Client{}).Do(req)
				if err != nil {
					return err
				}
				defer resp.Body.Close()
				if resp.StatusCode != http.StatusMethodNotAllowed {
					return fmt.Errorf("expected 405, got %d", resp.StatusCode)
				}
				return nil
			}, testTimeout, testInterval).Should(Succeed())
		})

		It("Should return 200 with valid JSON for a well-formed GET request", func() {
			url := fmt.Sprintf("%s?cpuCores=2&memoryGiB=4&storageGB=50&targetStorageClass=%s",
				apiBase, targetStorageClass)

			var response controllers.ClusterRecommendationResponse
			Eventually(func() error {
				resp, err := http.Get(url)
				if err != nil {
					return err
				}
				defer resp.Body.Close()
				if resp.StatusCode != http.StatusOK {
					return fmt.Errorf("expected 200, got %d", resp.StatusCode)
				}
				return json.NewDecoder(resp.Body).Decode(&response)
			}, testTimeout, testInterval).Should(Succeed())

			// Status depends on cluster availability; all three are valid here.
			Expect(response.Status).To(Or(Equal("success"), Equal("error"), Equal("warning")))
			Expect(response.VMRequirements.CPUCores).To(Equal(int64(2)))
			Expect(response.VMRequirements.MemoryGiB).To(Equal(int64(4)))
			Expect(response.VMRequirements.StorageGB).To(Equal(int64(50)))
			Expect(response.VMRequirements.TargetStorageClass).To(Equal(targetStorageClass))
		})

		It("Should return 200 with valid JSON for a well-formed POST request", func() {
			body, err := json.Marshal(controllers.VMResourceRequirements{
				CPUCores:           4,
				MemoryGiB:          8,
				StorageGB:          100,
				TargetStorageClass: targetStorageClass,
			})
			Expect(err).ToNot(HaveOccurred())

			var response controllers.ClusterRecommendationResponse
			Eventually(func() error {
				resp, err := http.Post(apiBase, "application/json", strings.NewReader(string(body)))
				if err != nil {
					return err
				}
				defer resp.Body.Close()
				if resp.StatusCode != http.StatusOK {
					return fmt.Errorf("expected 200, got %d", resp.StatusCode)
				}
				return json.NewDecoder(resp.Body).Decode(&response)
			}, testTimeout, testInterval).Should(Succeed())

			Expect(response.Status).To(Or(Equal("success"), Equal("error"), Equal("warning")))
			Expect(response.VMRequirements.CPUCores).To(Equal(int64(4)))
			Expect(response.VMRequirements.MemoryGiB).To(Equal(int64(8)))
			Expect(response.VMRequirements.StorageGB).To(Equal(int64(100)))
		})
	})

	// ─────────────────────────────────────────────────────────────────────────
	// Context 2: ManagedCluster lifecycle tests — needs the ManagedCluster CRD.
	//   With cluster-proxy unavailable (CRD-only or no MSA token), the API
	//   returns Status="error" and AllClusters=[].
	//   With full OCM (cluster-proxy-addon-user Service + MSA addon running),
	//   the API returns scored clusters in AllClusters.
	//
	// Required infrastructure for full scoring:
	//   - cluster-proxy-addon-user Service in the multicluster-engine namespace
	//   - managed-serviceaccount addon agent running on each managed cluster
	//   - openshift-service-ca.crt ConfigMap in each cluster's namespace (MCE)
	//     OR the operator running with DEV_MODE=true pointing to a Route
	//   - At least one node labeled kubevirt.io/schedulable=true
	//   - metrics-server installed on the managed cluster
	//
	// Run "make prepare-e2e-ocm" to provision the full OCM stack on kind.
	// ─────────────────────────────────────────────────────────────────────────
	Context("When testing with managed clusters registered via OCM", func() {

		var testClusterName string

		BeforeEach(func() {
			testClusterName = "e2e-cluster-" + randomString(5)

			By("Creating a test managed cluster with CNV label")
			testCluster := &clusterv1.ManagedCluster{
				ObjectMeta: metav1.ObjectMeta{
					Name: testClusterName,
					Labels: map[string]string{
						"acm/cnv-operator-install": "true",
					},
				},
				Spec: clusterv1.ManagedClusterSpec{
					ManagedClusterClientConfigs: []clusterv1.ClientConfig{
						{URL: fmt.Sprintf("https://api.%s.example.com:6443", testClusterName)},
					},
				},
			}
			_, err := clusterClient.ClusterV1().ManagedClusters().Create(
				context.TODO(), testCluster, metav1.CreateOptions{})
			Expect(err).ToNot(HaveOccurred())
		})

		AfterEach(func() {
			By("Cleaning up test managed cluster")
			err := clusterClient.ClusterV1().ManagedClusters().Delete(
				context.TODO(), testClusterName, metav1.DeleteOptions{})
			if err != nil {
				GinkgoWriter.Printf("Warning: failed to delete test cluster %s: %v\n", testClusterName, err)
			}
		})

		It("Should return 200 and echo back VM requirements even when scoring fails", func() {
			// The cluster has a fake API URL, so cluster-proxy will fail to reach it.
			// The handler must still return HTTP 200 with the VMRequirements echoed.
			url := fmt.Sprintf("%s?cpuCores=1&memoryGiB=2&storageGB=20&targetStorageClass=%s",
				apiBase, targetStorageClass)

			var response controllers.ClusterRecommendationResponse
			Eventually(func() error {
				resp, err := http.Get(url)
				if err != nil {
					return err
				}
				defer resp.Body.Close()
				if resp.StatusCode != http.StatusOK {
					return fmt.Errorf("expected 200, got %d", resp.StatusCode)
				}
				return json.NewDecoder(resp.Body).Decode(&response)
			}, testTimeout, testInterval).Should(Succeed())

			Expect(response.Status).To(Or(Equal("success"), Equal("error"), Equal("warning")))
			Expect(response.VMRequirements.CPUCores).To(Equal(int64(1)))
			Expect(response.VMRequirements.MemoryGiB).To(Equal(int64(2)))
			Expect(response.VMRequirements.StorageGB).To(Equal(int64(20)))
		})

		It("Should score the cluster when cluster-proxy is fully available", func() {
			// This sub-test is only meaningful when the full OCM stack is running:
			// cluster-proxy-addon-user Service + managed-serviceaccount addon.
			// Skip it when cluster-proxy is not installed to avoid false negatives.
			if !clusterProxyAvailable() {
				Skip("cluster-proxy-addon-user Service not found in multicluster-engine namespace — " +
					"run 'make prepare-e2e-ocm' and join a managed cluster to enable this test")
			}

			url := fmt.Sprintf("%s?cpuCores=1&memoryGiB=2&storageGB=20&targetStorageClass=%s",
				apiBase, targetStorageClass)

			// Wait long enough for the MSA addon to provision a token (can take ~30 s).
			var response controllers.ClusterRecommendationResponse
			Eventually(func() error {
				resp, err := http.Get(url)
				if err != nil {
					return err
				}
				defer resp.Body.Close()
				if resp.StatusCode != http.StatusOK {
					return fmt.Errorf("expected 200, got %d", resp.StatusCode)
				}
				if err := json.NewDecoder(resp.Body).Decode(&response); err != nil {
					return err
				}
				// Keep retrying until we get a non-error status (MSA token may not be ready).
				if response.Status == "error" {
					return fmt.Errorf("cluster scoring not ready yet: %s", response.Message)
				}
				return nil
			}, testTimeout, testInterval).Should(Succeed())

			Expect(response.Status).To(Or(Equal("success"), Equal("warning")))
			Expect(response.AllClusters).ToNot(BeEmpty())

			By("Verifying test cluster appears in scored results")
			var found *controllers.ClusterScore
			for i := range response.AllClusters {
				if response.AllClusters[i].ClusterName == testClusterName {
					found = &response.AllClusters[i]
					break
				}
			}
			Expect(found).ToNot(BeNil(), "cluster %s not found in AllClusters", testClusterName)
			Expect(found.ClusterURL).To(Equal(
				fmt.Sprintf("https://api.%s.example.com:6443", testClusterName)))
		})

		It("Should not recommend any cluster for extremely large VM requirements", func() {
			// 128 vCPUs / 512 GiB is beyond any reasonable kind node.
			url := fmt.Sprintf("%s?cpuCores=128&memoryGiB=512&storageGB=100000&targetStorageClass=%s",
				apiBase, targetStorageClass)

			var response controllers.ClusterRecommendationResponse
			Eventually(func() error {
				resp, err := http.Get(url)
				if err != nil {
					return err
				}
				defer resp.Body.Close()
				if resp.StatusCode != http.StatusOK {
					return fmt.Errorf("expected 200, got %d", resp.StatusCode)
				}
				return json.NewDecoder(resp.Body).Decode(&response)
			}, testTimeout, testInterval).Should(Succeed())

			Expect(response.RecommendedCluster).To(BeNil())
			// "warning" = clusters scored but none can fit; "error" = scoring failed entirely.
			Expect(response.Status).To(Or(Equal("warning"), Equal("error")))
		})
	})
})

func randomString(length int) string {
	const charset = "abcdefghijklmnopqrstuvwxyz0123456789"
	b := make([]byte, length)
	for i := range b {
		b[i] = charset[time.Now().UnixNano()%int64(len(charset))]
	}
	return string(b)
}
