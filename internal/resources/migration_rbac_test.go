package resources

import (
	"os/exec"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	costv1alpha1 "github.com/project-koku/koku-service-operator/api/v1alpha1"
)

func TestRBACMigrationScriptUsesNativeSeeds(t *testing.T) {
	script := rbacMigrationScript()
	for _, want := range []string{
		"python manage.py migrate --noinput",
		"python manage.py seeds --skip-notifications",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("rbacMigrationScript missing %q", want)
		}
	}
	for _, bad := range []string{"SEED_SCRIPT", "manage.py shell", "seeds --groups"} {
		if strings.Contains(script, bad) {
			t.Errorf("rbacMigrationScript should not contain %q", bad)
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
	if got := job.Annotations["koku.costmanagement.io/image-tag"]; got != "test-cmseed2" {
		t.Errorf("image-tag annotation = %q, want test-cmseed2", got)
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

// TestMigrationScriptsSyntax runs bash -n on every migration script string to
// catch syntax errors (orphaned loop bodies, unclosed heredocs, etc.) that
// pattern-match tests would miss.
func TestMigrationScriptsSyntax(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	scripts := map[string]string{
		"kokuMigrationScript": kokuMigrationScript(),
		"rosMigrationScript":  rosMigrationScript(),
		"rbacMigrationScript": rbacMigrationScript(),
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
