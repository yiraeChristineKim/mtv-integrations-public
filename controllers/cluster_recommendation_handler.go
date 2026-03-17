package controllers

import (
	"encoding/json"
	"fmt"
	"net/http"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// ClusterRecommendationHandler handles HTTP requests for cluster recommendations
type ClusterRecommendationHandler struct {
	service *ClusterRecommendationService
}

// NewClusterRecommendationHandler creates a new handler
func NewClusterRecommendationHandler(c client.Client, scheme *runtime.Scheme, restConfig *rest.Config, dynClient dynamic.Interface) *ClusterRecommendationHandler {
	service := &ClusterRecommendationService{
		Client:        c,
		Scheme:        scheme,
		RestConfig:    restConfig,
		DynamicClient: dynClient,
	}
	return &ClusterRecommendationHandler{
		service: service,
	}
}

// HandleClusterRecommendation handles GET /api/cluster-recommendation
//
// Required query parameters:
//
//	cluster     — name of the managed cluster where the VM currently lives
//	vmName      — VirtualMachine name
//	vmNamespace — VirtualMachine namespace
//
// Storage class names are derived automatically from each DataVolume/PVC in the VM spec.
//
// Example:
//
//	GET /api/cluster-recommendation?cluster=managed1&vmName=my-vm&vmNamespace=default
func (h *ClusterRecommendationHandler) HandleClusterRecommendation(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	query := r.URL.Query()
	clusterName := query.Get("cluster")
	vmName := query.Get("vmName")
	vmNamespace := query.Get("vmNamespace")

	if clusterName == "" || vmName == "" || vmNamespace == "" {
		http.Error(w,
			"Missing required parameters: cluster, vmName, vmNamespace",
			http.StatusBadRequest)
		return
	}

	req := VMLocationRequest{
		ClusterName: clusterName,
		VMName:      vmName,
		VMNamespace: vmNamespace,
	}

	h.handleRequest(w, r, req)
}

// HandleClusterRecommendationPost handles POST /api/cluster-recommendation
//
// JSON body fields:
//
//	cluster     — name of the managed cluster where the VM currently lives
//	vmName      — VirtualMachine name
//	vmNamespace — VirtualMachine namespace
//
// Storage class names are derived automatically from each DataVolume/PVC in the VM spec.
func (h *ClusterRecommendationHandler) HandleClusterRecommendationPost(w http.ResponseWriter, r *http.Request) {
	log := log.FromContext(r.Context())

	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req VMLocationRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		log.Error(err, "Failed to decode JSON request")
		http.Error(w, "Invalid JSON request body", http.StatusBadRequest)
		return
	}

	if req.ClusterName == "" || req.VMName == "" || req.VMNamespace == "" {
		http.Error(w,
			"Missing required fields: cluster, vmName, vmNamespace",
			http.StatusBadRequest)
		return
	}

	h.handleRequest(w, r, req)
}

// handleRequest is the shared logic for GET and POST: fetch VM requirements,
// score all eligible clusters, and write the JSON response.
func (h *ClusterRecommendationHandler) handleRequest(w http.ResponseWriter, r *http.Request, req VMLocationRequest) {
	log := log.FromContext(r.Context())

	log.Info("Fetching VM requirements from source cluster",
		"cluster", req.ClusterName, "vm", req.VMName, "namespace", req.VMNamespace)

	vmRequirements, warnings, err := h.service.fetchVMRequirements(r.Context(), req)
	if err != nil {
		log.Error(err, "Failed to fetch VM requirements")
		http.Error(w, fmt.Sprintf("Failed to inspect VM: %v", err), http.StatusBadRequest)
		return
	}

	log.Info("Derived VM requirements",
		"cpu", vmRequirements.CPUCores,
		"memoryGiB", vmRequirements.MemoryGiB,
		"volumes", len(vmRequirements.Volumes),
		"warnings", warnings)

	response, err := h.service.GetBestManagedCluster(r.Context(), vmRequirements)
	if err != nil {
		log.Error(err, "Failed to get cluster recommendation")
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}

	response.Warnings = append(response.Warnings, warnings...)

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(response); err != nil {
		log.Error(err, "Failed to encode response")
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}

	log.Info("Successfully processed cluster recommendation request",
		"recommended", func() string {
			if response.RecommendedCluster != nil {
				return response.RecommendedCluster.ClusterName
			}
			return "none"
		}())
}
