package controller

import (
	"testing"
	"time"

	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	costv1alpha1 "github.com/project-koku/koku-service-operator/api/v1alpha1"
)

// simulatePass mirrors reconcile() around the phases: capture the prior
// Progressing condition, flip it True for the pass, then complete the pass.
func simulatePass(r *CostManagementServiceConfigReconciler, cfg *costv1alpha1.CostManagementServiceConfig) {
	prior := apimeta.FindStatusCondition(cfg.Status.Conditions, costv1alpha1.ConditionProgressing)
	if prior != nil {
		prior = prior.DeepCopy()
	}
	r.setCondition(cfg, costv1alpha1.ConditionProgressing, metav1.ConditionTrue, "Reconciling", "Reconciliation in progress")
	r.completeProgressing(cfg, prior)
}

// Steady state (COST-8517): a successful pass that starts from
// Progressing=False must produce no status change at all. Before the fix the
// True→False flip stamped a new LastTransitionTime, every pass patched status,
// and the status update re-enqueued the CR (~46 passes/min on cluster).
func TestCompleteProgressing_SteadyStateWritesNoStatus(t *testing.T) {
	r := &CostManagementServiceConfigReconciler{}
	cfg := minimalCR(testCRName, testNamespace)
	since := metav1.NewTime(time.Now().Add(-time.Hour).Truncate(time.Second))
	apimeta.SetStatusCondition(&cfg.Status.Conditions, metav1.Condition{
		Type: costv1alpha1.ConditionProgressing, Status: metav1.ConditionFalse,
		Reason: "ReconcileComplete", LastTransitionTime: since,
	})
	original := cfg.DeepCopy()

	simulatePass(r, cfg)

	cond := apimeta.FindStatusCondition(cfg.Status.Conditions, costv1alpha1.ConditionProgressing)
	if cond == nil || cond.Status != metav1.ConditionFalse || !cond.LastTransitionTime.Equal(&since) {
		t.Fatalf("expected Progressing=False with LastTransitionTime %s, got %+v", since, cond)
	}
	patch, err := client.MergeFrom(original).Data(cfg)
	if err != nil {
		t.Fatalf("patch data: %v", err)
	}
	if string(patch) != "{}" {
		t.Fatalf("steady-state pass must not change status, patch = %s", patch)
	}
}

// A pass that finishes work started earlier (Progressing was True) is a real
// transition and gets a new LastTransitionTime.
func TestCompleteProgressing_RealTransitionUpdatesTime(t *testing.T) {
	r := &CostManagementServiceConfigReconciler{}
	cfg := minimalCR(testCRName, testNamespace)
	since := metav1.NewTime(time.Now().Add(-time.Hour).Truncate(time.Second))
	apimeta.SetStatusCondition(&cfg.Status.Conditions, metav1.Condition{
		Type: costv1alpha1.ConditionProgressing, Status: metav1.ConditionTrue,
		Reason: "Reconciling", LastTransitionTime: since,
	})

	simulatePass(r, cfg)

	cond := apimeta.FindStatusCondition(cfg.Status.Conditions, costv1alpha1.ConditionProgressing)
	if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != "ReconcileComplete" {
		t.Fatalf("expected Progressing=False ReconcileComplete, got %+v", cond)
	}
	if !cond.LastTransitionTime.After(since.Time) {
		t.Fatalf("expected a new LastTransitionTime after %s, got %s", since, cond.LastTransitionTime)
	}
}

// First pass: no prior condition, Progressing ends False with a set time.
func TestCompleteProgressing_NoPriorCondition(t *testing.T) {
	r := &CostManagementServiceConfigReconciler{}
	cfg := minimalCR(testCRName, testNamespace)

	simulatePass(r, cfg)

	cond := apimeta.FindStatusCondition(cfg.Status.Conditions, costv1alpha1.ConditionProgressing)
	if cond == nil || cond.Status != metav1.ConditionFalse || cond.LastTransitionTime.IsZero() {
		t.Fatalf("expected Progressing=False with a LastTransitionTime, got %+v", cond)
	}
}
