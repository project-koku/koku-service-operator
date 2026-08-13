package resources

import (
	"crypto/sha256"
	"fmt"
	"strconv"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	costv1alpha1 "github.com/project-koku/koku-service-operator/api/v1alpha1"
)

const (
	kruizeDBName = KruizeDBName
	kruizePort   = 8080
)

// NameKruizeConfigMap returns the Kruize cdappconfig ConfigMap name.
func NameKruizeConfigMap(cfg *costv1alpha1.CostManagementServiceConfig) string {
	return cfg.Name + "-kruize-config"
}

// NameKruizeClusterRole returns the ClusterRole name for Kruize.
// Format: "{crName}-kruize-{nsHash8}" where nsHash8 is the first 4 bytes
// (8 hex chars) of sha256(namespace). This keeps the CR name readable in
// `kubectl get clusterrole` while ensuring uniqueness across namespaces
// and staying well under the 253-char Kubernetes name limit.
func NameKruizeClusterRole(cfg *costv1alpha1.CostManagementServiceConfig) string {
	h := sha256.Sum256([]byte(cfg.Namespace))
	return fmt.Sprintf("%s-kruize-%x", cfg.Name, h[:4])
}

// NameKruizeServiceAccount returns the Kruize service account name.
func NameKruizeServiceAccount(cfg *costv1alpha1.CostManagementServiceConfig) string {
	return cfg.Name + "-kruize"
}

// KruizeServiceAccount builds the Kruize ServiceAccount.
func KruizeServiceAccount(cfg *costv1alpha1.CostManagementServiceConfig) *corev1.ServiceAccount {
	return &corev1.ServiceAccount{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "ServiceAccount"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      NameKruizeServiceAccount(cfg),
			Namespace: cfg.Namespace,
			Labels:    Labels(cfg, "ros-optimization"),
		},
	}
}

// KruizeClusterRole builds the ClusterRole that Kruize needs to watch workloads.
func KruizeClusterRole(cfg *costv1alpha1.CostManagementServiceConfig) *rbacv1.ClusterRole {
	return &rbacv1.ClusterRole{
		TypeMeta: metav1.TypeMeta{APIVersion: "rbac.authorization.k8s.io/v1", Kind: "ClusterRole"},
		ObjectMeta: metav1.ObjectMeta{
			Name:   NameKruizeClusterRole(cfg),
			Labels: Labels(cfg, "ros-optimization"),
		},
		Rules: []rbacv1.PolicyRule{
			{APIGroups: []string{""}, Resources: []string{"pods", "services", "configmaps", "nodes", "endpoints"}, Verbs: []string{"get", "list", "watch"}},
			{APIGroups: []string{"apps"}, Resources: []string{"deployments", "replicasets", "statefulsets", "daemonsets"}, Verbs: []string{"get", "list", "watch"}},
			{APIGroups: []string{"metrics.k8s.io"}, Resources: []string{"nodes", "pods"}, Verbs: []string{"get", "list"}},
			{APIGroups: []string{"custom.metrics.k8s.io"}, Resources: []string{"*"}, Verbs: []string{"get", "list"}},
		},
	}
}

// KruizeClusterRoleBinding binds the Kruize ClusterRole to its ServiceAccount.
func KruizeClusterRoleBinding(cfg *costv1alpha1.CostManagementServiceConfig) *rbacv1.ClusterRoleBinding {
	return &rbacv1.ClusterRoleBinding{
		TypeMeta: metav1.TypeMeta{APIVersion: "rbac.authorization.k8s.io/v1", Kind: "ClusterRoleBinding"},
		ObjectMeta: metav1.ObjectMeta{
			Name:   NameKruizeClusterRole(cfg),
			Labels: Labels(cfg, "ros-optimization"),
		},
		Subjects: []rbacv1.Subject{{
			Kind:      "ServiceAccount",
			Name:      NameKruizeServiceAccount(cfg),
			Namespace: cfg.Namespace,
		}},
		RoleRef: rbacv1.RoleRef{
			APIGroup: "rbac.authorization.k8s.io",
			Kind:     "ClusterRole",
			Name:     NameKruizeClusterRole(cfg),
		},
	}
}

// KruizeConfigMap builds the cdappconfig.json ConfigMap that Kruize mounts at /tmp/.
func KruizeConfigMap(cfg *costv1alpha1.CostManagementServiceConfig) *corev1.ConfigMap {
	host := DatabaseHost(cfg)
	port := cfg.Spec.Database.Port
	if port == 0 {
		port = 5432
	}
	sslMode := cfg.Spec.Database.SSLMode
	if sslMode == "" {
		sslMode = "disable"
	}

	json := fmt.Sprintf(`{
  "database": {
    "hostname": %q,
    "name": %q,
    "port": %s,
    "sslMode": %q
  },
  "kafka": {
    "brokers": [{"hostname": %q, "port": %s}],
    "topics": [
      {"name": %q, "requestedName": %q},
      {"name": %q, "requestedName": %q}
    ]
  },
  "logging": {"type": "null"},
  "metricsPath": "/metrics",
  "metricsPort": 9000,
  "privatePort": 10000,
  "publicPort": %d,
  "webPort": %d
}`,
		host, kruizeDBName, strconv.Itoa(int(port)), sslMode,
		KafkaHost(cfg), KafkaPort(cfg),
		uploadTopic, uploadTopic,
		recommendationTopic, recommendationTopic,
		kruizePort, kruizePort,
	)

	return &corev1.ConfigMap{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "ConfigMap"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      NameKruizeConfigMap(cfg),
			Namespace: cfg.Namespace,
			Labels:    Labels(cfg, "ros-optimization"),
		},
		Data: map[string]string{"cdappconfig.json": json},
	}
}

// kruizeEnv returns the standard env vars for Kruize containers (main + init).
func kruizeEnv(cfg *costv1alpha1.CostManagementServiceConfig, isInit bool) []corev1.EnvVar {
	dbSecret := NameDBCredentials(cfg)
	env := []corev1.EnvVar{
		EnvVal("DB_CONFIG_FILE", "/tmp/cdappconfig.json"),
		EnvVal("dbdriver", "jdbc:postgresql://"),
		EnvVal("database_name", kruizeDBName),
		EnvVal("clustertype", "kubernetes"),
		EnvVal("k8stype", "openshift"),
		EnvVal("authtype", ""),
		EnvVal("monitoringagent", "prometheus"),
		EnvVal("monitoringservice", "prometheus"),
		EnvVal("monitoringendpoint", "prometheus"),
		EnvVal("savetodb", "true"),
		EnvVal("local", "true"),
		EnvVal("hibernate_dialect", "org.hibernate.dialect.PostgreSQLDialect"),
		EnvVal("hibernate_driver", "org.postgresql.Driver"),
		EnvVal("hibernate_c3p0minsize", "2"),
		EnvVal("hibernate_c3p0maxsize", "5"),
		EnvVal("hibernate_c3p0timeout", "300"),
		EnvVal("hibernate_c3p0maxstatements", "100"),
		EnvVal("hibernate_hbm2ddlauto", "none"),
		EnvVal("hibernate_showsql", "false"),
		EnvVal("hibernate_timezone", "UTC"),
		EnvFromSecret("database_username", dbSecret, "kruize-user"),
		EnvFromSecret("database_password", dbSecret, "kruize-password"),
	}
	if isInit {
		// Partition init also needs admin credentials.
		env = append(env,
			EnvFromSecret("database_adminusername", dbSecret, "postgres-user"),
			EnvFromSecret("database_adminpassword", dbSecret, "postgres-password"),
			EnvVal("START_AUTOTUNE", "false"),
		)
	} else {
		env = append(env,
			EnvVal("LOG_ALL_HTTP_REQ_AND_RESPONSE", "true"),
			EnvVal("plots", "true"),
		)
	}
	return env
}

// kruizeConfigVolumeAndMount returns the kruize-config volume + subPath mount pair.
func kruizeConfigVolumeAndMount(cfg *costv1alpha1.CostManagementServiceConfig) (corev1.Volume, corev1.VolumeMount) {
	vol := corev1.Volume{
		Name: "kruize-config",
		VolumeSource: corev1.VolumeSource{
			ConfigMap: &corev1.ConfigMapVolumeSource{
				LocalObjectReference: corev1.LocalObjectReference{Name: NameKruizeConfigMap(cfg)},
			},
		},
	}
	mount := corev1.VolumeMount{
		Name:      "kruize-config",
		MountPath: "/tmp/cdappconfig.json",
		SubPath:   "cdappconfig.json",
		ReadOnly:  true,
	}
	return vol, mount
}

// KruizeDeployment builds the Kruize optimization engine Deployment.
func KruizeDeployment(cfg *costv1alpha1.CostManagementServiceConfig) *appsv1.Deployment {
	spec := cfg.Spec.Kruize
	image := spec.Image.Repository + ":" + spec.Image.Tag
	selLabels := SelectorLabels(cfg, "ros-optimization")
	allLabels := Labels(cfg, "ros-optimization")
	replicas := spec.Replicas
	if replicas == 0 {
		replicas = 1
	}

	cfgVol, cfgMount := kruizeConfigVolumeAndMount(cfg)
	initEnv := kruizeEnv(cfg, true)
	initEnv = append(initEnv,
		EnvVal("LOGGING_LEVEL", "info"),
		EnvVal("ROOT_LOGGING_LEVEL", "error"),
	)

	mainEnv := kruizeEnv(cfg, false)
	mainEnv = append(mainEnv,
		EnvVal("LOGGING_LEVEL", "debug"),
		EnvVal("ROOT_LOGGING_LEVEL", "error"),
	)

	// Partition creation init container (Java binary inside the Kruize image).
	createPartitionsInit := corev1.Container{
		Name:            "create-kruize-partitions",
		Image:           image,
		ImagePullPolicy: pullPolicy(cfg),
		Command:         []string{"sh"},
		Args:            []string{"-c", "export DB_CONFIG_FILE=/tmp/cdappconfig.json && /home/autotune/app/target/bin/CreatePartition"},
		Env:             initEnv,
		VolumeMounts:    []corev1.VolumeMount{cfgMount},
		Resources:       spec.Partitions.Resources,
		SecurityContext: restrictedContainerSC(),
	}

	initContainers := []corev1.Container{waitForROSDB(cfg)}
	if costv1alpha1.BoolVal(spec.Partitions.CreateEnabled, true) {
		initContainers = append(initContainers, createPartitionsInit)
	}

	return &appsv1.Deployment{
		TypeMeta:   metav1.TypeMeta{APIVersion: "apps/v1", Kind: "Deployment"},
		ObjectMeta: metav1.ObjectMeta{Name: NameKruize(cfg), Namespace: cfg.Namespace, Labels: allLabels},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{MatchLabels: selLabels},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: allLabels},
				Spec: corev1.PodSpec{
					ServiceAccountName: NameKruizeServiceAccount(cfg),
					SecurityContext:    nonRootPodSC(),
					ImagePullSecrets:   imagePullSecrets(cfg),
					InitContainers:     initContainers,
					Containers: []corev1.Container{{
						Name:            "kruize",
						Image:           image,
						ImagePullPolicy: pullPolicy(cfg),
						Ports: []corev1.ContainerPort{
							{Name: "http", ContainerPort: kruizePort, Protocol: corev1.ProtocolTCP},
						},
						Env: mainEnv,
						LivenessProbe: &corev1.Probe{
							ProbeHandler:        corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{Path: "/listPerformanceProfiles", Port: intstr.FromInt32(kruizePort)}},
							InitialDelaySeconds: 60, PeriodSeconds: 10, TimeoutSeconds: 5, FailureThreshold: 3,
						},
						ReadinessProbe: &corev1.Probe{
							ProbeHandler:        corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{Path: "/listPerformanceProfiles", Port: intstr.FromInt32(kruizePort)}},
							InitialDelaySeconds: 30, PeriodSeconds: 10, TimeoutSeconds: 5, FailureThreshold: 3,
						},
						Resources:       spec.Resources,
						VolumeMounts:    []corev1.VolumeMount{cfgMount},
						SecurityContext: restrictedContainerSC(),
					}},
					Volumes: []corev1.Volume{cfgVol},
				},
			},
		},
	}
}

// KruizeService exposes Kruize internally.
func KruizeService(cfg *costv1alpha1.CostManagementServiceConfig) *corev1.Service {
	return &corev1.Service{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "Service"},
		ObjectMeta: metav1.ObjectMeta{Name: NameKruize(cfg), Namespace: cfg.Namespace, Labels: Labels(cfg, "ros-optimization")},
		Spec: corev1.ServiceSpec{
			Selector: SelectorLabels(cfg, "ros-optimization"),
			Ports:    []corev1.ServicePort{{Name: "http", Port: kruizePort, Protocol: corev1.ProtocolTCP}},
		},
	}
}

// KruizeDeletePartitionsCronJob builds the CronJob that purges old Kruize partitions.
func KruizeDeletePartitionsCronJob(cfg *costv1alpha1.CostManagementServiceConfig) *batchv1.CronJob {
	spec := cfg.Spec.Kruize
	image := spec.Image.Repository + ":" + spec.Image.Tag
	schedule := spec.Partitions.DeleteSchedule
	if schedule == "" {
		schedule = "0 0 * * *"
	}
	threshold := spec.Partitions.DeletePartitionsThreshold
	if threshold == "" {
		threshold = "16"
	}
	concurrencyForbid := batchv1.ForbidConcurrent
	onFailure := corev1.RestartPolicyOnFailure

	cfgVol, cfgMount := kruizeConfigVolumeAndMount(cfg)

	env := kruizeEnv(cfg, true)
	env = append(env,
		EnvVal("LOGGING_LEVEL", "info"),
		EnvVal("ROOT_LOGGING_LEVEL", "error"),
		EnvVal("deletepartitionsthreshold", threshold),
	)

	return &batchv1.CronJob{
		TypeMeta: metav1.TypeMeta{APIVersion: "batch/v1", Kind: "CronJob"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      cfg.Name + "-kruize-delete-partitions",
			Namespace: cfg.Namespace,
			Labels:    Labels(cfg, "ros-optimization-maintenance"),
		},
		Spec: batchv1.CronJobSpec{
			Schedule:          schedule,
			ConcurrencyPolicy: concurrencyForbid,
			JobTemplate: batchv1.JobTemplateSpec{
				Spec: batchv1.JobSpec{
					Template: corev1.PodTemplateSpec{
						ObjectMeta: metav1.ObjectMeta{Labels: Labels(cfg, "ros-optimization-maintenance")},
						Spec: corev1.PodSpec{
							RestartPolicy:    onFailure,
							SecurityContext:  nonRootPodSC(),
							ImagePullSecrets: imagePullSecrets(cfg),
							Containers: []corev1.Container{{
								Name:            "delete-kruize-partitions",
								Image:           image,
								ImagePullPolicy: pullPolicy(cfg),
								Command:         []string{"sh"},
								Args:            []string{"-c", "export DB_CONFIG_FILE=/tmp/cdappconfig.json && /home/autotune/app/target/bin/RetentionPartition"},
								Env:             env,
								Resources:       spec.Partitions.Resources,
								VolumeMounts:    []corev1.VolumeMount{cfgMount},
								SecurityContext: restrictedContainerSC(),
							}},
							Volumes: []corev1.Volume{cfgVol},
						},
					},
				},
			},
		},
	}
}
