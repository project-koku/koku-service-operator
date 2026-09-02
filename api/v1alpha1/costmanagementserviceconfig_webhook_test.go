package v1alpha1

import (
	"strings"
	"testing"
)

func TestCostManagementServiceConfigValidate_RequiresAuthKeycloakURL(t *testing.T) {
	t.Parallel()

	cfg := &CostManagementServiceConfig{}
	err := cfg.validateCostManagementServiceConfig()
	if err == nil {
		t.Fatal("expected validation error for missing spec.auth.keycloak.url")
	}
	if !strings.Contains(err.Error(), "spec.auth.keycloak.url") {
		t.Fatalf("expected auth.keycloak.url validation error, got %v", err)
	}
}

func TestCostManagementServiceConfigValidate_RejectsAuthKeycloakURLWithoutScheme(t *testing.T) {
	t.Parallel()

	cfg := &CostManagementServiceConfig{
		Spec: CostManagementServiceConfigSpec{
			Auth: AuthConfig{
				Keycloak: KeycloakSpec{
					URL: "keycloak.example.com",
				},
			},
		},
	}
	err := cfg.validateCostManagementServiceConfig()
	if err == nil {
		t.Fatal("expected validation error for auth.keycloak.url without scheme")
	}
	if !strings.Contains(err.Error(), "spec.auth.keycloak.url") {
		t.Fatalf("expected auth.keycloak.url validation error, got %v", err)
	}
}

func TestCostManagementServiceConfigValidate_RequiresKeycloakSyncSecretNameWhenEnabled(t *testing.T) {
	t.Parallel()

	cfg := &CostManagementServiceConfig{
		Spec: CostManagementServiceConfigSpec{
			Auth: AuthConfig{
				Keycloak: KeycloakSpec{
					URL: "https://keycloak.example.com",
				},
			},
			RBAC: RBACConfig{
				KeycloakSync: KeycloakSyncSpec{
					Enabled: true,
				},
			},
		},
	}
	err := cfg.validateCostManagementServiceConfig()
	if err == nil {
		t.Fatal("expected validation error for missing rbac.keycloakSync.clientSecretRef.name")
	}
	if !strings.Contains(err.Error(), "spec.rbac.keycloakSync.clientSecretRef.name") {
		t.Fatalf("expected keycloakSync clientSecretRef.name validation error, got %v", err)
	}
}
