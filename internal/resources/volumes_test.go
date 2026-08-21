package resources

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
)

func TestKokuAPIInitMountsUserCAExtra(t *testing.T) {
	cfg := testCfg()
	cfg.Spec.ObjectStorage.CACertSecretName = "s3-ca-secret"
	cfg.Spec.Kafka.TLS.Enabled = true
	cfg.Spec.Kafka.TLS.CACertSecret = "kafka-ca-secret"
	dep := KokuAPIDeployment(cfg)
	if !volumeProjectsSecret(dep.Spec.Template.Spec.Volumes, "s3-ca-secret", "object-storage-ca.crt") {
		t.Fatal("KokuAPIDeployment missing object storage CA in ca-extra")
	}
	if !volumeProjectsSecret(dep.Spec.Template.Spec.Volumes, "kafka-ca-secret", "kafka-ca.crt") {
		t.Fatal("KokuAPIDeployment missing kafka CA in ca-extra")
	}
	init := dep.Spec.Template.Spec.InitContainers[0]
	if init.Name != "prepare-ca-bundle" {
		t.Fatalf("init[0] = %q, want prepare-ca-bundle", init.Name)
	}
	found := false
	for _, m := range init.VolumeMounts {
		if m.MountPath == "/ca-extra" {
			found = true
		}
	}
	if !found {
		t.Fatal("koku prepare-ca-bundle must mount /ca-extra")
	}
}

func TestUserCAExtraVolume_Empty(t *testing.T) {
	if UserCAExtraVolume(testCfg()) != nil {
		t.Fatal("expected nil volume when no user CA secrets are set")
	}
	init := CACombineInitContainer(testCfg())
	for _, m := range init.VolumeMounts {
		if m.MountPath == "/ca-extra" {
			t.Fatal("prepare-ca-bundle must not mount /ca-extra when all CA secrets are empty")
		}
	}
}

func TestUserCAExtraVolume_AllSources(t *testing.T) {
	cfg := testCfg()
	cfg.Spec.Auth.Keycloak.TLS.CACertSecretName = "keycloak-ca-secret"
	cfg.Spec.Kafka.TLS.Enabled = true
	cfg.Spec.Kafka.TLS.CACertSecret = "kafka-ca-secret"
	cfg.Spec.Cache.TLS.Enabled = true
	cfg.Spec.Cache.TLS.CACertSecretName = "cache-ca-secret"
	cfg.Spec.ObjectStorage.CACertSecretName = "s3-ca-secret"

	vol := UserCAExtraVolume(cfg)
	if vol == nil {
		t.Fatal("expected projected ca-extra volume")
	}
	if vol.Name != caExtraVolumeName {
		t.Errorf("volume name = %q, want %q", vol.Name, caExtraVolumeName)
	}
	if vol.Projected == nil {
		t.Fatal("expected Projected volume source")
	}

	want := map[string]string{
		"keycloak-ca.crt":       "keycloak-ca-secret",
		"kafka-ca.crt":          "kafka-ca-secret",
		"cache-ca.crt":          "cache-ca-secret",
		"object-storage-ca.crt": "s3-ca-secret",
	}
	got := map[string]string{}
	for _, src := range vol.Projected.Sources {
		if src.Secret == nil {
			t.Fatal("expected Secret projection")
		}
		if len(src.Secret.Items) != 1 || src.Secret.Items[0].Key != "ca.crt" {
			t.Fatalf("expected ca.crt key mapping, got %+v", src.Secret.Items)
		}
		got[src.Secret.Items[0].Path] = src.Secret.Name
	}
	if len(got) != len(want) {
		t.Fatalf("projected files = %v, want %v", got, want)
	}
	for path, secret := range want {
		if got[path] != secret {
			t.Errorf("path %s = %q, want secret %q", path, got[path], secret)
		}
	}

	init := CACombineInitContainer(cfg)
	found := false
	for _, m := range init.VolumeMounts {
		if m.Name == caExtraVolumeName && m.MountPath == "/ca-extra" {
			found = true
			if !m.ReadOnly {
				t.Error("ca-extra mount must be read-only")
			}
		}
	}
	if !found {
		t.Fatal("prepare-ca-bundle missing /ca-extra mount")
	}
}

func TestUserCAExtraVolume_TLSDisabledSkipsKafkaAndCache(t *testing.T) {
	cfg := testCfg()
	cfg.Spec.Kafka.TLS.Enabled = false
	cfg.Spec.Kafka.TLS.CACertSecret = "kafka-ca-secret"
	cfg.Spec.Cache.TLS.Enabled = false
	cfg.Spec.Cache.TLS.CACertSecretName = "cache-ca-secret"
	if UserCAExtraVolume(cfg) != nil {
		t.Fatal("kafka/cache CA must not be projected when TLS is disabled")
	}
}

func TestKokuVolumesIncludesUserCAExtra(t *testing.T) {
	cfg := testCfg()
	cfg.Spec.ObjectStorage.CACertSecretName = "s3-ca-secret"
	cfg.Spec.Kafka.TLS.Enabled = true
	cfg.Spec.Kafka.TLS.CACertSecret = "kafka-ca-secret"
	cfg.Spec.Cache.TLS.Enabled = true
	cfg.Spec.Cache.TLS.CACertSecretName = "cache-ca-secret"

	vols := KokuVolumes(cfg)
	if !volumeProjectsSecret(vols, "s3-ca-secret", "object-storage-ca.crt") {
		t.Error("KokuVolumes missing object storage CA in ca-extra")
	}
	if !volumeProjectsSecret(vols, "kafka-ca-secret", "kafka-ca.crt") {
		t.Error("KokuVolumes missing kafka CA in ca-extra")
	}
	if !volumeProjectsSecret(vols, "cache-ca-secret", "cache-ca.crt") {
		t.Error("KokuVolumes missing cache CA in ca-extra")
	}
	// Dedicated mounts remain for app env (KAFKA_SSL_CA_LOCATION / REDIS_SSL_CA_CERTS).
	if !volumeHasSecret(vols, "kafka-ca-cert", "kafka-ca-secret") {
		t.Error("dedicated kafka-ca-cert volume must remain")
	}
	if !volumeHasSecret(vols, "redis-tls-ca", "cache-ca-secret") {
		t.Error("dedicated redis-tls-ca volume must remain")
	}
}

func TestKokuVolumesOmitsCaExtraWhenEmpty(t *testing.T) {
	for _, v := range KokuVolumes(testCfg()) {
		if v.Name == caExtraVolumeName {
			t.Fatal("KokuVolumes must not include ca-extra when no user CAs are set")
		}
	}
}

func TestIngressInitMountsUserCAExtra(t *testing.T) {
	cfg := ingressCfg()
	cfg.Spec.ObjectStorage.CACertSecretName = "s3-ca-secret"
	dep := IngressDeployment(cfg)
	if !volumeProjectsSecret(dep.Spec.Template.Spec.Volumes, "s3-ca-secret", "object-storage-ca.crt") {
		t.Fatal("IngressDeployment missing object storage CA in ca-extra")
	}
	init := dep.Spec.Template.Spec.InitContainers[0]
	found := false
	for _, m := range init.VolumeMounts {
		if m.MountPath == "/ca-extra" {
			found = true
		}
	}
	if !found {
		t.Fatal("ingress prepare-ca-bundle must mount /ca-extra")
	}
}

func volumeProjectsSecret(vols []corev1.Volume, secretName, path string) bool {
	for _, v := range vols {
		if v.Projected == nil {
			continue
		}
		for _, src := range v.Projected.Sources {
			if src.Secret == nil {
				continue
			}
			if src.Secret.Name != secretName {
				continue
			}
			for _, item := range src.Secret.Items {
				if item.Path == path {
					return true
				}
			}
		}
	}
	return false
}

func volumeHasSecret(vols []corev1.Volume, volName, secretName string) bool {
	for _, v := range vols {
		if v.Name == volName && v.Secret != nil && v.Secret.SecretName == secretName {
			return true
		}
	}
	return false
}
