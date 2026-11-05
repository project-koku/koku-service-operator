package controller

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"

	costv1alpha1 "github.com/project-koku/koku-service-operator/api/v1alpha1"
)

// The Ready event fires only on a genuine non-Ready → Ready transition.
// reconcile() resets Phase to Progressing before running phases, so markReady
// must decide on the phase read at the start of the pass (priorPhase). That
// read is uncached (see TestTransitionEvent_NotReemittedOnStaleCache), so a
// pass that follows a Ready pass sees PhaseReady and stays quiet.
func TestMarkReady_EmitsReadyOnlyOnTransition(t *testing.T) {
	cases := []struct {
		name         string
		priorPhase   costv1alpha1.Phase
		rollbackHold bool
		wantEvent    bool
	}{
		{name: "first install", priorPhase: costv1alpha1.PhasePending, wantEvent: true},
		{name: "rollout completes", priorPhase: costv1alpha1.PhaseProgressing, wantEvent: true},
		{name: "recovered from degraded", priorPhase: costv1alpha1.PhaseDegraded, wantEvent: true},
		{name: "drift pass", priorPhase: costv1alpha1.PhaseReady, wantEvent: false},
		// rollbackUpgrade owns Degraded/PhaseChanged while the source is restored.
		{name: "rollback hold", priorPhase: costv1alpha1.PhaseDegraded, rollbackHold: true, wantEvent: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := record.NewFakeRecorder(10)
			r := &CostManagementServiceConfigReconciler{Recorder: rec}
			cfg := minimalCR(testCRName, testNamespace)
			cfg.Status.Phase = costv1alpha1.PhaseProgressing // reconcileStack reset
			r.setCondition(cfg, costv1alpha1.ConditionDegraded, metav1.ConditionTrue, "UpgradeRolledBack", "rolled back")

			r.markReady(cfg, tc.priorPhase, tc.rollbackHold)

			if tc.wantEvent {
				assertEvent(t, rec, "Ready")
			} else {
				assertNoEvent(t, rec, "Ready")
			}
			if cfg.Status.Phase != costv1alpha1.PhaseReady {
				t.Errorf("Phase = %q, want Ready", cfg.Status.Phase)
			}
			avail := findCondition(cfg.Status.Conditions, costv1alpha1.ConditionAvailable)
			if avail == nil || avail.Status != metav1.ConditionTrue || avail.Reason != "AllComponentsReady" {
				t.Errorf("expected Available=True AllComponentsReady, got %+v", avail)
			}
			deg := findCondition(cfg.Status.Conditions, costv1alpha1.ConditionDegraded)
			if wantDegraded := tc.rollbackHold; (deg.Status == metav1.ConditionTrue) != wantDegraded {
				t.Errorf("Degraded = %s/%s, want True only during a rollback hold", deg.Status, deg.Reason)
			}
		})
	}
}
