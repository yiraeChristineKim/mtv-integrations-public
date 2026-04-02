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

package main

import (
	"context"
	"crypto/tls"
	"flag"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"time"

	// Import all Kubernetes client auth plugins (e.g. Azure, GCP, OIDC, etc.)
	// to ensure that exec-entrypoint and run can make use of them.
	"github.com/stolostron/mtv-integrations/controllers"
	"github.com/stolostron/mtv-integrations/controllers/migrationadvisor"
	miwebhook "github.com/stolostron/mtv-integrations/webhook"
	_ "k8s.io/client-go/plugin/pkg/client/auth"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/dynamic"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	clusterv1 "open-cluster-management.io/api/cluster/v1"
	auth "open-cluster-management.io/managed-serviceaccount/apis/authentication/v1beta1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/certwatcher"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/metrics/filters"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/webhook"

	forkliftv1beta1 "github.com/kubev2v/forklift/pkg/apis/forklift/v1beta1"
	authorizationv1 "k8s.io/api/authorization/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	// +kubebuilder:scaffold:imports
)

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(clusterv1.Install(scheme))
	utilruntime.Must(auth.AddToScheme(scheme))
	utilruntime.Must(forkliftv1beta1.SchemeBuilder.AddToScheme(scheme))
	utilruntime.Must(authorizationv1.AddToScheme(scheme))
	utilruntime.Must(apiextensionsv1.AddToScheme(scheme))
	// +kubebuilder:scaffold:scheme
}

var routeGVR = schema.GroupVersionResource{
	Group:    "route.openshift.io",
	Version:  "v1",
	Resource: "routes",
}

// discoverAdvisorEndpoints auto-discovers the OpenShift Route URLs for the ACM
// Search API and Thanos Query Frontend. It is called at startup when the
// corresponding flags are not set, so there is no need to look up Route URLs
// manually when running the controller outside the cluster.
//
// Route names are stable across ACM/MCO installations:
//   - search-api       in open-cluster-management
//   - rbac-query-proxy in open-cluster-management-observability
//
// Returns empty strings for any endpoint that could not be discovered; the
// clients will then fall back to their in-cluster service defaults.
func discoverAdvisorEndpoints(
	ctx context.Context,
	dynClient dynamic.Interface,
	searchEndpoint, thanosEndpoint string,
) (string, string, error) {
	routeHost := func(namespace, routeName string) (string, error) {
		obj, err := dynClient.Resource(routeGVR).Namespace(namespace).Get(ctx, routeName, metav1.GetOptions{})
		if err != nil {
			if apierrors.IsNotFound(err) {
				setupLog.Info("Route not found, will use in-cluster default",
					"namespace", namespace, "route", routeName)
				return "", nil
			}
			setupLog.Error(err, "Failed to lookup Route",
				"namespace", namespace, "route", routeName)
			return "", err
		}
		host, _, _ := unstructured.NestedString(obj.Object, "spec", "host")
		return host, nil
	}

	if searchEndpoint == "" {
		host, err := routeHost("open-cluster-management", "search-api")
		if err != nil {
			return "", "", err
		}
		if host != "" {
			searchEndpoint = fmt.Sprintf("https://%s/searchapi/graphql", host)
			setupLog.Info("Discovered Search API Route", "endpoint", searchEndpoint)
		}
	}

	if thanosEndpoint == "" {
		host, err := routeHost("open-cluster-management-observability", "rbac-query-proxy")
		if err != nil {
			return "", "", err
		}
		if host != "" {
			thanosEndpoint = fmt.Sprintf("https://%s", host)
			setupLog.Info("Discovered Thanos Route", "endpoint", thanosEndpoint)
		}
	}

	return searchEndpoint, thanosEndpoint, nil
}

// nolint:gocyclo
// runnableFunc adapts a plain function to the ctrl.Runnable interface so it
// can be added to the controller-runtime manager's lifecycle.
type runnableFunc func(ctx context.Context) error

func (f runnableFunc) Start(ctx context.Context) error { return f(ctx) }

func main() {
	var metricsAddr string
	var metricsCertPath, metricsCertName, metricsCertKey string
	var webhookCertPath, webhookCertName, webhookCertKey string
	var enableLeaderElection bool
	// For local testing
	var enableWebhook bool
	var probeAddr string
	var secureMetrics bool
	var enableHTTP2 bool
	var tlsOpts []func(*tls.Config)
	var searchAPIEndpoint string
	var thanosHost string
	var advisorAddr string
	var advisorCacheTTL time.Duration
	flag.StringVar(&metricsAddr, "metrics-bind-address", "0", "The address the metrics endpoint binds to. "+
		"Use :8443 for HTTPS or :8080 for HTTP, or leave as 0 to disable the metrics service.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "The address the probe endpoint binds to.")
	flag.BoolVar(&enableLeaderElection, "leader-elect", false,
		"Enable leader election for controller manager. "+
			"Enabling this will ensure there is only one active controller manager.")
	flag.BoolVar(&secureMetrics, "metrics-secure", true,
		"If set, the metrics endpoint is served securely via HTTPS. Use --metrics-secure=false to use HTTP instead.")
	flag.BoolVar(&enableWebhook, "enable-webhook", true,
		"If set to false, the webhook endpoint is disabled. "+
			"This is useful for local testing or when the webhook is not needed.")
	flag.StringVar(&webhookCertPath, "webhook-cert-path", "/tmp/k8s-webhook-server/serving-certs",
		"The directory that contains the webhook certificate.")
	flag.StringVar(&webhookCertName, "webhook-cert-name", "tls.crt", "The name of the webhook certificate file.")
	flag.StringVar(&webhookCertKey, "webhook-cert-key", "tls.key", "The name of the webhook key file.")
	flag.StringVar(&metricsCertPath, "metrics-cert-path", "",
		"The directory that contains the metrics server certificate.")
	flag.StringVar(&metricsCertName, "metrics-cert-name", "tls.crt", "The name of the metrics server certificate file.")
	flag.StringVar(&metricsCertKey, "metrics-cert-key", "tls.key", "The name of the metrics server key file.")
	flag.BoolVar(&enableHTTP2, "enable-http2", false,
		"If set, HTTP/2 will be enabled for the metrics and webhook servers")
	flag.StringVar(&searchAPIEndpoint, "search-api-endpoint", "",
		"Full URL of the ACM Search API GraphQL endpoint. "+
			"Defaults to the in-cluster service (search-search-api.open-cluster-management.svc:4010). "+
			"For local testing pass the OpenShift Route URL, e.g. "+
			"https://search-api-open-cluster-management.apps.<hub-domain>/searchapi/graphql")
	flag.StringVar(&thanosHost, "thanos-host", "",
		"Base URL of the Thanos Query Frontend. "+
			"Defaults to the in-cluster MCO service ("+
			"observability-thanos-query-frontend.open-cluster-management-observability.svc:9090). "+
			"For local testing pass the Route URL, e.g. "+
			"https://rbac-query-proxy-open-cluster-management-observability.apps.<hub-domain>")
	flag.StringVar(&advisorAddr, "advisor-addr", ":8082",
		"TCP address the migration advisor API listens on (plain HTTP, no TLS required). "+
			"Example: :8082 or 127.0.0.1:8082")
	flag.DurationVar(&advisorCacheTTL, "advisor-cache-ttl", 0,
		"How long cluster-wide data (node metrics, StorageClasses) is cached by the "+
			"migration advisor. Defaults to 30s when not set.")
	opts := zap.Options{
		Development: true,
	}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	// if the enable-http2 flag is false (the default), http/2 should be disabled
	// due to its vulnerabilities. More specifically, disabling http/2 will
	// prevent from being vulnerable to the HTTP/2 Stream Cancellation and
	// Rapid Reset CVEs. For more information see:
	// - https://github.com/advisories/GHSA-qppj-fm5r-hxr3
	// - https://github.com/advisories/GHSA-4374-p667-p6c8
	disableHTTP2 := func(c *tls.Config) {
		setupLog.Info("disabling http/2")
		c.NextProtos = []string{"http/1.1"}
	}

	if !enableHTTP2 {
		tlsOpts = append(tlsOpts, disableHTTP2)
	}

	// Create watchers for metrics and webhooks certificates
	var metricsCertWatcher, webhookCertWatcher *certwatcher.CertWatcher

	// Initial webhook TLS options
	webhookTLSOpts := tlsOpts

	if enableWebhook && len(webhookCertPath) > 0 {
		setupLog.Info("Initializing webhook certificate watcher using provided certificates",
			"webhook-cert-path", webhookCertPath, "webhook-cert-name", webhookCertName, "webhook-cert-key", webhookCertKey)

		var err error
		webhookCertWatcher, err = certwatcher.New(
			filepath.Join(webhookCertPath, webhookCertName),
			filepath.Join(webhookCertPath, webhookCertKey),
		)
		if err != nil {
			setupLog.Error(err, "Failed to initialize webhook certificate watcher")
			os.Exit(1)
		}

		webhookTLSOpts = append(webhookTLSOpts, func(config *tls.Config) {
			config.GetCertificate = webhookCertWatcher.GetCertificate
		})
	}

	webhookServer := webhook.NewServer(webhook.Options{
		TLSOpts: webhookTLSOpts,
		Port:    9443,
	})

	// Metrics endpoint is enabled in 'config/default/kustomization.yaml'. The Metrics options configure the server.
	// More info:
	// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.20.0/pkg/metrics/server
	// - https://book.kubebuilder.io/reference/metrics.html
	metricsServerOptions := metricsserver.Options{
		BindAddress:   metricsAddr,
		SecureServing: secureMetrics,
		TLSOpts:       tlsOpts,
	}

	if secureMetrics {
		// FilterProvider is used to protect the metrics endpoint with authn/authz.
		// These configurations ensure that only authorized users and service accounts
		// can access the metrics endpoint. The RBAC are configured in 'config/rbac/kustomization.yaml'. More info:
		// https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.20.0/pkg/metrics/filters#WithAuthenticationAndAuthorization
		metricsServerOptions.FilterProvider = filters.WithAuthenticationAndAuthorization
	}

	// If the certificate is not specified, controller-runtime will automatically
	// generate self-signed certificates for the metrics server. While convenient for development and testing,
	// this setup is not recommended for production.
	//
	// TODO(user): If you enable certManager, uncomment the following lines:
	// - [METRICS-WITH-CERTS] at config/default/kustomization.yaml to generate and use certificates
	// managed by cert-manager for the metrics server.
	// - [PROMETHEUS-WITH-CERTS] at config/prometheus/kustomization.yaml for TLS certification.
	if len(metricsCertPath) > 0 {
		setupLog.Info("Initializing metrics certificate watcher using provided certificates",
			"metrics-cert-path", metricsCertPath, "metrics-cert-name", metricsCertName, "metrics-cert-key", metricsCertKey)

		var err error
		metricsCertWatcher, err = certwatcher.New(
			filepath.Join(metricsCertPath, metricsCertName),
			filepath.Join(metricsCertPath, metricsCertKey),
		)
		if err != nil {
			setupLog.Error(err, "to initialize metrics certificate watcher", "error", err)
			os.Exit(1)
		}

		metricsServerOptions.TLSOpts = append(metricsServerOptions.TLSOpts, func(config *tls.Config) {
			config.GetCertificate = metricsCertWatcher.GetCertificate
		})
	}

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsServerOptions,
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         enableLeaderElection,
		LeaderElectionID:       "f8b3c90a.mtv.managedclusters.cluster.open-cluster-management.io",
		// LeaderElectionReleaseOnCancel defines if the leader should step down voluntarily
		// when the Manager ends. This requires the binary to immediately end when the
		// Manager is stopped, otherwise, this setting is unsafe. Setting this significantly
		// speeds up voluntary leader transitions as the new leader don't have to wait
		// LeaseDuration time first.
		//
		// In the default scaffold provided, the program ends immediately after
		// the manager stops, so would be fine to enable this option. However,
		// if you are doing or is intended to do any operation such as perform cleanups
		// after the manager stops then its usage might be unsafe.
		LeaderElectionReleaseOnCancel: true,
	})
	if err != nil {
		setupLog.Error(err, "unable to start manager")
		os.Exit(1)
	}

	dynamicClient, err := dynamic.NewForConfig(mgr.GetConfig())
	if err != nil {
		setupLog.Error(err, "unable to create dynamic client")
		os.Exit(1)
	}

	// Auto-discover Route URLs when flags were not set explicitly.
	// When running in-cluster the Routes are reachable but unnecessary (the
	// in-cluster service URLs are faster); when running locally the Routes are
	// the only way to reach the services.
	if searchAPIEndpoint == "" || thanosHost == "" {
		discoverCtx, discoverCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer discoverCancel()
		discoveredSearch, discoveredThanos, err := discoverAdvisorEndpoints(
			discoverCtx, dynamicClient, searchAPIEndpoint, thanosHost)
		if err != nil {
			setupLog.Error(err, "failed to discover advisor endpoints")
			os.Exit(1)
		}
		if searchAPIEndpoint == "" {
			searchAPIEndpoint = discoveredSearch
		}
		if thanosHost == "" {
			thanosHost = discoveredThanos
		}
	}

	if err = (&controllers.ManagedClusterReconciler{
		Client:        mgr.GetClient(),
		Scheme:        mgr.GetScheme(),
		DynamicClient: dynamicClient,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "MTV-ManagedCluster")
		os.Exit(1)
	}

	if enableWebhook {
		if err := mgr.Add(webhookServer); err != nil {
			os.Exit(1)
		}

		webhookServer.Register("/validate-plan", miwebhook.ValidateWebhook(mgr.GetClient(), *mgr.GetConfig()))
	}

	// Start a dedicated plain-HTTP server for the migration advisor API.
	// This is completely separate from the webhook server so no TLS certificate
	// is required, and the advisor works regardless of whether --enable-webhook
	// is set.
	advisorHandler := &migrationadvisor.Handler{
		DynamicClient:     dynamicClient,
		RestConfig:        mgr.GetConfig(),
		SearchAPIEndpoint: searchAPIEndpoint,
		ThanosHost:        thanosHost,
		CacheTTL:          advisorCacheTTL,
	}
	advisorMux := http.NewServeMux()
	advisorMux.Handle("/api/v1/migration-targets", advisorHandler)
	advisorMux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		obsClient := &migrationadvisor.ObservabilityClient{
			RestConfig: mgr.GetConfig(),
			ThanosHost: thanosHost,
		}
		if err := obsClient.CheckHealth(r.Context()); err != nil {
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
			return
		}

		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	advisorServer := &http.Server{
		Addr:              advisorAddr,
		Handler:           advisorMux,
		ReadTimeout:       30 * time.Second,
		ReadHeaderTimeout: 10 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	if err := mgr.Add(runnableFunc(func(ctx context.Context) error {
		setupLog.Info("Starting migration advisor API server", "addr", advisorAddr)
		errCh := make(chan error, 1)
		go func() { errCh <- advisorServer.ListenAndServe() }()
		select {
		case <-ctx.Done():
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			return advisorServer.Shutdown(shutdownCtx)
		case err := <-errCh:
			return err
		}
	})); err != nil {
		setupLog.Error(err, "unable to add advisor server to manager")
		os.Exit(1)
	}
	// +kubebuilder:scaffold:builder

	if metricsCertWatcher != nil {
		setupLog.Info("Adding metrics certificate watcher to manager")
		if err := mgr.Add(metricsCertWatcher); err != nil {
			setupLog.Error(err, "unable to add metrics certificate watcher to manager")
			os.Exit(1)
		}
	}

	if webhookCertWatcher != nil {
		setupLog.Info("Adding webhook certificate watcher to manager")
		if err := mgr.Add(webhookCertWatcher); err != nil {
			setupLog.Error(err, "unable to add webhook certificate watcher to manager")
			os.Exit(1)
		}
	}

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up ready check")
		os.Exit(1)
	}

	setupLog.Info("starting manager")
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "problem running manager")
		os.Exit(1)
	}
}
