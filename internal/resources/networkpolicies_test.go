package resources

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
)

func TestGatewayNetworkPolicy(t *testing.T) {
	cfg := testCfg()
	np := GatewayNetworkPolicy(cfg)
	if np.Name != cfg.Name+"-gateway" {
		t.Errorf("Name = %q", np.Name)
	}
	assertIngressOnly(t, np)
	if got := np.Spec.PodSelector.MatchLabels[labelComponent]; got != "gateway" {
		t.Errorf("podSelector component = %q", got)
	}
	// Router + UI + monitoring rules.
	if len(np.Spec.Ingress) < 3 {
		t.Fatalf("expected ≥3 ingress rules, got %d", len(np.Spec.Ingress))
	}
	if !ruleAllowsPort(np.Spec.Ingress, envoyHTTPPort) {
		t.Errorf("missing HTTP port %d", envoyHTTPPort)
	}
	if !ruleAllowsPort(np.Spec.Ingress, envoyAdminPort) {
		t.Errorf("missing admin port %d", envoyAdminPort)
	}
}

func TestIngressNetworkPolicy_GatewayAndMonitoring(t *testing.T) {
	cfg := testCfg()
	np := IngressNetworkPolicy(cfg)
	if np.Name != cfg.Name+"-ingress" {
		t.Errorf("Name = %q", np.Name)
	}
	assertIngressOnly(t, np)
	if len(np.Spec.Ingress) != 2 {
		t.Fatalf("expected gateway + monitoring rules, got %d", len(np.Spec.Ingress))
	}
	if !peerHasComponent(np.Spec.Ingress[0], "gateway") {
		t.Error("ingress rule must allow from gateway")
	}
	if !ruleAllowsPort(np.Spec.Ingress, ingressHTTPPort) {
		t.Errorf("missing ingress HTTP port %d", ingressHTTPPort)
	}
	if !ruleAllowsPort(np.Spec.Ingress, ingressMetricsPort) {
		t.Errorf("missing ingress metrics port %d", ingressMetricsPort)
	}
}

func TestKruizeNetworkPolicy(t *testing.T) {
	cfg := testCfg()
	np := KruizeNetworkPolicy(cfg)
	assertIngressOnly(t, np)
	wantFrom := []string{"ros-processor", "ros-recommendation-poller", "ros-housekeeper"}
	for _, comp := range wantFrom {
		found := false
		for _, rule := range np.Spec.Ingress {
			if peerHasComponent(rule, comp) {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("missing peer component %q", comp)
		}
	}
}

func TestRBACAPINetworkPolicy(t *testing.T) {
	cfg := testCfg()
	np := RBACAPINetworkPolicy(cfg)
	assertIngressOnly(t, np)
	wantFrom := []string{"gateway", "cost-management-api", "cost-processor", "ros-api"}
	for _, comp := range wantFrom {
		found := false
		for _, rule := range np.Spec.Ingress {
			if peerHasComponent(rule, comp) {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("missing peer component %q", comp)
		}
	}
}

func TestMasuNetworkPolicy(t *testing.T) {
	const masuPort = int32(9000)
	cfg := testCfg()
	np := MasuNetworkPolicy(cfg)
	if np.Name != cfg.Name+"-masu" {
		t.Errorf("Name = %q", np.Name)
	}
	assertIngressOnly(t, np)
	if got := np.Spec.PodSelector.MatchLabels[labelComponent]; got != "cost-processor" {
		t.Errorf("podSelector component = %q, want cost-processor", got)
	}
	if len(np.Spec.Ingress) != 1 {
		t.Fatalf("expected single monitoring rule, got %d", len(np.Spec.Ingress))
	}
	rule := np.Spec.Ingress[0]
	if len(rule.From) != 3 {
		t.Fatalf("expected 3 monitoring namespace peers, got %d", len(rule.From))
	}
	wantNSLabels := []map[string]string{
		{"network.openshift.io/policy-group": "monitoring"},
		{"kubernetes.io/metadata.name": "openshift-monitoring"},
		{"kubernetes.io/metadata.name": "openshift-user-workload-monitoring"},
	}
	matched := make([]bool, len(wantNSLabels))
	for _, from := range rule.From {
		if from.PodSelector != nil {
			t.Error("masu ingress must not allow pod peers")
		}
		if from.IPBlock != nil {
			t.Error("masu ingress must not allow IPBlock peers")
		}
		if from.NamespaceSelector == nil {
			t.Fatal("masu ingress peer must use NamespaceSelector")
		}
		found := false
		for i, want := range wantNSLabels {
			if mapsEqual(from.NamespaceSelector.MatchLabels, want) {
				if matched[i] {
					t.Errorf("duplicate namespace peer MatchLabels %v", want)
				}
				matched[i] = true
				found = true
				break
			}
		}
		if !found {
			t.Errorf("unexpected namespace peer MatchLabels = %v", from.NamespaceSelector.MatchLabels)
		}
	}
	for i, want := range wantNSLabels {
		if !matched[i] {
			t.Errorf("missing namespace peer MatchLabels %v", want)
		}
	}
	if len(rule.Ports) != 1 {
		t.Fatalf("expected single TCP port, got %d", len(rule.Ports))
	}
	port := rule.Ports[0]
	if port.Port == nil || port.Port.IntVal != masuPort {
		t.Errorf("port = %v, want TCP %d", port.Port, masuPort)
	}
	if port.Protocol == nil || *port.Protocol != corev1.ProtocolTCP {
		t.Errorf("protocol = %v, want TCP", port.Protocol)
	}
}

func TestKokuAPINetworkPolicy(t *testing.T) {
	const (
		kokuAPIPort     = int32(8000)
		kokuMetricsPort = int32(9000)
	)
	cfg := testCfg()
	np := KokuAPINetworkPolicy(cfg)
	assertIngressOnly(t, np)
	if !ruleAllowsPort(np.Spec.Ingress, kokuAPIPort) {
		t.Errorf("missing koku API port %d", kokuAPIPort)
	}
	if !ruleAllowsPort(np.Spec.Ingress, kokuMetricsPort) {
		t.Errorf("missing koku metrics port %d", kokuMetricsPort)
	}
	if !peerHasComponent(np.Spec.Ingress[0], "gateway") {
		t.Error("first rule should allow gateway")
	}
}

func TestCacheNetworkPolicy(t *testing.T) {
	wantFrom := []string{
		"cost-management-api", "cost-processor", "listener", "cost-scheduler",
		"cost-worker-celery", "cost-worker-priority", "cost-worker-summary",
		"cost-worker-ocp", "cost-worker-cost-model", "cost-worker-refresh",
		"cost-worker-hcs", "cost-worker-download",
		"cost-worker-subs-extraction", "cost-worker-subs-transmission",
		"rbac-api", "rbac-worker",
	}

	cfg := testCfg()
	np := CacheNetworkPolicy(cfg)
	if np.Name != cfg.Name+"-cache" {
		t.Errorf("Name = %q", np.Name)
	}
	assertIngressOnly(t, np)
	if got := np.Spec.PodSelector.MatchLabels[labelComponent]; got != "cache" {
		t.Errorf("podSelector component = %q, want cache", got)
	}
	if len(np.Spec.Ingress) != len(wantFrom) {
		t.Fatalf("ingress rules = %d, want %d", len(np.Spec.Ingress), len(wantFrom))
	}
	for _, comp := range wantFrom {
		found := false
		for _, rule := range np.Spec.Ingress {
			if peerHasComponent(rule, comp) {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("missing peer component %q", comp)
		}
	}
	if !ruleAllowsPort(np.Spec.Ingress, 6379) {
		t.Error("missing default cache port 6379")
	}

	cfgCustom := testCfg()
	cfgCustom.Spec.Cache.Port = 6380
	npCustom := CacheNetworkPolicy(cfgCustom)
	if !ruleAllowsPort(npCustom.Spec.Ingress, 6380) {
		t.Errorf("missing custom cache port 6380")
	}
}

func TestDatabaseNetworkPolicy(t *testing.T) {
	wantFrom := []string{
		"cost-management-api", "cost-processor", "cost-management-migration",
		"rbac-api", "rbac-worker", "rbac-migration", "rbac-admin-bootstrap", "rbac-keycloak-sync",
		"ros-api", "ros-processor", "ros-recommendation-poller",
		"ros-housekeeper", "ros-optimization", "ros-migration",
	}

	cfg := testCfg()
	np := DatabaseNetworkPolicy(cfg)
	if np.Name != cfg.Name+"-database" {
		t.Errorf("Name = %q", np.Name)
	}
	assertIngressOnly(t, np)
	if got := np.Spec.PodSelector.MatchLabels[labelComponent]; got != "database" {
		t.Errorf("podSelector component = %q, want database", got)
	}
	if len(np.Spec.Ingress) != len(wantFrom) {
		t.Fatalf("ingress rules = %d, want %d", len(np.Spec.Ingress), len(wantFrom))
	}
	for _, comp := range wantFrom {
		found := false
		for _, rule := range np.Spec.Ingress {
			if peerHasComponent(rule, comp) {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("missing peer component %q", comp)
		}
	}
	if !ruleAllowsPort(np.Spec.Ingress, 5432) {
		t.Error("missing default database port 5432")
	}

	cfgCustom := testCfg()
	cfgCustom.Spec.Database.Port = 5433
	npCustom := DatabaseNetworkPolicy(cfgCustom)
	if !ruleAllowsPort(npCustom.Spec.Ingress, 5433) {
		t.Errorf("missing custom database port 5433")
	}
}

func TestUINetworkPolicy(t *testing.T) {
	cfg := testCfg()
	np := UINetworkPolicy(cfg)
	if np.Name != cfg.Name+"-ui" {
		t.Errorf("Name = %q, want %s-ui", np.Name, cfg.Name)
	}
	assertIngressOnly(t, np)
	if got := np.Spec.PodSelector.MatchLabels[labelComponent]; got != "ui" {
		t.Errorf("podSelector component = %q, want ui", got)
	}
	if len(np.Spec.Ingress) != 1 {
		t.Fatalf("expected 1 ingress rule, got %d", len(np.Spec.Ingress))
	}
	rule := np.Spec.Ingress[0]
	if !ruleHasNamespaceLabel(rule, "network.openshift.io/policy-group", "ingress") {
		t.Error("missing OpenShift ingress policy-group peer")
	}
	if !ruleHasNamespaceLabel(rule, "kubernetes.io/metadata.name", "openshift-ingress") {
		t.Error("missing openshift-ingress namespace peer")
	}
	if !ruleAllowsPort(np.Spec.Ingress, uiProxyPort) {
		t.Errorf("missing UI proxy port %d", uiProxyPort)
	}
}

func TestListenerNetworkPolicy(t *testing.T) {
	cfg := testCfg()
	np := ListenerNetworkPolicy(cfg)
	if np.Name != cfg.Name+"-listener" {
		t.Errorf("Name = %q, want %s-listener", np.Name, cfg.Name)
	}
	assertIngressOnly(t, np)
	if got := np.Spec.PodSelector.MatchLabels[labelComponent]; got != "listener" {
		t.Errorf("podSelector component = %q, want listener", got)
	}
	if len(np.Spec.Ingress) != 0 {
		t.Errorf("Listener Ingress = %+v, want empty (deny all inbound)", np.Spec.Ingress)
	}
}

func TestROSAPINetworkPolicy_GatewayAndMonitoring(t *testing.T) {
	// Gateway on 8000 (JWT-authenticated API traffic) plus Prometheus scrape
	// of the metrics port (matches chart ros-api-metrics NetworkPolicy).
	cfg := testCfg()
	np := ROSAPINetworkPolicy(cfg)
	assertIngressOnly(t, np)
	if len(np.Spec.Ingress) != 2 {
		t.Fatalf("expected gateway + monitoring rules, got %d", len(np.Spec.Ingress))
	}

	var gateway, mon *networkingv1.NetworkPolicyIngressRule
	for i := range np.Spec.Ingress {
		r := &np.Spec.Ingress[i]
		if peerHasComponent(*r, "gateway") {
			gateway = r
		}
		if ruleHasNamespaceLabel(*r, "network.openshift.io/policy-group", "monitoring") {
			mon = r
		}
	}
	if gateway == nil {
		t.Fatal("missing gateway ingress rule")
	}
	if mon == nil {
		t.Fatal("missing monitoring ingress rule")
	}
	if gateway == mon {
		t.Fatal("gateway and monitoring must be separate ingress rules")
	}

	if !ingressRuleAllowsPort(*gateway, rosAPIPort) {
		t.Errorf("gateway rule missing ros API port %d", rosAPIPort)
	}
	if ingressRuleAllowsPort(*gateway, rosMetricPort) {
		t.Errorf("gateway rule must not allow metrics port %d", rosMetricPort)
	}
	if !ingressRuleAllowsPort(*mon, rosMetricPort) {
		t.Errorf("monitoring rule missing ros metrics port %d", rosMetricPort)
	}
	if ingressRuleAllowsPort(*mon, rosAPIPort) {
		t.Errorf("monitoring rule must not allow API port %d", rosAPIPort)
	}
	if len(mon.From) != 3 {
		t.Fatalf("expected 3 monitoring namespace peers, got %d", len(mon.From))
	}
}

func assertIngressOnly(t *testing.T, np *networkingv1.NetworkPolicy) {
	t.Helper()
	if len(np.Spec.PolicyTypes) != 1 || np.Spec.PolicyTypes[0] != networkingv1.PolicyTypeIngress {
		t.Errorf("PolicyTypes = %v, want [Ingress]", np.Spec.PolicyTypes)
	}
}

func ruleHasNamespaceLabel(rule networkingv1.NetworkPolicyIngressRule, key, value string) bool {
	for _, from := range rule.From {
		if from.NamespaceSelector != nil && from.NamespaceSelector.MatchLabels[key] == value {
			return true
		}
	}
	return false
}

func peerHasComponent(rule networkingv1.NetworkPolicyIngressRule, component string) bool {
	for _, from := range rule.From {
		if from.PodSelector != nil && from.PodSelector.MatchLabels[labelComponent] == component {
			return true
		}
	}
	return false
}

func mapsEqual(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

func ruleAllowsPort(rules []networkingv1.NetworkPolicyIngressRule, port int32) bool {
	for _, rule := range rules {
		if ingressRuleAllowsPort(rule, port) {
			return true
		}
	}
	return false
}

func ingressRuleAllowsPort(rule networkingv1.NetworkPolicyIngressRule, port int32) bool {
	for _, p := range rule.Ports {
		if p.Port != nil && p.Port.IntVal == port {
			if p.Protocol == nil || *p.Protocol == corev1.ProtocolTCP {
				return true
			}
		}
	}
	return false
}
