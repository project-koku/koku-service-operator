package resources

import (
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	costv1alpha1 "github.com/project-koku/koku-service-operator/api/v1alpha1"
)

func TestRBACMigrationScriptSeedsCostManagementAndSources(t *testing.T) {
	script := rbacMigrationScript()
	for _, want := range []string{
		"sources:*:*",
		"Cost Administrator",
		"Sources administrator",
		"admin_default",
		"Cost Admin Default",
		"platform_default",
		"PERMISSION_SEEDING_ENABLED", // env is separate; script should still complete
		"bootstrap_tenants --all",
	} {
		if want == "PERMISSION_SEEDING_ENABLED" {
			continue // asserted on Job env below
		}
		if !strings.Contains(script, want) {
			t.Errorf("rbacMigrationScript missing %q", want)
		}
	}
}

func TestRBACMigrationJobEnvEnablesSeeding(t *testing.T) {
	cfg := &costv1alpha1.CostManagementServiceConfig{
		ObjectMeta: metav1.ObjectMeta{Name: "cost-onprem", Namespace: "cost-tests"},
		Spec: costv1alpha1.CostManagementServiceConfigSpec{
			RBAC: costv1alpha1.RBACConfig{
				Image: costv1alpha1.ImageSpec{Repository: "rbac", Tag: "test"},
			},
		},
	}
	job := RBACMigrationJob(cfg, "test")
	if got := job.Annotations["koku.costmanagement.io/image-tag"]; got != "test-cmseed1" {
		t.Errorf("image-tag annotation = %q, want test-cmseed1", got)
	}
	env := map[string]string{}
	for _, e := range job.Spec.Template.Spec.Containers[0].Env {
		env[e.Name] = e.Value
	}
	for _, k := range []string{"PERMISSION_SEEDING_ENABLED", "ROLE_SEEDING_ENABLED", "GROUP_SEEDING_ENABLED"} {
		if env[k] != "True" {
			t.Errorf("env %s = %q, want True", k, env[k])
		}
	}
}

func TestAdminBootstrapJobGated(t *testing.T) {
	cfg := &costv1alpha1.CostManagementServiceConfig{
		ObjectMeta: metav1.ObjectMeta{Name: "cost-onprem", Namespace: "cost-tests"},
		Spec: costv1alpha1.CostManagementServiceConfigSpec{
			RBAC: costv1alpha1.RBACConfig{
				Image: costv1alpha1.ImageSpec{Repository: "rbac", Tag: "test"},
			},
		},
	}
	if job := AdminBootstrapJob(cfg, "test"); job != nil {
		t.Fatal("expected nil when bootstrapAdmin.enabled is false")
	}
	cfg.Spec.RBAC.BootstrapAdmin.Enabled = true
	if job := AdminBootstrapJob(cfg, "test"); job != nil {
		t.Fatal("expected nil when no orgAdmin realm user")
	}
	cfg.Spec.Auth.RealmUsers = []costv1alpha1.RealmUser{{
		Username: "admin", OrgID: "org1234567", AccountNumber: "7890123", OrgAdmin: true,
	}}
	job := AdminBootstrapJob(cfg, "test")
	if job == nil {
		t.Fatal("expected AdminBootstrapJob when enabled with orgAdmin user")
	}
	if job.Name != "cost-onprem-rbac-admin-bootstrap" {
		t.Errorf("name = %q", job.Name)
	}
	full := strings.Join(job.Spec.Template.Spec.Containers[0].Command, "\n")
	if !strings.Contains(full, "Cost Admin Default") {
		t.Error("bootstrap script missing Cost Admin Default group")
	}
	env := map[string]string{}
	for _, e := range job.Spec.Template.Spec.Containers[0].Env {
		env[e.Name] = e.Value
	}
	if env["SYNC_USERNAME"] != "admin" || env["SYNC_ORG_ID"] != "org1234567" {
		t.Errorf("bootstrap env = username=%q org=%q", env["SYNC_USERNAME"], env["SYNC_ORG_ID"])
	}
}

func TestResolveBootstrapAdmin(t *testing.T) {
	cfg := &costv1alpha1.CostManagementServiceConfig{}
	if _, ok := ResolveBootstrapAdmin(cfg); ok {
		t.Fatal("expected no identity")
	}
	cfg.Spec.Auth.RealmUsers = []costv1alpha1.RealmUser{
		{Username: "viewer", OrgAdmin: false},
		{Username: "admin", OrgID: "org9", AccountNumber: "acct9", OrgAdmin: true},
	}
	id, ok := ResolveBootstrapAdmin(cfg)
	if !ok || id.Username != "admin" || id.OrgID != "org9" {
		t.Fatalf("got %+v ok=%v", id, ok)
	}
}
