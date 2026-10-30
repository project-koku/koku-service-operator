package controller

import (
	"context"
	"strings"
	"testing"

	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	costv1alpha1 "github.com/project-koku/koku-service-operator/api/v1alpha1"
	"github.com/project-koku/koku-service-operator/internal/resources"
)

func TestDiscoveryCompleteEvent_FiresOnce(t *testing.T) {
	c := fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithObjects(
			openShiftIngress(testClusterDomain),
			defaultStorageClass("gp3-csi"),
		).
		Build()

	rec := record.NewFakeRecorder(10)
	r := &CostManagementServiceConfigReconciler{Client: c, Recorder: rec}
	cfg := &costv1alpha1.CostManagementServiceConfig{
		ObjectMeta: metav1.ObjectMeta{Name: testCRName, Namespace: testNamespace},
		Spec: costv1alpha1.CostManagementServiceConfigSpec{
			Global: costv1alpha1.GlobalConfig{
				ClusterDomain: testClusterDomain,
				StorageClass:  "gp3-csi",
			},
		},
	}

	// First call — should emit DiscoveryComplete.
	if _, err := r.reconcileDiscovery(context.Background(), cfg); err != nil {
		t.Fatalf("pass 1: %v", err)
	}
	assertEvent(t, rec, "DiscoveryComplete")

	// Second call — condition already True, should NOT emit.
	if _, err := r.reconcileDiscovery(context.Background(), cfg); err != nil {
		t.Fatalf("pass 2: %v", err)
	}
	assertNoEvent(t, rec, "DiscoveryComplete")
}

func TestInfrastructureReadyEvent_ExternalInfra(t *testing.T) {
	falseVal := false
	scheme := testScheme(t)
	c := fake.NewClientBuilder().WithScheme(scheme).Build()

	rec := record.NewFakeRecorder(10)
	r := &CostManagementServiceConfigReconciler{Client: c, Scheme: scheme, Recorder: rec}
	cfg := &costv1alpha1.CostManagementServiceConfig{
		ObjectMeta: metav1.ObjectMeta{Name: testCRName, Namespace: testNamespace},
		Spec: costv1alpha1.CostManagementServiceConfigSpec{
			Database: costv1alpha1.DatabaseConfig{Deploy: &falseVal},
			Cache:    costv1alpha1.CacheConfig{Deploy: &falseVal},
		},
	}

	// First call with external infra — no waiting, should emit.
	result, err := r.reconcileInfrastructure(context.Background(), cfg)
	if err != nil {
		t.Fatalf("pass 1: %v", err)
	}
	if !result.IsZero() {
		t.Fatalf("expected zero result for external infra, got %+v", result)
	}
	assertEvent(t, rec, "InfrastructureReady")

	// Second call — should NOT emit.
	if _, err := r.reconcileInfrastructure(context.Background(), cfg); err != nil {
		t.Fatalf("pass 2: %v", err)
	}
	assertNoEvent(t, rec, "InfrastructureReady")
}

func TestInfrastructureReadyEvent_DefaultsToExternalInfraWhenDeployUnset(t *testing.T) {
	scheme := testScheme(t)
	c := fake.NewClientBuilder().WithScheme(scheme).Build()

	rec := record.NewFakeRecorder(10)
	r := &CostManagementServiceConfigReconciler{Client: c, Scheme: scheme, Recorder: rec}
	cfg := &costv1alpha1.CostManagementServiceConfig{
		ObjectMeta: metav1.ObjectMeta{Name: testCRName, Namespace: testNamespace},
	}

	result, err := r.reconcileInfrastructure(context.Background(), cfg)
	if err != nil {
		t.Fatalf("pass 1: %v", err)
	}
	if !result.IsZero() {
		t.Fatalf("expected zero result for default BYOI infra, got %+v", result)
	}
	assertEvent(t, rec, "InfrastructureReady")

	db := apimeta.FindStatusCondition(cfg.Status.Conditions, costv1alpha1.ConditionDatabaseReady)
	if db == nil || db.Status != metav1.ConditionTrue || db.Reason != "ExternalDatabase" {
		t.Fatalf("expected DatabaseReady=True ExternalDatabase, got %+v", db)
	}
	cache := apimeta.FindStatusCondition(cfg.Status.Conditions, costv1alpha1.ConditionCacheReady)
	if cache == nil || cache.Status != metav1.ConditionTrue || cache.Reason != "ExternalCache" {
		t.Fatalf("expected CacheReady=True ExternalCache, got %+v", cache)
	}
}

// Core services becoming ready must not emit a success Event or publish
// Available=True: only the end of reconcile() does that via Ready (COST-8103).
func TestCoreServices_Ready_NoSuccessEvent(t *testing.T) {
	scheme := ownershipScheme(t)
	cfg := minimalCR(testCRName, testNamespace)
	c := fakeClientPreservingStatus(scheme)
	rec := record.NewFakeRecorder(10)
	r := &CostManagementServiceConfigReconciler{Client: c, Scheme: scheme, Recorder: rec}

	if _, err := r.reconcileCoreServices(context.Background(), cfg); err != nil {
		t.Fatalf("first pass: %v", err)
	}
	for _, name := range []string{
		resources.NameRBACAPI(cfg), resources.NameRBACWorker(cfg), resources.NameKokuAPI(cfg),
		resources.NameMasu(cfg), resources.NameListener(cfg),
	} {
		markDeploymentReady(t, c, testNamespace, name)
	}

	if _, err := r.reconcileCoreServices(context.Background(), cfg); err != nil {
		t.Fatalf("second pass: %v", err)
	}
	assertNoEvent(t, rec, "CoreServicesAvailable")
	if !apimeta.IsStatusConditionTrue(cfg.Status.Conditions, costv1alpha1.ConditionCoreServicesReady) {
		t.Fatal("expected CoreServicesReady=True")
	}
	if apimeta.IsStatusConditionTrue(cfg.Status.Conditions, costv1alpha1.ConditionAvailable) {
		t.Fatal("Available must not be True before workers and Edge complete")
	}
}

func TestMigrationsCompleteEvent_FiresOnce(t *testing.T) {
	r, cfg, c := newMigrationTestReconciler(t)
	rec := record.NewFakeRecorder(20)
	r.Recorder = rec

	for _, jobName := range []string{resources.NameKokuMigration(cfg), resources.NameRBACMigration(cfg)} {
		if _, err := r.reconcileMigration(context.Background(), cfg); err != nil {
			t.Fatalf("step %s: %v", jobName, err)
		}
		markJobComplete(t, c, testNamespace, jobName)
	}

	// First pass with all Jobs complete — transition to SchemaUpToDate=True.
	if _, err := r.reconcileMigration(context.Background(), cfg); err != nil {
		t.Fatalf("final pass: %v", err)
	}
	assertEvent(t, rec, "MigrationsComplete")

	// Drift pass — schema already up to date, should NOT emit.
	if _, err := r.reconcileMigration(context.Background(), cfg); err != nil {
		t.Fatalf("drift pass: %v", err)
	}
	assertNoEvent(t, rec, "MigrationsComplete")
}

func TestPausedEvent_FiresOnce(t *testing.T) {
	cr := minimalCR(testCRName, testNamespace)
	controllerutil.AddFinalizer(cr, finalizerName)
	cr.Annotations = map[string]string{pauseAnnotation: annotationTrue}

	c := fake.NewClientBuilder().
		WithScheme(ownershipScheme(t)).
		WithObjects(cr).
		WithStatusSubresource(cr).
		Build()
	rec := record.NewFakeRecorder(10)
	r := &CostManagementServiceConfigReconciler{Client: c, Scheme: ownershipScheme(t), Recorder: rec}
	req := reconcile.Request{NamespacedName: types.NamespacedName{Name: testCRName, Namespace: testNamespace}}

	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("pass 1: %v", err)
	}
	assertEvent(t, rec, "Paused")

	// Still paused — Paused=True already persisted, should NOT emit.
	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("pass 2: %v", err)
	}
	assertNoEvent(t, rec, "Paused")
}

// assertEvent drains the recorder and checks that at least one event contains reason.
func assertEvent(t *testing.T, rec *record.FakeRecorder, reason string) {
	t.Helper()
	for {
		select {
		case event := <-rec.Events:
			if strings.Contains(event, reason) {
				return
			}
		default:
			t.Errorf("expected %s event, got none", reason)
			return
		}
	}
}

// assertNoEvent drains the recorder and fails if any event contains reason.
func assertNoEvent(t *testing.T, rec *record.FakeRecorder, reason string) {
	t.Helper()
	for {
		select {
		case event := <-rec.Events:
			if strings.Contains(event, reason) {
				t.Errorf("unexpected %s event on second pass: %s", reason, event)
				return
			}
		default:
			return
		}
	}
}

// staleCMSCClient simulates informer-cache lag: Get on the CMSC returns a
// snapshot taken before the previous reconcile patched status.
type staleCMSCClient struct {
	client.Client
	stale *costv1alpha1.CostManagementServiceConfig
}

func (s *staleCMSCClient) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	if cfg, ok := obj.(*costv1alpha1.CostManagementServiceConfig); ok && key.Name == s.stale.Name && key.Namespace == s.stale.Namespace {
		s.stale.DeepCopyInto(cfg)
		return nil
	}
	return s.Client.Get(ctx, key, obj, opts...)
}

// A reconcile that reads a stale cached CR must not re-emit a transition
// Event the previous pass already emitted: Reconcile reads the CR via the
// uncached APIReader so transition guards see the latest status.
func TestTransitionEvent_NotReemittedOnStaleCache(t *testing.T) {
	cr := minimalCR(testCRName, testNamespace)
	controllerutil.AddFinalizer(cr, finalizerName)
	cr.Annotations = map[string]string{pauseAnnotation: annotationTrue}

	c := fake.NewClientBuilder().
		WithScheme(ownershipScheme(t)).
		WithObjects(cr).
		WithStatusSubresource(cr).
		Build()
	req := reconcile.Request{NamespacedName: types.NamespacedName{Name: testCRName, Namespace: testNamespace}}

	stale := &costv1alpha1.CostManagementServiceConfig{}
	if err := c.Get(context.Background(), req.NamespacedName, stale); err != nil {
		t.Fatalf("snapshot: %v", err)
	}

	rec := record.NewFakeRecorder(10)
	r := &CostManagementServiceConfigReconciler{Client: c, APIReader: c, Scheme: ownershipScheme(t), Recorder: rec}
	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("pass 1: %v", err)
	}
	assertEvent(t, rec, "Paused")

	// Cache still holds the pre-pause status; the API server has Paused=True.
	r.Client = &staleCMSCClient{Client: c, stale: stale}
	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("pass 2: %v", err)
	}
	assertNoEvent(t, rec, "Paused")
}
