package controller

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	costv1alpha1 "github.com/project-koku/koku-service-operator/api/v1alpha1"
	"github.com/project-koku/koku-service-operator/internal/resources"
)

// TestMonitoringRealApplyErrorSurfaces verifies that non-CRD-absent errors
// from reconcileMonitoring are returned rather than silently swallowed.
func TestMonitoringRealApplyErrorSurfaces(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = costv1alpha1.AddToScheme(scheme)

	cfg := &costv1alpha1.CostManagementServiceConfig{
		ObjectMeta: metav1.ObjectMeta{Name: testCRName, Namespace: "test"},
	}

	realErr := errors.New("etcd is on fire")

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithInterceptorFuncs(interceptor.Funcs{
			Patch: func(_ context.Context, _ client.WithWatch, _ client.Object, _ client.Patch, _ ...client.PatchOption) error {
				return realErr
			},
		}).
		Build()

	r := &CostManagementServiceConfigReconciler{
		Client:   fakeClient,
		Recorder: &noopRecorder{},
	}

	_, err := r.reconcileMonitoring(context.Background(), cfg)
	if err == nil {
		t.Error("reconcileMonitoring should surface non-CRD-absent apply errors, got nil")
	}
}

// TestMonitoringCRDAbsentSkipsResource verifies IsNoMatchError is treated as success.
func TestMonitoringCRDAbsentSkipsResource(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = costv1alpha1.AddToScheme(scheme)

	cfg := &costv1alpha1.CostManagementServiceConfig{
		ObjectMeta: metav1.ObjectMeta{Name: testCRName, Namespace: testNamespace},
	}

	noMatchErr := &apimeta.NoKindMatchError{GroupKind: schema.GroupKind{Group: "monitoring.coreos.com", Kind: "ServiceMonitor"}}

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithInterceptorFuncs(interceptor.Funcs{
			Patch: func(_ context.Context, _ client.WithWatch, _ client.Object, _ client.Patch, _ ...client.PatchOption) error {
				return noMatchErr
			},
		}).
		Build()

	r := &CostManagementServiceConfigReconciler{
		Client:   fakeClient,
		Recorder: &noopRecorder{},
	}

	result, err := r.reconcileMonitoring(context.Background(), cfg)
	if err != nil {
		t.Errorf("reconcileMonitoring should skip CRD-absent resources (got error: %v)", err)
	}
	if !result.IsZero() {
		t.Errorf("reconcileMonitoring should return zero result on CRD-absent, got %+v", result)
	}
}

// TestMonitoringAppliesServiceMonitorAndPrometheusRule ensures PR2 applies both.
func TestMonitoringAppliesServiceMonitorAndPrometheusRule(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = costv1alpha1.AddToScheme(scheme)

	cfg := &costv1alpha1.CostManagementServiceConfig{
		ObjectMeta: metav1.ObjectMeta{Name: testCRName, Namespace: testNamespace},
	}

	var patchKinds []string

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithInterceptorFuncs(interceptor.Funcs{
			Patch: func(_ context.Context, _ client.WithWatch, obj client.Object, _ client.Patch, _ ...client.PatchOption) error {
				patchKinds = append(patchKinds, obj.GetObjectKind().GroupVersionKind().Kind)
				return nil
			},
		}).
		Build()

	r := &CostManagementServiceConfigReconciler{
		Client:   fakeClient,
		Recorder: &noopRecorder{},
	}

	if _, err := r.reconcileMonitoring(context.Background(), cfg); err != nil {
		t.Fatalf("reconcileMonitoring: %v", err)
	}
	if len(patchKinds) != 6 ||
		patchKinds[0] != "ServiceMonitor" ||
		patchKinds[1] != "ServiceMonitor" ||
		patchKinds[2] != "ServiceMonitor" ||
		patchKinds[3] != "ServiceMonitor" ||
		patchKinds[4] != "ServiceMonitor" ||
		patchKinds[5] != "PrometheusRule" {
		t.Errorf("expected App+Gateway+Operator+Celery+RBAC ServiceMonitors then PrometheusRule, got %v", patchKinds)
	}
}

// TestMonitoringDisabledDeletesManagedResources verifies disable cleanup.
func TestMonitoringDisabledDeletesManagedResources(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = costv1alpha1.AddToScheme(scheme)

	enabled := false
	cfg := &costv1alpha1.CostManagementServiceConfig{
		ObjectMeta: metav1.ObjectMeta{Name: testCRName, Namespace: testNamespace},
		Spec: costv1alpha1.CostManagementServiceConfigSpec{
			Monitoring: costv1alpha1.MonitoringConfig{Enabled: &enabled},
		},
	}

	var deleted []string
	var patched atomic.Bool

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithInterceptorFuncs(interceptor.Funcs{
			Patch: func(_ context.Context, _ client.WithWatch, _ client.Object, _ client.Patch, _ ...client.PatchOption) error {
				patched.Store(true)
				return nil
			},
			Delete: func(_ context.Context, _ client.WithWatch, obj client.Object, _ ...client.DeleteOption) error {
				deleted = append(deleted, obj.GetName())
				return nil
			},
		}).
		Build()

	r := &CostManagementServiceConfigReconciler{
		Client:   fakeClient,
		Recorder: &noopRecorder{},
	}

	if _, err := r.reconcileMonitoring(context.Background(), cfg); err != nil {
		t.Fatalf("reconcileMonitoring: %v", err)
	}
	if patched.Load() {
		t.Error("disabled monitoring must not Patch/apply resources")
	}
	wantDelete := map[string]bool{
		testCRName + "-app-metrics":      false,
		testCRName + "-gateway-metrics":  false,
		testCRName + "-operator-metrics": false,
		testCRName + "-celery-metrics":   false,
		testCRName + "-rbac-metrics":     false,
		testCRName + "-alerts":           false,
		testCRName + "-kruize-metrics":   false, // not applied yet (COST-8054); cleanup on disable
	}
	for _, name := range deleted {
		if _, ok := wantDelete[name]; ok {
			wantDelete[name] = true
		}
	}
	for name, ok := range wantDelete {
		if !ok {
			t.Errorf("expected delete of %s, got deleted=%v", name, deleted)
		}
	}
}

// TestMonitoringDisabledDeletesKruizeServiceMonitor ensures disable removes
// the Kruize ServiceMonitor by name even though it is not applied in beta
// (ROS scrape lands in COST-8054).
func TestMonitoringDisabledDeletesKruizeServiceMonitor(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = costv1alpha1.AddToScheme(scheme)

	enabled := false
	cfg := &costv1alpha1.CostManagementServiceConfig{
		ObjectMeta: metav1.ObjectMeta{Name: testCRName, Namespace: testNamespace},
		Spec: costv1alpha1.CostManagementServiceConfigSpec{
			Monitoring: costv1alpha1.MonitoringConfig{Enabled: &enabled},
		},
	}

	legacy := resources.KruizeServiceMonitor(cfg)
	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(legacy).
		Build()

	r := &CostManagementServiceConfigReconciler{
		Client:   fakeClient,
		Recorder: &noopRecorder{},
	}

	if _, err := r.reconcileMonitoring(context.Background(), cfg); err != nil {
		t.Fatalf("reconcileMonitoring: %v", err)
	}

	got := legacy.DeepCopy()
	err := fakeClient.Get(context.Background(), client.ObjectKey{
		Namespace: testNamespace,
		Name:      testCRName + "-kruize-metrics",
	}, got)
	if err == nil {
		t.Fatal("expected Kruize ServiceMonitor to be deleted on monitoring disable")
	}
	if !apierrors.IsNotFound(err) {
		t.Fatalf("expected NotFound after disable cleanup, got %v", err)
	}
}

func TestEmitPhaseChanged_OnlyOnChange(t *testing.T) {
	rec := record.NewFakeRecorder(4)
	r := &CostManagementServiceConfigReconciler{Recorder: rec}
	cfg := &costv1alpha1.CostManagementServiceConfig{
		ObjectMeta: metav1.ObjectMeta{Name: testCRName, Namespace: testNamespace},
	}

	r.emitPhaseChanged(cfg, costv1alpha1.PhaseProgressing, costv1alpha1.PhaseReady)
	assertEvent(t, rec, "PhaseChanged")

	r.emitPhaseChanged(cfg, costv1alpha1.PhaseReady, costv1alpha1.PhaseReady)
	assertNoEvent(t, rec, "PhaseChanged")
}

func TestEmitDependencyFailed_OnlyOnTransition(t *testing.T) {
	rec := record.NewFakeRecorder(4)
	r := &CostManagementServiceConfigReconciler{Recorder: rec}
	cfg := &costv1alpha1.CostManagementServiceConfig{
		ObjectMeta: metav1.ObjectMeta{Name: testCRName, Namespace: testNamespace},
	}

	r.emitDependencyFailed(cfg, costv1alpha1.ConditionDatabaseReady, "unreachable")
	assertEvent(t, rec, "DependencyFailed")

	r.setCondition(cfg, costv1alpha1.ConditionDatabaseReady, metav1.ConditionFalse, "DatabaseUnreachable", "unreachable")
	r.emitDependencyFailed(cfg, costv1alpha1.ConditionDatabaseReady, "unreachable again")
	assertNoEvent(t, rec, "DependencyFailed")
}

func TestMigrationsCompleteEvent_Reason(t *testing.T) {
	rec := record.NewFakeRecorder(2)
	r := &CostManagementServiceConfigReconciler{Recorder: rec}
	cfg := &costv1alpha1.CostManagementServiceConfig{
		ObjectMeta: metav1.ObjectMeta{Name: testCRName, Namespace: testNamespace},
	}

	// Simulate completion path: SchemaUpToDate not yet True.
	if !apimeta.IsStatusConditionTrue(cfg.Status.Conditions, costv1alpha1.ConditionSchemaUpToDate) {
		r.Recorder.Event(cfg, "Normal", "MigrationsComplete", "All schema migrations succeeded")
	}
	r.setCondition(cfg, costv1alpha1.ConditionSchemaUpToDate, metav1.ConditionTrue, "MigrationComplete", "ok")
	assertEvent(t, rec, "MigrationsComplete")

	if !apimeta.IsStatusConditionTrue(cfg.Status.Conditions, costv1alpha1.ConditionSchemaUpToDate) {
		r.Recorder.Event(cfg, "Normal", "MigrationsComplete", "should not fire")
	}
	assertNoEvent(t, rec, "MigrationsComplete")
}
