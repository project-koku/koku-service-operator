package resources

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
)

// MigrationExecutionAnnotation identifies schema execution independently of the
// legacy image-tag annotation used by existing lifecycle tools.
const MigrationExecutionAnnotation = "koku.costmanagement.io/migration-execution"

// MigrationInputRevisionAnnotation versions mounted seed data independently of
// the application image's tag. Changing a descriptive tag on a digest pin must
// not invalidate an otherwise identical migration.
const MigrationInputRevisionAnnotation = "koku.costmanagement.io/migration-input-revision"

type migrationContainerInput struct {
	Name         string                 `json:"name"`
	Image        string                 `json:"image"`
	Command      []string               `json:"command,omitempty"`
	Args         []string               `json:"args,omitempty"`
	WorkingDir   string                 `json:"workingDir,omitempty"`
	Env          []corev1.EnvVar        `json:"env,omitempty"`
	EnvFrom      []corev1.EnvFromSource `json:"envFrom,omitempty"`
	VolumeMounts []corev1.VolumeMount   `json:"volumeMounts,omitempty"`
}

// migrationRuntimeOnlyEnv lists settings that `manage.py migrate` and the RBAC
// seed do not read: object storage, cache, Kafka, RBAC service wiring, report
// scheduling, retention, logging and cache tuning. Changing them must not stop
// every application writer for a no-op migration. Anything not listed still
// identifies the execution, so unknown or new inputs conservatively rerun.
var migrationRuntimeOnlyEnv = map[string]bool{
	"S3_ENDPOINT": true, "S3_REGION": true, "S3_ACCESS_KEY": true, "S3_SECRET": true,
	"REQUESTED_BUCKET": true, "REQUESTED_ROS_BUCKET": true,
	"AWS_ACCESS_KEY_ID": true, "AWS_SECRET_ACCESS_KEY": true, "AWS_CONFIG_FILE": true,
	"AWS_CA_BUNDLE": true, "REQUESTS_CA_BUNDLE": true,
	"REDIS_HOST": true, "REDIS_PORT": true, "REDIS_USERNAME": true, "REDIS_PASSWORD": true,
	"REDIS_SSL": true, "REDIS_SSL_CA_CERTS": true,
	"INSIGHTS_KAFKA_HOST": true, "INSIGHTS_KAFKA_PORT": true, "KAFKA_SASL_MECHANISM": true,
	"KAFKA_SASL_USERNAME": true, "KAFKA_SASL_PASSWORD": true, "KAFKA_SECURITY_PROTOCOL": true,
	"KAFKA_SSL_CA_LOCATION": true,
	"RBAC_SERVICE_HOST":     true, "RBAC_SERVICE_PORT": true, "RBAC_SERVICE_PATH": true,
	"RBAC_SERVICE_PROTOCOL": true, "RBAC_CACHE_TIMEOUT": true,
	"SCHEDULE_REPORT_CHECKS": true, "REPORT_DOWNLOAD_SCHEDULE": true, "RETAIN_NUM_MONTHS": true,
	"POLLING_TIMER": true, "INITIAL_INGEST_NUM_MONTHS": true, "INITIAL_INGEST_OVERRIDE": true,
	"NOTIFICATION_CHECK_TIME": true, "CELERY_RESULT_EXPIRES": true, "CURRENCY_URL": true,
	"GUNICORN_LOG_LEVEL": true, "DJANGO_LOG_LEVEL": true, "DJANGO_LOG_FORMATTER": true,
	"DJANGO_LOG_HANDLERS": true, "KOKU_LOG_LEVEL": true, "KOKU_ENABLE_SENTRY": true,
	"CACHED_VIEWS_DISABLED": true, "CACHE_TIMEOUT": true, "TAG_ENABLED_LIMIT": true,
	"PROMETHEUS_MULTIPROC_DIR": true,
}

type migrationVolumeInput struct {
	Name      string `json:"name"`
	ConfigMap string `json:"configMap,omitempty"`
	Secret    string `json:"secret,omitempty"`
}

// MigrationExecutionID fingerprints the migration image, command, database
// connection references, and seed revision. It can also identify legacy Jobs that predate
// the annotation. The live Pod template is authoritative, not a copied receipt.
//
// Init-container images, resource sizing, retry budgets, and scheduling do not
// identify schema execution: changing the operator's wait-for image must not
// rerun a completed migration, nor may runtime-only settings listed in
// migrationRuntimeOnlyEnv. Referenced Secret/ConfigMap contents are not read;
// mounted migration input changes must carry an explicit seed/script revision.
func MigrationExecutionID(job *batchv1.Job) string {
	input := struct {
		Version    string                    `json:"version"`
		Revision   string                    `json:"revision"`
		Containers []migrationContainerInput `json:"containers"`
		Volumes    []migrationVolumeInput    `json:"volumes,omitempty"`
	}{Version: "v2", Revision: job.Annotations[MigrationInputRevisionAnnotation]}
	if input.Revision == "" && job.Labels[labelComponent] == "rbac-migration" {
		// Legacy RBAC Jobs stored the seed revision as a suffix of the tag.
		tag := job.Annotations[MigrationImageTagAnnotation]
		if index := strings.LastIndex(tag, "-cmseed"); index >= 0 {
			input.Revision = tag[index+1:]
		}
	}
	for _, container := range job.Spec.Template.Spec.Containers {
		input.Containers = append(input.Containers, migrationContainerInput{
			Name: container.Name, Image: container.Image,
			Command: container.Command, Args: container.Args, WorkingDir: container.WorkingDir,
			Env: migrationEnv(container.Env), EnvFrom: container.EnvFrom, VolumeMounts: container.VolumeMounts,
		})
	}
	for _, volume := range job.Spec.Template.Spec.Volumes {
		ref := migrationVolumeInput{Name: volume.Name}
		if volume.ConfigMap != nil {
			ref.ConfigMap = volume.ConfigMap.Name
		}
		if volume.Secret != nil {
			ref.Secret = volume.Secret.SecretName
		}
		if ref.ConfigMap != "" || ref.Secret != "" {
			input.Volumes = append(input.Volumes, ref)
		}
	}
	// The input contains only JSON-supported structs, strings, and slices.
	data, _ := json.Marshal(input)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func migrationEnv(env []corev1.EnvVar) []corev1.EnvVar {
	var kept []corev1.EnvVar
	for _, e := range env {
		if !migrationRuntimeOnlyEnv[e.Name] {
			kept = append(kept, e)
		}
	}
	return kept
}
