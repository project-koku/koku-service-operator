package controller

import (
	"context"
	"fmt"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	costv1alpha1 "github.com/project-koku/koku-service-operator/api/v1alpha1"
	"github.com/project-koku/koku-service-operator/internal/resources"
)

const (
	fieldOwner    = "koku-service-operator"
	finalizerName = "costmanagementserviceconfigs.service.costmanagement.openshift.io/cleanup"
	requeueFast   = 10 * time.Second
	requeueSlow   = 30 * time.Second
	requeueDrift  = 5 * time.Minute
)

type CostManagementServiceConfigReconciler struct {
	client.Client
	// APIReader is an uncached client for cross-namespace reads (e.g. NooBaa
	// admin Secret in openshift-storage) that are outside Cache.DefaultNamespaces.
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
// +kubebuilder:rbac:groups=monitoring.coreos.com,resources=servicemonitors;prometheusrules,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=route.openshift.io,resources=routes,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=route.openshift.io,resources=routes/custom-host,verbs=create
// Namespace-scoped RBAC objects (Role + RoleBinding) — granted via RoleBinding.
// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=roles;rolebindings,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=networking.k8s.io,resources=networkpolicies,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core,resources=events,verbs=create;patch
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

	original := cfg.DeepCopy()

	result, reconcileErr := r.reconcile(ctx, cfg)

	if patchErr := r.patchStatus(ctx, original, cfg); patchErr != nil {
		logger.Error(patchErr, "failed to patch status")
		if reconcileErr == nil {
			return result, patchErr
		}
	}

	return result, reconcileErr
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
		r.Recorder.Eventf(cfg, corev1.EventTypeWarning, "ReconcileError", "%v", err)
		return ctrl.Result{RequeueAfter: requeueSlow}, err
	}
	if !result.IsZero() {
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

	return Result{}, nil
}

// -----------------------------------------------------------------------------
// Stage 2 — Infrastructure
// -----------------------------------------------------------------------------

func (r *CostManagementServiceConfigReconciler) reconcileInfrastructure(ctx context.Context, cfg *costv1alpha1.CostManagementServiceConfig) (Result, error) {
	if costv1alpha1.BoolVal(cfg.Spec.Database.Deploy, true) {
		if err := r.apply(ctx, cfg, resources.DatabaseService(cfg)); err != nil {
			return Result{}, fmt.Errorf("database service: %w", err)
		}
		if err := r.applyStatefulSet(ctx, cfg, resources.DatabaseStatefulSet(cfg)); err != nil {
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
	// ROS schema migrate only when ROS is enabled — otherwise the Job uses an
	// empty image tag (image ":") and fails DeadlineExceeded.
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
	r.setCondition(cfg, costv1alpha1.ConditionSchemaUpToDate, metav1.ConditionTrue, "MigrationComplete", "all schema migrations succeeded")
	r.setCondition(cfg, costv1alpha1.ConditionDegraded, metav1.ConditionFalse, "MigrationSucceeded", "")
	r.Recorder.Event(cfg, corev1.EventTypeNormal, "MigrationComplete", "All schema migrations succeeded (Koku → ROS → RBAC)")
	return Result{}, nil
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
	if existing.Annotations["koku.costmanagement.io/image-tag"] != imageTag {
		if delErr := r.Delete(ctx, existing, client.PropagationPolicy(metav1.DeletePropagationBackground)); delErr != nil && !errors.IsNotFound(delErr) {
			return Result{}, fmt.Errorf("delete stale %s: %w", jobName, delErr)
		}
		progress("restarting for new image")
		return Result{RequeueAfter: requeueFast}, nil
	}

	if isJobComplete(existing) {
		return Result{}, nil // proceed to next step
	}
	if isJobFailed(existing) {
		msg := fmt.Sprintf("%s exhausted retries — check pod logs", jobName)
		r.setCondition(cfg, costv1alpha1.ConditionSchemaUpToDate, metav1.ConditionFalse, "MigrationFailed", msg)
		r.setCondition(cfg, costv1alpha1.ConditionDegraded, metav1.ConditionTrue, "MigrationFailed", msg)
		cfg.Status.Phase = costv1alpha1.PhaseDegraded
		r.Recorder.Eventf(cfg, corev1.EventTypeWarning, "MigrationFailed",
			"Migration job %s exhausted retries — manual intervention required", jobName)
		return Result{Stop: true}, nil
	}

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

	// Gate on the API being available.
	ready, err := r.isDeploymentReady(ctx, cfg.Namespace, resources.NameKokuAPI(cfg))
	if err != nil {
		return Result{}, err
	}
	if !ready {
		r.setCondition(cfg, costv1alpha1.ConditionAvailable, metav1.ConditionFalse, "WaitingForAPI", "waiting for Koku API")
		return Result{RequeueAfter: requeueSlow}, nil
	}
	r.setCondition(cfg, costv1alpha1.ConditionAvailable, metav1.ConditionTrue, "KokuAvailable", "")
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

	ready, err := r.isDeploymentReady(ctx, cfg.Namespace, resources.NameEnvoy(cfg))
	if err != nil {
		return Result{}, err
	}
	if !ready {
		r.setCondition(cfg, costv1alpha1.ConditionAuthReady, metav1.ConditionFalse,
			"WaitingForGateway", "waiting for Envoy gateway Deployment")
		return Result{RequeueAfter: requeueSlow}, nil
	}

	if route == nil {
		r.setCondition(cfg, costv1alpha1.ConditionAuthReady, metav1.ConditionFalse,
			"ClusterDomainPending", "Envoy gateway ready; API Route deferred until cluster domain is available")
		return Result{RequeueAfter: requeueSlow}, nil
	}

	r.setCondition(cfg, costv1alpha1.ConditionAuthReady, metav1.ConditionTrue,
		"GatewayReady", "Envoy JWT gateway and API Route are ready")

	if err := r.reconcileUI(ctx, cfg); err != nil {
		return Result{}, err
	}

	// NetworkPolicies — restrict traffic to expected flows per component.
	nps := []client.Object{
		resources.GatewayNetworkPolicy(cfg),
		resources.IngressNetworkPolicy(cfg),
		resources.RBACAPINetworkPolicy(cfg),
		resources.KokuAPINetworkPolicy(cfg),
	}
	if costv1alpha1.ROSEnabled(cfg) {
		nps = append(nps,
			resources.KruizeNetworkPolicy(cfg),
			resources.ROSAPINetworkPolicy(cfg),
		)
	}
	for _, np := range nps {
		if err := r.apply(ctx, cfg, np); err != nil {
			return Result{}, fmt.Errorf("networkpolicy %s: %w", np.GetName(), err)
		}
	}

	if apimeta.IsStatusConditionFalse(cfg.Status.Conditions, costv1alpha1.ConditionUIReady) {
		return Result{RequeueAfter: requeueSlow}, nil
	}
	return Result{}, nil
}

// reconcileUI ensures cookie/nginx config, validates the user-provided OAuth
// client Secret, and applies UI Deploy/Service/Route/ConsoleLink when ready.
func (r *CostManagementServiceConfigReconciler) reconcileUI(ctx context.Context, cfg *costv1alpha1.CostManagementServiceConfig) error {
	// Cookie secret + nginx ConfigMap are safe without OAuth credentials.
	if err := r.ensureSecret(ctx, cfg, resources.UICookieSecret(cfg)); err != nil {
		return fmt.Errorf("ui cookie secret: %w", err)
	}
	if err := r.apply(ctx, cfg, resources.UINginxConfigMap(cfg)); err != nil {
		return fmt.Errorf("ui %s: %w", resources.NameUINginxConfigMap(cfg), err)
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

	r.setCondition(cfg, costv1alpha1.ConditionUIReady, metav1.ConditionTrue,
		"OAuthClientSecretReady", "UI OAuth client Secret is present")

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
	}

	if err := r.applyClusterScoped(ctx, resources.ConsoleLink(cfg)); err != nil {
		return fmt.Errorf("consolelink: %w", err)
	}
	return nil
}

// -----------------------------------------------------------------------------
// Stage 8 — Monitoring (ServiceMonitors + PrometheusRules)
// -----------------------------------------------------------------------------

// reconcileMonitoring applies ServiceMonitor and PrometheusRule objects when
// spec.monitoring.enabled is true (the default). Both types are Prometheus
// Operator CRDs; the stage is silently skipped when those CRDs are absent so
// the operator works on clusters without the monitoring stack.
func (r *CostManagementServiceConfigReconciler) reconcileMonitoring(ctx context.Context, cfg *costv1alpha1.CostManagementServiceConfig) (Result, error) {
	if !costv1alpha1.BoolVal(cfg.Spec.Monitoring.Enabled, true) {
		return Result{}, nil
	}
	for _, obj := range []client.Object{
		resources.AppServiceMonitor(cfg),
		resources.KruizeServiceMonitor(cfg),
		resources.PrometheusRules(cfg),
	} {
		if err := r.apply(ctx, cfg, obj); err != nil {
			gvk := obj.GetObjectKind().GroupVersionKind()
			if apimeta.IsNoMatchError(err) {
				// Prometheus Operator CRDs not installed — expected on clusters
				// without the monitoring stack. Skip silently.
				log.FromContext(ctx).Info("monitoring resource skipped (CRD absent)",
					"kind", gvk.Kind, "name", obj.GetName())
				continue
			}
			// Real error (permissions, API server issues, etc.) — surface it so
			// the reconcile loop sets Degraded and the user is notified.
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

// applyStatefulSet applies a StatefulSet, handling the VolumeClaimTemplate
// immutability constraint by creating on first call and only patching spec
// (not VCT) on subsequent calls.
func (r *CostManagementServiceConfigReconciler) applyStatefulSet(ctx context.Context, cfg *costv1alpha1.CostManagementServiceConfig, desired *appsv1.StatefulSet) error {
	existing := &appsv1.StatefulSet{}
	err := r.Get(ctx, types.NamespacedName{Namespace: desired.Namespace, Name: desired.Name}, existing)
	if errors.IsNotFound(err) {
		setOwnerRef(cfg, desired)
		return r.Create(ctx, desired)
	}
	if err != nil {
		return err
	}
	// Only update mutable fields (replicas, container image, resources, pull secrets).
	patch := existing.DeepCopy()
	patch.Spec.Replicas = desired.Spec.Replicas
	patch.Spec.Template.Spec.ImagePullSecrets = desired.Spec.Template.Spec.ImagePullSecrets
	if len(patch.Spec.Template.Spec.Containers) > 0 && len(desired.Spec.Template.Spec.Containers) > 0 {
		patch.Spec.Template.Spec.Containers[0].Image = desired.Spec.Template.Spec.Containers[0].Image
		patch.Spec.Template.Spec.Containers[0].Resources = desired.Spec.Template.Spec.Containers[0].Resources
		patch.Spec.Template.Spec.Containers[0].Env = desired.Spec.Template.Spec.Containers[0].Env
	}
	return r.Update(ctx, patch)
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
	return ss.Status.ReadyReplicas >= *ss.Spec.Replicas, nil
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
