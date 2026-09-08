package resources

import (
	"encoding/json"
	"strings"
	"testing"

	costv1alpha1 "github.com/project-koku/koku-service-operator/api/v1alpha1"
)

func TestRBACSeedPermissionsConfigMap(t *testing.T) {
	cfg := testCfg()
	cm, err := RBACSeedPermissionsConfigMap(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if cm.Name != NameRBACSeedPermissionsConfigMap(cfg) {
		t.Errorf("Name = %q", cm.Name)
	}
	for _, key := range []string{"cost-management.json", "sources.json"} {
		raw, ok := cm.Data[key]
		if !ok || raw == "" {
			t.Fatalf("missing %s", key)
		}
		if !json.Valid([]byte(raw)) {
			t.Fatalf("%s is not valid JSON", key)
		}
	}
}

func TestRBACSeedDefinitionsConfigMap(t *testing.T) {
	cfg := testCfg()
	cm, err := RBACSeedDefinitionsConfigMap(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if cm.Name != NameRBACSeedDefinitionsConfigMap(cfg) {
		t.Errorf("Name = %q", cm.Name)
	}
	for _, key := range []string{"cost-management.json", "sources.json"} {
		raw, ok := cm.Data[key]
		if !ok || raw == "" {
			t.Fatalf("missing %s", key)
		}
		if !json.Valid([]byte(raw)) {
			t.Fatalf("%s is not valid JSON", key)
		}
	}
	for _, want := range []string{"Cost Administrator", "Sources administrator", "admin_default"} {
		if !strings.Contains(cm.Data["cost-management.json"], want) && !strings.Contains(cm.Data["sources.json"], want) {
			t.Errorf("definitions missing %q", want)
		}
	}
}

func TestRBACMigrationJob_MountsSeedConfigMaps(t *testing.T) {
	cfg := testCfg()
	cfg.Spec.RBAC.Image = costv1alpha1.ImageSpec{Repository: "rbac", Tag: "test"}
	job := RBACMigrationJob(cfg, "test")
	if job == nil {
		t.Fatal("RBACMigrationJob returned nil")
	}

	volNames := map[string]bool{}
	for _, v := range job.Spec.Template.Spec.Volumes {
		volNames[v.Name] = true
		if v.ConfigMap != nil {
			switch v.Name {
			case "rbac-seed-permissions":
				if v.ConfigMap.Name != NameRBACSeedPermissionsConfigMap(cfg) {
					t.Errorf("permissions volume ConfigMap = %q", v.ConfigMap.Name)
				}
			case "rbac-seed-definitions":
				if v.ConfigMap.Name != NameRBACSeedDefinitionsConfigMap(cfg) {
					t.Errorf("definitions volume ConfigMap = %q", v.ConfigMap.Name)
				}
			}
		}
	}
	for _, want := range []string{"tmp", "rbac-seed-permissions", "rbac-seed-definitions"} {
		if !volNames[want] {
			t.Errorf("missing volume %q", want)
		}
	}

	mountPaths := map[string]bool{}
	for _, m := range job.Spec.Template.Spec.Containers[0].VolumeMounts {
		mountPaths[m.MountPath] = true
	}
	for _, want := range []string{
		rbacSeedPermissionsMountDir + "/cost-management.json",
		rbacSeedPermissionsMountDir + "/sources.json",
		rbacSeedDefinitionsMountDir + "/cost-management.json",
		rbacSeedDefinitionsMountDir + "/sources.json",
	} {
		if !mountPaths[want] {
			t.Errorf("missing volume mount %q", want)
		}
	}
}
