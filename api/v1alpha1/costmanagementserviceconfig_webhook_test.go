package v1alpha1

import (
	"strings"
	"testing"
)

func TestCostManagementServiceConfigValidate_RequiresObjectStorageEndpointAndBucketWhenSecretProvided(t *testing.T) {
	t.Parallel()

	cfg := &CostManagementServiceConfig{
		Spec: CostManagementServiceConfigSpec{
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
