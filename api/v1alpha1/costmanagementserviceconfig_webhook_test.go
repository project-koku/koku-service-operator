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

func TestCostManagementServiceConfigValidate_RequiresObjectStorageEndpointAndBucketWhenSecretProvided(t *testing.T) {
	t.Parallel()

	cfg := &CostManagementServiceConfig{
		Spec: CostManagementServiceConfigSpec{
			Auth: AuthConfig{
				Keycloak: KeycloakSpec{
					URL: "https://keycloak.example.com",
				},
			},
			ObjectStorage: ObjectStorageConfig{
				SecretName: "my-s3-credentials",
			},
		},
	}

	err := cfg.validateCostManagementServiceConfig()
	if err == nil {
		t.Fatal("expected validation error for incomplete explicit object storage config")
	}
	if !strings.Contains(err.Error(), "spec.objectStorage.endpoint") {
		t.Fatalf("expected objectStorage.endpoint validation error, got %v", err)
	}
	if !strings.Contains(err.Error(), "spec.objectStorage.buckets.koku") {
		t.Fatalf("expected objectStorage.buckets.koku validation error, got %v", err)
	}
	if !strings.Contains(err.Error(), "spec.objectStorage.buckets.ingress") {
		t.Fatalf("expected objectStorage.buckets.ingress validation error, got %v", err)
	}
}

func TestCostManagementServiceConfigValidate_RequiresROSBucketWhenROSEnabled(t *testing.T) {
	t.Parallel()

	enabled := true
	cfg := &CostManagementServiceConfig{
		Spec: CostManagementServiceConfigSpec{
			Auth: AuthConfig{
				Keycloak: KeycloakSpec{
					URL: "https://keycloak.example.com",
				},
			},
			ROS: ROSConfig{Enabled: &enabled},
			ObjectStorage: ObjectStorageConfig{
				Endpoint:   "s3.example.com",
				SecretName: "my-s3-credentials",
				Buckets: ObjectStorageBucketsSpec{
					Koku:    "koku-bucket",
					Ingress: "koku-upload-bucket",
				},
			},
		},
	}

	err := cfg.validateCostManagementServiceConfig()
	if err == nil {
		t.Fatal("expected validation error for missing ros bucket")
	}
	if !strings.Contains(err.Error(), "spec.objectStorage.buckets.ros") {
		t.Fatalf("expected rosBucketName validation error, got %v", err)
	}
}
