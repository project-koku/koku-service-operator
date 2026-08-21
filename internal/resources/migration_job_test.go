package resources

import (
	"slices"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	costv1alpha1 "github.com/project-koku/koku-service-operator/api/v1alpha1"
)

func minimalCRForResources(name, ns string) *costv1alpha1.CostManagementServiceConfig {
	return &costv1alpha1.CostManagementServiceConfig{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: ns,
			UID:       "test-uid-1234",
		},
	}
}

// TestMigrationJob_UsesConstants verifies Job spec uses package-level constants
// for backoffLimit, activeDeadlineSeconds, and the image-tag annotation key.
func TestMigrationJob_UsesConstants(t *testing.T) {
	cfg := minimalCRForResources("test", "ns")
	cfg.Spec.Database.Deploy = new(true)
	cfg.Spec.CostManagement.API.Image.Tag = "v1"

	job := MigrationJob(cfg, "v1")

	if *job.Spec.BackoffLimit != MigrationBackoffLimit {
		t.Errorf("BackoffLimit = %d, want %d (MigrationBackoffLimit)", *job.Spec.BackoffLimit, MigrationBackoffLimit)
	}
	if *job.Spec.ActiveDeadlineSeconds != MigrationDeadlineSeconds {
		t.Errorf("ActiveDeadlineSeconds = %d, want %d (MigrationDeadlineSeconds)", *job.Spec.ActiveDeadlineSeconds, MigrationDeadlineSeconds)
	}
	if job.Annotations[MigrationImageTagAnnotation] != "v1" {
		t.Errorf("Annotation %q = %q, want v1", MigrationImageTagAnnotation, job.Annotations[MigrationImageTagAnnotation])
	}
}

// TestMigrationJob_TTLNil verifies TTLSecondsAfterFinished is nil.
// A TTL would cause Job GC and re-run migrations on every reconcile (~hourly).
func TestMigrationJob_TTLNil(t *testing.T) {
	cfg := minimalCRForResources("test", "ns")
	cfg.Spec.Database.Deploy = new(true)
	cfg.Spec.CostManagement.API.Image.Tag = "v1"

	job := MigrationJob(cfg, "v1")

	if job.Spec.TTLSecondsAfterFinished != nil {
		t.Errorf("TTLSecondsAfterFinished should be nil, got %d", *job.Spec.TTLSecondsAfterFinished)
	}
}

// TestMigrationJobNames verifies naming convention for all 4 migration Jobs.
func TestMigrationJobNames(t *testing.T) {
	cfg := minimalCRForResources("cost-onprem", "cost-tests")

	tests := map[string]string{
		"Koku":           "cost-onprem-koku-migrate",
		"ROS":            "cost-onprem-ros-migrate",
		"RBAC":           "cost-onprem-rbac-migrate",
		"AdminBootstrap": "cost-onprem-rbac-admin-bootstrap",
	}

	for name, want := range tests {
		var got string
		switch name {
		case "Koku":
			got = NameKokuMigration(cfg)
		case "ROS":
			got = NameROSMigration(cfg)
		case "RBAC":
			got = NameRBACMigration(cfg)
		case "AdminBootstrap":
			got = NameRBACAdminBootstrap(cfg)
		}
		if got != want {
			t.Errorf("%s: got %q, want %q", name, got, want)
		}
	}
}

// TestRBACSeedTagFormat verifies the seed revision suffix format.
func TestRBACSeedTagFormat(t *testing.T) {
	if RBACSeedJobTag("v1.2.3") != "v1.2.3-cmseed1" {
		t.Errorf("RBACSeedJobTag format wrong")
	}
}

// TestMigrationJob_ContainerResources verifies container resource quantities.
func TestMigrationJob_ContainerResources(t *testing.T) {
	cfg := minimalCRForResources("test", "ns")
	cfg.Spec.Database.Deploy = new(true)
	cfg.Spec.CostManagement.API.Image.Tag = "v1"

	job := MigrationJob(cfg, "v1")
	container := job.Spec.Template.Spec.Containers[0]

	cpuReq := container.Resources.Requests[corev1.ResourceCPU]
	memReq := container.Resources.Requests[corev1.ResourceMemory]
	cpuLim := container.Resources.Limits[corev1.ResourceCPU]
	memLim := container.Resources.Limits[corev1.ResourceMemory]

	if cpuReq.String() != "250m" {
		t.Errorf("CPU request = %s, want 250m", cpuReq.String())
	}
	if memReq.String() != "512Mi" {
		t.Errorf("Memory request = %s, want 512Mi", memReq.String())
	}
	if cpuLim.String() != "500m" {
		t.Errorf("CPU limit = %s, want 500m", cpuLim.String())
	}
	if memLim.String() != "1Gi" {
		t.Errorf("Memory limit = %s, want 1Gi", memLim.String())
	}
}

// TestMigrationJob_ContainerCapabilitiesDropAll verifies Capabilities.Drop includes ALL.
func TestMigrationJob_ContainerCapabilitiesDropAll(t *testing.T) {
	cfg := minimalCRForResources("test", "ns")
	cfg.Spec.Database.Deploy = new(true)
	cfg.Spec.CostManagement.API.Image.Tag = "v1"

	job := MigrationJob(cfg, "v1")
	container := job.Spec.Template.Spec.Containers[0]

	if container.SecurityContext == nil || container.SecurityContext.Capabilities == nil || len(container.SecurityContext.Capabilities.Drop) == 0 {
		t.Fatal("expected container SecurityContext with Capabilities.Drop")
	}
	if !slices.Contains(container.SecurityContext.Capabilities.Drop, "ALL") {
		t.Errorf("expected Capabilities.Drop to include ALL, got %v", container.SecurityContext.Capabilities.Drop)
	}
}

// TestAdminBootstrapJob_SecretKeyRefKeys verifies the specific secret keys used.
func TestAdminBootstrapJob_SecretKeyRefKeys(t *testing.T) {
	cfg := minimalCRForResources("test", "ns")
	cfg.Spec.Database.Deploy = new(true)
	cfg.Spec.RBAC.Image.Tag = "rbac-tag"
	cfg.Spec.RBAC.BootstrapAdmin.Enabled = true
	cfg.Spec.RBAC.BootstrapAdmin.SecretRef.Name = "rbac-bootstrap-admin"

	job := AdminBootstrapJob(cfg, "rbac-tag")
	if job == nil {
		t.Fatal("expected AdminBootstrapJob when enabled with secretRef set")
		return
	}

	envSecrets := map[string]string{}
	for _, e := range job.Spec.Template.Spec.Containers[0].Env {
		if e.ValueFrom != nil && e.ValueFrom.SecretKeyRef != nil {
			envSecrets[e.Name] = e.ValueFrom.SecretKeyRef.Key
		}
	}

	for _, k := range []string{"SYNC_ORG_ID", "SYNC_ACCOUNT_NUMBER", "SYNC_USERNAME"} {
		if envSecrets[k] == "" {
			t.Errorf("env %s must use secretKeyRef", k)
		}
	}
	if envSecrets["SYNC_ORG_ID"] != "org-id" || envSecrets["SYNC_ACCOUNT_NUMBER"] != "account-number" || envSecrets["SYNC_USERNAME"] != "username" {
		t.Errorf("secretKeyRef keys incorrect: %+v", envSecrets)
	}
}
