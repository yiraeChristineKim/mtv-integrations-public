package controllers

import (
	"k8s.io/apimachinery/pkg/runtime/schema"
	clusterv1 "open-cluster-management.io/api/cluster/v1"
)

func providerPayload(managedCluster *clusterv1.ManagedCluster) map[string]interface{} {
	managedClusterMTV := managedCluster.Name + "-mtv"
	return map[string]interface{}{
		"apiVersion": "forklift.konveyor.io/v1beta1",
		"kind":       "Provider",
		"metadata": map[string]interface{}{
			"name":      managedClusterMTV,
			"namespace": managedCluster.Name,
		},
		"spec": map[string]interface{}{
			"type": "openshift",
			"url":  managedCluster.Spec.ManagedClusterClientConfigs[0].URL,
			"secret": map[string]interface{}{
				"name":      managedClusterMTV,
				"namespace": managedCluster.Name,
			},
		},
	}
}

func clusterPermissionPayload(managedCluster *clusterv1.ManagedCluster) map[string]interface{} {
	managedClusterMTV := managedCluster.Name + "-mtv"
	return map[string]interface{}{
		"apiVersion": "rbac.open-cluster-management.io/v1alpha1",
		"kind":       "ClusterPermission",
		"metadata": map[string]interface{}{
			"name":      managedClusterMTV,
			"namespace": managedCluster.Name,
			"ownerReferences": []map[string]interface{}{
				map[string]interface{}{
					"kind":       "ManagedCluster",
					"name":       managedCluster.Name,
					"uid":        managedCluster.UID,
					"apiVersion": "cluster.open-cluster-management.io/v1",
				},
			},
		},
		"spec": map[string]interface{}{
			"clusterRoleBinding": map[string]interface{}{
				"subject": map[string]interface{}{
					"kind":      "ServiceAccount",
					"name":      managedClusterMTV,
					"namespace": "open-cluster-management-agent-addon", // The ServiceAccount is created here on the ManagedCluster
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

func generateGVR(group string, version string, resource string) schema.GroupVersionResource {

	return schema.GroupVersionResource{
		Group:    group,
		Version:  version,
		Resource: resource,
	}
}
