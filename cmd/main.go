package main

import (
	"flag"
	"fmt"
	"os"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	costv1alpha1 "github.com/project-koku/koku-service-operator/api/v1alpha1"
	"github.com/project-koku/koku-service-operator/internal/controller"
	"github.com/project-koku/koku-service-operator/internal/resources"
)

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(costv1alpha1.AddToScheme(scheme))
	_ = appsv1.AddToScheme(scheme)
	_ = batchv1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)
}

// serviceAccountNamespacePath is the in-cluster namespace file. Tests override
// it so inCluster() can be exercised without a real kubelet mount.
var serviceAccountNamespacePath = "/var/run/secrets/kubernetes.io/serviceaccount/namespace"

// inCluster reports whether the process is running inside a pod. Presence of
// the SA namespace file is the signal — not its contents. AllNamespaces must
// not treat that file as the watch namespace (that would pin OLM AllNamespaces
// installs to the operator pod NS).
func inCluster() bool {
	_, err := os.Stat(serviceAccountNamespacePath)
	return err == nil
}

// watchNamespace returns a namespace to pin the informer cache, or "" to
// watch all namespaces (AllNamespaces — the product install mode).
//
//   - WATCH_NAMESPACE set: pin to that namespace (OLMv0 Own/Single escape
//     hatch; not advertised in the CSV).
//   - In-cluster, WATCH_NAMESPACE empty: watch all (OLMv0 AllNamespaces
//     OperatorGroup and OLMv1 ClusterExtension with no watchNamespace).
//   - Out-of-cluster: NAMESPACE pins the cache for laptop `make run`.
//
// BYOI infrastructure may live in other namespaces; the operator connects to
// it via CR fields and does not own those namespaces.
func watchNamespace() string {
	if ns := strings.TrimSpace(os.Getenv("WATCH_NAMESPACE")); ns != "" {
		return ns
	}
	if inCluster() {
		return ""
	}
	return strings.TrimSpace(os.Getenv("NAMESPACE"))
}

// cacheOptionsForNamespace pins DefaultNamespaces when ns is set. Empty ns
// is cluster-wide cache (AllNamespaces).
func cacheOptionsForNamespace(ns string) cache.Options {
	if ns == "" {
		return cache.Options{}
	}
	return cache.Options{
		DefaultNamespaces: map[string]cache.Config{
			ns: {},
		},
	}
}

func main() {
	var (
		metricsAddr      string
		probeAddr        string
		leaderElect      bool
		leaderElectionID string
		developmentMode  bool
		operatorImage    string
	)

	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8080", "Address for the metrics endpoint.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "Address for health probes.")
	flag.BoolVar(&leaderElect, "leader-elect", false, "Enable leader election for controller manager.")
	flag.StringVar(&leaderElectionID, "leader-election-id", "costmanagementserviceconfigs.service.costmanagement.openshift.io", "Leader election resource ID.")
	flag.BoolVar(&developmentMode, "dev", false, "Development mode: skip registering admission webhooks (no TLS certs required) and enable zap development logging. Do not pass on in-cluster Deployments.")
	// --operator-image is the fully-qualified image reference for this operator pod.
	// It is used as the image for wait-for init containers so no separate image is needed.
	// Set it to match the registry and tag in your environment:
	//   --operator-image=quay.io/my-org/koku-service-operator:v1.2.3
	// In OLM deployments the CSV injects this via the Deployment args.
	// Local: IMG=... make run  (Makefile passes --operator-image=$(IMG) and --dev).
	flag.StringVar(&operatorImage, "operator-image", "", "operator image used for init containers (registry/name:tag)")
	opts := zap.Options{}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()
	// DevelopmentMode must be set after Parse — flag defaults are false until then.
	opts.Development = developmentMode

	if operatorImage == "" {
		_, _ = fmt.Fprintln(os.Stderr, "error: --operator-image is required")
		_, _ = fmt.Fprintln(os.Stderr, "  Used as the image for wait-for init containers on Jobs.")
		_, _ = fmt.Fprintln(os.Stderr, "  Examples:")
		_, _ = fmt.Fprintln(os.Stderr, "    IMG=quay.io/project-koku/koku-service-operator:v0.0.1 make run")
		_, _ = fmt.Fprintln(os.Stderr, "    go run ./cmd/main.go --dev --operator-image=quay.io/.../koku-service-operator:tag")
		_, _ = fmt.Fprintln(os.Stderr, "  In-cluster lab: IMG=... ./hack/deploy-incluster.sh <ns> (passes the flag for you).")
		os.Exit(1)
	}
	resources.OperatorImage = operatorImage

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	ns := watchNamespace()
	if ns == "" {
		setupLog.Info("AllNamespaces: watching CostManagementServiceConfig in every namespace")
	} else {
		setupLog.Info("restricting informer cache to namespace", "namespace", ns)
	}

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                 scheme,
		Cache:                  cacheOptionsForNamespace(ns),
		Metrics:                metricsserver.Options{BindAddress: metricsAddr},
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         leaderElect,
		LeaderElectionID:       leaderElectionID,
	})
	if err != nil {
		setupLog.Error(err, "unable to start manager")
		os.Exit(1)
	}

	if err = (&controller.CostManagementServiceConfigReconciler{
		Client:    mgr.GetClient(),
		APIReader: mgr.GetAPIReader(),
		Scheme:    mgr.GetScheme(),
		Recorder:  mgr.GetEventRecorderFor("koku-service-operator"),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "CostManagementServiceConfig")
		os.Exit(1)
	}

	if developmentMode {
		setupLog.Info("dev mode: skipping admission webhook registration (no TLS serving certs required)")
	} else if err = (&costv1alpha1.CostManagementServiceConfig{}).SetupWebhookWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create webhook", "webhook", "CostManagementServiceConfig")
		os.Exit(1)
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
