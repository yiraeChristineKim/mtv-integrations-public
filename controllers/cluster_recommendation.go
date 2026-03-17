package controllers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	metricsv1beta1 "k8s.io/metrics/pkg/apis/metrics/v1beta1"
	metricsv "k8s.io/metrics/pkg/client/clientset/versioned"
	clusterv1 "open-cluster-management.io/api/cluster/v1"
	authv1beta1 "open-cluster-management.io/managed-serviceaccount/apis/authentication/v1beta1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

const (
	// clusterProxyServiceName and namespace identify the cluster-proxy user-server.
	clusterProxyServiceName      = "cluster-proxy-addon-user"
	clusterProxyServiceNamespace = "multicluster-engine"

	// openshiftServiceCAConfigMap is automatically injected into every namespace
	// by the OpenShift service CA controller. It contains the CA that signs
	// cluster-proxy-addon-user's TLS certificate.
	openshiftServiceCAConfigMap = "openshift-service-ca.crt"
	openshiftServiceCAKey       = "service-ca.crt"

	// clusterRecommendMSAName is the ManagedServiceAccount created per cluster
	// to authenticate cluster-proxy calls.
	clusterRecommendMSAName = "cluster-recommendation"

	// devModeEnvVar enables dev mode when set to "true".
	// In dev mode the cluster-proxy Route is used instead of the in-cluster Service,
	// allowing the operator to be run locally outside the cluster.
	devModeEnvVar = "DEV_MODE"
)

var clusterProxyRouteGVR = schema.GroupVersionResource{
	Group:    "route.openshift.io",
	Version:  "v1",
	Resource: "routes",
}

var (
	// virtualMachineInstanceGVR is used instead of VirtualMachine because the VMI
	// holds the actual runtime values — CPU topology and memory are already resolved
	// (no instancetype indirection), and status.volumeStatus carries the real PVC
	// names and sizes for every mounted disk.
	virtualMachineInstanceGVR = schema.GroupVersionResource{Group: "kubevirt.io", Version: "v1", Resource: "virtualmachineinstances"}
)

// errMSATokenNotReady is returned when the ManagedServiceAccount exists but OCM
// has not yet provisioned its token secret. This is a transient condition —
// callers should log a warning and skip the cluster rather than treating it as
// a hard error.
var errMSATokenNotReady = errors.New("MSA token not yet available")

// getClusterProxyHost returns the host (and port where needed) for cluster-proxy.
// In dev mode (DEV_MODE=true) it reads the OpenShift Route spec.host so the
// operator can reach the proxy from outside the cluster.
// In normal mode it reads the Service and returns "svc-name.ns.svc:<port>".
func (s *ClusterRecommendationService) getClusterProxyHost(ctx context.Context) (string, error) {
	if os.Getenv(devModeEnvVar) == "true" {
		return s.getClusterProxyHostFromRoute(ctx)
	}
	return s.getClusterProxyHostFromService(ctx)
}

// getClusterProxyHostFromService reads the Service port and returns "svc.ns.svc:<port>".
func (s *ClusterRecommendationService) getClusterProxyHostFromService(ctx context.Context) (string, error) {
	svc := &corev1.Service{}
	if err := s.Get(ctx, types.NamespacedName{
		Name:      clusterProxyServiceName,
		Namespace: clusterProxyServiceNamespace,
	}, svc); err != nil {
		return "", fmt.Errorf("failed to get cluster-proxy service %q in namespace %q: %w",
			clusterProxyServiceName, clusterProxyServiceNamespace, err)
	}
	if len(svc.Spec.Ports) == 0 {
		return "", fmt.Errorf("cluster-proxy service %q has no ports defined", clusterProxyServiceName)
	}
	return fmt.Sprintf("%s.%s.svc:%d",
		clusterProxyServiceName,
		clusterProxyServiceNamespace,
		svc.Spec.Ports[0].Port), nil
}

// getClusterProxyHostFromRoute reads the OpenShift Route spec.host.
// Used in dev mode so the operator running locally can reach the proxy via the
// public route instead of the internal service DNS.
func (s *ClusterRecommendationService) getClusterProxyHostFromRoute(ctx context.Context) (string, error) {
	route, err := s.DynamicClient.Resource(clusterProxyRouteGVR).
		Namespace(clusterProxyServiceNamespace).
		Get(ctx, clusterProxyServiceName, metav1.GetOptions{})
	if err != nil {
		return "", fmt.Errorf("failed to get cluster-proxy route %q in namespace %q: %w",
			clusterProxyServiceName, clusterProxyServiceNamespace, err)
	}
	host, found, err := unstructured.NestedString(route.Object, "spec", "host")
	if err != nil || !found || host == "" {
		return "", fmt.Errorf("cluster-proxy route %q has no spec.host", clusterProxyServiceName)
	}
	// Routes are HTTPS on port 443 — no port suffix needed.
	return host, nil
}

// ClusterRecommendationService provides cluster recommendation functionality
type ClusterRecommendationService struct {
	client.Client
	Scheme        *runtime.Scheme
	RestConfig    *rest.Config
	DynamicClient dynamic.Interface
}

// VMLocationRequest identifies a VM on a managed cluster to inspect.
// The handler fetches the VM spec via cluster-proxy to derive CPU, memory,
// and per-volume storage requirements automatically.
type VMLocationRequest struct {
	ClusterName string `json:"cluster"`
	VMName      string `json:"vmName"`
	VMNamespace string `json:"vmNamespace"`
}

// VolumeRequirement holds per-volume storage requirements extracted from a
// DataVolume or PersistentVolumeClaim in the VM spec.
// StorageClassName is read directly from the volume's spec — the caller does
// not need to supply it.
type VolumeRequirement struct {
	VolumeName       string `json:"volumeName"`
	StorageClassName string `json:"storageClassName"`
	SizeGB           int64  `json:"sizeGB"`
}

// VMResourceRequirements represents the resource requirements derived from a
// VM spec. CPU and memory are totals; storage is expressed per-volume so that
// each volume's storage class can be checked independently on the target cluster.
type VMResourceRequirements struct {
	CPUCores  int64               `json:"cpuCores"`
	MemoryGiB int64               `json:"memoryGiB"`
	Volumes   []VolumeRequirement `json:"volumes"`
}

// NodeResources represents free resources on a node:
//
//	free = Allocatable − actual_usage (from metrics-server)
type NodeResources struct {
	NodeName           string `json:"nodeName"`
	AvailableCPUCores  int64  `json:"availableCpuCores"`
	AvailableMemoryGiB int64  `json:"availableMemoryGiB"`
}

// StorageClassInfo represents storage class capacity information
type StorageClassInfo struct {
	Name                 string `json:"name"`
	Provisioner          string `json:"provisioner"`
	VolumeBindingMode    string `json:"volumeBindingMode"`
	AllowVolumeExpansion bool   `json:"allowVolumeExpansion"`
	IsDynamic            bool   `json:"isDynamic"`
	// CapacityKnown is true only when AvailableCapacityGB is real measured data:
	//   Ceph      → status.ceph.capacity.bytesAvailable from CephCluster CR
	//   Static PVs → sum of capacity of Available PVs
	// For other dynamic provisioners (NFS, cloud CSI, etc.) the backend pool
	// cannot be introspected, so CapacityKnown stays false and AvailableCapacityGB is 0.
	CapacityKnown       bool    `json:"capacityKnown"`
	AvailableCapacityGB int64   `json:"availableCapacityGB"`
	// AvailablePVSizes holds the individual size (GB) of each Available PV for
	// static provisioners. Used for 1:1 volume→PV matching — a DataVolume needs
	// its own PV, so summing all PV capacity is not sufficient.
	AvailablePVSizes    []int64 `json:"availablePvSizes,omitempty"`
}

// ClusterScore represents the score for a managed cluster
type ClusterScore struct {
	ClusterName      string             `json:"clusterName"`
	ClusterURL       string             `json:"clusterUrl"`
	TotalScore       float64            `json:"totalScore"`
	CPUScore         float64            `json:"cpuScore"`
	MemoryScore      float64            `json:"memoryScore"`
	StorageScore     float64            `json:"storageScore"`
	SchedulableNodes int                `json:"schedulableNodes"`
	// BestNode is the schedulable node with the highest available CPU and memory.
	// This is the node KubeVirt would most likely target for the VM.
	BestNode         *NodeResources     `json:"bestNode,omitempty"`
	NodeResources    []NodeResources    `json:"nodeResources"`
	StorageClasses   []StorageClassInfo `json:"storageClasses"`
	CanFitVM         bool               `json:"canFitVm"`
}

// ClusterRecommendationResponse represents the API response
type ClusterRecommendationResponse struct {
	RecommendedCluster *ClusterScore          `json:"recommendedCluster"`
	AllClusters        []ClusterScore         `json:"allClusters"`
	VMRequirements     VMResourceRequirements `json:"vmRequirements"`
	// Warnings contains non-fatal messages from VM inspection, e.g. volumes
	// whose size could not be determined (hostDisk, unknown types).
	Warnings []string `json:"warnings,omitempty"`
	Status   string   `json:"status"`
	Message  string   `json:"message,omitempty"`
}

// GetBestManagedCluster finds the best managed cluster for VM migration
func (s *ClusterRecommendationService) GetBestManagedCluster(
	ctx context.Context,
	vmRequirements VMResourceRequirements,
) (*ClusterRecommendationResponse, error) {
	log := log.FromContext(ctx)
	log.Info("Finding best managed cluster for VM migration", "requirements", vmRequirements)

	// Get all managed clusters with CNV operator installed
	managedClusters, err := s.getEligibleManagedClusters(ctx)
	if err != nil {
		return &ClusterRecommendationResponse{
			Status:  "error",
			Message: fmt.Sprintf("Failed to get eligible managed clusters: %v", err),
		}, err
	}

	if len(managedClusters) == 0 {
		return &ClusterRecommendationResponse{
			Status:         "error",
			Message:        "No managed clusters found with acm/cnv-operator-install=true label",
			VMRequirements: vmRequirements,
		}, nil
	}

	log.Info("Found eligible managed clusters", "count", len(managedClusters))

	// Score each cluster in parallel so that a hung cluster-proxy call to a
	// disconnected cluster does not block scoring of healthy clusters.
	type scoreResult struct {
		score *ClusterScore
		err   error
	}
	resultCh := make(chan scoreResult, len(managedClusters))

	for _, cluster := range managedClusters {
		cluster := cluster // capture loop variable
		go func() {
			// Give each cluster a bounded window so that a completely
			// unreachable managed cluster does not hold up the response.
			clusterCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
			defer cancel()
			score, err := s.scoreCluster(clusterCtx, &cluster, vmRequirements)
			resultCh <- scoreResult{score: score, err: err}
		}()
	}

	var clusterScores []ClusterScore
	var excludedClusters []string

	for range managedClusters {
		res := <-resultCh
		if res.err != nil {
			if errors.Is(res.err, errMSATokenNotReady) {
				// Token provisioning is in progress — warn and skip; the next
				// request will retry once OCM has populated the secret.
				log.Info("Skipping cluster: MSA token not yet ready (transient)", "reason", res.err.Error())
			} else {
				log.Error(res.err, "Failed to score cluster")
			}
			continue
		}
		if res.score.TotalScore == 0 && !res.score.CanFitVM {
			excludedClusters = append(excludedClusters, res.score.ClusterName)
		}
		clusterScores = append(clusterScores, *res.score)
	}

	if len(clusterScores) == 0 {
		return &ClusterRecommendationResponse{
			Status:         "error", 
			Message:        "No clusters could be scored successfully",
			VMRequirements: vmRequirements,
		}, nil
	}

	// Sort clusters by total score (descending)
	sort.Slice(clusterScores, func(i, j int) bool {
		return clusterScores[i].TotalScore > clusterScores[j].TotalScore
	})

	// Find the first cluster that can fit the VM (score > 0)
	var recommendedCluster *ClusterScore
	for i := range clusterScores {
		if clusterScores[i].CanFitVM && clusterScores[i].TotalScore > 0 {
			recommendedCluster = &clusterScores[i]
			break
		}
	}

	// Log excluded clusters for transparency
	if len(excludedClusters) > 0 {
		log.Info("Clusters excluded due to insufficient resources",
			"excludedClusters", excludedClusters,
			"requiredCPU", vmRequirements.CPUCores,
			"requiredMemory", vmRequirements.MemoryGiB,
			"volumes", len(vmRequirements.Volumes))
	}

	response := &ClusterRecommendationResponse{
		AllClusters:    clusterScores,
		VMRequirements: vmRequirements,
		Status:         "success",
	}

	if recommendedCluster != nil {
		response.RecommendedCluster = recommendedCluster
		log.Info("Found recommended cluster", "cluster", recommendedCluster.ClusterName, "score", recommendedCluster.TotalScore)
	} else {
		response.Status = "warning"
		response.Message = "No cluster has sufficient resources to fit the VM"
		log.Info("No cluster found that can fit the VM requirements")
	}

	return response, nil
}

// getEligibleManagedClusters returns all managed clusters with CNV operator installed
func (s *ClusterRecommendationService) getEligibleManagedClusters(ctx context.Context) ([]clusterv1.ManagedCluster, error) {
	var managedClusters clusterv1.ManagedClusterList

	labelSelector := labels.SelectorFromSet(labels.Set{
		LabelCNVOperatorInstall: "true",
	})

	err := s.List(ctx, &managedClusters, &client.ListOptions{
		LabelSelector: labelSelector,
	})

	if err != nil {
		return nil, fmt.Errorf("failed to list managed clusters: %w", err)
	}

	return managedClusters.Items, nil
}

// scoreCluster calculates a score for a managed cluster based on available resources
func (s *ClusterRecommendationService) scoreCluster(
	ctx context.Context,
	cluster *clusterv1.ManagedCluster,
	vmRequirements VMResourceRequirements,
) (*ClusterScore, error) {
	log := log.FromContext(ctx)
	
	// Get cluster URL
	var clusterURL string
	if len(cluster.Spec.ManagedClusterClientConfigs) > 0 {
		clusterURL = cluster.Spec.ManagedClusterClientConfigs[0].URL
	}

	clusterScore := &ClusterScore{
		ClusterName: cluster.Name,
		ClusterURL:  clusterURL,
	}

	// Get free CPU/memory for each schedulable node.
	nodeResources, err := s.getSchedulableNodeResources(ctx, cluster.Name)
	if err != nil {
		return clusterScore, fmt.Errorf("failed to get schedulable node resources: %w", err)
	}

	if len(nodeResources) == 0 {
		log.Info("No schedulable nodes found", "cluster", cluster.Name)
		return clusterScore, nil
	}

	clusterScore.SchedulableNodes = len(nodeResources)

	clusterScore.NodeResources = nodeResources
	clusterScore.BestNode = bestNode(nodeResources)

	// Fetch each unique storage class referenced by the VM's volumes.
	scNames := uniqueStorageClassNames(vmRequirements.Volumes)
	storageClasses, err := s.getStorageClasses(ctx, cluster.Name, scNames)
	if err != nil {
		return clusterScore, fmt.Errorf("failed to fetch storage classes on cluster %q: %w", cluster.Name, err)
	}
	clusterScore.StorageClasses = storageClasses

	// Calculate scores
	var storageKnown bool
	clusterScore.CPUScore, clusterScore.MemoryScore, clusterScore.StorageScore, storageKnown = s.calculateResourceScores(nodeResources, vmRequirements, storageClasses)

	// Scores of -1 indicate the cluster cannot satisfy the minimum requirements.
	if clusterScore.CPUScore < 0 || clusterScore.MemoryScore < 0 || clusterScore.StorageScore < 0 {
		clusterScore.CPUScore = 0
		clusterScore.MemoryScore = 0
		clusterScore.StorageScore = 0
		clusterScore.TotalScore = 0
		clusterScore.CanFitVM = false
		log.Info("Cluster excluded due to insufficient resources",
			"cluster", cluster.Name,
			"requiredCPU", vmRequirements.CPUCores,
			"requiredMemory", vmRequirements.MemoryGiB,
			"volumes", len(vmRequirements.Volumes))
		return clusterScore, nil
	}

	// When storage capacity is known (Ceph), include it in the total score so that
	// clusters with more free Ceph storage rank higher.
	// When capacity is unknown (NFS, cloud CSI, etc.) storage is not a ranking
	// factor — TotalScore is the average of CPU and memory only.
	if storageKnown {
		clusterScore.TotalScore = (clusterScore.CPUScore + clusterScore.MemoryScore + clusterScore.StorageScore) / 3
	} else {
		clusterScore.TotalScore = (clusterScore.CPUScore + clusterScore.MemoryScore) / 2
	}

	// Check if VM can fit (considering both node storage and dynamic storage)
	clusterScore.CanFitVM = s.canFitVM(nodeResources, vmRequirements, storageClasses)

	return clusterScore, nil
}


// ensureManagedServiceAccount creates a ManagedServiceAccount for cluster-proxy
// authentication if it does not already exist, then ensures a ClusterPermission
// grants the resulting ServiceAccount cluster-admin access on the managed cluster.
func (s *ClusterRecommendationService) ensureManagedServiceAccount(ctx context.Context, clusterName string) error {
	log := log.FromContext(ctx)
	msa := &authv1beta1.ManagedServiceAccount{}
	key := types.NamespacedName{Name: clusterRecommendMSAName, Namespace: clusterName}
	if err := s.Get(ctx, key, msa); err != nil {
		if client.IgnoreNotFound(err) != nil {
			return fmt.Errorf("failed to get ManagedServiceAccount: %w", err)
		}
		msa = &authv1beta1.ManagedServiceAccount{
			ObjectMeta: metav1.ObjectMeta{
				Name:      clusterRecommendMSAName,
				Namespace: clusterName,
			},
			Spec: authv1beta1.ManagedServiceAccountSpec{
				Rotation: authv1beta1.ManagedServiceAccountRotation{
					Enabled:  true,
					Validity: metav1.Duration{Duration: 24 * time.Hour},
				},
			},
		}
		if err := s.Create(ctx, msa); err != nil && !k8serrors.IsAlreadyExists(err) {
			return fmt.Errorf("failed to create ManagedServiceAccount: %w", err)
		}
		log.Info("ManagedServiceAccount created", "cluster", clusterName, "name", clusterRecommendMSAName)
	} else {
		log.Info("ManagedServiceAccount already exists", "cluster", clusterName, "name", clusterRecommendMSAName,
			"tokenSecretRef", msa.Status.TokenSecretRef,
			"conditions", msa.Status.Conditions)
	}

	if err := s.ensureClusterPermission(ctx, clusterName); err != nil {
		return err
	}
	return nil
}

// ensureClusterPermission creates a ClusterPermission on the hub that binds the
// cluster-recommendation ManagedServiceAccount to cluster-admin on the managed cluster.
// OCM's ClusterPermission addon translates this into a ClusterRoleBinding on the
// managed cluster, using the same mechanism as reconcileClusterPermissions in
// managedcluster_controller.go.
func (s *ClusterRecommendationService) ensureClusterPermission(ctx context.Context, clusterName string) error {
	log := log.FromContext(ctx)

	msaaNamespace, err := findMsaaDeploymentNs(ctx, s.Client)
	if err != nil {
		return fmt.Errorf("failed to find managed-serviceaccount-addon-agent namespace: %w", err)
	}
	log.Info("Found msaa namespace", "namespace", msaaNamespace)

	_, err = s.DynamicClient.Resource(ClusterPermissionsGVR).Namespace(clusterName).Get(
		ctx, clusterRecommendMSAName, metav1.GetOptions{})
	if err != nil {
		if !k8serrors.IsNotFound(err) {
			return fmt.Errorf("failed to get ClusterPermission for cluster %q: %w", clusterName, err)
		}

		payload := map[string]interface{}{
			"apiVersion": "rbac.open-cluster-management.io/v1alpha1",
			"kind":       "ClusterPermission",
			"metadata": map[string]interface{}{
				"name":      clusterRecommendMSAName,
				"namespace": clusterName,
			},
			"spec": map[string]interface{}{
				"clusterRoleBinding": map[string]interface{}{
					"subject": map[string]interface{}{
						"kind":      "ServiceAccount",
						"name":      clusterRecommendMSAName,
						"namespace": msaaNamespace,
					},
					"roleRef": map[string]interface{}{
						"kind":     "ClusterRole",
						"name":     "cluster-admin",
						"apiGroup": "rbac.authorization.k8s.io",
					},
				},
			},
		}

		payloadJSON, err := json.Marshal(payload)
		if err != nil {
			return fmt.Errorf("failed to marshal ClusterPermission: %w", err)
		}
		obj := &unstructured.Unstructured{}
		if err := json.Unmarshal(payloadJSON, obj); err != nil {
			return fmt.Errorf("failed to unmarshal ClusterPermission: %w", err)
		}
		if _, err := s.DynamicClient.Resource(ClusterPermissionsGVR).Namespace(clusterName).Create(
			ctx, obj, metav1.CreateOptions{}); err != nil && !k8serrors.IsAlreadyExists(err) {
			return fmt.Errorf("failed to create ClusterPermission for cluster %q: %w", clusterName, err)
		}
		log.Info("ClusterPermission created", "cluster", clusterName, "name", clusterRecommendMSAName, "msaaNamespace", msaaNamespace)
	} else {
		log.Info("ClusterPermission already exists", "cluster", clusterName, "name", clusterRecommendMSAName)
	}
	return nil
}

// getMSAToken ensures a ManagedServiceAccount exists and returns its bearer token.
// Returns an error if the token is not yet provisioned by OCM.
func (s *ClusterRecommendationService) getMSAToken(ctx context.Context, clusterName string) (string, error) {
	if err := s.ensureManagedServiceAccount(ctx, clusterName); err != nil {
		return "", err
	}

	msa := &authv1beta1.ManagedServiceAccount{}
	if err := s.Get(ctx, types.NamespacedName{Name: clusterRecommendMSAName, Namespace: clusterName}, msa); err != nil {
		return "", fmt.Errorf("failed to get ManagedServiceAccount: %w", err)
	}

	if msa.Status.TokenSecretRef == nil || msa.Status.TokenSecretRef.Name == "" {
		return "", fmt.Errorf("%w: ManagedServiceAccount %q for cluster %q — "+
			"ensure the managed-serviceaccount addon is running and the ClusterPermission is applied",
			errMSATokenNotReady, clusterRecommendMSAName, clusterName)
	}

	secret := &corev1.Secret{}
	if err := s.Get(ctx, types.NamespacedName{Name: msa.Status.TokenSecretRef.Name, Namespace: clusterName}, secret); err != nil {
		return "", fmt.Errorf("failed to get MSA token secret: %w", err)
	}

	token, ok := secret.Data["token"]
	if !ok || len(token) == 0 {
		return "", fmt.Errorf("token key missing in MSA secret %q", msa.Status.TokenSecretRef.Name)
	}
	return string(token), nil
}

// newClusterProxyConfig builds a rest.Config that routes API calls through
// cluster-proxy-addon-user to the named managed cluster.
//
// In dev mode (DEV_MODE=true) the Route is used and TLS verification is skipped
// because the Route's certificate is signed by the ingress CA, not the
// openshift-service-ca.crt that is used in-cluster.
func (s *ClusterRecommendationService) newClusterProxyConfig(ctx context.Context, clusterName string) (*rest.Config, error) {
	proxyHost, err := s.getClusterProxyHost(ctx)
	if err != nil {
		return nil, err
	}
	token, err := s.getMSAToken(ctx, clusterName)
	if err != nil {
		return nil, fmt.Errorf("failed to get MSA token for cluster %q: %w", clusterName, err)
	}

	tlsCfg := rest.TLSClientConfig{}
	if os.Getenv(devModeEnvVar) == "true" {
		// Route certificate is signed by the ingress/router CA which is not
		// available in-cluster as a ConfigMap — skip verification in dev mode only.
		tlsCfg.Insecure = true
	} else {
		caData, err := s.getProxyServiceCA(ctx, clusterName)
		if err != nil {
			return nil, fmt.Errorf("failed to get proxy service CA for cluster %q: %w", clusterName, err)
		}
		tlsCfg.CAData = caData
	}

	return &rest.Config{
		Host:            fmt.Sprintf("https://%s/%s", proxyHost, clusterName),
		TLSClientConfig: tlsCfg,
		BearerToken:     token,
	}, nil
}

// getProxyServiceCA reads the OpenShift service CA from the well-known configmap
// that is automatically injected into every namespace. This CA signs the
// cluster-proxy-addon-user TLS certificate.
// clusterName is used as the namespace because the openshift-service-ca.crt
// ConfigMap is injected into every namespace, and the cluster namespace is
// always guaranteed to exist for a managed cluster.
func (s *ClusterRecommendationService) getProxyServiceCA(ctx context.Context, clusterName string) ([]byte, error) {
	cm := &corev1.ConfigMap{}
	if err := s.Get(ctx, types.NamespacedName{
		Name:      openshiftServiceCAConfigMap,
		Namespace: clusterName,
	}, cm); err != nil {
		return nil, fmt.Errorf("failed to get OpenShift service CA configmap in namespace %q: %w", clusterName, err)
	}

	caData, ok := cm.Data[openshiftServiceCAKey]
	if !ok || caData == "" {
		return nil, fmt.Errorf("key %q not found in configmap %q", openshiftServiceCAKey, openshiftServiceCAConfigMap)
	}
	return []byte(caData), nil
}

// fetchNodesViaClusterProxy builds a Kubernetes client routed through
// cluster-proxy-addon-user and lists nodes on the managed cluster.
//
// getSchedulableNodeResources returns the free CPU and memory for every
// schedulable KubeVirt node on the managed cluster.
//
// It makes two parallel API calls through cluster-proxy:
//   - GET /api/v1/nodes?labelSelector=kubevirt.io/schedulable=true  → Allocatable
//   - GET /apis/metrics.k8s.io/v1beta1/nodes                        → actual usage
//
// Free space = Allocatable − actual_usage (same formula as `oc adm top nodes`).
// Allocatable is used only as an intermediate inside this function and is never
// exposed to callers.
func (s *ClusterRecommendationService) getSchedulableNodeResources(ctx context.Context, clusterName string) ([]NodeResources, error) {
	log := log.FromContext(ctx)

	cfg, err := s.newClusterProxyConfig(ctx, clusterName)
	if err != nil {
		return nil, err
	}

	kubeClient, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("failed to create kube client for cluster %q: %w", clusterName, err)
	}
	metricsClient, err := metricsv.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("failed to create metrics client for cluster %q: %w", clusterName, err)
	}

	// Fetch nodes and metrics in parallel.
	type nodeResult struct {
		list *corev1.NodeList
		err  error
	}
	type metricsResult struct {
		list *metricsv1beta1.NodeMetricsList
		err  error
	}
	nodeCh := make(chan nodeResult, 1)
	metricsCh := make(chan metricsResult, 1)

	go func() {
		list, err := kubeClient.CoreV1().Nodes().List(ctx, metav1.ListOptions{
			LabelSelector: "kubevirt.io/schedulable=true",
		})
		nodeCh <- nodeResult{list, err}
	}()
	go func() {
		list, err := metricsClient.MetricsV1beta1().NodeMetricses().List(ctx, metav1.ListOptions{})
		metricsCh <- metricsResult{list, err}
	}()

	nr := <-nodeCh
	mr := <-metricsCh

	if nr.err != nil {
		return nil, fmt.Errorf("failed to list nodes on cluster %q: %w", clusterName, nr.err)
	}
	if mr.err != nil {
		return nil, fmt.Errorf("failed to get node metrics on cluster %q: %w", clusterName, mr.err)
	}

	// Build usage lookup: node name → actual CPU/mem usage from metrics-server.
	type usage struct{ cpuMillis, memBytes int64 }
	actualUsage := make(map[string]usage, len(mr.list.Items))
	for _, nm := range mr.list.Items {
		cpuQ := nm.Usage[corev1.ResourceCPU]
		memQ := nm.Usage[corev1.ResourceMemory]
		actualUsage[nm.Name] = usage{cpuMillis: cpuQ.MilliValue(), memBytes: memQ.Value()}
	}

	var result []NodeResources
	for _, node := range nr.list.Items {
		// Keep only Ready nodes (kubevirt.io/schedulable label is already filtered at API level).
		ready := false
		for _, c := range node.Status.Conditions {
			if c.Type == corev1.NodeReady && c.Status == corev1.ConditionTrue {
				ready = true
				break
			}
		}
		if !ready {
			continue
		}

		allocCPU := node.Status.Allocatable[corev1.ResourceCPU]
		allocMem := node.Status.Allocatable[corev1.ResourceMemory]
		allocCPUMillis := allocCPU.MilliValue()
		allocMemBytes := allocMem.Value()

		u := actualUsage[node.Name]
		freeCPU := (allocCPUMillis - u.cpuMillis) / 1000
		freeMem := (allocMemBytes - u.memBytes) / (1024 * 1024 * 1024)
		if freeCPU < 0 {
			freeCPU = 0
		}
		if freeMem < 0 {
			freeMem = 0
		}

		result = append(result, NodeResources{
			NodeName:           node.Name,
			AvailableCPUCores:  freeCPU,
			AvailableMemoryGiB: freeMem,
		})
		log.Info("Node free resources",
			"node", node.Name,
			"freeCPU", freeCPU,
			"freeMemGiB", freeMem,
		)
	}
	return result, nil
}

// getStorageClasses fetches each named storage class from the managed cluster
// via cluster-proxy and enriches it with available capacity where measurable.
// For Ceph, capacity is fetched once and shared across all Ceph-backed classes.
// Missing classes are returned as errors so the caller can mark the cluster ineligible.
func (s *ClusterRecommendationService) getStorageClasses(ctx context.Context, clusterName string, storageClassNames []string) ([]StorageClassInfo, error) {
	log := log.FromContext(ctx)

	if len(storageClassNames) == 0 {
		return nil, nil
	}

	cfg, err := s.newClusterProxyConfig(ctx, clusterName)
	if err != nil {
		return nil, err
	}
	kubeClient, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("failed to create kube client via cluster-proxy for cluster %q: %w", clusterName, err)
	}

	// Fetch Ceph capacity once — all Ceph-backed storage classes share the same pool.
	var cephAvailableGB int64
	var cephFetched bool

	var result []StorageClassInfo
	for _, scName := range storageClassNames {
		raw, err := kubeClient.StorageV1().StorageClasses().Get(ctx, scName, metav1.GetOptions{})
		if err != nil {
			if k8serrors.IsNotFound(err) {
				return nil, fmt.Errorf("StorageClass %q not found on cluster %q", scName, clusterName)
			}
			return nil, fmt.Errorf("failed to get StorageClass %q on cluster %q: %w", scName, clusterName, err)
		}

		sc := storageClassInfoFromTyped(raw)

		if isCephProvisioner(sc.Provisioner) {
			if !cephFetched {
				cephAvailableGB, err = s.getCephCapacity(ctx, clusterName)
				if err != nil {
					log.Error(err, "Failed to get Ceph capacity, storage score will use CPU/memory average", "cluster", clusterName)
				}
				cephFetched = true
			}
			if cephAvailableGB > 0 {
				sc.AvailableCapacityGB = cephAvailableGB
				sc.CapacityKnown = true
			}
		} else if !sc.IsDynamic {
			// Static provisioner: capacity comes from pre-created Available PVs.
			// Store individual PV sizes for 1:1 volume→PV matching.
			pvSizes, err := s.getAvailablePVSizes(ctx, clusterName, scName)
			if err != nil {
				log.Error(err, "Failed to query available PVs for static storage class", "storageClass", scName)
			} else {
				sc.AvailablePVSizes = pvSizes
				var total int64
				for _, sz := range pvSizes {
					total += sz
				}
				sc.AvailableCapacityGB = total
				sc.CapacityKnown = true
			}
		}
		// Other dynamic provisioners (NFS, cloud CSI, etc.) provision on-demand —
		// CapacityKnown stays false.

		log.Info("StorageClass retrieved", "cluster", clusterName, "storageClass", sc.Name,
			"provisioner", sc.Provisioner, "dynamic", sc.IsDynamic, "availableGB", sc.AvailableCapacityGB)
		result = append(result, sc)
	}
	return result, nil
}

// getAvailablePVSizes returns the individual size (GB) of each Available
// PersistentVolume on the managed cluster belonging to the given storage class.
// Individual sizes are needed for 1:1 volume→PV matching — a DataVolume must be
// backed by a single PV large enough to hold it; two small PVs cannot merge.
func (s *ClusterRecommendationService) getAvailablePVSizes(ctx context.Context, clusterName, storageClassName string) ([]int64, error) {
	log := log.FromContext(ctx)

	cfg, err := s.newClusterProxyConfig(ctx, clusterName)
	if err != nil {
		return nil, err
	}
	kubeClient, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("failed to create kube client via cluster-proxy for cluster %q: %w", clusterName, err)
	}

	pvList, err := kubeClient.CoreV1().PersistentVolumes().List(ctx, metav1.ListOptions{
		FieldSelector: fmt.Sprintf("spec.storageClassName=%s,status.phase=%s",
			storageClassName, string(corev1.VolumeAvailable)),
	})
	if err != nil {
		return nil, fmt.Errorf("failed to list PersistentVolumes via cluster-proxy for cluster %q: %w", clusterName, err)
	}

	if len(pvList.Items) == 0 {
		return nil, fmt.Errorf("no Available PersistentVolumes found for storage class %q on cluster %q", storageClassName, clusterName)
	}

	sizes := make([]int64, 0, len(pvList.Items))
	for _, pv := range pvList.Items {
		if storage, ok := pv.Spec.Capacity[corev1.ResourceStorage]; ok {
			gb := storage.Value() / (1024 * 1024 * 1024)
			if gb == 0 {
				gb = 1
			}
			sizes = append(sizes, gb)
		}
	}

	log.Info("Available PVs listed", "cluster", clusterName, "storageClass", storageClassName, "count", len(sizes))
	return sizes, nil
}

// cephNamespaces lists the namespaces where a CephCluster CR may live.
// ODF (OpenShift Data Foundation) always uses openshift-storage;
// Rook-Ceph standalone clusters default to rook-ceph.
var cephNamespaces = []string{"openshift-storage", "rook-ceph"}

// getCephCapacity queries the CephCluster CR on the managed cluster and returns
// the available capacity in GB. It tries each known Ceph namespace in order and
// returns the first successful result. Returns 0 if Ceph is not present.
//
// Verified against ceph.rook.io/v1 CephCluster types.go:
//
//	ClusterStatus.CephStatus  json:"ceph"
//	CephStatus.Capacity       json:"capacity"
//	Capacity.AvailableBytes   json:"bytesAvailable"
func (s *ClusterRecommendationService) getCephCapacity(ctx context.Context, clusterName string) (int64, error) {
	log := log.FromContext(ctx)

	for _, ns := range cephNamespaces {
		availableGB, err := s.queryCephCapacityInNamespace(ctx, clusterName, ns)
		if err != nil {
			log.V(1).Info("CephCluster not found in namespace", "namespace", ns, "error", err)
			continue
		}
		if availableGB > 0 {
			log.Info("Ceph cluster capacity retrieved", "cluster", clusterName, "namespace", ns, "availableGB", availableGB)
			return availableGB, nil
		}
	}

	return 0, nil
}

var cephClusterGVR = schema.GroupVersionResource{
	Group:   "ceph.rook.io",
	Version: "v1",
	Resource: "cephclusters",
}

func (s *ClusterRecommendationService) queryCephCapacityInNamespace(ctx context.Context, clusterName, namespace string) (int64, error) {
	cfg, err := s.newClusterProxyConfig(ctx, clusterName)
	if err != nil {
		return 0, err
	}
	dynClient, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return 0, fmt.Errorf("failed to create dynamic client via cluster-proxy for cluster %q: %w", clusterName, err)
	}

	list, err := dynClient.Resource(cephClusterGVR).Namespace(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return 0, fmt.Errorf("failed to list CephClusters in namespace %q on cluster %q: %w", namespace, clusterName, err)
	}
	if len(list.Items) == 0 {
		return 0, nil
	}

	// Path: status.ceph.capacity.bytesAvailable
	// NestedInt64 traverses the unstructured object without any marshal/unmarshal round-trip.
	bytesAvailable, found, err := unstructured.NestedInt64(
		list.Items[0].Object,
		"status", "ceph", "capacity", "bytesAvailable",
	)
	if err != nil || !found || bytesAvailable <= 0 {
		return 0, nil
	}

	return bytesAvailable / (1024 * 1024 * 1024), nil
}

// isCephProvisioner reports whether the provisioner is Ceph-backed (ODF or Rook).
func isCephProvisioner(provisioner string) bool {
	return strings.Contains(provisioner, "ceph") || strings.Contains(provisioner, "rbd")
}

// parseStorageClass extracts storage class information from the raw data
// storageClassInfoFromTyped builds a StorageClassInfo from a typed storagev1.StorageClass.
func storageClassInfoFromTyped(sc *storagev1.StorageClass) StorageClassInfo {
	info := StorageClassInfo{
		Name:      sc.Name,
		Provisioner: sc.Provisioner,
		IsDynamic:   isDynamicStorageProvisioner(sc.Provisioner),
	}

	if sc.VolumeBindingMode != nil {
		info.VolumeBindingMode = string(*sc.VolumeBindingMode)
	}
	if sc.AllowVolumeExpansion != nil {
		info.AllowVolumeExpansion = *sc.AllowVolumeExpansion
	}

	// AvailableCapacityGB and CapacityKnown are filled in by getStorageClasses
	// after querying Ceph or counting available PVs. For other dynamic provisioners
	// the backend pool cannot be introspected — both stay at zero/false.

	return info
}

// staticProvisioners are known non-dynamic (manual/local) provisioners that must
// never be classified as dynamic regardless of any suffix heuristic.
var staticProvisioners = map[string]bool{
	"kubernetes.io/no-provisioner": true, // standard local static provisioner
	"kubernetes.io/host-path":      true, // in-tree host-path
	"rancher.io/local-path":        true, // Rancher local-path provisioner
	"local.csi.k8s.io":            true, // sig-storage local static CSI — pre-creates PVs, does NOT auto-provision
}

// isDynamicStorageProvisioner checks if the provisioner supports dynamic provisioning.
func isDynamicStorageProvisioner(provisioner string) bool {
	if provisioner == "" {
		return false
	}
	if staticProvisioners[provisioner] {
		return false
	}

	// Explicit allow-list covers all common dynamic provisioners.
	dynamicProvisioners := []string{
		// Ceph — ODF / OpenShift Data Foundation (most common on OpenShift)
		"openshift-storage.rbd.csi.ceph.com",
		"openshift-storage.cephfs.csi.ceph.com",
		// Ceph — Rook
		"rook-ceph.rbd.csi.ceph.com",
		"rook-ceph.cephfs.csi.ceph.com",
		// Ceph — legacy in-tree
		"ceph.rook.io/block",
		"kubernetes.io/rbd",
		// AWS
		"ebs.csi.aws.com",
		"kubernetes.io/aws-ebs",
		// Azure
		"disk.csi.azure.com",
		"file.csi.azure.com",
		"kubernetes.io/azure-disk",
		"kubernetes.io/azure-file",
		// GCP
		"pd.csi.storage.gke.io",
		"kubernetes.io/gce-pd",
		// vSphere
		"csi.vsphere.vmware.com",
		"kubernetes.io/vsphere-volume",
		// NFS
		"nfs.csi.k8s.io",
		// NetApp
		"csi.trident.netapp.io",
		// Longhorn
		"driver.longhorn.io",
		// OpenEBS
		"openebs.io/local",
		"cstor.csi.openebs.io",
		// LVMS / LVM Storage Operator (OpenShift) / TopoLVM — dynamic (provisions LVM volumes on-demand)
		"topolvm.io",
		"lvm.topolvm.io",
	}
	for _, dp := range dynamicProvisioners {
		if provisioner == dp {
			return true
		}
	}

	// Fallback: any remaining provisioner with "csi" in its name is almost
	// certainly a CSI dynamic provisioner (the static guard above already
	// excluded known static ones).
	return strings.Contains(provisioner, "csi")
}

// calculateResourceScores calculates scores for CPU, memory, and storage
// Returns -1 for all scores if cluster doesn't meet minimum requirements
// bestNode returns the node with the highest combined CPU and memory headroom.
// CPU and memory are weighted equally; the result is the node KubeVirt is most
// likely to target for a new VM.
func bestNode(nodes []NodeResources) *NodeResources {
	if len(nodes) == 0 {
		return nil
	}
	best := &nodes[0]
	for i := 1; i < len(nodes); i++ {
		n := &nodes[i]
		// Use sum of CPU + memory as a simple combined headroom metric.
		if n.AvailableCPUCores+n.AvailableMemoryGiB > best.AvailableCPUCores+best.AvailableMemoryGiB {
			best = n
		}
	}
	return best
}

func (s *ClusterRecommendationService) calculateResourceScores(
	nodeResources []NodeResources,
	vmRequirements VMResourceRequirements,
	storageClasses []StorageClassInfo,
) (cpuScore, memoryScore, storageScore float64, storageKnown bool) {
	if len(nodeResources) == 0 {
		return -1, -1, -1, false
	}

	// A VM lands on exactly one node — find the node with the most headroom.
	var maxAvailableCPU, maxAvailableMemory int64
	for _, node := range nodeResources {
		if node.AvailableCPUCores > maxAvailableCPU {
			maxAvailableCPU = node.AvailableCPUCores
		}
		if node.AvailableMemoryGiB > maxAvailableMemory {
			maxAvailableMemory = node.AvailableMemoryGiB
		}
	}

	if maxAvailableCPU < vmRequirements.CPUCores {
		return -1, -1, -1, false
	}
	if maxAvailableMemory < vmRequirements.MemoryGiB {
		return -1, -1, -1, false
	}

	// Build a lookup: storageClassName → StorageClassInfo
	scByName := make(map[string]StorageClassInfo, len(storageClasses))
	for _, sc := range storageClasses {
		scByName[sc.Name] = sc
	}

	// Aggregate required storage per class.
	// Dynamic + CapacityKnown=false → always passes, not scored.
	// Dynamic + CapacityKnown=true (Ceph) → check total required vs available.
	// Static + CapacityKnown=true → check total required vs available PVs.
	classRequired := make(map[string]int64)
	for _, vol := range vmRequirements.Volumes {
		classRequired[vol.StorageClassName] += vol.SizeGB
	}

	var totalRequired, totalAvailable int64
	for className, requiredGB := range classRequired {
		sc, found := scByName[className]
		if !found {
			// Storage class not present on this cluster — ineligible.
			return -1, -1, -1, false
		}
		if sc.IsDynamic {
			if sc.CapacityKnown {
				if sc.AvailableCapacityGB < requiredGB {
					return -1, -1, -1, false
				}
				totalRequired += requiredGB
				totalAvailable += sc.AvailableCapacityGB
				storageKnown = true
			}
			// Unknown-capacity dynamic (NFS, cloud CSI, topolvm) → always passes, not scored.
		} else {
			// Static: each volume needs its own matching Available PV (1:1).
			// Summing all PV capacity is insufficient — two small PVs cannot
			// satisfy one large DataVolume.
			if !sc.CapacityKnown || len(sc.AvailablePVSizes) == 0 {
				return -1, -1, -1, false
			}
			volSizes := volumeSizesForClass(vmRequirements.Volumes, className)
			if !canMatchVolumesToPVs(volSizes, sc.AvailablePVSizes) {
				return -1, -1, -1, false
			}
			totalRequired += requiredGB
			totalAvailable += sc.AvailableCapacityGB
			storageKnown = true
		}
	}

	cpuScore = float64(maxAvailableCPU) / float64(vmRequirements.CPUCores)
	memoryScore = float64(maxAvailableMemory) / float64(vmRequirements.MemoryGiB)
	if storageKnown && totalRequired > 0 {
		storageScore = float64(totalAvailable) / float64(totalRequired)
	}

	return cpuScore, memoryScore, storageScore, storageKnown
}

// canFitVM checks whether at least one node has enough CPU and memory for the VM,
// and whether every volume's storage class on the target cluster has sufficient
// capacity. Node-local ephemeral-storage is not considered — CNV VMs use PVC-backed disks.
func (s *ClusterRecommendationService) canFitVM(
	nodeResources []NodeResources,
	vmRequirements VMResourceRequirements,
	storageClasses []StorageClassInfo,
) bool {
	nodeOK := false
	for _, node := range nodeResources {
		if node.AvailableCPUCores >= vmRequirements.CPUCores &&
			node.AvailableMemoryGiB >= vmRequirements.MemoryGiB {
			nodeOK = true
			break
		}
	}
	if !nodeOK {
		return false
	}

	scByName := make(map[string]StorageClassInfo, len(storageClasses))
	for _, sc := range storageClasses {
		scByName[sc.Name] = sc
	}

	// Aggregate required storage per class and check each.
	classRequired := make(map[string]int64)
	for _, vol := range vmRequirements.Volumes {
		classRequired[vol.StorageClassName] += vol.SizeGB
	}
	for className, requiredGB := range classRequired {
		sc, found := scByName[className]
		if !found {
			return false
		}
		if sc.IsDynamic {
			if sc.CapacityKnown && sc.AvailableCapacityGB < requiredGB {
				return false
			}
		} else {
			// Static: 1:1 matching — each volume needs its own Available PV.
			if !sc.CapacityKnown || len(sc.AvailablePVSizes) == 0 {
				return false
			}
			volSizes := volumeSizesForClass(vmRequirements.Volumes, className)
			if !canMatchVolumesToPVs(volSizes, sc.AvailablePVSizes) {
				return false
			}
		}
	}
	return true
}

// ── VM inspection via cluster-proxy ─────────────────────────────────────────

// fetchVMRequirements connects to the source managed cluster via cluster-proxy,
// fetches the VirtualMachine spec, and derives CPU, memory, and per-volume
// storage requirements. Storage class names are read from each DataVolume/PVC
// spec — the caller does not supply them. Non-fatal warnings are returned for
// volumes whose size or storage class cannot be determined (e.g. hostDisk).
func (s *ClusterRecommendationService) fetchVMRequirements(ctx context.Context, req VMLocationRequest) (VMResourceRequirements, []string, error) {
	cfg, err := s.newClusterProxyConfig(ctx, req.ClusterName)
	if err != nil {
		return VMResourceRequirements{}, nil, fmt.Errorf("failed to connect to source cluster %q: %w", req.ClusterName, err)
	}

	dynClient, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return VMResourceRequirements{}, nil, fmt.Errorf("failed to create dynamic client for cluster %q: %w", req.ClusterName, err)
	}
	kubeClient, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return VMResourceRequirements{}, nil, fmt.Errorf("failed to create kube client for cluster %q: %w", req.ClusterName, err)
	}

	// Fetch the VMI — it has actual runtime values (CPU topology, memory) already
	// resolved, unlike the VM spec which may reference instancetypes with resources: {}.
	vmi, err := dynClient.Resource(virtualMachineInstanceGVR).Namespace(req.VMNamespace).Get(ctx, req.VMName, metav1.GetOptions{})
	if err != nil {
		return VMResourceRequirements{}, nil, fmt.Errorf("failed to get VMI %q/%q on cluster %q: %w", req.VMNamespace, req.VMName, req.ClusterName, err)
	}

	cpuCores := extractVMICPU(vmi)

	memoryGiB, err := extractVMIMemory(vmi)
	if err != nil {
		return VMResourceRequirements{}, nil, fmt.Errorf("failed to extract memory from VMI %q: %w", req.VMName, err)
	}

	volumes, warnings, err := extractVMIStorage(ctx, vmi, kubeClient)
	if err != nil {
		return VMResourceRequirements{}, warnings, err
	}

	return VMResourceRequirements{
		CPUCores:  cpuCores,
		MemoryGiB: memoryGiB,
		Volumes:   volumes,
	}, warnings, nil
}

// extractVMICPU returns the total vCPU count from a VirtualMachineInstance:
// sockets × cores × threads. The VMI spec has already resolved any instancetype
// references so the values here reflect the actual running CPU topology.
// Each field defaults to 1 when not set.
func extractVMICPU(vmi *unstructured.Unstructured) int64 {
	// VMI path: spec.domain.cpu (no template.spec wrapper unlike the VM object)
	base := []string{"spec", "domain", "cpu"}
	getInt := func(field string) int64 {
		v, _, _ := unstructured.NestedInt64(vmi.Object, append(base, field)...)
		return v
	}
	sockets := getInt("sockets")
	cores := getInt("cores")
	threads := getInt("threads")
	if sockets == 0 {
		sockets = 1
	}
	if cores == 0 {
		cores = 1
	}
	if threads == 0 {
		threads = 1
	}
	return sockets * cores * threads
}

// extractVMIMemory parses memory from a VirtualMachineInstance and returns GiB.
// Priority:
//  1. status.memory.guestAtBoot — actual memory allocated at boot time
//  2. spec.domain.memory.guest  — requested guest memory
//
// The VMI already has these values resolved from any instancetype, unlike the
// VM spec which may have resources: {} when an instancetype is used.
func extractVMIMemory(vmi *unstructured.Unstructured) (int64, error) {
	// 1. Actual boot memory (most accurate)
	memStr, found, _ := unstructured.NestedString(vmi.Object, "status", "memory", "guestAtBoot")
	if !found || memStr == "" {
		// 2. Requested guest memory from spec
		memStr, found, _ = unstructured.NestedString(vmi.Object, "spec", "domain", "memory", "guest")
	}
	if !found || memStr == "" {
		return 0, fmt.Errorf("no memory found in VMI (checked status.memory.guestAtBoot and spec.domain.memory.guest)")
	}
	q, err := resource.ParseQuantity(memStr)
	if err != nil {
		return 0, fmt.Errorf("failed to parse memory quantity %q: %w", memStr, err)
	}
	gib := q.Value() / (1024 * 1024 * 1024)
	if gib == 0 {
		gib = 1
	}
	return gib, nil
}

// extractVMIStorage reads storage requirements from the VMI's status.volumeStatus.
// Each entry with a persistentVolumeClaimInfo block represents a PVC-backed disk.
// The claimName and actual capacity come directly from the VMI status — no need
// to fetch each DataVolume separately. The PVC is fetched only to read storageClassName.
//
// Volumes without persistentVolumeClaimInfo (cloudInit, containerDisk, etc.) are
// silently skipped. hostDisk volumes are detected from spec.volumes and warned about.
func extractVMIStorage(
	ctx context.Context,
	vmi *unstructured.Unstructured,
	kubeClient kubernetes.Interface,
) ([]VolumeRequirement, []string, error) {
	namespace, _, _ := unstructured.NestedString(vmi.Object, "metadata", "namespace")

	// Build a set of hostDisk volume names from spec.volumes for warning purposes.
	specVolumes, _, _ := unstructured.NestedSlice(vmi.Object, "spec", "volumes")
	hostDiskNames := map[string]struct{}{}
	for _, v := range specVolumes {
		vol, ok := v.(map[string]interface{})
		if !ok {
			continue
		}
		if hasMapKey(vol, "hostDisk") {
			name, _, _ := unstructured.NestedString(vol, "name")
			hostDiskNames[name] = struct{}{}
		}
	}

	volumeStatuses, _, _ := unstructured.NestedSlice(vmi.Object, "status", "volumeStatus")

	var result []VolumeRequirement
	var warnings []string

	for _, vs := range volumeStatuses {
		vsMap, ok := vs.(map[string]interface{})
		if !ok {
			continue
		}
		volName, _, _ := unstructured.NestedString(vsMap, "name")

		// Warn about hostDisk volumes — their size cannot be calculated from the VMI.
		if _, isHostDisk := hostDiskNames[volName]; isHostDisk {
			warnings = append(warnings,
				fmt.Sprintf("hostDisk volume %q is not included in storage calculation", volName))
			continue
		}

		// Only PVC-backed volumes have persistentVolumeClaimInfo.
		// cloudInit, containerDisk, etc. don't — skip them silently.
		pvcInfo, found, _ := unstructured.NestedMap(vsMap, "persistentVolumeClaimInfo")
		if !found {
			continue
		}

		claimName, _, _ := unstructured.NestedString(pvcInfo, "claimName")
		capacityStr, _, _ := unstructured.NestedString(pvcInfo, "capacity", "storage")

		if claimName == "" {
			warnings = append(warnings, fmt.Sprintf("volume %q has persistentVolumeClaimInfo but no claimName", volName))
			continue
		}

		// Parse actual capacity from VMI status — no need to fetch the DataVolume.
		var sizeGB int64
		if capacityStr != "" {
			q, err := resource.ParseQuantity(capacityStr)
			if err != nil {
				warnings = append(warnings, fmt.Sprintf("failed to parse capacity %q for volume %q: %v", capacityStr, volName, err))
			} else {
				sizeGB = q.Value() / (1024 * 1024 * 1024)
				if sizeGB == 0 {
					sizeGB = 1
				}
			}
		}

		// Fetch the PVC only to get its storageClassName.
		scName, err := fetchPVCStorageClass(ctx, namespace, claimName, kubeClient)
		if err != nil {
			warnings = append(warnings, fmt.Sprintf("failed to get storageClassName for PVC %q: %v", claimName, err))
		}
		if scName == "" {
			warnings = append(warnings, fmt.Sprintf("PVC %q has no storageClassName — will use cluster default", claimName))
		}

		result = append(result, VolumeRequirement{
			VolumeName:       volName,
			StorageClassName: scName,
			SizeGB:           sizeGB,
		})
	}

	return result, warnings, nil
}

// fetchPVCStorageClass fetches a PVC and returns its storageClassName.
func fetchPVCStorageClass(ctx context.Context, namespace, claimName string, kubeClient kubernetes.Interface) (string, error) {
	pvc, err := kubeClient.CoreV1().PersistentVolumeClaims(namespace).Get(ctx, claimName, metav1.GetOptions{})
	if err != nil {
		return "", fmt.Errorf("failed to get PVC %q: %w", claimName, err)
	}
	if pvc.Spec.StorageClassName != nil {
		return *pvc.Spec.StorageClassName, nil
	}
	return "", nil
}

// hasMapKey reports whether key exists in m (regardless of value type).
func hasMapKey(m map[string]interface{}, key string) bool {
	_, ok := m[key]
	return ok
}

// volumeSizesForClass returns the size (GB) of every volume that uses the given
// storage class. Used to build the per-class volume list for PV matching.
func volumeSizesForClass(volumes []VolumeRequirement, className string) []int64 {
	var sizes []int64
	for _, v := range volumes {
		if v.StorageClassName == className {
			sizes = append(sizes, v.SizeGB)
		}
	}
	return sizes
}

// canMatchVolumesToPVs checks whether each volume can be matched 1:1 to an
// individual Available PV large enough to hold it. This is necessary because
// static PVs are pre-provisioned at fixed sizes — two small PVs cannot merge
// to satisfy one large DataVolume.
//
// Algorithm: sort volumes and PVs descending, greedily assign the largest
// available PV to the largest unmatched volume.
func canMatchVolumesToPVs(volumeSizesGB []int64, pvSizesGB []int64) bool {
	vols := make([]int64, len(volumeSizesGB))
	copy(vols, volumeSizesGB)
	pvs := make([]int64, len(pvSizesGB))
	copy(pvs, pvSizesGB)

	// Both slices are sorted descending so the largest volume is matched first
	// and the largest PV is always tried first.
	sort.Slice(vols, func(i, j int) bool { return vols[i] > vols[j] })
	sort.Slice(pvs, func(i, j int) bool { return pvs[i] > pvs[j] })

	// pvIdx is a cursor into the sorted PV slice that only moves forward.
	// Because both slices are sorted descending, once a PV is too small for
	// the current volume it is also too small for every remaining (smaller)
	// volume — so we never need to look backwards.
	// Each iteration either consumes a PV (pvIdx++) or returns false.
	pvIdx := 0
	for _, volSize := range vols {
		// Skip PVs that are too small for this volume.
		for pvIdx < len(pvs) && pvs[pvIdx] < volSize {
			pvIdx++
		}
		if pvIdx >= len(pvs) {
			return false // no remaining PV is large enough for this volume
		}
		pvIdx++ // mark this PV as consumed; it cannot be reused for another volume
	}
	return true
}


// uniqueStorageClassNames returns the deduplicated list of storage class names
// across all volumes, skipping empty names.
func uniqueStorageClassNames(volumes []VolumeRequirement) []string {
	seen := map[string]struct{}{}
	var names []string
	for _, v := range volumes {
		if v.StorageClassName == "" {
			continue
		}
		if _, ok := seen[v.StorageClassName]; !ok {
			seen[v.StorageClassName] = struct{}{}
			names = append(names, v.StorageClassName)
		}
	}
	return names
}
