package controller

import (
	"errors"
	"fmt"
	"time"

	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	costv1alpha1 "github.com/project-koku/koku-service-operator/api/v1alpha1"
	opmetrics "github.com/project-koku/koku-service-operator/internal/metrics"
)

// Result controls reconcile loop flow.
type Result struct {
	// RequeueAfter > 0 means requeue after this duration.
	RequeueAfter time.Duration
	// Stop signals that this phase wants to halt the current reconcile pass
	// without returning an error (e.g., waiting for infrastructure).
	Stop bool
}

func (r Result) IsZero() bool { return r.RequeueAfter == 0 && !r.Stop }

// PhaseFn is the signature every reconcile phase must satisfy.
type PhaseFn func() (Result, error)

// PhaseError carries structured metadata so the reconcile loop can set the
// right condition without if-chains at the call site.
type PhaseError struct {
	// Err is the underlying error.
	Err error
	// ConditionType is the condition that should be updated (e.g. "Ready").
	ConditionType string
	// Reason is the machine-readable reason string for the condition.
	Reason string
	// Phase is the phase that failed — used for the status.Phase field.
	Phase costv1alpha1.Phase
}

func (e *PhaseError) Error() string {
	return fmt.Sprintf("[%s/%s] %v", e.ConditionType, e.Reason, e.Err)
}

func (e *PhaseError) Unwrap() error { return e.Err }

// NewPhaseError wraps err as a PhaseError. Callers use this at the point of
// failure to attach the condition type, reason, and target phase so the
// reconcile loop can update status without if-chains.
func NewPhaseError(err error, condType, reason string, phase costv1alpha1.Phase) error {
	if err == nil {
		return nil
	}
	return &PhaseError{Err: err, ConditionType: condType, Reason: reason, Phase: phase}
}

// applyPhaseError updates cfg.Status from a PhaseError returned by a phase.
func applyPhaseError(cfg *costv1alpha1.CostManagementServiceConfig, err error) {
	var pe *PhaseError
	if asPhaseError(err, &pe) {
		cfg.Status.Phase = pe.Phase
		apimeta.SetStatusCondition(&cfg.Status.Conditions, metav1.Condition{
			Type:               pe.ConditionType,
			Status:             metav1.ConditionFalse,
			Reason:             pe.Reason,
			Message:            pe.Err.Error(),
			ObservedGeneration: cfg.Generation,
		})
		opmetrics.SetCondition(cfg.Namespace, cfg.Name, pe.ConditionType, metav1.ConditionFalse)
	}
}

func asPhaseError(err error, target **PhaseError) bool {
	if err == nil {
		return false
	}
	return errors.As(err, target)
}

// runPhases executes phases in order. Each phase can:
//   - return (zero Result, nil)        → continue to next phase
//   - return (Result{RequeueAfter:…}, nil) → stop, requeue
//   - return (Result{Stop:true}, nil)  → stop, don't requeue (used for: waiting)
//   - return (_, err)                  → stop, surface error
//
// The caller converts the returned Result to ctrl.Result.
func runPhases(phases []PhaseFn) (Result, error) {
	for _, phase := range phases {
		result, err := phase()
		if err != nil || !result.IsZero() {
			return result, err
		}
	}
	return Result{}, nil
}
