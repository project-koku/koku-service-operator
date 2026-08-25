package controller

import (
	"context"
	"strings"
	"testing"

	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	costv1alpha1 "github.com/project-koku/koku-service-operator/api/v1alpha1"
)

// pinTestImages sets repository+tag on every image the reconciler requires so
// unit tests can exercise later phases. Values are placeholders, not catalog pins.
func pinTestImages(cfg *costv1alpha1.CostManagementServiceConfig) {
	img := func(repo, tag string) costv1alpha1.ImageSpec {
		return costv1alpha1.ImageSpec{Repository: repo, Tag: tag}
	}
	cfg.Spec.Database.Image = img("example.com/postgres", "16")
	cfg.Spec.Cache.Image = img("example.com/valkey", "8")
	cfg.Spec.Auth.Envoy.Image = img("example.com/envoy", "v1")
	cfg.Spec.UI.OAuthProxy.Image = img("example.com/oauth2-proxy", "v1")
	cfg.Spec.UI.App.Image = img("example.com/ui", "v1")
	cfg.Spec.CostManagement.API.Image = img("example.com/koku", "v1")
	cfg.Spec.CostManagement.Masu.Image = img("example.com/koku", "v1")
	cfg.Spec.RBAC.Image = img("example.com/rbac", "v1")
	cfg.Spec.Ingress.Image = img("example.com/ingress", "v1")
}

func TestReconcile_MissingImagesDegrades(t *testing.T) {
	cr := minimalCR(testCRName, testNamespace)
	controllerutil.AddFinalizer(cr, finalizerName)

	c := fake.NewClientBuilder().
		WithScheme(ownershipScheme(t)).
		WithObjects(cr).
		WithStatusSubresource(cr).
		Build()

	r := &CostManagementServiceConfigReconciler{
		Client:   c,
		Scheme:   ownershipScheme(t),
		Recorder: record.NewFakeRecorder(8),
	}

	_, err := r.Reconcile(context.Background(), reconcile.Request{
		NamespacedName: types.NamespacedName{Name: testCRName, Namespace: testNamespace},
	})
	if err == nil {
		t.Fatal("expected error for empty spec.*.image")
	}
	if !strings.Contains(err.Error(), "spec.database.image") {
		t.Errorf("error = %v, want spec.database.image", err)
	}

	updated := &costv1alpha1.CostManagementServiceConfig{}
	if getErr := c.Get(context.Background(), types.NamespacedName{Name: testCRName, Namespace: testNamespace}, updated); getErr != nil {
		t.Fatalf("get CR: %v", getErr)
	}
	if updated.Status.Phase != costv1alpha1.PhaseDegraded {
		t.Errorf("phase = %q, want Degraded", updated.Status.Phase)
	}
	deg := apimeta.FindStatusCondition(updated.Status.Conditions, costv1alpha1.ConditionDegraded)
	if deg == nil || deg.Status != metav1.ConditionTrue {
		t.Fatalf("Degraded = %+v, want True", deg)
	}
	if deg.Reason != reasonImageNotSet {
		t.Errorf("Degraded.Reason = %q, want %s", deg.Reason, reasonImageNotSet)
	}
	if !strings.Contains(deg.Message, "spec.database.image") {
		t.Errorf("Degraded message = %q, want spec.database.image", deg.Message)
	}
}

func TestReconcileWorkloadImages_OKWhenPinned(t *testing.T) {
	cfg := minimalCR(testCRName, testNamespace)
	falseVal := false
	cfg.Spec.Database.Deploy = &falseVal
	cfg.Spec.Cache.Deploy = &falseVal
	pinTestImages(cfg)

	r := &CostManagementServiceConfigReconciler{Recorder: &noopRecorder{}}
	result, err := r.reconcileWorkloadImages(context.Background(), cfg)
	if err != nil {
		t.Fatalf("reconcileWorkloadImages: %v", err)
	}
	if !result.IsZero() {
		t.Errorf("result = %+v, want zero", result)
	}
}
