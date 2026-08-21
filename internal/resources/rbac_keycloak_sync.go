package resources

import (
	_ "embed"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	costv1alpha1 "github.com/project-koku/koku-service-operator/api/v1alpha1"
)

//go:embed scripts/sync_keycloak_principals.py
var syncKeycloakScript string

// KeycloakSyncConfigMap embeds the sync_keycloak_principals.py script
// that the CronJob mounts and executes under `manage.py shell`.
func KeycloakSyncConfigMap(cfg *costv1alpha1.CostManagementServiceConfig) *corev1.ConfigMap {
	return &corev1.ConfigMap{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "ConfigMap"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      NameRBACKeycloakSyncConfigMap(cfg),
			Namespace: cfg.Namespace,
			Labels:    Labels(cfg, "rbac-keycloak-sync"),
		},
		Data: map[string]string{
			"sync_keycloak_principals.py": syncKeycloakScript,
		},
	}
}

// KeycloakSyncCronJob builds the CronJob that periodically syncs Keycloak
// group membership into RBAC Tenant/Principal/Group records.
func KeycloakSyncCronJob(cfg *costv1alpha1.CostManagementServiceConfig) *batchv1.CronJob {
	spec := cfg.Spec.RBAC.KeycloakSync
	image := cfg.Spec.RBAC.Image.Repository + ":" + cfg.Spec.RBAC.Image.Tag

	schedule := spec.Schedule
	if schedule == "" {
		schedule = "*/15 * * * *"
	}
	orgGroupPrefix := spec.OrgGroupPrefix
	if orgGroupPrefix == "" {
		orgGroupPrefix = "org-"
	}
	orgAdminSubgroup := spec.OrgAdminSubgroup
	if orgAdminSubgroup == "" {
		orgAdminSubgroup = "org-admin"
	}

	falseVal := false

	env := rbacEnv(cfg)
	env = append(env,
		EnvVal("KEYCLOAK_URL", KeycloakURL(cfg)),
		EnvVal("KEYCLOAK_REALM", KeycloakRealm(cfg)),
		EnvVal("KEYCLOAK_CLIENT_ID", spec.ClientID),
		EnvFromSecret("KEYCLOAK_CLIENT_SECRET", spec.ClientSecretRef.Name, clientSecretKey(spec.ClientSecretRef.Key)),
		EnvVal("KEYCLOAK_TLS_VERIFY", keycloakTLSVerifyStr(cfg)),
		EnvVal("SYNC_ORG_GROUP_PREFIX", orgGroupPrefix),
		EnvVal("SYNC_ORG_ADMIN_SUBGROUP", orgAdminSubgroup),
		EnvVal("SYNC_PRUNE_ORPHANS", boolEnvStr(costv1alpha1.BoolVal(spec.PruneOrphans, true))),
	)

	vols, mounts := rbacVolumesAndMounts(cfg)
	vols = append(vols, corev1.Volume{
		Name: "sync-script",
		VolumeSource: corev1.VolumeSource{
			ConfigMap: &corev1.ConfigMapVolumeSource{
				LocalObjectReference: corev1.LocalObjectReference{Name: NameRBACKeycloakSyncConfigMap(cfg)},
			},
		},
	})
	mounts = append(mounts, corev1.VolumeMount{
		Name:      "sync-script",
		MountPath: "/opt/rbac/rbac/sync_keycloak_principals.py",
		SubPath:   "sync_keycloak_principals.py",
		ReadOnly:  true,
	})

	return &batchv1.CronJob{
		TypeMeta: metav1.TypeMeta{APIVersion: "batch/v1", Kind: "CronJob"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      NameRBACKeycloakSync(cfg),
			Namespace: cfg.Namespace,
			Labels:    Labels(cfg, "rbac-keycloak-sync"),
		},
		Spec: batchv1.CronJobSpec{
			Schedule:                   schedule,
			StartingDeadlineSeconds:    &CronJobStartingDeadlineSeconds,
			ConcurrencyPolicy:          CronJobConcurrencyForbid,
			SuccessfulJobsHistoryLimit: &CronJobSuccessHistoryLimit,
			FailedJobsHistoryLimit:     &CronJobFailedHistoryLimit,
			JobTemplate: batchv1.JobTemplateSpec{
				Spec: batchv1.JobSpec{
					ActiveDeadlineSeconds: &CronJobActiveDeadlineSeconds,
					BackoffLimit:          &CronJobBackoffLimit,
					Template: corev1.PodTemplateSpec{
						ObjectMeta: metav1.ObjectMeta{Labels: Labels(cfg, "rbac-keycloak-sync")},
						Spec: corev1.PodSpec{
							ServiceAccountName:           NameRBACServiceAccount(cfg),
							RestartPolicy:                CronJobRestartOnFailure,
							AutomountServiceAccountToken: &falseVal,
							SecurityContext:              nonRootPodSC(),
							ImagePullSecrets:             imagePullSecrets(cfg),
							InitContainers: []corev1.Container{
								waitForRBACDB(cfg),
							},
							Containers: []corev1.Container{{
								Name:            "keycloak-sync",
								Image:           image,
								ImagePullPolicy: pullPolicy(cfg),
								WorkingDir:      "/opt/rbac/rbac",
								Command:         []string{"/bin/bash", "-c"},
								Args:            []string{"python manage.py shell < /opt/rbac/rbac/sync_keycloak_principals.py"},
								Env:             env,
								VolumeMounts:    mounts,
								Resources:       spec.Resources,
								SecurityContext: rbacAppContainerSC(),
							}},
							Volumes: vols,
						},
					},
				},
			},
		},
	}
}

func keycloakTLSVerifyStr(cfg *costv1alpha1.CostManagementServiceConfig) string {
	if cfg.Spec.Auth.Keycloak.TLS.InsecureSkipVerify {
		return "false"
	}
	return "true"
}

func boolEnvStr(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

func clientSecretKey(key string) string {
	if key == "" {
		return "CLIENT_SECRET"
	}
	return key
}
