package e2e

import (
	"context"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"
)

var clientHub kubernetes.Interface
var dynamicClientHub dynamic.Interface

const kubeconfigHub = "../../kubeconfig_e2e"

func TestE2e(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "MTV integrations e2e Suite")
}

var _ = BeforeSuite(func() {
	By("Setup the controller-runtime logger")
	ctrllog.SetLogger(GinkgoLogr)

	config, err := clientcmd.BuildConfigFromFlags("", kubeconfigHub)
	if err != nil {
		Expect(err).NotTo(HaveOccurred())
	}

	clientHub, err = kubernetes.NewForConfig(config)
	Expect(err).NotTo(HaveOccurred())

	dynamicClientHub, err = dynamic.NewForConfig(config)
	Expect(err).NotTo(HaveOccurred())
})

// clusterProxyAvailable returns true when the cluster-proxy-addon-user Service
// exists in the multicluster-engine namespace.  This is satisfied by a full MCE
// installation or by running "make prepare-e2e-ocm" which installs OCM via
// clusteradm and creates the multicluster-engine namespace alias.
// Tests that require actual cluster scoring use this to skip gracefully on
// plain kind clusters that have only CRDs installed.
func clusterProxyAvailable() bool {
	_, err := clientHub.CoreV1().Services("multicluster-engine").Get(
		context.TODO(), "cluster-proxy-addon-user", metav1.GetOptions{})
	return err == nil
}
