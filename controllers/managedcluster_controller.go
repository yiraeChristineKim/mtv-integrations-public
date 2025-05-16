package controllers

import (
	"bytes"
	"context"
	"encoding/json"
	"time"

	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	clusterv1 "open-cluster-management.io/api/cluster/v1"
	auth "open-cluster-management.io/managed-serviceaccount/apis/authentication/v1beta1"
)

// ManagedClusterReconciler reconciles a ManagedCluster object
type ManagedClusterReconciler struct {
	client.Client
	Scheme        *runtime.Scheme
	DynamicClient *dynamic.DynamicClient
}

//+kubebuilder:rbac:groups=cluster.open-cluster-management.io,resources=managedclusters,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=cluster.open-cluster-management.io,resources=managedclusters/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=cluster.open-cluster-management.io,resources=managedclusters/finalizers,verbs=update

func (r *ManagedClusterReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := log.FromContext(ctx)
	log.Info("Reconciling ManagedCluster", "name", req.NamespacedName)

	// Fetch the ManagedCluster instance
	managedCluster := &clusterv1.ManagedCluster{}
	if err := r.Get(ctx, req.NamespacedName, managedCluster); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	ManagedClusterMTV := managedCluster.Name + "-mtv"

	if managedCluster.GetLabels()["acm/cnv-operator-install"] == "true" {

		// ManagedServiceAccount Exists
		managedServiceAccount := &auth.ManagedServiceAccount{}
		if err := r.Get(ctx, types.NamespacedName{Name: ManagedClusterMTV, Namespace: managedCluster.Name}, managedServiceAccount); errors.IsNotFound(err) {
			log.Info("ManagedServiceAccount not found")

			managedServiceAccount.Name = ManagedClusterMTV
			managedServiceAccount.Namespace = managedCluster.Name
			managedServiceAccount.Spec.Rotation.Enabled = true
			managedServiceAccount.Spec.Rotation.Validity = metav1.Duration{
				Duration: time.Minute * 60}
			controllerutil.SetControllerReference(managedCluster, managedServiceAccount, r.Scheme)
			if err := r.Create(ctx, managedServiceAccount, &client.CreateOptions{}); err != nil {
				log.Error(err, "Failed to create ManagedServiceAccount")
				return ctrl.Result{}, err
			}
			log.Info("Created successfully", "ManagedServiceAccount", managedServiceAccount.Name)

			return ctrl.Result{RequeueAfter: time.Second * 2}, nil

		} else if err != nil {
			log.Error(err, "Failed to get ManagedServiceAccount")
			return ctrl.Result{}, err
		}

		if err := r.reconcileResource(ctx,
			generateGVR("rbac.open-cluster-management.io", "v1alpha1", "clusterpermissions"),
			managedCluster.Name, clusterPermissionPayload(managedCluster)); err != nil {
			log.Error(err, "Failed to reconcile Provider")
			return ctrl.Result{}, err
		}

		secret := &corev1.Secret{}
		namespacedName := types.NamespacedName{
			Name:      ManagedClusterMTV,
			Namespace: managedCluster.Name,
		}

		// Do not attempt to reconcile the secret if the ManagedServiceAccount has not provisioned it yet
		// This is the case when the ManagedServiceAccount is created for the first time
		// and the token is not yet available
		// The secret is created by the ManagedServiceAccount controller
		if managedServiceAccount.Status.TokenSecretRef != nil && managedServiceAccount.Status.TokenSecretRef.Name != "" {
			if err := r.Client.Get(ctx, namespacedName, secret); err != nil {
				log.Error(err, "Failed to retrieve secret")
				return ctrl.Result{}, err
			}
			if !bytes.Equal(secret.Data["cacert"], secret.Data["ca.crt"]) {
				log.Info("Adding provider details to secret", "secret", secret.Name)

				secret.Data["insecureSkipVerify"] = []byte("false")
				secret.Data["url"] = []byte(managedCluster.Spec.ManagedClusterClientConfigs[0].URL)
				secret.Data["cacert"] = secret.Data["ca.crt"]

				if err := r.Client.Update(ctx, secret); err != nil {
					log.Error(err, "Failed to update secret")
					return ctrl.Result{}, err
				}
				log.Info("Updated successfully", "provider_secret", secret.Name)
			}
		} else {
			log.Info("ManagedServiceAccount secret is not ready")
			return ctrl.Result{RequeueAfter: time.Second * 2}, nil // Wait for the token to be created
		}

		// The plan is reconciled last to make sure it synchronizes with the Provider secret
		if err := r.reconcileResource(
			ctx, generateGVR("forklift.konveyor.io", "v1beta1", "providers"),
			managedCluster.Name, providerPayload(managedCluster)); err != nil {
			log.Error(err, "Failed to reconcile Provider")
			return ctrl.Result{}, err
		}
	}

	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *ManagedClusterReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&clusterv1.ManagedCluster{}).
		Owns(&auth.ManagedServiceAccount{}). // Watch ManagedServiceAccounts owned by ManagedClusters
		Complete(r)
}

func (r *ManagedClusterReconciler) reconcileResource(ctx context.Context, gvr schema.GroupVersionResource, managedClusterName string, payload map[string]interface{}) error {
	log := log.FromContext(ctx)
	resourceKind := gvr.Resource
	ManagedClusterMTV := managedClusterName + "-mtv"

	_, err := r.DynamicClient.Resource(gvr).Namespace(managedClusterName).Get(ctx, ManagedClusterMTV, metav1.GetOptions{})
	if errors.IsNotFound(err) {
		log.Info("Create " + resourceKind)

		payloadJSON, err := json.Marshal(payload)
		if err != nil {
			log.Error(err, "Failed to marshal "+resourceKind+" JSON")
			return err
		}

		unstructuredPayload := &unstructured.Unstructured{}
		err = json.Unmarshal(payloadJSON, unstructuredPayload)
		if err != nil {
			log.Error(err, "Failed to unmarshal "+resourceKind+" JSON to unstructured")
			return err
		}

		_, err = r.DynamicClient.Resource(gvr).Namespace(managedClusterName).Create(ctx, unstructuredPayload, metav1.CreateOptions{})
		if err != nil {
			log.Error(err, "Failed to create"+resourceKind)
			return err
		}
		log.Info("Created successfully", resourceKind, managedClusterName)
	} else if err != nil {
		return nil
	}
	return nil
}
