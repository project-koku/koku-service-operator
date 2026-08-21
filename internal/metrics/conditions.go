// Package metrics exposes operator Prometheus series for UWM-fireable alerts
// (COST-8108 option B). Series are registered on the controller-runtime registry.
package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

// Prometheus label keys shared across Cost Management series.
const (
	labelNamespace = "namespace"
	labelName      = "name"
	labelType      = "type"
	labelStatus    = "status"
	labelJob       = "job"
	labelPod       = "pod"
	labelContainer = "container"
	labelSecret    = "secret"
	labelKind      = "kind"
)

var (
	// Condition is 1 for the active status of a CMSC condition (True/False/Unknown).
	Condition = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "costmanagement_condition",
		Help: "CostManagementServiceConfig status.conditions mirror (1 = active status for type)",
	}, []string{labelNamespace, labelName, labelType, labelStatus})

	// ReconcileErrors counts reconcile failures (G1 CostManagementReconcileFailure).
	ReconcileErrors = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "costmanagement_reconcile_errors_total",
		Help: "Total reconcile errors for CostManagementServiceConfig",
	}, []string{labelNamespace, labelName})

	// MigrationJobFailed is 1 while a managed migrate Job is in Failed state.
	MigrationJobFailed = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "costmanagement_migration_job_failed",
		Help: "1 when a Cost Management schema migration Job has failed",
	}, []string{labelNamespace, labelName, labelJob})

	// ManagedPodRestarts mirrors container restart counts for managed pods.
	ManagedPodRestarts = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "costmanagement_managed_pod_restarts",
		Help: "Container restart count for pods owned by the Cost Management instance",
	}, []string{labelNamespace, labelName, labelPod, labelContainer})

	// SecretRotatedTotal increments when credentials Secrets are rotated (COST-7694 / G4).
	SecretRotatedTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "costmanagement_secret_rotated_total",
		Help: "Count of managed Secret rotations",
	}, []string{labelNamespace, labelName, labelSecret})

	// DriftCorrectedTotal increments when SSA re-applies correct drift (G4).
	DriftCorrectedTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "costmanagement_drift_corrected_total",
		Help: "Count of drift corrections applied to managed resources",
	}, []string{labelNamespace, labelName, labelKind})
)

func init() {
	metrics.Registry.MustRegister(
		Condition,
		ReconcileErrors,
		MigrationJobFailed,
		ManagedPodRestarts,
		SecretRotatedTotal,
		DriftCorrectedTotal,
	)
}

// SetCondition mirrors one CMSC condition into costmanagement_condition.
func SetCondition(namespace, name, condType string, status metav1.ConditionStatus) {
	if status == "" {
		status = metav1.ConditionUnknown
	}
	for _, s := range []string{
		string(metav1.ConditionTrue),
		string(metav1.ConditionFalse),
		string(metav1.ConditionUnknown),
	} {
		v := 0.0
		if string(status) == s {
			v = 1
		}
		Condition.WithLabelValues(namespace, name, condType, s).Set(v)
	}
}

// ClearConditionMetrics removes all condition gauges for a CMSC instance.
func ClearConditionMetrics(namespace, name string) int {
	return Condition.DeletePartialMatch(prometheus.Labels{
		labelNamespace: namespace,
		labelName:      name,
	})
}

// ClearMigrationJobFailedAll removes migration-failed gauges for a CMSC instance.
func ClearMigrationJobFailedAll(namespace, name string) int {
	return MigrationJobFailed.DeletePartialMatch(prometheus.Labels{
		labelNamespace: namespace,
		labelName:      name,
	})
}

// ClearMigrationJobFailed resets the failed gauge for a job name.
func ClearMigrationJobFailed(namespace, name, job string) {
	SetMigrationJobFailed(namespace, name, job, false)
}

// SetMigrationJobFailed marks a migrate Job as failed (1) or clear (0).
func SetMigrationJobFailed(namespace, name, job string, failed bool) {
	v := 0.0
	if failed {
		v = 1
	}
	MigrationJobFailed.WithLabelValues(namespace, name, job).Set(v)
}

// ClearManagedPodRestarts removes all restart gauges for a CMSC instance
// (namespace + name), including series for pods that no longer exist.
// Returns how many series were deleted.
func ClearManagedPodRestarts(namespace, name string) int {
	return ManagedPodRestarts.DeletePartialMatch(prometheus.Labels{
		labelNamespace: namespace,
		labelName:      name,
	})
}
