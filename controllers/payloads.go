package controllers

import (
	"context"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	clusterv1 "open-cluster-management.io/api/cluster/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	ManagedClusterFinalizer  = "mtv-integrations.open-cluster-management.io/resource-cleanup"
	LabelCNVOperatorInstall  = "acm/cnv-operator-install"
	MTVIntegrationsNamespace = "mtv-integrations"
)

var TokenWaitDuration = 4 * time.Second

var (
	ClusterPermissionsGVR     = generateGVR("rbac.open-cluster-management.io", "v1alpha1", "clusterpermissions")
	ManagedServiceAccountsGVR = generateGVR(
		"authentication.open-cluster-management.io",
		"v1beta1",
		"managedserviceaccounts")
)
var (
	ProvidersGVR      = generateGVR("forklift.konveyor.io", "v1beta1", "providers")
	ProviderSecretGVR = generateGVR("", "v1", "secrets")
)

func providerPayload(managedCluster *clusterv1.ManagedCluster) map[string]interface{} {
	managedClusterMTV := managedCluster.Name + "-mtv"

	// Safely get the URL from ManagedClusterClientConfigs
	var clusterURL string
	if len(managedCluster.Spec.ManagedClusterClientConfigs) > 0 {
		clusterURL = managedCluster.Spec.ManagedClusterClientConfigs[0].URL
	}

	return map[string]interface{}{
		"apiVersion": "forklift.konveyor.io/v1beta1",
		"kind":       "Provider",
		"metadata": map[string]interface{}{
			"name":      managedClusterMTV,
			"namespace": MTVIntegrationsNamespace,
		},
		"spec": map[string]interface{}{
			"type": "openshift",
			"url":  clusterURL,
			"secret": map[string]interface{}{
				"name":      managedClusterMTV,
				"namespace": MTVIntegrationsNamespace,
			},
		},
	}
}

func clusterPermissionPayload(managedCluster *clusterv1.ManagedCluster, msaaNamespace string) map[string]interface{} {
	managedClusterMTV := managedCluster.Name + "-mtv"
	return map[string]interface{}{
		"apiVersion": "rbac.open-cluster-management.io/v1alpha1",
		"kind":       "ClusterPermission",
		"metadata": map[string]interface{}{
			"name":      managedClusterMTV,
			"namespace": managedCluster.Name,
		},
		"spec": map[string]interface{}{
			"clusterRoleBinding": map[string]interface{}{
				"subject": map[string]interface{}{
					"kind":      "ServiceAccount",
					"name":      managedClusterMTV,
					"namespace": msaaNamespace, // The ServiceAccount is created here on the ManagedCluster
				},
				"roleRef": map[string]interface{}{ // This is the documented RBAC for the MTV Provider
					"kind":     "ClusterRole",
					"name":     "cluster-admin",
					"apiGroup": "rbac.authorization.k8s.io",
				},
			},
		},
	}
}

// findMsaaDeploymentNs returns the namespace where the managed-serviceaccount-addon-agent
// deployment runs. Shared by ManagedClusterReconciler and ClusterRecommendationService.
func findMsaaDeploymentNs(ctx context.Context, c client.Client) (string, error) {
	var depList appsv1.DeploymentList
	if err := c.List(ctx, &depList); err != nil {
		return "", err
	}
	for _, d := range depList.Items {
		if d.Name == "managed-serviceaccount-addon-agent" {
			return d.Namespace, nil
		}
	}
	return "", errors.NewNotFound(
		schema.GroupResource{Group: "apps", Resource: "deployments"},
		"managed-serviceaccount-addon-agent",
	)
}

func generateGVR(group string, version string, resource string) schema.GroupVersionResource {
	return schema.GroupVersionResource{
		Group:    group,
		Version:  version,
		Resource: resource,
	}
}
