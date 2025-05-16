/*
Copyright 2025.

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

package e2e

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/stolostron/mtv-integrations/controllers"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/rest"
	v1 "open-cluster-management.io/api/cluster/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

var (
	// Optional Environment Variables:
	// - PROMETHEUS_INSTALL_SKIP=true: Skips Prometheus Operator installation during test setup.
	// - CERT_MANAGER_INSTALL_SKIP=true: Skips CertManager installation during test setup.
	// These variables are useful if Prometheus or CertManager is already installed, avoiding
	// re-installation and conflicts.
	skipPrometheusInstall  = os.Getenv("PROMETHEUS_INSTALL_SKIP") == "true"
	skipCertManagerInstall = os.Getenv("CERT_MANAGER_INSTALL_SKIP") == "true"
	// isPrometheusOperatorAlreadyInstalled will be set true when prometheus CRDs be found on the cluster
	isPrometheusOperatorAlreadyInstalled = false
	// isCertManagerAlreadyInstalled will be set true when CertManager CRDs be found on the cluster
	isCertManagerAlreadyInstalled = false

	// projectImage is the name of the image which will be build and loaded
	// with the code source changes to be tested.
	projectImage = "quay.io/mtv-integrations:v0.0.1"

	k8sClient client.Client
	cfg       *rest.Config
	ctx       context.Context
	cancel    context.CancelFunc
	cluster   *v1.ManagedCluster
)

// TestE2E runs the end-to-end (e2e) test suite for the project. These tests execute in an isolated,
// temporary environment to validate project changes with the the purposed to be used in CI jobs.
// The default setup requires Kind, builds/loads the Manager Docker image locally, and installs
// CertManager and Prometheus.
func TestE2E(t *testing.T) {
	RegisterFailHandler(Fail)
	_, _ = fmt.Fprintf(GinkgoWriter, "Starting mtv-integrations integration test suite\n")
	RunSpecs(t, "e2e suite")
}

var _ = BeforeSuite(func() {
	/*By("Ensure that Prometheus is enabled")
	_ = utils.UncommentCode("config/default/kustomization.yaml", "#- ../prometheus", "#")

	By("building the manager(Operator) image")
	cmd := exec.Command("make", "docker-build", fmt.Sprintf("IMG=%s", projectImage))
	_, err := utils.Run(cmd)
	ExpectWithOffset(1, err).NotTo(HaveOccurred(), "Failed to build the manager(Operator) image")

	// TODO(user): If you want to change the e2e test vendor from Kind, ensure the image is
	// built and available before running the tests. Also, remove the following block.
	By("loading the manager(Operator) image on Kind")
	err = utils.LoadImageToKindClusterWithName(projectImage)
	ExpectWithOffset(1, err).NotTo(HaveOccurred(), "Failed to load the manager(Operator) image into Kind")

	// The tests-e2e are intended to run on a temporary cluster that is created and destroyed for testing.
	// To prevent errors when tests run in environments with Prometheus or CertManager already installed,
	// we check for their presence before execution.
	// Setup Prometheus and CertManager before the suite if not skipped and if not already installed
	if !skipPrometheusInstall {
		By("checking if prometheus is installed already")
		isPrometheusOperatorAlreadyInstalled = utils.IsPrometheusCRDsInstalled()
		if !isPrometheusOperatorAlreadyInstalled {
			_, _ = fmt.Fprintf(GinkgoWriter, "Installing Prometheus Operator...\n")
			Expect(utils.InstallPrometheusOperator()).To(Succeed(), "Failed to install Prometheus Operator")
		} else {
			_, _ = fmt.Fprintf(GinkgoWriter, "WARNING: Prometheus Operator is already installed. Skipping installation...\n")
		}
	}
	if !skipCertManagerInstall {
		By("checking if cert manager is installed already")
		isCertManagerAlreadyInstalled = utils.IsCertManagerCRDsInstalled()
		if !isCertManagerAlreadyInstalled {
			_, _ = fmt.Fprintf(GinkgoWriter, "Installing CertManager...\n")
			Expect(utils.InstallCertManager()).To(Succeed(), "Failed to install CertManager")
		} else {
			_, _ = fmt.Fprintf(GinkgoWriter, "WARNING: CertManager is already installed. Skipping installation...\n")
		}
	}*/
	cfg = ctrl.GetConfigOrDie()

	// init ctx
	ctx, cancel = context.WithCancel(context.Background())

	// Define scheme
	scheme := runtime.NewScheme()
	Expect(v1.AddToScheme(scheme)).To(Succeed())

	// Create the fake client
	k8sClient = fake.NewClientBuilder().WithScheme(scheme).Build()

	// Optionally: start the controller manager here if needed

	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme: scheme,
	})
	Expect(err).NotTo(HaveOccurred())

	// Register your controllers here, e.g.:
	err = (&controllers.ManagedClusterReconciler{
		Client:        mgr.GetClient(),
		Scheme:        mgr.GetScheme(),
		DynamicClient: nil,
	}).SetupWithManager(mgr)
	Expect(err).NotTo(HaveOccurred())

	go func() {
		defer GinkgoRecover()
		Expect(mgr.Start(ctx)).To(Succeed())
	}()

	cluster = &v1.ManagedCluster{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test-cluster",
		},
		Spec: v1.ManagedClusterSpec{
			// fill in spec
		},
	}
	err = k8sClient.Create(ctx, cluster)
	Expect(err).NotTo(HaveOccurred())
})

var _ = AfterSuite(func() {
	// Teardown Prometheus and CertManager after the suite if not skipped and if they were not already installed
	/*if !skipPrometheusInstall && !isPrometheusOperatorAlreadyInstalled {
		_, _ = fmt.Fprintf(GinkgoWriter, "Uninstalling Prometheus Operator...\n")
		utils.UninstallPrometheusOperator()
	}
	if !skipCertManagerInstall && !isCertManagerAlreadyInstalled {
		_, _ = fmt.Fprintf(GinkgoWriter, "Uninstalling CertManager...\n")
		utils.UninstallCertManager()
	}*/
	// Delete the resource and check cleanup
	err := k8sClient.Delete(ctx, cluster)
	Expect(err).NotTo(HaveOccurred())
	Expect(err).NotTo(HaveOccurred())
	Eventually(func() bool {
		err := k8sClient.Get(ctx, client.ObjectKey{Name: "test-cluster", Namespace: "default"}, cluster)
		return k8serrors.IsNotFound(err)
	}, time.Second*10, time.Millisecond*250).Should(BeTrue())
	cancel()
})
