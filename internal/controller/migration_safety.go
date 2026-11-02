package controller

import (
	"context"
	"fmt"
	"slices"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	costv1alpha1 "github.com/project-koku/koku-service-operator/api/v1alpha1"
	opmetrics "github.com/project-koku/koku-service-operator/internal/metrics"
	"github.com/project-koku/koku-service-operator/internal/resources"
)

const (
	migrationJobKind      = "Job"
	reasonMigrationFailed = "MigrationFailed"
)

// guardMigrationPipeline checks every known step before changing any of them.
// In particular, a new first-step image cannot race a later-step migration
// from the previous target, and disabling ROS cannot hide a running ROS Job.
func (r *CostManagementServiceConfigReconciler) guardMigrationPipeline(
	ctx context.Context, cfg *costv1alpha1.CostManagementServiceConfig,
) (Result, error) {
	names := migrationNames(cfg)
	reader := r.safetyReader()
	waiting := ""
	for _, name := range names {
		job := &batchv1.Job{}
		err := reader.Get(ctx, types.NamespacedName{Namespace: cfg.Namespace, Name: name}, job)
		if errors.IsNotFound(err) {
			opmetrics.SetMigrationJobFailed(cfg.Namespace, cfg.Name, name, false)
			continue
		}
		if err != nil {
			return Result{}, fmt.Errorf("inspect migration %s: %w", name, err)
		}
		if isJobFailed(job) {
			return r.migrationFailed(cfg, job), nil
		}
		opmetrics.SetMigrationJobFailed(cfg.Namespace, cfg.Name, name, false)
		if !job.DeletionTimestamp.IsZero() || !isJobComplete(job) {
			waiting = name
		}
	}
	if waiting != "" {
		return r.waitForMigration(cfg, "waiting for existing migration Job "+waiting+" to finish before accepting target changes"), nil
	}

	// Background deletion, including manual deletion, can remove the Job before
	// its Pods stop. Select this instance's managed Pods and inspect Job owners;
	// do not assume that absence of a Job proves absence of a database writer.
	pods := &corev1.PodList{}
	labels := resources.Labels(cfg, "")
	delete(labels, "app.kubernetes.io/component")
	if err := reader.List(ctx, pods, client.InNamespace(cfg.Namespace), client.MatchingLabels(labels)); err != nil {
		return Result{}, fmt.Errorf("inspect migration pods: %w", err)
	}
	for i := range pods.Items {
		pod := &pods.Items[i]
		if pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed {
			continue
		}
		owner := metav1.GetControllerOf(pod)
		if owner == nil || owner.Kind != migrationJobKind || owner.APIVersion != batchv1.SchemeGroupVersion.String() {
			continue
		}
		if slices.Contains(names, owner.Name) {
			return r.waitForMigration(cfg, "waiting for migration Pod "+pod.Name+" to terminate before replacing a Job"), nil
		}
	}
	return Result{}, nil
}

func (r *CostManagementServiceConfigReconciler) waitForMigration(
	cfg *costv1alpha1.CostManagementServiceConfig, message string,
) Result {
	r.setCondition(cfg, costv1alpha1.ConditionSchemaUpToDate, metav1.ConditionFalse,
		"MigrationRunning", message)
	return Result{RequeueAfter: requeueFast}
}

// migrationFailed holds the whole pipeline even if the requested image changed.
// A failed Job may have committed earlier migrations: recovery is an explicit
// operator action, not an automatic retry with a different image.
func (r *CostManagementServiceConfigReconciler) migrationFailed(
	cfg *costv1alpha1.CostManagementServiceConfig, job *batchv1.Job,
) Result {
	// Persist the UID in the condition so restart preserves deduplication for
	// this execution, while a recovery Job with the same name reports anew.
	jobIdentity := fmt.Sprintf("%s (UID %s)", job.Name, job.UID)
	msg := fmt.Sprintf("%s exhausted retries — inspect pod logs and database state before recovery", jobIdentity)
	previous := apimeta.FindStatusCondition(cfg.Status.Conditions, costv1alpha1.ConditionUpgradeFailed)
	newFailure := previous == nil || previous.Status != metav1.ConditionTrue || previous.Message != msg
	for _, condition := range []string{
		costv1alpha1.ConditionSchemaUpToDate, costv1alpha1.ConditionAvailable, costv1alpha1.ConditionProgressing,
	} {
		r.setCondition(cfg, condition, metav1.ConditionFalse, reasonMigrationFailed, msg)
	}
	r.setCondition(cfg, costv1alpha1.ConditionDegraded, metav1.ConditionTrue, reasonMigrationFailed, msg)
	r.setCondition(cfg, costv1alpha1.ConditionUpgradeFailed, metav1.ConditionTrue, reasonMigrationFailed, msg)
	cfg.Status.Phase = costv1alpha1.PhaseDegraded
	opmetrics.SetMigrationJobFailed(cfg.Namespace, cfg.Name, job.Name, true)
	if newFailure && r.Recorder != nil {
		r.Recorder.Eventf(cfg, corev1.EventTypeWarning, reasonMigrationFailed,
			"Migration job %s exhausted retries — manual intervention required", jobIdentity)
		// During a journaled upgrade, failUpgrade emits UpgradeFailed with this message.
		if cfg.Status.Upgrade.Operation == nil {
			r.Recorder.Eventf(cfg, corev1.EventTypeWarning, "UpgradeFailed",
				"Application upgrade blocked by migration job %s; inspect database state before recovery", jobIdentity)
		}
	}
	return Result{Stop: true}
}
