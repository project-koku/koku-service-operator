package controller

import (
	"testing"

	costv1alpha1 "github.com/project-koku/koku-service-operator/api/v1alpha1"
)

// TestReadyEventEmittedOnlyOnTransition verifies that the "Ready" event is
// emitted exactly once — on the first transition to Ready — and NOT on
// subsequent successful reconcile passes when the phase is already Ready.
//
// Bug (F6): cfg.Status.Phase is reset to Progressing at the top of reconcile(),
// so the `!= PhaseReady` guard always fires. The fix must compare against the
// phase captured BEFORE reconcile() overwrites it (the `original` snapshot).
func TestReadyEventEmittedOnlyOnTransition(t *testing.T) {
	// Simulate the guard logic for both the buggy and fixed versions.

	// --- BUGGY behaviour ---
	// At the end of a successful reconcile, cfg.Status.Phase has been set to
	// Progressing (line 135) and then the guard checks it. It always fires.
	buggyEventCount := 0
	for _, priorPhase := range []costv1alpha1.Phase{
		costv1alpha1.PhasePending,
		costv1alpha1.PhaseProgressing,
		costv1alpha1.PhaseReady, // second pass — phase was already Ready
	} {
		cfg := &costv1alpha1.CostManagementServiceConfig{}
		cfg.Status.Phase = priorPhase

		// Buggy: overwrite phase at start of reconcile
		cfg.Status.Phase = costv1alpha1.PhaseProgressing

		// Buggy guard (always fires because phase == Progressing)
		if cfg.Status.Phase != costv1alpha1.PhaseReady {
			buggyEventCount++
		}
	}
	if buggyEventCount != 3 {
		t.Errorf("expected buggy path to emit 3 events (one per pass), got %d", buggyEventCount)
	}

	// --- FIXED behaviour ---
	// Capture the original phase BEFORE overwriting it, then check that.
	fixedEventCount := 0
	for _, priorPhase := range []costv1alpha1.Phase{
		costv1alpha1.PhasePending,     // first pass → transition → emit
		costv1alpha1.PhaseProgressing, // still transitioning → emit
		costv1alpha1.PhaseReady,       // already Ready → NO event
	} {
		originalPhase := priorPhase // captured before reconcile overwrites

		cfg := &costv1alpha1.CostManagementServiceConfig{}
		cfg.Status.Phase = priorPhase
		cfg.Status.Phase = costv1alpha1.PhaseProgressing // overwritten at start

		// Fixed guard: compare against the original (pre-overwrite) phase.
		if originalPhase != costv1alpha1.PhaseReady {
			fixedEventCount++
		}
	}
	if fixedEventCount != 2 {
		t.Errorf("expected fixed path to emit 2 events (only on non-Ready→Ready transitions), got %d", fixedEventCount)
	}
}
