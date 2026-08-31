package resources

import (
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	costv1alpha1 "github.com/project-koku/koku-service-operator/api/v1alpha1"
)

// netpol builds a Ingress-only NetworkPolicy with the given pod selector and rules.
func netpol(cfg *costv1alpha1.CostManagementServiceConfig, name, component string, rules []networkingv1.NetworkPolicyIngressRule) *networkingv1.NetworkPolicy {
	return &networkingv1.NetworkPolicy{
		TypeMeta: metav1.TypeMeta{APIVersion: "networking.k8s.io/v1", Kind: "NetworkPolicy"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: cfg.Namespace,
			Labels:    Labels(cfg, component),
		},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{
				MatchLabels: SelectorLabels(cfg, component),
			},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress},
			Ingress:     rules,
		},
	}
}

func tcpPort(port int32) networkingv1.NetworkPolicyPort {
	p := intstr.FromInt32(port)
	proto := corev1.ProtocolTCP
	return networkingv1.NetworkPolicyPort{Port: &p, Protocol: &proto}
}

// podFrom returns an ingress rule allowing traffic from pods with the given component label.
func podFrom(cfg *costv1alpha1.CostManagementServiceConfig, component string, port int32) networkingv1.NetworkPolicyIngressRule {
	return networkingv1.NetworkPolicyIngressRule{
		From: []networkingv1.NetworkPolicyPeer{{
			PodSelector: &metav1.LabelSelector{
				MatchLabels: SelectorLabels(cfg, component),
			},
		}},
		Ports: []networkingv1.NetworkPolicyPort{tcpPort(port)},
	}
}

// ingressFromPods builds one ingress rule per pod component on the given port.
func ingressFromPods(cfg *costv1alpha1.CostManagementServiceConfig, port int32, pods []string) []networkingv1.NetworkPolicyIngressRule {
	rules := make([]networkingv1.NetworkPolicyIngressRule, 0, len(pods))
	for _, name := range pods {
		rules = append(rules, podFrom(cfg, name, port))
	}
	return rules
}

// monitoringFrom allows OpenShift platform and user-workload Prometheus to
// scrape the given container port.
func monitoringFrom(port int32) networkingv1.NetworkPolicyIngressRule {
	return networkingv1.NetworkPolicyIngressRule{
		From: []networkingv1.NetworkPolicyPeer{
			{NamespaceSelector: &metav1.LabelSelector{
				MatchLabels: map[string]string{"network.openshift.io/policy-group": "monitoring"},
			}},
			{NamespaceSelector: &metav1.LabelSelector{
				MatchLabels: map[string]string{"kubernetes.io/metadata.name": "openshift-monitoring"},
			}},
			{NamespaceSelector: &metav1.LabelSelector{
				MatchLabels: map[string]string{"kubernetes.io/metadata.name": "openshift-user-workload-monitoring"},
			}},
		},
		Ports: []networkingv1.NetworkPolicyPort{tcpPort(port)},
	}
}

// prometheusFrom allows only the OpenShift platform and user-workload
// Prometheus pods to scrape a port. Use this for a metrics endpoint that
// shares its listener with application routes, because NetworkPolicy cannot
// restrict access by HTTP path.
func prometheusFrom(port int32) networkingv1.NetworkPolicyIngressRule {
	return networkingv1.NetworkPolicyIngressRule{
		From: []networkingv1.NetworkPolicyPeer{
			{
				NamespaceSelector: &metav1.LabelSelector{
					MatchLabels: map[string]string{"kubernetes.io/metadata.name": "openshift-monitoring"},
				},
				PodSelector: &metav1.LabelSelector{
					MatchLabels: map[string]string{"app.kubernetes.io/name": "prometheus"},
				},
			},
			{
				NamespaceSelector: &metav1.LabelSelector{
					MatchLabels: map[string]string{"kubernetes.io/metadata.name": "openshift-user-workload-monitoring"},
				},
				PodSelector: &metav1.LabelSelector{
					MatchLabels: map[string]string{"app.kubernetes.io/name": "prometheus"},
				},
			},
		},
		Ports: []networkingv1.NetworkPolicyPort{tcpPort(port)},
	}
}

// -----------------------------------------------------------------------------
// Gateway (Envoy JWT proxy)
// -----------------------------------------------------------------------------

// GatewayNetworkPolicy allows traffic to the Envoy gateway from:
//   - OpenShift router pods (external API access)
//   - UI pods (nginx proxies /api/ through the gateway)
//   - Prometheus / OpenShift monitoring (scrape the admin port)
func GatewayNetworkPolicy(cfg *costv1alpha1.CostManagementServiceConfig) *networkingv1.NetworkPolicy {
	return netpol(cfg, cfg.Name+"-gateway", "gateway", []networkingv1.NetworkPolicyIngressRule{
		// OpenShift router — external traffic through the Route
		{
			From: []networkingv1.NetworkPolicyPeer{
				{NamespaceSelector: &metav1.LabelSelector{
					MatchLabels: map[string]string{"network.openshift.io/policy-group": "ingress"},
				}},
				{NamespaceSelector: &metav1.LabelSelector{
					MatchLabels: map[string]string{"kubernetes.io/metadata.name": "openshift-ingress"},
				}},
			},
			Ports: []networkingv1.NetworkPolicyPort{tcpPort(envoyHTTPPort)},
		},
		// UI nginx proxying /api/ to the gateway
		podFrom(cfg, "ui", envoyHTTPPort),
		// Prometheus scraping admin/metrics port
		monitoringFrom(envoyAdminPort),
	})
}

// -----------------------------------------------------------------------------
// Ingress upload handler
// -----------------------------------------------------------------------------

// IngressNetworkPolicy allows traffic to the upload handler from the gateway
// (JWT-validated uploads) and Prometheus scrape of the metrics port.
func IngressNetworkPolicy(cfg *costv1alpha1.CostManagementServiceConfig) *networkingv1.NetworkPolicy {
	return netpol(cfg, cfg.Name+"-ingress", "ingress", []networkingv1.NetworkPolicyIngressRule{
		podFrom(cfg, "gateway", ingressHTTPPort),
		monitoringFrom(ingressMetricsPort),
	})
}

// -----------------------------------------------------------------------------
// Kruize (resource optimiser)
// -----------------------------------------------------------------------------

// KruizeNetworkPolicy allows ROS processor, recommendation-poller, and
// housekeeper to reach Kruize REST endpoints.
func KruizeNetworkPolicy(cfg *costv1alpha1.CostManagementServiceConfig) *networkingv1.NetworkPolicy {
	const kruizePort = int32(8080)
	return netpol(cfg, cfg.Name+"-kruize", "ros-optimization", []networkingv1.NetworkPolicyIngressRule{
		podFrom(cfg, "ros-processor", kruizePort),
		podFrom(cfg, "ros-recommendation-poller", kruizePort),
		podFrom(cfg, "ros-housekeeper", kruizePort),
	})
}

// -----------------------------------------------------------------------------
// RBAC API
// -----------------------------------------------------------------------------

// RBACAPINetworkPolicy allows the gateway, koku-api, masu, and ros-api to
// call the RBAC service for authorization checks, and Prometheus to scrape
// its metrics endpoint.
func RBACAPINetworkPolicy(cfg *costv1alpha1.CostManagementServiceConfig) *networkingv1.NetworkPolicy {
	return netpol(cfg, cfg.Name+"-rbac-api", "rbac-api", []networkingv1.NetworkPolicyIngressRule{
		podFrom(cfg, "gateway", rbacAPIPort),
		podFrom(cfg, "cost-management-api", rbacAPIPort),
		podFrom(cfg, "cost-processor", rbacAPIPort),
		podFrom(cfg, "ros-api", rbacAPIPort),
		prometheusFrom(rbacAPIPort),
	})
}

// -----------------------------------------------------------------------------
// ROS API
// -----------------------------------------------------------------------------

// ROSAPINetworkPolicy restricts REST access to the ROS API to the Envoy
// gateway (JWT), and allows Prometheus to scrape the metrics port. Without
// the gateway rule, any pod in the namespace can reach ros-api:8000 and
// bypass Envoy authentication.
func ROSAPINetworkPolicy(cfg *costv1alpha1.CostManagementServiceConfig) *networkingv1.NetworkPolicy {
	return netpol(cfg, cfg.Name+"-ros-api", "ros-api", []networkingv1.NetworkPolicyIngressRule{
		podFrom(cfg, "gateway", rosAPIPort),
		monitoringFrom(rosMetricPort),
	})
}

// -----------------------------------------------------------------------------
// Bundled Valkey (cache)
// -----------------------------------------------------------------------------

// CacheNetworkPolicy restricts ingress to the bundled Valkey instance.
// Only Koku workloads (API, Masu, Listener, Celery, RBAC) need cache access.
// The bundled Valkey runs with --protected-mode no (dev-only, no auth); this
// NetworkPolicy is defense-in-depth to prevent arbitrary namespace pods from
// reading/writing the cache.
func CacheNetworkPolicy(cfg *costv1alpha1.CostManagementServiceConfig) *networkingv1.NetworkPolicy {
	cachePort := cfg.Spec.Cache.Port
	if cachePort == 0 {
		cachePort = 6379
	}
	return netpol(cfg, cfg.Name+"-cache", "cache", ingressFromPods(cfg, cachePort, []string{
		"cost-management-api",
		"cost-processor",
		"listener",
		"cost-scheduler",
		"cost-worker-celery",
		"cost-worker-priority",
		"cost-worker-summary",
		"cost-worker-ocp",
		"cost-worker-cost-model",
		"cost-worker-refresh",
		"cost-worker-hcs",
		"cost-worker-download",
		"cost-worker-subs-extraction",
		"cost-worker-subs-transmission",
		"rbac-api",
		"rbac-worker",
	}))
}

// -----------------------------------------------------------------------------
// Bundled Database (PostgreSQL)
// -----------------------------------------------------------------------------

// DatabaseNetworkPolicy restricts ingress to the bundled PostgreSQL instance.
// Only services that connect to the database need access.
func DatabaseNetworkPolicy(cfg *costv1alpha1.CostManagementServiceConfig) *networkingv1.NetworkPolicy {
	dbPort := cfg.Spec.Database.Port
	if dbPort == 0 {
		dbPort = 5432
	}
	return netpol(cfg, cfg.Name+"-database", "database", ingressFromPods(cfg, dbPort, []string{
		"cost-management-api",
		"cost-processor",
		"cost-management-migration",
		"rbac-api",
		"rbac-worker",
		"rbac-migration",
		"rbac-admin-bootstrap",
		"rbac-keycloak-sync",
		"ros-api",
		"ros-processor",
		"ros-recommendation-poller",
		"ros-housekeeper",
		"ros-optimization",
		"ros-migration",
	}))
}

// -----------------------------------------------------------------------------
// UI (oauth2-proxy)
// -----------------------------------------------------------------------------

// UINetworkPolicy allows OpenShift ingress to reach the UI oauth2-proxy on 8443.
func UINetworkPolicy(cfg *costv1alpha1.CostManagementServiceConfig) *networkingv1.NetworkPolicy {
	return netpol(cfg, cfg.Name+"-ui", "ui", []networkingv1.NetworkPolicyIngressRule{{
		From: []networkingv1.NetworkPolicyPeer{
			{NamespaceSelector: &metav1.LabelSelector{
				MatchLabels: map[string]string{"network.openshift.io/policy-group": "ingress"},
			}},
			{NamespaceSelector: &metav1.LabelSelector{
				MatchLabels: map[string]string{"kubernetes.io/metadata.name": "openshift-ingress"},
			}},
		},
		Ports: []networkingv1.NetworkPolicyPort{tcpPort(uiProxyPort)},
	}})
}

// -----------------------------------------------------------------------------
// Listener (Kafka consumer — no Service)
// -----------------------------------------------------------------------------

// ListenerNetworkPolicy denies all inbound traffic to the Kafka listener.
// The listener has no Service; it only consumes from Kafka.
func ListenerNetworkPolicy(cfg *costv1alpha1.CostManagementServiceConfig) *networkingv1.NetworkPolicy {
	return netpol(cfg, cfg.Name+"-listener", "listener", nil)
}

// -----------------------------------------------------------------------------
// Koku API
// -----------------------------------------------------------------------------

// MasuNetworkPolicy restricts ingress to Masu (cost-processor) pods.
// Masu is ClusterIP-only and is not fronted by the Envoy gateway or a public
// Route (COST-8060). In normal runtime no other workload calls Masu over HTTP;
// report processing is driven by Kafka and Celery. This policy blocks casual
// same-namespace access to admin endpoints on port 9000; app-level auth is
// tracked under COST-7841. Prometheus (platform + UWM) may scrape metrics on
// the same port.
func MasuNetworkPolicy(cfg *costv1alpha1.CostManagementServiceConfig) *networkingv1.NetworkPolicy {
	const masuMetricsPort = int32(9000)
	return netpol(cfg, cfg.Name+"-masu", "cost-processor", []networkingv1.NetworkPolicyIngressRule{
		monitoringFrom(masuMetricsPort),
	})
}

// CeleryWorkersNetworkPolicy allows Prometheus to scrape WorkerProbeServer
// /metrics on pods labeled metrics-role=celery-worker (replicas > 0 only).
func CeleryWorkersNetworkPolicy(cfg *costv1alpha1.CostManagementServiceConfig) *networkingv1.NetworkPolicy {
	return &networkingv1.NetworkPolicy{
		TypeMeta: metav1.TypeMeta{APIVersion: "networking.k8s.io/v1", Kind: "NetworkPolicy"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      cfg.Name + "-celery-workers",
			Namespace: cfg.Namespace,
			Labels:    Labels(cfg, "celery-worker"),
		},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{
				MatchLabels: map[string]string{
					labelApp:         cfg.Name,
					labelInstance:    cfg.Name,
					labelMetricsRole: MetricsRoleCeleryWorker,
				},
			},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress},
			Ingress:     []networkingv1.NetworkPolicyIngressRule{monitoringFrom(celeryWorkerMetricsPort)},
		},
	}
}

// KokuAPINetworkPolicy allows the gateway and internal services to reach the
// Koku API on its Service port (8000), and Prometheus to scrape the metrics
// port (9000).
func KokuAPINetworkPolicy(cfg *costv1alpha1.CostManagementServiceConfig) *networkingv1.NetworkPolicy {
	const (
		kokuAPIPort     = int32(8000)
		kokuMetricsPort = int32(9000)
	)
	return netpol(cfg, cfg.Name+"-koku-api", "cost-management-api", []networkingv1.NetworkPolicyIngressRule{
		podFrom(cfg, "gateway", kokuAPIPort),
		podFrom(cfg, "cost-processor", kokuAPIPort),
		podFrom(cfg, "ros-housekeeper", kokuAPIPort),
		monitoringFrom(kokuMetricsPort),
	})
}
