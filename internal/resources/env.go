package resources

import (
	"cmp"
	"fmt"
	"slices"

	corev1 "k8s.io/api/core/v1"

	costv1alpha1 "github.com/project-koku/koku-service-operator/api/v1alpha1"
)

// KokuCommonEnv builds the environment variables shared by all Koku
// containers (API, Masu, Listener, Celery workers, migration Job).
// Mirrors the cost-onprem.koku.commonEnv Helm helper.
func KokuCommonEnv(cfg *costv1alpha1.CostManagementServiceConfig) []corev1.EnvVar {
	dbSecret := NameDBCredentials(cfg)
	storageSecret := NameStorageSecret(cfg)
	djangoSecret := NameDjangoSecret(cfg)

	cachePort := fmt.Sprintf("%d", cfg.Spec.Cache.Port)
	if cfg.Spec.Cache.Port == 0 {
		cachePort = "6379"
	}
	dbPort := fmt.Sprintf("%d", cfg.Spec.Database.Port)
	if cfg.Spec.Database.Port == 0 {
		dbPort = "5432"
	}

	env := []corev1.EnvVar{
		EnvVal("ONPREM", "True"),
		EnvVal("DATABASE_SERVICE_NAME", "database"),
		EnvVal("DATABASE_ENGINE", "postgresql"),
		EnvVal("DATABASE_SERVICE_HOST", DatabaseHost(cfg)),
		EnvVal("DATABASE_SERVICE_PORT", dbPort),
		EnvVal("DATABASE_NAME", "costonprem_koku"),
		EnvFromSecret("DATABASE_USER", dbSecret, "koku-user"),
		EnvFromSecret("DATABASE_PASSWORD", dbSecret, "koku-password"),
		EnvVal("REDIS_HOST", CacheHost(cfg)),
		EnvVal("REDIS_PORT", cachePort),
		EnvVal("INSIGHTS_KAFKA_HOST", KafkaHost(cfg)),
		EnvVal("INSIGHTS_KAFKA_PORT", KafkaPort(cfg)),

		EnvVal("S3_ENDPOINT", S3Endpoint(cfg)),
		EnvVal("REQUESTED_BUCKET", cfg.Spec.CostManagement.Storage.BucketName),
		EnvVal("REQUESTED_ROS_BUCKET", cfg.Spec.CostManagement.Storage.ROSBucketName),
		EnvVal("AWS_CA_BUNDLE", "/etc/pki/ca-trust/combined/ca-bundle.crt"),
		EnvVal("REQUESTS_CA_BUNDLE", "/etc/pki/ca-trust/combined/ca-bundle.crt"),
		EnvFromSecretOptional("AWS_ACCESS_KEY_ID", storageSecret, "access-key"),
		EnvFromSecretOptional("AWS_SECRET_ACCESS_KEY", storageSecret, "secret-key"),
		EnvVal("S3_REGION", cfg.Spec.ObjectStorage.S3.Region),
		EnvVal("AWS_CONFIG_FILE", "/etc/aws/config"),
		EnvFromSecret("DJANGO_SECRET_KEY", djangoSecret, "secret-key"),
		EnvVal("SCHEDULE_REPORT_CHECKS", boolStr(costv1alpha1.BoolVal(cfg.Spec.CostManagement.ScheduleReportChecks, true))),
		EnvVal("REPORT_DOWNLOAD_SCHEDULE", cfg.Spec.CostManagement.ReportDownloadSchedule),
		EnvVal("RETAIN_NUM_MONTHS", fmt.Sprintf("%d", cfg.Spec.CostManagement.DataRetentionMonths)),
		EnvVal("RBAC_SERVICE_HOST", NameRBACAPI(cfg)),
		EnvVal("RBAC_SERVICE_PORT", "8080"),
		EnvVal("RBAC_SERVICE_PATH", "/api/rbac/v1/access/"),
		EnvVal("RBAC_SERVICE_PROTOCOL", "http"),
	}

	// Celery result expiry (default 28800 = 8 hours)
	env = append(env, EnvVal("CELERY_RESULT_EXPIRES", "28800"))

	// Default to console-only logging. The koku settings.py configures a file
	// handler; setting this env var overrides which handlers loggers use so
	// Django doesn't try to write logs to the (read-only) container filesystem.
	env = append(env, EnvVal("DJANGO_LOG_HANDLERS", "console"))

	// Kafka SASL (BYOI secured Kafka)
	if cfg.Spec.Kafka.SASL.Mechanism != "" {
		env = append(env, EnvVal("KAFKA_SASL_MECHANISM", cfg.Spec.Kafka.SASL.Mechanism))
		if cfg.Spec.Kafka.SASL.ExistingSecret != "" {
			env = append(env,
				EnvFromSecret("KAFKA_SASL_USERNAME", cfg.Spec.Kafka.SASL.ExistingSecret, "username"),
				EnvFromSecret("KAFKA_SASL_PASSWORD", cfg.Spec.Kafka.SASL.ExistingSecret, "password"),
			)
		}
	}

	// Kafka TLS (BYOI secured Kafka)
	if cfg.Spec.Kafka.SecurityProtocol != "" && cfg.Spec.Kafka.SecurityProtocol != "PLAINTEXT" {
		env = append(env, EnvVal("KAFKA_SECURITY_PROTOCOL", cfg.Spec.Kafka.SecurityProtocol))
	}
	if cfg.Spec.Kafka.TLS.Enabled && cfg.Spec.Kafka.TLS.CACertSecret != "" {
		env = append(env, EnvVal("KAFKA_SSL_CA_LOCATION", "/etc/kafka/certs/ca.crt"))
	}

	// Optional: Valkey auth
	if cfg.Spec.Cache.Auth.Enabled && cfg.Spec.Cache.Auth.SecretName != "" {
		env = append(env,
			EnvFromSecretOptional("REDIS_USERNAME", cfg.Spec.Cache.Auth.SecretName, "redis-username"),
			EnvFromSecret("REDIS_PASSWORD", cfg.Spec.Cache.Auth.SecretName, "redis-password"),
		)
	}

	// Optional: Valkey TLS
	if cfg.Spec.Cache.TLS.Enabled {
		env = append(env, EnvVal("REDIS_SSL", "True"))
		if cfg.Spec.Cache.TLS.CACertSecretName != "" {
			env = append(env, EnvVal("REDIS_SSL_CA_CERTS", "/etc/redis-tls/ca.crt"))
		}
	}

	// Optional: currency URL
	if cfg.Spec.CostManagement.API.Env["CURRENCY_URL"] != "" {
		env = append(env, EnvVal("CURRENCY_URL", cfg.Spec.CostManagement.API.Env["CURRENCY_URL"]))
	}

	return env
}

func boolStr(b bool) string {
	if b {
		return "True"
	}
	return "False"
}

// MergeEnv replaces base entries whose key appears in overrides, then
// appends any override keys not already present. Keys are sorted so SSA
// applies produce a stable pod template.
func MergeEnv(base []corev1.EnvVar, overrides map[string]string) []corev1.EnvVar {
	if len(overrides) == 0 {
		return base
	}
	seen := make(map[string]bool, len(overrides))
	for i, e := range base {
		if v, ok := overrides[e.Name]; ok {
			base[i] = EnvVal(e.Name, v)
			seen[e.Name] = true
		}
	}
	keys := make([]string, 0, len(overrides))
	for k := range overrides {
		if !seen[k] {
			keys = append(keys, k)
		}
	}
	slices.SortFunc(keys, cmp.Compare)
	for _, k := range keys {
		base = append(base, EnvVal(k, overrides[k]))
	}
	return base
}
