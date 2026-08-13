package controller

import (
	"context"
	"errors"
	"testing"

	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	costv1alpha1 "github.com/project-koku/koku-service-operator/api/v1alpha1"
)

// TestMonitoringRealApplyErrorSurfaces verifies that non-CRD-absent errors
// from reconcileMonitoring are returned rather than silently swallowed.
//
// Bug (F5): any apply error in reconcileMonitoring was caught and logged but
// the stage always returned (Result{}, nil), hiding real failures from the
// reconcile loop. The Degraded condition was never set and the CR stayed
// Progressing forever.
func TestMonitoringRealApplyErrorSurfaces(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = costv1alpha1.AddToScheme(scheme)

	cfg := &costv1alpha1.CostManagementServiceConfig{
		ObjectMeta: metav1.ObjectMeta{Name: testCRName, Namespace: "test"},
	}

	realErr := errors.New("etcd is on fire")

	// Use an interceptor client that returns a real (non-CRD-absent) error on Patch.
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

// TestMonitoringCRDAbsentSkipsResource verifies that when apply returns an
// IsNoMatchError (Prometheus Operator CRDs not installed), reconcileMonitoring
// silently skips that resource and returns success — the operator should work
// on clusters without the monitoring stack.
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
