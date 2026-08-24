package resources

import (
	"embed"
	"fmt"
	"io/fs"
	"path"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	costv1alpha1 "github.com/project-koku/koku-service-operator/api/v1alpha1"
)

const (
	rbacRolePermissionsMountPath = "/opt/rbac/rbac/management/role/permissions"
	rbacRoleDefinitionsMountPath = "/opt/rbac/rbac/management/role/definitions"
	rbacRolePermissionsVolume    = "rbac-role-permissions"
	rbacRoleDefinitionsVolume    = "rbac-role-definitions"
	rbacCostManagementSeedFile   = "cost-management.json"
	rbacSourcesSeedFile          = "sources.json"
)

//go:embed rbac-config/permissions/*
var rbacPermissionConfig embed.FS

//go:embed rbac-config/definitions/*
var rbacDefinitionConfig embed.FS

func embeddedConfigMapData(fsys embed.FS, root string) (map[string]string, error) {
	data := make(map[string]string)
	err := fs.WalkDir(fsys, root, func(p string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			return nil
		}
		b, readErr := fsys.ReadFile(p)
		if readErr != nil {
			return readErr
		}
		data[path.Base(p)] = string(b)
		return nil
	})
	if err != nil {
		return nil, err
	}
	if len(data) == 0 {
		return nil, fmt.Errorf("no JSON files under %s", root)
	}
	return data, nil
}

func mustEmbeddedConfigMapData(fsys embed.FS, root string) map[string]string {
	data, err := embeddedConfigMapData(fsys, root)
	if err != nil {
		panic(fmt.Sprintf("rbac seed embed %s: %v", root, err))
	}
	return data
}

func init() {
	mustEmbeddedConfigMapData(rbacPermissionConfig, "rbac-config/permissions")
	mustEmbeddedConfigMapData(rbacDefinitionConfig, "rbac-config/definitions")
}

// RBACRolePermissionsConfigMap holds on-prem cost-management and sources permissions
// for manage.py seeds (same mount path as SaaS rbac-clowdapp).
func RBACRolePermissionsConfigMap(cfg *costv1alpha1.CostManagementServiceConfig) *corev1.ConfigMap {
	return &corev1.ConfigMap{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "ConfigMap"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      NameRBACRolePermissionsConfigMap(cfg),
			Namespace: cfg.Namespace,
			Labels:    Labels(cfg, "rbac-seed"),
		},
		Data: mustEmbeddedConfigMapData(rbacPermissionConfig, "rbac-config/permissions"),
	}
}

// RBACRoleDefinitionsConfigMap holds on-prem cost-management and sources role definitions
// for manage.py seeds.
func RBACRoleDefinitionsConfigMap(cfg *costv1alpha1.CostManagementServiceConfig) *corev1.ConfigMap {
	return &corev1.ConfigMap{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "ConfigMap"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      NameRBACRoleDefinitionsConfigMap(cfg),
			Namespace: cfg.Namespace,
			Labels:    Labels(cfg, "rbac-seed"),
		},
		Data: mustEmbeddedConfigMapData(rbacDefinitionConfig, "rbac-config/definitions"),
	}
}

// appendRBACSeedConfigVolumes mounts operator-owned seed JSON files via subPath so
// built-in image catalog files in the same directories remain visible to seeds.
func appendRBACSeedConfigVolumes(
	cfg *costv1alpha1.CostManagementServiceConfig,
	vols []corev1.Volume,
	mounts []corev1.VolumeMount,
) ([]corev1.Volume, []corev1.VolumeMount) {
	permissionsCM := NameRBACRolePermissionsConfigMap(cfg)
	definitionsCM := NameRBACRoleDefinitionsConfigMap(cfg)
	vols = append(vols,
		corev1.Volume{
			Name: rbacRolePermissionsVolume,
			VolumeSource: corev1.VolumeSource{
				ConfigMap: &corev1.ConfigMapVolumeSource{
					LocalObjectReference: corev1.LocalObjectReference{Name: permissionsCM},
				},
			},
		},
		corev1.Volume{
			Name: rbacRoleDefinitionsVolume,
			VolumeSource: corev1.VolumeSource{
				ConfigMap: &corev1.ConfigMapVolumeSource{
					LocalObjectReference: corev1.LocalObjectReference{Name: definitionsCM},
				},
			},
		},
	)
	for _, seed := range []struct {
		volume    string
		mountBase string
		file      string
	}{
		{rbacRolePermissionsVolume, rbacRolePermissionsMountPath, rbacCostManagementSeedFile},
		{rbacRolePermissionsVolume, rbacRolePermissionsMountPath, rbacSourcesSeedFile},
		{rbacRoleDefinitionsVolume, rbacRoleDefinitionsMountPath, rbacCostManagementSeedFile},
		{rbacRoleDefinitionsVolume, rbacRoleDefinitionsMountPath, rbacSourcesSeedFile},
	} {
		mounts = append(mounts, corev1.VolumeMount{
			Name:      seed.volume,
			MountPath: seed.mountBase + "/" + seed.file,
			SubPath:   seed.file,
			ReadOnly:  true,
		})
	}
	return vols, mounts
}
