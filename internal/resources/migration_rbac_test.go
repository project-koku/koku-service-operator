package resources

import (
	"os/exec"
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
	if sa := job.Spec.Template.Spec.ServiceAccountName; sa != NameRBACServiceAccount(cfg) {
		t.Errorf("RBAC migration ServiceAccountName = %q, want %q", sa, NameRBACServiceAccount(cfg))
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
		t.Fatal("expected nil when secretRef.name is empty")
	}
	cfg.Spec.RBAC.BootstrapAdmin.SecretRef.Name = "rbac-bootstrap-admin"
	job := AdminBootstrapJob(cfg, "test")
	if job == nil {
		t.Fatal("expected AdminBootstrapJob when enabled with secretRef set")
		return
	}
	if job.Name != "cost-onprem-rbac-admin-bootstrap" {
		t.Errorf("name = %q", job.Name)
	}
	if sa := job.Spec.Template.Spec.ServiceAccountName; sa != NameRBACServiceAccount(cfg) {
		t.Errorf("bootstrap ServiceAccountName = %q, want %q", sa, NameRBACServiceAccount(cfg))
	}
	full := strings.Join(job.Spec.Template.Spec.Containers[0].Command, "\n")
	if !strings.Contains(full, "Cost Admin Default") {
		t.Error("bootstrap script missing Cost Admin Default group")
	}
	// Identity values must come from the Secret via secretKeyRef — never hardcoded.
	for _, e := range job.Spec.Template.Spec.Containers[0].Env {
		if e.Name == "SYNC_ORG_ID" || e.Name == "SYNC_ACCOUNT_NUMBER" || e.Name == "SYNC_USERNAME" {
			if e.Value != "" {
				t.Errorf("env %s has inline value %q — must use secretKeyRef", e.Name, e.Value)
			}
			if e.ValueFrom == nil || e.ValueFrom.SecretKeyRef == nil {
				t.Errorf("env %s must use secretKeyRef, got %+v", e.Name, e.ValueFrom)
			}
			if e.ValueFrom != nil && e.ValueFrom.SecretKeyRef != nil &&
				e.ValueFrom.SecretKeyRef.Name != "rbac-bootstrap-admin" {
				t.Errorf("env %s secretKeyRef.name = %q, want rbac-bootstrap-admin",
					e.Name, e.ValueFrom.SecretKeyRef.Name)
			}
		}
	}
}

// TestMigrationScriptsSyntax runs bash -n on every migration script string to
// catch syntax errors (orphaned loop bodies, unclosed heredocs, etc.) that
// pattern-match tests would miss.
func TestMigrationScriptsSyntax(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	scripts := map[string]string{
		"kokuMigrationScript":      kokuMigrationScript(),
		"rosMigrationScript":       rosMigrationScript(),
		"rbacMigrationScript":      rbacMigrationScript(),
		"rbacAdminBootstrapScript": rbacAdminBootstrapScript(),
	}
	for name, script := range scripts {
		t.Run(name, func(t *testing.T) {
			cmd := exec.Command("bash", "-n", "-c", script)
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Errorf("%s has a bash syntax error:\n%s", name, string(out))
			}
		})
	}
}
