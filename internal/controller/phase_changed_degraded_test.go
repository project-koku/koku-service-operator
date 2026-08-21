package controller

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	costv1alpha1 "github.com/project-koku/koku-service-operator/api/v1alpha1"
	opmetrics "github.com/project-koku/koku-service-operator/internal/metrics"
)

// ssaCreateOrUpdate makes fake-client Server-Side Apply usable in unit tests
// (Patch Apply otherwise fails with NotFound for new objects).
func ssaCreateOrUpdate() interceptor.Funcs {
	return interceptor.Funcs{
		Patch: func(ctx context.Context, inner client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
			if patch.Type() != types.ApplyPatchType {
				return inner.Patch(ctx, obj, patch, opts...)
			}
			key := client.ObjectKeyFromObject(obj)
			got := obj.DeepCopyObject().(client.Object)
			err := inner.Get(ctx, key, got)
			if apierrors.IsNotFound(err) {
				return inner.Create(ctx, obj)
			}
			if err != nil {
				return err
			}
			obj.SetResourceVersion(got.GetResourceVersion())
			return inner.Update(ctx, obj)
		},
	}
}

// TestReconcile_EmitsPhaseChangedOnValidationDegraded covers Ready→Degraded when
// a blocking dependency probe returns RequeueAfter (no error), which previously
// skipped emitPhaseChanged.
func TestReconcile_EmitsPhaseChangedOnValidationDegraded(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = costv1alpha1.AddToScheme(scheme)

	cfg := &costv1alpha1.CostManagementServiceConfig{
		ObjectMeta: metav1.ObjectMeta{Name: testCRName, Namespace: testNamespace, UID: "uid-phase"},
		Spec: costv1alpha1.CostManagementServiceConfigSpec{
			Global: costv1alpha1.GlobalConfig{
				ClusterDomain: "apps.example.com",
				StorageClass:  "gp3",
			},
			Database: costv1alpha1.DatabaseConfig{
				Deploy: falsePtr(),
				Host:   "127.0.0.1",
				Port:   1,
			},
			Cache: costv1alpha1.CacheConfig{
				Deploy: falsePtr(),
				Host:   "127.0.0.1",
				Port:   1,
			},
			ObjectStorage: costv1alpha1.ObjectStorageConfig{SecretName: "s3-creds"},
			Kafka:         costv1alpha1.KafkaConfig{BootstrapServers: ""},
		},
		Status: costv1alpha1.CostManagementServiceConfigStatus{
			Phase: costv1alpha1.PhaseReady,
		},
	}
	controllerutil.AddFinalizer(cfg, finalizerName)

	s3 := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "s3-creds", Namespace: testNamespace},
		Data: map[string][]byte{
			"access-key": []byte("ak"),
			"secret-key": []byte("sk"),
		},
	}

	rec := record.NewFakeRecorder(8)
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(cfg, s3).
		WithStatusSubresource(cfg).
		WithInterceptorFuncs(ssaCreateOrUpdate()).
		Build()
	r := &CostManagementServiceConfigReconciler{
		Client:   c,
		Scheme:   scheme,
		Recorder: rec,
	}

	_, err := r.reconcile(context.Background(), cfg)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if cfg.Status.Phase != costv1alpha1.PhaseDegraded {
		t.Fatalf("phase=%q want Degraded", cfg.Status.Phase)
	}
	assertEvent(t, rec, "PhaseChanged")
}

func TestReconcileDelete_ClearsConditionAndMigrationMetrics(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = costv1alpha1.AddToScheme(scheme)

	cfg := minimalCR(testCRName, testNamespace)
	controllerutil.AddFinalizer(cfg, finalizerName)
	now := metav1.Now()
	cfg.DeletionTimestamp = &now

	t.Cleanup(func() {
		opmetrics.ClearManagedPodRestarts(testNamespace, testCRName)
		opmetrics.ClearConditionMetrics(testNamespace, testCRName)
		opmetrics.ClearMigrationJobFailedAll(testNamespace, testCRName)
	})

	opmetrics.ManagedPodRestarts.WithLabelValues(testNamespace, testCRName, "p1", "app").Set(2)
	opmetrics.SetCondition(testNamespace, testCRName, "Degraded", metav1.ConditionTrue)
	opmetrics.SetMigrationJobFailed(testNamespace, testCRName, testCRName+"-koku-migrate", true)

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cfg).Build()
	r := &CostManagementServiceConfigReconciler{Client: c, Scheme: scheme}

	if _, err := r.reconcileDelete(context.Background(), cfg); err != nil {
		t.Fatalf("reconcileDelete: %v", err)
	}

	if opmetrics.ManagedPodRestarts.DeleteLabelValues(testNamespace, testCRName, "p1", "app") {
		t.Error("expected managed pod restart series cleared")
	}
	if opmetrics.Condition.DeleteLabelValues(testNamespace, testCRName, "Degraded", "True") {
		t.Error("expected condition series cleared")
	}
	if opmetrics.MigrationJobFailed.DeleteLabelValues(testNamespace, testCRName, testCRName+"-koku-migrate") {
		t.Error("expected migration-failed series cleared")
	}
}
