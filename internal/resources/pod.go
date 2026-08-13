package resources

import (
	corev1 "k8s.io/api/core/v1"

	costv1alpha1 "github.com/project-koku/koku-service-operator/api/v1alpha1"
)

// Shared PodSpec helpers used by workload builders across this package.

func nonRootPodSC() *corev1.PodSecurityContext {
	nonRoot := true
	return &corev1.PodSecurityContext{
		RunAsNonRoot: &nonRoot,
		SeccompProfile: &corev1.SeccompProfile{
			Type: corev1.SeccompProfileTypeRuntimeDefault,
		},
	}
}

func pullPolicy(cfg *costv1alpha1.CostManagementServiceConfig) corev1.PullPolicy {
	if cfg.Spec.Global.PullPolicy != "" {
		return cfg.Spec.Global.PullPolicy
	}
	return corev1.PullIfNotPresent
}

// imagePullSecrets returns global.imagePullSecrets for Pod specs so private
// registry credentials (e.g. registry.redhat.io pull secrets) are applied to
// every workload the operator creates.
func imagePullSecrets(cfg *costv1alpha1.CostManagementServiceConfig) []corev1.LocalObjectReference {
	return cfg.Spec.Global.ImagePullSecrets
}
