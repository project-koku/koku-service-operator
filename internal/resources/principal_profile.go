package resources

import (
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	costv1alpha1 "github.com/project-koku/koku-service-operator/api/v1alpha1"
)

const (
	principalProfilePort = int32(8099)
	// Loopback only: RBAC API talks over the pod network namespace; kubelet
	// probes use --healthcheck (HTTP probes hit the pod IP and would fail).
	principalProfileAddr     = "127.0.0.1:8099"
	principalProfileURLLocal = "http://127.0.0.1:8099"
)

// principalProfileEnabled reports whether the Cost principal-profile sidecar
// should run beside the RBAC API (COST-8167).
func principalProfileEnabled(cfg *costv1alpha1.CostManagementServiceConfig) bool {
	return cfg.Spec.RBAC.KeycloakSync.Enabled
}

// rbacEnvWithPrincipalProfile is rbacEnv plus PRINCIPAL_PROFILE_SERVICE_URL when
// the Cost sidecar is enabled. Keycloak secrets stay on the sidecar only.
func rbacEnvWithPrincipalProfile(cfg *costv1alpha1.CostManagementServiceConfig) []corev1.EnvVar {
	env := rbacEnv(cfg)
	if principalProfileEnabled(cfg) {
		env = append(env, EnvVal("PRINCIPAL_PROFILE_SERVICE_URL", principalProfileURLLocal))
	}
	return env
}

// principalProfileContainer builds the Cost-owned Keycloak profile sidecar.
// It reuses the operator image (ships /principal-profile next to /manager).
func principalProfileContainer(cfg *costv1alpha1.CostManagementServiceConfig) corev1.Container {
	spec := cfg.Spec.RBAC.KeycloakSync
	orgPrefix := spec.OrgGroupPrefix
	if orgPrefix == "" {
		orgPrefix = "org-"
	}
	env := []corev1.EnvVar{
		EnvVal("PRINCIPAL_PROFILE_ADDR", principalProfileAddr),
		EnvVal("KEYCLOAK_URL", KeycloakURL(cfg)),
		EnvVal("KEYCLOAK_REALM", KeycloakRealm(cfg)),
		EnvVal("KEYCLOAK_CLIENT_ID", spec.ClientID),
		EnvFromSecret("KEYCLOAK_CLIENT_SECRET", spec.ClientSecretRef.Name, clientSecretKey(spec.ClientSecretRef.Key)),
		EnvVal("KEYCLOAK_TLS_VERIFY", keycloakTLSVerifyStr(cfg)),
		EnvVal("SYNC_ORG_GROUP_PREFIX", orgPrefix),
		EnvVal("PRINCIPAL_PROFILE_CACHE_TTL", "5m"),
	}
	mounts := []corev1.VolumeMount{}
	if keycloakSyncVerifiesTLS(cfg) {
		env = append(env, EnvVal("KEYCLOAK_CA_FILE", "/etc/keycloak-ca/ca.crt"))
		env = append(env, EnvVal("SSL_CERT_FILE", "/etc/keycloak-ca/ca.crt"))
		mounts = append(mounts, corev1.VolumeMount{Name: "keycloak-ca", MountPath: "/etc/keycloak-ca", ReadOnly: true})
	}
	cpu := resource.MustParse("50m")
	mem := resource.MustParse("64Mi")
	healthcheck := []string{"/principal-profile", "--healthcheck"}
	return corev1.Container{
		Name:            "principal-profile",
		Image:           OperatorImage,
		ImagePullPolicy: pullPolicy(cfg),
		Command:         []string{"/principal-profile"},
		// Port is documented for operators; the process binds loopback only.
		Ports: []corev1.ContainerPort{
			{Name: "profile", ContainerPort: principalProfilePort, Protocol: corev1.ProtocolTCP},
		},
		Env:          env,
		VolumeMounts: mounts,
		Resources: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{corev1.ResourceCPU: cpu, corev1.ResourceMemory: mem},
			Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("200m"), corev1.ResourceMemory: resource.MustParse("128Mi")},
		},
		ReadinessProbe: &corev1.Probe{
			ProbeHandler: corev1.ProbeHandler{
				Exec: &corev1.ExecAction{Command: healthcheck},
			},
			InitialDelaySeconds: 3, PeriodSeconds: 10, TimeoutSeconds: 3, FailureThreshold: 3,
		},
		LivenessProbe: &corev1.Probe{
			ProbeHandler: corev1.ProbeHandler{
				Exec: &corev1.ExecAction{Command: healthcheck},
			},
			InitialDelaySeconds: 10, PeriodSeconds: 20, TimeoutSeconds: 3, FailureThreshold: 5,
		},
		SecurityContext: rbacAppContainerSC(),
	}
}
