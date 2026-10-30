package controller

import (
	"context"
	"errors"
	"testing"

	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	costv1alpha1 "github.com/project-koku/koku-service-operator/api/v1alpha1"
)

// status.profile is "the tier whose sized workloads were applied for the current
// generation" (design §3.4, D20): written by markProfileApplied once the last
// sized-workload phase has applied them, even if later readiness-only gates
// (UI, Progressing) are not yet satisfied. Test phases that stand in for the
// workload-applying phases call it themselves.

func (r *CostManagementServiceConfigReconciler) appliedPhases(_ context.Context, cfg *costv1alpha1.CostManagementServiceConfig) []PhaseFn {
	return []PhaseFn{func() (Result, error) { r.markProfileApplied(cfg); return Result{}, nil }}
}

// uiGatedPhases applies the sized workloads, then stops on the UI gate
// (UIReady=False, the normal Cluster Bot state without an OAuth client Secret).
func (r *CostManagementServiceConfigReconciler) uiGatedPhases(_ context.Context, cfg *costv1alpha1.CostManagementServiceConfig) []PhaseFn {
	return []PhaseFn{
		func() (Result, error) { r.markProfileApplied(cfg); return Result{}, nil },
		func() (Result, error) { return Result{RequeueAfter: requeueSlow}, nil },
	}
}

func failPhases(context.Context, *costv1alpha1.CostManagementServiceConfig) []PhaseFn {
	return []PhaseFn{func() (Result, error) { return Result{}, errors.New("boom") }}
}

func gatePhases(context.Context, *costv1alpha1.CostManagementServiceConfig) []PhaseFn {
	return []PhaseFn{func() (Result, error) { return Result{Stop: true}, nil }}
}

func newProfileReconciler(t *testing.T, cr *costv1alpha1.CostManagementServiceConfig) (*CostManagementServiceConfigReconciler, client.Client, types.NamespacedName) {
	t.Helper()
	controllerutil.AddFinalizer(cr, finalizerName)
	c := fake.NewClientBuilder().
		WithScheme(ownershipScheme(t)).
		WithObjects(cr).
		WithStatusSubresource(cr).
		Build()
	r := &CostManagementServiceConfigReconciler{
		Client:   c,
		Scheme:   ownershipScheme(t),
		Recorder: record.NewFakeRecorder(100),
	}
	return r, c, types.NamespacedName{Name: cr.Name, Namespace: cr.Namespace}
}

func reconcileOnce(t *testing.T, r *CostManagementServiceConfigReconciler, key types.NamespacedName) {
	t.Helper()
	// Errors from gated/failed phases are expected by some callers.
	_, _ = r.Reconcile(context.Background(), reconcile.Request{NamespacedName: key})
}

func statusProfile(t *testing.T, c client.Client, key types.NamespacedName) costv1alpha1.Profile {
	t.Helper()
	got := &costv1alpha1.CostManagementServiceConfig{}
	if err := c.Get(context.Background(), key, got); err != nil {
		t.Fatalf("get CR: %v", err)
	}
	return got.Status.Profile
}

func TestStatusProfile_SuccessReportsAppliedTier(t *testing.T) {
	cr := minimalCR(testCRName, testNamespace)
	r, c, key := newProfileReconciler(t, cr)
	r.phasesOverride = r.appliedPhases

	reconcileOnce(t, r, key)
	if got := statusProfile(t, c, key); got != costv1alpha1.ProfileSmall {
		t.Fatalf("unset profile: status.profile = %q, want small", got)
	}

	cur := &costv1alpha1.CostManagementServiceConfig{}
	if err := c.Get(context.Background(), key, cur); err != nil {
		t.Fatal(err)
	}
	cur.Spec.Profile = costv1alpha1.ProfileLarge
	if err := c.Update(context.Background(), cur); err != nil {
		t.Fatal(err)
	}
	reconcileOnce(t, r, key)
	if got := statusProfile(t, c, key); got != costv1alpha1.ProfileLarge {
		t.Fatalf("after switch: status.profile = %q, want large", got)
	}
}

func TestStatusProfile_PausedKeepsPreviousValue(t *testing.T) {
	cr := minimalCR(testCRName, testNamespace)
	cr.Spec.Profile = costv1alpha1.ProfileSmall
	r, c, key := newProfileReconciler(t, cr)
	r.phasesOverride = r.appliedPhases
	reconcileOnce(t, r, key)

	cur := &costv1alpha1.CostManagementServiceConfig{}
	if err := c.Get(context.Background(), key, cur); err != nil {
		t.Fatal(err)
	}
	cur.Spec.Profile = costv1alpha1.ProfileLarge
	cur.Annotations = map[string]string{pauseAnnotation: annotationTrue}
	if err := c.Update(context.Background(), cur); err != nil {
		t.Fatal(err)
	}
	reconcileOnce(t, r, key)
	if got := statusProfile(t, c, key); got != costv1alpha1.ProfileSmall {
		t.Fatalf("paused: status.profile = %q, want previous small", got)
	}
}

func TestStatusProfile_PausedNeverReconciledStaysEmpty(t *testing.T) {
	cr := minimalCR(testCRName, testNamespace)
	cr.Spec.Profile = costv1alpha1.ProfileXLarge
	cr.Annotations = map[string]string{pauseAnnotation: annotationTrue}
	r, c, key := newProfileReconciler(t, cr)
	reconcileOnce(t, r, key)
	if got := statusProfile(t, c, key); got != "" {
		t.Fatalf("paused, never applied: status.profile = %q, want empty", got)
	}
}

func TestStatusProfile_FailedOrGatedReconcileKeepsPreviousValue(t *testing.T) {
	for name, bad := range map[string]func(context.Context, *costv1alpha1.CostManagementServiceConfig) []PhaseFn{
		"phase error": failPhases,
		"early gate":  gatePhases,
	} {
		t.Run(name, func(t *testing.T) {
			cr := minimalCR(testCRName, testNamespace)
			r, c, key := newProfileReconciler(t, cr)
			r.phasesOverride = r.appliedPhases
			reconcileOnce(t, r, key)

			cur := &costv1alpha1.CostManagementServiceConfig{}
			if err := c.Get(context.Background(), key, cur); err != nil {
				t.Fatal(err)
			}
			cur.Spec.Profile = costv1alpha1.ProfileLarge
			if err := c.Update(context.Background(), cur); err != nil {
				t.Fatal(err)
			}
			r.phasesOverride = bad
			reconcileOnce(t, r, key)
			if got := statusProfile(t, c, key); got != costv1alpha1.ProfileSmall {
				t.Fatalf("status.profile = %q, want previous small", got)
			}
		})
	}
}

func TestStatusProfile_UIGateUnmetStillReportsAppliedTier(t *testing.T) {
	cr := minimalCR(testCRName, testNamespace)
	cr.Spec.Profile = costv1alpha1.ProfileMedium
	r, c, key := newProfileReconciler(t, cr)
	r.phasesOverride = r.uiGatedPhases

	reconcileOnce(t, r, key)
	if got := statusProfile(t, c, key); got != costv1alpha1.ProfileMedium {
		t.Fatalf("UI gate unmet: status.profile = %q, want medium", got)
	}
}

func TestStatusProfile_FailureBeforeWorkloadsKeepsPreviousAcrossGenerations(t *testing.T) {
	cr := minimalCR(testCRName, testNamespace)
	r, c, key := newProfileReconciler(t, cr)
	r.phasesOverride = r.uiGatedPhases
	reconcileOnce(t, r, key)

	cur := &costv1alpha1.CostManagementServiceConfig{}
	if err := c.Get(context.Background(), key, cur); err != nil {
		t.Fatal(err)
	}
	cur.Spec.Profile = costv1alpha1.ProfileXLarge
	if err := c.Update(context.Background(), cur); err != nil {
		t.Fatal(err)
	}
	r.phasesOverride = failPhases
	reconcileOnce(t, r, key)
	if got := statusProfile(t, c, key); got != costv1alpha1.ProfileSmall {
		t.Fatalf("status.profile = %q, want previous small", got)
	}
}
