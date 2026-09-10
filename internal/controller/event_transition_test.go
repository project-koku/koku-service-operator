package controller

import (
	"context"
	"strings"
	"testing"

	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	costv1alpha1 "github.com/project-koku/koku-service-operator/api/v1alpha1"
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

func TestCoreServicesAvailableEvent_GuardLogic(t *testing.T) {
	rec := record.NewFakeRecorder(10)
	r := &CostManagementServiceConfigReconciler{Recorder: rec}
	cfg := &costv1alpha1.CostManagementServiceConfig{
		ObjectMeta: metav1.ObjectMeta{Name: testCRName, Namespace: testNamespace},
	}

	// Simulate first transition: Available not yet True → Event fires.
	if !apimeta.IsStatusConditionTrue(cfg.Status.Conditions, costv1alpha1.ConditionAvailable) {
		r.Recorder.Event(cfg, "Normal", "CoreServicesAvailable", "Koku API is ready")
	}
	r.setCondition(cfg, costv1alpha1.ConditionAvailable, metav1.ConditionTrue, reasonKokuAvailable, "")
	assertEvent(t, rec, "CoreServicesAvailable")

	// Simulate second pass: Available already True → no Event.
	if !apimeta.IsStatusConditionTrue(cfg.Status.Conditions, costv1alpha1.ConditionAvailable) {
		r.Recorder.Event(cfg, "Normal", "CoreServicesAvailable", "Koku API is ready")
	}
	assertNoEvent(t, rec, "CoreServicesAvailable")
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
