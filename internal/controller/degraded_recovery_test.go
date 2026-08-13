package controller

import (
	"errors"
	"testing"

	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	costv1alpha1 "github.com/project-koku/koku-service-operator/api/v1alpha1"
)

func TestDegradedClearedOnSuccessfulReconcile(t *testing.T) {
	cfg := &costv1alpha1.CostManagementServiceConfig{
		ObjectMeta: metav1.ObjectMeta{Generation: 1},
	}
	r := &CostManagementServiceConfigReconciler{
		Recorder: &noopRecorder{},
	}

	// Step 1: simulate a phase error setting Degraded=True.
	phaseErr := errors.New("quota exceeded")
	applyPhaseError(cfg, phaseErr)
	r.setCondition(cfg, costv1alpha1.ConditionDegraded, metav1.ConditionTrue,
		"ReconcileError", phaseErr.Error())
	cfg.Status.Phase = costv1alpha1.PhaseDegraded

	if !apimeta.IsStatusConditionTrue(cfg.Status.Conditions, costv1alpha1.ConditionDegraded) {
		t.Fatal("precondition: Degraded should be True after error")
	}
	if cfg.Status.Phase != costv1alpha1.PhaseDegraded {
		t.Fatalf("precondition: Phase = %q, want Degraded", cfg.Status.Phase)
	}

	// Step 2: simulate the success path from reconcile().
	r.setCondition(cfg, costv1alpha1.ConditionAvailable, metav1.ConditionTrue, "AllComponentsReady", "All components are running")
	r.setCondition(cfg, costv1alpha1.ConditionProgressing, metav1.ConditionFalse, "ReconcileComplete", "")
	r.setCondition(cfg, costv1alpha1.ConditionDegraded, metav1.ConditionFalse, "ReconcileComplete", "")
	cfg.Status.Phase = costv1alpha1.PhaseReady

	// Verify Degraded is cleared.
	if apimeta.IsStatusConditionTrue(cfg.Status.Conditions, costv1alpha1.ConditionDegraded) {
		t.Error("Degraded should be False after successful reconcile")
	}
	cond := apimeta.FindStatusCondition(cfg.Status.Conditions, costv1alpha1.ConditionDegraded)
	if cond == nil {
		t.Fatal("Degraded condition should exist (False), not be absent")
	}
	if cond.Reason != "ReconcileComplete" {
		t.Errorf("Degraded.Reason = %q, want ReconcileComplete", cond.Reason)
	}

	// Verify the other conditions are correct.
	if !apimeta.IsStatusConditionTrue(cfg.Status.Conditions, costv1alpha1.ConditionAvailable) {
		t.Error("Available should be True")
	}
	if apimeta.IsStatusConditionTrue(cfg.Status.Conditions, costv1alpha1.ConditionProgressing) {
		t.Error("Progressing should be False")
	}
	if cfg.Status.Phase != costv1alpha1.PhaseReady {
		t.Errorf("Phase = %q, want Ready", cfg.Status.Phase)
	}
}

func TestSuccessfulReconcileSetsDegradedFalseFromCleanStart(t *testing.T) {
	cfg := &costv1alpha1.CostManagementServiceConfig{
		ObjectMeta: metav1.ObjectMeta{Generation: 1},
	}
	r := &CostManagementServiceConfigReconciler{
		Recorder: &noopRecorder{},
	}

	// Simulate the success path — no prior Degraded condition exists.
	r.setCondition(cfg, costv1alpha1.ConditionAvailable, metav1.ConditionTrue, "AllComponentsReady", "All components are running")
	r.setCondition(cfg, costv1alpha1.ConditionProgressing, metav1.ConditionFalse, "ReconcileComplete", "")
	r.setCondition(cfg, costv1alpha1.ConditionDegraded, metav1.ConditionFalse, "ReconcileComplete", "")
	cfg.Status.Phase = costv1alpha1.PhaseReady

	cond := apimeta.FindStatusCondition(cfg.Status.Conditions, costv1alpha1.ConditionDegraded)
	if cond == nil {
		t.Fatal("Degraded condition should be present (False) even on clean start")
	}
	if cond.Status != metav1.ConditionFalse {
		t.Errorf("Degraded.Status = %q, want False", cond.Status)
	}
}

func TestErrorSetsDegradedTrue(t *testing.T) {
	cfg := &costv1alpha1.CostManagementServiceConfig{
		ObjectMeta: metav1.ObjectMeta{Generation: 1},
	}
	r := &CostManagementServiceConfigReconciler{
		Recorder: &noopRecorder{},
	}

	// Simulate the error path from reconcile().
	err := errors.New("webhook rejected deployment")
	applyPhaseError(cfg, err)
	r.setCondition(cfg, costv1alpha1.ConditionDegraded, metav1.ConditionTrue,
		"ReconcileError", err.Error())
	cfg.Status.Phase = costv1alpha1.PhaseDegraded

	if !apimeta.IsStatusConditionTrue(cfg.Status.Conditions, costv1alpha1.ConditionDegraded) {
		t.Error("Degraded should be True after error")
	}
	cond := apimeta.FindStatusCondition(cfg.Status.Conditions, costv1alpha1.ConditionDegraded)
	if cond.Reason != "ReconcileError" {
		t.Errorf("Degraded.Reason = %q, want ReconcileError", cond.Reason)
	}
	if cond.Message != "webhook rejected deployment" {
		t.Errorf("Degraded.Message = %q, want error text", cond.Message)
	}
	if cfg.Status.Phase != costv1alpha1.PhaseDegraded {
		t.Errorf("Phase = %q, want Degraded", cfg.Status.Phase)
	}
}
