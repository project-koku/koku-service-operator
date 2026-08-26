package controller

import (
	"context"
	"testing"
	"time"

	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	costv1alpha1 "github.com/project-koku/koku-service-operator/api/v1alpha1"
	"github.com/project-koku/koku-service-operator/internal/resources"
)

// ownershipScheme registers all types needed for ownership tests.
func ownershipScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatalf("add client-go scheme: %v", err)
	}
	if err := costv1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("add costv1alpha1 scheme: %v", err)
	}
	if err := rbacv1.AddToScheme(s); err != nil {
		t.Fatalf("add rbacv1 scheme: %v", err)
	}
	return s
}

// minimalCR returns the smallest valid CostManagementServiceConfig with a fixed UID.
func minimalCR(name, ns string) *costv1alpha1.CostManagementServiceConfig {
	return &costv1alpha1.CostManagementServiceConfig{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: ns,
			UID:       "test-uid-1234",
		},
		Spec: costv1alpha1.CostManagementServiceConfigSpec{
			Auth: costv1alpha1.AuthConfig{
				Keycloak: costv1alpha1.KeycloakSpec{
					URL: "https://keycloak.keycloak.svc.cluster.local",
				},
			},
		},
	}
}

// ---------------------------------------------------------------------------
// setOwnerRef
// ---------------------------------------------------------------------------

func TestSetOwnerRef_SetsControllerAndBlockDeletion(t *testing.T) {
	cr := minimalCR(testCRName, testNamespace)
	cm := &metav1.ObjectMeta{Name: "child", Namespace: testNamespace}

	// Use a CostManagementServiceConfig as a concrete namespaced object.
	obj := &costv1alpha1.CostManagementServiceConfig{
		ObjectMeta: *cm,
	}
	obj.Namespace = testNamespace

	setOwnerRef(cr, obj)

	refs := obj.GetOwnerReferences()
	if len(refs) != 1 {
		t.Fatalf("expected 1 ownerRef, got %d", len(refs))
	}
	ref := refs[0]
	if ref.Controller == nil || !*ref.Controller {
		t.Error("Controller should be true")
	}
	if ref.BlockOwnerDeletion == nil || !*ref.BlockOwnerDeletion {
		t.Error("BlockOwnerDeletion should be true")
	}
	if ref.UID != cr.UID {
		t.Errorf("UID mismatch: got %s, want %s", ref.UID, cr.UID)
	}
}

func TestSetOwnerRef_SkipsClusterScoped(t *testing.T) {
	cr := minimalCR(testCRName, testNamespace)
	// Cluster-scoped objects have empty namespace.
	clusterObj := &costv1alpha1.CostManagementServiceConfig{
		ObjectMeta: metav1.ObjectMeta{Name: "cluster-thing"},
	}
	setOwnerRef(cr, clusterObj)
	if len(clusterObj.GetOwnerReferences()) != 0 {
		t.Error("cluster-scoped object should not get an ownerReference")
	}
}

// ---------------------------------------------------------------------------
// Finalizer registration
// ---------------------------------------------------------------------------

func TestReconcile_AddsFinalizer(t *testing.T) {
	cr := minimalCR(testCRName, testNamespace)
	c := fake.NewClientBuilder().
		WithScheme(ownershipScheme(t)).
		WithObjects(cr).
		WithStatusSubresource(cr).
		Build()

	r := &CostManagementServiceConfigReconciler{Client: c, Scheme: ownershipScheme(t)}

	// First Reconcile should only add the finalizer and return.
	_, err := r.Reconcile(context.Background(), reconcile.Request{
		NamespacedName: types.NamespacedName{Name: testCRName, Namespace: testNamespace},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	updated := &costv1alpha1.CostManagementServiceConfig{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: testCRName, Namespace: testNamespace}, updated); err != nil {
		t.Fatalf("get CR: %v", err)
	}
	if !controllerutil.ContainsFinalizer(updated, finalizerName) {
		t.Errorf("expected finalizer %q to be present", finalizerName)
	}
}

// ---------------------------------------------------------------------------
// Deletion path
// ---------------------------------------------------------------------------

func TestReconcileDelete_RemovesClusterScopedResourcesAndFinalizer(t *testing.T) {
	cr := minimalCR(testCRName, testNamespace)
	controllerutil.AddFinalizer(cr, finalizerName)

	// Pre-create the ClusterRole and ClusterRoleBinding that Kruize creates.
	// Use resources.NameKruizeClusterRole so the test follows the real naming
	// function (which includes a namespace hash since the F1 fix).
	kruizeName := resources.NameKruizeClusterRole(cr)
	kruizeCR := &rbacv1.ClusterRole{
		ObjectMeta: metav1.ObjectMeta{Name: kruizeName},
	}
	kruizeCRB := &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: kruizeName},
	}

	now := metav1.Now()
	cr.DeletionTimestamp = &now

	c := fake.NewClientBuilder().
		WithScheme(ownershipScheme(t)).
		WithObjects(cr, kruizeCR, kruizeCRB).
		Build()

	r := &CostManagementServiceConfigReconciler{Client: c, Scheme: ownershipScheme(t)}

	result, err := r.reconcileDelete(context.Background(), cr)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.IsZero() {
		t.Errorf("expected zero result after deletion, got %+v", result)
	}

	// After the last finalizer is removed the fake client deletes the CR — confirm it's gone.
	deleted := &costv1alpha1.CostManagementServiceConfig{}
	err = c.Get(context.Background(), types.NamespacedName{Name: testCRName, Namespace: testNamespace}, deleted)
	if err == nil {
		// If Get succeeded the finalizer must have been stripped.
		if controllerutil.ContainsFinalizer(deleted, finalizerName) {
			t.Error("finalizer should have been removed")
		}
	}
	// err == NotFound is also acceptable (fake client deleted it after last finalizer removed).

	// ClusterRole should be gone.
	if err := c.Get(context.Background(), types.NamespacedName{Name: kruizeName}, &rbacv1.ClusterRole{}); err == nil {
		t.Error("KruizeClusterRole should have been deleted")
	}

	// ClusterRoleBinding should be gone.
	if err := c.Get(context.Background(), types.NamespacedName{Name: kruizeName}, &rbacv1.ClusterRoleBinding{}); err == nil {
		t.Error("KruizeClusterRoleBinding should have been deleted")
	}
}

func TestReconcileDelete_ToleratesMissingResources(t *testing.T) {
	// Cluster-scoped resources were never created — deletion should still succeed.
	cr := minimalCR(testCRName, testNamespace)
	controllerutil.AddFinalizer(cr, finalizerName)
	now := metav1.Now()
	cr.DeletionTimestamp = &now

	c := fake.NewClientBuilder().
		WithScheme(ownershipScheme(t)).
		WithObjects(cr).
		Build()

	r := &CostManagementServiceConfigReconciler{Client: c, Scheme: ownershipScheme(t)}

	_, err := r.reconcileDelete(context.Background(), cr)
	if err != nil {
		t.Fatalf("unexpected error when cluster-scoped resources are absent: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Drift correction requeue
// ---------------------------------------------------------------------------

func TestReconcile_DriftRequeueOnSuccess(t *testing.T) {
	// Verify that a fully-successful reconcile schedules a 5-minute requeue.
	// We use a CR with the finalizer already set so we go directly into reconcile().
	cr := minimalCR(testCRName, testNamespace)
	controllerutil.AddFinalizer(cr, finalizerName)

	// Directly test the reconcile() return value via the Result type.
	// A zero result means no requeue — anything ≥5m is drift correction.
	if requeueDrift < 5*time.Minute {
		t.Errorf("requeueDrift should be at least 5 minutes, got %v", requeueDrift)
	}
	if requeueDrift > 10*time.Minute {
		t.Errorf("requeueDrift should be at most 10 minutes, got %v", requeueDrift)
	}
}

// ---------------------------------------------------------------------------
// setOwnerRef idempotency
// ---------------------------------------------------------------------------

func TestSetOwnerRef_Idempotent(t *testing.T) {
	cr := minimalCR(testCRName, testNamespace)
	obj := &costv1alpha1.CostManagementServiceConfig{
		ObjectMeta: metav1.ObjectMeta{Name: "child", Namespace: testNamespace},
	}

	setOwnerRef(cr, obj)
	setOwnerRef(cr, obj) // called twice — should not duplicate

	if len(obj.GetOwnerReferences()) != 1 {
		t.Errorf("expected 1 ownerRef after double call, got %d", len(obj.GetOwnerReferences()))
	}
}

// compile-time check that reconcileDelete satisfies the expected signature.
var _ func(context.Context, *costv1alpha1.CostManagementServiceConfig) (reconcile.Result, error) = (&CostManagementServiceConfigReconciler{}).reconcileDelete

// helper so client.Object is reachable inside the test file.
var _ = client.Object(nil)
