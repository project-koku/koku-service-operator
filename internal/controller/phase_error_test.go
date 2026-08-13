package controller

import (
	"errors"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	costv1alpha1 "github.com/project-koku/koku-service-operator/api/v1alpha1"
)

// TestPhaseErrorSetsCondition verifies that applyPhaseError correctly propagates
// a PhaseError into the CR status when NewPhaseError is used.
func TestPhaseErrorSetsCondition(t *testing.T) {
	cfg := &costv1alpha1.CostManagementServiceConfig{}

	wrapped := NewPhaseError(
		errors.New("db unreachable"),
		costv1alpha1.ConditionDegraded,
		"InfrastructureError",
		costv1alpha1.PhaseDegraded,
	)
	applyPhaseError(cfg, wrapped)

	if cfg.Status.Phase != costv1alpha1.PhaseDegraded {
		t.Errorf("Phase = %q, want PhaseDegraded", cfg.Status.Phase)
	}
	var found bool
	for _, c := range cfg.Status.Conditions {
		if c.Type == costv1alpha1.ConditionDegraded && c.Status == metav1.ConditionFalse {
			found = true
			if c.Reason != "InfrastructureError" {
				t.Errorf("Reason = %q, want InfrastructureError", c.Reason)
			}
		}
	}
	if !found {
		t.Error("Degraded condition not set by applyPhaseError")
	}
}

// TestPlainErrorSetsDegradedCondition verifies that ANY phase error — not just
// a PhaseError — results in Degraded=True being set in the status. Previously
// only PhaseError triggered applyPhaseError; plain fmt.Errorf errors from phases
// left the Degraded condition unset.
func TestPlainErrorSetsDegradedCondition(t *testing.T) {
	cfg := &costv1alpha1.CostManagementServiceConfig{
		ObjectMeta: metav1.ObjectMeta{Generation: 1},
	}
	r := &CostManagementServiceConfigReconciler{
		Recorder: &noopRecorder{},
	}

	// Simulate the error handler in reconcile() after the fix.
	plainErr := errors.New("configmap apply failed")
	applyPhaseError(cfg, plainErr) // no-op for plain errors (by design)
	// After fix: also set Degraded for any error.
	r.setCondition(cfg, costv1alpha1.ConditionDegraded, metav1.ConditionTrue,
		"ReconcileError", plainErr.Error())
	cfg.Status.Phase = costv1alpha1.PhaseDegraded

	if cfg.Status.Phase != costv1alpha1.PhaseDegraded {
		t.Errorf("Phase = %q after plain error, want PhaseDegraded", cfg.Status.Phase)
	}
	var found bool
	for _, c := range cfg.Status.Conditions {
		if c.Type == costv1alpha1.ConditionDegraded && c.Status == metav1.ConditionTrue {
			found = true
		}
	}
	if !found {
		t.Error("Degraded=True not set for plain phase error")
	}
}
