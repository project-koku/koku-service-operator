package resources

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
)

func TestRBACAPIDeployment_PrincipalProfileSidecar(t *testing.T) {
	old := OperatorImage
	OperatorImage = "quay.io/project-koku/koku-service-operator:test"
	defer func() { OperatorImage = old }()

	cfg := keycloakSyncCfg()
	dep := RBACAPIDeployment(cfg)
	containers := dep.Spec.Template.Spec.Containers
	if len(containers) < 2 {
		t.Fatalf("want rbac-api + principal-profile, got %d containers", len(containers))
	}
	var profile *corev1.Container
	var apiEnv []corev1.EnvVar
	for i := range containers {
		switch containers[i].Name {
		case "principal-profile":
			profile = &containers[i]
		case "rbac-api":
			apiEnv = containers[i].Env
		}
	}
	if profile == nil {
		t.Fatal("missing principal-profile sidecar")
	}
	if profile.Image != OperatorImage {
		t.Fatalf("sidecar image = %q", profile.Image)
	}
	if len(profile.Command) != 1 || profile.Command[0] != "/principal-profile" {
		t.Fatalf("command = %#v", profile.Command)
	}
	envMap := map[string]string{}
	for _, e := range profile.Env {
		if e.Value != "" {
			envMap[e.Name] = e.Value
		}
	}
	if envMap["PRINCIPAL_PROFILE_ADDR"] != principalProfileAddr {
		t.Fatalf("sidecar addr = %q, want %q", envMap["PRINCIPAL_PROFILE_ADDR"], principalProfileAddr)
	}
	if profile.ReadinessProbe == nil || profile.ReadinessProbe.Exec == nil {
		t.Fatal("readiness probe must be Exec --healthcheck (loopback bind)")
	}
	if got := profile.ReadinessProbe.Exec.Command; len(got) != 2 || got[0] != "/principal-profile" || got[1] != "--healthcheck" {
		t.Fatalf("readiness exec = %#v", got)
	}
	if envMap["KEYCLOAK_URL"] == "" || envMap["KEYCLOAK_CLIENT_ID"] == "" {
		t.Fatalf("sidecar missing Keycloak env: %#v", envMap)
	}
	foundURL := false
	for _, e := range apiEnv {
		if e.Name == "PRINCIPAL_PROFILE_SERVICE_URL" && e.Value == principalProfileURLLocal {
			foundURL = true
		}
		if e.Name == "KEYCLOAK_CLIENT_SECRET" {
			t.Fatal("rbac-api must not mount Keycloak client secret; sidecar owns Keycloak")
		}
	}
	if !foundURL {
		t.Fatal("rbac-api missing PRINCIPAL_PROFILE_SERVICE_URL")
	}

	cfg.Spec.RBAC.KeycloakSync.Enabled = false
	dep = RBACAPIDeployment(cfg)
	for _, c := range dep.Spec.Template.Spec.Containers {
		if c.Name == "principal-profile" {
			t.Fatal("sidecar must be absent when keycloakSync disabled")
		}
	}
}
