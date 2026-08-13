package resources

import (
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	costv1alpha1 "github.com/project-koku/koku-service-operator/api/v1alpha1"
)

func TestGatewayAPIHostExplicit(t *testing.T) {
	cfg := testCfg()
	cfg.Spec.GatewayRoute.Host = "api.example.com"
	host, ok := GatewayAPIHost(cfg)
	if !ok || host != "api.example.com" {
		t.Errorf("GatewayAPIHost = %q, %v; want api.example.com, true", host, ok)
	}
}

func TestGatewayAPIHostDefaultFromClusterDomain(t *testing.T) {
	cfg := testCfg()
	cfg.Status.DiscoveredConfig = &costv1alpha1.DiscoveredConfig{ClusterDomain: "apps.example.com"}
	host, ok := GatewayAPIHost(cfg)
	want := "cost-management-gateway-cost-onprem.apps.example.com"
	if !ok || host != want {
		t.Errorf("GatewayAPIHost = %q, %v; want %q, true", host, ok, want)
	}
}

func TestGatewayAPIHostMissingDomain(t *testing.T) {
	cfg := testCfg()
	if _, ok := GatewayAPIHost(cfg); ok {
		t.Error("expected ok=false when no host and no cluster domain")
	}
	if GatewayAPIRoute(cfg) != nil {
		t.Error("expected nil Route when host cannot be resolved")
	}
}

func TestGatewayAPIRouteSpec(t *testing.T) {
	cfg := testCfg()
	cfg.Spec.Global.ClusterDomain = "apps.cluster.local"
	cfg.Spec.GatewayRoute.Annotations = map[string]string{"haproxy.router.openshift.io/timeout": "60s"}

	route := GatewayAPIRoute(cfg)
	if route == nil {
		t.Fatal("expected Route")
	}
	if route.GetName() != "cost-management-api" {
		t.Errorf("name = %q", route.GetName())
	}
	if route.GroupVersionKind() != routeGVK {
		t.Errorf("GVK = %v", route.GroupVersionKind())
	}

	host, _, _ := unstructured.NestedString(route.Object, "spec", "host")
	wantHost := "cost-management-gateway-cost-onprem.apps.cluster.local"
	if host != wantHost {
		t.Errorf("spec.host = %q, want %q", host, wantHost)
	}
	path, _, _ := unstructured.NestedString(route.Object, "spec", "path")
	if path != "/api" {
		t.Errorf("spec.path = %q, want /api", path)
	}
	svc, _, _ := unstructured.NestedString(route.Object, "spec", "to", "name")
	if svc != "cost-management-gateway" {
		t.Errorf("spec.to.name = %q", svc)
	}
	port, _, _ := unstructured.NestedString(route.Object, "spec", "port", "targetPort")
	if port != "http" {
		t.Errorf("spec.port.targetPort = %q", port)
	}
	term, _, _ := unstructured.NestedString(route.Object, "spec", "tls", "termination")
	if term != "edge" {
		t.Errorf("tls.termination = %q, want edge", term)
	}
	insecure, _, _ := unstructured.NestedString(route.Object, "spec", "tls", "insecureEdgeTerminationPolicy")
	if insecure != "Redirect" {
		t.Errorf("tls.insecureEdgeTerminationPolicy = %q, want Redirect", insecure)
	}
	if route.GetAnnotations()["haproxy.router.openshift.io/timeout"] != "60s" {
		t.Errorf("annotations = %v", route.GetAnnotations())
	}
}

func TestGatewayAPIRouteTLSOverrides(t *testing.T) {
	cfg := testCfg()
	cfg.Spec.GatewayRoute.Host = "api.example.com"
	cfg.Spec.GatewayRoute.TLS = costv1alpha1.RouteTLSSpec{
		Termination:                   "reencrypt",
		InsecureEdgeTerminationPolicy: "Allow",
	}
	route := GatewayAPIRoute(cfg)
	term, _, _ := unstructured.NestedString(route.Object, "spec", "tls", "termination")
	if term != "reencrypt" {
		t.Errorf("termination = %q", term)
	}
}
