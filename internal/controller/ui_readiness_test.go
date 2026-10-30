package controller

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"

	costv1alpha1 "github.com/project-koku/koku-service-operator/api/v1alpha1"
	"github.com/project-koku/koku-service-operator/internal/resources"
)

// uiReadyFixture returns a reconciler whose UI OAuth Secret is valid and whose
// UI Route is already admitted, so UI Deployment readiness is the only gate
// left in reconcileUI.
func uiReadyFixture(t *testing.T) (*CostManagementServiceConfigReconciler, *costv1alpha1.CostManagementServiceConfig, client.Client) {
	t.Helper()
	scheme := ownershipScheme(t)
	cfg := minimalCR(testCRName, testNamespace)
	cfg.Status.DiscoveredConfig = &costv1alpha1.DiscoveredConfig{ClusterDomain: "apps.example.com"}

	oauth := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: resources.NameUIOAuthClientSecret(cfg), Namespace: testNamespace},
		Data: map[string][]byte{
			"client-id":     []byte("cost-ui"),
			"client-secret": []byte("s3cret"),
		},
	}
	uiRoute := resources.UIRoute(cfg)
	if uiRoute == nil {
		t.Fatal("expected UI Route")
	}
	if err := unstructured.SetNestedSlice(uiRoute.Object, []any{
		map[string]any{
			"host":       "ui.apps.example.com",
			"conditions": []any{map[string]any{"type": routeAdmittedType, "status": "True"}},
		},
	}, "status", "ingress"); err != nil {
		t.Fatalf("seed UI Route status: %v", err)
	}

	c := fakeClientPreservingStatus(scheme, oauth, uiRoute)
	r := &CostManagementServiceConfigReconciler{Client: c, Scheme: scheme, Recorder: &noopRecorder{}}
	return r, cfg, c
}

// A valid OAuth Secret is not enough: UIReady and Available wait for the UI
// Deployment (oauth2-proxy + app) to be ready.
func TestReconcileUI_DeploymentNotReady_BlocksUIReady(t *testing.T) {
	r, cfg, _ := uiReadyFixture(t)

	result, err := r.reconcileUI(context.Background(), cfg)
	if err != nil {
		t.Fatalf("reconcileUI: %v", err)
	}
	if result.RequeueAfter == 0 {
		t.Fatal("expected requeue while the UI Deployment is not ready")
	}
	ui := findCondition(cfg.Status.Conditions, costv1alpha1.ConditionUIReady)
	if ui == nil || ui.Status != metav1.ConditionFalse || ui.Reason != reasonWaitingForUI {
		t.Fatalf("expected UIReady=False %s, got %+v", reasonWaitingForUI, ui)
	}
	avail := findCondition(cfg.Status.Conditions, costv1alpha1.ConditionAvailable)
	if avail == nil || avail.Status != metav1.ConditionFalse || avail.Reason != reasonWaitingForUI {
		t.Fatalf("expected Available=False %s, got %+v", reasonWaitingForUI, avail)
	}
}

func TestReconcileUI_DeploymentReady_SetsUIReady(t *testing.T) {
	r, cfg, c := uiReadyFixture(t)

	if _, err := r.reconcileUI(context.Background(), cfg); err != nil {
		t.Fatalf("first pass: %v", err)
	}
	markDeploymentReady(t, c, testNamespace, resources.NameUI(cfg))

	result, err := r.reconcileUI(context.Background(), cfg)
	if err != nil {
		t.Fatalf("second pass: %v", err)
	}
	if !result.IsZero() {
		t.Fatalf("expected zero Result once the UI Deployment is ready, got %+v", result)
	}
	ui := findCondition(cfg.Status.Conditions, costv1alpha1.ConditionUIReady)
	if ui == nil || ui.Status != metav1.ConditionTrue || ui.Reason != reasonUIAvailable {
		t.Fatalf("expected UIReady=True %s, got %+v", reasonUIAvailable, ui)
	}
}

// Cluster-observed misreport: after Ready, the UI pod crash-looped (oauth2-proxy
// could not verify Keycloak TLS) while the CR kept UIReady=True and
// Available=True AllComponentsReady. A drift pass must flip both to False.
func TestReconcileUI_DriftPass_UICrash_ClearsAvailable(t *testing.T) {
	r, cfg, c := uiReadyFixture(t)

	if _, err := r.reconcileUI(context.Background(), cfg); err != nil {
		t.Fatalf("first pass: %v", err)
	}
	markDeploymentReady(t, c, testNamespace, resources.NameUI(cfg))
	if _, err := r.reconcileUI(context.Background(), cfg); err != nil {
		t.Fatalf("ready pass: %v", err)
	}
	r.markReady(cfg, costv1alpha1.PhaseProgressing, false)

	markDeploymentNotReady(t, c, testNamespace, resources.NameUI(cfg))
	result, err := r.reconcileUI(context.Background(), cfg)
	if err != nil {
		t.Fatalf("drift pass: %v", err)
	}
	if result.RequeueAfter == 0 {
		t.Fatal("expected requeue while the UI Deployment is down")
	}
	if apimeta.IsStatusConditionTrue(cfg.Status.Conditions, costv1alpha1.ConditionUIReady) {
		t.Fatal("UIReady must not stay True while the UI Deployment is down")
	}
	avail := findCondition(cfg.Status.Conditions, costv1alpha1.ConditionAvailable)
	if avail == nil || avail.Status != metav1.ConditionFalse || avail.Reason != reasonWaitingForUI {
		t.Fatalf("expected Available=False %s, got %+v", reasonWaitingForUI, avail)
	}
}

// A UI wait older than readinessTimeout must survive the core-services pass
// that runs before Edge (no clock reset) and degrade naming the UI.
func TestReconcileUI_Timeout_DegradesAcrossCorePass(t *testing.T) {
	r, cfg, c := uiReadyFixture(t)

	// Core services and the UI Deployment exist; core is ready, UI is not.
	if _, err := r.reconcileCoreServices(context.Background(), cfg); err != nil {
		t.Fatalf("core first pass: %v", err)
	}
	for _, name := range []string{
		resources.NameRBACAPI(cfg), resources.NameRBACWorker(cfg), resources.NameKokuAPI(cfg),
		resources.NameMasu(cfg), resources.NameListener(cfg),
	} {
		markDeploymentReady(t, c, testNamespace, name)
	}
	if _, err := r.reconcileUI(context.Background(), cfg); err != nil {
		t.Fatalf("ui first pass: %v", err)
	}

	// The UI has been waiting for 6 minutes.
	started := metav1.NewTime(time.Now().Add(-6 * time.Minute).Truncate(time.Second))
	apimeta.RemoveStatusCondition(&cfg.Status.Conditions, costv1alpha1.ConditionAvailable)
	apimeta.SetStatusCondition(&cfg.Status.Conditions, metav1.Condition{
		Type: costv1alpha1.ConditionAvailable, Status: metav1.ConditionFalse,
		Reason: reasonWaitingForUI, Message: msgWaitingForUI, LastTransitionTime: started,
	})

	// Next full pass: core first, then Edge/UI.
	if _, err := r.reconcileCoreServices(context.Background(), cfg); err != nil {
		t.Fatalf("core pass: %v", err)
	}
	if _, err := r.reconcileUI(context.Background(), cfg); err != nil {
		t.Fatalf("ui pass: %v", err)
	}

	avail := findCondition(cfg.Status.Conditions, costv1alpha1.ConditionAvailable)
	if avail == nil || avail.Reason != reasonWaitingForUI || !avail.LastTransitionTime.Equal(&started) {
		t.Fatalf("expected Available=False %s with the original wait clock %s, got %+v", reasonWaitingForUI, started, avail)
	}
	deg := findCondition(cfg.Status.Conditions, costv1alpha1.ConditionDegraded)
	if deg == nil || deg.Status != metav1.ConditionTrue || deg.Reason != reasonDeploymentNotReady || deg.Message != "UI is not ready" {
		t.Fatalf("expected Degraded=True %s naming the UI, got %+v", reasonDeploymentNotReady, deg)
	}
}

// Once the UI wait has degraded, a later core pass must not clear Degraded
// only for the UI wait to set it again: Degraded's LastTransitionTime stays
// stable across passes while the UI is still down.
func TestReconcileUI_Degraded_StableAcrossCorePass(t *testing.T) {
	r, cfg, c := uiReadyFixture(t)

	if _, err := r.reconcileCoreServices(context.Background(), cfg); err != nil {
		t.Fatalf("core first pass: %v", err)
	}
	for _, name := range []string{
		resources.NameRBACAPI(cfg), resources.NameRBACWorker(cfg), resources.NameKokuAPI(cfg),
		resources.NameMasu(cfg), resources.NameListener(cfg),
	} {
		markDeploymentReady(t, c, testNamespace, name)
	}
	if _, err := r.reconcileUI(context.Background(), cfg); err != nil {
		t.Fatalf("ui first pass: %v", err)
	}
	apimeta.RemoveStatusCondition(&cfg.Status.Conditions, costv1alpha1.ConditionAvailable)
	apimeta.SetStatusCondition(&cfg.Status.Conditions, metav1.Condition{
		Type: costv1alpha1.ConditionAvailable, Status: metav1.ConditionFalse,
		Reason: reasonWaitingForUI, Message: msgWaitingForUI,
		LastTransitionTime: metav1.NewTime(time.Now().Add(-6 * time.Minute)),
	})
	degradedSince := metav1.NewTime(time.Now().Add(-time.Minute).Truncate(time.Second))
	apimeta.SetStatusCondition(&cfg.Status.Conditions, metav1.Condition{
		Type: costv1alpha1.ConditionDegraded, Status: metav1.ConditionTrue,
		Reason: reasonDeploymentNotReady, Message: "UI is not ready", LastTransitionTime: degradedSince,
	})

	if _, err := r.reconcileCoreServices(context.Background(), cfg); err != nil {
		t.Fatalf("core pass: %v", err)
	}
	if _, err := r.reconcileUI(context.Background(), cfg); err != nil {
		t.Fatalf("ui pass: %v", err)
	}

	deg := findCondition(cfg.Status.Conditions, costv1alpha1.ConditionDegraded)
	if deg == nil || deg.Status != metav1.ConditionTrue || !deg.LastTransitionTime.Equal(&degradedSince) {
		t.Fatalf("expected Degraded=True with stable LastTransitionTime %s, got %+v", degradedSince, deg)
	}
}

// Same as above, but over the real phase order of a pass: the migration
// stage (already complete) runs first and must not clear a later wait's
// DeploymentNotReady either.
func TestReconcileUI_Degraded_StableAcrossMigrationPass(t *testing.T) {
	r, cfg, c := uiReadyFixture(t)
	// Same spec as newMigrationTestReconciler so the migration stage runs Jobs.
	cfg.Spec.Database.Deploy = new(true)
	cfg.Spec.Cache.Deploy = new(true)
	cfg.Spec.CostManagement.API.Image = costv1alpha1.ImageSpec{Repository: "quay.io/test/koku", Tag: "v1"}
	cfg.Spec.RBAC.Image = costv1alpha1.ImageSpec{Repository: "quay.io/test/rbac", Tag: "v1"}

	for _, job := range []string{resources.NameKokuMigration(cfg), resources.NameRBACMigration(cfg)} {
		if _, err := r.reconcileMigration(context.Background(), cfg); err != nil {
			t.Fatalf("migration %s: %v", job, err)
		}
		markJobComplete(t, c, testNamespace, job)
	}
	if _, err := r.reconcileMigration(context.Background(), cfg); err != nil {
		t.Fatalf("migration complete: %v", err)
	}
	if _, err := r.reconcileCoreServices(context.Background(), cfg); err != nil {
		t.Fatalf("core first pass: %v", err)
	}
	for _, name := range []string{
		resources.NameRBACAPI(cfg), resources.NameRBACWorker(cfg), resources.NameKokuAPI(cfg),
		resources.NameMasu(cfg), resources.NameListener(cfg),
	} {
		markDeploymentReady(t, c, testNamespace, name)
	}
	if _, err := r.reconcileUI(context.Background(), cfg); err != nil {
		t.Fatalf("ui first pass: %v", err)
	}
	apimeta.RemoveStatusCondition(&cfg.Status.Conditions, costv1alpha1.ConditionAvailable)
	apimeta.SetStatusCondition(&cfg.Status.Conditions, metav1.Condition{
		Type: costv1alpha1.ConditionAvailable, Status: metav1.ConditionFalse,
		Reason: reasonWaitingForUI, Message: msgWaitingForUI,
		LastTransitionTime: metav1.NewTime(time.Now().Add(-6 * time.Minute)),
	})
	degradedSince := metav1.NewTime(time.Now().Add(-time.Minute).Truncate(time.Second))
	apimeta.SetStatusCondition(&cfg.Status.Conditions, metav1.Condition{
		Type: costv1alpha1.ConditionDegraded, Status: metav1.ConditionTrue,
		Reason: reasonDeploymentNotReady, Message: "UI is not ready", LastTransitionTime: degradedSince,
	})

	// One full pass in pipeline order: migration → core → Edge/UI.
	if _, err := r.reconcileMigration(context.Background(), cfg); err != nil {
		t.Fatalf("migration pass: %v", err)
	}
	if _, err := r.reconcileCoreServices(context.Background(), cfg); err != nil {
		t.Fatalf("core pass: %v", err)
	}
	if _, err := r.reconcileUI(context.Background(), cfg); err != nil {
		t.Fatalf("ui pass: %v", err)
	}

	deg := findCondition(cfg.Status.Conditions, costv1alpha1.ConditionDegraded)
	if deg == nil || deg.Status != metav1.ConditionTrue || !deg.LastTransitionTime.Equal(&degradedSince) {
		t.Fatalf("expected Degraded=True with stable LastTransitionTime %s, got %+v", degradedSince, deg)
	}
}
