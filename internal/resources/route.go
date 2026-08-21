package resources

import (
	"fmt"
	"maps"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	costv1alpha1 "github.com/project-koku/koku-service-operator/api/v1alpha1"
)

const (
	gatewayRouteTimeoutAnnotation = "haproxy.router.openshift.io/timeout"
	gatewayRouteTimeoutDefault    = "180s"
)

var routeGVK = schema.GroupVersionKind{
	Group:   "route.openshift.io",
	Version: "v1",
	Kind:    "Route",
}

// GatewayAPIHost returns the OpenShift Route host for the API gateway.
// When no explicit host is set, it uses {name}-gateway-{ns}.{clusterDomain}.
// ok is false when neither an explicit host nor a cluster domain is available.
func GatewayAPIHost(cfg *costv1alpha1.CostManagementServiceConfig) (host string, ok bool) {
	if cfg.Spec.GatewayRoute.Host != "" {
		return cfg.Spec.GatewayRoute.Host, true
	}
	domain := clusterDomain(cfg)
	if domain == "" {
		return "", false
	}
	return fmt.Sprintf("%s-gateway-%s.%s", cfg.Name, cfg.Namespace, domain), true
}

// clusterDomain returns status.discoveredConfig.clusterDomain, falling back to
// spec.global.clusterDomain. Empty when neither is set.
func clusterDomain(cfg *costv1alpha1.CostManagementServiceConfig) string {
	if cfg.Status.DiscoveredConfig != nil && cfg.Status.DiscoveredConfig.ClusterDomain != "" {
		return cfg.Status.DiscoveredConfig.ClusterDomain
	}
	return cfg.Spec.Global.ClusterDomain
}

// GatewayAPIRoute builds the OpenShift Route that fronts Envoy at path /api.
// Uses unstructured so github.com/openshift/api is not a dependency.
// Returns nil when a host cannot be resolved (caller should skip apply + requeue).
func GatewayAPIRoute(cfg *costv1alpha1.CostManagementServiceConfig) *unstructured.Unstructured {
	host, ok := GatewayAPIHost(cfg)
	if !ok {
		return nil
	}

	termination := cfg.Spec.GatewayRoute.TLS.Termination
	if termination != "edge" {
		termination = "edge"
	}
	insecurePolicy := cfg.Spec.GatewayRoute.TLS.InsecureEdgeTerminationPolicy
	if insecurePolicy == "" {
		insecurePolicy = "Redirect"
	}

	route := &unstructured.Unstructured{}
	route.SetGroupVersionKind(routeGVK)
	route.SetName(NameAPIRoute(cfg))
	route.SetNamespace(cfg.Namespace)
	route.SetLabels(Labels(cfg, envoyComponent))

	// Default timeout matches Helm and the longest Envoy route (/api/ingress/).
	// CR annotations overlay this map; omitting the timeout key keeps 180s.
	annotations := map[string]string{
		gatewayRouteTimeoutAnnotation: gatewayRouteTimeoutDefault,
	}
	maps.Copy(annotations, cfg.Spec.GatewayRoute.Annotations)
	route.SetAnnotations(annotations)

	_ = unstructured.SetNestedField(route.Object, host, "spec", "host")
	_ = unstructured.SetNestedField(route.Object, "/api", "spec", "path")
	_ = unstructured.SetNestedMap(route.Object, map[string]any{
		"kind":   "Service",
		"name":   NameEnvoy(cfg),
		"weight": int64(100),
	}, "spec", "to")
	_ = unstructured.SetNestedField(route.Object, "http", "spec", "port", "targetPort")
	_ = unstructured.SetNestedMap(route.Object, map[string]any{
		"termination":                   termination,
		"insecureEdgeTerminationPolicy": insecurePolicy,
	}, "spec", "tls")

	return route
}
