package resources

import (
	"strings"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	costv1alpha1 "github.com/project-koku/koku-service-operator/api/v1alpha1"
)

const (
	rbacDBName = RbacDBName

	// MigrationBackoffLimit is the number of Kubernetes retries per Job,
	// matching the COST-7685 specification.
	MigrationBackoffLimit = int32(3)
	// MigrationDeadlineSeconds is the hard timeout per migration Job.
	MigrationDeadlineSeconds = int64(600)

	// MigrationImageTagAnnotation is the annotation key used to track the
	// image tag for upgrade detection.
	MigrationImageTagAnnotation = "koku.costmanagement.io/image-tag"
)

// NameROSMigration returns the ROS migration Job name.
func NameROSMigration(cfg *costv1alpha1.CostManagementServiceConfig) string {
	return cfg.Name + "-ros-migrate"
}

// NameRBACMigration returns the RBAC migration Job name.
func NameRBACMigration(cfg *costv1alpha1.CostManagementServiceConfig) string {
	return cfg.Name + "-rbac-migrate"
}

// NameRBACAdminBootstrap returns the RBAC admin-bootstrap Job name.
func NameRBACAdminBootstrap(cfg *costv1alpha1.CostManagementServiceConfig) string {
	return cfg.Name + "-rbac-admin-bootstrap"
}

// rbacSeedRevision bumps when the migrate/seed script changes so completed
// Jobs are recreated (runMigrationStep keys off the image-tag annotation).
const rbacSeedRevision = "cmseed1"

// RBACSeedJobTag returns the annotation value used for RBAC migrate/bootstrap
// Jobs (image tag + seed revision).
func RBACSeedJobTag(imageTag string) string {
	return imageTag + "-" + rbacSeedRevision
}

// -----------------------------------------------------------------------------
// Koku migration
// -----------------------------------------------------------------------------

// MigrationJob builds the Koku Django migration Job.
func MigrationJob(cfg *costv1alpha1.CostManagementServiceConfig, imageTag string) *batchv1.Job {
	image := cfg.Spec.CostManagement.API.Image.Repository + ":" + cfg.Spec.CostManagement.API.Image.Tag
	if image == ":" {
		image = "quay.io/redhat-services-prod/cost-mgmt-dev-tenant/koku:latest"
	}

	env := KokuCommonEnv(cfg)
	env = append(env,
		EnvVal("MASU", "false"),
		EnvVal("PROMETHEUS_MULTIPROC_DIR", "/tmp"),
		EnvVal("KOKU_LOG_LEVEL", "INFO"),
	)

	host := DatabaseHost(cfg)
	dbPort := cfg.Spec.Database.Port
	if dbPort == 0 {
		dbPort = 5432
	}
	return migrationJob(cfg, NameKokuMigration(cfg), image, imageTag,
		"cost-management-migration", kokuMigrationScript(), env,
		KokuVolumeMounts(cfg), KokuVolumes(cfg),
		[]corev1.Container{
			CACombineInitContainer(cfg),
			// DB wait via init container — no /dev/tcp expansion in the script.
			waitForPostgres(cfg, host, int32String(dbPort)),
		},
	)
}

func kokuMigrationScript() string {
	// DB readiness is handled by the waitForPostgres init container above.
	return `set -e
mkdir -p /tmp/prometheus
cd /opt/koku/koku
python manage.py migrate --noinput
echo "=== Koku migrations completed ==="`
}

// -----------------------------------------------------------------------------
// ROS migration
// -----------------------------------------------------------------------------

// ROSMigrationJob builds the ROS schema migration Job.
func ROSMigrationJob(cfg *costv1alpha1.CostManagementServiceConfig, imageTag string) *batchv1.Job {
	image := cfg.Spec.ROS.Image.Repository + ":" + cfg.Spec.ROS.Image.Tag

	dbSecret := NameDBCredentials(cfg)
	host := DatabaseHost(cfg)
	port := cfg.Spec.Database.Port
	if port == 0 {
		port = 5432
	}

	env := []corev1.EnvVar{
		EnvVal("CLOWDER_ENABLED", "false"),
		EnvVal("DB_HOST", host),
		EnvVal("DB_PORT", int32String(port)),
		EnvVal("DB_NAME", rosDBName),
		EnvFromSecret("DB_USER", dbSecret, "ros-user"),
		EnvFromSecret("DB_PASSWORD", dbSecret, "ros-password"),
		EnvVal("LOG_LEVEL", "INFO"),
	}

	vols := []corev1.Volume{{
		Name:         "tmp",
		VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
	}}
	mounts := []corev1.VolumeMount{{Name: "tmp", MountPath: "/tmp"}}

	return migrationJob(cfg, NameROSMigration(cfg), image, imageTag,
		"ros-migration", rosMigrationScript(), env, mounts, vols,
		[]corev1.Container{waitForPostgres(cfg, host, int32String(port))},
	)
}

func rosMigrationScript() string {
	// DB readiness is handled by the waitForPostgres init container.
	return `set -e
echo "=== ROS Database Migrations ==="
./rosocp db migrate up
echo "=== ROS migrations completed ==="`
}

// -----------------------------------------------------------------------------
// RBAC migration + seeding
// -----------------------------------------------------------------------------

// RBACMigrationJob builds the RBAC schema migration + on-prem seeding Job.
// Catalog JSON is mounted from ConfigMaps; seeds loads permissions, roles, and groups.
func RBACMigrationJob(cfg *costv1alpha1.CostManagementServiceConfig, imageTag string) *batchv1.Job {
	image := cfg.Spec.RBAC.Image.Repository + ":" + cfg.Spec.RBAC.Image.Tag

	env := rbacMigrationEnv(cfg)
	script := rbacMigrationScript()

	vols, mounts := rbacVolumesAndMounts(cfg)

	host := DatabaseHost(cfg)
	dbPort := cfg.Spec.Database.Port
	if dbPort == 0 {
		dbPort = 5432
	}
	return migrationJob(cfg, NameRBACMigration(cfg), image, RBACSeedJobTag(imageTag),
		"rbac-migration", script, env, mounts, vols,
		[]corev1.Container{waitForPostgres(cfg, host, int32String(dbPort))},
	)
}

func rbacMigrationEnv(cfg *costv1alpha1.CostManagementServiceConfig) []corev1.EnvVar {
	// Same on-prem env as API/worker so seeds (V2 UUIDs, BOP bypass) succeed.
	return append(rbacEnv(cfg),
		EnvVal("PERMISSION_SEEDING_ENABLED", "True"),
		EnvVal("ROLE_SEEDING_ENABLED", "True"),
		EnvVal("GROUP_SEEDING_ENABLED", "True"),
	)
}

func rbacMigrationScript() string {
	// DB readiness is handled by the waitForPostgres init container.
	// Cost/sources catalog is mounted from ConfigMaps; seeds loads permissions → roles → groups.
	return `set -e
echo "=== insights-rbac migration job ==="
cd /opt/rbac/rbac
python manage.py migrate --noinput
echo "✓ Migrations complete"
python manage.py seeds --skip-notifications
echo "✓ Built-in seeding complete"
set +e
python manage.py bootstrap_tenants --all -v 2
bootstrap_rc=$?
set -e
if [ $bootstrap_rc -ne 0 ]; then
  echo "WARNING: bootstrap_tenants exited with code $bootstrap_rc (non-fatal)"
else
  echo "✓ Tenant bootstrap complete"
fi
echo "=== insights-rbac migration job completed ==="`
}

// AdminBootstrapJob builds the post-migrate Job that seeds a Tenant/Principal
// in insights-rbac. Returns nil when disabled or secretRef.name is empty.
func AdminBootstrapJob(cfg *costv1alpha1.CostManagementServiceConfig, imageTag string) *batchv1.Job {
	ba := cfg.Spec.RBAC.BootstrapAdmin
	if !ba.Enabled || ba.SecretRef.Name == "" {
		return nil
	}
	secretName := ba.SecretRef.Name

	image := cfg.Spec.RBAC.Image.Repository + ":" + cfg.Spec.RBAC.Image.Tag

	baseEnv := rbacEnv(cfg)
	env := make([]corev1.EnvVar, len(baseEnv), len(baseEnv)+3)
	copy(env, baseEnv)
	env = append(env,
		EnvFromSecret("SYNC_ORG_ID", secretName, "org-id"),
		EnvFromSecret("SYNC_ACCOUNT_NUMBER", secretName, "account-number"),
		EnvFromSecret("SYNC_USERNAME", secretName, "username"),
	)
	script := rbacAdminBootstrapScript()
	vols, mounts := rbacVolumesAndMounts(cfg)

	host := DatabaseHost(cfg)
	dbPort := cfg.Spec.Database.Port
	if dbPort == 0 {
		dbPort = 5432
	}
	return migrationJob(cfg, NameRBACAdminBootstrap(cfg), image, RBACSeedJobTag(imageTag),
		"rbac-admin-bootstrap", script, env, mounts, vols,
		[]corev1.Container{waitForPostgres(cfg, host, int32String(dbPort))},
	)
}

func rbacAdminBootstrapScript() string {
	// DB readiness is handled by the waitForPostgres init container.
	// Identity comes from Secret via env; ensure_user is in the RBAC image (COST-8088).
	return `set -e
echo "=== insights-rbac admin bootstrap job ==="
cd /opt/rbac/rbac
python manage.py ensure_user \
  --username "${SYNC_USERNAME}" \
  --org-id "${SYNC_ORG_ID}" \
  --account-number "${SYNC_ACCOUNT_NUMBER}" \
  --application cost-management \
  --application sources \
  --admin \
  --admin-group-name "Cost Admin Default" \
  --admin-group-description "Admin default: grants admin_default roles to bootstrap admin user" \
  --admin-policy-name "Cost Admin Default Policy"
echo "=== insights-rbac admin bootstrap job completed ==="`
}

// -----------------------------------------------------------------------------
// Shared job builder
// -----------------------------------------------------------------------------

func migrationJob(
	cfg *costv1alpha1.CostManagementServiceConfig,
	name, image, imageTag, component, script string,
	env []corev1.EnvVar,
	mounts []corev1.VolumeMount,
	vols []corev1.Volume,
	initContainers []corev1.Container,
) *batchv1.Job {
	backoff := MigrationBackoffLimit
	deadline := MigrationDeadlineSeconds
	return &batchv1.Job{
		TypeMeta: metav1.TypeMeta{APIVersion: "batch/v1", Kind: "Job"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: cfg.Namespace,
			Labels:    Labels(cfg, component),
			Annotations: map[string]string{
				MigrationImageTagAnnotation: imageTag,
			},
		},
		Spec: batchv1.JobSpec{
			BackoffLimit:          &backoff,
			ActiveDeadlineSeconds: &deadline,
			// TTLSecondsAfterFinished intentionally absent: runMigrationStep uses
			// Job existence to determine whether a migration has already completed.
			// Setting a TTL causes GC to delete the Job, making the controller
			// re-run migrations on every subsequent reconcile (~every hour).
			// Failed Jobs must also persist so the operator holds in Degraded state
			// rather than silently retrying.
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: Labels(cfg, component)},
				Spec: corev1.PodSpec{
					ServiceAccountName:           migrationServiceAccountName(cfg, component),
					AutomountServiceAccountToken: boolPtr(false),
					RestartPolicy:                corev1.RestartPolicyOnFailure,
					SecurityContext:              nonRootPodSC(),
					ImagePullSecrets:             imagePullSecrets(cfg),
					InitContainers:               initContainers,
					Containers: []corev1.Container{{
						Name:            "migrate",
						Image:           image,
						ImagePullPolicy: pullPolicy(cfg),
						Command:         []string{"bash", "-c", script},
						Env:             env,
						Resources: corev1.ResourceRequirements{
							Requests: corev1.ResourceList{
								corev1.ResourceCPU:    resource.MustParse("250m"),
								corev1.ResourceMemory: resource.MustParse("512Mi"),
							},
							Limits: corev1.ResourceList{
								corev1.ResourceCPU:    resource.MustParse("500m"),
								corev1.ResourceMemory: resource.MustParse("1Gi"),
							},
						},
						VolumeMounts:    mounts,
						SecurityContext: migrationContainerSC(),
					}},
					Volumes: vols,
				},
			},
		},
	}
}

func boolPtr(b bool) *bool { return &b }

// migrationServiceAccountName selects the family SA for a migration/bootstrap Job.
// RBAC jobs share {cr}-rbac; Koku and ROS migrations keep {cr}-koku (ROS SA
// wiring is COST-8054).
func migrationServiceAccountName(cfg *costv1alpha1.CostManagementServiceConfig, component string) string {
	if strings.HasPrefix(component, "rbac-") {
		return NameRBACServiceAccount(cfg)
	}
	return NameKokuServiceAccount(cfg)
}
