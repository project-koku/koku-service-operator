package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	cost "github.com/project-koku/koku-service-operator/api/v1alpha1"
	"github.com/project-koku/koku-service-operator/internal/resources"
)

const (
	reasonWaitingForApplicationRollout = "WaitingForApplicationRollout"
	replicaSetKind                     = "ReplicaSet"
	// Bundled infrastructure components (dev/CI) are never application writers.
	componentCache    = "cache"
	componentDatabase = "database"
)

func applicationSelector(cfg *cost.CostManagementServiceConfig) client.ListOption {
	labels := resources.Labels(cfg, "")
	delete(labels, "app.kubernetes.io/component")
	return client.MatchingLabels(labels)
}
func ownedByConfig(obj client.Object, cfg *cost.CostManagementServiceConfig) bool {
	owner := metav1.GetControllerOf(obj)
	return owner != nil && owner.UID == cfg.UID && owner.Kind == "CostManagementServiceConfig"
}

// quiesceApplications suspends scheduling and drains managed application Pods.
// Database/cache infrastructure is never stopped. No Pod is force-deleted.
func (r *CostManagementServiceConfigReconciler) quiesceApplications(ctx context.Context, cfg *cost.CostManagementServiceConfig) (bool, error) {
	r.setCondition(cfg, cost.ConditionAvailable, metav1.ConditionFalse, "UpgradeMaintenance", "waiting for application writers to stop")
	r.setCondition(cfg, cost.ConditionProgressing, metav1.ConditionTrue, "UpgradeMaintenance", "draining applications before schema mutation")
	cfg.Status.Phase = cost.PhaseProgressing
	deployments := &appsv1.DeploymentList{}
	if err := r.safetyReader().List(ctx, deployments, client.InNamespace(cfg.Namespace), applicationSelector(cfg)); err != nil {
		return false, err
	}
	quiet := true
	for i := range deployments.Items {
		d := &deployments.Items[i]
		if !ownedByConfig(d, cfg) || d.Labels["app.kubernetes.io/component"] == componentCache {
			continue
		}
		if d.Spec.Replicas == nil || *d.Spec.Replicas != 0 {
			before := d.DeepCopy()
			d.Spec.Replicas = new(int32(0))
			if err := r.Patch(ctx, d, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})); err != nil {
				return false, err
			}
			quiet = false
		}
		if d.Status.ObservedGeneration < d.Generation || d.Status.Replicas != 0 {
			quiet = false
		}
	}
	schedulerQuiet, err := r.suspendApplicationSchedules(ctx, cfg)
	if err != nil {
		return false, err
	}
	podsQuiet, err := r.applicationPodsStopped(ctx, cfg)
	if err != nil {
		return false, err
	}
	return quiet && schedulerQuiet && podsQuiet, nil
}
func (r *CostManagementServiceConfigReconciler) suspendApplicationSchedules(ctx context.Context, cfg *cost.CostManagementServiceConfig) (bool, error) {
	quiet := true
	cronjobs := &batchv1.CronJobList{}
	if err := r.safetyReader().List(ctx, cronjobs, client.InNamespace(cfg.Namespace), applicationSelector(cfg)); err != nil {
		return false, err
	}
	for i := range cronjobs.Items {
		j := &cronjobs.Items[i]
		if !ownedByConfig(j, cfg) {
			continue
		}
		if j.Spec.Suspend == nil || !*j.Spec.Suspend {
			before := j.DeepCopy()
			j.Spec.Suspend = new(true)
			if err := r.Patch(ctx, j, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})); err != nil {
				return false, err
			}
			quiet = false
		}
	}
	jobs := &batchv1.JobList{}
	if err := r.safetyReader().List(ctx, jobs, client.InNamespace(cfg.Namespace), applicationSelector(cfg)); err != nil {
		return false, err
	}
	for i := range jobs.Items {
		j := &jobs.Items[i]
		owner := metav1.GetControllerOf(j)
		if owner != nil && owner.Kind == "CronJob" && !isJobComplete(j) && !isJobFailed(j) {
			quiet = false
		}
	}
	return quiet, nil
}
func (r *CostManagementServiceConfigReconciler) applicationPodsStopped(ctx context.Context, cfg *cost.CostManagementServiceConfig) (bool, error) {
	quiet := true
	pods := &corev1.PodList{}
	if err := r.safetyReader().List(ctx, pods, client.InNamespace(cfg.Namespace), applicationSelector(cfg)); err != nil {
		return false, err
	}
	for i := range pods.Items {
		p := &pods.Items[i]
		if component := p.Labels["app.kubernetes.io/component"]; podTerminal(p) || component == componentCache || component == componentDatabase {
			continue
		}
		owner := metav1.GetControllerOf(p)
		if owner != nil && owner.Kind == migrationJobKind && knownMigration(cfg, owner.Name) {
			continue
		}
		quiet = false
	}
	return quiet, nil
}
func knownMigration(cfg *cost.CostManagementServiceConfig, name string) bool {
	return slices.Contains(migrationNames(cfg), name)
}

func podTerminal(p *corev1.Pod) bool {
	return p.Status.Phase == corev1.PodSucceeded || p.Status.Phase == corev1.PodFailed
}

// Failed Jobs are retained evidence, not active writers. This separate check
// lets rollback restore images while still refusing overlap with any live writer.
func (r *CostManagementServiceConfigReconciler) migrationWritersStopped(ctx context.Context, cfg *cost.CostManagementServiceConfig) (bool, error) {
	for _, name := range migrationNames(cfg) {
		job := &batchv1.Job{}
		err := r.safetyReader().Get(ctx, client.ObjectKey{Namespace: cfg.Namespace, Name: name}, job)
		if apierrors.IsNotFound(err) {
			continue
		}
		if err != nil {
			return false, err
		}
		if !isJobComplete(job) && !isJobFailed(job) {
			return false, nil
		}
	}
	pods := &corev1.PodList{}
	if err := r.safetyReader().List(ctx, pods, client.InNamespace(cfg.Namespace), applicationSelector(cfg)); err != nil {
		return false, err
	}
	for i := range pods.Items {
		p := &pods.Items[i]
		owner := metav1.GetControllerOf(p)
		if !podTerminal(p) && owner != nil && owner.Kind == migrationJobKind && knownMigration(cfg, owner.Name) {
			return false, nil
		}
	}
	return true, nil
}

// applyBeat provides stop-before-start semantics beyond maxSurge=0: a
// terminating old Beat Pod must disappear before its replacement is applied.
func (r *CostManagementServiceConfigReconciler) applyBeat(ctx context.Context, desired *appsv1.Deployment) error {
	const key = "koku.costmanagement.io/beat-template"
	data, _ := json.Marshal(desired.Spec.Template)
	sum := sha256.Sum256(data)
	hash := hex.EncodeToString(sum[:])
	if desired.Annotations == nil {
		desired.Annotations = map[string]string{}
	}
	desired.Annotations[key] = hash
	existing := &appsv1.Deployment{}
	err := r.safetyReader().Get(ctx, client.ObjectKeyFromObject(desired), existing)
	if err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	if err == nil && existing.Annotations[key] != hash && (desired.Spec.Replicas == nil || *desired.Spec.Replicas > 0) {
		if existing.Spec.Replicas == nil || *existing.Spec.Replicas != 0 {
			before := existing.DeepCopy()
			existing.Spec.Replicas = new(int32(0))
			return r.Patch(ctx, existing, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{}))
		}
		pods := &corev1.PodList{}
		if err := r.safetyReader().List(ctx, pods, client.InNamespace(desired.Namespace), client.MatchingLabels(existing.Spec.Selector.MatchLabels)); err != nil {
			return err
		}
		for i := range pods.Items {
			if !podTerminal(&pods.Items[i]) {
				return nil
			}
		}
	}
	return r.Patch(ctx, desired, client.Apply, client.ForceOwnership, client.FieldOwner(fieldOwner))
}

// waitForApplicationRollout checks every enabled target, not only the API. It
// includes workers, Beat, UI, optional ROS and deliberately disabled queues.
func (r *CostManagementServiceConfigReconciler) waitForApplicationRollout(ctx context.Context, cfg *cost.CostManagementServiceConfig) (Result, error) {
	for _, desired := range resources.ApplicationDeployments(cfg) {
		live := &appsv1.Deployment{}
		if err := r.safetyReader().Get(ctx, client.ObjectKeyFromObject(desired), live); err != nil {
			if apierrors.IsNotFound(err) {
				return r.notReadyWait(cfg, reasonWaitingForApplicationRollout, "waiting for "+desired.Name, desired.Name), nil
			}
			return Result{}, err
		}
		ready := deploymentMatchesTarget(live, desired)
		if !ready {
			return r.notReadyWait(cfg, reasonWaitingForApplicationRollout, "waiting for target rollout of "+desired.Name, desired.Name), nil
		}
		stopped, err := r.oldApplicationPodsStopped(ctx, cfg, live, desired)
		if err != nil {
			return Result{}, err
		}
		if !stopped {
			return r.notReadyWait(cfg, reasonWaitingForApplicationRollout, "waiting for old Pods of "+desired.Name+" to terminate", desired.Name), nil
		}
	}
	return Result{}, nil
}

func deploymentMatchesTarget(live, desired *appsv1.Deployment) bool {
	want := *desired.Spec.Replicas
	ready := live.Spec.Replicas != nil && *live.Spec.Replicas == want &&
		live.Status.ObservedGeneration >= live.Generation && live.Status.UpdatedReplicas == want &&
		live.Status.ReadyReplicas == want && live.Status.AvailableReplicas == want
	for _, c := range desired.Spec.Template.Spec.Containers {
		found := false
		for _, actual := range live.Spec.Template.Spec.Containers {
			if actual.Name == c.Name && actual.Image == c.Image {
				found = true
			}
		}
		ready = ready && found
	}

	return ready
}

func (r *CostManagementServiceConfigReconciler) oldApplicationPodsStopped(ctx context.Context, cfg *cost.CostManagementServiceConfig, live, desired *appsv1.Deployment) (bool, error) {
	want := *desired.Spec.Replicas
	pods := &corev1.PodList{}
	if err := r.safetyReader().List(ctx, pods, client.InNamespace(cfg.Namespace), client.MatchingLabels(desired.Spec.Selector.MatchLabels)); err != nil {
		return false, err
	}
	for i := range pods.Items {
		p := &pods.Items[i]
		if podTerminal(p) {
			continue
		}
		if want == 0 {
			return false, nil
		}
		owner := metav1.GetControllerOf(p)
		if owner == nil || owner.Kind != replicaSetKind {
			continue
		}
		rs := &appsv1.ReplicaSet{}
		if err := r.safetyReader().Get(ctx, client.ObjectKey{Namespace: cfg.Namespace, Name: owner.Name}, rs); err != nil {
			if apierrors.IsNotFound(err) {
				// An old ReplicaSet pruned mid-rollout leaves its Pod for garbage
				// collection; keep waiting instead of failing the upgrade.
				return false, nil
			}
			return false, fmt.Errorf("inspect rollout ReplicaSet: %w", err)
		}
		if rs.Annotations["deployment.kubernetes.io/revision"] != live.Annotations["deployment.kubernetes.io/revision"] {
			return false, nil
		}
	}
	return true, nil
}
