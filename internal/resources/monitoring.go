package resources

import (
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	costv1alpha1 "github.com/project-koku/koku-service-operator/api/v1alpha1"
)

var (
	serviceMonitorGVK = schema.GroupVersionKind{Group: "monitoring.coreos.com", Version: "v1", Kind: "ServiceMonitor"}
	prometheusRuleGVK = schema.GroupVersionKind{Group: "monitoring.coreos.com", Version: "v1", Kind: "PrometheusRule"}
)

// serviceMonitor builds a ServiceMonitor that selects Services by component label.
func serviceMonitor(cfg *costv1alpha1.CostManagementServiceConfig, name, portName, path string, components []string) *unstructured.Unstructured {
	matchExpressions := make([]any, len(components))
	for i, c := range components {
		matchExpressions[i] = c
	}

	sm := &unstructured.Unstructured{}
	sm.SetGroupVersionKind(serviceMonitorGVK)
	sm.SetName(name)
	sm.SetNamespace(cfg.Namespace)
	sm.SetLabels(Labels(cfg, "monitoring"))

	_ = unstructured.SetNestedSlice(sm.Object, []any{
		map[string]any{
			"port":     portName,
			"path":     path,
			"interval": "30s",
		},
	}, "spec", "endpoints")
	_ = unstructured.SetNestedField(sm.Object, map[string]any{
		"matchLabels": map[string]any{
			"app.kubernetes.io/managed-by": "koku-service-operator",
			"app.kubernetes.io/instance":   cfg.Name,
		},
		"matchExpressions": []any{
			map[string]any{
				"key":      "app.kubernetes.io/component",
				"operator": "In",
				"values":   matchExpressions,
			},
		},
	}, "spec", "selector")
	// Target only the CR's own namespace.
	_ = unstructured.SetNestedSlice(sm.Object, []any{cfg.Namespace}, "spec", "namespaceSelector", "matchNames")

	return sm
}

// AppServiceMonitor scrapes beta managed workloads that expose Prometheus
// /metrics on a Service port named "metrics": Koku API, Masu, and Ingress.
// Listener / ROS / Kruize / Gateway are intentionally excluded (no named metrics
// port, wrong path, or out of beta).
func AppServiceMonitor(cfg *costv1alpha1.CostManagementServiceConfig) *unstructured.Unstructured {
	return serviceMonitor(cfg, cfg.Name+"-app-metrics", "metrics", "/metrics", []string{
		"cost-management-api", "cost-processor", "ingress",
	})
}

// KruizeServiceMonitor watches Kruize's Quarkus metrics on the http Service
// port (8080) at /q/metrics, matching the Helm chart. The Service has no
// port named "metrics". Not applied in beta; retained for ROS cleanup when
// ros.enabled flips false.
func KruizeServiceMonitor(cfg *costv1alpha1.CostManagementServiceConfig) *unstructured.Unstructured {
	return serviceMonitor(cfg, cfg.Name+"-kruize-metrics", "http", "/q/metrics", []string{"ros-optimization"})
}

// OperatorServiceMonitor watches the controller-manager metrics endpoint from
// the operator namespace. It is shipped as a singleton with the operator
// bundle, rather than created once per CMSC.
func OperatorServiceMonitor(namespace string) *unstructured.Unstructured {
	sm := &unstructured.Unstructured{}
	sm.SetGroupVersionKind(serviceMonitorGVK)
	sm.SetName("controller-manager-metrics-monitor")
	sm.SetNamespace(namespace)
	sm.SetLabels(map[string]string{
		"control-plane":                "controller-manager",
		"app.kubernetes.io/name":       "koku-service-operator",
		"app.kubernetes.io/managed-by": "kustomize",
	})

	_ = unstructured.SetNestedSlice(sm.Object, []any{
		map[string]any{
			"port":            "https",
			"path":            "/metrics",
			"interval":        "30s",
			"scheme":          "http",
			"bearerTokenFile": "/var/run/secrets/kubernetes.io/serviceaccount/token",
		},
	}, "spec", "endpoints")
	_ = unstructured.SetNestedStringMap(sm.Object, map[string]string{
		"control-plane":          "controller-manager",
		"app.kubernetes.io/name": "koku-service-operator",
	}, "spec", "selector", "matchLabels")

	return sm
}

// LegacyOperatorServiceMonitor identifies the per-CMSC ServiceMonitor created
// by versions before COST-8519. It is retained only so reconciliation can
// remove objects left behind by an upgrade.
func LegacyOperatorServiceMonitor(cfg *costv1alpha1.CostManagementServiceConfig) *unstructured.Unstructured {
	sm := OperatorServiceMonitor(cfg.Namespace)
	sm.SetName(cfg.Name + "-operator-metrics")
	sm.SetLabels(Labels(cfg, "monitoring"))
	_ = unstructured.SetNestedSlice(sm.Object, []any{cfg.Namespace}, "spec", "namespaceSelector", "matchNames")
	return sm
}

// GatewayServiceMonitor scrapes Envoy admin Prometheus stats (G2 leftover).
func GatewayServiceMonitor(cfg *costv1alpha1.CostManagementServiceConfig) *unstructured.Unstructured {
	return serviceMonitor(cfg, cfg.Name+"-gateway-metrics", "admin", "/stats/prometheus", []string{"gateway"})
}

// CeleryServiceMonitor scrapes Cost Management Celery worker WorkerProbeServer
// /metrics (queue backlog gauges) via the aggregated celery-workers Service.
func CeleryServiceMonitor(cfg *costv1alpha1.CostManagementServiceConfig) *unstructured.Unstructured {
	return serviceMonitor(cfg, cfg.Name+"-celery-metrics", "metrics", "/metrics", []string{"celery-worker"})
}

// RBACServiceMonitor scrapes the RBAC API Prometheus endpoint through its
// existing http Service port.
func RBACServiceMonitor(cfg *costv1alpha1.CostManagementServiceConfig) *unstructured.Unstructured {
	return serviceMonitor(cfg, cfg.Name+"-rbac-metrics", "http", "/metrics", []string{"rbac-api"})
}

// PrometheusRules returns the per-CMSC application-metric alerts. Operator
// metric alerts live in OperatorPrometheusRules, a singleton shipped in the
// operator namespace, so they can evaluate series for CMSCs in any namespace.
func PrometheusRules(cfg *costv1alpha1.CostManagementServiceConfig) *unstructured.Unstructured {
	instance := cfg.Name
	ns := cfg.Namespace
	kokuAPIService := NameKokuAPI(cfg)
	// cr_name avoids overwriting Prometheus's reserved "instance" (scrape target).
	labels := func(severity string) map[string]any {
		return map[string]any{
			"severity": severity,
			"cr_name":  instance,
		}
	}
	rules := make([]any, 1, 1+len(celeryBacklogQueues))
	rules[0] = map[string]any{
		"alert": "CostManagementAPIDown",
		"expr": `(up{namespace="` + ns + `",service="` + kokuAPIService + `"} == 0)` +
			` or (absent(up{namespace="` + ns + `",service="` + kokuAPIService + `"}) == 1)`,
		"for":    "5m",
		"labels": labels("critical"),
		"annotations": map[string]any{
			"summary":     "Cost Management API metrics endpoint unreachable",
			"description": "Prometheus cannot scrape /metrics on Service {{ $labels.service }} in namespace {{ $labels.namespace }} (down or absent) for more than 5 minutes.",
		},
	}
	// One Prometheus rule per queue. OpenShift Observe groups by alert name and
	// does not show labels in the Alerts table, so a shared CostManagementCeleryBacklog
	// name is indistinguishable across queues (COST-7692).
	rules = append(rules, celeryBacklogRules(ns, labels)...)
	return prometheusRules(cfg.Name+"-alerts", cfg.Namespace, Labels(cfg, "monitoring"), rules)
}

// OperatorPrometheusRules contains alerts based only on operator-owned metric
// series. It is intentionally namespace-agnostic: the singleton rule is
// installed beside the operator metrics Service and matches CMSCs in every
// namespace. The custom labels avoid collisions with Prometheus target labels.
func OperatorPrometheusRules() *unstructured.Unstructured {
	condition := func(typ, status string) string {
		return `costmanagement_condition{type="` + typ + `",status="` + status + `"} == 1`
	}
	labels := func(severity string) map[string]any {
		return map[string]any{"severity": severity}
	}
	rules := []any{
		map[string]any{
			"alert":  "CostManagementMigrationFailed",
			"expr":   `costmanagement_migration_job_failed == 1`,
			"for":    "1m",
			"labels": labels("critical"),
			"annotations": map[string]any{
				"summary":     "Cost Management migration job failed",
				"description": "Migration job {{ $labels.migration_job }} has failed for {{ $labels.cr_namespace }}/{{ $labels.name }}. Schema upgrades are blocked.",
			},
		},
		map[string]any{
			"alert":  "CostManagementMigrationStalled",
			"expr":   condition("SchemaUpToDate", "False"),
			"for":    "10m",
			"labels": labels("warning"),
			"annotations": map[string]any{
				"summary":     "Cost Management schema migration stalled",
				"description": "Database schema is not up to date for {{ $labels.cr_namespace }}/{{ $labels.name }} for more than 10 minutes.",
			},
		},
		map[string]any{
			"alert":  "CostManagementDegraded",
			"expr":   condition("Degraded", "True"),
			"for":    "5m",
			"labels": labels("critical"),
			"annotations": map[string]any{
				"summary":     "Cost Management operator is degraded",
				"description": "The CostManagementServiceConfig {{ $labels.cr_namespace }}/{{ $labels.name }} has been in Degraded state for 5 minutes.",
			},
		},
		map[string]any{
			"alert":  "CostManagementDependencyDown",
			"expr":   `(` + condition("DatabaseReady", "False") + `) or (` + condition("CacheReady", "False") + `)`,
			"for":    "5m",
			"labels": labels("critical"),
			"annotations": map[string]any{
				"summary":     "Cost Management dependency validation failed",
				"description": "DatabaseReady or CacheReady is False on {{ $labels.cr_namespace }}/{{ $labels.name }} for more than 5 minutes.",
			},
		},
		map[string]any{
			"alert":  "CostManagementPodRestarting",
			"expr":   `costmanagement_managed_pod_restarts > 3`,
			"for":    "15m",
			"labels": labels("warning"),
			"annotations": map[string]any{
				"summary":     "Cost Management managed pod restarting",
				"description": "Pod {{ $labels.managed_pod }} container {{ $labels.managed_container }} for {{ $labels.cr_namespace }}/{{ $labels.name }} has restart count > 3 for 15 minutes.",
			},
		},
		map[string]any{
			"alert":  "CostManagementNotAvailable",
			"expr":   condition("Available", "False"),
			"for":    "30m",
			"labels": labels("warning"),
			"annotations": map[string]any{
				"summary":     "Cost Management stack is not available",
				"description": "CostManagementServiceConfig {{ $labels.cr_namespace }}/{{ $labels.name }} has Available=False for 30 minutes.",
			},
		},
		map[string]any{
			// Any reconcile error in the last 15m (for:0m avoids boundary flapping
			// with increase(...[15m]) and for:15m).
			"alert":  "CostManagementReconcileFailure",
			"expr":   `increase(costmanagement_reconcile_errors_total[15m]) > 0`,
			"for":    "0m",
			"labels": labels("warning"),
			"annotations": map[string]any{
				"summary":     "Cost Management reconcile errors",
				"description": "Operator reconcile errors for {{ $labels.cr_namespace }}/{{ $labels.name }} over the last 15 minutes.",
			},
		},
	}
	return prometheusRules("controller-manager-alerts", "", map[string]string{
		"control-plane":                "controller-manager",
		"app.kubernetes.io/name":       "koku-service-operator",
		"app.kubernetes.io/managed-by": "kustomize",
	}, rules)
}

func prometheusRules(name, namespace string, labels map[string]string, rules []any) *unstructured.Unstructured {
	pr := &unstructured.Unstructured{}
	pr.SetGroupVersionKind(prometheusRuleGVK)
	pr.SetName(name)
	pr.SetNamespace(namespace)
	pr.SetLabels(labels)

	_ = unstructured.SetNestedSlice(pr.Object, []any{
		map[string]any{
			"name":  "cost-management.rules",
			"rules": rules,
		},
	}, "spec", "groups")

	return pr
}

// celeryBacklogQueues is metric -> Celery queue name for on-prem backlog alerts.
// SaaS-only queues (hcs, subs_*) are omitted.
var celeryBacklogQueues = []struct{ metric, queue string }{
	{"download_backlog", "download"},
	{"download_xl_backlog", "download_xl"},
	{"download_penalty_backlog", "download_penalty"},
	{"summary_backlog", "summary"},
	{"summary_xl_backlog", "summary_xl"},
	{"summary_penalty_backlog", "summary_penalty"},
	{"priority_backlog", "priority"},
	{"priority_xl_backlog", "priority_xl"},
	{"priority_penalty_backlog", "priority_penalty"},
	{"refresh_backlog", "refresh"},
	{"refresh_xl_backlog", "refresh_xl"},
	{"refresh_penalty_backlog", "refresh_penalty"},
	{"cost_model_backlog", "cost_model"},
	{"cost_model_xl_backlog", "cost_model_xl"},
	{"cost_model_penalty_backlog", "cost_model_penalty"},
	{"default_backlog", "celery"},
	{"ocp_backlog", "ocp"},
	{"ocp_xl_backlog", "ocp_xl"},
	{"ocp_penalty_backlog", "ocp_penalty"},
}

func celeryBacklogAlertName(queue string) string {
	var b strings.Builder
	b.WriteString("CostManagementCeleryBacklog")
	for part := range strings.SplitSeq(queue, "_") {
		if part == "" {
			continue
		}
		b.WriteString(strings.ToUpper(part[:1]))
		b.WriteString(part[1:])
	}
	return b.String()
}

func celeryBacklogRules(namespace string, labels func(string) map[string]any) []any {
	out := make([]any, 0, len(celeryBacklogQueues))
	for _, q := range celeryBacklogQueues {
		lbl := labels("warning")
		lbl["queue"] = q.queue
		out = append(out, map[string]any{
			"alert":  celeryBacklogAlertName(q.queue),
			"expr":   `max by (namespace) (` + q.metric + `{namespace="` + namespace + `"} > 1000)`,
			"for":    "10m",
			"labels": lbl,
			"annotations": map[string]any{
				"summary":     "Cost Management Celery " + q.queue + " queue backlog high",
				"description": "Celery queue " + q.queue + " depth is {{ $value }} (>1000) in namespace {{ $labels.namespace }} for more than 10 minutes.",
			},
		})
	}
	return out
}
