package controller

import (
	"context"
	"net"
	"testing"

	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	costv1alpha1 "github.com/project-koku/koku-service-operator/api/v1alpha1"
	"github.com/project-koku/koku-service-operator/internal/resources"
)

// listenLocal returns a reachable TCP port on localHost for TCP probes.
func listenLocal(t *testing.T) int32 {
	t.Helper()
	ln, err := net.Listen("tcp", net.JoinHostPort(localHost, "0"))
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	return int32(ln.Addr().(*net.TCPAddr).Port)
}

// byoiCfg builds a fully external (BYOI) CR with the given DB / cache ports on
// localHost. Port 1 is reserved, so probes to it fail.
func byoiCfg(dbPort, cachePort int32) *costv1alpha1.CostManagementServiceConfig {
	return &costv1alpha1.CostManagementServiceConfig{
		ObjectMeta: metav1.ObjectMeta{Name: testCRName, Namespace: testNamespace},
		Spec: costv1alpha1.CostManagementServiceConfigSpec{
			Database: costv1alpha1.DatabaseConfig{Deploy: falsePtr(), Host: localHost, Port: dbPort},
			Cache:    costv1alpha1.CacheConfig{Deploy: falsePtr(), Host: localHost, Port: cachePort},
		},
	}
}

func newInfraEventsReconciler(t *testing.T) *CostManagementServiceConfigReconciler {
	t.Helper()
	scheme := testScheme(t)
	return &CostManagementServiceConfigReconciler{
		Client:   fake.NewClientBuilder().WithScheme(scheme).Build(),
		Scheme:   scheme,
		Recorder: record.NewFakeRecorder(100),
	}
}

// infraPass runs the infrastructure and validation stages exactly as phases()
// wires them for one reconcile pass.
func infraPass(t *testing.T, r *CostManagementServiceConfigReconciler, cfg *costv1alpha1.CostManagementServiceConfig) {
	t.Helper()
	for i, phase := range r.infrastructurePhases(context.Background(), cfg) {
		if _, err := phase(); err != nil {
			t.Fatalf("infrastructure stage %d: %v", i, err)
		}
	}
}

func assertConditionStable(t *testing.T, cfg *costv1alpha1.CostManagementServiceConfig, condType string, status metav1.ConditionStatus, reason string, since metav1.Time) {
	t.Helper()
	c := apimeta.FindStatusCondition(cfg.Status.Conditions, condType)
	if c == nil || c.Status != status || c.Reason != reason || !c.LastTransitionTime.Equal(&since) {
		t.Errorf("expected stable %s=%s %s since %s, got %+v", condType, status, reason, since, c)
	}
}

// An unreachable external database or cache must not produce an Event storm:
// the infrastructure stage used to claim DatabaseReady/CacheReady=True
// (ExternalDatabase/ExternalCache) on every pass and validation flipped it back
// to False, defeating both the InfrastructureReady and DependencyFailed
// transition guards.
func TestExternalDependencyOutage_NoEventStorm(t *testing.T) {
	cases := []struct {
		name      string
		condType  string
		reason    string
		dbPort    func(*testing.T) int32
		cachePort func(*testing.T) int32
	}{
		{"database", costv1alpha1.ConditionDatabaseReady, reasonDatabaseUnreachable,
			func(*testing.T) int32 { return 1 }, listenLocal},
		{"cache", costv1alpha1.ConditionCacheReady, reasonCacheUnreachable,
			listenLocal, func(*testing.T) int32 { return 1 }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := newInfraEventsReconciler(t)
			cfg := byoiCfg(tc.dbPort(t), tc.cachePort(t))

			infraPass(t, r, cfg)
			since := apimeta.FindStatusCondition(cfg.Status.Conditions, tc.condType).LastTransitionTime
			infraPass(t, r, cfg)
			infraPass(t, r, cfg)

			events := drainEvents(r)
			if got := countEvents(events, "DependencyFailed"); got != 1 {
				t.Errorf("DependencyFailed events = %d, want 1 (outage start only): %v", got, events)
			}
			if got := countEvents(events, "InfrastructureReady"); got != 0 {
				t.Errorf("InfrastructureReady events = %d, want 0 during the outage: %v", got, events)
			}
			assertConditionStable(t, cfg, tc.condType, metav1.ConditionFalse, tc.reason, since)
		})
	}
}

// Recovering external dependencies emit InfrastructureReady exactly once, and
// a healthy steady state emits nothing further.
func TestExternalDependencyRecovery_InfrastructureReadyOnce(t *testing.T) {
	r := newInfraEventsReconciler(t)
	cachePort := listenLocal(t)
	cfg := byoiCfg(1, cachePort)

	infraPass(t, r, cfg)
	drainEvents(r)

	cfg.Spec.Database.Port = listenLocal(t)
	infraPass(t, r, cfg)
	infraPass(t, r, cfg)

	events := drainEvents(r)
	if got := countEvents(events, "InfrastructureReady"); got != 1 {
		t.Errorf("InfrastructureReady events = %d, want 1 on recovery: %v", got, events)
	}
	if got := countEvents(events, "DependencyFailed"); got != 0 {
		t.Errorf("DependencyFailed events = %d, want 0 after recovery: %v", got, events)
	}
	db := apimeta.FindStatusCondition(cfg.Status.Conditions, costv1alpha1.ConditionDatabaseReady)
	if db == nil || db.Status != metav1.ConditionTrue || db.Reason != "DatabaseReachable" {
		t.Errorf("expected DatabaseReady=True DatabaseReachable, got %+v", db)
	}
}

// The infrastructure stage no longer writes conditions for external
// dependencies; only validation (which probes) does.
func TestReconcileInfrastructure_ExternalLeavesConditionsToValidation(t *testing.T) {
	r := newInfraEventsReconciler(t)
	cfg := byoiCfg(1, 1)

	result, err := r.reconcileInfrastructure(context.Background(), cfg)
	if err != nil {
		t.Fatalf("reconcileInfrastructure: %v", err)
	}
	if !result.IsZero() {
		t.Fatalf("expected zero result for external infra, got %+v", result)
	}
	for _, typ := range []string{costv1alpha1.ConditionDatabaseReady, costv1alpha1.ConditionCacheReady} {
		if c := apimeta.FindStatusCondition(cfg.Status.Conditions, typ); c != nil {
			t.Errorf("reconcileInfrastructure wrote %s for an external dependency: %+v", typ, c)
		}
	}
	if events := drainEvents(r); len(events) != 0 {
		t.Errorf("reconcileInfrastructure emitted events: %v", events)
	}
}

// A validation-blocked pass keeps the pipeline stopped and emits no
// InfrastructureReady even when the stage before it succeeded.
func TestInfrastructurePhases_BlockedValidationStopsPipeline(t *testing.T) {
	r := newInfraEventsReconciler(t)
	cfg := byoiCfg(1, listenLocal(t))

	phases := r.infrastructurePhases(context.Background(), cfg)
	if len(phases) != 2 {
		t.Fatalf("expected infrastructure and validation stages, got %d", len(phases))
	}
	result, err := runPhases(phases)
	if err != nil {
		t.Fatalf("runPhases: %v", err)
	}
	if result.RequeueAfter != requeueSlow {
		t.Errorf("expected validation to block with requeueSlow, got %+v", result)
	}
	if got := countEvents(drainEvents(r), "InfrastructureReady"); got != 0 {
		t.Errorf("InfrastructureReady events = %d, want 0 while blocked", got)
	}
}

// Bundled PostgreSQL and Valkey keep the once-per-transition InfrastructureReady
// Event: none while Valkey is still starting, one when both are ready, none on
// the following steady-state pass.
func TestInfrastructurePhases_BundledInfraReadyOnce(t *testing.T) {
	scheme := ownershipScheme(t)
	cfg := minimalCR(testCRName, testNamespace)
	cfg.Spec.Database.Deploy = truePtr()
	cfg.Spec.Cache.Deploy = truePtr()

	db := resources.DatabaseStatefulSet(cfg)
	replicas := int32(1)
	db.Spec.Replicas = &replicas
	c := fakeClientPreservingStatus(scheme, db)
	markStatefulSetReady(t, c, testNamespace, resources.NameDatabase(cfg))
	r := &CostManagementServiceConfigReconciler{Client: c, Scheme: scheme, Recorder: record.NewFakeRecorder(100)}

	// Pass 1: Valkey Deployment created but not ready — pipeline waits.
	if _, err := runPhases(r.infrastructurePhases(context.Background(), cfg)); err != nil {
		t.Fatalf("pass 1: %v", err)
	}
	if got := countEvents(drainEvents(r), "InfrastructureReady"); got != 0 {
		t.Fatalf("pass 1: InfrastructureReady events = %d, want 0 while Valkey starts", got)
	}

	markDeploymentReady(t, c, testNamespace, resources.NameValkey(cfg))
	for pass, want := range []int{1, 0} {
		if _, err := runPhases(r.infrastructurePhases(context.Background(), cfg)); err != nil {
			t.Fatalf("pass %d: %v", pass+2, err)
		}
		if got := countEvents(drainEvents(r), "InfrastructureReady"); got != want {
			t.Fatalf("pass %d: InfrastructureReady events = %d, want %d", pass+2, got, want)
		}
	}
}
