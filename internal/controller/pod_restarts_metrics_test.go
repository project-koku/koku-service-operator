package controller

import (
	"context"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	costv1alpha1 "github.com/project-koku/koku-service-operator/api/v1alpha1"
	opmetrics "github.com/project-koku/koku-service-operator/internal/metrics"
)

func managedPod(name string, restarts int32) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: testNamespace,
			Labels: map[string]string{
				"app.kubernetes.io/instance":   testCRName,
				"app.kubernetes.io/managed-by": "koku-service-operator",
			},
		},
		Status: corev1.PodStatus{
			ContainerStatuses: []corev1.ContainerStatus{{
				Name:         "app",
				RestartCount: restarts,
			}},
		},
	}
}

func TestSyncManagedPodRestarts_ClearsDeletedAndReplacedPods(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = costv1alpha1.AddToScheme(scheme)

	cfg := &costv1alpha1.CostManagementServiceConfig{
		ObjectMeta: metav1.ObjectMeta{Name: testCRName, Namespace: testNamespace},
	}
	t.Cleanup(func() { opmetrics.ClearManagedPodRestarts(testNamespace, testCRName) })

	// Stale series from a prior reconcile (pod gone / replaced).
	opmetrics.ManagedPodRestarts.WithLabelValues(testNamespace, testCRName, "old-pod", "app").Set(9)
	opmetrics.ManagedPodRestarts.WithLabelValues(testNamespace, testCRName, "replaced-pod", "app").Set(4)

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(managedPod("current-pod", 2)).
		Build()

	r := &CostManagementServiceConfigReconciler{Client: fakeClient}
	if err := r.syncManagedPodRestarts(context.Background(), cfg); err != nil {
		t.Fatalf("syncManagedPodRestarts: %v", err)
	}

	// DeleteLabelValues returns true only if the series still existed.
	if opmetrics.ManagedPodRestarts.DeleteLabelValues(testNamespace, testCRName, "old-pod", "app") {
		t.Error("expected old-pod restart series to be deleted")
	}
	if opmetrics.ManagedPodRestarts.DeleteLabelValues(testNamespace, testCRName, "replaced-pod", "app") {
		t.Error("expected replaced-pod restart series to be deleted")
	}
	if got := testutil.ToFloat64(opmetrics.ManagedPodRestarts.WithLabelValues(testNamespace, testCRName, "current-pod", "app")); got != 2 {
		t.Fatalf("current-pod restarts = %v, want 2", got)
	}
}

func TestClearManagedPodRestarts_OnCMSCDelete(t *testing.T) {
	t.Cleanup(func() { opmetrics.ClearManagedPodRestarts(testNamespace, testCRName) })
	opmetrics.ManagedPodRestarts.WithLabelValues(testNamespace, testCRName, "p1", "app").Set(3)
	if n := opmetrics.ClearManagedPodRestarts(testNamespace, testCRName); n != 1 {
		t.Fatalf("ClearManagedPodRestarts deleted %d series, want 1", n)
	}
	if opmetrics.ManagedPodRestarts.DeleteLabelValues(testNamespace, testCRName, "p1", "app") {
		t.Error("expected CMSC restart series cleared on delete helper")
	}
}
