package resources

import (
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	costv1alpha1 "github.com/project-koku/koku-service-operator/api/v1alpha1"
)

const rbacAPIPort = int32(8080)

// rbacEnv returns the standard env vars for all RBAC containers.
// Values mirror cost-onprem-chart _helpers-rbac.tpl (on-prem: no BOP, no Kafka,
// placeholder system role UUIDs for V2 bootstrap).
func rbacEnv(cfg *costv1alpha1.CostManagementServiceConfig) []corev1.EnvVar {
	dbSecret := NameDBCredentials(cfg)
	host := DatabaseHost(cfg)
	port := cfg.Spec.Database.Port
	if port == 0 {
		port = 5432
	}
	sslMode := cfg.Spec.Database.SSLMode
	if sslMode == "" {
		sslMode = "disable"
	}
	env := []corev1.EnvVar{
		// Chart sets /api/rbac; Django mounts v1 under that prefix. Using
		// /api/rbac/v1 makes unauthenticated /api/rbac/v1/status/ probes 401.
		EnvVal("API_PATH_PREFIX", "/api/rbac"),
		EnvVal("DATABASE_NAME", rbacDBName),
		EnvFromSecret("DATABASE_USER", dbSecret, "rbac-user"),
		EnvFromSecret("DATABASE_PASSWORD", dbSecret, "rbac-password"),
		EnvVal("DATABASE_HOST", host),
		EnvVal("DATABASE_PORT", int32String(port)),
		EnvVal("REDIS_HOST", CacheHost(cfg)),
		EnvVal("REDIS_PORT", cachePortStr(cfg)),
		EnvFromSecret("DJANGO_SECRET_KEY", NameDjangoSecret(cfg), "secret-key"),
		EnvVal("V2_BOOTSTRAP_TENANT", "True"),
		// Placeholder UUIDs required by RBAC V2/Kessel bootstrap (same as chart).
		EnvVal("SYSTEM_DEFAULT_ROOT_WORKSPACE_ROLE_UUID", "00000000-0000-4000-a000-000000000001"),
		EnvVal("SYSTEM_DEFAULT_TENANT_ROLE_UUID", "00000000-0000-4000-a000-000000000002"),
		EnvVal("SYSTEM_ADMIN_ROOT_WORKSPACE_ROLE_UUID", "00000000-0000-4000-a000-000000000003"),
		EnvVal("SYSTEM_ADMIN_TENANT_ROLE_UUID", "00000000-0000-4000-a000-000000000004"),
		EnvVal("CLOWDER_ENABLED", "false"),
		// On-prem has no Back Office Proxy; accept X-Rh-Identity from Envoy.
		EnvVal("BYPASS_BOP_VERIFICATION", "True"),
		EnvVal("KAFKA_ENABLED", "false"),
		EnvVal("PGSSLMODE", sslMode),
		EnvVal("DJANGO_LOG_LEVEL", "INFO"),
		EnvVal("RBAC_LOG_LEVEL", "INFO"),
		EnvVal("DJANGO_LOG_FORMATTER", "simple"),
		EnvVal("DJANGO_LOG_HANDLERS", "console"),
		EnvVal("ACCESS_CACHE_ENABLED", "True"),
	}
	if cfg.Spec.Cache.Auth.Enabled && cfg.Spec.Cache.Auth.SecretName != "" {
		env = append(env,
			EnvFromSecretOptional("REDIS_USERNAME", cfg.Spec.Cache.Auth.SecretName, "redis-username"),
			EnvFromSecret("REDIS_PASSWORD", cfg.Spec.Cache.Auth.SecretName, "redis-password"),
		)
	}
	if cfg.Spec.Cache.TLS.Enabled {
		env = append(env, EnvVal("REDIS_SSL", "True"))
		if cfg.Spec.Cache.TLS.CACertSecretName != "" {
			env = append(env, EnvVal("REDIS_SSL_CA_CERTS", "/etc/redis-tls/ca.crt"))
		}
	}
	return env
}

// rbacVolumesAndMounts returns shared volumes for RBAC pods (/tmp + optional Valkey TLS).
func rbacVolumesAndMounts(cfg *costv1alpha1.CostManagementServiceConfig) ([]corev1.Volume, []corev1.VolumeMount) {
	vols := []corev1.Volume{{
		Name:         "tmp",
		VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
	}}
	mounts := []corev1.VolumeMount{{Name: "tmp", MountPath: "/tmp"}}
	if cfg.Spec.Cache.TLS.Enabled && cfg.Spec.Cache.TLS.CACertSecretName != "" {
		vols = append(vols, corev1.Volume{
			Name: "redis-tls-ca",
			VolumeSource: corev1.VolumeSource{
				Secret: &corev1.SecretVolumeSource{
					SecretName: cfg.Spec.Cache.TLS.CACertSecretName,
					Items:      []corev1.KeyToPath{{Key: "ca.crt", Path: "ca.crt"}},
				},
			},
		})
		mounts = append(mounts, corev1.VolumeMount{Name: "redis-tls-ca", MountPath: "/etc/redis-tls", ReadOnly: true})
	}
	return vols, mounts
}

// rbacAppContainerSC is used for RBAC API/worker containers. readOnlyRootFilesystem
// is omitted because gunicorn needs a writable temp dir and Django configures a
// file log handler at startup (same pattern as kokuAppContainerSC).
func rbacAppContainerSC() *corev1.SecurityContext {
	f := false
	t := true
	return &corev1.SecurityContext{
		AllowPrivilegeEscalation: &f,
		Privileged:               &f,
		RunAsNonRoot:             &t,
		Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
	}
}

// waitForRBACDB blocks until the database is ready to accept RBAC connections.
func waitForRBACDB(cfg *costv1alpha1.CostManagementServiceConfig) corev1.Container {
	host := DatabaseHost(cfg)
	port := cfg.Spec.Database.Port
	if port == 0 {
		port = 5432
	}
	return waitForPostgres(cfg, host, int32String(port))
}

// -----------------------------------------------------------------------------
// RBAC API
// -----------------------------------------------------------------------------

// RBACAPIDeployment builds the RBAC Gunicorn API Deployment.
// Koku delegates all permission checks to this service via HTTP.
func RBACAPIDeployment(cfg *costv1alpha1.CostManagementServiceConfig) *appsv1.Deployment {
	spec := cfg.Spec.RBAC
	image := spec.Image.Repository + ":" + spec.Image.Tag
	replicas := spec.API.Replicas
	if replicas == 0 {
		replicas = 1
	}
	falseVal := false
	selLabels := SelectorLabels(cfg, "rbac-api")
	allLabels := Labels(cfg, "rbac-api")
	vols, mounts := rbacVolumesAndMounts(cfg)

	return &appsv1.Deployment{
		TypeMeta: metav1.TypeMeta{APIVersion: "apps/v1", Kind: "Deployment"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      NameRBACAPI(cfg),
			Namespace: cfg.Namespace,
			Labels:    allLabels,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{MatchLabels: selLabels},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: allLabels},
				Spec: corev1.PodSpec{
					AutomountServiceAccountToken: &falseVal,
					SecurityContext:              nonRootPodSC(),
					ImagePullSecrets:             imagePullSecrets(cfg),
					InitContainers: []corev1.Container{
						waitForRBACDB(cfg),
						WaitForValkeyInitContainer(cfg),
					},
					Containers: []corev1.Container{{
						Name:            "rbac-api",
						Image:           image,
						ImagePullPolicy: pullPolicy(cfg),
						WorkingDir:      "/opt/rbac/rbac",
						Command:         []string{"gunicorn"},
						Args: []string{
							"rbac.wsgi",
							"--bind=0.0.0.0:8080",
							"--workers=2",
							"--threads=2",
							"--timeout=120",
							"--access-logfile=-",
						},
						Ports: []corev1.ContainerPort{
							{Name: "http", ContainerPort: rbacAPIPort, Protocol: corev1.ProtocolTCP},
						},
						Env: rbacEnv(cfg),
						LivenessProbe: &corev1.Probe{
							ProbeHandler:        corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{Path: "/api/rbac/v1/status/", Port: intstr.FromString("http")}},
							InitialDelaySeconds: 30, PeriodSeconds: 20, TimeoutSeconds: 5, FailureThreshold: 5,
						},
						ReadinessProbe: &corev1.Probe{
							ProbeHandler:        corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{Path: "/api/rbac/v1/status/", Port: intstr.FromString("http")}},
							InitialDelaySeconds: 15, PeriodSeconds: 10, TimeoutSeconds: 5, FailureThreshold: 3,
						},
						Resources:       spec.API.Resources,
						VolumeMounts:    mounts,
						SecurityContext: rbacAppContainerSC(),
					}},
					Volumes: vols,
				},
			},
		},
	}
}

// RBACAPIService exposes the RBAC API inside the cluster.
func RBACAPIService(cfg *costv1alpha1.CostManagementServiceConfig) *corev1.Service {
	return &corev1.Service{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Service"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      NameRBACAPI(cfg),
			Namespace: cfg.Namespace,
			Labels:    Labels(cfg, "rbac-api"),
		},
		Spec: corev1.ServiceSpec{
			Selector: SelectorLabels(cfg, "rbac-api"),
			Ports:    []corev1.ServicePort{{Name: "http", Port: rbacAPIPort, Protocol: corev1.ProtocolTCP}},
		},
	}
}

// -----------------------------------------------------------------------------
// RBAC Worker
// -----------------------------------------------------------------------------

// RBACWorkerDeployment builds the RBAC Celery worker Deployment.
func RBACWorkerDeployment(cfg *costv1alpha1.CostManagementServiceConfig) *appsv1.Deployment {
	spec := cfg.Spec.RBAC
	image := spec.Image.Repository + ":" + spec.Image.Tag
	replicas := spec.Worker.Replicas
	if replicas == 0 {
		replicas = 1
	}
	falseVal := false
	selLabels := SelectorLabels(cfg, "rbac-worker")
	allLabels := Labels(cfg, "rbac-worker")
	vols, mounts := rbacVolumesAndMounts(cfg)

	return &appsv1.Deployment{
		TypeMeta: metav1.TypeMeta{APIVersion: "apps/v1", Kind: "Deployment"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      NameRBACWorker(cfg),
			Namespace: cfg.Namespace,
			Labels:    allLabels,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{MatchLabels: selLabels},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: allLabels},
				Spec: corev1.PodSpec{
					AutomountServiceAccountToken: &falseVal,
					SecurityContext:              nonRootPodSC(),
					ImagePullSecrets:             imagePullSecrets(cfg),
					InitContainers: []corev1.Container{
						waitForRBACDB(cfg),
						WaitForValkeyInitContainer(cfg),
					},
					Containers: []corev1.Container{{
						Name:            "rbac-worker",
						Image:           image,
						ImagePullPolicy: pullPolicy(cfg),
						WorkingDir:      "/opt/rbac/rbac",
						Command:         []string{"celery"},
						Args:            []string{"-A", "rbac.celery", "worker", "--pool=solo", "--loglevel=info"},
						Env:             rbacEnv(cfg),
						Resources:       spec.Worker.Resources,
						VolumeMounts:    mounts,
						SecurityContext: rbacAppContainerSC(),
					}},
					Volumes: vols,
				},
			},
		},
	}
}
