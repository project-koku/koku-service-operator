package resources

import (
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	costv1alpha1 "github.com/project-koku/koku-service-operator/api/v1alpha1"
)

// ApplicationRollout sets explicit rollout and shutdown budgets. The controller
// additionally waits for terminating Beat Pods before starting its replacement.
func ApplicationRollout(d *appsv1.Deployment) *appsv1.Deployment {
	component := d.Labels[labelComponent]
	worker := strings.HasPrefix(component, "cost-worker-") || component == "cost-scheduler" ||
		component == "listener" || component == "rbac-worker" || component == "ros-processor" ||
		component == "ros-recommendation-poller" || component == "ros-housekeeper"
	surge, unavailable := intstr.FromString("25%"), intstr.FromString("25%")
	grace := int64(60)
	progressDeadline := int32(900)
	if worker {
		surge, unavailable, grace = intstr.FromInt32(0), intstr.FromInt32(1), 3600
		progressDeadline = 4500
	}
	d.Spec.Strategy = appsv1.DeploymentStrategy{Type: appsv1.RollingUpdateDeploymentStrategyType,
		RollingUpdate: &appsv1.RollingUpdateDeployment{MaxSurge: &surge, MaxUnavailable: &unavailable}}
	d.Spec.ProgressDeadlineSeconds = &progressDeadline
	d.Spec.MinReadySeconds = 10
	d.Spec.Template.Spec.TerminationGracePeriodSeconds = &grace
	return d
}

// ApplicationDeployments is the complete enabled set used for completion and
// recovery. A zero-replica worker remains in the set as an intentionally off target.
func ApplicationDeployments(cfg *costv1alpha1.CostManagementServiceConfig) []*appsv1.Deployment {
	deployments := []*appsv1.Deployment{KokuAPIDeployment(cfg), MasuDeployment(cfg), ListenerDeployment(cfg),
		RBACAPIDeployment(cfg), RBACWorkerDeployment(cfg), CeleryBeatDeployment(cfg),
		IngressDeployment(cfg), EnvoyDeployment(cfg), UIDeployment(cfg)}
	deployments = append(deployments, CeleryWorkerDeployments(cfg)...)
	if costv1alpha1.ROSEnabled(cfg) {
		deployments = append(deployments, KruizeDeployment(cfg), ROSAPIDeployment(cfg), ROSProcessorDeployment(cfg), ROSPollerDeployment(cfg), ROSHousekeeperDeployment(cfg))
	}
	return deployments
}

func BeatReadiness() *corev1.Probe {
	return &corev1.Probe{ProbeHandler: corev1.ProbeHandler{Exec: &corev1.ExecAction{
		Command: []string{"/bin/sh", "-c", "read -r pid < /tmp/celerybeat.pid && case \"$pid\" in ''|*[!0-9]*) exit 1;; esac && kill -0 \"$pid\""},
	}}, InitialDelaySeconds: 5, PeriodSeconds: 10, TimeoutSeconds: 3, FailureThreshold: 3}
}
