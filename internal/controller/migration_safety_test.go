package controller

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	costv1alpha1 "github.com/project-koku/koku-service-operator/api/v1alpha1"
	opmetrics "github.com/project-koku/koku-service-operator/internal/metrics"
	"github.com/project-koku/koku-service-operator/internal/resources"
)

func TestMigrationUpgrade_FullImageReference(t *testing.T) {
	for _, test := range []struct {
		name   string
		change func(*costv1alpha1.ImageSpec)
	}{
		{"repository with unchanged tag", func(img *costv1alpha1.ImageSpec) { img.Repository = "quay.io/new/koku" }},
		{"digest without a tag", func(img *costv1alpha1.ImageSpec) { img.Tag = ""; img.Digest = "sha256:" + strings.Repeat("a", 64) }},
	} {
		t.Run(test.name, func(t *testing.T) {
			r, cfg, c := newMigrationTestReconciler(t)
			if _, err := r.reconcileMigration(context.Background(), cfg); err != nil {
				t.Fatal(err)
			}
			name := resources.NameKokuMigration(cfg)
			markJobComplete(t, c, cfg.Namespace, name)
			test.change(&cfg.Spec.CostManagement.API.Image)
			if _, err := r.reconcileMigration(context.Background(), cfg); err != nil {
				t.Fatal(err)
			}
			if jobExists(c, cfg.Namespace, name) {
				t.Fatal("completed Job must be replaced for a changed full image reference")
			}
			if _, err := r.reconcileMigration(context.Background(), cfg); err != nil {
				t.Fatal(err)
			}
			job := &batchv1.Job{}
			if err := c.Get(context.Background(), types.NamespacedName{Namespace: cfg.Namespace, Name: name}, job); err != nil {
				t.Fatal(err)
			}
			want, _ := resources.KokuImage(cfg)
			if job.Spec.Template.Spec.Containers[0].Image != want {
				t.Fatalf("replacement image = %q, want %q", job.Spec.Template.Spec.Containers[0].Image, want)
			}
		})
	}
}

func TestMigrationUpgrade_PreservesRunningJobAcrossTargetEdit(t *testing.T) {
	r, cfg, c := newMigrationTestReconciler(t)
	if _, err := r.reconcileMigration(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	name := resources.NameKokuMigration(cfg)
	original := getMigrationJob(t, c, cfg.Namespace, name)
	cfg.Spec.CostManagement.API.Image.Tag = "new-target"
	result, err := r.reconcileMigration(context.Background(), cfg)
	if err != nil || result.RequeueAfter == 0 {
		t.Fatalf("running migration must requeue without error: %+v, %v", result, err)
	}
	current := getMigrationJob(t, c, cfg.Namespace, name)
	if current.Spec.Template.Spec.Containers[0].Image != original.Spec.Template.Spec.Containers[0].Image {
		t.Fatal("target edit replaced an active migration")
	}
	if countJobs(c, cfg.Namespace) != 1 || !current.DeletionTimestamp.IsZero() {
		t.Fatal("active migration must be retained without starting another step")
	}
}

func TestMigrationUpgrade_DoesNotOverlapLaterStep(t *testing.T) {
	r, cfg, c := newMigrationTestReconciler(t)
	if _, err := r.reconcileMigration(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	name := resources.NameKokuMigration(cfg)
	markJobComplete(t, c, cfg.Namespace, name)
	if _, err := r.reconcileMigration(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	original := getMigrationJob(t, c, cfg.Namespace, name)
	cfg.Spec.CostManagement.API.Image.Tag = "new-target"
	if _, err := r.reconcileMigration(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	current := getMigrationJob(t, c, cfg.Namespace, name)
	if current.ResourceVersion != original.ResourceVersion || !current.DeletionTimestamp.IsZero() {
		t.Fatal("a new first-step target must wait for the existing RBAC migration")
	}
	markJobComplete(t, c, cfg.Namespace, resources.NameRBACMigration(cfg))
	if _, err := r.reconcileMigration(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	if jobExists(c, cfg.Namespace, name) {
		t.Fatal("new target should proceed after all previous steps terminate")
	}
}

func TestMigrationUpgrade_FailureSurvivesImageEdit(t *testing.T) {
	r, cfg, c := newMigrationTestReconciler(t)
	recorder := record.NewFakeRecorder(16)
	r.Recorder = recorder
	if _, err := r.reconcileMigration(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	name := resources.NameKokuMigration(cfg)
	markJobFailed(t, c, cfg.Namespace, name)
	cfg.Spec.CostManagement.API.Image.Tag = "different-target"
	for range 2 {
		result, err := r.reconcileMigration(context.Background(), cfg)
		if err != nil || !result.Stop {
			t.Fatalf("failure must hold the pipeline: %+v, %v", result, err)
		}
	}
	if !isJobFailed(getMigrationJob(t, c, cfg.Namespace, name)) || countJobs(c, cfg.Namespace) != 1 {
		t.Fatal("image edit erased failed migration evidence or started another migration")
	}
	condition := apimeta.FindStatusCondition(cfg.Status.Conditions, costv1alpha1.ConditionUpgradeFailed)
	if condition == nil || condition.Status != metav1.ConditionTrue || !strings.Contains(condition.Message, name) {
		t.Fatalf("UpgradeFailed must identify the failed Job: %+v", condition)
	}
	close(recorder.Events)
	failures := 0
	for event := range recorder.Events {
		if strings.Contains(event, " UpgradeFailed ") {
			failures++
		}
	}
	if failures != 1 {
		t.Fatalf("UpgradeFailed transition events = %d, want one", failures)
	}
}

func TestMigrationUpgrade_FailureEventsPerJobUID(t *testing.T) {
	for _, gate := range []string{"pipeline", "step"} {
		t.Run(gate, func(t *testing.T) {
			r, cfg, _ := newMigrationTestReconciler(t)
			c := fake.NewClientBuilder().WithScheme(r.Scheme).WithObjects(cfg).
				WithStatusSubresource(cfg, &batchv1.Job{}).Build()
			recorder := record.NewFakeRecorder(16)
			desired := resources.MigrationJob(cfg, cfg.Spec.CostManagement.API.Image.Tag)
			for _, uid := range []types.UID{"first-attempt", "recovery-attempt"} {
				job := desired.DeepCopy()
				job.UID = uid
				if err := c.Create(context.Background(), job); err != nil {
					t.Fatal(err)
				}
				markJobFailed(t, c, cfg.Namespace, job.Name)
				for range 2 {
					// Rebuild the reconciler and reload persisted status each time:
					// deduplication must survive restart without hiding a new UID.
					restored := &costv1alpha1.CostManagementServiceConfig{}
					if err := c.Get(context.Background(), client.ObjectKeyFromObject(cfg), restored); err != nil {
						t.Fatal(err)
					}
					cfg = restored
					r = &CostManagementServiceConfigReconciler{
						Client: c, APIReader: c, Scheme: r.Scheme, Recorder: recorder,
					}
					var result Result
					var err error
					if gate == "pipeline" {
						result, err = r.reconcileMigration(context.Background(), cfg)
					} else {
						result, err = r.runMigrationStep(context.Background(), cfg, desired.DeepCopy(), 1, 2)
					}
					if err != nil || !result.Stop {
						t.Fatalf("failed migration must stop the pipeline: %+v, %v", result, err)
					}
					if err := c.Status().Update(context.Background(), cfg); err != nil {
						t.Fatal(err)
					}
				}
				counts := map[string]int{"MigrationFailed": 0, "UpgradeFailed": 0}
				for len(recorder.Events) > 0 {
					event := <-recorder.Events
					for reason := range counts {
						if strings.Contains(event, " "+reason+" ") {
							counts[reason]++
						}
					}
					if !strings.Contains(event, string(uid)) {
						t.Errorf("failure Event must identify Job UID %s: %s", uid, event)
					}
				}
				for reason, count := range counts {
					if count != 1 {
						t.Errorf("Job UID %s emitted %d %s Events, want one", uid, count, reason)
					}
				}
				condition := apimeta.FindStatusCondition(cfg.Status.Conditions, costv1alpha1.ConditionUpgradeFailed)
				if condition == nil || condition.Status != metav1.ConditionTrue || !strings.Contains(condition.Message, string(uid)) {
					t.Errorf("UpgradeFailed must retain failed Job UID %s: %+v", uid, condition)
				}
				// Explicit recovery removes the failed Job, while the historical
				// failure condition remains until the whole migration pipeline succeeds.
				if err := c.Delete(context.Background(), job); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

func TestMigrationUpgrade_PreviousLaterFailureBlocksNewFirstStep(t *testing.T) {
	r, cfg, c := newMigrationTestReconciler(t)
	if _, err := r.reconcileMigration(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	name := resources.NameKokuMigration(cfg)
	markJobComplete(t, c, cfg.Namespace, name)
	if _, err := r.reconcileMigration(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	markJobFailed(t, c, cfg.Namespace, resources.NameRBACMigration(cfg))
	cfg.Spec.CostManagement.API.Image.Tag = "new-target"
	result, err := r.reconcileMigration(context.Background(), cfg)
	if err != nil || !result.Stop {
		t.Fatalf("previous later-step failure must hold a new target: %+v, %v", result, err)
	}
	if !isJobComplete(getMigrationJob(t, c, cfg.Namespace, name)) {
		t.Fatal("a new first step replaced its receipt before later-step failure recovery")
	}
}

func TestMigrationUpgrade_GuardsDisabledROSJob(t *testing.T) {
	r, cfg, c := newMigrationTestReconciler(t)
	job := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: resources.NameROSMigration(cfg), Namespace: cfg.Namespace}}
	if err := c.Create(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	result, err := r.reconcileMigration(context.Background(), cfg)
	if err != nil || result.RequeueAfter == 0 || jobExists(c, cfg.Namespace, resources.NameKokuMigration(cfg)) {
		t.Fatalf("disabled ROS must not hide a previous active migration: %+v, %v", result, err)
	}
}

func TestMigrationUpgrade_SurvivingPodBlocksReplacement(t *testing.T) {
	for _, phase := range []corev1.PodPhase{corev1.PodPending, corev1.PodRunning, corev1.PodUnknown, corev1.PodSucceeded, corev1.PodFailed} {
		t.Run(string(phase), func(t *testing.T) {
			r, cfg, c := newMigrationTestReconciler(t)
			pod := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name: "surviving-migration", Namespace: cfg.Namespace, Labels: resources.Labels(cfg, "rbac-migration"),
					OwnerReferences: []metav1.OwnerReference{{APIVersion: "batch/v1", Kind: "Job", Name: resources.NameRBACMigration(cfg), UID: "deleted-job", Controller: new(true)}},
				},
				Status: corev1.PodStatus{Phase: phase},
			}
			if err := c.Create(context.Background(), pod); err != nil {
				t.Fatal(err)
			}
			result, err := r.reconcileMigration(context.Background(), cfg)
			if err != nil || result.RequeueAfter == 0 {
				t.Fatalf("reconcile: %+v, %v", result, err)
			}
			terminal := phase == corev1.PodSucceeded || phase == corev1.PodFailed
			if jobExists(c, cfg.Namespace, resources.NameKokuMigration(cfg)) != terminal {
				t.Fatalf("a new migration may start only after the previous Pod is terminal (phase %s)", phase)
			}
		})
	}
}

func TestMigrationUpgrade_LegacyJobAdoption(t *testing.T) {
	r, cfg, c := newMigrationTestReconciler(t)
	job := resources.MigrationJob(cfg, cfg.Spec.CostManagement.API.Image.Tag)
	delete(job.Annotations, resources.MigrationExecutionAnnotation)
	if err := c.Create(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	markJobComplete(t, c, cfg.Namespace, job.Name)
	if _, err := r.reconcileMigration(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	if !isJobComplete(getMigrationJob(t, c, cfg.Namespace, job.Name)) || !jobExists(c, cfg.Namespace, resources.NameRBACMigration(cfg)) {
		t.Fatal("matching legacy Job must be adopted without replaying schema changes")
	}
}

func TestMigrationUpgrade_UsesForegroundDeletionWithUID(t *testing.T) {
	r, cfg, c := newMigrationTestReconciler(t)
	job := resources.MigrationJob(cfg, cfg.Spec.CostManagement.API.Image.Tag)
	job.UID = "previous-job-uid"
	job.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}}
	if err := c.Create(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	deleted := false
	r.Client = interceptor.NewClient(c.(client.WithWatch), interceptor.Funcs{
		Delete: func(_ context.Context, _ client.WithWatch, obj client.Object, options ...client.DeleteOption) error {
			deleted = true
			opts := &client.DeleteOptions{}
			opts.ApplyOptions(options)
			if opts.PropagationPolicy == nil || *opts.PropagationPolicy != metav1.DeletePropagationForeground ||
				opts.Preconditions == nil || opts.Preconditions.UID == nil || *opts.Preconditions.UID != obj.GetUID() {
				t.Fatalf("replacement must foreground-delete the observed UID: %+v", opts)
			}
			return nil
		},
	})
	cfg.Spec.CostManagement.API.Image.Repository = "quay.io/new/koku"
	if _, err := r.reconcileMigration(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	if !deleted {
		t.Fatal("expected old completed Job deletion")
	}
}

func TestMigrationGuard_APIReadErrorsFailClosed(t *testing.T) {
	for _, failList := range []bool{false, true} {
		t.Run(map[bool]string{false: "job get", true: "pod list"}[failList], func(t *testing.T) {
			r, cfg, _ := newMigrationTestReconciler(t)
			failure := errors.New("api unavailable")
			r.Client = fake.NewClientBuilder().WithScheme(ownershipScheme(t)).WithInterceptorFuncs(interceptor.Funcs{
				Get: func(_ context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, options ...client.GetOption) error {
					if !failList {
						return failure
					}
					return c.Get(context.Background(), key, obj, options...)
				},
				List: func(_ context.Context, _ client.WithWatch, _ client.ObjectList, _ ...client.ListOption) error {
					return failure
				},
			}).Build()
			if _, err := r.reconcileMigration(context.Background(), cfg); !errors.Is(err, failure) {
				t.Fatalf("API inspection failure must block migration: %v", err)
			}
		})
	}
}

func TestMigrationStep_ChangedScriptAndForgedReceipt(t *testing.T) {
	r, cfg, c := newMigrationTestReconciler(t)
	desired := resources.MigrationJob(cfg, cfg.Spec.CostManagement.API.Image.Tag)
	previous := desired.DeepCopy()
	previous.Spec.Template.Spec.Containers[0].Command[2] = "echo old-migration-script"
	// Keep the desired annotation to prove that the actual template, rather
	// than an annotation copied onto the old execution, decides compatibility.
	previous.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}}
	if err := c.Create(context.Background(), previous); err != nil {
		t.Fatal(err)
	}
	if _, err := r.runMigrationStep(context.Background(), cfg, desired, 1, 2); err != nil {
		t.Fatal(err)
	}
	if jobExists(c, cfg.Namespace, previous.Name) {
		t.Fatal("new operator migration script must invalidate the old execution")
	}
}

func TestMigrationGuard_DoesNotBlockUnrelatedPod(t *testing.T) {
	r, cfg, c := newMigrationTestReconciler(t)
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name: "unrelated", Namespace: cfg.Namespace, Labels: resources.Labels(cfg, "rbac-migration"),
		OwnerReferences: []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: "ReplicaSet", Name: "application", UID: "app-uid", Controller: new(true)}},
	}}
	if err := c.Create(context.Background(), pod); err != nil {
		t.Fatal(err)
	}
	if _, err := r.reconcileMigration(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	if !jobExists(c, cfg.Namespace, resources.NameKokuMigration(cfg)) {
		t.Fatal("unrelated workload Pods must not block migrations")
	}
}

func getMigrationJob(t *testing.T, c client.Client, namespace, name string) *batchv1.Job {
	t.Helper()
	job := &batchv1.Job{}
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: namespace, Name: name}, job); err != nil {
		t.Fatal(err)
	}
	return job
}

// A controller read can race a Job update after the pipeline guard. The
// per-step gate must independently preserve the unfinished/failing execution.
func TestMigrationStep_StateChangeAfterPipelineGuard(t *testing.T) {
	for _, state := range []string{"running", "failed", "deleting"} {
		t.Run(state, func(t *testing.T) {
			r, cfg, _ := newMigrationTestReconciler(t)
			desired := resources.MigrationJob(cfg, cfg.Spec.CostManagement.API.Image.Tag)
			previous := desired.DeepCopy()
			previous.Spec.Template.Spec.Containers[0].Image = "quay.io/previous/koku:v0"
			if state == "failed" {
				previous.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobFailed, Status: corev1.ConditionTrue}}
			}
			if state == "deleting" {
				previous.DeletionTimestamp = new(metav1.Now())
				previous.Finalizers = []string{metav1.FinalizerDeleteDependents}
			}
			r.Client = fake.NewClientBuilder().WithScheme(ownershipScheme(t)).WithObjects(previous).Build()
			result, err := r.runMigrationStep(context.Background(), cfg, desired, 1, 2)
			if err != nil || result.IsZero() {
				t.Fatalf("changed state must stop progression: %+v, %v", result, err)
			}
			current := getMigrationJob(t, r.Client, cfg.Namespace, previous.Name)
			if current.Spec.Template.Spec.Containers[0].Image != previous.Spec.Template.Spec.Containers[0].Image {
				t.Fatal("raced Job state was overwritten")
			}
		})
	}
}

func TestMigrationStep_APIWriteErrors(t *testing.T) {
	for _, operation := range []string{"get", "create", "delete"} {
		t.Run(operation, func(t *testing.T) {
			r, cfg, _ := newMigrationTestReconciler(t)
			desired := resources.MigrationJob(cfg, cfg.Spec.CostManagement.API.Image.Tag)
			previous := desired.DeepCopy()
			previous.Spec.Template.Spec.Containers[0].Image = "quay.io/previous/koku:v0"
			previous.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}}
			failure := errors.New("api write unavailable")
			builder := fake.NewClientBuilder().WithScheme(ownershipScheme(t))
			if operation == "delete" {
				builder = builder.WithObjects(previous)
			}
			r.Client = builder.WithInterceptorFuncs(interceptor.Funcs{
				Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
					if operation == "get" {
						return failure
					}
					return c.Get(ctx, key, obj, opts...)
				},
				Create: func(context.Context, client.WithWatch, client.Object, ...client.CreateOption) error { return failure },
				Delete: func(context.Context, client.WithWatch, client.Object, ...client.DeleteOption) error { return failure },
			}).Build()
			if _, err := r.runMigrationStep(context.Background(), cfg, desired, 1, 2); !errors.Is(err, failure) {
				t.Fatalf("%s error must hold the migration gate: %v", operation, err)
			}
		})
	}
}

func TestMigrationGuard_UncachedReaderSeesWriters(t *testing.T) {
	for _, writer := range []string{"job", "pod"} {
		t.Run(writer, func(t *testing.T) {
			r, cfg, cached := newMigrationTestReconciler(t)
			live := fake.NewClientBuilder().WithScheme(ownershipScheme(t))
			if writer == "job" {
				live = live.WithObjects(resources.MigrationJob(cfg, cfg.Spec.CostManagement.API.Image.Tag))
			} else {
				live = live.WithObjects(&corev1.Pod{ObjectMeta: metav1.ObjectMeta{
					Name: "live-migration", Namespace: cfg.Namespace, Labels: resources.Labels(cfg, "rbac-migration"),
					OwnerReferences: []metav1.OwnerReference{{APIVersion: "batch/v1", Kind: "Job", Name: resources.NameRBACMigration(cfg), UID: "old-job", Controller: new(true)}},
				}})
			}
			r.APIReader = live.Build()
			result, err := r.reconcileMigration(context.Background(), cfg)
			if err != nil || result.RequeueAfter == 0 || countJobs(cached, cfg.Namespace) != 0 {
				t.Fatalf("empty informer cache must not bypass a live writer: %+v, %v", result, err)
			}
		})
	}
}

func TestMigrationStep_UncachedReaderAdoptsCompletedExecution(t *testing.T) {
	r, cfg, cached := newMigrationTestReconciler(t)
	desired := resources.MigrationJob(cfg, cfg.Spec.CostManagement.API.Image.Tag)
	live := desired.DeepCopy()
	live.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}}
	r.APIReader = fake.NewClientBuilder().WithScheme(ownershipScheme(t)).WithObjects(live).Build()
	result, err := r.runMigrationStep(context.Background(), cfg, desired, 1, 2)
	if err != nil || !result.IsZero() || countJobs(cached, cfg.Namespace) != 0 {
		t.Fatalf("a stale cache must not replay a completed live migration: %+v, %v", result, err)
	}
}

func TestMigrationUpgrade_ExplicitRecoveryClearsFailure(t *testing.T) {
	r, cfg, c := newMigrationTestReconciler(t)
	name := resources.NameROSMigration(cfg)
	failed := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: cfg.Namespace},
		Status: batchv1.JobStatus{Conditions: []batchv1.JobCondition{{Type: batchv1.JobFailed, Status: corev1.ConditionTrue}}}}
	if err := c.Create(context.Background(), failed); err != nil {
		t.Fatal(err)
	}
	if _, err := r.reconcileMigration(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	if !apimeta.IsStatusConditionTrue(cfg.Status.Conditions, costv1alpha1.ConditionUpgradeFailed) {
		t.Fatal("failed previous ROS execution must be reported even after ROS is disabled")
	}
	// Simulate explicit recovery after inspecting database state. Production
	// recovery must also stop old writers; this fake has no surviving Pods.
	if err := c.Delete(context.Background(), failed); err != nil {
		t.Fatal(err)
	}
	for _, next := range []string{resources.NameKokuMigration(cfg), resources.NameRBACMigration(cfg)} {
		if _, err := r.reconcileMigration(context.Background(), cfg); err != nil {
			t.Fatal(err)
		}
		markJobComplete(t, c, cfg.Namespace, next)
	}
	if _, err := r.reconcileMigration(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	if !apimeta.IsStatusConditionFalse(cfg.Status.Conditions, costv1alpha1.ConditionUpgradeFailed) {
		t.Fatal("successful explicit recovery must clear UpgradeFailed")
	}
	if got := testutil.ToFloat64(opmetrics.MigrationJobFailed.WithLabelValues(cfg.Namespace, cfg.Name, name)); got != 0 {
		t.Fatalf("removed disabled-step failure metric = %v, want 0", got)
	}
}
