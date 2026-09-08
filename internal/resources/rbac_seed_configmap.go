package resources

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	costv1alpha1 "github.com/project-koku/koku-service-operator/api/v1alpha1"
	"github.com/project-koku/koku-service-operator/internal/resources/rbac_seed"
)

const (
	rbacSeedPermissionsMountDir = "/opt/rbac/rbac/management/role/permissions"
	rbacSeedDefinitionsMountDir = "/opt/rbac/rbac/management/role/definitions"
)

// RBACSeedPermissionsConfigMap holds cost-management and sources permission
// JSON for insights-rbac manage.py seeds (mounted over permissions/).
func RBACSeedPermissionsConfigMap(cfg *costv1alpha1.CostManagementServiceConfig) (*corev1.ConfigMap, error) {
	data, err := rbac_seed.PermissionsConfigMapData()
	if err != nil {
		return nil, err
	}
	return &corev1.ConfigMap{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "ConfigMap"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      NameRBACSeedPermissionsConfigMap(cfg),
			Namespace: cfg.Namespace,
			Labels:    Labels(cfg, "rbac-seed"),
		},
		Data: data,
	}, nil
}

// RBACSeedDefinitionsConfigMap holds cost-management and sources role JSON for
// insights-rbac manage.py seeds (mounted over definitions/).
func RBACSeedDefinitionsConfigMap(cfg *costv1alpha1.CostManagementServiceConfig) (*corev1.ConfigMap, error) {
	data, err := rbac_seed.DefinitionsConfigMapData()
	if err != nil {
		return nil, err
	}
	return &corev1.ConfigMap{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "ConfigMap"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      NameRBACSeedDefinitionsConfigMap(cfg),
			Namespace: cfg.Namespace,
			Labels:    Labels(cfg, "rbac-seed"),
		},
		Data: data,
	}, nil
}

func rbacSeedVolumes(cfg *costv1alpha1.CostManagementServiceConfig) []corev1.Volume {
	return []corev1.Volume{
		{
			Name: "rbac-seed-permissions",
			VolumeSource: corev1.VolumeSource{
				ConfigMap: &corev1.ConfigMapVolumeSource{
					LocalObjectReference: corev1.LocalObjectReference{
						Name: NameRBACSeedPermissionsConfigMap(cfg),
					},
				},
			},
		},
		{
			Name: "rbac-seed-definitions",
			VolumeSource: corev1.VolumeSource{
				ConfigMap: &corev1.ConfigMapVolumeSource{
					LocalObjectReference: corev1.LocalObjectReference{
						Name: NameRBACSeedDefinitionsConfigMap(cfg),
					},
				},
			},
		},
	}
}

func rbacSeedVolumeMounts() []corev1.VolumeMount {
	return []corev1.VolumeMount{
		{
			Name:      "rbac-seed-permissions",
			MountPath: rbacSeedPermissionsMountDir + "/cost-management.json",
			SubPath:   "cost-management.json",
			ReadOnly:  true,
		},
		{
			Name:      "rbac-seed-permissions",
			MountPath: rbacSeedPermissionsMountDir + "/sources.json",
			SubPath:   "sources.json",
			ReadOnly:  true,
		},
		{
			Name:      "rbac-seed-definitions",
			MountPath: rbacSeedDefinitionsMountDir + "/cost-management.json",
			SubPath:   "cost-management.json",
			ReadOnly:  true,
		},
		{
			Name:      "rbac-seed-definitions",
			MountPath: rbacSeedDefinitionsMountDir + "/sources.json",
			SubPath:   "sources.json",
			ReadOnly:  true,
		},
	}
}
