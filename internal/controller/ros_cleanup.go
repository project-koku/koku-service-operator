package controller

import (
	"context"
	"fmt"

	batchv1 "k8s.io/api/batch/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	costv1alpha1 "github.com/project-koku/koku-service-operator/api/v1alpha1"
	"github.com/project-koku/koku-service-operator/internal/resources"
)

// rosCleanupObjects returns every ROS/Kruize object the operator creates.
// Used to tear the stack down when spec.ros.enabled flips to false.
func rosCleanupObjects(cfg *costv1alpha1.CostManagementServiceConfig) []client.Object {
	objs := []client.Object{
		// Cluster-scoped (no ownerRef — must delete explicitly).
		resources.KruizeClusterRoleBinding(cfg),
		resources.KruizeClusterRole(cfg),

		// Kruize namespaced
		resources.KruizeDeployment(cfg),
		resources.KruizeService(cfg),
		resources.KruizeServiceAccount(cfg),
		resources.KruizeConfigMap(cfg),
		resources.KruizeDeletePartitionsCronJob(cfg),
		resources.KruizeNetworkPolicy(cfg),
		resources.KruizeServiceMonitor(cfg),

		// ROS namespaced
		resources.ROSAPIDeployment(cfg),
		resources.ROSAPIService(cfg),
		resources.ROSProcessorDeployment(cfg),
		resources.ROSPollerDeployment(cfg),
		resources.ROSHousekeeperDeployment(cfg),
		resources.ROSPartitionCleanerCronJob(cfg),
		resources.ROSAPINetworkPolicy(cfg),
		resources.CdappConfigMap(cfg),

		// Completed ROS migration Job (if any).
		&batchv1.Job{
			ObjectMeta: metav1.ObjectMeta{
				Name:      resources.NameROSMigration(cfg),
				Namespace: cfg.Namespace,
			},
		},
	}
	// Only delete the ROS ServiceAccount when the operator created it.
	if costv1alpha1.BoolVal(cfg.Spec.ROS.ServiceAccount.Create, true) {
		objs = append(objs, resources.ROSServiceAccount(cfg))
	}
	return objs
}

// reconcileROSFeature is reconciler policy bookkeeping, not a provisioning
// stage. It sets the ROSEnabled condition and, when ROS is disabled, deletes
// leftover ROS/Kruize resources from a prior enabled state.
func (r *CostManagementServiceConfigReconciler) reconcileROSFeature(ctx context.Context, cfg *costv1alpha1.CostManagementServiceConfig) error {
	if costv1alpha1.ROSEnabled(cfg) {
		r.setCondition(cfg, costv1alpha1.ConditionROSEnabled, metav1.ConditionTrue, "Enabled",
			"ROS and Kruize are enabled")
		return nil
	}

	r.setCondition(cfg, costv1alpha1.ConditionROSEnabled, metav1.ConditionFalse, "Disabled",
		"ROS and Kruize skipped per spec.ros.enabled=false")
	return r.reconcileROSCleanup(ctx, cfg)
}

// reconcileROSCleanup deletes all ROS/Kruize managed objects. Missing objects
// and absent CRDs (e.g. ServiceMonitor without Prometheus Operator) are ignored
// so the path is safe when ROS was never enabled or monitoring CRDs are absent.
func (r *CostManagementServiceConfigReconciler) reconcileROSCleanup(ctx context.Context, cfg *costv1alpha1.CostManagementServiceConfig) error {
	logger := log.FromContext(ctx)
	for _, obj := range rosCleanupObjects(cfg) {
		if err := r.Delete(ctx, obj, client.PropagationPolicy(metav1.DeletePropagationBackground)); err != nil && !isIgnorableROSCleanupErr(err) {
			return fmt.Errorf("deleting ROS/Kruize resource %s: %w", obj.GetName(), err)
		}
	}
	logger.Info("cleaned up ROS/Kruize resources (ros.enabled=false)")
	return nil
}

// isIgnorableROSCleanupErr reports whether a delete failure should be treated as
// success: NotFound (never created) or NoKindMatch (CRD absent on the cluster).
func isIgnorableROSCleanupErr(err error) bool {
	return errors.IsNotFound(err) || apimeta.IsNoMatchError(err)
}
