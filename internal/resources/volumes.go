package resources

import (
	corev1 "k8s.io/api/core/v1"

	costv1alpha1 "github.com/project-koku/koku-service-operator/api/v1alpha1"
)

// KokuVolumes returns the standard volume list shared by all Koku pods.
// Mirrors cost-onprem.koku.volumes in _helpers-koku.tpl.
func KokuVolumes(cfg *costv1alpha1.CostManagementServiceConfig) []corev1.Volume {
	vols := []corev1.Volume{
		{
			Name:         "tmp",
			VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
		},
		{
			Name: "aws-config",
			VolumeSource: corev1.VolumeSource{
				ConfigMap: &corev1.ConfigMapVolumeSource{
					LocalObjectReference: corev1.LocalObjectReference{Name: NameAWSConfigMap(cfg)},
				},
			},
		},
		{
			Name: "ca-scripts",
			VolumeSource: corev1.VolumeSource{
				ConfigMap: &corev1.ConfigMapVolumeSource{
					LocalObjectReference: corev1.LocalObjectReference{Name: NameCACombineConfigMap(cfg)},
					Items: []corev1.KeyToPath{
						{Key: "combine-ca.sh", Path: "combine-ca.sh", Mode: int32Ptr(0755)},
					},
				},
			},
		},
		{
			Name: "ca-source",
			VolumeSource: corev1.VolumeSource{
				ConfigMap: &corev1.ConfigMapVolumeSource{
					LocalObjectReference: corev1.LocalObjectReference{Name: NameServiceCAConfigMap(cfg)},
				},
			},
		},
		{
			Name:         "combined-ca-bundle",
			VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
		},
	}

	if cfg.Spec.Cache.TLS.Enabled && cfg.Spec.Cache.TLS.CACertSecretName != "" {
		vols = append(vols, corev1.Volume{
			Name: "redis-tls-ca",
			VolumeSource: corev1.VolumeSource{
				Secret: &corev1.SecretVolumeSource{
					SecretName: cfg.Spec.Cache.TLS.CACertSecretName,
					Items:      []corev1.KeyToPath{{Key: "ca.crt", Path: "ca.crt"}},
				},
			},
		})
	}

	// Kafka TLS CA (BYOI secured Kafka)
	if cfg.Spec.Kafka.TLS.Enabled && cfg.Spec.Kafka.TLS.CACertSecret != "" {
		vols = append(vols, corev1.Volume{
			Name: "kafka-ca-cert",
			VolumeSource: corev1.VolumeSource{
				Secret: &corev1.SecretVolumeSource{
					SecretName: cfg.Spec.Kafka.TLS.CACertSecret,
					Items:      []corev1.KeyToPath{{Key: "ca.crt", Path: "ca.crt"}},
				},
			},
		})
	}

	return vols
}

// KokuVolumeMounts returns the standard volume mounts for all Koku containers.
// Mirrors cost-onprem.koku.volumeMounts in _helpers-koku.tpl.
func KokuVolumeMounts(cfg *costv1alpha1.CostManagementServiceConfig) []corev1.VolumeMount {
	mounts := []corev1.VolumeMount{
		{Name: "tmp", MountPath: "/tmp"},
		{Name: "aws-config", MountPath: "/etc/aws", ReadOnly: true},
		{Name: "combined-ca-bundle", MountPath: "/etc/pki/ca-trust/combined", ReadOnly: true},
	}

	if cfg.Spec.Cache.TLS.Enabled && cfg.Spec.Cache.TLS.CACertSecretName != "" {
		mounts = append(mounts, corev1.VolumeMount{
			Name:      "redis-tls-ca",
			MountPath: "/etc/redis-tls",
			ReadOnly:  true,
		})
	}

	// Kafka TLS CA (BYOI secured Kafka)
	if cfg.Spec.Kafka.TLS.Enabled && cfg.Spec.Kafka.TLS.CACertSecret != "" {
		mounts = append(mounts, corev1.VolumeMount{
			Name:      "kafka-ca-cert",
			MountPath: "/etc/kafka/certs",
			ReadOnly:  true,
		})
	}

	return mounts
}

// ubiMinimalInitSC is the security context for ubi-minimal-based init containers.
// RunAsUser is intentionally absent: restricted-v2 SCC injects the namespace UID
// before validation, satisfying RunAsNonRoot even when the image default is root.
// ubi-minimal follows the Red Hat arbitrary-UID convention (GID 0, ug+rw on all
// app directories) so it functions correctly under any injected UID.
func ubiMinimalInitSC() *corev1.SecurityContext {
	f := false
	t := true
	return &corev1.SecurityContext{
		AllowPrivilegeEscalation: &f,
		Privileged:               &f,
		ReadOnlyRootFilesystem:   &t,
		RunAsNonRoot:             &t,
		Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
	}
}

// CACombineInitContainer returns the init container that merges system and
// cluster CA certificates into a combined bundle.
func CACombineInitContainer(_ *costv1alpha1.CostManagementServiceConfig) corev1.Container {
	return corev1.Container{
		Name:    "prepare-ca-bundle",
		Image:   UBIMinimalImage,
		Command: []string{"bash", "/scripts/combine-ca.sh"},
		VolumeMounts: []corev1.VolumeMount{
			{Name: "ca-scripts", MountPath: "/scripts", ReadOnly: true},
			{Name: "ca-source", MountPath: "/ca-source", ReadOnly: true},
			{Name: "combined-ca-bundle", MountPath: "/ca-output"},
		},
		SecurityContext: ubiMinimalInitSC(),
	}
}

// WaitForValkeyInitContainer returns an init container that blocks until
// the Valkey service accepts connections.
func WaitForValkeyInitContainer(cfg *costv1alpha1.CostManagementServiceConfig) corev1.Container {
	host := CacheHost(cfg)
	port := "6379"
	if cfg.Spec.Cache.Port != 0 {
		port = int32String(cfg.Spec.Cache.Port)
	}
	return waitForTCP("wait-for-valkey", host, port)
}

// kokuAppContainerSC is used for koku application containers (API, Masu,
// Listener, Celery). RunAsUser is absent — restricted-v2 SCC injects the
// namespace UID. The koku image uses adduser -g 0 with ug+rw permissions so
// it works under any injected UID (confirmed by the SaaS Clowder deployment).
//
// ReadOnlyRootFilesystem is absent: Django's settings.py unconditionally
// instantiates a file log handler at /opt/koku/koku/app.log at startup
// regardless of DJANGO_LOG_HANDLERS. Fix in the koku image by making log
// handler instantiation respect the env var.
func kokuAppContainerSC() *corev1.SecurityContext {
	f := false
	t := true
	return &corev1.SecurityContext{
		AllowPrivilegeEscalation: &f,
		Privileged:               &f,
		RunAsNonRoot:             &t,
		Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
	}
}

// migrationContainerSC is like kokuAppContainerSC: no readOnlyRootFilesystem
// (Django log handlers) and an explicit numeric UID for the koku image USER.
func migrationContainerSC() *corev1.SecurityContext {
	return kokuAppContainerSC()
}

func int32Ptr(i int32) *int32 { return &i }

func restrictedContainerSC() *corev1.SecurityContext {
	f := false
	t := true
	return &corev1.SecurityContext{
		AllowPrivilegeEscalation: &f,
		Privileged:               &f,
		ReadOnlyRootFilesystem:   &t,
		RunAsNonRoot:             &t,
		Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
	}
}

// uiOAuthProxyContainerSC is for the oauth2-proxy sidecar.
// RunAsUser absent — restricted-v2 SCC injects the namespace UID.
func uiOAuthProxyContainerSC() *corev1.SecurityContext {
	f := false
	t := true
	return &corev1.SecurityContext{
		AllowPrivilegeEscalation: &f,
		Privileged:               &f,
		ReadOnlyRootFilesystem:   &t,
		RunAsNonRoot:             &t,
		Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
	}
}

// uiAppContainerSC is for the koku-ui-onprem nginx container.
// Writable nginx paths provided via emptyDir mounts. RunAsUser absent.
func uiAppContainerSC() *corev1.SecurityContext {
	f := false
	t := true
	return &corev1.SecurityContext{
		AllowPrivilegeEscalation: &f,
		Privileged:               &f,
		ReadOnlyRootFilesystem:   &t,
		RunAsNonRoot:             &t,
		Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
	}
}
