package controller

import (
	"context"
	"fmt"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/project-koku/koku-service-operator/internal/resources"
)

func TestEnsureServiceAccount_CreateTrueApplies(t *testing.T) {
	cfg := minimalCR(testCRName, testNamespace)
	r := &CostManagementServiceConfigReconciler{
		Client:   fakeClientWithApplySupport(sharedConfigScheme(t)),
		Recorder: &noopRecorder{},
	}

	sa := resources.KokuServiceAccount(cfg)
	if err := r.ensureServiceAccount(context.Background(), cfg, cfg.Spec.CostManagement.ServiceAccount, sa); err != nil {
		t.Fatalf("ensureServiceAccount: %v", err)
	}

	got := &corev1.ServiceAccount{}
	if err := r.Get(context.Background(), types.NamespacedName{Namespace: testNamespace, Name: sa.Name}, got); err != nil {
		t.Fatalf("Get SA: %v", err)
	}
	if len(got.OwnerReferences) != 1 {
		t.Fatalf("expected ownerRef on managed SA, got %d", len(got.OwnerReferences))
	}
}

func TestEnsureServiceAccount_CreateFalseSkipsApply(t *testing.T) {
	cfg := minimalCR(testCRName, testNamespace)
	cfg.Spec.CostManagement.ServiceAccount.Create = new(false)
	cfg.Spec.CostManagement.ServiceAccount.Name = "external-koku-sa"

	existing := &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{Name: "external-koku-sa", Namespace: testNamespace},
	}

	var patched bool
	c := fake.NewClientBuilder().
		WithScheme(sharedConfigScheme(t)).
		WithObjects(existing).
		WithInterceptorFuncs(interceptor.Funcs{
			Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
				patched = true
				return c.Patch(ctx, obj, patch, opts...)
			},
		}).
		Build()

	r := &CostManagementServiceConfigReconciler{Client: c, Recorder: &noopRecorder{}}
	sa := resources.KokuServiceAccount(cfg)
	if err := r.ensureServiceAccount(context.Background(), cfg, cfg.Spec.CostManagement.ServiceAccount, sa); err != nil {
		t.Fatalf("ensureServiceAccount: %v", err)
	}
	if patched {
		t.Fatal("create=false must not Patch/apply the ServiceAccount")
	}

	got := &corev1.ServiceAccount{}
	if err := r.Get(context.Background(), types.NamespacedName{Namespace: testNamespace, Name: "external-koku-sa"}, got); err != nil {
		t.Fatalf("Get SA: %v", err)
	}
	if len(got.OwnerReferences) != 0 {
		t.Fatalf("external SA must not gain ownerRefs, got %#v", got.OwnerReferences)
	}
}

func TestEnsureServiceAccount_CreateFalseMissingErrors(t *testing.T) {
	cfg := minimalCR(testCRName, testNamespace)
	cfg.Spec.ROS.ServiceAccount.Create = new(false)
	cfg.Spec.ROS.ServiceAccount.Name = "missing-ros-sa"

	r := &CostManagementServiceConfigReconciler{
		Client:   fake.NewClientBuilder().WithScheme(sharedConfigScheme(t)).Build(),
		Recorder: &noopRecorder{},
	}
	sa := resources.ROSServiceAccount(cfg)
	err := r.ensureServiceAccount(context.Background(), cfg, cfg.Spec.ROS.ServiceAccount, sa)
	if err == nil {
		t.Fatal("expected error when create=false and SA is missing")
	}
	if !strings.Contains(err.Error(), "create=false") {
		t.Fatalf("error should mention create=false, got: %v", err)
	}
	if !strings.Contains(err.Error(), testNamespace+"/missing-ros-sa") {
		t.Fatalf("error should include namespace/name, got: %v", err)
	}
}

func TestEnsureServiceAccount_CreateFalseGetError(t *testing.T) {
	cfg := minimalCR(testCRName, testNamespace)
	cfg.Spec.CostManagement.ServiceAccount.Create = new(false)

	c := fake.NewClientBuilder().
		WithScheme(sharedConfigScheme(t)).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				return fmt.Errorf("apiserver unavailable")
			},
		}).
		Build()

	r := &CostManagementServiceConfigReconciler{Client: c, Recorder: &noopRecorder{}}
	sa := resources.KokuServiceAccount(cfg)
	err := r.ensureServiceAccount(context.Background(), cfg, cfg.Spec.CostManagement.ServiceAccount, sa)
	if err == nil || !strings.Contains(err.Error(), "get serviceaccount") {
		t.Fatalf("expected wrapped get error, got: %v", err)
	}
	if !strings.Contains(err.Error(), testNamespace+"/"+sa.Name) {
		t.Fatalf("error should include namespace/name, got: %v", err)
	}
}

func TestEnsureServiceAccount_ROSCreateFalseSkipsApply(t *testing.T) {
	cfg := minimalCR(testCRName, testNamespace)
	cfg.Spec.ROS.ServiceAccount.Create = new(false)
	cfg.Spec.ROS.ServiceAccount.Name = "external-ros-sa"

	existing := &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{Name: "external-ros-sa", Namespace: testNamespace},
	}

	var patched bool
	c := fake.NewClientBuilder().
		WithScheme(sharedConfigScheme(t)).
		WithObjects(existing).
		WithInterceptorFuncs(interceptor.Funcs{
			Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
				patched = true
				return c.Patch(ctx, obj, patch, opts...)
			},
		}).
		Build()

	r := &CostManagementServiceConfigReconciler{Client: c, Recorder: &noopRecorder{}}
	if err := r.ensureServiceAccount(context.Background(), cfg, cfg.Spec.ROS.ServiceAccount, resources.ROSServiceAccount(cfg)); err != nil {
		t.Fatalf("ensureServiceAccount: %v", err)
	}
	if patched {
		t.Fatal("create=false must not Patch/apply the ROS ServiceAccount")
	}
}

func TestReconcileSharedConfig_CreateFalseSkipsKokuSA(t *testing.T) {
	cfg := minimalCR(testCRName, testNamespace)
	cfg.Spec.CostManagement.ServiceAccount.Create = new(false)
	cfg.Spec.CostManagement.ServiceAccount.Name = "external-koku-sa"

	existing := &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{Name: "external-koku-sa", Namespace: testNamespace},
	}
	r := &CostManagementServiceConfigReconciler{
		Client:   fakeClientWithApplySupport(sharedConfigScheme(t), existing),
		Recorder: &noopRecorder{},
	}

	if _, err := r.reconcileSharedConfig(context.Background(), cfg); err != nil {
		t.Fatalf("reconcileSharedConfig: %v", err)
	}

	got := &corev1.ServiceAccount{}
	if err := r.Get(context.Background(), types.NamespacedName{Namespace: testNamespace, Name: "external-koku-sa"}, got); err != nil {
		t.Fatalf("Get SA: %v", err)
	}
	if len(got.OwnerReferences) != 0 {
		t.Fatalf("external SA must not gain ownerRefs via reconcileSharedConfig, got %#v", got.OwnerReferences)
	}
}

func TestReconcileSharedConfig_CreateFalseMissingSAErrors(t *testing.T) {
	cfg := minimalCR(testCRName, testNamespace)
	cfg.Spec.CostManagement.ServiceAccount.Create = new(false)
	cfg.Spec.CostManagement.ServiceAccount.Name = "missing-koku-sa"

	r := &CostManagementServiceConfigReconciler{
		Client:   fakeClientWithApplySupport(sharedConfigScheme(t)),
		Recorder: &noopRecorder{},
	}

	_, err := r.reconcileSharedConfig(context.Background(), cfg)
	if err == nil {
		t.Fatal("expected reconcileSharedConfig error when create=false SA is missing")
	}
	if !strings.Contains(err.Error(), "koku serviceaccount") {
		t.Fatalf("error should wrap koku serviceaccount call site, got: %v", err)
	}
	if !strings.Contains(err.Error(), "create=false") {
		t.Fatalf("error should mention create=false, got: %v", err)
	}
}

func TestROSCleanupObjects_OmitsSAWhenCreateFalse(t *testing.T) {
	cfg := minimalCR(testCRName, testNamespace)
	cfg.Spec.ROS.ServiceAccount.Create = new(false)

	for _, obj := range rosCleanupObjects(cfg) {
		if obj.GetName() == resources.NameROSServiceAccount(cfg) {
			t.Fatalf("rosCleanupObjects must not delete external ROS SA when create=false")
		}
	}
}

func TestROSCleanupObjects_IncludesSAWhenCreateTrue(t *testing.T) {
	cfg := minimalCR(testCRName, testNamespace)
	// Default Create=true (nil) must keep the operator-managed SA on the cleanup list.
	found := false
	for _, obj := range rosCleanupObjects(cfg) {
		if obj.GetName() == resources.NameROSServiceAccount(cfg) {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("rosCleanupObjects must include ROS SA when create=true")
	}
}
