package resources

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestRBACRolePermissionsConfigMap(t *testing.T) {
	cfg := testCfg()
	cm := RBACRolePermissionsConfigMap(cfg)
	if cm.Name != NameRBACRolePermissionsConfigMap(cfg) {
		t.Errorf("Name = %q", cm.Name)
	}
	for _, file := range []string{"cost-management.json", "sources.json"} {
		raw, ok := cm.Data[file]
		if !ok {
			t.Fatalf("missing %s in permissions ConfigMap", file)
		}
		var parsed map[string][]map[string]string
		if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
			t.Fatalf("permissions %s is not valid JSON: %v", file, err)
		}
		if len(parsed) == 0 {
			t.Fatalf("permissions %s is empty", file)
		}
	}
}

func TestRBACRoleDefinitionsConfigMap(t *testing.T) {
	cfg := testCfg()
	cm := RBACRoleDefinitionsConfigMap(cfg)
	if cm.Name != NameRBACRoleDefinitionsConfigMap(cfg) {
		t.Errorf("Name = %q", cm.Name)
	}

	type roleDef struct {
		Name            string `json:"name"`
		AdminDefault    bool   `json:"admin_default"`
		PlatformDefault bool   `json:"platform_default"`
	}
	type rolesFile struct {
		Roles []roleDef `json:"roles"`
	}

	costRaw := cm.Data["cost-management.json"]
	var cost rolesFile
	if err := json.Unmarshal([]byte(costRaw), &cost); err != nil {
		t.Fatalf("cost-management definitions JSON: %v", err)
	}
	adminRoles := 0
	for _, role := range cost.Roles {
		if role.Name == "Cost Administrator" {
			if !role.AdminDefault {
				t.Error("Cost Administrator must have admin_default: true")
			}
			if role.PlatformDefault {
				t.Error("Cost Administrator must not have platform_default: true")
			}
			adminRoles++
		}
		if strings.Contains(role.Name, "Viewer") && role.PlatformDefault {
			t.Errorf("viewer role %q must not have platform_default: true", role.Name)
		}
	}
	if adminRoles != 1 {
		t.Errorf("expected exactly one Cost Administrator role, found %d", adminRoles)
	}

	sourcesRaw := cm.Data["sources.json"]
	var sources rolesFile
	if err := json.Unmarshal([]byte(sourcesRaw), &sources); err != nil {
		t.Fatalf("sources definitions JSON: %v", err)
	}
	foundSourcesAdmin := false
	for _, role := range sources.Roles {
		if role.Name == "Sources administrator" {
			if !role.AdminDefault {
				t.Error("Sources administrator must have admin_default: true")
			}
			foundSourcesAdmin = true
		}
	}
	if !foundSourcesAdmin {
		t.Fatal("missing Sources administrator role in definitions")
	}
}

func TestRBACAPIDeploymentMountsSeedConfig(t *testing.T) {
	cfg := testCfg()
	dep := RBACAPIDeployment(cfg)
	assertRBACSeedVolumeMounts(t, dep.Spec.Template.Spec.Volumes, dep.Spec.Template.Spec.Containers[0].VolumeMounts)
}

func TestRBACWorkerDeploymentMountsSeedConfig(t *testing.T) {
	cfg := testCfg()
	dep := RBACWorkerDeployment(cfg)
	assertRBACSeedVolumeMounts(t, dep.Spec.Template.Spec.Volumes, dep.Spec.Template.Spec.Containers[0].VolumeMounts)
}
