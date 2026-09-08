package resources

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	costv1alpha1 "github.com/project-koku/koku-service-operator/api/v1alpha1"
)

func familyServiceAccount(cfg *costv1alpha1.CostManagementServiceConfig, name, component string) *corev1.ServiceAccount {
	return &corev1.ServiceAccount{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "ServiceAccount"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: cfg.Namespace,
			Labels:    Labels(cfg, component),
		},
		AutomountServiceAccountToken: new(false),
	}
}

// GatewayServiceAccount is used by the Envoy JWT gateway.
func GatewayServiceAccount(cfg *costv1alpha1.CostManagementServiceConfig) *corev1.ServiceAccount {
	return familyServiceAccount(cfg, NameGatewayServiceAccount(cfg), "gateway")
}

// IngressServiceAccount is used by the insights-ingress-go upload handler.
func IngressServiceAccount(cfg *costv1alpha1.CostManagementServiceConfig) *corev1.ServiceAccount {
	return familyServiceAccount(cfg, NameIngressServiceAccount(cfg), "ingress")
}

// RBACServiceAccount is shared by RBAC API, worker, and RBAC Jobs/CronJobs.
func RBACServiceAccount(cfg *costv1alpha1.CostManagementServiceConfig) *corev1.ServiceAccount {
	return familyServiceAccount(cfg, NameRBACServiceAccount(cfg), "rbac")
}

// UIServiceAccount is used by the UI oauth2-proxy + nginx Deployment.
func UIServiceAccount(cfg *costv1alpha1.CostManagementServiceConfig) *corev1.ServiceAccount {
	return familyServiceAccount(cfg, NameUIServiceAccount(cfg), "ui")
}
