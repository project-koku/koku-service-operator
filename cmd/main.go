package main

import (
	"flag"
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
// it to exercise the SA-file branch without a real kubelet mount.
var serviceAccountNamespacePath = "/var/run/secrets/kubernetes.io/serviceaccount/namespace"

// watchNamespace returns the namespace this OwnNamespace operator instance
// watches and manages. In-cluster: the pod's service-account namespace file
// (install NS == watch NS). Out-of-cluster (dev): NAMESPACE env var.
//
// BYOI infrastructure may live in other namespaces; the operator connects to
// it via CR fields and does not informer-watch those namespaces.
func watchNamespace() string {
	if data, err := os.ReadFile(serviceAccountNamespacePath); err == nil {
		if ns := strings.TrimSpace(string(data)); ns != "" {
			return ns
		}
	}
	return os.Getenv("NAMESPACE")
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
	flag.BoolVar(&developmentMode, "dev", false, "Enable development mode (verbose logging).")
	// --operator-image is the fully-qualified image reference for this operator pod.
	// It is used as the image for wait-for init containers so no separate image is needed.
	// Set it to match the registry and tag in your environment:
	//   --operator-image=quay.io/my-org/koku-service-operator:v1.2.3
	// In OLM deployments the CSV injects this via the Deployment args.
	flag.StringVar(&operatorImage, "operator-image", "", "operator image used for init containers (registry/name:tag)")
	opts := zap.Options{Development: developmentMode}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	if operatorImage == "" {
		setupLog.Error(nil, "--operator-image is required: set it to the fully-qualified image reference of this operator pod (e.g. quay.io/project-koku/koku-service-operator:v1.0.0)")
		os.Exit(1)
	}
	resources.OperatorImage = operatorImage

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	// OwnNamespace: restrict the informer cache to the watched namespace so a
	// compromised or misconfigured operator cannot list Secrets/Jobs cluster-wide.
	// Cluster-scoped resources (StorageClass, ConsoleLink, …) are unaffected.
	ns := watchNamespace()
	if ns == "" {
		setupLog.Error(nil, "unable to determine watch namespace — set NAMESPACE when running out-of-cluster")
		os.Exit(1)
	}
	setupLog.Info("OwnNamespace: restricting cache to namespace", "namespace", ns)

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme: scheme,
		Cache: cache.Options{
			DefaultNamespaces: map[string]cache.Config{
				ns: {},
			},
		},
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
