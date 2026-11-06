package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"reflect"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilnet "k8s.io/apimachinery/pkg/util/net"
	"k8s.io/apimachinery/pkg/util/uuid"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	cost "github.com/project-koku/koku-service-operator/api/v1alpha1"
	"github.com/project-koku/koku-service-operator/internal/release"
	"github.com/project-koku/koku-service-operator/internal/resources"
)

// reconcile preserves the requested spec. All default resolution, frozen-plan
// execution and rollback operate on a copy; only status is returned to Reconcile.
func (r *CostManagementServiceConfigReconciler) reconcile(ctx context.Context, requested *cost.CostManagementServiceConfig) (ctrl.Result, error) {
	cfg := requested.DeepCopy()
	defer func() { requested.Status = cfg.Status }()
	bundle := release.Bundle(r.ReleaseImages)
	if bundle == nil {
		var err error
		bundle, err = release.Current()
		if err != nil {
			return r.rejectUpgradeTarget(ctx, cfg, err)
		}
	}
	resolveErr := bundle.Resolve(cfg)
	op := cfg.Status.Upgrade.Operation
	if op != nil {
		return r.advanceUpgrade(ctx, cfg, requested, bundle, resolveErr)
	}
	if resolveErr != nil {
		return r.rejectUpgradeTarget(ctx, cfg, resolveErr)
	}
	if len(resources.MissingWorkloadImages(cfg)) != 0 {
		return r.reconcileStack(ctx, cfg, true)
	}
	if cfg.Status.Upgrade.Installed == nil {
		return r.reconcileInitial(ctx, cfg, bundle)
	}
	// Resolve discovery before freezing a target; no target resources are applied.
	if result, err := r.reconcileDiscovery(ctx, cfg); err != nil || !result.IsZero() {
		return ctrl.Result{RequeueAfter: result.RequeueAfter}, err
	}
	target := applicationRevision(cfg, bundle)
	if err := bundle.ValidateTransition(requested, cfg.Status.Upgrade.Installed, target.Images); err != nil {
		return r.rejectUpgradeTarget(ctx, cfg, err)
	}
	needsMigration, err := r.migrationExecutionNeeded(ctx, cfg, target)
	if err != nil {
		return ctrl.Result{}, err
	}
	if target.ID == cfg.Status.Upgrade.Installed.ID && !needsMigration {
		result, err := r.reconcileStack(ctx, cfg, true, stackOptions{discoveryAlreadyRun: true})
		if err == nil && cfg.Status.Phase == cost.PhaseReady {
			cfg.Status.Upgrade.Installed = applicationRevision(cfg, bundle)
		}
		return result, err
	}
	stage := cost.UpgradeStageRollingOut
	if needsMigration {
		stage = cost.UpgradeStageQuiescing
		r.setCondition(cfg, cost.ConditionSchemaUpToDate, metav1.ConditionFalse, "MigrationPending", "target schema execution is pending")
	}
	cfg.Status.Upgrade.Operation = &cost.UpgradeOperation{
		ID: string(uuid.NewUUID()), TargetGeneration: cfg.Generation,
		Source: cfg.Status.Upgrade.Installed.DeepCopy(), Target: *target,
		Stage: stage, StartedAt: metav1.Now(), StageStartedAt: metav1.Now(),
	}
	r.setCondition(cfg, cost.ConditionProgressing, metav1.ConditionTrue, "UpgradePlanned", "application upgrade plan persisted before execution")
	cfg.Status.Phase = cost.PhaseProgressing
	// Returning before any child write is essential: a failed outer status patch
	// must never leave an unjournaled mutation or migration behind.
	return ctrl.Result{RequeueAfter: requeueFast}, nil
}

// rejectUpgradeTarget keeps reconciling the last installed application spec
// when a new target is invalid. The requested CMSC is preserved; its failure is
// reported after the serving revision has received ordinary drift correction.
func (r *CostManagementServiceConfigReconciler) rejectUpgradeTarget(
	ctx context.Context, cfg *cost.CostManagementServiceConfig, cause error,
) (ctrl.Result, error) {
	installed := cfg.Status.Upgrade.Installed
	if installed == nil {
		return r.upgradeConfigError(cfg, cause)
	}
	var source cost.CostManagementServiceConfigSpec
	if err := json.Unmarshal(installed.Spec.Raw, &source); err != nil {
		return r.upgradeConfigError(cfg, fmt.Errorf("cannot reconcile installed revision after rejecting target: %w", err))
	}
	requested := cfg.Spec
	cfg.Spec = source
	cfg.Status.DiscoveredConfig = installed.DiscoveredConfig.DeepCopy()
	result, err := r.reconcileStack(ctx, cfg, false, stackOptions{preserveUpgradeFailure: true})
	cfg.Spec = requested
	if err != nil {
		return result, err
	}
	blockedResult, blockedErr := r.upgradeConfigError(cfg, cause)
	if result.RequeueAfter > 0 {
		blockedResult.RequeueAfter = result.RequeueAfter
	}
	return blockedResult, blockedErr
}

func applicationRevision(cfg *cost.CostManagementServiceConfig, bundle release.Bundle) *cost.ApplicationRevision {
	images := map[string]string{}
	for key, image := range release.ImageFields(cfg) {
		if (key == "ros" || key == "kruize") && !cost.ROSEnabled(cfg) {
			continue
		}
		images[key], _ = resources.ImageRef(*image)
	}
	masu, ok := resources.ImageRef(cfg.Spec.CostManagement.Masu.Image)
	if !ok {
		masu = images["koku"]
	}
	images["masu"] = masu
	migrations := map[string]string{}
	for _, job := range migrationJobs(cfg) {
		migrations[job.Name] = resources.MigrationExecutionID(job)
	}
	identity, _ := json.Marshal(struct{ Images, Migrations map[string]string }{images, migrations})
	sum := sha256.Sum256(identity)
	spec, _ := json.Marshal(cfg.Spec)
	var discovered *cost.DiscoveredConfig
	if cfg.Status.DiscoveredConfig != nil {
		discovered = cfg.Status.DiscoveredConfig.DeepCopy()
	}
	return &cost.ApplicationRevision{
		ID: hex.EncodeToString(sum[:]), ReleaseID: bundle.ID(), ReleaseManaged: bundle.Matches(cfg),
		Images: images, Migrations: migrations, Spec: runtime.RawExtension{Raw: spec}, DiscoveredConfig: discovered,
	}
}

// migrationJobs is the ordered migration pipeline: Koku, optional ROS, then RBAC.
func migrationJobs(cfg *cost.CostManagementServiceConfig) []*batchv1.Job {
	jobs := []*batchv1.Job{resources.MigrationJob(cfg, cfg.Spec.CostManagement.API.Image.Tag)}
	if cost.ROSEnabled(cfg) {
		jobs = append(jobs, resources.ROSMigrationJob(cfg, cfg.Spec.ROS.Image.Tag))
	}
	return append(jobs, resources.RBACMigrationJob(cfg, cfg.Spec.RBAC.Image.Tag))
}

type legacyBaselineCheck struct {
	Matches              bool
	MissingMigrationJobs []string
}

func (r *CostManagementServiceConfigReconciler) migrationExecutionNeeded(ctx context.Context, cfg *cost.CostManagementServiceConfig, target *cost.ApplicationRevision) (bool, error) {
	if !reflect.DeepEqual(cfg.Status.Upgrade.Installed.Migrations, target.Migrations) {
		return true, nil
	}
	return r.migrationReceiptRefreshNeeded(ctx, cfg)
}

func (r *CostManagementServiceConfigReconciler) migrationReceiptRefreshNeeded(ctx context.Context, cfg *cost.CostManagementServiceConfig) (bool, error) {
	for _, desired := range migrationJobs(cfg) {
		existing := &batchv1.Job{}
		err := r.safetyReader().Get(ctx, client.ObjectKeyFromObject(desired), existing)
		if apierrors.IsNotFound(err) {
			return true, nil
		}
		if err != nil {
			return false, err
		}
		if !existing.DeletionTimestamp.IsZero() || !isJobComplete(existing) || resources.MigrationExecutionID(existing) != resources.MigrationExecutionID(desired) {
			return true, nil
		}
	}
	return false, nil
}

func (r *CostManagementServiceConfigReconciler) advanceUpgrade(ctx context.Context, cfg *cost.CostManagementServiceConfig, requested *cost.CostManagementServiceConfig, bundle release.Bundle, resolveErr error) (ctrl.Result, error) {
	op := cfg.Status.Upgrade.Operation
	handled, err := r.acceptUpgradeRecovery(ctx, cfg, requested, bundle, resolveErr)
	if err != nil {
		if _, monitorErr := r.reconcileMonitoring(ctx, cfg); monitorErr != nil {
			return ctrl.Result{}, monitorErr
		}
		return r.upgradeConfigError(cfg, err)
	}
	if handled {
		// A recovery request is a separate short pass before its new frozen plan
		// executes. Keep monitoring in sync once, using the accepted request.
		if _, err := r.reconcileMonitoring(ctx, cfg); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: requeueFast}, nil
	}
	if op.Stage == cost.UpgradeStageRollingBack || op.Stage == cost.UpgradeStageResumingPrevious || op.Stage == cost.UpgradeStageRecoveryRequired || op.Stage == cost.UpgradeStageRolledBack {
		cfg.Generation = op.TargetGeneration
		return r.rollbackUpgrade(ctx, cfg)
	}
	return r.advanceFrozenUpgrade(ctx, cfg, requested.Spec)
}

func (r *CostManagementServiceConfigReconciler) advanceFrozenUpgrade(
	ctx context.Context, cfg *cost.CostManagementServiceConfig, requestedSpec cost.CostManagementServiceConfigSpec,
) (ctrl.Result, error) {
	op := cfg.Status.Upgrade.Operation
	// Work with the frozen target/source, never a mid-flight requested spec.
	var targetSpec cost.CostManagementServiceConfigSpec
	if err := json.Unmarshal(op.Target.Spec.Raw, &targetSpec); err != nil {
		return r.upgradeConfigError(cfg, fmt.Errorf("invalid frozen upgrade target: %w", err))
	}
	cfg.Spec = targetSpec
	defer func() { cfg.Spec = requestedSpec }()
	cfg.Generation = op.TargetGeneration
	cfg.Status.DiscoveredConfig = op.Target.DiscoveredConfig.DeepCopy()
	if missing := resources.MissingWorkloadImages(cfg); len(missing) > 0 {
		return r.upgradeConfigError(cfg, fmt.Errorf("frozen target has incomplete images: %v", missing))
	}
	if (op.Stage == cost.UpgradeStageQuiescing || op.Stage == cost.UpgradeStageMigrating) && frozenMigrationInputsChanged(cfg, op.Target.Migrations) {
		return r.failUpgrade(cfg, "operator migration inputs changed during this operation; use a compatible operator or explicit recovery"), nil
	}
	// The normal rollout stack owns monitoring in RollingOut. Earlier stages do
	// not run that stack, so reconcile the frozen target exactly once here.
	if op.Stage != cost.UpgradeStageRollingOut {
		if _, err := r.reconcileMonitoring(ctx, cfg); err != nil {
			return ctrl.Result{}, err
		}
	}

	timeout := upgradeTimeoutSeconds(cfg, op)
	if upgradeExpired(op, timeout) {
		return r.failUpgrade(cfg, "upgrade exceeded its execution deadline"), nil
	}
	switch op.Stage {
	case cost.UpgradeStageQuiescing:
		return r.quiesceUpgrade(ctx, cfg)
	case cost.UpgradeStageMigrating:
		return r.migrateUpgrade(ctx, cfg)
	case cost.UpgradeStageRollingOut:
		return r.rolloutUpgrade(ctx, cfg), nil
	case cost.UpgradeStageRecovering:
		return r.recoverUpgrade(ctx, cfg)
	default:
		return r.upgradeConfigError(cfg, fmt.Errorf("unknown upgrade stage %q; refusing mutation", op.Stage))
	}
}

func (r *CostManagementServiceConfigReconciler) upgradeStage(op *cost.UpgradeOperation, stage string) {
	op.Stage = stage
	op.StageStartedAt = metav1.Now()
}
func (r *CostManagementServiceConfigReconciler) failUpgrade(cfg *cost.CostManagementServiceConfig, message string) ctrl.Result {
	op := cfg.Status.Upgrade.Operation
	// The migration phase may set UpgradeFailed before this method emits its
	// event, so deduplicate against the persisted operation transition instead.
	newFailure := op.Stage != cost.UpgradeStageRollingBack || op.Failure != message
	op.Failure = message
	r.upgradeStage(op, cost.UpgradeStageRollingBack)
	r.setCondition(cfg, cost.ConditionUpgradeFailed, metav1.ConditionTrue, "UpgradeFailed", message)
	availableMessage := "restoring previous application images"
	if op.SchemaMutationPossible {
		availableMessage = "stopping failed target workloads while database recovery is assessed"
	}
	r.setCondition(cfg, cost.ConditionAvailable, metav1.ConditionFalse, cost.UpgradeStageRollingBack, availableMessage)
	if newFailure && r.Recorder != nil {
		r.Recorder.Event(cfg, corev1.EventTypeWarning, "UpgradeFailed", message)
	}
	return ctrl.Result{RequeueAfter: requeueFast}
}

func (r *CostManagementServiceConfigReconciler) rollbackUpgrade(ctx context.Context, cfg *cost.CostManagementServiceConfig) (ctrl.Result, error) {
	op := cfg.Status.Upgrade.Operation
	if op.Stage == cost.UpgradeStageRecoveryRequired && !op.SchemaMutationPossible {
		// A source rollout that timed out or became unhealthy is also a hold.
		// Do not turn RecoveryRequired into another automatic source rollout;
		// require a new, operation-bound Retry or ResumePrevious request.
		return ctrl.Result{RequeueAfter: requeueDrift}, nil
	}
	timeout := upgradeTimeoutSeconds(cfg, op)
	// RolledBack is a completed, serving hold. Ordinary drift correction must
	// keep retrying transient errors even after the restoration deadline passed.
	deadlineExceeded := op.Stage != cost.UpgradeStageRolledBack && time.Since(op.StageStartedAt.Time) > time.Duration(timeout)*time.Second
	requestedSpec := cfg.Spec
	defer func() { cfg.Spec = requestedSpec }()
	var target cost.CostManagementServiceConfigSpec
	if err := json.Unmarshal(op.Target.Spec.Raw, &target); err != nil {
		return r.holdInvalidUpgradeTarget(ctx, cfg, op, err, deadlineExceeded)
	}
	cfg.Spec = target
	cfg.Status.DiscoveredConfig = op.Target.DiscoveredConfig.DeepCopy()
	stopped, err := r.migrationWritersStopped(ctx, cfg)
	if err != nil {
		return ctrl.Result{}, err
	}
	if !stopped {
		return ctrl.Result{RequeueAfter: requeueFast}, nil
	}
	if op.SchemaMutationPossible {
		return r.holdFailedUpgradeTarget(ctx, cfg, op, deadlineExceeded)
	}
	return r.resumeSourceAfterSafeFailure(ctx, cfg, op, deadlineExceeded)
}

func (r *CostManagementServiceConfigReconciler) holdInvalidUpgradeTarget(
	ctx context.Context, cfg *cost.CostManagementServiceConfig, op *cost.UpgradeOperation, targetErr error, deadlineExceeded bool,
) (ctrl.Result, error) {
	names := make([]string, 0, len(op.Target.Migrations))
	for name := range op.Target.Migrations {
		names = append(names, name)
	}
	stopped, err := r.migrationWritersStoppedFor(ctx, cfg, names)
	if err != nil {
		return ctrl.Result{}, err
	}
	if !stopped {
		return ctrl.Result{RequeueAfter: requeueFast}, nil
	}
	quiet, err := r.quiesceApplications(ctx, cfg)
	if err != nil {
		return ctrl.Result{}, err
	}
	if !quiet && !deadlineExceeded {
		return ctrl.Result{RequeueAfter: requeueFast}, nil
	}
	return r.requireRecovery(cfg, fmt.Sprintf("frozen failed target is invalid (%v); workloads are held stopped and require manual recovery", targetErr)), nil
}

func (r *CostManagementServiceConfigReconciler) holdFailedUpgradeTarget(
	ctx context.Context, cfg *cost.CostManagementServiceConfig, op *cost.UpgradeOperation, deadlineExceeded bool,
) (ctrl.Result, error) {
	quiet, err := r.quiesceApplications(ctx, cfg)
	if err != nil {
		return ctrl.Result{}, err
	}
	missing := resources.MissingWorkloadImages(cfg)
	if len(missing) == 0 {
		if err := r.applyStoppedApplicationRevision(ctx, cfg); err != nil {
			return ctrl.Result{}, err
		}
	}
	if !quiet {
		if deadlineExceeded {
			return r.requireRecovery(cfg, "rollback deadline exceeded; target workloads are being stopped and writers must remain down until database recovery"), nil
		}
		return ctrl.Result{RequeueAfter: requeueFast}, nil
	}
	if len(missing) > 0 {
		return r.requireRecovery(cfg, fmt.Sprintf("frozen failed target has incomplete images: %v; repair the target before recovery", missing)), nil
	}
	if _, err := r.reconcileSharedConfig(ctx, cfg); err != nil {
		return ctrl.Result{}, err
	}
	if _, err := r.reconcileMonitoring(ctx, cfg); err != nil {
		return ctrl.Result{}, err
	}
	message := "failed target images are retained with workloads stopped; inspect/repair database state before resuming"
	r.requireRecovery(cfg, message)
	if op.Failure != "" {
		r.setCondition(cfg, cost.ConditionUpgradeFailed, metav1.ConditionTrue, "UpgradeFailed", op.Failure)
	}
	return ctrl.Result{RequeueAfter: requeueDrift}, nil
}

func (r *CostManagementServiceConfigReconciler) applyStoppedApplicationRevision(ctx context.Context, cfg *cost.CostManagementServiceConfig) error {
	for _, d := range resources.ApplicationDeployments(cfg) {
		d.Spec.Replicas = new(int32(0))
		if err := r.apply(ctx, cfg, d); err != nil {
			return err
		}
	}
	return nil
}

func (r *CostManagementServiceConfigReconciler) resumeSourceAfterSafeFailure(
	ctx context.Context, cfg *cost.CostManagementServiceConfig, op *cost.UpgradeOperation, deadlineExceeded bool,
) (ctrl.Result, error) {
	if op.RecoveryRequestID != "" && op.RecoveryAction != cost.RecoveryRetry && op.Stage == cost.UpgradeStageRollingBack {
		// A recovery action may arrive while old Pods or a scheduled writer are
		// still draining (for example, after the rollback deadline). Do not resume
		// the source until that explicit recovery path has completed quiescence.
		// Legacy journals without an action retain this conservative barrier.
		// A safe image-only Retry must not drain unchanged application writers.
		quiet, err := r.quiesceApplications(ctx, cfg)
		if err != nil {
			return ctrl.Result{}, err
		}
		if !quiet {
			return ctrl.Result{RequeueAfter: requeueFast}, nil
		}
		// Persist the completed drain barrier before applying the source revision.
		// Otherwise each reconcile would reapply the failed target at zero first,
		// continually resetting the source rollout's observed generation.
		r.upgradeStage(op, cost.UpgradeStageResumingPrevious)
		return ctrl.Result{RequeueAfter: requeueFast}, nil
	}
	if op.Source == nil {
		return r.upgradeConfigError(cfg, fmt.Errorf("failed upgrade has no installed source revision"))
	}
	var source cost.CostManagementServiceConfigSpec
	if err := json.Unmarshal(op.Source.Spec.Raw, &source); err != nil {
		return ctrl.Result{}, err
	}
	cfg.Spec = source
	cfg.Status.DiscoveredConfig = op.Source.DiscoveredConfig.DeepCopy()
	if missing := resources.MissingWorkloadImages(cfg); len(missing) > 0 {
		return r.requireRecovery(cfg, fmt.Sprintf("frozen source has incomplete images: %v; provide a runnable recovery target", missing)), nil
	}
	result, err := r.reconcileStack(ctx, cfg, false)
	if err != nil {
		if deadlineExceeded && !errors.Is(err, context.Canceled) {
			return r.requireRecovery(cfg, fmt.Sprintf("rollback deadline exceeded while reconciling the source revision: %v", err)), nil
		}
		return result, err
	}
	if cfg.Status.Phase != cost.PhaseReady {
		if deadlineExceeded {
			return r.requireRecovery(cfg, "rollback deadline exceeded before the source revision became ready; inspect workloads and database state before recovery"), nil
		}
		return result, nil
	}
	r.upgradeStageIfChanged(op, cost.UpgradeStageRolledBack)
	r.setCondition(cfg, cost.ConditionSchemaUpToDate, metav1.ConditionTrue, "SourceRestored", "previous revision resumed before any migration execution, or after explicit recovery")
	r.setCondition(cfg, cost.ConditionRecoveryRequired, metav1.ConditionFalse, "SourceRestored", "")
	r.setCondition(cfg, cost.ConditionUpgradeFailed, metav1.ConditionTrue, "UpgradeFailed", op.Failure)
	r.setCondition(cfg, cost.ConditionDegraded, metav1.ConditionTrue, "UpgradeFailed", op.Failure)
	r.setCondition(cfg, cost.ConditionProgressing, metav1.ConditionFalse, op.Stage, "target upgrade did not complete")
	cfg.Status.Phase = cost.PhaseDegraded
	return ctrl.Result{RequeueAfter: requeueDrift}, nil
}

func (r *CostManagementServiceConfigReconciler) requireRecovery(cfg *cost.CostManagementServiceConfig, message string) ctrl.Result {
	op := cfg.Status.Upgrade.Operation
	if op.Stage != cost.UpgradeStageRecoveryRequired && r.Recorder != nil {
		r.Recorder.Event(cfg, corev1.EventTypeWarning, cost.UpgradeStageRecoveryRequired, message)
	}
	r.upgradeStageIfChanged(op, cost.UpgradeStageRecoveryRequired)
	r.setCondition(cfg, cost.ConditionRecoveryRequired, metav1.ConditionTrue, "RecoveryRequired", message+"; submit an explicit Retry or ResumePrevious request for operation "+op.ID)
	r.setCondition(cfg, cost.ConditionAvailable, metav1.ConditionFalse, cost.UpgradeStageRecoveryRequired, "application availability requires operator recovery")
	r.setCondition(cfg, cost.ConditionUpgradeFailed, metav1.ConditionTrue, "UpgradeFailed", message)
	r.setCondition(cfg, cost.ConditionDegraded, metav1.ConditionTrue, "UpgradeFailed", message)
	r.setCondition(cfg, cost.ConditionProgressing, metav1.ConditionFalse, cost.UpgradeStageRecoveryRequired, message)
	cfg.Status.Phase = cost.PhaseDegraded
	return ctrl.Result{RequeueAfter: requeueDrift}
}
func (r *CostManagementServiceConfigReconciler) upgradeStageIfChanged(op *cost.UpgradeOperation, stage string) {
	if op.Stage != stage {
		r.upgradeStage(op, stage)
	}
}
func (r *CostManagementServiceConfigReconciler) upgradeConfigError(cfg *cost.CostManagementServiceConfig, err error) (ctrl.Result, error) {
	// A rejected target does not take an already serving installed revision
	// offline. Keep its last known availability while reporting the bad request.
	if cfg.Status.Upgrade.Installed == nil || !apimeta.IsStatusConditionTrue(cfg.Status.Conditions, cost.ConditionAvailable) {
		r.setCondition(cfg, cost.ConditionAvailable, metav1.ConditionFalse, "UpgradeBlocked", err.Error())
	}
	r.setCondition(cfg, cost.ConditionDegraded, metav1.ConditionTrue, "UpgradeBlocked", err.Error())
	r.setCondition(cfg, cost.ConditionProgressing, metav1.ConditionFalse, "UpgradeBlocked", err.Error())
	cfg.Status.Phase = cost.PhaseDegraded
	return ctrl.Result{RequeueAfter: requeueSlow}, err
}
func (r *CostManagementServiceConfigReconciler) safetyReader() client.Reader {
	if r.APIReader != nil {
		return r.APIReader
	}
	return r.Client
}
func migrationNames(cfg *cost.CostManagementServiceConfig) []string {
	return []string{resources.NameKokuMigration(cfg), resources.NameROSMigration(cfg), resources.NameRBACMigration(cfg)}
}

func (r *CostManagementServiceConfigReconciler) legacyBaselineMatches(ctx context.Context, cfg *cost.CostManagementServiceConfig) (legacyBaselineCheck, error) {
	check := legacyBaselineCheck{}
	for _, desired := range resources.ApplicationDeployments(cfg) {
		live := desired.DeepCopy()
		err := r.safetyReader().Get(ctx, client.ObjectKeyFromObject(desired), live)
		if apierrors.IsNotFound(err) {
			continue
		}
		if err != nil {
			return legacyBaselineCheck{}, err
		}
		for _, want := range desired.Spec.Template.Spec.Containers {
			found := false
			for _, actual := range live.Spec.Template.Spec.Containers {
				if actual.Name == want.Name && actual.Image == want.Image {
					found = true
				}
			}
			if !found {
				return legacyBaselineCheck{}, nil
			}
		}
	}
	for _, desired := range migrationJobs(cfg) {
		live := &batchv1.Job{}
		err := r.safetyReader().Get(ctx, client.ObjectKeyFromObject(desired), live)
		if apierrors.IsNotFound(err) {
			check.MissingMigrationJobs = append(check.MissingMigrationJobs, desired.Name)
			continue
		}
		if err != nil {
			return legacyBaselineCheck{}, err
		}
		if !isJobComplete(live) || resources.MigrationExecutionID(live) != resources.MigrationExecutionID(desired) {
			return legacyBaselineCheck{}, nil
		}
	}
	check.Matches = true
	return check, nil
}

func (r *CostManagementServiceConfigReconciler) reconcileInitial(ctx context.Context, cfg *cost.CostManagementServiceConfig, bundle release.Bundle) (ctrl.Result, error) {
	// An old operator has no durable source snapshot. Adopt only while the
	// requested generation still matches the last reconciled installation.
	blocked := apimeta.FindStatusCondition(cfg.Status.Conditions, cost.ConditionDegraded)
	available := apimeta.IsStatusConditionTrue(cfg.Status.Conditions, cost.ConditionAvailable)
	if available || (blocked != nil && strings.HasPrefix(blocked.Message, "upgrade baseline unknown")) {
		baseline, err := r.legacyBaselineMatches(ctx, cfg)
		if err != nil {
			return ctrl.Result{}, err
		}
		if !baseline.Matches {
			return r.upgradeConfigError(cfg, fmt.Errorf("upgrade baseline unknown: restore the installed image/configuration pins before requesting an upgrade"))
		}
		if len(baseline.MissingMigrationJobs) > 0 {
			// Adopt the verified image baseline first, then let the ordinary upgrade
			// planner create a journaled migration operation. It will quiesce the
			// existing writers before recreating any missing migration receipt.
			cfg.Status.Upgrade.Installed = applicationRevision(cfg, bundle)
			message := "legacy migration Job receipt is missing; starting a quiesced migration recovery"
			r.setCondition(cfg, cost.ConditionSchemaUpToDate, metav1.ConditionFalse, "MigrationPending", message)
			r.setCondition(cfg, cost.ConditionAvailable, metav1.ConditionFalse, "MigrationPending", message)
			r.setCondition(cfg, cost.ConditionProgressing, metav1.ConditionTrue, "LegacyBaselineAdopted", message)
			r.setCondition(cfg, cost.ConditionDegraded, metav1.ConditionFalse, "LegacyBaselineAdopted", "")
			cfg.Status.Phase = cost.PhaseProgressing
			return ctrl.Result{RequeueAfter: requeueFast}, nil
		}
	}
	result, err := r.reconcileStack(ctx, cfg, true)
	if err == nil && cfg.Status.Phase == cost.PhaseReady {
		cfg.Status.Upgrade.Installed = applicationRevision(cfg, bundle)
	}
	return result, err
}

func (r *CostManagementServiceConfigReconciler) quiesceUpgrade(ctx context.Context, cfg *cost.CostManagementServiceConfig) (ctrl.Result, error) {
	op := cfg.Status.Upgrade.Operation
	quiet, err := r.quiesceApplications(ctx, cfg)
	if err != nil {
		return ctrl.Result{}, err
	}
	if !quiet {
		return ctrl.Result{RequeueAfter: requeueFast}, nil
	}
	stopped, err := r.migrationWritersStopped(ctx, cfg)
	if err != nil {
		return ctrl.Result{}, err
	}
	if !stopped {
		return ctrl.Result{RequeueAfter: requeueFast}, nil
	}
	// Persist this before any migration creation; timeouts and lost Job/Pod
	// evidence must conservatively assume a migration could have committed DDL.
	r.upgradeStage(op, cost.UpgradeStageMigrating)
	return ctrl.Result{RequeueAfter: requeueFast}, nil
}

func (r *CostManagementServiceConfigReconciler) migrateUpgrade(ctx context.Context, cfg *cost.CostManagementServiceConfig) (ctrl.Result, error) {
	op := cfg.Status.Upgrade.Operation
	quiet, err := r.quiesceApplications(ctx, cfg)
	if err != nil {
		return ctrl.Result{}, err
	}
	if !quiet {
		return ctrl.Result{RequeueAfter: requeueFast}, nil
	}
	// infrastructurePhases emits InfrastructureReady when a DB/cache outage
	// ends during the preflight, as it does in the regular pipeline.
	preflight := append([]PhaseFn{
		func() (Result, error) { return r.reconcileSharedConfig(ctx, cfg) },
	}, r.infrastructurePhases(ctx, cfg)...)
	for _, phase := range preflight {
		result, err := phase()
		if err != nil {
			if errors.Is(err, context.Canceled) {
				return ctrl.Result{}, err
			}
			return r.retryOrFailUpgrade(cfg, "migration preflight", err), nil
		}
		if result.Stop {
			return r.failUpgrade(cfg, "migration/dependency gate failed; inspect conditions and Job logs"), nil
		}
		if !result.IsZero() {
			if message, retryable := transientDatabaseOrCacheDependency(cfg); retryable {
				return r.retryUpgrade(cfg, "migration preflight", message), nil
			}
			return ctrl.Result{RequeueAfter: result.RequeueAfter}, nil
		}
	}
	if !op.SchemaMutationPossible {
		op.SchemaMutationPossible = true
		return ctrl.Result{RequeueAfter: requeueFast}, nil
	}
	result, err := r.reconcileMigration(ctx, cfg)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			return ctrl.Result{}, err
		}
		return r.retryOrFailUpgrade(cfg, "migration", err), nil
	}
	if result.Stop {
		// Keep the failed Job name/UID from migrationFailed in the operation.
		message := "migration gate failed; inspect Job logs and database state"
		if failed := apimeta.FindStatusCondition(cfg.Status.Conditions, cost.ConditionUpgradeFailed); failed != nil &&
			failed.Status == metav1.ConditionTrue && failed.Reason == reasonMigrationFailed {
			message = failed.Message
		}
		return r.failUpgrade(cfg, message), nil
	}
	if !result.IsZero() {
		return ctrl.Result{RequeueAfter: result.RequeueAfter}, nil
	}
	r.upgradeStage(op, cost.UpgradeStageRollingOut)
	return ctrl.Result{RequeueAfter: requeueFast}, nil
}

func (r *CostManagementServiceConfigReconciler) rolloutUpgrade(ctx context.Context, cfg *cost.CostManagementServiceConfig) ctrl.Result {
	op := cfg.Status.Upgrade.Operation
	result, err := r.reconcileStack(ctx, cfg, false)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			return ctrl.Result{}
		}
		return r.retryOrFailUpgrade(cfg, "application rollout", err)
	}
	if cfg.Status.Phase == cost.PhaseDegraded {
		if message, retryable := transientDatabaseOrCacheDependency(cfg); retryable && result.RequeueAfter > 0 {
			return r.retryUpgrade(cfg, "application rollout", message)
		}
		return r.failUpgrade(cfg, "target rollout failed readiness checks")
	}
	if cfg.Status.Phase != cost.PhaseReady {
		return result
	}
	cfg.Status.Upgrade.Installed = op.Target.DeepCopy()
	cfg.Status.Upgrade.Operation = nil
	r.setCondition(cfg, cost.ConditionUpgradeFailed, metav1.ConditionFalse, "UpgradeComplete", "all target application Deployments are ready")
	r.setCondition(cfg, cost.ConditionRecoveryRequired, metav1.ConditionFalse, "UpgradeComplete", "")
	if r.Recorder != nil {
		r.Recorder.Event(cfg, corev1.EventTypeNormal, "UpgradeComplete", "application upgrade completed")
	}
	return ctrl.Result{RequeueAfter: requeueFast}
}

func retryableUpgradeError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var networkErr net.Error
	if errors.As(err, &networkErr) && networkErr.Timeout() {
		return true
	}
	// API server restarts (routine during OpenShift upgrades) surface as dial,
	// reset or EOF errors rather than timeouts; none of them is a verdict.
	if utilnet.IsConnectionRefused(err) || utilnet.IsConnectionReset(err) || utilnet.IsProbableEOF(err) {
		return true
	}
	return apierrors.IsConflict(err) || apierrors.IsAlreadyExists(err) ||
		apierrors.IsTimeout(err) || apierrors.IsServerTimeout(err) ||
		apierrors.IsTooManyRequests(err) || apierrors.IsServiceUnavailable(err) ||
		apierrors.IsInternalError(err)
}

func transientDatabaseOrCacheDependency(cfg *cost.CostManagementServiceConfig) (string, bool) {
	for _, item := range []struct {
		conditionType string
		reason        string
	}{
		{cost.ConditionDatabaseReady, reasonDatabaseUnreachable},
		{cost.ConditionCacheReady, reasonCacheUnreachable},
	} {
		condition := apimeta.FindStatusCondition(cfg.Status.Conditions, item.conditionType)
		if condition != nil && condition.Status == metav1.ConditionFalse && condition.Reason == item.reason {
			return condition.Message, true
		}
	}
	return "", false
}

func (r *CostManagementServiceConfigReconciler) retryOrFailUpgrade(cfg *cost.CostManagementServiceConfig, phase string, err error) ctrl.Result {
	if retryableUpgradeError(err) {
		return r.retryUpgrade(cfg, phase, err.Error())
	}
	return r.failUpgrade(cfg, err.Error())
}

func (r *CostManagementServiceConfigReconciler) retryUpgrade(cfg *cost.CostManagementServiceConfig, phase, message string) ctrl.Result {
	message = fmt.Sprintf("retrying %s after a transient error: %s", phase, message)
	r.setCondition(cfg, cost.ConditionAvailable, metav1.ConditionFalse, "UpgradeRetrying", message)
	r.setCondition(cfg, cost.ConditionProgressing, metav1.ConditionTrue, "UpgradeRetrying", message)
	if apimeta.IsStatusConditionTrue(cfg.Status.Conditions, cost.ConditionDegraded) {
		cfg.Status.Phase = cost.PhaseDegraded
	} else {
		cfg.Status.Phase = cost.PhaseProgressing
	}
	return ctrl.Result{RequeueAfter: requeueSlow}
}

func (r *CostManagementServiceConfigReconciler) recoverUpgrade(ctx context.Context, cfg *cost.CostManagementServiceConfig) (ctrl.Result, error) {
	op := cfg.Status.Upgrade.Operation
	quiet, err := r.quiesceApplications(ctx, cfg)
	if err != nil {
		return ctrl.Result{}, err
	}
	if !quiet {
		return ctrl.Result{RequeueAfter: requeueFast}, nil
	}
	stopped, err := r.migrationWritersStopped(ctx, cfg)
	if err != nil {
		return ctrl.Result{}, err
	}
	if !stopped {
		return ctrl.Result{RequeueAfter: requeueFast}, nil
	}
	for _, name := range migrationNames(cfg) {
		job := &batchv1.Job{}
		err := r.safetyReader().Get(ctx, client.ObjectKey{Namespace: cfg.Namespace, Name: name}, job)
		if apierrors.IsNotFound(err) {
			continue
		}
		if err != nil {
			return ctrl.Result{}, err
		}
		if isJobFailed(job) {
			if err := r.Delete(ctx, job, client.PropagationPolicy(metav1.DeletePropagationForeground), client.Preconditions{UID: &job.UID}); err != nil && !apierrors.IsNotFound(err) {
				return ctrl.Result{}, err
			}
			return ctrl.Result{RequeueAfter: requeueFast}, nil
		}
	}
	r.upgradeStage(op, cost.UpgradeStageQuiescing)
	return ctrl.Result{RequeueAfter: requeueFast}, nil
}

func (r *CostManagementServiceConfigReconciler) acceptUpgradeRecovery(
	ctx context.Context, cfg, requested *cost.CostManagementServiceConfig, bundle release.Bundle, resolveErr error,
) (bool, error) {
	op := cfg.Status.Upgrade.Operation
	request := cfg.Spec.Upgrade.Recovery
	if (op.Stage != cost.UpgradeStageRecoveryRequired && op.Stage != cost.UpgradeStageRolledBack) || request == nil || request.OperationID != op.ID || request.RequestID == "" || request.RequestID == op.RecoveryRequestID {
		return false, nil
	}
	switch request.Action {
	case cost.RecoveryRetry:
		if resolveErr != nil {
			return true, resolveErr
		}
		if missing := resources.MissingWorkloadImages(cfg); len(missing) > 0 {
			return true, fmt.Errorf("recovery target has incomplete images: %v", missing)
		}
		target := applicationRevision(cfg, bundle)
		if err := bundle.ValidateTransition(requested, op.Source, target.Images); err != nil {
			return true, err
		}
		imageOnly := !op.SchemaMutationPossible && op.Source != nil && reflect.DeepEqual(op.Source.Migrations, target.Migrations)
		if imageOnly {
			// ResumePrevious retains failed Job evidence. A later Retry of the
			// source still needs to replace that receipt before becoming Ready.
			refresh, err := r.migrationReceiptRefreshNeeded(ctx, cfg)
			if err != nil {
				return true, err
			}
			imageOnly = !refresh
		}
		op.Target = *target
		op.TargetGeneration = cfg.Generation
		if imageOnly {
			// A failed image-only rollout has not touched the database. Retry the
			// requested application images directly without stopping writers or
			// replaying completed migration Jobs.
			r.upgradeStage(op, cost.UpgradeStageRollingOut)
		} else {
			r.upgradeStage(op, cost.UpgradeStageRecovering)
		}
	case cost.RecoveryResumePrevious:
		// Only this explicit action attests that external repair/restore made
		// the recorded source database/configuration safe to resume.
		op.SchemaMutationPossible = false
		r.upgradeStage(op, cost.UpgradeStageRollingBack)
	default:
		return true, fmt.Errorf("unknown recovery action %q", request.Action)
	}
	op.RecoveryRequestID = request.RequestID
	op.RecoveryAction = request.Action
	op.StartedAt = metav1.Now()
	return true, nil
}

func upgradeExpired(op *cost.UpgradeOperation, timeout int32) bool {
	switch op.Stage {
	case cost.UpgradeStageRollingBack, cost.UpgradeStageResumingPrevious, cost.UpgradeStageRecoveryRequired, cost.UpgradeStageRolledBack:
		return false
	}
	return time.Since(op.StartedAt.Time) > time.Duration(timeout)*time.Second
}

func upgradeTimeoutSeconds(cfg *cost.CostManagementServiceConfig, op *cost.UpgradeOperation) int32 {
	var frozen cost.CostManagementServiceConfigSpec
	if op != nil && json.Unmarshal(op.Target.Spec.Raw, &frozen) == nil && frozen.Upgrade.TimeoutSeconds > 0 {
		return frozen.Upgrade.TimeoutSeconds
	}
	if cfg.Spec.Upgrade.TimeoutSeconds > 0 {
		return cfg.Spec.Upgrade.TimeoutSeconds
	}
	return 7200
}
func frozenMigrationInputsChanged(cfg *cost.CostManagementServiceConfig, expected map[string]string) bool {
	jobs := migrationJobs(cfg)
	if len(jobs) != len(expected) {
		return true
	}
	for _, job := range jobs {
		if expected[job.Name] != resources.MigrationExecutionID(job) {
			return true
		}
	}
	return false
}
