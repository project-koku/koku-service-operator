package controller

import (
	"context"
	"fmt"
	"maps"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	costv1alpha1 "github.com/project-koku/koku-service-operator/api/v1alpha1"
	opmetrics "github.com/project-koku/koku-service-operator/internal/metrics"
	"github.com/project-koku/koku-service-operator/internal/resources"

	stderrors "errors"
)

const (
	fieldOwner    = "koku-service-operator"
	finalizerName = "costmanagementserviceconfigs.service.costmanagement.openshift.io/cleanup"
	// pauseAnnotation halts phased reconciliation when set to annotationTrue.
	// Deletion (finalizer cleanup) still runs while paused.
	pauseAnnotation = "costmanagementserviceconfigs.service.costmanagement.openshift.io/pause"
	// annotationTrue is the canonical truthy value for Kubernetes-style annotations
	// (pause, default StorageClass, etc.). Shared to satisfy goconst.
	annotationTrue   = "true"
	requeueFast      = 10 * time.Second
	requeueSlow      = 30 * time.Second
	requeueDrift     = 5 * time.Minute
	readinessTimeout = 5 * time.Minute

	// Status condition reasons shared with tests so the strings cannot drift.
	reasonWaitingForRBAC         = "WaitingForRBAC"
	reasonRBACAvailable          = "RBACAvailable"
	reasonWaitingForRBACWorker   = "WaitingForRBACWorker"
	reasonRBACWorkerAvailable    = "RBACWorkerAvailable"
	reasonWaitingForAPI          = "WaitingForAPI"
	reasonWaitingForMasu         = "WaitingForMasu"
	reasonWaitingForListener     = "WaitingForListener"
	reasonWaitingForKruize       = "WaitingForKruize"
	reasonWaitingForROSAPI       = "WaitingForROSAPI"
	reasonWaitingForROSProcessor = "WaitingForROSProcessor"
	reasonKokuAvailable          = "KokuAvailable"
	reasonDeploymentNotReady     = "DeploymentNotReady"
	reasonDeploymentReady        = "DeploymentReady"
	msgWaitingForRBACAPI         = "waiting for RBAC API"
	msgWaitingForRBACWorker      = "waiting for RBAC worker"
	msgWaitingForKokuAPI         = "waiting for Koku API"
	msgWaitingForMasu            = "waiting for Masu"
	msgWaitingForListener        = "waiting for Listener"
	msgWaitingForKruize          = "waiting for Kruize"
	msgWaitingForROSAPI          = "waiting for ROS API"
	msgWaitingForROSProcessor    = "waiting for ROS Processor"
)

type CostManagementServiceConfigReconciler struct {
	client.Client
	// APIReader is an uncached client for cross-namespace reads (e.g. NooBaa
	// admin Secret, typically in openshift-storage) that are outside
	// Cache.DefaultNamespaces.
	APIReader client.Reader
	Scheme    *runtime.Scheme
	Recorder  record.EventRecorder
}

// +kubebuilder:rbac:groups=service.costmanagement.openshift.io,resources=costmanagementserviceconfigs,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=service.costmanagement.openshift.io,resources=costmanagementserviceconfigs/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=service.costmanagement.openshift.io,resources=costmanagementserviceconfigs/finalizers,verbs=update
// +kubebuilder:rbac:groups=apps,resources=deployments;statefulsets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=batch,resources=jobs;cronjobs,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core,resources=services;configmaps;secrets;serviceaccounts;persistentvolumeclaims,verbs=get;list;watch;create;update;patch;delete
// Pods: read-only for syncManagedPodRestarts (CostManagementPodRestarting gauge).
// +kubebuilder:rbac:groups=core,resources=pods,verbs=get;list;watch
// +kubebuilder:rbac:groups=monitoring.coreos.com,resources=servicemonitors;prometheusrules,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=route.openshift.io,resources=routes,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=route.openshift.io,resources=routes/custom-host,verbs=create
// Namespace-scoped RBAC objects (Role + RoleBinding) — granted via RoleBinding.
// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=roles;rolebindings,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=networking.k8s.io,resources=networkpolicies,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core,resources=events,verbs=create;patch
// ObjectBucketClaims: namespaced Get/List during reconcile (findBoundOBC), not a watcher.
// +kubebuilder:rbac:groups=objectbucket.io,resources=objectbucketclaims,verbs=get;list
// Cluster-scoped resources (ingresses, storageclasses, consolelinks, clusterroles,
// clusterrolebindings, noobaa-admin secret) live in cluster_access_role.yaml
// (hand-maintained, bound via ClusterRoleBinding) — not here.

func (r *CostManagementServiceConfigReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	cfg := &costv1alpha1.CostManagementServiceConfig{}
	if err := r.Get(ctx, req.NamespacedName, cfg); err != nil {
		if errors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	// Deletion path: clean up cluster-scoped resources then remove finalizer.
	if !cfg.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, cfg)
	}

	// Ensure our finalizer is registered before touching any cluster-scoped resource.
	if !controllerutil.ContainsFinalizer(cfg, finalizerName) {
		controllerutil.AddFinalizer(cfg, finalizerName)
		if err := r.Update(ctx, cfg); err != nil {
			return ctrl.Result{}, fmt.Errorf("adding finalizer: %w", err)
		}
		return ctrl.Result{}, nil // requeue triggered by the Update
	}

	// Pause/resume (COST-7680): annotation short-circuits the phase pipeline.
	// Finalizer registration and deletion above still run while paused.
	if isPaused(cfg) {
		original := cfg.DeepCopy()
		r.markPaused(cfg)
		if patchErr := r.patchStatus(ctx, original, cfg); patchErr != nil {
			logger.Error(patchErr, "failed to patch status while paused")
			return ctrl.Result{}, patchErr
		}
		return ctrl.Result{}, nil
	}

	original := cfg.DeepCopy()
	// Clear stale Paused before phases so resume is recorded even when
	// reconcile returns an error (status is still patched below).
	r.clearPaused(cfg)

	result, reconcileErr := r.reconcile(ctx, cfg)
	if reconcileErr != nil {
		opmetrics.ReconcileErrors.WithLabelValues(cfg.Namespace, cfg.Name).Inc()
	}
	if err := r.syncManagedPodRestarts(ctx, cfg); err != nil {
		logger.Error(err, "failed to sync managed pod restart metrics")
	}
	r.syncConditionMetrics(cfg)

	if patchErr := r.patchStatus(ctx, original, cfg); patchErr != nil {
		logger.Error(patchErr, "failed to patch status")
		if reconcileErr == nil {
			return result, patchErr
		}
	}

	return result, reconcileErr
}

// markPaused sets Paused/Progressing conditions and emits a Paused event.
func (r *CostManagementServiceConfigReconciler) markPaused(cfg *costv1alpha1.CostManagementServiceConfig) {
	r.setCondition(cfg, costv1alpha1.ConditionPaused, metav1.ConditionTrue,
		"AnnotationSet", fmt.Sprintf("reconciliation paused (%s=%s)", pauseAnnotation, annotationTrue))
	r.setCondition(cfg, costv1alpha1.ConditionProgressing, metav1.ConditionFalse,
		"Paused", "reconciliation paused via annotation")
	cfg.Status.ObservedGeneration = cfg.Generation
	if r.Recorder != nil {
		r.Recorder.Event(cfg, corev1.EventTypeNormal, "Paused",
			"Reconciliation paused; remove the pause annotation to resume")
	}
}

// clearPaused flips a stale Paused=True condition to False (Resumed).
// No-op when Paused is absent or already False.
func (r *CostManagementServiceConfigReconciler) clearPaused(cfg *costv1alpha1.CostManagementServiceConfig) {
	if !apimeta.IsStatusConditionTrue(cfg.Status.Conditions, costv1alpha1.ConditionPaused) {
		return
	}
	r.setCondition(cfg, costv1alpha1.ConditionPaused, metav1.ConditionFalse,
		"Resumed", "pause annotation cleared; reconciliation active")
}

// isPaused reports whether the CR pause annotation is set to annotationTrue
// (case-insensitive). Missing or any other value means not paused.
func isPaused(cfg *costv1alpha1.CostManagementServiceConfig) bool {
	if cfg.Annotations == nil {
		return false
	}
	v, ok := cfg.Annotations[pauseAnnotation]
	if !ok {
		return false
	}
	return strings.EqualFold(strings.TrimSpace(v), annotationTrue)
}

// reconcileDelete removes cluster-scoped resources that cannot be cleaned up via
// ownerReferences, then strips the finalizer so the CR can be fully deleted.
func (r *CostManagementServiceConfigReconciler) reconcileDelete(ctx context.Context, cfg *costv1alpha1.CostManagementServiceConfig) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	logger.Info("deleting cluster-scoped resources before CR removal")

	// All cluster-scoped resources the operator creates, keyed by type.
	// Add entries here whenever a new cluster-scoped resource type is introduced.
	clusterScoped := []client.Object{
		resources.ConsoleLink(cfg),
		resources.KruizeClusterRoleBinding(cfg),
		resources.KruizeClusterRole(cfg),
	}

	for _, obj := range clusterScoped {
		if err := r.Delete(ctx, obj); err != nil && !errors.IsNotFound(err) {
			return ctrl.Result{}, fmt.Errorf("deleting %s %s: %w", obj.GetObjectKind().GroupVersionKind().Kind, obj.GetName(), err)
		}
	}

	// Drop UWM series for this CMSC so deleted instances cannot keep alerts firing.
	opmetrics.ClearManagedPodRestarts(cfg.Namespace, cfg.Name)
	opmetrics.ClearConditionMetrics(cfg.Namespace, cfg.Name)
	opmetrics.ClearMigrationJobFailedAll(cfg.Namespace, cfg.Name)

	controllerutil.RemoveFinalizer(cfg, finalizerName)
	if err := r.Update(ctx, cfg); err != nil {
		return ctrl.Result{}, fmt.Errorf("removing finalizer: %w", err)
	}
	return ctrl.Result{}, nil
}

// reconcile drives the ordered, staged rollout:
//  0. Discovery (cluster domain, StorageClass, S3)
//  1. Shared configuration (ConfigMaps, Secrets, ServiceAccount)
//  2. Infrastructure (PostgreSQL, Valkey)
//  3. Validation (TCP/HTTP probes for external deps, Secret key checks)
//  4. DB migration gate (Koku → ROS → RBAC)
//  5. Core services (Koku API, Masu, Listener)
//  6. Workers (Celery, ROS, Kruize)
//  7. Edge (Envoy gateway, UI, Route)
func (r *CostManagementServiceConfigReconciler) reconcile(ctx context.Context, cfg *costv1alpha1.CostManagementServiceConfig) (ctrl.Result, error) {
	// Capture the phase before overwriting it so we can detect the
	// first Ready transition at the end of this pass.
	priorPhase := cfg.Status.Phase
	cfg.Status.ObservedGeneration = cfg.Generation
	cfg.Status.Phase = costv1alpha1.PhaseProgressing
	r.setCondition(cfg, costv1alpha1.ConditionProgressing, metav1.ConditionTrue, "Reconciling", "Reconciliation in progress")

	result, err := runPhases([]PhaseFn{
		func() (Result, error) { return r.reconcileDiscovery(ctx, cfg) },
		func() (Result, error) { return r.reconcileSharedConfig(ctx, cfg) },
		func() (Result, error) { return r.reconcileInfrastructure(ctx, cfg) },
		func() (Result, error) { return r.reconcileValidation(ctx, cfg) },
		func() (Result, error) { return r.reconcileMigration(ctx, cfg) },
		func() (Result, error) { return r.reconcileCoreServices(ctx, cfg) },
		func() (Result, error) { return r.reconcileWorkers(ctx, cfg) },
		func() (Result, error) { return r.reconcileEdge(ctx, cfg) },
		func() (Result, error) { return r.reconcileMonitoring(ctx, cfg) },
	})

	if err != nil {
		// applyPhaseError handles structured PhaseError (condition + phase from err).
		applyPhaseError(cfg, err)
		// For any error (structured or plain), ensure Degraded is visible in status.
		// Without this, plain fmt.Errorf from phases left Degraded unset.
		r.setCondition(cfg, costv1alpha1.ConditionDegraded, metav1.ConditionTrue,
			"ReconcileError", err.Error())
		cfg.Status.Phase = costv1alpha1.PhaseDegraded
		r.emitPhaseChanged(cfg, priorPhase, costv1alpha1.PhaseDegraded)
		r.Recorder.Eventf(cfg, corev1.EventTypeWarning, "ReconcileError", "%v", err)
		return ctrl.Result{RequeueAfter: requeueSlow}, err
	}
	if !result.IsZero() {
		// Blocking validation / failed migrate set PhaseDegraded with RequeueAfter or
		// Stop and never return an error — emit PhaseChanged here or Ready→Degraded
		// is silent (COST-7692).
		if cfg.Status.Phase == costv1alpha1.PhaseDegraded && cfg.Status.Phase != priorPhase {
			r.emitPhaseChanged(cfg, priorPhase, cfg.Status.Phase)
		}
		return ctrl.Result{RequeueAfter: result.RequeueAfter}, nil
	}

	// Emit Ready event only on the first transition to Ready.
	// Use priorPhase (captured before this pass reset it to Progressing)
	// to detect a genuine non-Ready → Ready transition.
	if priorPhase != costv1alpha1.PhaseReady {
		r.Recorder.Event(cfg, corev1.EventTypeNormal, "Ready", "All components are running")
	}
	r.setCondition(cfg, costv1alpha1.ConditionAvailable, metav1.ConditionTrue, "AllComponentsReady", "All components are running")
	r.setCondition(cfg, costv1alpha1.ConditionProgressing, metav1.ConditionFalse, "ReconcileComplete", "")
	r.setCondition(cfg, costv1alpha1.ConditionDegraded, metav1.ConditionFalse, "ReconcileComplete", "")
	cfg.Status.Phase = costv1alpha1.PhaseReady
	r.emitPhaseChanged(cfg, priorPhase, costv1alpha1.PhaseReady)
	// Periodic drift correction: re-apply all desired state every 5 minutes so
	// manual edits to managed resources are reverted without waiting for an event.
	return ctrl.Result{RequeueAfter: requeueDrift}, nil
}

// -----------------------------------------------------------------------------
// Stage 1 — Shared configuration objects
// -----------------------------------------------------------------------------

func (r *CostManagementServiceConfigReconciler) reconcileSharedConfig(ctx context.Context, cfg *costv1alpha1.CostManagementServiceConfig) (Result, error) {
	// Secrets — create-if-absent only (never overwrite credentials).
	// Skip DB credentials when the user named their own external Secret:
	// creating one with random passwords would silently overwrite their creds.
	if cfg.Spec.Database.SecretName == "" {
		if err := r.ensureSecret(ctx, cfg, resources.DBCredentialsSecret(cfg)); err != nil {
			return Result{}, fmt.Errorf("db-credentials secret: %w", err)
		}
	}
	if err := r.ensureSecret(ctx, cfg, resources.DjangoSecret(cfg)); err != nil {
		return Result{}, fmt.Errorf("django secret: %w", err)
	}
	if cfg.Spec.ObjectStorage.SecretName == "" {
		if err := r.ensureSecret(ctx, cfg, resources.StorageCredentialsSecret(cfg)); err != nil {
			return Result{}, fmt.Errorf("storage credentials secret: %w", err)
		}
	}

	// ConfigMaps
	for _, cm := range []*corev1.ConfigMap{
		resources.DBInitConfigMap(cfg),
		resources.AWSConfigMap(cfg),
		resources.CACombineConfigMap(cfg),
		resources.ServiceCAConfigMap(cfg),
	} {
		if err := r.apply(ctx, cfg, cm); err != nil {
			return Result{}, fmt.Errorf("configmap %s: %w", cm.Name, err)
		}
	}

	// ServiceAccount (skipped when costManagement.serviceAccount.create=false).
	if err := r.ensureServiceAccount(ctx, cfg, cfg.Spec.CostManagement.ServiceAccount, resources.KokuServiceAccount(cfg)); err != nil {
		return Result{}, fmt.Errorf("koku serviceaccount: %w", err)
	}
	// Family SAs with no CR create=false knob — Create defaults true.
	for _, item := range []struct {
		kind string
		sa   *corev1.ServiceAccount
	}{
		{"gateway", resources.GatewayServiceAccount(cfg)},
		{"ingress", resources.IngressServiceAccount(cfg)},
		{"rbac", resources.RBACServiceAccount(cfg)},
		{"ui", resources.UIServiceAccount(cfg)},
	} {
		if err := r.ensureServiceAccount(ctx, cfg, costv1alpha1.ServiceAccountSpec{}, item.sa); err != nil {
			return Result{}, fmt.Errorf("%s serviceaccount: %w", item.kind, err)
		}
	}

	return Result{}, nil
}

// -----------------------------------------------------------------------------
// Stage 2 — Infrastructure
// -----------------------------------------------------------------------------

func (r *CostManagementServiceConfigReconciler) reconcileInfrastructure(ctx context.Context, cfg *costv1alpha1.CostManagementServiceConfig) (Result, error) {
	alreadyReady := apimeta.IsStatusConditionTrue(cfg.Status.Conditions, costv1alpha1.ConditionDatabaseReady) &&
		apimeta.IsStatusConditionTrue(cfg.Status.Conditions, costv1alpha1.ConditionCacheReady)

	if costv1alpha1.BoolVal(cfg.Spec.Database.Deploy, true) {
		if err := r.apply(ctx, cfg, resources.DatabaseService(cfg)); err != nil {
			return Result{}, fmt.Errorf("database service: %w", err)
		}
		if err := r.applyStatefulSet(ctx, cfg, resources.DatabaseStatefulSet(cfg)); err != nil {
			if stderrors.Is(err, ErrStorageConfigChanged) {
				// VCT mismatch: condition already set, stop reconciliation to avoid overwriting it.
				return Result{RequeueAfter: requeueFast}, nil
			}
			return Result{}, fmt.Errorf("database statefulset: %w", err)
		}
		// Gate: wait for the DB pod to be ready.
		ready, err := r.isStatefulSetReady(ctx, cfg.Namespace, resources.NameDatabase(cfg))
		if err != nil {
			return Result{}, err
		}
		if !ready {
			r.setCondition(cfg, costv1alpha1.ConditionDatabaseReady, metav1.ConditionFalse, "WaitingForDatabase", "waiting for PostgreSQL pod")
			return Result{RequeueAfter: requeueFast}, nil
		}
		r.setCondition(cfg, costv1alpha1.ConditionDatabaseReady, metav1.ConditionTrue, "DatabaseAvailable", "")
	} else {
		r.setCondition(cfg, costv1alpha1.ConditionDatabaseReady, metav1.ConditionTrue, "ExternalDatabase", "")
	}

	if costv1alpha1.BoolVal(cfg.Spec.Cache.Deploy, true) {
		if err := r.apply(ctx, cfg, resources.CachePVC(cfg)); err != nil {
			return Result{}, fmt.Errorf("valkey pvc: %w", err)
		}
		if err := r.apply(ctx, cfg, resources.CacheDeployment(cfg)); err != nil {
			return Result{}, fmt.Errorf("valkey deployment: %w", err)
		}
		if err := r.apply(ctx, cfg, resources.CacheService(cfg)); err != nil {
			return Result{}, fmt.Errorf("valkey service: %w", err)
		}
		ready, err := r.isDeploymentReady(ctx, cfg.Namespace, resources.NameValkey(cfg))
		if err != nil {
			return Result{}, err
		}
		if !ready {
			r.setCondition(cfg, costv1alpha1.ConditionCacheReady, metav1.ConditionFalse, "WaitingForCache", "waiting for Valkey pod")
			return Result{RequeueAfter: requeueFast}, nil
		}
		r.setCondition(cfg, costv1alpha1.ConditionCacheReady, metav1.ConditionTrue, "CacheAvailable", "")
	} else {
		r.setCondition(cfg, costv1alpha1.ConditionCacheReady, metav1.ConditionTrue, "ExternalCache", "")
	}

	if !alreadyReady {
		r.Recorder.Event(cfg, corev1.EventTypeNormal, "InfrastructureReady", "Database and cache are available")
	}
	return Result{}, nil
}

// -----------------------------------------------------------------------------
// Stage 3 — DB migration gate
// -----------------------------------------------------------------------------

// reconcileMigration runs migration Jobs sequentially:
// Koku → ROS → RBAC migrate+seed → (optional) RBAC admin-bootstrap.
// Each Job must complete before the next is created. Previously-succeeded
// Jobs are not re-created unless the image-tag annotation changed.
func (r *CostManagementServiceConfigReconciler) reconcileMigration(ctx context.Context, cfg *costv1alpha1.CostManagementServiceConfig) (Result, error) {
	if msg := migrationImageError(cfg); msg != "" {
		r.setCondition(cfg, costv1alpha1.ConditionSchemaUpToDate, metav1.ConditionFalse, "ImageNotSet", msg)
		r.setCondition(cfg, costv1alpha1.ConditionDegraded, metav1.ConditionTrue, "ImageNotSet", msg)
		cfg.Status.Phase = costv1alpha1.PhaseDegraded
		r.Recorder.Event(cfg, corev1.EventTypeWarning, "ImageNotSet", msg)
		return Result{Stop: true}, nil
	}

	type migStep struct {
		name     string
		imageTag string
		build    func() *batchv1.Job
	}

	steps := []migStep{
		{
			name:     resources.NameKokuMigration(cfg),
			imageTag: cfg.Spec.CostManagement.API.Image.Tag,
			build:    func() *batchv1.Job { return resources.MigrationJob(cfg, cfg.Spec.CostManagement.API.Image.Tag) },
		},
	}
	// ROS schema migrate only when ROS is enabled.
	if costv1alpha1.ROSEnabled(cfg) {
		steps = append(steps, migStep{
			name:     resources.NameROSMigration(cfg),
			imageTag: cfg.Spec.ROS.Image.Tag,
			build:    func() *batchv1.Job { return resources.ROSMigrationJob(cfg, cfg.Spec.ROS.Image.Tag) },
		})
	}
	steps = append(steps, migStep{
		name:     resources.NameRBACMigration(cfg),
		imageTag: resources.RBACSeedJobTag(cfg.Spec.RBAC.Image.Tag),
		build:    func() *batchv1.Job { return resources.RBACMigrationJob(cfg, cfg.Spec.RBAC.Image.Tag) },
	})
	if resources.AdminBootstrapJob(cfg, cfg.Spec.RBAC.Image.Tag) != nil {
		steps = append(steps, migStep{
			name:     resources.NameRBACAdminBootstrap(cfg),
			imageTag: resources.RBACSeedJobTag(cfg.Spec.RBAC.Image.Tag),
			build:    func() *batchv1.Job { return resources.AdminBootstrapJob(cfg, cfg.Spec.RBAC.Image.Tag) },
		})
	} else if cfg.Spec.RBAC.BootstrapAdmin.Enabled {
		r.Recorder.Eventf(cfg, corev1.EventTypeWarning, "BootstrapAdminSkipped",
			"bootstrapAdmin.enabled is true but secretRef.name is empty — admin bootstrap will not run; set spec.rbac.bootstrapAdmin.secretRef to a Secret with keys org-id, account-number, username")
	}

	for i, step := range steps {
		result, err := r.runMigrationStep(ctx, cfg, step.name, step.imageTag, step.build, i+1, len(steps))
		if err != nil || !result.IsZero() {
			return result, err
		}
	}

	// All steps completed.
	schemaWasReady := apimeta.IsStatusConditionTrue(cfg.Status.Conditions, costv1alpha1.ConditionSchemaUpToDate)
	r.setCondition(cfg, costv1alpha1.ConditionSchemaUpToDate, metav1.ConditionTrue, "MigrationComplete", "all schema migrations succeeded")
	r.setCondition(cfg, costv1alpha1.ConditionDegraded, metav1.ConditionFalse, "MigrationSucceeded", "")
	if !schemaWasReady && r.Recorder != nil {
		r.Recorder.Event(cfg, corev1.EventTypeNormal, "MigrationsComplete",
			"All schema migrations succeeded (Koku → ROS → RBAC)")
	}
	return Result{}, nil
}

// migrationImageError returns a human-readable error when a required workload
// image is missing. Koku migrations use spec.costManagement.api.image (the same
// image as API/Masu/listener). Empty images must fail closed rather than
// creating Jobs with ":" or a hardcoded :latest tag.
func migrationImageError(cfg *costv1alpha1.CostManagementServiceConfig) string {
	if _, ok := resources.KokuImage(cfg); !ok {
		return "spec.costManagement.api.image.repository and tag are required (Koku migration must use the same image as the API)"
	}
	if costv1alpha1.ROSEnabled(cfg) {
		if _, ok := resources.ImageRef(cfg.Spec.ROS.Image); !ok {
			return "spec.ros.image.repository and tag are required when ROS is enabled"
		}
	}
	if _, ok := resources.ImageRef(cfg.Spec.RBAC.Image); !ok {
		return "spec.rbac.image.repository and tag are required"
	}
	return ""
}

// runMigrationStep manages a single migration Job: create if absent, detect
// upgrades, poll completion, surface failures. Returns a non-zero Result when
// the pipeline should pause (job still running or just failed).
func (r *CostManagementServiceConfigReconciler) runMigrationStep(
	ctx context.Context,
	cfg *costv1alpha1.CostManagementServiceConfig,
	jobName, imageTag string,
	build func() *batchv1.Job,
	stepNum, totalSteps int,
) (Result, error) {
	progress := func(msg string) {
		r.setCondition(cfg, costv1alpha1.ConditionSchemaUpToDate, metav1.ConditionFalse,
			"MigrationRunning",
			fmt.Sprintf("step %d/%d — %s: %s", stepNum, totalSteps, jobName, msg))
	}

	existing := &batchv1.Job{}
	err := r.Get(ctx, types.NamespacedName{Namespace: cfg.Namespace, Name: jobName}, existing)

	if errors.IsNotFound(err) {
		job := build()
		if job == nil {
			return Result{}, fmt.Errorf("create %s: image is unset", jobName)
		}
		setOwnerRef(cfg, job)
		if createErr := r.Create(ctx, job); createErr != nil {
			return Result{}, fmt.Errorf("create %s: %w", jobName, createErr)
		}
		progress("job created")
		r.Recorder.Eventf(cfg, corev1.EventTypeNormal, "MigrationStarted",
			"Migration step %d/%d started: %s", stepNum, totalSteps, jobName)
		return Result{RequeueAfter: requeueFast}, nil
	}
	if err != nil {
		return Result{}, err
	}

	// Upgrade: image tag changed → delete and let next reconcile recreate.
	if existing.Annotations[resources.MigrationImageTagAnnotation] != imageTag {
		if delErr := r.Delete(ctx, existing, client.PropagationPolicy(metav1.DeletePropagationBackground)); delErr != nil && !errors.IsNotFound(delErr) {
			return Result{}, fmt.Errorf("delete stale %s: %w", jobName, delErr)
		}
		progress("restarting for new image")
		return Result{RequeueAfter: requeueFast}, nil
	}

	if isJobComplete(existing) {
		opmetrics.SetMigrationJobFailed(cfg.Namespace, cfg.Name, jobName, false)
		return Result{}, nil // proceed to next step
	}
	if isJobFailed(existing) {
		msg := fmt.Sprintf("%s exhausted retries — check pod logs", jobName)
		r.setCondition(cfg, costv1alpha1.ConditionSchemaUpToDate, metav1.ConditionFalse, "MigrationFailed", msg)
		r.setCondition(cfg, costv1alpha1.ConditionDegraded, metav1.ConditionTrue, "MigrationFailed", msg)
		cfg.Status.Phase = costv1alpha1.PhaseDegraded
		opmetrics.SetMigrationJobFailed(cfg.Namespace, cfg.Name, jobName, true)
		r.Recorder.Eventf(cfg, corev1.EventTypeWarning, "MigrationFailed",
			"Migration job %s exhausted retries — manual intervention required", jobName)
		return Result{Stop: true}, nil
	}
	opmetrics.SetMigrationJobFailed(cfg.Namespace, cfg.Name, jobName, false)

	progress("running")
	return Result{RequeueAfter: requeueFast}, nil
}

// -----------------------------------------------------------------------------
// Stage 4 — Core services
// -----------------------------------------------------------------------------

func (r *CostManagementServiceConfigReconciler) reconcileCoreServices(ctx context.Context, cfg *costv1alpha1.CostManagementServiceConfig) (Result, error) {
	// Bookkeeping first: when ROS is disabled, tear down leftover Kruize/ROS
	// objects so a prior enabled install cannot block later stages.
	if err := r.reconcileROSFeature(ctx, cfg); err != nil {
		return Result{}, err
	}

	if costv1alpha1.ROSEnabled(cfg) {
		// Kruize (needed by ROS Processor and Poller — deploy before ROS).
		kruizeObjs := []client.Object{
			resources.KruizeServiceAccount(cfg),
			resources.KruizeConfigMap(cfg),
			resources.KruizeDeployment(cfg),
			resources.KruizeService(cfg),
		}
		for _, obj := range kruizeObjs {
			if err := r.apply(ctx, cfg, obj); err != nil {
				return Result{}, fmt.Errorf("kruize %s: %w", obj.GetName(), err)
			}
		}
		// Kruize ClusterRole/ClusterRoleBinding are cluster-scoped — no ownerRef.
		// Cleaned up by the CR finalizer in reconcileDelete().
		for _, obj := range []client.Object{resources.KruizeClusterRole(cfg), resources.KruizeClusterRoleBinding(cfg)} {
			if err := r.applyClusterScoped(ctx, obj); err != nil {
				return Result{}, fmt.Errorf("kruize rbac %s: %w", obj.GetName(), err)
			}
		}
	}

	// RBAC API + worker (must be up before Koku API starts serving requests).
	rbacObjs := []client.Object{
		resources.RBACAPIDeployment(cfg),
		resources.RBACAPIService(cfg),
		resources.RBACWorkerDeployment(cfg),
	}
	for _, obj := range rbacObjs {
		if err := r.apply(ctx, cfg, obj); err != nil {
			return Result{}, fmt.Errorf("rbac %s: %w", obj.GetName(), err)
		}
	}

	// Koku core. ROS SA/config only when ROS is enabled.
	objs := []client.Object{
		resources.KokuAPIDeployment(cfg),
		resources.KokuAPIService(cfg),
		resources.MasuDeployment(cfg),
		resources.MasuService(cfg),
		resources.ListenerDeployment(cfg),
	}
	if costv1alpha1.ROSEnabled(cfg) {
		// ROS ServiceAccount (skipped when ros.serviceAccount.create=false).
		if err := r.ensureServiceAccount(ctx, cfg, cfg.Spec.ROS.ServiceAccount, resources.ROSServiceAccount(cfg)); err != nil {
			return Result{}, fmt.Errorf("ros serviceaccount: %w", err)
		}
		objs = append([]client.Object{resources.CdappConfigMap(cfg)}, objs...)
	}
	for _, obj := range objs {
		if err := r.apply(ctx, cfg, obj); err != nil {
			return Result{}, fmt.Errorf("core service %s: %w", obj.GetName(), err)
		}
	}

	// RBAC worker is independent of Available: surface it as its own
	// condition so a down worker is not hidden behind RBACReady=True.
	workerReady, err := r.isDeploymentReady(ctx, cfg.Namespace, resources.NameRBACWorker(cfg))
	if err != nil {
		return Result{}, err
	}
	if !workerReady {
		r.setCondition(cfg, costv1alpha1.ConditionRBACWorkerReady, metav1.ConditionFalse, reasonWaitingForRBACWorker, msgWaitingForRBACWorker)
	} else {
		r.setCondition(cfg, costv1alpha1.ConditionRBACWorkerReady, metav1.ConditionTrue, reasonRBACWorkerAvailable, "")
	}

	// Gate on the RBAC API (not the worker). Koku and Envoy call this
	// service for authorization; do not report Available while it is down.
	rbacReady, err := r.isDeploymentReady(ctx, cfg.Namespace, resources.NameRBACAPI(cfg))
	if err != nil {
		return Result{}, err
	}
	if !rbacReady {
		r.setCondition(cfg, costv1alpha1.ConditionRBACReady, metav1.ConditionFalse, reasonWaitingForRBAC, msgWaitingForRBACAPI)
		r.setCondition(cfg, costv1alpha1.ConditionAvailable, metav1.ConditionFalse, reasonWaitingForRBAC, msgWaitingForRBACAPI)
		return Result{RequeueAfter: requeueSlow}, nil
	}
	r.setCondition(cfg, costv1alpha1.ConditionRBACReady, metav1.ConditionTrue, reasonRBACAvailable, "")

	return r.waitForCoreServiceReadiness(ctx, cfg)
}

func (r *CostManagementServiceConfigReconciler) waitForCoreServiceReadiness(ctx context.Context, cfg *costv1alpha1.CostManagementServiceConfig) (Result, error) {
	waits := []deploymentWait{
		{resources.NameKokuAPI(cfg), reasonWaitingForAPI, msgWaitingForKokuAPI, "Koku API"},
		{resources.NameMasu(cfg), reasonWaitingForMasu, msgWaitingForMasu, "Masu"},
		{resources.NameListener(cfg), reasonWaitingForListener, msgWaitingForListener, "Listener"},
	}
	if costv1alpha1.ROSEnabled(cfg) {
		waits = append(waits, deploymentWait{resources.NameKruize(cfg), reasonWaitingForKruize, msgWaitingForKruize, "Kruize"})
	}
	if result, err := r.waitForDeployments(ctx, cfg, waits); err != nil || !result.IsZero() {
		return result, err
	}

	// A later phase (ROS API/Processor) may already own Available=False.
	// Promoting to KokuAvailable here would stamp a new LastTransitionTime
	// and reset the 5-minute DeploymentNotReady clock on every pass.
	// CoreServicesAvailable is skipped on this path; reconcile() emits
	// AllComponentsReady when the wait clears.
	if holdingWorkerReadinessWait(cfg) {
		return Result{}, nil
	}

	if !apimeta.IsStatusConditionTrue(cfg.Status.Conditions, costv1alpha1.ConditionAvailable) {
		r.Recorder.Event(cfg, corev1.EventTypeNormal, "CoreServicesAvailable", "Koku API is ready")
	}
	r.setCondition(cfg, costv1alpha1.ConditionAvailable, metav1.ConditionTrue, reasonKokuAvailable, "")
	return Result{}, nil
}

// -----------------------------------------------------------------------------
// Stage 5 — Workers and supporting services
// -----------------------------------------------------------------------------

func (r *CostManagementServiceConfigReconciler) reconcileWorkers(ctx context.Context, cfg *costv1alpha1.CostManagementServiceConfig) (Result, error) {
	workers := resources.CeleryWorkerDeployments(cfg)
	objs := make([]client.Object, 0, 1+len(workers))

	// Celery beat + workers
	objs = append(objs, resources.CeleryBeatDeployment(cfg))
	for _, d := range workers {
		objs = append(objs, d)
	}

	if costv1alpha1.ROSEnabled(cfg) {
		objs = append(objs,
			resources.ROSAPIDeployment(cfg),
			resources.ROSAPIService(cfg),
			resources.ROSProcessorDeployment(cfg),
			resources.ROSPollerDeployment(cfg),
			resources.ROSHousekeeperDeployment(cfg),
		)

		// Kruize delete-partitions CronJob — apply when enabled, delete when disabled.
		if costv1alpha1.BoolVal(cfg.Spec.Kruize.Partitions.DeleteEnabled, true) {
			objs = append(objs, resources.KruizeDeletePartitionsCronJob(cfg))
		} else {
			cj := resources.KruizeDeletePartitionsCronJob(cfg)
			if err := r.Delete(ctx, cj); err != nil && !errors.IsNotFound(err) {
				return Result{}, fmt.Errorf("delete kruize delete-partitions cronjob: %w", err)
			}
		}

		// ROS partition-cleaner CronJob — apply when enabled, delete when disabled.
		if costv1alpha1.BoolVal(cfg.Spec.ROS.Housekeeper.PartitionCleaner.Enabled, true) {
			objs = append(objs, resources.ROSPartitionCleanerCronJob(cfg))
		} else {
			cj := resources.ROSPartitionCleanerCronJob(cfg)
			if err := r.Delete(ctx, cj); err != nil && !errors.IsNotFound(err) {
				return Result{}, fmt.Errorf("delete ros partition-cleaner cronjob: %w", err)
			}
		}
	}

	// RBAC Keycloak-to-RBAC principal sync CronJob.
	if cfg.Spec.RBAC.KeycloakSync.Enabled {
		if cfg.Spec.RBAC.KeycloakSync.ClientSecretRef.Name == "" {
			return Result{}, fmt.Errorf("rbac.keycloakSync.clientSecretRef.name is required when keycloakSync is enabled")
		}
		objs = append(objs,
			resources.KeycloakSyncConfigMap(cfg),
			resources.KeycloakSyncCronJob(cfg),
		)
	} else {
		for _, obj := range []client.Object{
			resources.KeycloakSyncCronJob(cfg),
			resources.KeycloakSyncConfigMap(cfg),
		} {
			if err := r.Delete(ctx, obj); err != nil && !errors.IsNotFound(err) {
				return Result{}, fmt.Errorf("delete keycloak-sync %s: %w", obj.GetName(), err)
			}
		}
	}

	// Ingress upload handler — must be deployed before reconcileEdge so the
	// Envoy gateway has a live backend for /api/ingress/ routes.
	objs = append(objs,
		resources.IngressDeployment(cfg),
		resources.IngressService(cfg),
	)

	for _, obj := range objs {
		if err := r.apply(ctx, cfg, obj); err != nil {
			return Result{}, fmt.Errorf("worker %s: %w", obj.GetName(), err)
		}
	}

	return r.waitForWorkerReadiness(ctx, cfg)
}

func (r *CostManagementServiceConfigReconciler) waitForWorkerReadiness(ctx context.Context, cfg *costv1alpha1.CostManagementServiceConfig) (Result, error) {
	ready, err := r.isDeploymentReady(ctx, cfg.Namespace, resources.NameIngress(cfg))
	if err != nil {
		return Result{}, err
	}
	if !ready {
		r.setCondition(cfg, costv1alpha1.ConditionIngressReady, metav1.ConditionFalse,
			"WaitingForIngress", "waiting for Ingress upload Deployment")
		return Result{RequeueAfter: requeueSlow}, nil
	}
	r.setCondition(cfg, costv1alpha1.ConditionIngressReady, metav1.ConditionTrue,
		"IngressReady", "Ingress upload Deployment is ready")

	if costv1alpha1.ROSEnabled(cfg) {
		rosWaits := []deploymentWait{
			{resources.NameROSAPI(cfg), reasonWaitingForROSAPI, msgWaitingForROSAPI, "ROS API"},
			{resources.NameROSProcessor(cfg), reasonWaitingForROSProcessor, msgWaitingForROSProcessor, "ROS Processor"},
		}
		if result, err := r.waitForDeployments(ctx, cfg, rosWaits); err != nil || !result.IsZero() {
			return result, err
		}
	}
	return Result{}, nil
}

// -----------------------------------------------------------------------------
// Stage 6 — Edge: gateway, UI, routes
// -----------------------------------------------------------------------------

func (r *CostManagementServiceConfigReconciler) reconcileEdge(ctx context.Context, cfg *costv1alpha1.CostManagementServiceConfig) (Result, error) {
	logger := log.FromContext(ctx)

	objs := []client.Object{
		resources.EnvoyConfigMap(cfg),
		resources.EnvoyService(cfg),
		resources.EnvoyDeployment(cfg),
	}
	for _, obj := range objs {
		if err := r.apply(ctx, cfg, obj); err != nil {
			return Result{}, fmt.Errorf("edge %s: %w", obj.GetName(), err)
		}
	}

	route := resources.GatewayAPIRoute(cfg)
	if route != nil {
		if err := r.apply(ctx, cfg, route); err != nil {
			return Result{}, fmt.Errorf("edge route %s: %w", route.GetName(), err)
		}
	} else {
		logger.Info("skipping API Route: cluster domain not resolved; gateway still deployed for in-cluster access")
	}

	// Isolation and UI bootstrap do not wait on the API Route. OpenShift
	// defaults to allow-all until a NetworkPolicy exists, and cookie/nginx
	// ConfigMaps are safe without a host.
	if err := r.applyNetworkPolicies(ctx, cfg); err != nil {
		return Result{}, err
	}
	if err := r.applyUICookieAndNginx(ctx, cfg); err != nil {
		return Result{}, err
	}

	ready, err := r.isDeploymentReady(ctx, cfg.Namespace, resources.NameEnvoy(cfg))
	if err != nil {
		return Result{}, err
	}
	if !ready {
		r.setCondition(cfg, costv1alpha1.ConditionGatewayReady, metav1.ConditionFalse,
			"WaitingForGateway", "waiting for Envoy gateway Deployment")
		return Result{RequeueAfter: requeueSlow}, nil
	}

	if route == nil {
		r.setCondition(cfg, costv1alpha1.ConditionGatewayReady, metav1.ConditionFalse,
			"ClusterDomainPending", "Envoy gateway ready; API Route deferred until cluster domain is available")
		return Result{RequeueAfter: requeueSlow}, nil
	}

	live, err := r.getRoute(ctx, cfg.Namespace, route.GetName())
	if err != nil {
		return Result{}, fmt.Errorf("get gateway route %s: %w", route.GetName(), err)
	}
	if !routeAdmitted(live) {
		r.setCondition(cfg, costv1alpha1.ConditionGatewayReady, metav1.ConditionFalse,
			"RouteNotAdmitted", "waiting for API Route admission")
		return Result{RequeueAfter: requeueSlow}, nil
	}

	r.setCondition(cfg, costv1alpha1.ConditionGatewayReady, metav1.ConditionTrue,
		"GatewayReady", "Envoy JWT gateway and API Route are ready")

	if err := r.reconcileUI(ctx, cfg); err != nil {
		return Result{}, err
	}

	if apimeta.IsStatusConditionFalse(cfg.Status.Conditions, costv1alpha1.ConditionUIReady) {
		return Result{RequeueAfter: requeueSlow}, nil
	}
	return Result{}, nil
}

func (r *CostManagementServiceConfigReconciler) applyNetworkPolicies(ctx context.Context, cfg *costv1alpha1.CostManagementServiceConfig) error {
	netpols := []client.Object{
		resources.GatewayNetworkPolicy(cfg),
		resources.IngressNetworkPolicy(cfg),
		resources.RBACAPINetworkPolicy(cfg),
		resources.KokuAPINetworkPolicy(cfg),
		resources.UINetworkPolicy(cfg),
		resources.ListenerNetworkPolicy(cfg),
		resources.MasuNetworkPolicy(cfg),
	}
	if costv1alpha1.ROSEnabled(cfg) {
		netpols = append(netpols,
			resources.KruizeNetworkPolicy(cfg),
			resources.ROSAPINetworkPolicy(cfg),
		)
	}
	if costv1alpha1.BoolVal(cfg.Spec.Cache.Deploy, true) {
		netpols = append(netpols, resources.CacheNetworkPolicy(cfg))
	}
	if costv1alpha1.BoolVal(cfg.Spec.Database.Deploy, true) {
		netpols = append(netpols, resources.DatabaseNetworkPolicy(cfg))
	}
	for _, np := range netpols {
		if err := r.apply(ctx, cfg, np); err != nil {
			return fmt.Errorf("networkpolicy %s: %w", np.GetName(), err)
		}
	}
	return nil
}

func (r *CostManagementServiceConfigReconciler) applyUICookieAndNginx(ctx context.Context, cfg *costv1alpha1.CostManagementServiceConfig) error {
	if err := r.ensureSecret(ctx, cfg, resources.UICookieSecret(cfg)); err != nil {
		return fmt.Errorf("ui cookie secret: %w", err)
	}
	if err := r.apply(ctx, cfg, resources.UINginxConfigMap(cfg)); err != nil {
		return fmt.Errorf("ui %s: %w", resources.NameUINginxConfigMap(cfg), err)
	}
	return nil
}

// reconcileUI ensures cookie/nginx config, validates the user-provided OAuth
// client Secret, and applies UI Deploy/Service/Route/ConsoleLink when ready.
func (r *CostManagementServiceConfigReconciler) reconcileUI(ctx context.Context, cfg *costv1alpha1.CostManagementServiceConfig) error {
	if err := r.applyUICookieAndNginx(ctx, cfg); err != nil {
		return err
	}

	// OAuth client Secret is user-provided (same-namespace SecretRef). Gate UI
	// Deploy/Service/Route/ConsoleLink until client-id and client-secret exist.
	oauthSecretName := resources.NameUIOAuthClientSecret(cfg)
	var oauthSecret corev1.Secret
	err := r.Get(ctx, types.NamespacedName{Name: oauthSecretName, Namespace: cfg.Namespace}, &oauthSecret)
	switch {
	case errors.IsNotFound(err):
		r.setCondition(cfg, costv1alpha1.ConditionUIReady, metav1.ConditionFalse,
			"OAuthClientSecretMissing",
			fmt.Sprintf("Secret %q with keys client-id and client-secret is required for the UI", oauthSecretName))
		return nil
	case err != nil:
		return fmt.Errorf("get ui oauth client secret %s: %w", oauthSecretName, err)
	case resources.ValidateUIOAuthClientSecret(&oauthSecret) != nil:
		r.setCondition(cfg, costv1alpha1.ConditionUIReady, metav1.ConditionFalse,
			"OAuthClientSecretInvalid",
			fmt.Sprintf("Secret %q must contain non-empty keys client-id and client-secret", oauthSecretName))
		return nil
	}

	for _, obj := range []client.Object{
		resources.UIDeployment(cfg),
		resources.UIService(cfg),
	} {
		if err := r.apply(ctx, cfg, obj); err != nil {
			return fmt.Errorf("ui %s: %w", obj.GetName(), err)
		}
	}

	if uiRoute := resources.UIRoute(cfg); uiRoute != nil {
		if err := r.apply(ctx, cfg, uiRoute); err != nil {
			return fmt.Errorf("ui route: %w", err)
		}
		live, err := r.getRoute(ctx, cfg.Namespace, uiRoute.GetName())
		if err != nil {
			return fmt.Errorf("get ui route %s: %w", uiRoute.GetName(), err)
		}
		if !routeAdmitted(live) {
			r.setCondition(cfg, costv1alpha1.ConditionUIReady, metav1.ConditionFalse,
				"RouteNotAdmitted", "waiting for UI Route admission")
			return nil
		}
	}

	r.setCondition(cfg, costv1alpha1.ConditionUIReady, metav1.ConditionTrue,
		"OAuthClientSecretReady", "UI OAuth client Secret is present")

	if err := r.applyClusterScoped(ctx, resources.ConsoleLink(cfg)); err != nil {
		return fmt.Errorf("consolelink: %w", err)
	}
	return nil
}

// -----------------------------------------------------------------------------
// Stage 8 — Monitoring (PrometheusRules + App ServiceMonitor)
// -----------------------------------------------------------------------------

// reconcileMonitoring applies App/Gateway/Operator ServiceMonitors and
// PrometheusRules when spec.monitoring.enabled is true (the default). When
// disabled, best-effort deletes those managed objects so Alerting/scrape
// targets do not linger. Kruize ServiceMonitor is not applied here yet
// (ROS scrape → COST-8054) but is still deleted on disable so a future or
// manually present object does not linger. CRDs absent → skip one resource
// and continue.
func (r *CostManagementServiceConfigReconciler) reconcileMonitoring(ctx context.Context, cfg *costv1alpha1.CostManagementServiceConfig) (Result, error) {
	logger := log.FromContext(ctx)
	objs := []client.Object{
		resources.AppServiceMonitor(cfg),
		resources.GatewayServiceMonitor(cfg),
		resources.OperatorServiceMonitor(cfg),
		resources.PrometheusRules(cfg),
	}

	if !costv1alpha1.BoolVal(cfg.Spec.Monitoring.Enabled, true) {
		// Kruize SM is not in the apply set yet (COST-8054); still delete by
		// name so disable cleans it up once ROS scrape lands (or if present).
		toDelete := append(objs, resources.KruizeServiceMonitor(cfg))
		for _, obj := range toDelete {
			obj.SetNamespace(cfg.Namespace)
			if err := r.Delete(ctx, obj); err != nil && !errors.IsNotFound(err) && !apimeta.IsNoMatchError(err) {
				gvk := obj.GetObjectKind().GroupVersionKind()
				return Result{}, fmt.Errorf("delete monitoring %s %s: %w", gvk.Kind, obj.GetName(), err)
			}
		}
		return Result{}, nil
	}

	for _, obj := range objs {
		if err := r.apply(ctx, cfg, obj); err != nil {
			gvk := obj.GetObjectKind().GroupVersionKind()
			if apimeta.IsNoMatchError(err) {
				logger.Info("monitoring resource skipped (CRD absent)",
					"kind", gvk.Kind, "name", obj.GetName())
				continue
			}
			return Result{}, fmt.Errorf("monitoring %s %s: %w", gvk.Kind, obj.GetName(), err)
		}
	}
	return Result{}, nil
}

// -----------------------------------------------------------------------------
// Apply / create helpers
// -----------------------------------------------------------------------------

// applyClusterScoped applies a cluster-scoped resource (no namespace, no ownerRef).
// These resources require finalizer-based cleanup — see COST-7681.
func (r *CostManagementServiceConfigReconciler) applyClusterScoped(ctx context.Context, obj client.Object) error {
	return r.Patch(ctx, obj, client.Apply, client.ForceOwnership, client.FieldOwner(fieldOwner))
}

// apply creates or updates obj using Server-Side Apply.
func (r *CostManagementServiceConfigReconciler) apply(ctx context.Context, cfg *costv1alpha1.CostManagementServiceConfig, obj client.Object) error {
	obj.SetNamespace(cfg.Namespace)
	setOwnerRef(cfg, obj)
	return r.Patch(ctx, obj, client.Apply, client.ForceOwnership, client.FieldOwner(fieldOwner))
}

func routeGVK() schema.GroupVersionKind {
	return schema.GroupVersionKind{Group: "route.openshift.io", Version: "v1", Kind: routeKind}
}

func (r *CostManagementServiceConfigReconciler) getRoute(ctx context.Context, namespace, name string) (*unstructured.Unstructured, error) {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(routeGVK())
	if err := r.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, u); err != nil {
		return nil, err
	}
	return u, nil
}

// ensureServiceAccount applies sa when spec.Create is true (the default).
// When create=false, it only verifies the named ServiceAccount already exists —
// it never applies, sets ownerRefs, or otherwise mutates the object (so deleting
// the CR cannot garbage-collect a user-managed SA).
func (r *CostManagementServiceConfigReconciler) ensureServiceAccount(
	ctx context.Context,
	cfg *costv1alpha1.CostManagementServiceConfig,
	spec costv1alpha1.ServiceAccountSpec,
	sa *corev1.ServiceAccount,
) error {
	if !costv1alpha1.BoolVal(spec.Create, true) {
		existing := &corev1.ServiceAccount{}
		key := types.NamespacedName{Namespace: cfg.Namespace, Name: sa.Name}
		if err := r.Get(ctx, key, existing); err != nil {
			if errors.IsNotFound(err) {
				return fmt.Errorf("serviceaccount %s/%s not found (create=false); create it manually or set create=true", key.Namespace, key.Name)
			}
			return fmt.Errorf("get serviceaccount %s/%s: %w", key.Namespace, key.Name, err)
		}
		return nil
	}
	return r.apply(ctx, cfg, sa)
}

// applyStatefulSet applies a StatefulSet using Server-Side Apply (SSA).
// VolumeClaimTemplates are immutable; if they differ from the existing
// StatefulSet, we set DatabaseReady/Available=False and Degraded=True with
// StorageConfigChanged and skip the SSA apply, returning ErrStorageConfigChanged
// so the caller stops before later phases overwrite those conditions.
// When VCT matches, the live templates are copied onto desired so SSA never
// proposes a VCT update (API defaulting / managedFields must not 403).
func (r *CostManagementServiceConfigReconciler) applyStatefulSet(ctx context.Context, cfg *costv1alpha1.CostManagementServiceConfig, desired *appsv1.StatefulSet) error {
	existing := &appsv1.StatefulSet{}
	err := r.Get(ctx, types.NamespacedName{Namespace: desired.Namespace, Name: desired.Name}, existing)
	if errors.IsNotFound(err) {
		return r.apply(ctx, cfg, desired)
	}
	if err != nil {
		return err
	}

	// Check if VolumeClaimTemplates differ (immutable field).
	if !volumeClaimTemplatesEqual(existing.Spec.VolumeClaimTemplates, desired.Spec.VolumeClaimTemplates) {
		diff := volumeClaimTemplateDiff(existing.Spec.VolumeClaimTemplates, desired.Spec.VolumeClaimTemplates)
		msg := fmt.Sprintf("immutable VolumeClaimTemplates change (%s); revert spec.database.storage.size or spec.global.storageClass to match the live volume, or delete the StatefulSet and PVC to recreate", diff)
		r.setCondition(cfg, costv1alpha1.ConditionDatabaseReady, metav1.ConditionFalse, "StorageConfigChanged", msg)
		r.setCondition(cfg, costv1alpha1.ConditionAvailable, metav1.ConditionFalse, "StorageConfigChanged", msg)
		r.setCondition(cfg, costv1alpha1.ConditionDegraded, metav1.ConditionTrue, "StorageConfigChanged", msg)
		r.setCondition(cfg, costv1alpha1.ConditionProgressing, metav1.ConditionFalse, "StorageConfigChanged", msg)
		cfg.Status.Phase = costv1alpha1.PhaseDegraded
		r.Recorder.Eventf(cfg, corev1.EventTypeWarning, "StorageConfigChanged", "%s", msg)
		return ErrStorageConfigChanged
	}

	// Stamp live VCT onto desired so SSA only updates mutable fields.
	desired.Spec.VolumeClaimTemplates = append([]corev1.PersistentVolumeClaim(nil), existing.Spec.VolumeClaimTemplates...)
	return r.apply(ctx, cfg, desired)
}

// ErrStorageConfigChanged signals that VolumeClaimTemplates differ and
// reconciliation should stop to avoid overwriting the StorageConfigChanged condition.
var ErrStorageConfigChanged = fmt.Errorf("storage config changed: VolumeClaimTemplates are immutable")

// volumeClaimTemplateDiff summarizes live vs desired VolumeClaimTemplate
// changes for status messages. Size and storageClass are the CR-driven fields.
func volumeClaimTemplateDiff(live, desired []corev1.PersistentVolumeClaim) string {
	if len(live) != len(desired) {
		return fmt.Sprintf("count %d -> %d", len(live), len(desired))
	}
	var parts []string
	for i := range live {
		name := live[i].Name
		if live[i].Name != desired[i].Name {
			parts = append(parts, fmt.Sprintf("name %q -> %q", live[i].Name, desired[i].Name))
			name = desired[i].Name
		}
		liveSize := live[i].Spec.Resources.Requests[corev1.ResourceStorage]
		desiredSize := desired[i].Spec.Resources.Requests[corev1.ResourceStorage]
		if !liveSize.Equal(desiredSize) {
			parts = append(parts, fmt.Sprintf("%s size %s -> %s", name, liveSize.String(), desiredSize.String()))
		}
		if !ptrEqual(live[i].Spec.StorageClassName, desired[i].Spec.StorageClassName) {
			parts = append(parts, fmt.Sprintf("%s storageClass %s -> %s", name, pvcStorageClass(live[i]), pvcStorageClass(desired[i])))
		}
	}
	if len(parts) == 0 {
		return "other immutable fields differ"
	}
	return strings.Join(parts, "; ")
}

func pvcStorageClass(pvc corev1.PersistentVolumeClaim) string {
	if pvc.Spec.StorageClassName == nil || *pvc.Spec.StorageClassName == "" {
		return "<unset>"
	}
	return *pvc.Spec.StorageClassName
}

// volumeClaimTemplatesEqual compares two VolumeClaimTemplate slices for equality
// of the immutable fields. It normalizes defaulted values per Kubernetes API
// conventions before comparison to avoid false mismatches.
// Compared fields: Name, Labels, Annotations, StorageClassName, Resources
// (Requests & Limits), AccessModes, VolumeMode, Selector, VolumeName,
// DataSource, DataSourceRef (including Namespace), VolumeAttributesClassName.
func volumeClaimTemplatesEqual(a, b []corev1.PersistentVolumeClaim) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].Name != b[i].Name {
			return false
		}
		if !maps.Equal(a[i].Labels, b[i].Labels) {
			return false
		}
		if !maps.Equal(a[i].Annotations, b[i].Annotations) {
			return false
		}
		if !ptrEqual(a[i].Spec.StorageClassName, b[i].Spec.StorageClassName) {
			return false
		}
		if !resourceRequirementsEqual(a[i].Spec.Resources, b[i].Spec.Resources) {
			return false
		}
		if !accessModesEqual(a[i].Spec.AccessModes, b[i].Spec.AccessModes) {
			return false
		}
		if !volumeModeEqual(a[i].Spec.VolumeMode, b[i].Spec.VolumeMode) {
			return false
		}
		if !labelSelectorEqual(a[i].Spec.Selector, b[i].Spec.Selector) {
			return false
		}
		if !volumeNameEqual(a[i].Spec.VolumeName, b[i].Spec.VolumeName) {
			return false
		}
		if !dataSourceEqual(a[i].Spec.DataSource, b[i].Spec.DataSource) {
			return false
		}
		if !dataSourceRefEqual(a[i].Spec.DataSourceRef, b[i].Spec.DataSourceRef) {
			return false
		}
		if !volumeAttributesClassNameEqual(a[i].Spec.VolumeAttributesClassName, b[i].Spec.VolumeAttributesClassName) {
			return false
		}
	}
	return true
}

func resourceRequirementsEqual(a, b corev1.VolumeResourceRequirements) bool {
	// Compare both Requests and Limits, treating nil and empty as equivalent
	// to handle Kubernetes API defaulting.
	if !resourceListEqual(a.Requests, b.Requests) {
		return false
	}
	if !resourceListEqual(a.Limits, b.Limits) {
		return false
	}
	return true
}

func resourceListEqual(a, b corev1.ResourceList) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if v2, ok := b[k]; !ok || !v.Equal(v2) {
			return false
		}
	}
	return true
}

func accessModesEqual(a, b []corev1.PersistentVolumeAccessMode) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func volumeModeEqual(a, b *corev1.PersistentVolumeMode) bool {
	// PersistentVolumeClaimSpec.VolumeMode defaults to Filesystem when unset
	// (Kubernetes API defaulting). Treat nil and Filesystem as equal so live
	// vs desired comparisons do not false-mismatch after defaulting.
	if a == nil && b == nil {
		return true
	}
	if a == nil && b != nil && *b == corev1.PersistentVolumeFilesystem {
		return true
	}
	if b == nil && a != nil && *a == corev1.PersistentVolumeFilesystem {
		return true
	}
	if a != nil && b != nil {
		return *a == *b
	}
	return false
}

func labelSelectorEqual(a, b *metav1.LabelSelector) bool {
	if a == nil && b == nil {
		return true
	}
	if a == nil || b == nil {
		return false
	}
	if len(a.MatchLabels) != len(b.MatchLabels) {
		return false
	}
	for k, v := range a.MatchLabels {
		if v2, ok := b.MatchLabels[k]; !ok || v != v2 {
			return false
		}
	}
	if len(a.MatchExpressions) != len(b.MatchExpressions) {
		return false
	}
	for i := range a.MatchExpressions {
		if a.MatchExpressions[i].Key != b.MatchExpressions[i].Key ||
			a.MatchExpressions[i].Operator != b.MatchExpressions[i].Operator {
			return false
		}
		if len(a.MatchExpressions[i].Values) != len(b.MatchExpressions[i].Values) {
			return false
		}
		for j := range a.MatchExpressions[i].Values {
			if a.MatchExpressions[i].Values[j] != b.MatchExpressions[i].Values[j] {
				return false
			}
		}
	}
	return true
}

func volumeNameEqual(a, b string) bool {
	// Empty string and nil are equivalent (Kubernetes treats unset as empty)
	return a == b
}

func dataSourceEqual(a, b *corev1.TypedLocalObjectReference) bool {
	if a == nil && b == nil {
		return true
	}
	if a == nil || b == nil {
		return false
	}
	return ptrEqual(a.APIGroup, b.APIGroup) && a.Kind == b.Kind && a.Name == b.Name
}

func dataSourceRefEqual(a, b *corev1.TypedObjectReference) bool {
	if a == nil && b == nil {
		return true
	}
	if a == nil || b == nil {
		return false
	}
	return ptrEqual(a.APIGroup, b.APIGroup) && a.Kind == b.Kind && a.Name == b.Name && ptrEqual(a.Namespace, b.Namespace)
}

func volumeAttributesClassNameEqual(a, b *string) bool {
	if a == nil && b == nil {
		return true
	}
	if a == nil || b == nil {
		return false
	}
	return *a == *b
}

func ptrEqual(a, b *string) bool {
	if a == nil && b == nil {
		return true
	}
	if a == nil || b == nil {
		return false
	}
	return *a == *b
}

// ensureSecret creates the secret only if it does not already exist.
// Existing secrets are never overwritten to preserve generated credentials.
func (r *CostManagementServiceConfigReconciler) ensureSecret(ctx context.Context, cfg *costv1alpha1.CostManagementServiceConfig, secret *corev1.Secret) error {
	existing := &corev1.Secret{}
	err := r.Get(ctx, types.NamespacedName{Namespace: secret.Namespace, Name: secret.Name}, existing)
	if errors.IsNotFound(err) {
		setOwnerRef(cfg, secret)
		return r.Create(ctx, secret)
	}
	return err
}

func setOwnerRef(owner *costv1alpha1.CostManagementServiceConfig, obj client.Object) {
	if obj.GetNamespace() == "" {
		return // cluster-scoped: owner refs don't apply, use finalizer instead
	}
	isController := true
	blockDeletion := true
	ref := metav1.OwnerReference{
		APIVersion:         costv1alpha1.GroupVersion.String(),
		Kind:               "CostManagementServiceConfig",
		Name:               owner.Name,
		UID:                owner.UID,
		Controller:         &isController,
		BlockOwnerDeletion: &blockDeletion,
	}
	refs := obj.GetOwnerReferences()
	for i, r := range refs {
		if r.Kind == ref.Kind && r.Name == ref.Name {
			refs[i] = ref
			obj.SetOwnerReferences(refs)
			return
		}
	}
	obj.SetOwnerReferences(append(refs, ref))
}

// -----------------------------------------------------------------------------
// Readiness helpers
// -----------------------------------------------------------------------------

func (r *CostManagementServiceConfigReconciler) isDeploymentReady(ctx context.Context, ns, name string) (bool, error) {
	d := &appsv1.Deployment{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, d); err != nil {
		if errors.IsNotFound(err) {
			return false, nil
		}
		return false, err
	}
	if d.Spec.Replicas == nil || *d.Spec.Replicas == 0 {
		return true, nil // 0 replicas = intentionally off
	}
	return d.Status.AvailableReplicas >= *d.Spec.Replicas, nil
}

type deploymentWait struct {
	name, reason, message, component string
}

// holdingWorkerReadinessWait is true when a later phase already set
// Available=False for a worker Deployment. Core must not overwrite that
// wait clock. Keep this reason list in sync with waitForWorkerReadiness
// (docs/review-follow-ups.md #12 if Ingress is routed through notReadyWait).
func holdingWorkerReadinessWait(cfg *costv1alpha1.CostManagementServiceConfig) bool {
	existing := apimeta.FindStatusCondition(cfg.Status.Conditions, costv1alpha1.ConditionAvailable)
	if existing == nil || existing.Status != metav1.ConditionFalse {
		return false
	}
	switch existing.Reason {
	case reasonWaitingForROSAPI, reasonWaitingForROSProcessor:
		return true
	default:
		return false
	}
}

func (r *CostManagementServiceConfigReconciler) waitForDeployments(
	ctx context.Context,
	cfg *costv1alpha1.CostManagementServiceConfig,
	waits []deploymentWait,
) (Result, error) {
	for _, w := range waits {
		ready, err := r.isDeploymentReady(ctx, cfg.Namespace, w.name)
		if err != nil {
			return Result{}, err
		}
		if !ready {
			return r.notReadyWait(cfg, w.reason, w.message, w.component), nil
		}
	}
	r.clearDeploymentNotReady(cfg)
	return Result{}, nil
}

// notReadyWait records Available=False for a named Deployment. After
// readinessTimeout it sets Degraded=True reason DeploymentNotReady and
// switches to stepped backoff (30s → 1m → 2m → 5m).
func (r *CostManagementServiceConfigReconciler) notReadyWait(
	cfg *costv1alpha1.CostManagementServiceConfig,
	reason, message, component string,
) Result {
	existing := apimeta.FindStatusCondition(cfg.Status.Conditions, costv1alpha1.ConditionAvailable)
	if existing != nil && (existing.Status != metav1.ConditionFalse || existing.Reason != reason) {
		apimeta.RemoveStatusCondition(&cfg.Status.Conditions, costv1alpha1.ConditionAvailable)
	}
	r.setCondition(cfg, costv1alpha1.ConditionAvailable, metav1.ConditionFalse, reason, message)
	existing = apimeta.FindStatusCondition(cfg.Status.Conditions, costv1alpha1.ConditionAvailable)
	if existing != nil && time.Since(existing.LastTransitionTime.Time) >= readinessTimeout {
		r.setCondition(cfg, costv1alpha1.ConditionDegraded, metav1.ConditionTrue, reasonDeploymentNotReady,
			fmt.Sprintf("%s is not ready", component))
		cfg.Status.Phase = costv1alpha1.PhaseDegraded
		return Result{RequeueAfter: readinessBackoff(existing.LastTransitionTime.Time)}
	}
	r.clearDeploymentNotReady(cfg)
	return Result{RequeueAfter: requeueSlow}
}

// clearDeploymentNotReady flips Degraded=False only when the reason is
// DeploymentNotReady. ReconcileError and migration Degraded stay put.
func (r *CostManagementServiceConfigReconciler) clearDeploymentNotReady(cfg *costv1alpha1.CostManagementServiceConfig) {
	existing := apimeta.FindStatusCondition(cfg.Status.Conditions, costv1alpha1.ConditionDegraded)
	if existing == nil || existing.Status != metav1.ConditionTrue || existing.Reason != reasonDeploymentNotReady {
		return
	}
	r.setCondition(cfg, costv1alpha1.ConditionDegraded, metav1.ConditionFalse, reasonDeploymentReady, "")
	if cfg.Status.Phase == costv1alpha1.PhaseDegraded {
		cfg.Status.Phase = costv1alpha1.PhaseProgressing
	}
}

func readinessBackoff(since time.Time) time.Duration {
	waited := time.Since(since) - readinessTimeout
	switch {
	case waited < 30*time.Second:
		return 30 * time.Second
	case waited < time.Minute:
		return time.Minute
	case waited < 3*time.Minute:
		return 2 * time.Minute
	default:
		return 5 * time.Minute
	}
}

func (r *CostManagementServiceConfigReconciler) isStatefulSetReady(ctx context.Context, ns, name string) (bool, error) {
	ss := &appsv1.StatefulSet{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, ss); err != nil {
		if errors.IsNotFound(err) {
			return false, nil
		}
		return false, err
	}
	if ss.Spec.Replicas == nil || *ss.Spec.Replicas == 0 {
		return true, nil
	}
	want := *ss.Spec.Replicas
	// ObservedGeneration lags metadata.generation until the controller has
	// seen the SSA-applied spec. ReadyReplicas alone can still be the old
	// revision's pods.
	if ss.Status.ObservedGeneration < ss.Generation {
		return false, nil
	}
	if ss.Status.ReadyReplicas < want {
		return false, nil
	}
	if ss.Status.UpdateRevision != "" && ss.Status.CurrentRevision != ss.Status.UpdateRevision {
		return false, nil
	}
	// UpdatedReplicas is 0 on first create until an update revision exists;
	// once the controller reports any updated pods, all replicas must match.
	if ss.Status.UpdatedReplicas > 0 && ss.Status.UpdatedReplicas < want {
		return false, nil
	}
	return true, nil
}

func isJobComplete(j *batchv1.Job) bool {
	for _, c := range j.Status.Conditions {
		if c.Type == batchv1.JobComplete && c.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

func isJobFailed(j *batchv1.Job) bool {
	for _, c := range j.Status.Conditions {
		if c.Type == batchv1.JobFailed && c.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

// -----------------------------------------------------------------------------
// Status helpers
// -----------------------------------------------------------------------------

func (r *CostManagementServiceConfigReconciler) patchStatus(ctx context.Context, original, updated *costv1alpha1.CostManagementServiceConfig) error {
	return r.Status().Patch(ctx, updated, client.MergeFrom(original))
}

func (r *CostManagementServiceConfigReconciler) setCondition(cfg *costv1alpha1.CostManagementServiceConfig, condType string, status metav1.ConditionStatus, reason, message string) {
	apimeta.SetStatusCondition(&cfg.Status.Conditions, metav1.Condition{
		Type:               condType,
		Status:             status,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: cfg.Generation,
	})
	opmetrics.SetCondition(cfg.Namespace, cfg.Name, condType, status)
}

// syncConditionMetrics mirrors all CMSC conditions into Prometheus gauges
// (COST-8108). Safe to call after phases so applyPhaseError paths are included.
func (r *CostManagementServiceConfigReconciler) syncConditionMetrics(cfg *costv1alpha1.CostManagementServiceConfig) {
	for _, c := range cfg.Status.Conditions {
		opmetrics.SetCondition(cfg.Namespace, cfg.Name, c.Type, c.Status)
	}
}

// syncManagedPodRestarts publishes restart counts for instance-labeled pods so
// CostManagementPodRestarting can evaluate under UWM without kube-state metrics.
// After a successful list, existing series for this CMSC are cleared so deleted
// or replaced pods cannot leave stale gauges that keep the alert firing.
func (r *CostManagementServiceConfigReconciler) syncManagedPodRestarts(ctx context.Context, cfg *costv1alpha1.CostManagementServiceConfig) error {
	var pods corev1.PodList
	if err := r.List(ctx, &pods, client.InNamespace(cfg.Namespace), client.MatchingLabels{
		"app.kubernetes.io/instance":   cfg.Name,
		"app.kubernetes.io/managed-by": "koku-service-operator",
	}); err != nil {
		return err
	}
	opmetrics.ClearManagedPodRestarts(cfg.Namespace, cfg.Name)
	for i := range pods.Items {
		p := &pods.Items[i]
		for _, cs := range p.Status.ContainerStatuses {
			opmetrics.ManagedPodRestarts.WithLabelValues(cfg.Namespace, cfg.Name, p.Name, cs.Name).Set(float64(cs.RestartCount))
		}
	}
	return nil
}

// emitPhaseChanged emits PhaseChanged when the reconcile settles on a phase
// different from the phase observed at the start of the pass.
func (r *CostManagementServiceConfigReconciler) emitPhaseChanged(cfg *costv1alpha1.CostManagementServiceConfig, from, to costv1alpha1.Phase) {
	if r.Recorder == nil || from == to || to == "" {
		return
	}
	fromLabel := string(from)
	if fromLabel == "" {
		fromLabel = "None"
	}
	r.Recorder.Eventf(cfg, corev1.EventTypeNormal, "PhaseChanged",
		"Phase changed from %s to %s", fromLabel, to)
}

// emitDependencyFailed emits DependencyFailed when a blocking dependency
// condition transitions to False (operator validation signal, not BYOI scrape).
func (r *CostManagementServiceConfigReconciler) emitDependencyFailed(cfg *costv1alpha1.CostManagementServiceConfig, condType, message string) {
	if r.Recorder == nil {
		return
	}
	if apimeta.IsStatusConditionFalse(cfg.Status.Conditions, condType) {
		return // already False — avoid spam on requeue
	}
	r.Recorder.Eventf(cfg, corev1.EventTypeWarning, "DependencyFailed",
		"%s: %s", condType, message)
}

// -----------------------------------------------------------------------------
// Controller registration
// -----------------------------------------------------------------------------

func (r *CostManagementServiceConfigReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&costv1alpha1.CostManagementServiceConfig{}).
		Owns(&appsv1.Deployment{}).
		Owns(&appsv1.StatefulSet{}).
		Owns(&batchv1.Job{}).
		Owns(&batchv1.CronJob{}).
		Owns(&corev1.Service{}).
		Owns(&corev1.ConfigMap{}).
		Owns(&corev1.Secret{}).
		Owns(&corev1.PersistentVolumeClaim{}).
		Owns(&corev1.ServiceAccount{}).
		Owns(&networkingv1.NetworkPolicy{}).
		Complete(r)
}
