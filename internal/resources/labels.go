package resources

import (
	costv1alpha1 "github.com/project-koku/koku-service-operator/api/v1alpha1"
)

const (
	labelApp       = "app.kubernetes.io/name"
	labelInstance  = "app.kubernetes.io/instance"
	labelPartOf    = "app.kubernetes.io/part-of"
	labelManagedBy = "app.kubernetes.io/managed-by"
	labelComponent = "app.kubernetes.io/component"
	labelVersion   = "app.kubernetes.io/version"
	// labelMetricsRole marks pods selected by the aggregated Celery workers Service.
	labelMetricsRole = "cost-management.openshift.io/metrics-role"

	ManagedBy = "koku-service-operator"

	// MetricsRoleCeleryWorker is the metrics-role value for Cost Management Celery workers.
	MetricsRoleCeleryWorker = "celery-worker"
)

// Labels returns the full set of labels for a resource owned by cfg.
func Labels(cfg *costv1alpha1.CostManagementServiceConfig, component string) map[string]string {
	return map[string]string{
		labelApp:       cfg.Name,
		labelInstance:  cfg.Name,
		labelPartOf:    "koku-service-operator",
		labelManagedBy: ManagedBy,
		labelComponent: component,
	}
}

// SelectorLabels returns the minimal stable set used in matchLabels.
// These must never change after initial creation.
func SelectorLabels(cfg *costv1alpha1.CostManagementServiceConfig, component string) map[string]string {
	return map[string]string{
		labelApp:       cfg.Name,
		labelInstance:  cfg.Name,
		labelComponent: component,
	}
}
