package resources

import (
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	costv1alpha1 "github.com/project-koku/koku-service-operator/api/v1alpha1"
)

func testMonitoringCFG() *costv1alpha1.CostManagementServiceConfig {
	return &costv1alpha1.CostManagementServiceConfig{
		ObjectMeta: metav1.ObjectMeta{Name: "cost-management", Namespace: "cost-onprem"},
	}
}

func alertNamesFromRules(t *testing.T, pr *unstructured.Unstructured) map[string]bool {
	t.Helper()
	groups, found, err := unstructured.NestedSlice(pr.Object, "spec", "groups")
	if err != nil || !found || len(groups) == 0 {
		t.Fatalf("expected PrometheusRule groups, found=%v err=%v", found, err)
	}
	group, ok := groups[0].(map[string]any)
	if !ok {
		t.Fatalf("group[0] type %T", groups[0])
	}
	rules, ok := group["rules"].([]any)
	if !ok {
		t.Fatalf("rules type %T", group["rules"])
	}
	names := make(map[string]bool, len(rules))
	for _, r := range rules {
		rm, ok := r.(map[string]any)
		if !ok {
			t.Fatalf("rule type %T", r)
		}
		name, _ := rm["alert"].(string)
		if name == "" {
			t.Fatal("rule missing alert name")
		}
		names[name] = true
	}
	return names
}

func TestPrometheusRules_BetaOperatorCentricSet(t *testing.T) {
	pr := PrometheusRules(testMonitoringCFG())
	if pr.GetName() != "cost-management-alerts" {
		t.Errorf("name: got %q", pr.GetName())
	}
	if pr.GetNamespace() != "cost-onprem" {
		t.Errorf("namespace: got %q", pr.GetNamespace())
	}

	names := alertNamesFromRules(t, pr)

	want := []string{
		"CostManagementMigrationFailed",
		"CostManagementMigrationStalled",
		"CostManagementDegraded",
		"CostManagementDependencyDown",
		"CostManagementPodRestarting",
		"CostManagementNotAvailable",
		"CostManagementAPIDown",
		"CostManagementReconcileFailure",
	}
	for _, a := range want {
		if !names[a] {
			t.Errorf("missing alert %s", a)
		}
	}

	absent := []string{
		"CostManagementSchemaOutOfDate",
		"CostManagementNotProgressing",
		"CostManagementSecretRotated",  // deferred until COST-7694 emit path
		"CostManagementDriftCorrected", // deferred until G4 emit path
		"CostManagementCeleryBacklog",  // deferred until Celery worker scrape
	}
	for _, a := range absent {
		if names[a] {
			t.Errorf("unexpected alert %s (deferred or replaced)", a)
		}
	}
}

func TestPrometheusRules_ConditionAlertsUseOperatorGauges(t *testing.T) {
	pr := PrometheusRules(testMonitoringCFG())
	expr := prometheusRuleExpr(t, pr, "CostManagementDegraded")
	if !strings.Contains(expr, `costmanagement_condition{`) || !strings.Contains(expr, `type="Degraded"`) {
		t.Fatalf("Degraded should use costmanagement_condition gauges: %s", expr)
	}
	if strings.Contains(expr, "kube_customresource_status_condition") {
		t.Fatalf("Degraded must not use kube_customresource_status_condition: %s", expr)
	}
}

func TestOperatorServiceMonitor_HTTPScheme(t *testing.T) {
	sm := OperatorServiceMonitor(testMonitoringCFG())
	if sm.GetName() != "cost-management-operator-metrics" {
		t.Errorf("name: got %q", sm.GetName())
	}
	endpoints, found, err := unstructured.NestedSlice(sm.Object, "spec", "endpoints")
	if err != nil || !found || len(endpoints) != 1 {
		t.Fatalf("endpoints: found=%v len=%d err=%v", found, len(endpoints), err)
	}
	ep := endpoints[0].(map[string]any)
	if ep["scheme"] != "http" {
		t.Errorf("scheme: got %v want http (SecureServing unset)", ep["scheme"])
	}
	if ep["port"] != "https" {
		t.Errorf("port name: got %v want https (Service port name)", ep["port"])
	}
	sel, found, err := unstructured.NestedStringMap(sm.Object, "spec", "selector", "matchLabels")
	if err != nil || !found {
		t.Fatalf("selector.matchLabels: found=%v err=%v", found, err)
	}
	if sel["control-plane"] != "controller-manager" {
		t.Errorf("control-plane label: got %q", sel["control-plane"])
	}
	if sel["app.kubernetes.io/name"] != "koku-service-operator" {
		t.Errorf("app.kubernetes.io/name: got %q want koku-service-operator", sel["app.kubernetes.io/name"])
	}
}

func TestGatewayServiceMonitor(t *testing.T) {
	sm := GatewayServiceMonitor(testMonitoringCFG())
	if sm.GetName() != "cost-management-gateway-metrics" {
		t.Errorf("name: got %q", sm.GetName())
	}
	endpoints, found, err := unstructured.NestedSlice(sm.Object, "spec", "endpoints")
	if err != nil || !found || len(endpoints) != 1 {
		t.Fatalf("endpoints: found=%v len=%d err=%v", found, len(endpoints), err)
	}
	ep := endpoints[0].(map[string]any)
	if ep["port"] != "admin" {
		t.Errorf("port: got %v want admin", ep["port"])
	}
	if ep["path"] != "/stats/prometheus" {
		t.Errorf("path: got %v want /stats/prometheus", ep["path"])
	}
}

func TestPrometheusRules_ReconcileFailureFiresOnAnyRecentError(t *testing.T) {
	pr := PrometheusRules(testMonitoringCFG())
	groups, _, _ := unstructured.NestedSlice(pr.Object, "spec", "groups")
	group := groups[0].(map[string]any)
	for _, r := range group["rules"].([]any) {
		rm := r.(map[string]any)
		if rm["alert"] != "CostManagementReconcileFailure" {
			continue
		}
		if rm["for"] != "0m" {
			t.Fatalf("ReconcileFailure for=%v want 0m (any recent error in increase window)", rm["for"])
		}
		return
	}
	t.Fatal("CostManagementReconcileFailure rule missing")
}

func TestPrometheusRules_APIDownTreatsAbsentUp(t *testing.T) {
	pr := PrometheusRules(testMonitoringCFG())
	expr := prometheusRuleExpr(t, pr, "CostManagementAPIDown")
	wantUp := `up{namespace="cost-onprem",service="cost-management-koku-api"} == 0`
	wantAbsent := `absent(up{namespace="cost-onprem",service="cost-management-koku-api"}) == 1`
	if !strings.Contains(expr, wantUp) {
		t.Fatalf("APIDown missing up==0 clause with service= label: %s", expr)
	}
	if !strings.Contains(expr, wantAbsent) {
		t.Fatalf("APIDown missing absent(up) clause (COST-8109): %s", expr)
	}
	if !strings.Contains(expr, " or ") {
		t.Fatalf("APIDown expected or of up==0 and absent(up): %s", expr)
	}
	if strings.Contains(expr, `job="`) {
		t.Fatalf("APIDown must use service= (App ServiceMonitor), not job=: %s", expr)
	}
}

func prometheusRuleExpr(t *testing.T, pr *unstructured.Unstructured, alert string) string {
	t.Helper()
	groups, found, err := unstructured.NestedSlice(pr.Object, "spec", "groups")
	if err != nil || !found || len(groups) == 0 {
		t.Fatalf("expected PrometheusRule groups, found=%v err=%v", found, err)
	}
	g0, ok := groups[0].(map[string]any)
	if !ok {
		t.Fatalf("group[0] type %T", groups[0])
	}
	rules, ok := g0["rules"].([]any)
	if !ok {
		t.Fatalf("rules type %T", g0["rules"])
	}
	var matches []string
	for _, raw := range rules {
		rule, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if rule["alert"] == alert {
			expr, _ := rule["expr"].(string)
			matches = append(matches, expr)
		}
	}
	if len(matches) == 0 {
		t.Fatalf("alert %q not found", alert)
	}
	if len(matches) != 1 {
		t.Fatalf("expected exactly one %q rule, got %d", alert, len(matches))
	}
	return matches[0]
}

func TestAppServiceMonitor_BetaComponents(t *testing.T) {
	sm := AppServiceMonitor(testMonitoringCFG())
	if sm.GetName() != "cost-management-app-metrics" {
		t.Errorf("name: got %q", sm.GetName())
	}
	if sm.GroupVersionKind().Kind != "ServiceMonitor" {
		t.Errorf("kind: got %s", sm.GroupVersionKind().Kind)
	}

	endpoints, found, err := unstructured.NestedSlice(sm.Object, "spec", "endpoints")
	if err != nil || !found || len(endpoints) != 1 {
		t.Fatalf("endpoints: found=%v len=%d err=%v", found, len(endpoints), err)
	}
	ep, ok := endpoints[0].(map[string]any)
	if !ok {
		t.Fatalf("endpoint type %T", endpoints[0])
	}
	if ep["port"] != "metrics" {
		t.Errorf("port: got %v want metrics", ep["port"])
	}
	if ep["path"] != "/metrics" {
		t.Errorf("path: got %v", ep["path"])
	}

	exprs, found, err := unstructured.NestedSlice(sm.Object, "spec", "selector", "matchExpressions")
	if err != nil || !found || len(exprs) != 1 {
		t.Fatalf("matchExpressions: found=%v len=%d err=%v", found, len(exprs), err)
	}
	expr, ok := exprs[0].(map[string]any)
	if !ok {
		t.Fatalf("expr type %T", exprs[0])
	}
	values, ok := expr["values"].([]any)
	if !ok {
		t.Fatalf("values type %T", expr["values"])
	}
	got := map[string]bool{}
	for _, v := range values {
		s, _ := v.(string)
		got[s] = true
	}
	for _, want := range []string{"cost-management-api", "cost-processor", "ingress"} {
		if !got[want] {
			t.Errorf("missing component %q", want)
		}
	}
	for _, absent := range []string{"listener", "ros-api", "ros-optimization", "gateway"} {
		if got[absent] {
			t.Errorf("unexpected component %q", absent)
		}
	}
}
