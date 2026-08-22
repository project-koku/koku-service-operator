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

// MigrationJob builds the Koku Django migration Job using the same image as
// the API/listener (spec.costManagement.api.image). Returns nil when that
// image is unset — callers must treat a missing image as an error rather than
// creating a Job with a hardcoded tag.
func MigrationJob(cfg *costv1alpha1.CostManagementServiceConfig, imageTag string) *batchv1.Job {
	image, ok := KokuImage(cfg)
	if !ok {
		return nil
	}

	env := KokuCommonEnv(cfg)
	env = append(env,
		EnvVal("MASU", "false"),
		EnvVal("PROMETHEUS_MULTIPROC_DIR", "/tmp"),
		EnvVal("KOKU_LOG_LEVEL", "INFO"),
		EnvVal("DJANGO_LOG_LEVEL", "INFO"),
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
// Returns nil when spec.ros.image repository or tag is unset.
func ROSMigrationJob(cfg *costv1alpha1.CostManagementServiceConfig, imageTag string) *batchv1.Job {
	image, ok := ImageRef(cfg.Spec.ROS.Image)
	if !ok {
		return nil
	}

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

// RBACMigrationJob builds the RBAC schema migration + chart-parity seeding Job.
// Combines migrate, built-in seeds, cost-management/sources role seed,
// admin_default groups, bootstrap_tenants, and platform_default cleanup.
// Returns nil when spec.rbac.image repository or tag is unset.
func RBACMigrationJob(cfg *costv1alpha1.CostManagementServiceConfig, imageTag string) *batchv1.Job {
	image, ok := ImageRef(cfg.Spec.RBAC.Image)
	if !ok {
		return nil
	}

	env := rbacMigrationEnv(cfg)
	script := rbacMigrationScript()

	vols := []corev1.Volume{{
		Name:         "tmp",
		VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
	}}
	mounts := []corev1.VolumeMount{{Name: "tmp", MountPath: "/tmp"}}

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
	return `set -e
echo "=== insights-rbac migration job ==="
cd /opt/rbac/rbac
python manage.py migrate --noinput
echo "✓ Migrations complete"
python manage.py seeds --skip-notifications
echo "✓ Built-in seeding complete"

echo "Seeding cost-management permissions and roles..."
python manage.py shell <<'SEED_SCRIPT'
from api.models import Tenant
from management.models import Permission, Role, Access

public_tenant = Tenant.objects.get(tenant_name='public')

cm_perms = [
    ("aws.account", "*"), ("aws.account", "read"),
    ("aws.organizational_unit", "*"), ("aws.organizational_unit", "read"),
    ("azure.subscription_guid", "*"), ("azure.subscription_guid", "read"),
    ("gcp.account", "*"), ("gcp.account", "read"),
    ("gcp.project", "*"), ("gcp.project", "read"),
    ("openshift.cluster", "*"), ("openshift.cluster", "read"),
    ("openshift.node", "*"), ("openshift.node", "read"),
    ("openshift.project", "*"), ("openshift.project", "read"),
    ("cost_model", "*"), ("cost_model", "read"), ("cost_model", "write"),
    ("settings", "*"), ("settings", "read"), ("settings", "write"),
    ("*", "*"),
]

perm_count = 0
for res, verb in cm_perms:
    _, created = Permission.objects.get_or_create(
        application="cost-management", resource_type=res, verb=verb,
        defaults={"permission": f"cost-management:{res}:{verb}", "tenant": public_tenant}
    )
    if created:
        perm_count += 1

_, created = Permission.objects.get_or_create(
    application="sources", resource_type="*", verb="*",
    defaults={"permission": "sources:*:*", "tenant": public_tenant}
)
if created:
    perm_count += 1
print(f"Seeded {perm_count} permissions")

roles = [
    ("Cost Administrator", "Perform any available operation on cost management resources.", True, False, [("cost-management", "*", "*")]),
    ("Cost Price List Administrator", "Perform read and write operations on cost models.", False, False, [("cost-management", "cost_model", "*"), ("cost-management", "settings", "*")]),
    ("Cost Price List Viewer", "Perform read operations on cost models.", False, False, [("cost-management", "cost_model", "read"), ("cost-management", "settings", "read")]),
    ("Cost Cloud Viewer", "Perform read operations on cost reports related to cloud sources.", False, False, [("cost-management", "aws.account", "*"), ("cost-management", "aws.organizational_unit", "*"), ("cost-management", "azure.subscription_guid", "*"), ("cost-management", "gcp.account", "*"), ("cost-management", "gcp.project", "*")]),
    ("Cost OpenShift Viewer", "Perform read operations on cost reports related to OpenShift sources.", False, False, [("cost-management", "openshift.cluster", "*")]),
    ("Sources administrator", "Perform any available operation on any source.", True, False, [("sources", "*", "*")]),
]

role_count = 0
for name, desc, admin_default, platform_default, access_list in roles:
    role, created = Role.objects.get_or_create(
        name=name, tenant=public_tenant,
        defaults={"description": desc, "system": True, "platform_default": platform_default, "admin_default": admin_default, "version": 2}
    )
    if created:
        role_count += 1
    for app, res, verb in access_list:
        perm = Permission.objects.get(application=app, resource_type=res, verb=verb, tenant=public_tenant)
        Access.objects.get_or_create(role=role, permission=perm, defaults={"tenant": public_tenant})

print(f"Seeded {role_count} roles (total: {Role.objects.count()})")
print("RBAC seeding complete.")
SEED_SCRIPT
echo "✓ Cost-management seeding complete"

echo "Creating admin_default group for org tenants..."
python manage.py shell <<'ADMIN_DEFAULT_SCRIPT'
from api.models import Tenant
from management.models import Group, Policy, Role

public_tenant = Tenant.objects.get(tenant_name='public')
admin_default_roles = Role.objects.filter(admin_default=True, tenant=public_tenant)
if not admin_default_roles.exists():
    print("WARNING: No admin_default roles found, skipping admin_default group")
else:
    user_tenants = Tenant.objects.exclude(tenant_name='public').filter(org_id__isnull=False)
    count = 0
    for tenant in user_tenants:
        grp, _ = Group.objects.get_or_create(
            name='Cost Admin Default', tenant=tenant,
            defaults={'admin_default': True, 'system': True,
                      'description': 'Admin default: grants admin_default roles to is_org_admin users'}
        )
        grp.admin_default = True
        grp.save()
        policy, _ = Policy.objects.get_or_create(
            name='Cost Admin Default Policy', tenant=tenant, group=grp
        )
        for role in admin_default_roles:
            policy.roles.add(role)
        count += 1
    role_names = list(admin_default_roles.values_list('name', flat=True))
    print(f"Created/updated admin_default group for {count} tenant(s) with roles: {role_names}")
ADMIN_DEFAULT_SCRIPT
echo "✓ Admin default group seeding complete"

set +e
python manage.py bootstrap_tenants --all -v 2
bootstrap_rc=$?
set -e
if [ $bootstrap_rc -ne 0 ]; then
  echo "WARNING: bootstrap_tenants exited with code $bootstrap_rc (non-fatal, continuing to cleanup)"
fi
echo "✓ Tenant bootstrap complete"

echo "Removing cost-management access from platform_default groups..."
python manage.py shell <<'CLEANUP_DEFAULTS'
from management.models import Group, Policy, Access

removed = 0
for group in Group.objects.filter(platform_default=True):
    for policy in Policy.objects.filter(group=group):
        for role in policy.roles.all():
            has_cm_access = Access.objects.filter(
                role=role,
                permission__application='cost-management',
            ).exists()
            if has_cm_access:
                policy.roles.remove(role)
                removed += 1
print(f"Removed {removed} role(s) with cost-management access from platform_default groups")
CLEANUP_DEFAULTS
echo "✓ Platform default cleanup complete"
echo "=== insights-rbac migration job completed ==="`
}

// AdminBootstrapJob builds the post-migrate Job that seeds a Tenant/Principal
// in insights-rbac. Returns nil when disabled, secretRef.name is empty, or the
// RBAC image is unset.
func AdminBootstrapJob(cfg *costv1alpha1.CostManagementServiceConfig, imageTag string) *batchv1.Job {
	ba := cfg.Spec.RBAC.BootstrapAdmin
	if !ba.Enabled || ba.SecretRef.Name == "" {
		return nil
	}
	secretName := ba.SecretRef.Name

	image, ok := ImageRef(cfg.Spec.RBAC.Image)
	if !ok {
		return nil
	}

	env := append(rbacEnv(cfg),
		EnvFromSecret("SYNC_ORG_ID", secretName, "org-id"),
		EnvFromSecret("SYNC_ACCOUNT_NUMBER", secretName, "account-number"),
		EnvFromSecret("SYNC_USERNAME", secretName, "username"),
	)
	script := rbacAdminBootstrapScript()
	vols := []corev1.Volume{{
		Name:         "tmp",
		VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
	}}
	mounts := []corev1.VolumeMount{{Name: "tmp", MountPath: "/tmp"}}

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
	return `set -e
echo "=== insights-rbac admin bootstrap job ==="
echo "Username: ${SYNC_USERNAME}"
echo "Org ID: ${SYNC_ORG_ID}"
echo "Account Number: ${SYNC_ACCOUNT_NUMBER}"
cd /opt/rbac/rbac
python manage.py shell <<'BOOTSTRAP_SCRIPT'
import os
from api.models import Tenant
from management.models import Group, Policy, Role, Principal
from django.core.cache import cache

username = os.environ['SYNC_USERNAME']
org_id = os.environ['SYNC_ORG_ID']
acct_number = os.environ['SYNC_ACCOUNT_NUMBER']

public_tenant = Tenant.objects.get(tenant_name='public')
admin_default_roles = Role.objects.filter(admin_default=True, tenant=public_tenant)
if not admin_default_roles.exists():
    print("ERROR: No admin_default roles found — migration job may not have completed")
    raise SystemExit(1)

tenant, created = Tenant.objects.get_or_create(
    org_id=org_id,
    defaults={'tenant_name': 'acct' + acct_number, 'ready': True}
)
print(f"{'Created' if created else 'Existing'} tenant for org_id={org_id}")

grp, _ = Group.objects.get_or_create(
    name='Cost Admin Default', tenant=tenant,
    defaults={'admin_default': True, 'system': True,
              'description': 'Admin default: grants admin_default roles to bootstrap admin user'}
)
grp.admin_default = True
grp.save()

policy, _ = Policy.objects.get_or_create(
    name='Cost Admin Default Policy', tenant=tenant, group=grp
)
for role in admin_default_roles:
    policy.roles.add(role)

principal, _ = Principal.objects.get_or_create(
    username=username, tenant=tenant,
    defaults={'type': 'user'}
)
grp.principals.add(principal)

role_names = list(admin_default_roles.values_list('name', flat=True))
cache.clear()
print(f"✓ User '{username}' granted {role_names} for org={org_id}")
BOOTSTRAP_SCRIPT
echo "✓ Admin user bootstrap complete"
set +e
python manage.py bootstrap_tenants --org-id "${SYNC_ORG_ID}" --force
bootstrap_rc=$?
set -e
if [ $bootstrap_rc -ne 0 ]; then
  echo "WARNING: bootstrap_tenants exited with code $bootstrap_rc (non-fatal)"
fi
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
