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
		{
			From: []networkingv1.NetworkPolicyPeer{
				{NamespaceSelector: &metav1.LabelSelector{
					MatchLabels: map[string]string{"network.openshift.io/policy-group": "monitoring"},
				}},
				{NamespaceSelector: &metav1.LabelSelector{
					MatchLabels: map[string]string{"kubernetes.io/metadata.name": "openshift-monitoring"},
				}},
			},
			Ports: []networkingv1.NetworkPolicyPort{tcpPort(envoyAdminPort)},
		},
	})
}

// -----------------------------------------------------------------------------
// Ingress upload handler
// -----------------------------------------------------------------------------

// IngressNetworkPolicy allows traffic to the upload handler only from the gateway.
// All uploads must pass through Envoy JWT validation before reaching the handler.
func IngressNetworkPolicy(cfg *costv1alpha1.CostManagementServiceConfig) *networkingv1.NetworkPolicy {
	return netpol(cfg, cfg.Name+"-ingress", "ingress", []networkingv1.NetworkPolicyIngressRule{
		podFrom(cfg, "gateway", ingressHTTPPort),
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
// call the RBAC service for authorization checks.
func RBACAPINetworkPolicy(cfg *costv1alpha1.CostManagementServiceConfig) *networkingv1.NetworkPolicy {
	return netpol(cfg, cfg.Name+"-rbac-api", "rbac-api", []networkingv1.NetworkPolicyIngressRule{
		podFrom(cfg, "gateway", rbacAPIPort),
		podFrom(cfg, "cost-management-api", rbacAPIPort),
		podFrom(cfg, "cost-processor", rbacAPIPort),
		podFrom(cfg, "ros-api", rbacAPIPort),
	})
}

// -----------------------------------------------------------------------------
// ROS API
// -----------------------------------------------------------------------------

// ROSAPINetworkPolicy restricts ingress to the ROS API to the Envoy gateway only.
// Without this, any pod in the namespace can reach ros-api:8000 directly,
// bypassing the Envoy JWT authentication layer entirely.
func ROSAPINetworkPolicy(cfg *costv1alpha1.CostManagementServiceConfig) *networkingv1.NetworkPolicy {
	return netpol(cfg, cfg.Name+"-ros-api", "ros-api", []networkingv1.NetworkPolicyIngressRule{
		podFrom(cfg, "gateway", rosAPIPort),
	})
}

// -----------------------------------------------------------------------------
// Koku API
// -----------------------------------------------------------------------------

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
		// Prometheus scraping metrics port
		{
			From: []networkingv1.NetworkPolicyPeer{
				{NamespaceSelector: &metav1.LabelSelector{
					MatchLabels: map[string]string{"network.openshift.io/policy-group": "monitoring"},
				}},
				{NamespaceSelector: &metav1.LabelSelector{
					MatchLabels: map[string]string{"kubernetes.io/metadata.name": "openshift-monitoring"},
				}},
			},
			Ports: []networkingv1.NetworkPolicyPort{tcpPort(kokuMetricsPort)},
		},
	})
}
