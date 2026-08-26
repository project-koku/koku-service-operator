package resources

import (
	"strings"
	"testing"

	costv1alpha1 "github.com/project-koku/koku-service-operator/api/v1alpha1"
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
					PruneOrphans:     boolPtr(true),
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
	cfg.Spec.RBAC.KeycloakSync.PruneOrphans = boolPtr(false)

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
