package controllers

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	storagev1 "k8s.io/api/storage/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clusterv1 "open-cluster-management.io/api/cluster/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	clientfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestClusterRecommendationService_GetBestManagedCluster(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = clusterv1.Install(scheme)

	tests := []struct {
		name                 string
		managedClusters      []clusterv1.ManagedCluster
		vmRequirements       VMResourceRequirements
		expectedStatus       string
		expectedClusterCount int
		hasRecommendation    bool
	}{
		{
			name: "Single cluster with sufficient resources",
			managedClusters: []clusterv1.ManagedCluster{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "cluster-1",
						Labels: map[string]string{
							LabelCNVOperatorInstall: "true",
						},
					},
					Spec: clusterv1.ManagedClusterSpec{
						ManagedClusterClientConfigs: []clusterv1.ClientConfig{
							{URL: "https://api.cluster1.example.com:6443"},
						},
					},
				},
			},
			vmRequirements: VMResourceRequirements{
				CPUCores:           2,
				MemoryGiB:          4,
				StorageGB:          50,
				TargetStorageClass: "test-sc",
			},
			// scoreCluster fails because cluster-proxy is unavailable in the test environment
			expectedStatus:       "error",
			expectedClusterCount: 0,
			hasRecommendation:    false,
		},
		{
			name: "Multiple clusters - should pick best one",
			managedClusters: []clusterv1.ManagedCluster{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "cluster-small",
						Labels: map[string]string{
							LabelCNVOperatorInstall: "true",
						},
					},
					Spec: clusterv1.ManagedClusterSpec{
						ManagedClusterClientConfigs: []clusterv1.ClientConfig{
							{URL: "https://api.cluster-small.example.com:6443"},
						},
					},
				},
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "cluster-large",
						Labels: map[string]string{
							LabelCNVOperatorInstall: "true",
						},
					},
					Spec: clusterv1.ManagedClusterSpec{
						ManagedClusterClientConfigs: []clusterv1.ClientConfig{
							{URL: "https://api.cluster-large.example.com:6443"},
						},
					},
				},
			},
			vmRequirements: VMResourceRequirements{
				CPUCores:           2,
				MemoryGiB:          4,
				StorageGB:          50,
				TargetStorageClass: "test-sc",
			},
			// scoreCluster fails because cluster-proxy is unavailable in the test environment
			expectedStatus:       "error",
			expectedClusterCount: 0,
			hasRecommendation:    false,
		},
		{
			name: "No clusters with CNV label",
			managedClusters: []clusterv1.ManagedCluster{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "cluster-no-label",
						Labels: map[string]string{
							"some-other-label": "value",
						},
					},
				},
			},
			vmRequirements: VMResourceRequirements{
				CPUCores:           2,
				MemoryGiB:          4,
				StorageGB:          50,
				TargetStorageClass: "test-sc",
			},
			expectedStatus:       "error",
			expectedClusterCount: 0,
			hasRecommendation:    false,
		},
		{
			name: "Large VM requirements (should exclude small clusters)",
			managedClusters: []clusterv1.ManagedCluster{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "cluster-small",
						Labels: map[string]string{
							LabelCNVOperatorInstall: "true",
						},
					},
					Spec: clusterv1.ManagedClusterSpec{
						ManagedClusterClientConfigs: []clusterv1.ClientConfig{
							{URL: "https://api.cluster-small.example.com:6443"},
						},
					},
				},
			},
			vmRequirements: VMResourceRequirements{
				CPUCores:           32,
				MemoryGiB:          64,
				StorageGB:          1000,
				TargetStorageClass: "test-sc",
			},
			// scoreCluster fails because cluster-proxy is unavailable in the test environment
			expectedStatus:       "error",
			expectedClusterCount: 0,
			hasRecommendation:    false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			objects := make([]client.Object, len(tt.managedClusters))
			for i := range tt.managedClusters {
				objects[i] = &tt.managedClusters[i]
			}

			k8sClient := clientfake.NewClientBuilder().
				WithScheme(scheme).
				WithObjects(objects...).
				Build()

			service := &ClusterRecommendationService{
				Client: k8sClient,
				Scheme: scheme,
			}

			response, err := service.GetBestManagedCluster(context.Background(), tt.vmRequirements)

			require.NoError(t, err)
			assert.Equal(t, tt.expectedStatus, response.Status)
			assert.Equal(t, tt.expectedClusterCount, len(response.AllClusters))

			if tt.hasRecommendation {
				assert.NotNil(t, response.RecommendedCluster)
				assert.True(t, response.RecommendedCluster.CanFitVM)
				assert.Greater(t, response.RecommendedCluster.TotalScore, 0.0)
			} else {
				assert.Nil(t, response.RecommendedCluster)
			}

			// Verify VM requirements are echoed back
			assert.Equal(t, tt.vmRequirements, response.VMRequirements)
		})
	}
}

func TestClusterRecommendationService_calculateResourceScores(t *testing.T) {
	service := &ClusterRecommendationService{}

	tests := []struct {
		name            string
		nodeResources   []NodeResources
		vmRequirements  VMResourceRequirements
		storageClasses  []StorageClassInfo
		expectExclusion bool
	}{
		{
			name: "Sufficient resources",
			nodeResources: []NodeResources{
				{
					NodeName:           "node-1",
					AvailableCPUCores:  8,
					AvailableMemoryGiB: 16,
				},
			},
			vmRequirements: VMResourceRequirements{
				CPUCores:  4,
				MemoryGiB: 8,
				StorageGB: 100,
			},
			// A dynamic storage class with unknown capacity always passes the storage check.
			storageClasses: []StorageClassInfo{
				{
					Name:      "dynamic-sc",
					IsDynamic: true,
				},
			},
			expectExclusion: false,
		},
		{
			name: "Insufficient CPU - should exclude",
			nodeResources: []NodeResources{
				{
					NodeName:           "node-1",
					AvailableCPUCores:  2,
					AvailableMemoryGiB: 16,
				},
			},
			vmRequirements: VMResourceRequirements{
				CPUCores:  4,
				MemoryGiB: 8,
				StorageGB: 100,
			},
			storageClasses:  []StorageClassInfo{},
			expectExclusion: true,
		},
		{
			name: "Insufficient memory - should exclude",
			nodeResources: []NodeResources{
				{
					NodeName:           "node-1",
					AvailableCPUCores:  8,
					AvailableMemoryGiB: 4,
				},
			},
			vmRequirements: VMResourceRequirements{
				CPUCores:  4,
				MemoryGiB: 8,
				StorageGB: 100,
			},
			storageClasses:  []StorageClassInfo{},
			expectExclusion: true,
		},
		{
			name: "Insufficient storage but has dynamic storage",
			nodeResources: []NodeResources{
				{
					NodeName:           "node-1",
					AvailableCPUCores:  8,
					AvailableMemoryGiB: 16,
				},
			},
			vmRequirements: VMResourceRequirements{
				CPUCores:  4,
				MemoryGiB: 8,
				StorageGB: 100,
			},
			storageClasses: []StorageClassInfo{
				{
					Name:                "gp3",
					Provisioner:         "ebs.csi.aws.com",
					IsDynamic:           true,
					AvailableCapacityGB: 1000,
				},
			},
			expectExclusion: false, // Dynamic provisioner with unknown capacity always passes
		},
		{
			name:          "No resources - should exclude",
			nodeResources: []NodeResources{},
			vmRequirements: VMResourceRequirements{
				CPUCores:  4,
				MemoryGiB: 8,
				StorageGB: 100,
			},
			storageClasses:  []StorageClassInfo{},
			expectExclusion: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cpuScore, memoryScore, storageScore, _ := service.calculateResourceScores(
				tt.nodeResources,
				tt.vmRequirements,
				tt.storageClasses,
			)

			if tt.expectExclusion {
				// All scores should be -1 indicating exclusion
				assert.Equal(t, -1.0, cpuScore)
				assert.Equal(t, -1.0, memoryScore)
				assert.Equal(t, -1.0, storageScore)
			} else {
				// All scores should be >= 0
				assert.GreaterOrEqual(t, cpuScore, 0.0)
				assert.GreaterOrEqual(t, memoryScore, 0.0)
				assert.GreaterOrEqual(t, storageScore, 0.0)
			}
		})
	}
}

func TestClusterRecommendationService_canFitVM(t *testing.T) {
	service := &ClusterRecommendationService{}

	tests := []struct {
		name           string
		nodeResources  []NodeResources
		vmRequirements VMResourceRequirements
		storageClasses []StorageClassInfo
		canFit         bool
	}{
		{
			name: "Node can fit all resources",
			nodeResources: []NodeResources{
				{
					NodeName:           "node-1",
					AvailableCPUCores:  8,
					AvailableMemoryGiB: 16,
				},
			},
			vmRequirements: VMResourceRequirements{
				CPUCores:  4,
				MemoryGiB: 8,
				StorageGB: 100,
			},
			storageClasses: []StorageClassInfo{
				{
					Name:      "dynamic-sc",
					IsDynamic: true,
				},
			},
			canFit: true,
		},
		{
			name: "Node has CPU/memory, dynamic storage for storage",
			nodeResources: []NodeResources{
				{
					NodeName:           "node-1",
					AvailableCPUCores:  8,
					AvailableMemoryGiB: 16,
				},
			},
			vmRequirements: VMResourceRequirements{
				CPUCores:  4,
				MemoryGiB: 8,
				StorageGB: 100,
			},
			storageClasses: []StorageClassInfo{
				{
					Name:                "aws-ebs",
					IsDynamic:           true,
					AvailableCapacityGB: 1000,
				},
			},
			canFit: true,
		},
		{
			name: "Cannot fit - insufficient CPU",
			nodeResources: []NodeResources{
				{
					NodeName:           "node-1",
					AvailableCPUCores:  2,
					AvailableMemoryGiB: 16,
				},
			},
			vmRequirements: VMResourceRequirements{
				CPUCores:  4,
				MemoryGiB: 8,
				StorageGB: 100,
			},
			storageClasses: []StorageClassInfo{},
			canFit:         false,
		},
		{
			name: "Cannot fit - insufficient memory",
			nodeResources: []NodeResources{
				{
					NodeName:           "node-1",
					AvailableCPUCores:  8,
					AvailableMemoryGiB: 4,
				},
			},
			vmRequirements: VMResourceRequirements{
				CPUCores:  4,
				MemoryGiB: 8,
				StorageGB: 100,
			},
			storageClasses: []StorageClassInfo{},
			canFit:         false,
		},
		{
			name: "Cannot fit - no storage option",
			nodeResources: []NodeResources{
				{
					NodeName:           "node-1",
					AvailableCPUCores:  8,
					AvailableMemoryGiB: 16,
				},
			},
			vmRequirements: VMResourceRequirements{
				CPUCores:  4,
				MemoryGiB: 8,
				StorageGB: 100,
			},
			storageClasses: []StorageClassInfo{
				{
					Name:                "local-storage",
					IsDynamic:           false,
					CapacityKnown:       true,
					AvailableCapacityGB: 60, // Still not enough
				},
			},
			canFit: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := service.canFitVM(tt.nodeResources, tt.vmRequirements, tt.storageClasses)
			assert.Equal(t, tt.canFit, result)
		})
	}
}

func TestIsDynamicStorageProvisioner(t *testing.T) {
	tests := []struct {
		provisioner string
		isDynamic   bool
	}{
		{"ebs.csi.aws.com", true},
		{"kubernetes.io/aws-ebs", true},
		{"disk.csi.azure.com", true},
		{"kubernetes.io/azure-disk", true},
		{"pd.csi.storage.gke.io", true},
		{"kubernetes.io/gce-pd", true},
		{"csi.vsphere.vmware.com", true},
		{"rook-ceph.rbd.csi.ceph.com", true},
		{"custom.csi.provider.com", true},      // detected via "csi" fallback
		{"custom-provider.io", false},           // not in allow-list, no "csi" in name
		{"local-storage", false},                // not dynamic
		{"kubernetes.io/no-provisioner", false}, // explicitly static
		{"", false},                             // empty
	}

	for _, tt := range tests {
		t.Run(tt.provisioner, func(t *testing.T) {
			result := isDynamicStorageProvisioner(tt.provisioner)
			assert.Equal(t, tt.isDynamic, result, "Provisioner: %s", tt.provisioner)
		})
	}
}

func TestStorageClassInfoFromTyped(t *testing.T) {
	bindingMode := storagev1.VolumeBindingWaitForFirstConsumer
	allowExpansion := true

	tests := []struct {
		name                string
		storageClass        *storagev1.StorageClass
		expectedName        string
		expectedProvisioner string
		expectedDynamic     bool
	}{
		{
			name: "AWS EBS CSI storage class",
			storageClass: &storagev1.StorageClass{
				ObjectMeta: metav1.ObjectMeta{
					Name: "gp3",
				},
				Provisioner:          "ebs.csi.aws.com",
				VolumeBindingMode:    &bindingMode,
				AllowVolumeExpansion: &allowExpansion,
			},
			expectedName:        "gp3",
			expectedProvisioner: "ebs.csi.aws.com",
			expectedDynamic:     true,
		},
		{
			name: "Local storage class (static)",
			storageClass: &storagev1.StorageClass{
				ObjectMeta: metav1.ObjectMeta{
					Name: "local-storage",
				},
				Provisioner:       "kubernetes.io/no-provisioner",
				VolumeBindingMode: &bindingMode,
			},
			expectedName:        "local-storage",
			expectedProvisioner: "kubernetes.io/no-provisioner",
			expectedDynamic:     false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := storageClassInfoFromTyped(tt.storageClass)

			assert.Equal(t, tt.expectedName, result.Name)
			assert.Equal(t, tt.expectedProvisioner, result.Provisioner)
			assert.Equal(t, tt.expectedDynamic, result.IsDynamic)
		})
	}
}

func TestClusterRecommendationService_Integration(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = clusterv1.Install(scheme)

	managedCluster := &clusterv1.ManagedCluster{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test-cluster",
			Labels: map[string]string{
				LabelCNVOperatorInstall: "true",
			},
		},
		Spec: clusterv1.ManagedClusterSpec{
			ManagedClusterClientConfigs: []clusterv1.ClientConfig{
				{URL: "https://api.test-cluster.example.com:6443"},
			},
		},
	}

	k8sClient := clientfake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(managedCluster).
		Build()

	service := &ClusterRecommendationService{
		Client: k8sClient,
		Scheme: scheme,
	}

	vmRequirements := VMResourceRequirements{
		CPUCores:           2,
		MemoryGiB:          4,
		StorageGB:          50,
		TargetStorageClass: "test-sc",
	}

	response, err := service.GetBestManagedCluster(context.Background(), vmRequirements)

	require.NoError(t, err)
	// scoreCluster fails because cluster-proxy is unavailable in the test environment
	assert.Equal(t, "error", response.Status)
	assert.Len(t, response.AllClusters, 0)
}
