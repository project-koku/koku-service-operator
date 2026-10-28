package resources

import (
	"strings"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	costv1alpha1 "github.com/project-koku/koku-service-operator/api/v1alpha1"
)

func TestMigrationExecutionID_ExecutionChanges(t *testing.T) {
	t.Parallel()
	cfg := minimalCRForResources("cm", "ns")
	job := MigrationJob(cfg, "v1")
	for _, test := range []struct {
		name   string
		change func(*batchv1.Job)
	}{
		{"same tag in a different repository", func(j *batchv1.Job) { j.Spec.Template.Spec.Containers[0].Image = "quay.io/other/koku:v1" }},
		{"image digest", func(j *batchv1.Job) {
			j.Spec.Template.Spec.Containers[0].Image = "quay.io/test/koku@sha256:" + strings.Repeat("a", 64)
		}},
		{"operator migration script", func(j *batchv1.Job) {
			j.Spec.Template.Spec.Containers[0].Command[2] += "\npython manage.py new_migration_step"
		}},
		{"migration arguments", func(j *batchv1.Job) { j.Spec.Template.Spec.Containers[0].Args = []string{"--new-option"} }},
		{"working directory", func(j *batchv1.Job) { j.Spec.Template.Spec.Containers[0].WorkingDir = "/different-project" }},
		{"database binding", func(j *batchv1.Job) {
			j.Spec.Template.Spec.Containers[0].Env = append(j.Spec.Template.Spec.Containers[0].Env, EnvVal("DATABASE_SERVICE_HOST", "new-db"))
		}},
		{"secret reference", func(j *batchv1.Job) {
			j.Spec.Template.Spec.Containers[0].Env = append(j.Spec.Template.Spec.Containers[0].Env, EnvFromSecret("NEW_KEY", "other-secret", "password"))
		}},
		{"envFrom input", func(j *batchv1.Job) {
			j.Spec.Template.Spec.Containers[0].EnvFrom = []corev1.EnvFromSource{{ConfigMapRef: &corev1.ConfigMapEnvSource{LocalObjectReference: corev1.LocalObjectReference{Name: "migration-input"}}}}
		}},
		{"mounted configmap reference", func(j *batchv1.Job) { j.Spec.Template.Spec.Volumes[1].ConfigMap.Name = "new-config" }},
		{"mounted secret reference", func(j *batchv1.Job) {
			j.Spec.Template.Spec.Volumes = append(j.Spec.Template.Spec.Volumes, corev1.Volume{Name: "input", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: "new-input"}}})
		}},
		{"volume mount", func(j *batchv1.Job) { j.Spec.Template.Spec.Containers[0].VolumeMounts[1].MountPath = "/new-path" }},
		{"seed input revision", func(j *batchv1.Job) { j.Annotations[MigrationInputRevisionAnnotation] = "seed-v2" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			changed := job.DeepCopy()
			test.change(changed)
			if MigrationExecutionID(changed) == MigrationExecutionID(job) {
				t.Fatal("changed migration execution must invalidate its receipt")
			}
		})
	}
}

func TestMigrationExecutionID_RuntimeChangesDoNotInvalidateReceipt(t *testing.T) {
	t.Parallel()
	job := MigrationJob(minimalCRForResources("cm", "ns"), "v1")
	changed := job.DeepCopy()
	changed.Spec.Template.Spec.InitContainers[1].Image = "quay.io/operator/new:v2"
	changed.Spec.Template.Spec.Containers[0].Resources.Requests[corev1.ResourceMemory] = resource.MustParse("1Gi")
	changed.Spec.Template.Spec.Containers[0].ImagePullPolicy = corev1.PullAlways
	changed.Spec.ActiveDeadlineSeconds = new(int64(1200))
	// Kubernetes defaults these after creation. Defaulting must not cause
	// perpetual migration replay when comparing a stored and a desired Job.
	changed.Spec.Template.Spec.Volumes[1].ConfigMap.DefaultMode = new(int32(0644))
	changed.Spec.Template.Spec.ServiceAccountName = "different-runtime-serviceaccount"
	changed.Annotations[MigrationExecutionAnnotation] = "untrusted-copied-annotation"
	if MigrationExecutionID(changed) != MigrationExecutionID(job) {
		t.Fatal("runtime-only changes or a copied annotation must not replay schema execution")
	}
}

func TestMigrationExecutionID_DigestPinIgnoresDescriptiveTag(t *testing.T) {
	t.Parallel()
	cfg := minimalCRForResources("cm", "ns")
	cfg.Spec.CostManagement.API.Image.Digest = "sha256:" + strings.Repeat("a", 64)
	first := MigrationJob(cfg, "v1")
	cfg.Spec.CostManagement.API.Image.Tag = "v2"
	second := MigrationJob(cfg, "v2")
	if MigrationExecutionID(first) != MigrationExecutionID(second) {
		t.Fatal("same immutable image and script must not replay for a descriptive tag edit")
	}
}

func TestMigrationExecutionID_LegacyRBACSeedReceipt(t *testing.T) {
	t.Parallel()
	cfg := minimalCRForResources("cm", "ns")
	cfg.Spec.RBAC.Image = costv1alpha1.ImageSpec{Repository: "quay.io/test/rbac", Tag: "v1"}
	current := RBACMigrationJob(cfg, "v1")
	legacy := current.DeepCopy()
	delete(legacy.Annotations, MigrationExecutionAnnotation)
	delete(legacy.Annotations, MigrationInputRevisionAnnotation)
	if MigrationExecutionID(legacy) != MigrationExecutionID(current) {
		t.Fatal("matching legacy seed revision should be adopted without replay")
	}
	legacy.Annotations[MigrationImageTagAnnotation] = "v1-cmseed1"
	if MigrationExecutionID(legacy) == MigrationExecutionID(current) {
		t.Fatal("a previous mounted seed revision must invalidate its receipt")
	}
}

func TestMigrationJobs_RecordExecutionIdentity(t *testing.T) {
	t.Parallel()
	cfg := minimalCRForResources("cm", "ns")
	cfg.Spec.RBAC.Image = costv1alpha1.ImageSpec{Repository: "r", Tag: "v1"}
	cfg.Spec.ROS.Image = costv1alpha1.ImageSpec{Repository: "ros", Tag: "v1"}
	for _, job := range []*batchv1.Job{MigrationJob(cfg, "v1"), RBACMigrationJob(cfg, "v1"), ROSMigrationJob(cfg, "v1")} {
		if got := job.Annotations[MigrationExecutionAnnotation]; len(got) != 64 || got != MigrationExecutionID(job) {
			t.Errorf("%s execution annotation = %q; expected computed SHA-256", job.Name, got)
		}
	}
}

// Day-2 runtime settings must not rerun `manage.py migrate`: a changed ID makes
// the upgrade planner stop every application writer for a no-op migration.
func TestMigrationExecutionID_IgnoresRuntimeOnlySettings(t *testing.T) {
	t.Parallel()
	base := minimalCRForResources("cm", "ns")
	want := MigrationExecutionID(MigrationJob(base, "v1"))
	for _, test := range []struct {
		name   string
		change func(*costv1alpha1.CostManagementServiceConfig)
	}{
		{"data retention", func(c *costv1alpha1.CostManagementServiceConfig) { c.Spec.CostManagement.DataRetentionMonths = 9 }},
		{"report download schedule", func(c *costv1alpha1.CostManagementServiceConfig) {
			c.Spec.CostManagement.ReportDownloadSchedule = "*/30 * * * *"
		}},
		{"report checks toggle", func(c *costv1alpha1.CostManagementServiceConfig) {
			c.Spec.CostManagement.ScheduleReportChecks = new(false)
		}},
		{"S3 endpoint", func(c *costv1alpha1.CostManagementServiceConfig) { c.Spec.ObjectStorage.Endpoint = "s3.example.com" }},
		{"S3 bucket", func(c *costv1alpha1.CostManagementServiceConfig) { c.Spec.ObjectStorage.Buckets.Koku = "other-bucket" }},
		{"discovered S3 lost", func(c *costv1alpha1.CostManagementServiceConfig) {
			c.Status.DiscoveredConfig = &costv1alpha1.DiscoveredConfig{S3: &costv1alpha1.DiscoveredS3{Endpoint: "https://s3.odf.svc", Bucket: "obc-bucket"}}
		}},
		{"cache host", func(c *costv1alpha1.CostManagementServiceConfig) { c.Spec.Cache.Host = "valkey.other.svc" }},
		{"kafka bootstrap", func(c *costv1alpha1.CostManagementServiceConfig) {
			c.Spec.Kafka.BootstrapServers = "kafka.other.svc:9092"
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			cfg := base.DeepCopy()
			test.change(cfg)
			if got := MigrationExecutionID(MigrationJob(cfg, "v1")); got != want {
				t.Fatalf("runtime-only %s change altered the migration execution identity", test.name)
			}
		})
	}
}

func TestMigrationExecutionID_UnknownEnvStillCounts(t *testing.T) {
	t.Parallel()
	job := MigrationJob(minimalCRForResources("cm", "ns"), "v1")
	before := MigrationExecutionID(job)
	job.Spec.Template.Spec.Containers[0].Env = append(job.Spec.Template.Spec.Containers[0].Env, EnvVal("KOKU_NEW_MIGRATION_TOGGLE", "true"))
	if MigrationExecutionID(job) == before {
		t.Fatal("unknown environment inputs must conservatively change the execution identity")
	}
}
