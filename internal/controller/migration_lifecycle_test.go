package controller

import (
	"context"
	"maps"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"

	costv1alpha1 "github.com/project-koku/koku-service-operator/api/v1alpha1"
	"github.com/project-koku/koku-service-operator/internal/resources"
)

func TestReconcileMigration_EmptyKokuImage_DegradedNoJob(t *testing.T) {
	r, cfg, c := newMigrationTestReconciler(t)
	cfg.Spec.CostManagement.API.Image = costv1alpha1.ImageSpec{}

	result, err := r.reconcileMigration(context.Background(), cfg)
	if err != nil {
		t.Fatalf("reconcileMigration: %v", err)
	}
	if !result.Stop {
		t.Fatal("expected Stop=true when API image is unset")
	}
	if jobExists(c, testNamespace, resources.NameKokuMigration(cfg)) {
		t.Fatal("must not create a Koku migration Job without spec.costManagement.api.image")
	}
	assertMigrationBlockedStatus(t, cfg, "ImageNotSet")
}

// ImageNotSet must flip a prior Ready status: Available/Progressing cannot
// stay True while Phase is Degraded (review on PR #109).
func TestReconcileMigration_ImageNotSet_ClearsStaleAvailable(t *testing.T) {
	r, cfg, c := newMigrationTestReconciler(t)
	r.setCondition(cfg, costv1alpha1.ConditionAvailable, metav1.ConditionTrue, "AllComponentsReady", "All components are running")
	r.setCondition(cfg, costv1alpha1.ConditionProgressing, metav1.ConditionFalse, "ReconcileComplete", "")
	r.setCondition(cfg, costv1alpha1.ConditionDegraded, metav1.ConditionFalse, "ReconcileComplete", "")
	r.setCondition(cfg, costv1alpha1.ConditionSchemaUpToDate, metav1.ConditionTrue, "MigrationComplete", "")
	cfg.Status.Phase = costv1alpha1.PhaseReady
	cfg.Spec.CostManagement.API.Image = costv1alpha1.ImageSpec{}

	result, err := r.reconcileMigration(context.Background(), cfg)
	if err != nil {
		t.Fatalf("reconcileMigration: %v", err)
	}
	if !result.Stop {
		t.Fatal("expected Stop=true when API image is unset")
	}
	if jobExists(c, testNamespace, resources.NameKokuMigration(cfg)) {
		t.Fatal("must not create a Koku migration Job after ImageNotSet")
	}
	assertMigrationBlockedStatus(t, cfg, "ImageNotSet")
}

func TestReconcileMigration_EmptyRBACImage_DegradedNoJob(t *testing.T) {
	r, cfg, c := newMigrationTestReconciler(t)
	cfg.Spec.RBAC.Image = costv1alpha1.ImageSpec{}

	result, err := r.reconcileMigration(context.Background(), cfg)
	if err != nil {
		t.Fatalf("reconcileMigration: %v", err)
	}
	if !result.Stop {
		t.Fatal("expected Stop=true when RBAC image is unset")
	}
	if jobExists(c, testNamespace, resources.NameKokuMigration(cfg)) {
		t.Fatal("must not create any migration Job when a required image is unset")
	}
	assertMigrationBlockedStatus(t, cfg, "ImageNotSet")
}

func TestReconcileMigration_ROSEnabledEmptyImage_DegradedNoJob(t *testing.T) {
	r, cfg, c := newMigrationTestReconciler(t)
	cfg.Spec.ROS.Enabled = boolPtr(true)
	cfg.Spec.ROS.Image = costv1alpha1.ImageSpec{}

	result, err := r.reconcileMigration(context.Background(), cfg)
	if err != nil {
		t.Fatalf("reconcileMigration: %v", err)
	}
	if !result.Stop {
		t.Fatal("expected Stop=true when ROS is enabled without an image")
	}
	if jobExists(c, testNamespace, resources.NameKokuMigration(cfg)) || jobExists(c, testNamespace, resources.NameROSMigration(cfg)) {
		t.Fatal("must not create migration Jobs when ROS image is unset")
	}
	assertMigrationBlockedStatus(t, cfg, "ImageNotSet")
}

func TestReconcileMigration_FirstReconcileCreatesKokuJob(t *testing.T) {
	r, cfg, c := newMigrationTestReconciler(t)

	result, err := r.reconcileMigration(context.Background(), cfg)
	if err != nil {
		t.Fatalf("reconcileMigration: %v", err)
	}
	if result.RequeueAfter == 0 {
		t.Fatal("expected RequeueAfter while Job is running")
	}

	kokuJobName := resources.NameKokuMigration(cfg)
	if !jobExists(c, testNamespace, kokuJobName) {
		t.Fatalf("expected Koku migration Job %q to exist", kokuJobName)
	}
	if getJobAnnotation(t, c, testNamespace, kokuJobName, resources.MigrationImageTagAnnotation) != cfg.Spec.CostManagement.API.Image.Tag {
		t.Errorf("Koku Job missing image-tag annotation")
	}

	// RBAC Job should NOT exist yet (sequential: always after Koku)
	// ROS Job should NOT exist yet (defaults to disabled via spec.ros.enabled=false)
	if jobExists(c, testNamespace, resources.NameRBACMigration(cfg)) {
		t.Fatal("expected RBAC Job to NOT exist on first pass (sequential)")
	}
	if jobExists(c, testNamespace, resources.NameROSMigration(cfg)) {
		t.Fatal("expected ROS Job to NOT exist on first pass (sequential)")
	}

	cond := findCondition(cfg.Status.Conditions, costv1alpha1.ConditionSchemaUpToDate)
	if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != "MigrationRunning" {
		t.Fatalf("expected SchemaUpToDate=False MigrationRunning, got %+v", cond)
	}
}

func TestReconcileMigration_KokuComplete_CreatesRBACJob(t *testing.T) {
	r, cfg, c := newMigrationTestReconciler(t)
	cfg.Spec.RBAC.Image.Tag = "rbac-tag"

	if _, err := r.reconcileMigration(context.Background(), cfg); err != nil {
		t.Fatalf("first pass: %v", err)
	}
	kokuJobName := resources.NameKokuMigration(cfg)
	markJobComplete(t, c, testNamespace, kokuJobName)

	result, err := r.reconcileMigration(context.Background(), cfg)
	if err != nil {
		t.Fatalf("second pass: %v", err)
	}
	if result.RequeueAfter == 0 {
		t.Fatal("expected RequeueAfter while RBAC Job is running")
	}

	rbacJobName := resources.NameRBACMigration(cfg)
	if !jobExists(c, testNamespace, rbacJobName) {
		t.Fatalf("expected RBAC migration Job %q to exist", rbacJobName)
	}
	if getJobAnnotation(t, c, testNamespace, rbacJobName, resources.MigrationImageTagAnnotation) != "rbac-tag-cmseed1" {
		t.Errorf("RBAC Job image-tag should include cmseed1 suffix")
	}

	cond := findCondition(cfg.Status.Conditions, costv1alpha1.ConditionSchemaUpToDate)
	if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != "MigrationRunning" {
		t.Fatalf("expected SchemaUpToDate=False MigrationRunning (RBAC), got %+v", cond)
	}
}

func TestReconcileMigration_AllComplete_SchemaUpToDateTrue(t *testing.T) {
	r, cfg, c := newMigrationTestReconciler(t)

	steps := []string{
		resources.NameKokuMigration(cfg),
		resources.NameRBACMigration(cfg),
	}
	for _, jobName := range steps {
		if _, err := r.reconcileMigration(context.Background(), cfg); err != nil {
			t.Fatalf("step for %s: %v", jobName, err)
		}
		markJobComplete(t, c, testNamespace, jobName)
	}

	result, err := r.reconcileMigration(context.Background(), cfg)
	if err != nil {
		t.Fatalf("final pass: %v", err)
	}
	if !result.IsZero() {
		t.Fatalf("expected zero Result when all migrations complete, got %+v", result)
	}

	cond := findCondition(cfg.Status.Conditions, costv1alpha1.ConditionSchemaUpToDate)
	if cond == nil || cond.Status != metav1.ConditionTrue || cond.Reason != "MigrationComplete" {
		t.Fatalf("expected SchemaUpToDate=True MigrationComplete, got %+v", cond)
	}

	// Degraded should be cleared on success (controller.go:411 sets Degraded=False MigrationSucceeded)
	degraded := findCondition(cfg.Status.Conditions, costv1alpha1.ConditionDegraded)
	if degraded == nil || degraded.Status != metav1.ConditionFalse || degraded.Reason != "MigrationSucceeded" {
		t.Fatalf("expected Degraded=False MigrationSucceeded, got %+v", degraded)
	}

	// Succeeded Jobs must not be re-created when image tags are unchanged.
	uids := jobUIDs(c, testNamespace)
	if len(uids) != 2 {
		t.Fatalf("expected 2 Jobs after complete, got %d", len(uids))
	}
	result, err = r.reconcileMigration(context.Background(), cfg)
	if err != nil {
		t.Fatalf("idempotent pass: %v", err)
	}
	if !result.IsZero() {
		t.Fatalf("expected zero Result on re-reconcile of succeeded Jobs, got %+v", result)
	}
	cond = findCondition(cfg.Status.Conditions, costv1alpha1.ConditionSchemaUpToDate)
	if cond == nil || cond.Status != metav1.ConditionTrue || cond.Reason != "MigrationComplete" {
		t.Fatalf("expected SchemaUpToDate to stay True, got %+v", cond)
	}
	if !maps.Equal(uids, jobUIDs(c, testNamespace)) {
		t.Fatal("succeeded Jobs were recreated on re-reconcile")
	}
}

func TestReconcileMigration_JobFailed_DegradedAndStop(t *testing.T) {
	r, cfg, c := newMigrationTestReconciler(t)

	if _, err := r.reconcileMigration(context.Background(), cfg); err != nil {
		t.Fatalf("create: %v", err)
	}
	kokuJobName := resources.NameKokuMigration(cfg)
	markJobFailed(t, c, testNamespace, kokuJobName)

	result, err := r.reconcileMigration(context.Background(), cfg)
	if err != nil {
		t.Fatalf("after failure: %v", err)
	}
	if !result.Stop {
		t.Fatal("expected Stop=true when Job fails")
	}

	// RBAC Job should NOT have been created (pipeline stops on failure)
	if jobExists(c, testNamespace, resources.NameRBACMigration(cfg)) {
		t.Fatal("expected RBAC Job to NOT exist after Koku failure (pipeline stops)")
	}
	assertMigrationBlockedStatus(t, cfg, "MigrationFailed")
}

// MigrationFailed must flip a prior Ready status: Available cannot stay True
// while schema migrate failed and core services were not rolled.
func TestReconcileMigration_MigrationFailed_ClearsStaleAvailable(t *testing.T) {
	r, cfg, c := newMigrationTestReconciler(t)
	r.setCondition(cfg, costv1alpha1.ConditionAvailable, metav1.ConditionTrue, "AllComponentsReady", "All components are running")
	r.setCondition(cfg, costv1alpha1.ConditionProgressing, metav1.ConditionFalse, "ReconcileComplete", "")
	r.setCondition(cfg, costv1alpha1.ConditionDegraded, metav1.ConditionFalse, "ReconcileComplete", "")
	r.setCondition(cfg, costv1alpha1.ConditionSchemaUpToDate, metav1.ConditionTrue, "MigrationComplete", "")
	cfg.Status.Phase = costv1alpha1.PhaseReady

	if _, err := r.reconcileMigration(context.Background(), cfg); err != nil {
		t.Fatalf("create: %v", err)
	}
	markJobFailed(t, c, testNamespace, resources.NameKokuMigration(cfg))

	result, err := r.reconcileMigration(context.Background(), cfg)
	if err != nil {
		t.Fatalf("after failure: %v", err)
	}
	if !result.Stop {
		t.Fatal("expected Stop=true when Job fails")
	}
	assertMigrationBlockedStatus(t, cfg, "MigrationFailed")
}

func TestReconcileMigration_ImageTagChange_RecreatesJob(t *testing.T) {
	r, cfg, c := newMigrationTestReconciler(t)
	cfg.Spec.CostManagement.API.Image.Tag = "v1"

	if _, err := r.reconcileMigration(context.Background(), cfg); err != nil {
		t.Fatalf("v1 create: %v", err)
	}
	kokuJobName := resources.NameKokuMigration(cfg)
	markJobComplete(t, c, testNamespace, kokuJobName)

	cfg.Spec.CostManagement.API.Image.Tag = "v2"
	result, err := r.reconcileMigration(context.Background(), cfg)
	if err != nil {
		t.Fatalf("v2 reconcile (delete): %v", err)
	}

	// Old Job should be deleted
	if jobExists(c, testNamespace, kokuJobName) {
		t.Fatal("expected old Job to be deleted on image tag change")
	}
	if result.RequeueAfter == 0 {
		t.Fatal("expected RequeueAfter after deleting stale Job")
	}

	// Next reconcile should create new Job with v2 tag
	result, err = r.reconcileMigration(context.Background(), cfg)
	if err != nil {
		t.Fatalf("v2 reconcile (recreate): %v", err)
	}
	if !jobExists(c, testNamespace, kokuJobName) {
		t.Fatal("expected new Job to be created on requeue")
	}
	newAnnotation := getJobAnnotation(t, c, testNamespace, kokuJobName, resources.MigrationImageTagAnnotation)
	if newAnnotation != "v2" {
		t.Errorf("expected Job recreated with v2 annotation, got %q", newAnnotation)
	}
	if result.RequeueAfter == 0 {
		t.Fatal("expected RequeueAfter for new Job")
	}
}

func TestReconcileMigration_ROSDisabled_SkipsROSMigration(t *testing.T) {
	r, cfg, c := newMigrationTestReconciler(t)
	cfg.Spec.ROS.Enabled = boolPtr(false)

	if _, err := r.reconcileMigration(context.Background(), cfg); err != nil {
		t.Fatalf("koku: %v", err)
	}
	markJobComplete(t, c, testNamespace, resources.NameKokuMigration(cfg))

	if _, err := r.reconcileMigration(context.Background(), cfg); err != nil {
		t.Fatalf("rbac: %v", err)
	}

	if jobExists(c, testNamespace, resources.NameROSMigration(cfg)) {
		t.Fatal("expected no ROS MigrationJob when ROS disabled")
	}

	if countJobs(c, testNamespace) != 2 {
		t.Errorf("expected 2 jobs (Koku + RBAC), got %d", countJobs(c, testNamespace))
	}
}

func TestReconcileMigration_ROSEnabled_IncludesROSMigration(t *testing.T) {
	r, cfg, c := newMigrationTestReconciler(t)
	cfg.Spec.ROS.Enabled = boolPtr(true)
	cfg.Spec.ROS.Image = costv1alpha1.ImageSpec{
		Repository: "quay.io/test/ros",
		Tag:        "v1",
	}

	// Step 1: Koku Job created
	if _, err := r.reconcileMigration(context.Background(), cfg); err != nil {
		t.Fatalf("koku: %v", err)
	}
	if !jobExists(c, testNamespace, resources.NameKokuMigration(cfg)) {
		t.Fatal("expected Koku Job on first pass")
	}
	if jobExists(c, testNamespace, resources.NameROSMigration(cfg)) {
		t.Fatal("ROS Job should not exist yet")
	}
	if jobExists(c, testNamespace, resources.NameRBACMigration(cfg)) {
		t.Fatal("RBAC Job should not exist yet (gated behind ROS)")
	}

	// Step 2: Complete Koku → ROS Job created
	markJobComplete(t, c, testNamespace, resources.NameKokuMigration(cfg))
	if _, err := r.reconcileMigration(context.Background(), cfg); err != nil {
		t.Fatalf("ros: %v", err)
	}
	if !jobExists(c, testNamespace, resources.NameROSMigration(cfg)) {
		t.Fatal("expected ROS MigrationJob after Koku complete")
	}
	if jobExists(c, testNamespace, resources.NameRBACMigration(cfg)) {
		t.Fatal("RBAC Job should not exist yet (gated behind ROS)")
	}

	// Step 3: Complete ROS → RBAC Job created
	markJobComplete(t, c, testNamespace, resources.NameROSMigration(cfg))
	if _, err := r.reconcileMigration(context.Background(), cfg); err != nil {
		t.Fatalf("rbac: %v", err)
	}
	if !jobExists(c, testNamespace, resources.NameRBACMigration(cfg)) {
		t.Fatal("expected RBAC MigrationJob after ROS complete")
	}
}

func TestReconcileMigration_AdminBootstrapGated(t *testing.T) {
	r, cfg, c := newMigrationTestReconciler(t)

	for _, jobName := range []string{
		resources.NameKokuMigration(cfg),
		resources.NameRBACMigration(cfg),
	} {
		if _, err := r.reconcileMigration(context.Background(), cfg); err != nil {
			t.Fatalf("step: %v", err)
		}
		markJobComplete(t, c, testNamespace, jobName)
	}

	result, err := r.reconcileMigration(context.Background(), cfg)
	if err != nil {
		t.Fatalf("final: %v", err)
	}
	if !result.IsZero() {
		t.Fatalf("expected zero result, got %+v", result)
	}
	if jobExists(c, testNamespace, resources.NameRBACAdminBootstrap(cfg)) {
		t.Fatal("expected no AdminBootstrap Job when disabled")
	}
}

func TestReconcileMigration_AdminBootstrapEnabledWithSecret_CreatesJob(t *testing.T) {
	r, cfg, c := newMigrationTestReconciler(t)
	cfg.Spec.RBAC.BootstrapAdmin.Enabled = true
	cfg.Spec.RBAC.BootstrapAdmin.SecretRef.Name = "rbac-bootstrap-admin"

	for _, jobName := range []string{
		resources.NameKokuMigration(cfg),
		resources.NameRBACMigration(cfg),
	} {
		if _, err := r.reconcileMigration(context.Background(), cfg); err != nil {
			t.Fatalf("step: %v", err)
		}
		markJobComplete(t, c, testNamespace, jobName)
	}

	bootstrapName := resources.NameRBACAdminBootstrap(cfg)
	if _, err := r.reconcileMigration(context.Background(), cfg); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	if !jobExists(c, testNamespace, bootstrapName) {
		t.Fatal("expected AdminBootstrap Job when enabled with secretRef")
	}

	markJobComplete(t, c, testNamespace, bootstrapName)
	result, err := r.reconcileMigration(context.Background(), cfg)
	if err != nil {
		t.Fatalf("after bootstrap complete: %v", err)
	}
	if !result.IsZero() {
		t.Fatalf("expected zero Result after 4-step complete, got %+v", result)
	}
	cond := findCondition(cfg.Status.Conditions, costv1alpha1.ConditionSchemaUpToDate)
	if cond == nil || cond.Status != metav1.ConditionTrue || cond.Reason != "MigrationComplete" {
		t.Fatalf("expected SchemaUpToDate=True MigrationComplete after bootstrap, got %+v", cond)
	}
	for _, name := range []string{
		resources.NameKokuMigration(cfg),
		resources.NameRBACMigration(cfg),
		bootstrapName,
	} {
		if !jobExists(c, testNamespace, name) {
			t.Errorf("expected Job %q after 4-step complete", name)
		}
	}
	if jobExists(c, testNamespace, resources.NameROSMigration(cfg)) {
		t.Fatal("expected no ROS Job when ROS is disabled")
	}
	if countJobs(c, testNamespace) != 3 {
		t.Errorf("expected 3 Jobs (Koku + RBAC + admin-bootstrap), got %d", countJobs(c, testNamespace))
	}
}

func TestReconcileMigration_AdminBootstrapEnabledNoSecret_WarningEvent(t *testing.T) {
	r, cfg, c := newMigrationTestReconciler(t)
	cfg.Spec.RBAC.BootstrapAdmin.Enabled = true
	// Replace recorder with FakeRecorder to capture events
	rec := record.NewFakeRecorder(10)
	r.Recorder = rec

	for _, jobName := range []string{
		resources.NameKokuMigration(cfg),
		resources.NameRBACMigration(cfg),
	} {
		if _, err := r.reconcileMigration(context.Background(), cfg); err != nil {
			t.Fatalf("step: %v", err)
		}
		markJobComplete(t, c, testNamespace, jobName)
	}

	if _, err := r.reconcileMigration(context.Background(), cfg); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}

	found := false
	for range 20 {
		select {
		case event := <-rec.Events:
			if strings.Contains(event, "BootstrapAdminSkipped") {
				found = true
			}
		default:
		}
		if found {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !found {
		t.Fatal("expected BootstrapAdminSkipped warning event in events channel")
	}

	if jobExists(c, testNamespace, resources.NameRBACAdminBootstrap(cfg)) {
		t.Fatal("expected no AdminBootstrap Job when secretRef empty")
	}
}

func markJobComplete(t *testing.T, c client.Client, ns, name string) {
	t.Helper()
	ctx := context.Background()
	job := &batchv1.Job{}
	if err := c.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, job); err != nil {
		t.Fatalf("get job %s: %v", name, err)
	}
	job.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}}
	if err := c.Status().Update(ctx, job); err != nil {
		t.Fatalf("mark job complete: %v", err)
	}
}

func markJobFailed(t *testing.T, c client.Client, ns, name string) {
	t.Helper()
	ctx := context.Background()
	job := &batchv1.Job{}
	if err := c.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, job); err != nil {
		t.Fatalf("get job %s: %v", name, err)
	}
	job.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobFailed, Status: corev1.ConditionTrue}}
	if err := c.Status().Update(ctx, job); err != nil {
		t.Fatalf("mark job failed: %v", err)
	}
}

func getJobAnnotation(t *testing.T, c client.Client, ns, name, key string) string {
	t.Helper()
	ctx := context.Background()
	job := &batchv1.Job{}
	if err := c.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, job); err != nil {
		t.Fatalf("get job %s: %v", name, err)
	}
	return job.Annotations[key]
}

func jobExists(c client.Client, ns, name string) bool {
	ctx := context.Background()
	job := &batchv1.Job{}
	err := c.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, job)
	return err == nil
}

func countJobs(c client.Client, ns string) int {
	return len(jobUIDs(c, ns))
}

func jobUIDs(c client.Client, ns string) map[string]types.UID {
	ctx := context.Background()
	list := &batchv1.JobList{}
	if err := c.List(ctx, list, client.InNamespace(ns)); err != nil {
		return nil
	}
	out := make(map[string]types.UID, len(list.Items))
	for _, j := range list.Items {
		out[j.Name] = j.UID
	}
	return out
}

// assertMigrationBlockedStatus checks OpenShift top-level conditions for a
// migration gate that stopped the pipeline (ImageNotSet or MigrationFailed).
func assertMigrationBlockedStatus(t *testing.T, cfg *costv1alpha1.CostManagementServiceConfig, reason string) {
	t.Helper()
	want := []struct {
		typ    string
		status metav1.ConditionStatus
	}{
		{costv1alpha1.ConditionAvailable, metav1.ConditionFalse},
		{costv1alpha1.ConditionProgressing, metav1.ConditionFalse},
		{costv1alpha1.ConditionDegraded, metav1.ConditionTrue},
		{costv1alpha1.ConditionSchemaUpToDate, metav1.ConditionFalse},
	}
	for _, w := range want {
		cond := findCondition(cfg.Status.Conditions, w.typ)
		if cond == nil || cond.Status != w.status || cond.Reason != reason {
			t.Errorf("%s: got %+v, want status=%s reason=%s", w.typ, cond, w.status, reason)
		}
	}
	if cfg.Status.Phase != costv1alpha1.PhaseDegraded {
		t.Errorf("Phase = %q, want %q", cfg.Status.Phase, costv1alpha1.PhaseDegraded)
	}
}

// newMigrationTestReconciler returns a reconciler and CR with bundled DB/Cache
// (so Job builders resolve in-cluster endpoints) and a noopRecorder.
// Tests call reconcileMigration directly; Validation is not run.
func newMigrationTestReconciler(t *testing.T) (*CostManagementServiceConfigReconciler, *costv1alpha1.CostManagementServiceConfig, client.Client) {
	t.Helper()
	scheme := ownershipScheme(t)
	cfg := minimalCR(testCRName, testNamespace)
	cfg.Spec.Database.Deploy = boolPtr(true)
	cfg.Spec.Cache.Deploy = boolPtr(true)
	cfg.Spec.CostManagement.API.Image = costv1alpha1.ImageSpec{
		Repository: "quay.io/test/koku",
		Tag:        "v1",
	}
	cfg.Spec.RBAC.Image = costv1alpha1.ImageSpec{
		Repository: "quay.io/test/rbac",
		Tag:        "v1",
	}

	c := fakeClientWithApplySupport(scheme)
	r := &CostManagementServiceConfigReconciler{
		Client:   c,
		Scheme:   scheme,
		Recorder: &noopRecorder{},
	}
	return r, cfg, c
}
