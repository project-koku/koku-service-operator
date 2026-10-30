package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	cost "github.com/project-koku/koku-service-operator/api/v1alpha1"
	"github.com/project-koku/koku-service-operator/internal/release"
	"github.com/project-koku/koku-service-operator/internal/resources"
)

func upgradeFixture(t *testing.T) (*CostManagementServiceConfigReconciler, *cost.CostManagementServiceConfig, client.Client) {
	t.Helper()
	r, cfg, _ := newMigrationTestReconciler(t)
	cfg.Spec.Upgrade.ImagePolicy = "FollowOperator"
	cfg.Spec.Database.Image = cost.ImageSpec{Repository: "postgres", Tag: "16"}
	cfg.Spec.Cache.Image = cost.ImageSpec{Repository: "valkey", Tag: "8"}
	bundle := mustCurrentReleaseBundle()
	if err := bundle.Resolve(cfg); err != nil {
		t.Fatal(err)
	}
	cfg.Status.Upgrade.Installed = applicationRevision(cfg, bundle)
	c := fake.NewClientBuilder().WithScheme(r.Scheme).WithStatusSubresource(cfg, &batchv1.Job{}, &appsv1.Deployment{}).WithInterceptorFuncs(ssaCreateOrUpdate()).Build()
	r.Client = c
	r.APIReader = c
	r.Recorder = record.NewFakeRecorder(100)
	return r, cfg, c
}

func mustCurrentReleaseBundle() release.Bundle {
	bundle, err := release.Current()
	if err != nil {
		panic(err)
	}
	return bundle
}

func attachUpgrade(cfg *cost.CostManagementServiceConfig, stage string, mutating bool) {
	target := cfg.DeepCopy()
	target.Spec.CostManagement.API.Image.Tag = "v2"
	cfg.Status.Upgrade.Operation = &cost.UpgradeOperation{ID: "operation-1", TargetGeneration: 2, Source: cfg.Status.Upgrade.Installed.DeepCopy(), Target: *applicationRevision(target, mustCurrentReleaseBundle()), Stage: stage, SchemaMutationPossible: mutating, StartedAt: metav1.Now(), StageStartedAt: metav1.Now()}
}

func TestUpgradePlanPersistsBeforeMutation(t *testing.T) {
	r, cfg, c := upgradeFixture(t)
	cfg.Spec.CostManagement.API.Image.Tag = "v2"
	// Discovery is resolved by the standard fake-client fixture.
	cfg.Spec.Global.ClusterDomain = "apps.example.com"
	cfg.Spec.Global.StorageClass = "standard"
	result, err := r.reconcile(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if result.RequeueAfter == 0 || cfg.Status.Upgrade.Operation == nil {
		t.Fatal("upgrade did not checkpoint its plan")
	}
	if countJobs(c, cfg.Namespace) != 0 {
		t.Fatal("migration created before plan status persisted")
	}
	if cfg.Spec.CostManagement.API.Image.Tag != "v2" {
		t.Fatal("requested image pin changed")
	}
	// The persisted raw plan must survive a JSON round-trip and ignore target edits.
	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	restored := &cost.CostManagementServiceConfig{}
	if err := json.Unmarshal(data, restored); err != nil {
		t.Fatal(err)
	}
	restored.Spec.CostManagement.API.Image.Tag = "v3"
	if _, err := r.reconcile(context.Background(), restored); err != nil {
		t.Fatal(err)
	}
	var frozen cost.CostManagementServiceConfigSpec
	if err := json.Unmarshal(restored.Status.Upgrade.Operation.Target.Spec.Raw, &frozen); err != nil {
		t.Fatal(err)
	}
	if frozen.CostManagement.API.Image.Tag != "v2" {
		t.Fatal("mid-flight edit replaced frozen target")
	}
	if restored.Status.Upgrade.Operation.Stage != "Migrating" {
		t.Fatalf("unexpected stage %s", restored.Status.Upgrade.Operation.Stage)
	}
}
func TestUpgradeRollbackRestoresImagesWithoutStartingOldWriters(t *testing.T) {
	r, cfg, c := upgradeFixture(t)
	attachUpgrade(cfg, "RollingBack", true)
	for _, d := range resources.ApplicationDeployments(cfg) {
		setOwnerRef(cfg, d)
		d.Spec.Template.Spec.Containers[0].Image = "quay.io/target/app:v2"
		if err := c.Create(context.Background(), d); err != nil {
			t.Fatal(err)
		}
	}
	// Use the existing SSA fake for actual source-image restoration.
	r.Client = fakeClientWithApplySupport(r.Scheme)
	r.APIReader = r.Client
	for _, d := range resources.ApplicationDeployments(cfg) {
		setOwnerRef(cfg, d)
		d.Spec.Template.Spec.Containers[0].Image = "quay.io/target/app:v2"
		if err := r.Create(context.Background(), d); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := r.rollbackUpgrade(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.Status.Upgrade.Operation.Stage != "RecoveryRequired" {
		t.Fatal("unknown schema incorrectly resumed previous release")
	}
	for _, want := range resources.ApplicationDeployments(cfg) {
		live := &appsv1.Deployment{}
		if err := r.Get(context.Background(), client.ObjectKeyFromObject(want), live); err != nil {
			t.Fatal(err)
		}
		if *live.Spec.Replicas != 0 || live.Spec.Template.Spec.Containers[0].Image != want.Spec.Template.Spec.Containers[0].Image {
			t.Errorf("%s did not restore source image at zero replicas", want.Name)
		}
	}
	if !apimeta.IsStatusConditionTrue(cfg.Status.Conditions, cost.ConditionRecoveryRequired) {
		t.Fatal("recovery condition missing")
	}
}
func TestUpgradeQuiescenceWaitsForPodsAndLeavesInfrastructure(t *testing.T) {
	r, cfg, c := upgradeFixture(t)
	d := resources.KokuAPIDeployment(cfg)
	setOwnerRef(cfg, d)
	cache := resources.CacheDeployment(cfg)
	setOwnerRef(cfg, cache)
	for _, obj := range []client.Object{d, cache, &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "old-writer", Namespace: cfg.Namespace, Labels: resources.Labels(cfg, "cost-management-api")}, Status: corev1.PodStatus{Phase: corev1.PodRunning}}} {
		if err := c.Create(context.Background(), obj); err != nil {
			t.Fatal(err)
		}
	}
	quiet, err := r.quiesceApplications(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if quiet {
		t.Fatal("running source writer was ignored")
	}
	live := &appsv1.Deployment{}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(cache), live); err != nil {
		t.Fatal(err)
	}
	if *live.Spec.Replicas != 1 {
		t.Fatal("database/cache infrastructure was stopped")
	}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(d), live); err != nil {
		t.Fatal(err)
	}
	if *live.Spec.Replicas != 0 {
		t.Fatal("application was not drained")
	}
}
func TestUpgradeRecoveryRequiresBoundRequest(t *testing.T) {
	r, cfg, _ := upgradeFixture(t)
	attachUpgrade(cfg, "RecoveryRequired", true)
	cfg.Spec.Upgrade.Recovery = &cost.UpgradeRecoveryRequest{OperationID: "operation-1", RequestID: "repair-1", Action: "Retry"}
	cfg.Spec.CostManagement.API.Image.Tag = "forward-fix"
	if _, err := r.reconcile(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	op := cfg.Status.Upgrade.Operation
	if op.Stage != "Recovering" || op.RecoveryRequestID != "repair-1" || !op.SchemaMutationPossible {
		t.Fatal("explicit retry did not preserve recovery checkpoint")
	}
	if op.Target.Images["koku"] != "quay.io/test/koku:forward-fix" {
		t.Fatal("forward recovery target was not frozen")
	}
}

func TestUpgradeRecoveryRejectsUnboundOrConsumedRequests(t *testing.T) {
	for name, request := range map[string]*cost.UpgradeRecoveryRequest{
		"absent":           nil,
		"wrong operation":  {OperationID: "other-operation", RequestID: "new", Action: cost.RecoveryRetry},
		"empty request ID": {OperationID: "operation-1", Action: cost.RecoveryRetry},
		"consumed request": {OperationID: "operation-1", RequestID: "used", Action: cost.RecoveryRetry},
	} {
		t.Run(name, func(t *testing.T) {
			r, cfg, _ := upgradeFixture(t)
			attachUpgrade(cfg, cost.UpgradeStageRecoveryRequired, true)
			op := cfg.Status.Upgrade.Operation
			op.RecoveryRequestID = "used"
			targetID := op.Target.ID
			cfg.Spec.Upgrade.Recovery = request
			cfg.Spec.CostManagement.API.Image.Tag = "unaccepted-target"
			handled, err := r.acceptUpgradeRecovery(cfg, cfg, mustCurrentReleaseBundle(), nil)
			if err != nil || handled || op.Stage != cost.UpgradeStageRecoveryRequired || op.Target.ID != targetID {
				t.Fatal("unbound/consumed recovery request changed the operation")
			}
		})
	}
}

func TestUpgradeTimeoutRollsBackWithoutKillingMigration(t *testing.T) {
	r, cfg, c := upgradeFixture(t)
	attachUpgrade(cfg, "Migrating", true)
	cfg.Status.Upgrade.Operation.StartedAt = metav1.NewTime(time.Now().Add(-24 * time.Hour))
	job := resources.MigrationJob(cfg, "v1")
	if err := c.Create(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	if _, err := r.reconcile(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.Status.Upgrade.Operation.Stage != "RollingBack" || !jobExists(c, cfg.Namespace, job.Name) {
		t.Fatal("timeout must hold and retain active migration")
	}
	if _, err := r.reconcile(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.Status.Upgrade.Operation.Stage != "RollingBack" {
		t.Fatal("rollback overlapped an active migration")
	}
}

func TestUpgradeReadinessAllowsWarmShutdownBudget(t *testing.T) {
	r, cfg, _ := upgradeFixture(t)
	attachUpgrade(cfg, cost.UpgradeStageRollingOut, false)
	cfg.Status.Conditions = []metav1.Condition{{
		Type: cost.ConditionAvailable, Status: metav1.ConditionFalse,
		Reason:             reasonWaitingForApplicationRollout,
		LastTransitionTime: metav1.NewTime(time.Now().Add(-2 * readinessTimeout)),
	}}
	result := r.notReadyWait(cfg, reasonWaitingForApplicationRollout, "worker is draining", "worker")
	if result.IsZero() || cfg.Status.Phase == cost.PhaseDegraded {
		t.Fatal("ordinary readiness timeout interrupted the upgrade shutdown budget")
	}
}
func TestBeatReplacementWaitsForTermination(t *testing.T) {
	r, cfg, _ := upgradeFixture(t)
	c := fakeClientWithApplySupport(r.Scheme)
	r.Client = c
	r.APIReader = c
	desired := resources.CeleryBeatDeployment(cfg)
	setOwnerRef(cfg, desired)
	if err := r.applyBeat(context.Background(), desired); err != nil {
		t.Fatal(err)
	}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "old-beat", Namespace: cfg.Namespace, Labels: desired.Spec.Selector.MatchLabels}, Status: corev1.PodStatus{Phase: corev1.PodRunning}}
	if err := c.Create(context.Background(), pod); err != nil {
		t.Fatal(err)
	}
	changed := desired.DeepCopy()
	changed.Spec.Template.Spec.Containers[0].Image = "quay.io/koku:v2"
	for range 2 {
		if err := r.applyBeat(context.Background(), changed); err != nil {
			t.Fatal(err)
		}
	}
	live := &appsv1.Deployment{}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(desired), live); err != nil {
		t.Fatal(err)
	}
	if *live.Spec.Replicas != 0 || live.Spec.Template.Spec.Containers[0].Image == changed.Spec.Template.Spec.Containers[0].Image {
		t.Fatal("replacement Beat overlapped its predecessor")
	}
	if err := c.Delete(context.Background(), pod); err != nil {
		t.Fatal(err)
	}
	if err := r.applyBeat(context.Background(), changed); err != nil {
		t.Fatal(err)
	}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(desired), live); err != nil {
		t.Fatal(err)
	}
	if *live.Spec.Replicas != 1 || live.Spec.Template.Spec.Containers[0].Image != changed.Spec.Template.Spec.Containers[0].Image {
		t.Fatal("Beat replacement did not start after termination")
	}
}

func TestUpgradeFollowsOperatorWithoutChangingGeneration(t *testing.T) {
	r, cfg, c := upgradeFixture(t)
	// Establish the exact shipped source, then remove the stored application pins.
	for _, image := range release.ImageFields(cfg) {
		*image = cost.ImageSpec{}
	}
	cfg.Spec.CostManagement.Masu.Image = cost.ImageSpec{}
	bundle := mustCurrentReleaseBundle()
	if err := bundle.Resolve(cfg); err != nil {
		t.Fatal(err)
	}
	cfg.Status.Upgrade.Installed = applicationRevision(cfg, bundle)
	for _, job := range migrationJobs(cfg) {
		if err := c.Create(context.Background(), job); err != nil {
			t.Fatal(err)
		}
		markJobComplete(t, c, cfg.Namespace, job.Name)
	}
	cfg.Spec.Global.ClusterDomain = "apps.example.com"
	cfg.Spec.Global.StorageClass = "standard"
	for _, image := range release.ImageFields(cfg) {
		*image = cost.ImageSpec{}
	}
	next := mustCurrentReleaseBundle()
	next["koku"] = "quay.io/project-koku/koku:v2"
	r.ReleaseImages = next
	generation := cfg.Generation
	if _, err := r.reconcile(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	op := cfg.Status.Upgrade.Operation
	if op == nil || op.Target.Images["koku"] != next["koku"] || op.Source.Images["koku"] == next["koku"] {
		t.Fatal("operator replacement did not plan an operand image change")
	}
	if cfg.Generation != generation || cfg.Spec.CostManagement.API.Image.Repository != "" {
		t.Fatal("operator replacement mutated the requested spec/generation")
	}
}

// lifecyclePlatform simulates the platform controllers, not the operator's
// upgrade decisions: it acknowledges current replica specs and Job results.
func lifecyclePlatform(t *testing.T) (*CostManagementServiceConfigReconciler, *cost.CostManagementServiceConfig, client.Client) {
	t.Helper()
	r, cfg, _ := upgradeFixture(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"keys":[{"kty":"RSA","kid":"test"}]}`))
	}))
	t.Cleanup(server.Close)
	cfg.Spec.Auth.Keycloak.URL = server.URL
	cfg.Spec.Global.ClusterDomain = "apps.example.com"
	cfg.Spec.Global.StorageClass = "standard"
	cfg.Spec.Monitoring.Enabled = new(false)
	cfg.Spec.ObjectStorage.Endpoint = "http://127.0.0.1:1"
	cfg.Spec.ObjectStorage.Port = 1
	cfg.Spec.ObjectStorage.SecretName = "test-s3"
	cfg.Spec.ObjectStorage.Buckets.Koku = "test-bucket"
	cfg.Spec.Kafka.BootstrapServers = "127.0.0.1:1"
	cfg.Status.Upgrade = cost.UpgradeStatus{}
	c := fakeClientPreservingStatus(r.Scheme)
	r.Client = c
	r.APIReader = c
	for _, secret := range []*corev1.Secret{
		{ObjectMeta: metav1.ObjectMeta{Name: resources.NameUIOAuthClientSecret(cfg), Namespace: cfg.Namespace}, Data: map[string][]byte{"client-id": []byte("id"), "client-secret": []byte("secret")}},
		{ObjectMeta: metav1.ObjectMeta{Name: "test-s3", Namespace: cfg.Namespace}, Data: map[string][]byte{"access-key": []byte("key"), "secret-key": []byte("secret")}},
	} {
		if err := c.Create(context.Background(), secret); err != nil {
			t.Fatal(err)
		}
	}
	return r, cfg, c
}
func driveLifecycle(t *testing.T, r *CostManagementServiceConfigReconciler, cfg *cost.CostManagementServiceConfig, c client.Client, failImage string, done func() bool) {
	t.Helper()
	for pass := range 60 {
		if _, err := r.reconcile(context.Background(), cfg); err != nil {
			t.Fatalf("pass %d: %v", pass, err)
		}
		deps := &appsv1.DeploymentList{}
		if err := c.List(context.Background(), deps, client.InNamespace(cfg.Namespace)); err != nil {
			t.Fatal(err)
		}
		for _, d := range deps.Items {
			markDeploymentReady(t, c, cfg.Namespace, d.Name)
		}
		states := &appsv1.StatefulSetList{}
		if err := c.List(context.Background(), states, client.InNamespace(cfg.Namespace)); err != nil {
			t.Fatal(err)
		}
		for _, s := range states.Items {
			markStatefulSetReady(t, c, cfg.Namespace, s.Name)
		}
		jobs := &batchv1.JobList{}
		if err := c.List(context.Background(), jobs, client.InNamespace(cfg.Namespace)); err != nil {
			t.Fatal(err)
		}
		for _, j := range jobs.Items {
			if isJobComplete(&j) || isJobFailed(&j) {
				continue
			}
			if failImage != "" && strings.Contains(j.Spec.Template.Spec.Containers[0].Image, failImage) {
				markJobFailed(t, c, cfg.Namespace, j.Name)
			} else {
				markJobComplete(t, c, cfg.Namespace, j.Name)
			}
		}
		for _, name := range []string{resources.NameAPIRoute(cfg), resources.NameUI(cfg)} {
			route, err := r.getRoute(context.Background(), cfg.Namespace, name)
			if err == nil {
				markRouteAdmitted(t, c, cfg.Namespace, route.GetName())
			}
		}
		// Simulate controller restart by round-tripping the whole CMSC and
		// rebuilding the reconciler; no operation state can rely on process memory.
		data, err := json.Marshal(cfg)
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(data, cfg); err != nil {
			t.Fatal(err)
		}
		*r = CostManagementServiceConfigReconciler{Client: r.Client, APIReader: r.APIReader, Scheme: r.Scheme, Recorder: r.Recorder, ReleaseImages: r.ReleaseImages}
		if done() {
			return
		}
	}
	t.Fatalf("lifecycle did not reach expected state: phase=%s upgrade=%+v conditions=%+v", cfg.Status.Phase, cfg.Status.Upgrade.Operation, cfg.Status.Conditions)
}
func TestUpgradeLifecycleSuccessAndForwardRecovery(t *testing.T) {
	r, cfg, c := lifecyclePlatform(t)
	driveLifecycle(t, r, cfg, c, "", func() bool { return cfg.Status.Phase == cost.PhaseReady && cfg.Status.Upgrade.Installed != nil })
	initial := cfg.Status.Upgrade.Installed.ID
	cfg.Spec.CostManagement.API.Image.Tag = "v2"
	cfg.Generation++
	driveLifecycle(t, r, cfg, c, "", func() bool {
		return cfg.Status.Phase == cost.PhaseReady && cfg.Status.Upgrade.Operation == nil && cfg.Status.Upgrade.Installed.ID != initial
	})
	if cfg.Status.Upgrade.Installed.Images["koku"] != "quay.io/test/koku:v2" {
		t.Fatal("target was not recorded as installed")
	}
	drainEvents(r)
	cfg.Spec.CostManagement.API.Image.Tag = "v3"
	cfg.Generation++
	driveLifecycle(t, r, cfg, c, ":v3", func() bool {
		return cfg.Status.Upgrade.Operation != nil && cfg.Status.Upgrade.Operation.Stage == "RecoveryRequired"
	})
	op := cfg.Status.Upgrade.Operation
	if !op.SchemaMutationPossible || !jobExists(c, cfg.Namespace, resources.NameKokuMigration(cfg)) {
		t.Fatal("failure evidence was lost")
	}
	failed := getMigrationJob(t, c, cfg.Namespace, resources.NameKokuMigration(cfg))
	identity := fmt.Sprintf("%s (UID %s)", failed.Name, failed.UID)
	condition := apimeta.FindStatusCondition(cfg.Status.Conditions, cost.ConditionUpgradeFailed)
	if condition == nil || condition.Status != metav1.ConditionTrue || !strings.Contains(condition.Message, identity) {
		t.Fatalf("UpgradeFailed must name the failed migration %s: %+v", identity, condition)
	}
	if got := countEvents(drainEvents(r), "UpgradeFailed"); got != 1 {
		t.Fatalf("want one UpgradeFailed Event for the failed migration, got %d", got)
	}
	cfg.Spec.CostManagement.API.Image.Tag = "v4"
	cfg.Spec.Upgrade.Recovery = &cost.UpgradeRecoveryRequest{OperationID: op.ID, RequestID: "forward-repair", Action: "Retry"}
	cfg.Generation++
	driveLifecycle(t, r, cfg, c, "", func() bool { return cfg.Status.Phase == cost.PhaseReady && cfg.Status.Upgrade.Operation == nil })
	if cfg.Status.Upgrade.Installed.Images["koku"] != "quay.io/test/koku:v4" || !apimeta.IsStatusConditionFalse(cfg.Status.Conditions, cost.ConditionRecoveryRequired) {
		t.Fatal("forward repair did not restore target readiness")
	}
}
func TestUpgradeAutomaticallyResumesSourceBeforeSchemaMutation(t *testing.T) {
	r, cfg, c := lifecyclePlatform(t)
	driveLifecycle(t, r, cfg, c, "", func() bool { return cfg.Status.Upgrade.Installed != nil })
	oldUI := cfg.Status.Upgrade.Installed.Images["ui"]
	cfg.Spec.UI.App.Image.Tag = "bad-ui"
	cfg.Generation++
	r.Client = interceptor.NewClient(c.(client.WithWatch), interceptor.Funcs{Patch: func(ctx context.Context, inner client.WithWatch, obj client.Object, p client.Patch, opts ...client.PatchOption) error {
		if d, ok := obj.(*appsv1.Deployment); ok && d.Name == resources.NameUI(cfg) {
			for _, container := range d.Spec.Template.Spec.Containers {
				if strings.Contains(container.Image, ":bad-ui") {
					return errors.New("target UI rollout rejected")
				}
			}
		}
		return inner.Patch(ctx, obj, p, opts...)
	}})
	driveLifecycle(t, r, cfg, c, "", func() bool {
		return cfg.Status.Upgrade.Operation != nil && cfg.Status.Upgrade.Operation.Stage == "RolledBack"
	})
	if !apimeta.IsStatusConditionTrue(cfg.Status.Conditions, cost.ConditionAvailable) || !apimeta.IsStatusConditionTrue(cfg.Status.Conditions, cost.ConditionUpgradeFailed) {
		t.Fatal("current availability and failed target were not distinguished")
	}
	d := &appsv1.Deployment{}
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: cfg.Namespace, Name: resources.NameUI(cfg)}, d); err != nil {
		t.Fatal(err)
	}
	restored := false
	for _, container := range d.Spec.Template.Spec.Containers {
		if container.Image == oldUI {
			restored = true
		}
	}
	if !restored {
		t.Fatal("previous UI image was not restored")
	}

	// The rollback hold must stay quiet across drift passes: no Ready/PhaseChanged
	// Events and no Degraded transition reset while the target remains failed.
	degraded := apimeta.FindStatusCondition(cfg.Status.Conditions, cost.ConditionDegraded)
	if degraded == nil || degraded.Status != metav1.ConditionTrue {
		t.Fatalf("rollback hold must report Degraded=True: %+v", degraded)
	}
	since := degraded.LastTransitionTime
	drainEvents(r)
	for range 3 {
		if _, err := r.reconcile(context.Background(), cfg); err != nil {
			t.Fatal(err)
		}
	}
	events := drainEvents(r)
	if n := countEvents(events, "Ready") + countEvents(events, "PhaseChanged"); n != 0 {
		t.Fatalf("rollback hold emitted %d Ready/PhaseChanged events: %v", n, events)
	}
	degraded = apimeta.FindStatusCondition(cfg.Status.Conditions, cost.ConditionDegraded)
	if degraded == nil || degraded.Status != metav1.ConditionTrue || !degraded.LastTransitionTime.Equal(&since) {
		t.Fatalf("rollback hold reset Degraded: %+v (was %v)", degraded, since)
	}
}

func TestQuiescenceIgnoresBundledDatabaseAndCachePods(t *testing.T) {
	r, cfg, c := upgradeFixture(t)
	for _, component := range []string{"database", "cache"} {
		pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "bundled-" + component, Namespace: cfg.Namespace, Labels: resources.Labels(cfg, component)}, Status: corev1.PodStatus{Phase: corev1.PodRunning}}
		if err := c.Create(context.Background(), pod); err != nil {
			t.Fatal(err)
		}
	}
	stopped, err := r.applicationPodsStopped(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !stopped {
		t.Fatal("bundled database/cache infrastructure must not block quiescence")
	}
}
func TestUpgradeExplicitResumePrevious(t *testing.T) {
	r, cfg, c := lifecyclePlatform(t)
	driveLifecycle(t, r, cfg, c, "", func() bool { return cfg.Status.Upgrade.Installed != nil })
	old := cfg.Status.Upgrade.Installed.Images["koku"]
	cfg.Spec.CostManagement.API.Image.Tag = "failed"
	cfg.Generation++
	driveLifecycle(t, r, cfg, c, ":failed", func() bool {
		return cfg.Status.Upgrade.Operation != nil && cfg.Status.Upgrade.Operation.Stage == "RecoveryRequired"
	})
	cfg.Spec.Upgrade.Recovery = &cost.UpgradeRecoveryRequest{OperationID: cfg.Status.Upgrade.Operation.ID, RequestID: "restore-verified", Action: "ResumePrevious"}
	driveLifecycle(t, r, cfg, c, "", func() bool { return cfg.Status.Upgrade.Operation.Stage == "RolledBack" })
	d := &appsv1.Deployment{}
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: cfg.Namespace, Name: resources.NameKokuAPI(cfg)}, d); err != nil {
		t.Fatal(err)
	}
	if d.Spec.Template.Spec.Containers[0].Image != old || *d.Spec.Replicas == 0 {
		t.Fatal("confirmed source recovery did not resume the previous image")
	}
	// A pending/new image edit must not undo the administrator's source
	// recovery or replay a consumed request. A new bound Retry is required.
	cfg.Spec.CostManagement.API.Image.Tag = "not-yet-accepted"
	cfg.Generation++
	for range 3 {
		if _, err := r.reconcile(context.Background(), cfg); err != nil {
			t.Fatal(err)
		}
		if cfg.Status.Upgrade.Operation == nil || cfg.Status.Upgrade.Operation.Stage != cost.UpgradeStageRolledBack {
			t.Fatal("image edit or consumed recovery request bypassed the rollback hold")
		}
	}
}

func TestLegacyUpgradeBaseline(t *testing.T) {
	r, cfg, c := upgradeFixture(t)
	for _, d := range resources.ApplicationDeployments(cfg) {
		setOwnerRef(cfg, d)
		if err := c.Create(context.Background(), d); err != nil {
			t.Fatal(err)
		}
	}
	for _, j := range migrationJobs(cfg) {
		if err := c.Create(context.Background(), j); err != nil {
			t.Fatal(err)
		}
		markJobComplete(t, c, cfg.Namespace, j.Name)
	}
	baseline, err := r.legacyBaselineMatches(context.Background(), cfg)
	if err != nil || !baseline.Matches || len(baseline.MissingMigrationJobs) != 0 {
		t.Fatalf("matching legacy baseline not adopted: %v", err)
	}
	cfg.Spec.CostManagement.API.Image.Tag = "uninstalled"
	baseline, err = r.legacyBaselineMatches(context.Background(), cfg)
	if err != nil || baseline.Matches {
		t.Fatal("requested image was mistaken for installed baseline")
	}
	cfg.Status.Upgrade = cost.UpgradeStatus{}
	cfg.Status.Phase = cost.PhaseReady
	if _, err := r.reconcile(context.Background(), cfg); err == nil {
		t.Fatal("unknown baseline must block before replacing children")
	}
	if _, err := r.reconcile(context.Background(), cfg); err == nil {
		t.Fatal("baseline block was lost on another reconcile")
	}
}

func TestLegacyBaselineMissingMigrationJobPlansQuiescedRecovery(t *testing.T) {
	r, cfg, c := upgradeFixture(t)
	cfg.Status.Upgrade.Installed = nil
	cfg.Status.Phase = cost.PhaseReady
	cfg.Spec.Global.ClusterDomain = "apps.example.com"
	cfg.Spec.Global.StorageClass = "standard"
	for _, deployment := range resources.ApplicationDeployments(cfg) {
		setOwnerRef(cfg, deployment)
		if err := c.Create(context.Background(), deployment); err != nil {
			t.Fatal(err)
		}
	}
	jobs := migrationJobs(cfg)
	for _, job := range jobs {
		if err := c.Create(context.Background(), job); err != nil {
			t.Fatal(err)
		}
		markJobComplete(t, c, cfg.Namespace, job.Name)
	}
	missing := jobs[0]
	if err := c.Delete(context.Background(), missing); err != nil {
		t.Fatal(err)
	}

	baseline, err := r.legacyBaselineMatches(context.Background(), cfg)
	if err != nil || !baseline.Matches || len(baseline.MissingMigrationJobs) != 1 || baseline.MissingMigrationJobs[0] != missing.Name {
		t.Fatalf("missing receipt did not preserve the verified baseline: check=%+v err=%v", baseline, err)
	}
	if _, err := r.reconcile(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.Status.Upgrade.Installed == nil || cfg.Status.Upgrade.Operation != nil || jobExists(c, cfg.Namespace, missing.Name) {
		t.Fatal("legacy adoption must checkpoint the baseline without starting a migration")
	}
	if cfg.Status.Phase != cost.PhaseProgressing {
		t.Fatalf("legacy recovery phase = %q, want Progressing", cfg.Status.Phase)
	}

	if _, err := r.reconcile(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.Status.Upgrade.Operation == nil || cfg.Status.Upgrade.Operation.Stage != cost.UpgradeStageQuiescing {
		t.Fatalf("missing receipt did not create a quiesced migration operation: %+v", cfg.Status.Upgrade.Operation)
	}
	if jobExists(c, cfg.Namespace, missing.Name) {
		t.Fatal("missing migration Job was recreated before application quiescence")
	}
}

func TestTransientMigrationAPIErrorRetriesWithoutFailingUpgrade(t *testing.T) {
	r, cfg, _ := upgradeFixture(t)
	listeners := make([]net.Listener, 0, 2)
	defer func() {
		for _, listener := range listeners {
			_ = listener.Close()
		}
	}()
	for _, item := range []struct {
		setHost func(string)
		setPort func(int32)
	}{
		{func(host string) { cfg.Spec.Database.Host = host }, func(port int32) { cfg.Spec.Database.Port = port }},
		{func(host string) { cfg.Spec.Cache.Host = host }, func(port int32) { cfg.Spec.Cache.Port = port }},
	} {
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		listeners = append(listeners, listener)
		item.setHost("127.0.0.1")
		item.setPort(int32(listener.Addr().(*net.TCPAddr).Port))
	}
	cfg.Spec.Database.Deploy = new(false)
	cfg.Spec.Cache.Deploy = new(false)
	attachUpgrade(cfg, cost.UpgradeStageMigrating, true)

	base := r.Client.(client.WithWatch)
	failedCreate := false
	r.Client = interceptor.NewClient(base, interceptor.Funcs{
		Create: func(ctx context.Context, inner client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			if job, ok := obj.(*batchv1.Job); ok && job.Name == resources.NameKokuMigration(cfg) && !failedCreate {
				failedCreate = true
				return apierrors.NewServiceUnavailable("temporary API outage")
			}
			return inner.Create(ctx, obj, opts...)
		},
	})
	r.APIReader = r.Client
	result, err := r.migrateUpgrade(context.Background(), cfg)
	if err != nil || result.RequeueAfter == 0 {
		t.Fatalf("transient migration API error should requeue: result=%+v err=%v", result, err)
	}
	if !failedCreate || cfg.Status.Upgrade.Operation.Stage != cost.UpgradeStageMigrating || !cfg.Status.Upgrade.Operation.SchemaMutationPossible {
		t.Fatalf("transient migration API error changed or discarded the migration checkpoint: failedCreate=%t operation=%+v", failedCreate, cfg.Status.Upgrade.Operation)
	}
	if cfg.Status.Upgrade.Operation.Failure != "" || apimeta.IsStatusConditionTrue(cfg.Status.Conditions, cost.ConditionUpgradeFailed) {
		t.Fatal("transient migration API error was recorded as a failed upgrade")
	}
	if jobExists(base, cfg.Namespace, resources.NameKokuMigration(cfg)) {
		t.Fatal("failed create unexpectedly left a migration Job")
	}

	r.Client = base
	r.APIReader = base
	result, err = r.migrateUpgrade(context.Background(), cfg)
	if err != nil || result.RequeueAfter == 0 {
		t.Fatalf("migration did not resume on retry: result=%+v err=%v", result, err)
	}
	if !jobExists(base, cfg.Namespace, resources.NameKokuMigration(cfg)) || cfg.Status.Upgrade.Operation.Stage != cost.UpgradeStageMigrating {
		t.Fatal("migration retry did not resume the same journaled operation")
	}
}

func TestUnreachableDatabaseDuringRolloutRequeuesWithoutRollback(t *testing.T) {
	r, cfg, _ := upgradeFixture(t)
	for _, set := range []func(int32){func(port int32) { cfg.Spec.Database.Port = port }, func(port int32) { cfg.Spec.Cache.Port = port }} {
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		port := int32(listener.Addr().(*net.TCPAddr).Port)
		if err := listener.Close(); err != nil {
			t.Fatal(err)
		}
		set(port)
	}
	cfg.Spec.Database.Deploy = new(false)
	cfg.Spec.Database.Host = "127.0.0.1"
	cfg.Spec.Cache.Deploy = new(false)
	cfg.Spec.Cache.Host = "127.0.0.1"
	attachUpgrade(cfg, cost.UpgradeStageRollingOut, true)

	result := r.rolloutUpgrade(context.Background(), cfg)
	if result.RequeueAfter == 0 {
		t.Fatal("unreachable database should requeue the rollout")
	}
	if cfg.Status.Upgrade.Operation.Stage != cost.UpgradeStageRollingOut || !cfg.Status.Upgrade.Operation.SchemaMutationPossible {
		t.Fatal("transient dependency failure changed or discarded the upgrade checkpoint")
	}
	if cfg.Status.Upgrade.Operation.Failure != "" || apimeta.IsStatusConditionTrue(cfg.Status.Conditions, cost.ConditionUpgradeFailed) {
		t.Fatal("transient dependency failure was recorded as a failed upgrade")
	}
	if !apimeta.IsStatusConditionTrue(cfg.Status.Conditions, cost.ConditionProgressing) ||
		!apimeta.IsStatusConditionFalse(cfg.Status.Conditions, cost.ConditionAvailable) {
		t.Fatal("retrying dependency failure did not report Progressing and unavailable")
	}
}

func TestUnreachableDatabaseDuringMigrationRequeuesWithoutRollback(t *testing.T) {
	r, cfg, _ := upgradeFixture(t)
	for _, item := range []struct {
		setHost func(string)
		setPort func(int32)
	}{
		{func(host string) { cfg.Spec.Database.Host = host }, func(port int32) { cfg.Spec.Database.Port = port }},
		{func(host string) { cfg.Spec.Cache.Host = host }, func(port int32) { cfg.Spec.Cache.Port = port }},
	} {
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		port := int32(listener.Addr().(*net.TCPAddr).Port)
		if err := listener.Close(); err != nil {
			t.Fatal(err)
		}
		item.setHost("127.0.0.1")
		item.setPort(port)
	}
	cfg.Spec.Database.Deploy = new(false)
	cfg.Spec.Cache.Deploy = new(false)
	attachUpgrade(cfg, cost.UpgradeStageMigrating, true)

	result, err := r.migrateUpgrade(context.Background(), cfg)
	if err != nil || result.RequeueAfter == 0 {
		t.Fatalf("unreachable database should requeue migration preflight: result=%+v err=%v", result, err)
	}
	if cfg.Status.Upgrade.Operation.Stage != cost.UpgradeStageMigrating || !cfg.Status.Upgrade.Operation.SchemaMutationPossible {
		t.Fatal("transient dependency failure changed or discarded the migration checkpoint")
	}
	if cfg.Status.Upgrade.Operation.Failure != "" || apimeta.IsStatusConditionTrue(cfg.Status.Conditions, cost.ConditionUpgradeFailed) {
		t.Fatal("transient dependency failure was recorded as a failed upgrade")
	}
	if !apimeta.IsStatusConditionTrue(cfg.Status.Conditions, cost.ConditionProgressing) ||
		!apimeta.IsStatusConditionFalse(cfg.Status.Conditions, cost.ConditionAvailable) ||
		!apimeta.IsStatusConditionTrue(cfg.Status.Conditions, cost.ConditionDegraded) {
		t.Fatal("retrying dependency failure did not preserve Degraded while reporting Progressing and unavailable")
	}
}

func TestApplicationCompletionIncludesAllTargetsAndOldPods(t *testing.T) {
	r, cfg, c := upgradeFixture(t)
	for _, d := range resources.ApplicationDeployments(cfg) {
		setOwnerRef(cfg, d)
		if err := c.Create(context.Background(), d); err != nil {
			t.Fatal(err)
		}
		markDeploymentReady(t, c, cfg.Namespace, d.Name)
	}
	result, err := r.waitForApplicationRollout(context.Background(), cfg)
	if err != nil || !result.IsZero() {
		t.Fatalf("complete targets not ready: %+v %v", result, err)
	}
	name := resources.NameKokuAPI(cfg)
	d := &appsv1.Deployment{}
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: cfg.Namespace, Name: name}, d); err != nil {
		t.Fatal(err)
	}
	d.Status.ReadyReplicas = 0
	if err := c.Status().Update(context.Background(), d); err != nil {
		t.Fatal(err)
	}
	result, err = r.waitForApplicationRollout(context.Background(), cfg)
	if err != nil || result.IsZero() {
		t.Fatal("unready target incorrectly completed")
	}
	markDeploymentReady(t, c, cfg.Namespace, name)
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(d), d); err != nil {
		t.Fatal(err)
	}
	d.Annotations = map[string]string{"deployment.kubernetes.io/revision": "2"}
	if err := c.Update(context.Background(), d); err != nil {
		t.Fatal(err)
	}
	rs := &appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{Name: "old-rs", Namespace: cfg.Namespace, Annotations: map[string]string{"deployment.kubernetes.io/revision": "1"}}}
	if err := c.Create(context.Background(), rs); err != nil {
		t.Fatal(err)
	}
	p := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "old-target", Namespace: cfg.Namespace, Labels: d.Spec.Selector.MatchLabels, OwnerReferences: []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: "ReplicaSet", Name: rs.Name, UID: "old-rs", Controller: new(true)}}}, Status: corev1.PodStatus{Phase: corev1.PodRunning}}
	if err := c.Create(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	result, err = r.waitForApplicationRollout(context.Background(), cfg)
	if err != nil || result.IsZero() {
		t.Fatal("old ReplicaSet Pod incorrectly ignored")
	}
	// A pruned old ReplicaSet whose Pod still terminates is a wait, not a failure.
	if err := c.Delete(context.Background(), rs); err != nil {
		t.Fatal(err)
	}
	result, err = r.waitForApplicationRollout(context.Background(), cfg)
	if err != nil || result.IsZero() {
		t.Fatalf("pruned ReplicaSet must keep the rollout waiting without error: %+v %v", result, err)
	}
	if err := c.Delete(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	result, err = r.waitForApplicationRollout(context.Background(), cfg)
	if err != nil || !result.IsZero() {
		t.Fatalf("rollout did not complete after old Pod stopped: %v", err)
	}
}
func TestUpgradeQuiescenceCoversScheduledAndMigrationWriters(t *testing.T) {
	r, cfg, c := upgradeFixture(t)
	cron := resources.KeycloakSyncCronJob(cfg)
	setOwnerRef(cfg, cron)
	if err := c.Create(context.Background(), cron); err != nil {
		t.Fatal(err)
	}
	job := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: "scheduled-work", Namespace: cfg.Namespace, Labels: resources.Labels(cfg, "rbac-sync"), OwnerReferences: []metav1.OwnerReference{{APIVersion: "batch/v1", Kind: "CronJob", Name: cron.Name, UID: "cron", Controller: new(true)}}}}
	if err := c.Create(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	quiet, err := r.quiesceApplications(context.Background(), cfg)
	if err != nil || quiet {
		t.Fatal("scheduled active Job was ignored")
	}
	markJobComplete(t, c, cfg.Namespace, job.Name)
	p := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "migration-survivor", Namespace: cfg.Namespace, Labels: resources.Labels(cfg, "cost-management-migration"), OwnerReferences: []metav1.OwnerReference{{APIVersion: "batch/v1", Kind: "Job", Name: resources.NameKokuMigration(cfg), UID: "deleted", Controller: new(true)}}}, Status: corev1.PodStatus{Phase: corev1.PodRunning}}
	if err := c.Create(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	stopped, err := r.migrationWritersStopped(context.Background(), cfg)
	if err != nil || stopped {
		t.Fatal("surviving migration writer was ignored")
	}
	quiet, err = r.quiesceApplications(context.Background(), cfg)
	if err != nil || !quiet {
		t.Fatal("application drain incorrectly treats migration writer as an application")
	}
}

func TestUpgradeCheckpointsSchemaRiskBeforeReplacingReceipt(t *testing.T) {
	r, cfg, c := lifecyclePlatform(t)
	driveLifecycle(t, r, cfg, c, "", func() bool { return cfg.Status.Upgrade.Installed != nil })
	cfg.Spec.CostManagement.API.Image.Tag = "v2"
	cfg.Generation++
	driveLifecycle(t, r, cfg, c, "", func() bool {
		return cfg.Status.Upgrade.Operation != nil && cfg.Status.Upgrade.Operation.Stage == "Migrating"
	})
	if cfg.Status.Upgrade.Operation.SchemaMutationPossible {
		t.Fatal("mutation marker must be a separate checkpoint")
	}
	before := cfg.Status.DeepCopy()
	job := getMigrationJob(t, c, cfg.Namespace, resources.NameKokuMigration(cfg))
	oldImage := job.Spec.Template.Spec.Containers[0].Image
	if _, err := r.reconcile(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	if !cfg.Status.Upgrade.Operation.SchemaMutationPossible {
		t.Fatal("schema risk not checkpointed")
	}
	job = getMigrationJob(t, c, cfg.Namespace, job.Name)
	if job.Spec.Template.Spec.Containers[0].Image != oldImage || !isJobComplete(job) {
		t.Fatal("receipt replaced before mutation marker could be persisted")
	}
	// Model loss of the outer status write. Reconciliation must checkpoint
	// again without advancing a Job based solely on transient process state.
	cfg.Status = *before
	if _, err := r.reconcile(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	job = getMigrationJob(t, c, cfg.Namespace, job.Name)
	if job.Spec.Template.Spec.Containers[0].Image != oldImage || !isJobComplete(job) {
		t.Fatal("lost status write allowed unjournaled schema execution")
	}
}

func TestOperatorHandoffRejectsChangedMigrationInputs(t *testing.T) {
	for _, stage := range []string{cost.UpgradeStageQuiescing, cost.UpgradeStageMigrating} {
		t.Run(stage, func(t *testing.T) {
			r, cfg, c := upgradeFixture(t)
			attachUpgrade(cfg, stage, stage == cost.UpgradeStageMigrating)
			// The replacement operator computes different execution identities
			// than the operator that saved this operation's target.
			cfg.Status.Upgrade.Operation.Target.Migrations[resources.NameKokuMigration(cfg)] = "old-operator-inputs"
			if _, err := r.reconcile(context.Background(), cfg); err != nil {
				t.Fatal(err)
			}
			if cfg.Status.Upgrade.Operation.Stage != cost.UpgradeStageRollingBack {
				t.Fatal("operator handoff silently replaced frozen migration inputs")
			}
			cms := &corev1.ConfigMapList{}
			if err := c.List(context.Background(), cms); err != nil {
				t.Fatal(err)
			}
			if len(cms.Items) != 0 || countJobs(c, cfg.Namespace) != 0 {
				t.Fatal("target execution inputs changed before handoff rejection")
			}
		})
	}
}

func TestMalformedFrozenRevisionsFailClosed(t *testing.T) {
	for _, stage := range []string{cost.UpgradeStageMigrating, cost.UpgradeStageRollingBack} {
		t.Run(stage, func(t *testing.T) {
			r, cfg, c := upgradeFixture(t)
			attachUpgrade(cfg, stage, true)
			if stage == cost.UpgradeStageRollingBack {
				cfg.Status.Upgrade.Operation.Source.Spec.Raw = []byte(`{}`)
			} else {
				cfg.Status.Upgrade.Operation.Target.Spec.Raw = []byte(`{}`)
			}
			if _, err := r.reconcile(context.Background(), cfg); err == nil {
				t.Fatal("incomplete saved revision must block mutation")
			}
			if countJobs(c, cfg.Namespace) != 0 {
				t.Fatal("incomplete saved revision launched a migration")
			}
		})
	}
}

func drainEvents(r *CostManagementServiceConfigReconciler) []string {
	recorder := r.Recorder.(*record.FakeRecorder)
	var events []string
	for {
		select {
		case event := <-recorder.Events:
			events = append(events, event)
		default:
			return events
		}
	}
}

func countEvents(events []string, reason string) int {
	count := 0
	for _, event := range events {
		if strings.Contains(event, " "+reason+" ") {
			count++
		}
	}
	return count
}

type timeoutError struct{}

func (timeoutError) Error() string   { return "i/o timeout" }
func (timeoutError) Timeout() bool   { return true }
func (timeoutError) Temporary() bool { return true }

func TestRetryableUpgradeError(t *testing.T) {
	resource := schema.GroupResource{Group: "batch", Resource: "jobs"}
	for name, tc := range map[string]struct {
		err  error
		want bool
	}{
		"nil":                {nil, false},
		"deadline":           {fmt.Errorf("wrapped: %w", context.DeadlineExceeded), true},
		"network timeout":    {&net.OpError{Op: "dial", Err: timeoutError{}}, true},
		"conflict":           {apierrors.NewConflict(resource, "job", errors.New("stale")), true},
		"too many requests":  {apierrors.NewTooManyRequests("slow down", 1), true},
		"service down":       {apierrors.NewServiceUnavailable("unavailable"), true},
		"connection refused": {&url.Error{Op: "Patch", URL: "https://api:6443", Err: &net.OpError{Op: "dial", Err: os.NewSyscallError("connect", syscall.ECONNREFUSED)}}, true},
		"connection reset":   {&url.Error{Op: "Get", URL: "https://api:6443", Err: &net.OpError{Op: "read", Err: os.NewSyscallError("read", syscall.ECONNRESET)}}, true},
		"unexpected EOF":     {&url.Error{Op: "Get", URL: "https://api:6443", Err: io.ErrUnexpectedEOF}, true},
		"forbidden":          {apierrors.NewForbidden(resource, "job", errors.New("denied")), false},
		"invalid":            {errors.New("invalid migration image"), false},
	} {
		t.Run(name, func(t *testing.T) {
			if got := retryableUpgradeError(tc.err); got != tc.want {
				t.Fatalf("retryableUpgradeError(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

func TestRuntimeConfigEditRollsWithoutMaintenance(t *testing.T) {
	r, cfg, c := lifecyclePlatform(t)
	driveLifecycle(t, r, cfg, c, "", func() bool { return cfg.Status.Phase == cost.PhaseReady && cfg.Status.Upgrade.Installed != nil })
	receipt := getMigrationJob(t, c, cfg.Namespace, resources.NameKokuMigration(cfg)).UID
	cfg.Spec.CostManagement.DataRetentionMonths = 9
	cfg.Spec.CostManagement.ReportDownloadSchedule = "*/30 * * * *"
	cfg.Generation++
	if _, err := r.reconcile(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.Status.Upgrade.Operation != nil {
		t.Fatalf("runtime-only edit planned a %s operation", cfg.Status.Upgrade.Operation.Stage)
	}
	driveLifecycle(t, r, cfg, c, "", func() bool { return cfg.Status.Phase == cost.PhaseReady })
	if getMigrationJob(t, c, cfg.Namespace, resources.NameKokuMigration(cfg)).UID != receipt {
		t.Fatal("runtime-only edit reran the Koku migration")
	}
	api := &appsv1.Deployment{}
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: cfg.Namespace, Name: resources.NameKokuAPI(cfg)}, api); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, e := range api.Spec.Template.Spec.Containers[0].Env {
		found = found || (e.Name == "RETAIN_NUM_MONTHS" && e.Value == "9")
	}
	if !found {
		t.Fatal("runtime-only edit was not rolled out to application workloads")
	}
}
