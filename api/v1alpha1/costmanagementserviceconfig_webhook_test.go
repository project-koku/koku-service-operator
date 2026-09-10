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
	// buckets.ingress is optional (inherits koku), so it must NOT be required here.
	if strings.Contains(err.Error(), "spec.objectStorage.buckets.ingress") {
		t.Fatalf("buckets.ingress must be optional, but validation required it: %v", err)
	}
}

func TestCostManagementServiceConfigValidate_AcceptsValidBYOIConfigWithoutIngressBucket(t *testing.T) {
	t.Parallel()

	// A complete BYOI object-storage config that omits buckets.ingress must be
	// accepted: ingress uploads inherit buckets.koku. This is the positive
	// counterpart to the "ingress is optional" assertion and guards against
	// future validation that over-tightens and rejects valid BYOI CRs.
	cfg := &CostManagementServiceConfig{
		Spec: CostManagementServiceConfigSpec{
			Auth: AuthConfig{
				Keycloak: KeycloakSpec{
					URL: "https://keycloak.example.com",
				},
			},
			ObjectStorage: ObjectStorageConfig{
				Endpoint:   "s3.example.com",
				SecretName: "my-s3-credentials",
				Buckets: ObjectStorageBucketsSpec{
					Koku: "koku-bucket",
				},
			},
		},
	}

	if err := cfg.validateCostManagementServiceConfig(); err != nil {
		t.Fatalf("valid BYOI config with buckets.ingress omitted must pass, got %v", err)
	}
}

func TestCostManagementServiceConfigValidate_KeycloakURLProducesSingleError(t *testing.T) {
	t.Parallel()

	// Regression guard: validateKeycloakURL is the single canonical check. A
	// prior duplicate inline block reported the same missing URL twice; assert
	// the path appears exactly once so that duplication cannot silently return.
	cfg := &CostManagementServiceConfig{}
	err := cfg.validateCostManagementServiceConfig()
	if err == nil {
		t.Fatal("expected validation error for missing spec.auth.keycloak.url")
	}
	if got := strings.Count(err.Error(), "spec.auth.keycloak.url"); got != 1 {
		t.Fatalf("expected exactly one spec.auth.keycloak.url error, got %d: %v", got, err)
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
		t.Fatalf("expected objectStorage.buckets.ros validation error, got %v", err)
	}
}
