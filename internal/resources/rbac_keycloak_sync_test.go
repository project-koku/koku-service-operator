package resources

import (
	"strings"
	"testing"

	costv1alpha1 "github.com/project-koku/koku-service-operator/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func keycloakSyncCfg() *costv1alpha1.CostManagementServiceConfig {
	deploy := true
	return &costv1alpha1.CostManagementServiceConfig{
		ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "default"},
		Spec: costv1alpha1.CostManagementServiceConfigSpec{
			Database: costv1alpha1.DatabaseConfig{
				Deploy: &deploy,
			},
			RBAC: costv1alpha1.RBACConfig{
				Image: costv1alpha1.ImageSpec{
					Repository: "quay.io/test/rbac",
					Tag:        "latest",
				},
				KeycloakSync: costv1alpha1.KeycloakSyncSpec{
					Enabled:          true,
					Schedule:         "*/10 * * * *",
					ClientID:         "rbac-sync-client",
					ClientSecretRef:  costv1alpha1.SecretKeyRef{Name: "kc-secret", Key: "CLIENT_SECRET"},
					OrgGroupPrefix:   "org-",
					OrgAdminSubgroup: "org-admin",
					PruneOrphans:     new(true),
				},
			},
			Auth: costv1alpha1.AuthConfig{
				Keycloak: costv1alpha1.KeycloakSpec{
					URL:   "http://keycloak.keycloak.svc:8080",
					Realm: "kubernetes",
				},
			},
		},
	}
}

func TestKeycloakSyncConfigMap(t *testing.T) {
	cfg := keycloakSyncCfg()
	cm := KeycloakSyncConfigMap(cfg)

	if cm.Name != "test-rbac-keycloak-sync-script" {
		t.Errorf("Name = %q, want test-rbac-keycloak-sync-script", cm.Name)
	}
	script, ok := cm.Data["sync_keycloak_principals.py"]
	if !ok {
		t.Fatal("ConfigMap missing sync_keycloak_principals.py key")
	}
	if !strings.Contains(script, "class KeycloakClient") {
		t.Error("script should contain the KeycloakClient class")
	}
	if !strings.Contains(script, "def discover_and_sync") {
		t.Error("script should contain discover_and_sync function")
	}
}

func TestKeycloakSyncCronJobMeta(t *testing.T) {
	cfg := keycloakSyncCfg()
	cj := KeycloakSyncCronJob(cfg)

	if cj.Name != "test-rbac-keycloak-sync" {
		t.Errorf("Name = %q, want test-rbac-keycloak-sync", cj.Name)
	}
	if cj.Spec.Schedule != "*/10 * * * *" {
		t.Errorf("Schedule = %q, want */10 * * * *", cj.Spec.Schedule)
	}
	if cj.Spec.ConcurrencyPolicy != CronJobConcurrencyForbid {
		t.Errorf("ConcurrencyPolicy = %q, want %q", cj.Spec.ConcurrencyPolicy, CronJobConcurrencyForbid)
	}
	if cj.Spec.StartingDeadlineSeconds == nil || *cj.Spec.StartingDeadlineSeconds != CronJobStartingDeadlineSeconds {
		t.Errorf("StartingDeadlineSeconds = %v, want %d", cj.Spec.StartingDeadlineSeconds, CronJobStartingDeadlineSeconds)
	}
	if cj.Spec.SuccessfulJobsHistoryLimit == nil || *cj.Spec.SuccessfulJobsHistoryLimit != CronJobSuccessHistoryLimit {
		t.Errorf("SuccessfulJobsHistoryLimit = %v, want %d", cj.Spec.SuccessfulJobsHistoryLimit, CronJobSuccessHistoryLimit)
	}
	if cj.Spec.FailedJobsHistoryLimit == nil || *cj.Spec.FailedJobsHistoryLimit != CronJobFailedHistoryLimit {
		t.Errorf("FailedJobsHistoryLimit = %v, want %d", cj.Spec.FailedJobsHistoryLimit, CronJobFailedHistoryLimit)
	}

	jobSpec := cj.Spec.JobTemplate.Spec
	if jobSpec.ActiveDeadlineSeconds == nil || *jobSpec.ActiveDeadlineSeconds != CronJobActiveDeadlineSeconds {
		t.Errorf("ActiveDeadlineSeconds = %v, want %d", jobSpec.ActiveDeadlineSeconds, CronJobActiveDeadlineSeconds)
	}
	if jobSpec.BackoffLimit == nil || *jobSpec.BackoffLimit != CronJobBackoffLimit {
		t.Errorf("BackoffLimit = %v, want %d", jobSpec.BackoffLimit, CronJobBackoffLimit)
	}

	podSpec := jobSpec.Template.Spec
	if podSpec.RestartPolicy != CronJobRestartOnFailure {
		t.Errorf("RestartPolicy = %q, want %q", podSpec.RestartPolicy, CronJobRestartOnFailure)
	}
	if podSpec.AutomountServiceAccountToken == nil || *podSpec.AutomountServiceAccountToken {
		t.Errorf("AutomountServiceAccountToken = %v, want false", podSpec.AutomountServiceAccountToken)
	}
	if podSpec.ServiceAccountName != NameRBACServiceAccount(cfg) {
		t.Errorf("ServiceAccountName = %q, want %q", podSpec.ServiceAccountName, NameRBACServiceAccount(cfg))
	}
}

func TestKeycloakSyncCronJobContainer(t *testing.T) {
	cfg := keycloakSyncCfg()
	cj := KeycloakSyncCronJob(cfg)
	pod := cj.Spec.JobTemplate.Spec.Template.Spec

	if len(pod.Containers) != 1 {
		t.Fatalf("expected 1 container, got %d", len(pod.Containers))
	}
	c := pod.Containers[0]
	if c.Image != "quay.io/test/rbac:latest" {
		t.Errorf("Image = %q", c.Image)
	}
	if c.WorkingDir != "/opt/rbac/rbac" {
		t.Errorf("WorkingDir = %q", c.WorkingDir)
	}
	if len(pod.InitContainers) < 1 || pod.InitContainers[0].Name != "wait-for-postgres" {
		t.Error("expected wait-for-postgres init container")
	}
}

func TestKeycloakSyncCronJobEnv(t *testing.T) {
	cfg := keycloakSyncCfg()
	cj := KeycloakSyncCronJob(cfg)
	c := cj.Spec.JobTemplate.Spec.Template.Spec.Containers[0]

	envMap := map[string]string{}
	for _, e := range c.Env {
		if e.Value != "" {
			envMap[e.Name] = e.Value
		}
	}
	for _, want := range []struct{ key, val string }{
		{"KEYCLOAK_URL", "http://keycloak.keycloak.svc:8080"},
		{"KEYCLOAK_REALM", "kubernetes"},
		{"KEYCLOAK_CLIENT_ID", "rbac-sync-client"},
		{"KEYCLOAK_TLS_VERIFY", "true"},
		{"SYNC_ORG_GROUP_PREFIX", "org-"},
		{"SYNC_ORG_ADMIN_SUBGROUP", "org-admin"},
		{"SYNC_PRUNE_ORPHANS", "true"},
	} {
		if got := envMap[want.key]; got != want.val {
			t.Errorf("env %s = %q, want %q", want.key, got, want.val)
		}
	}
}

func TestKeycloakSyncCronJobSecretRef(t *testing.T) {
	cfg := keycloakSyncCfg()
	cj := KeycloakSyncCronJob(cfg)
	c := cj.Spec.JobTemplate.Spec.Template.Spec.Containers[0]

	for _, e := range c.Env {
		if e.Name == "KEYCLOAK_CLIENT_SECRET" && e.ValueFrom != nil && e.ValueFrom.SecretKeyRef != nil {
			if e.ValueFrom.SecretKeyRef.Name != "kc-secret" {
				t.Errorf("secret name = %q", e.ValueFrom.SecretKeyRef.Name)
			}
			if e.ValueFrom.SecretKeyRef.Key != "CLIENT_SECRET" {
				t.Errorf("secret key = %q", e.ValueFrom.SecretKeyRef.Key)
			}
			return
		}
	}
	t.Error("KEYCLOAK_CLIENT_SECRET should be a secretKeyRef")
}

func TestKeycloakSyncCronJobSecretRefDefaultsKey(t *testing.T) {
	cfg := keycloakSyncCfg()
	cfg.Spec.RBAC.KeycloakSync.ClientSecretRef.Key = ""
	cj := KeycloakSyncCronJob(cfg)
	c := cj.Spec.JobTemplate.Spec.Template.Spec.Containers[0]

	for _, e := range c.Env {
		if e.Name == "KEYCLOAK_CLIENT_SECRET" && e.ValueFrom != nil && e.ValueFrom.SecretKeyRef != nil {
			if e.ValueFrom.SecretKeyRef.Key != "CLIENT_SECRET" {
				t.Errorf("empty key should default to CLIENT_SECRET, got %q", e.ValueFrom.SecretKeyRef.Key)
			}
			return
		}
	}
	t.Error("KEYCLOAK_CLIENT_SECRET should be a secretKeyRef")
}

func TestKeycloakSyncCronJobVolumes(t *testing.T) {
	cfg := keycloakSyncCfg()
	cj := KeycloakSyncCronJob(cfg)
	c := cj.Spec.JobTemplate.Spec.Template.Spec.Containers[0]

	for _, m := range c.VolumeMounts {
		if m.Name == "sync-script" && m.SubPath == "sync_keycloak_principals.py" {
			return
		}
	}
	t.Error("missing sync-script volume mount")
}

func TestKeycloakSyncCronJobPruneOrphansDefault(t *testing.T) {
	cfg := keycloakSyncCfg()
	cfg.Spec.RBAC.KeycloakSync.PruneOrphans = nil

	cj := KeycloakSyncCronJob(cfg)
	envMap := map[string]string{}
	for _, e := range cj.Spec.JobTemplate.Spec.Template.Spec.Containers[0].Env {
		if e.Value != "" {
			envMap[e.Name] = e.Value
		}
	}
	if got := envMap["SYNC_PRUNE_ORPHANS"]; got != "true" {
		t.Errorf("nil PruneOrphans should default to true, got %q", got)
	}
}

func TestKeycloakSyncCronJobPruneOrphansFalse(t *testing.T) {
	cfg := keycloakSyncCfg()
	cfg.Spec.RBAC.KeycloakSync.PruneOrphans = new(false)

	cj := KeycloakSyncCronJob(cfg)
	envMap := map[string]string{}
	for _, e := range cj.Spec.JobTemplate.Spec.Template.Spec.Containers[0].Env {
		envMap[e.Name] = e.Value
	}
	if got := envMap["SYNC_PRUNE_ORPHANS"]; got != "false" {
		t.Errorf("explicit false PruneOrphans = %q, want false", got)
	}
}

func TestKeycloakSyncCronJobDefaults(t *testing.T) {
	cfg := keycloakSyncCfg()
	cfg.Spec.RBAC.KeycloakSync.Schedule = ""
	cfg.Spec.RBAC.KeycloakSync.OrgGroupPrefix = ""
	cfg.Spec.RBAC.KeycloakSync.OrgAdminSubgroup = ""

	cj := KeycloakSyncCronJob(cfg)
	if cj.Spec.Schedule != "*/15 * * * *" {
		t.Errorf("default schedule = %q, want */15 * * * *", cj.Spec.Schedule)
	}

	envMap := map[string]string{}
	for _, e := range cj.Spec.JobTemplate.Spec.Template.Spec.Containers[0].Env {
		if e.Value != "" {
			envMap[e.Name] = e.Value
		}
	}
	if envMap["SYNC_ORG_GROUP_PREFIX"] != "org-" {
		t.Errorf("default org group prefix = %q", envMap["SYNC_ORG_GROUP_PREFIX"])
	}
	if envMap["SYNC_ORG_ADMIN_SUBGROUP"] != "org-admin" {
		t.Errorf("default org admin subgroup = %q", envMap["SYNC_ORG_ADMIN_SUBGROUP"])
	}
}

func TestKeycloakSyncTLSVerifyFalse(t *testing.T) {
	cfg := keycloakSyncCfg()
	cfg.Spec.Auth.Keycloak.TLS.InsecureSkipVerify = true

	cj := KeycloakSyncCronJob(cfg)
	envMap := map[string]string{}
	for _, e := range cj.Spec.JobTemplate.Spec.Template.Spec.Containers[0].Env {
		if e.Value != "" {
			envMap[e.Name] = e.Value
		}
	}
	if envMap["KEYCLOAK_TLS_VERIFY"] != "false" {
		t.Errorf("TLS verify = %q, want false when insecureSkipVerify=true", envMap["KEYCLOAK_TLS_VERIFY"])
	}
}

func TestSyncScriptEmbedded(t *testing.T) {
	if syncKeycloakScript == "" {
		t.Fatal("embedded sync script is empty")
	}
	if !strings.Contains(syncKeycloakScript, "def main()") {
		t.Error("embedded script missing main() function")
	}
}

// review-follow-ups #7: the sync CronJob must trust the same Keycloak CA as
// oauth2-proxy (auth.keycloak.tls.caCertSecretName, else the service CA) so a
// private-CA Keycloak works without insecureSkipVerify.
func TestKeycloakSyncCronJobKeycloakCA(t *testing.T) {
	cases := []struct {
		name       string
		caSecret   string
		wantSecret bool
	}{
		{name: "caCertSecretName set", caSecret: "keycloak-ingress-ca", wantSecret: true},
		{name: "service CA fallback", caSecret: "", wantSecret: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := keycloakSyncCfg()
			cfg.Spec.Auth.Keycloak.URL = "https://keycloak.keycloak.svc:8443"
			cfg.Spec.Auth.Keycloak.TLS.CACertSecretName = tc.caSecret
			pod := KeycloakSyncCronJob(cfg).Spec.JobTemplate.Spec.Template.Spec
			c := pod.Containers[0]

			var vol *corev1.Volume
			for i := range pod.Volumes {
				if pod.Volumes[i].Name == "keycloak-ca" {
					vol = &pod.Volumes[i]
				}
			}
			if vol == nil {
				t.Fatal("missing keycloak-ca volume")
			}
			if tc.wantSecret {
				if vol.Secret == nil || vol.Secret.SecretName != tc.caSecret {
					t.Fatalf("keycloak-ca volume = %+v, want Secret %q", vol.VolumeSource, tc.caSecret)
				}
			} else if vol.ConfigMap == nil || vol.ConfigMap.Name != NameServiceCAConfigMap(cfg) {
				t.Fatalf("keycloak-ca volume = %+v, want service-CA ConfigMap", vol.VolumeSource)
			}

			mounted := false
			for _, m := range c.VolumeMounts {
				if m.Name == "keycloak-ca" && m.MountPath == "/etc/keycloak-ca" && m.ReadOnly {
					mounted = true
				}
			}
			if !mounted {
				t.Fatalf("keycloak-ca not mounted read-only at /etc/keycloak-ca: %+v", c.VolumeMounts)
			}

			env := map[string]string{}
			for _, e := range c.Env {
				env[e.Name] = e.Value
			}
			if env["KEYCLOAK_CA_FILE"] != "/etc/keycloak-ca/ca.crt" {
				t.Fatalf("KEYCLOAK_CA_FILE = %q, want /etc/keycloak-ca/ca.crt", env["KEYCLOAK_CA_FILE"])
			}
		})
	}
}

// The script must add KEYCLOAK_CA_FILE to the default trust store (not
// replace it, as SSL_CERT_FILE would for the whole manage.py process).
func TestSyncScriptLoadsKeycloakCA(t *testing.T) {
	for _, want := range []string{`os.environ.get("KEYCLOAK_CA_FILE"`, "load_verify_locations(cafile="} {
		if !strings.Contains(syncKeycloakScript, want) {
			t.Errorf("sync script missing %q", want)
		}
	}
}

// The CA is only read for verified HTTPS. For an http:// URL or
// insecureSkipVerify the CronJob must not depend on it: the service-CA
// ConfigMap is created empty and filled by injection, and an unmounted key
// would keep the Pod in ContainerCreating.
func TestKeycloakSyncCronJobNoCAWhenUnused(t *testing.T) {
	for _, tc := range []struct {
		name     string
		url      string
		insecure bool
	}{
		{name: "http URL", url: "http://keycloak.keycloak.svc:8080"},
		{name: "https with insecureSkipVerify", url: "https://keycloak.keycloak.svc:8443", insecure: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := keycloakSyncCfg()
			cfg.Spec.Auth.Keycloak.URL = tc.url
			cfg.Spec.Auth.Keycloak.TLS.InsecureSkipVerify = tc.insecure
			cfg.Spec.Auth.Keycloak.TLS.CACertSecretName = "keycloak-ingress-ca"
			pod := KeycloakSyncCronJob(cfg).Spec.JobTemplate.Spec.Template.Spec

			for _, v := range pod.Volumes {
				if v.Name == "keycloak-ca" {
					t.Fatalf("unexpected keycloak-ca volume: %+v", v.VolumeSource)
				}
			}
			for _, e := range pod.Containers[0].Env {
				if e.Name == "KEYCLOAK_CA_FILE" {
					t.Fatalf("unexpected KEYCLOAK_CA_FILE=%q", e.Value)
				}
			}
		})
	}
}
