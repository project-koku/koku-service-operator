package resources

import (
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	costv1alpha1 "github.com/project-koku/koku-service-operator/api/v1alpha1"
)

const (
	rosDBName     = RosDBName
	rosAPIPort    = 8000
	rosMetricPort = 9000

	uploadTopic         = "hccm.ros.events"
	recommendationTopic = "rosocp.kruize.recommendations"
)

// NameROSServiceAccount returns the service account name for ROS pods.
func NameROSServiceAccount(cfg *costv1alpha1.CostManagementServiceConfig) string {
	sa := cfg.Spec.ROS.ServiceAccount
	if sa.Name != "" {
		return sa.Name
	}
	return cfg.Name + "-ros-backend"
}

// NameCdappConfigMap returns the name of the shared cdapp ConfigMap used by ROS.
func NameCdappConfigMap(cfg *costv1alpha1.CostManagementServiceConfig) string {
	return cfg.Name + "-cdapp-config"
}

// ROSServiceAccount builds the ServiceAccount for ROS pods.
func ROSServiceAccount(cfg *costv1alpha1.CostManagementServiceConfig) *corev1.ServiceAccount {
	return &corev1.ServiceAccount{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "ServiceAccount"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      NameROSServiceAccount(cfg),
			Namespace: cfg.Namespace,
			Labels:    Labels(cfg, "ros"),
		},
	}
}

// CdappConfigMap builds the shared Clowder-style config used by ROS Processor and Poller.
func CdappConfigMap(cfg *costv1alpha1.CostManagementServiceConfig) *corev1.ConfigMap {
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
    "port": %d,
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
  "publicPort": 8000,
  "webPort": 8000
}`,
		host, rosDBName, port, sslMode,
		KafkaHost(cfg), KafkaPort(cfg),
		uploadTopic, uploadTopic,
		recommendationTopic, recommendationTopic,
	)

	return &corev1.ConfigMap{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "ConfigMap"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      NameCdappConfigMap(cfg),
			Namespace: cfg.Namespace,
			Labels:    Labels(cfg, "ros"),
		},
		Data: map[string]string{"cdappconfig.json": json},
	}
}

// -----------------------------------------------------------------------------
// ROS shared helpers
// -----------------------------------------------------------------------------

// rosDBEnv returns the standard DB env vars that all ROS containers need.
func rosDBEnv(cfg *costv1alpha1.CostManagementServiceConfig) []corev1.EnvVar {
	dbSecret := NameDBCredentials(cfg)
	host := DatabaseHost(cfg)
	port := cfg.Spec.Database.Port
	if port == 0 {
		port = 5432
	}
	return []corev1.EnvVar{
		EnvVal("CLOWDER_ENABLED", "false"),
		EnvVal("DB_HOST", host),
		EnvVal("DB_PORT", int32String(port)),
		EnvVal("DB_NAME", rosDBName),
		EnvFromSecret("DB_USER", dbSecret, "ros-user"),
		EnvFromSecret("DB_PASSWORD", dbSecret, "ros-password"),
		// DATABASE_URL uses Kubernetes env var substitution for credentials.
		EnvVal("DATABASE_URL", fmt.Sprintf("postgresql://$(DB_USER):$(DB_PASSWORD)@%s:%s/%s",
			host, int32String(port), rosDBName)),
		EnvVal("KAFKA_BOOTSTRAP_SERVERS", cfg.Spec.Kafka.BootstrapServers),
	}
}

// rosKafkaEnv appends Kafka SASL/TLS env vars (reuses the Koku helpers).
func rosKafkaEnv(cfg *costv1alpha1.CostManagementServiceConfig, base []corev1.EnvVar) []corev1.EnvVar {
	if cfg.Spec.Kafka.SASL.Mechanism != "" {
		base = append(base, EnvVal("KAFKA_SASL_MECHANISM", cfg.Spec.Kafka.SASL.Mechanism))
		if cfg.Spec.Kafka.SASL.ExistingSecret != "" {
			base = append(base,
				EnvFromSecret("KAFKA_SASL_USERNAME", cfg.Spec.Kafka.SASL.ExistingSecret, "username"),
				EnvFromSecret("KAFKA_SASL_PASSWORD", cfg.Spec.Kafka.SASL.ExistingSecret, "password"),
			)
		}
	}
	if cfg.Spec.Kafka.SecurityProtocol != "" && cfg.Spec.Kafka.SecurityProtocol != "PLAINTEXT" {
		base = append(base, EnvVal("KAFKA_SECURITY_PROTOCOL", cfg.Spec.Kafka.SecurityProtocol))
	}
	return base
}

// waitForROSDB returns an init container that blocks until the database is
// ready to accept ROS connections.
func waitForROSDB(cfg *costv1alpha1.CostManagementServiceConfig) corev1.Container {
	host := DatabaseHost(cfg)
	port := cfg.Spec.Database.Port
	if port == 0 {
		port = 5432
	}
	return waitForPostgres(cfg, host, int32String(port))
}

// waitForKafka returns an init container that blocks until the Kafka broker is reachable.
func waitForKafka(cfg *costv1alpha1.CostManagementServiceConfig) corev1.Container {
	return waitForTCP("wait-for-kafka", KafkaHost(cfg), KafkaPort(cfg))
}

// waitForKruize returns an init container that polls Kruize's HTTP endpoint.
// curl is used instead of a raw TCP check because Kruize is only truly ready
// once the HTTP API responds — the port may be open before the JVM has
// finished initialisation.
func waitForKruize(cfg *costv1alpha1.CostManagementServiceConfig) corev1.Container {
	url := "http://" + NameKruize(cfg) + ":8080/listPerformanceProfiles"
	return waitForHTTP("wait-for-kruize", url)
}

// waitForKoku returns an init container that waits for the Koku API Service.
func waitForKoku(cfg *costv1alpha1.CostManagementServiceConfig) corev1.Container {
	return waitForTCP("wait-for-koku", NameKokuAPI(cfg), "8000")
}

// cdappVolumeAndMount returns the cdapp-config volume + mount pair used by processor/poller.
func cdappVolumeAndMount(cfg *costv1alpha1.CostManagementServiceConfig) (corev1.Volume, corev1.VolumeMount) {
	vol := corev1.Volume{
		Name: "cdapp-config",
		VolumeSource: corev1.VolumeSource{
			ConfigMap: &corev1.ConfigMapVolumeSource{
				LocalObjectReference: corev1.LocalObjectReference{Name: NameCdappConfigMap(cfg)},
			},
		},
	}
	mount := corev1.VolumeMount{Name: "cdapp-config", MountPath: "/cdapp", ReadOnly: true}
	return vol, mount
}

// kafkaTLSVolumeAndMount returns the Kafka TLS volume + mount (nil if not configured).
func kafkaTLSVolumeAndMount(cfg *costv1alpha1.CostManagementServiceConfig) (*corev1.Volume, *corev1.VolumeMount) {
	if !cfg.Spec.Kafka.TLS.Enabled || cfg.Spec.Kafka.TLS.CACertSecret == "" {
		return nil, nil
	}
	vol := corev1.Volume{
		Name: "kafka-ca-cert",
		VolumeSource: corev1.VolumeSource{
			Secret: &corev1.SecretVolumeSource{
				SecretName: cfg.Spec.Kafka.TLS.CACertSecret,
				Items:      []corev1.KeyToPath{{Key: "ca.crt", Path: "ca.crt"}},
			},
		},
	}
	mount := corev1.VolumeMount{Name: "kafka-ca-cert", MountPath: "/etc/kafka/certs", ReadOnly: true}
	return &vol, &mount
}

// -----------------------------------------------------------------------------
// ROS API
// -----------------------------------------------------------------------------

// ROSAPIDeployment builds the ROS API Deployment.
func ROSAPIDeployment(cfg *costv1alpha1.CostManagementServiceConfig) *appsv1.Deployment {
	image := cfg.Spec.ROS.Image.Repository + ":" + cfg.Spec.ROS.Image.Tag
	selLabels := SelectorLabels(cfg, "ros-api")
	allLabels := Labels(cfg, "ros-api")
	replicas := int32(1)
	falseVal := false

	env := rosDBEnv(cfg)
	env = rosKafkaEnv(cfg, env)
	env = append(env,
		EnvVal("PATH_PREFIX", "/api"),
		EnvVal("RBAC_ENABLE", "true"),
		EnvVal("RBACHOST", NameRBACAPI(cfg)),
		EnvVal("RBACPORT", "8080"),
		EnvVal("RBACPROTOCOL", "http"),
		EnvVal("DB_POOL_SIZE", "10"),
		EnvVal("DB_MAX_OVERFLOW", "20"),
		EnvVal("SERVICE_NAME", "ros-api"),
		EnvVal("LOG_LEVEL", cfg.Spec.ROS.API.LogLevel),
	)

	volumes, mounts := rosAPIVolumes(cfg)

	return &appsv1.Deployment{
		TypeMeta:   metav1.TypeMeta{APIVersion: "apps/v1", Kind: "Deployment"},
		ObjectMeta: metav1.ObjectMeta{Name: NameROSAPI(cfg), Namespace: cfg.Namespace, Labels: allLabels},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{MatchLabels: selLabels},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: allLabels},
				Spec: corev1.PodSpec{
					ServiceAccountName:           NameROSServiceAccount(cfg),
					AutomountServiceAccountToken: &falseVal,
					SecurityContext:              nonRootPodSC(),
					ImagePullSecrets:             imagePullSecrets(cfg),
					InitContainers: []corev1.Container{
						waitForROSDB(cfg),
						waitForKafka(cfg),
					},
					Containers: []corev1.Container{{
						Name:            "ros-api",
						Image:           image,
						ImagePullPolicy: pullPolicy(cfg),
						Command:         []string{"sh", "-c"},
						Args:            []string{"./rosocp start api"},
						Ports: []corev1.ContainerPort{
							{Name: "http", ContainerPort: rosAPIPort, Protocol: corev1.ProtocolTCP},
							{Name: "metrics", ContainerPort: rosMetricPort, Protocol: corev1.ProtocolTCP},
						},
						Env: env,
						LivenessProbe: &corev1.Probe{
							ProbeHandler:        corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{Path: "/status", Port: intstr.FromInt32(rosAPIPort)}},
							InitialDelaySeconds: 30, PeriodSeconds: 10, TimeoutSeconds: 5, FailureThreshold: 3,
						},
						ReadinessProbe: &corev1.Probe{
							ProbeHandler:        corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{Path: "/status", Port: intstr.FromInt32(rosAPIPort)}},
							InitialDelaySeconds: 15, PeriodSeconds: 10, TimeoutSeconds: 5, FailureThreshold: 3,
						},
						Resources:       cfg.Spec.ROS.API.Resources,
						VolumeMounts:    mounts,
						SecurityContext: restrictedContainerSC(),
					}},
					Volumes: volumes,
				},
			},
		},
	}
}

func rosAPIVolumes(cfg *costv1alpha1.CostManagementServiceConfig) ([]corev1.Volume, []corev1.VolumeMount) {
	var vols []corev1.Volume
	var mounts []corev1.VolumeMount
	if v, m := kafkaTLSVolumeAndMount(cfg); v != nil {
		vols = append(vols, *v)
		mounts = append(mounts, *m)
	}
	return vols, mounts
}

// ROSAPIService exposes the ROS API.
func ROSAPIService(cfg *costv1alpha1.CostManagementServiceConfig) *corev1.Service {
	return &corev1.Service{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "Service"},
		ObjectMeta: metav1.ObjectMeta{Name: NameROSAPI(cfg), Namespace: cfg.Namespace, Labels: Labels(cfg, "ros-api")},
		Spec: corev1.ServiceSpec{
			Selector: SelectorLabels(cfg, "ros-api"),
			Ports: []corev1.ServicePort{
				{Name: "http", Port: rosAPIPort, Protocol: corev1.ProtocolTCP},
				{Name: "metrics", Port: rosMetricPort, Protocol: corev1.ProtocolTCP},
			},
		},
	}
}

// -----------------------------------------------------------------------------
// ROS Processor
// -----------------------------------------------------------------------------

// ROSProcessorDeployment builds the ROS Processor Deployment.
func ROSProcessorDeployment(cfg *costv1alpha1.CostManagementServiceConfig) *appsv1.Deployment {
	image := cfg.Spec.ROS.Image.Repository + ":" + cfg.Spec.ROS.Image.Tag
	selLabels := SelectorLabels(cfg, "ros-processor")
	allLabels := Labels(cfg, "ros-processor")
	replicas := cfg.Spec.ROS.Processor.Replicas
	if replicas == 0 {
		replicas = 1
	}
	falseVal := false

	cdappVol, cdappMount := cdappVolumeAndMount(cfg)
	volumes := []corev1.Volume{
		cdappVol,
		// CA combine volumes (same ConfigMaps as KokuVolumes; mount path matches chart SSL_CERT_FILE).
		{
			Name: "ca-scripts",
			VolumeSource: corev1.VolumeSource{
				ConfigMap: &corev1.ConfigMapVolumeSource{
					LocalObjectReference: corev1.LocalObjectReference{Name: NameCACombineConfigMap(cfg)},
					Items: []corev1.KeyToPath{
						{Key: "combine-ca.sh", Path: "combine-ca.sh", Mode: int32Ptr(0755)},
					},
				},
			},
		},
		{
			Name: "ca-source",
			VolumeSource: corev1.VolumeSource{
				ConfigMap: &corev1.ConfigMapVolumeSource{
					LocalObjectReference: corev1.LocalObjectReference{Name: NameServiceCAConfigMap(cfg)},
				},
			},
		},
		{
			Name:         "combined-ca-bundle",
			VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
		},
	}
	mounts := []corev1.VolumeMount{
		cdappMount,
		{Name: "combined-ca-bundle", MountPath: "/etc/ssl/certs/ca-bundle", ReadOnly: true},
	}
	if v, m := kafkaTLSVolumeAndMount(cfg); v != nil {
		volumes = append(volumes, *v)
		mounts = append(mounts, *m)
	}

	env := rosDBEnv(cfg)
	env = rosKafkaEnv(cfg, env)
	env = append(env,
		EnvVal("KAFKA_CONSUMER_GROUP_ID", "ros-processor"),
		EnvVal("KAFKA_AUTO_COMMIT", "true"),
		EnvVal("UPLOAD_TOPIC", uploadTopic),
		EnvVal("KRUIZE_HOST", NameKruize(cfg)),
		EnvVal("KRUIZE_PORT", "8080"),
		EnvVal("KRUIZE_WAIT_TIME", "120"),
		EnvVal("SERVICE_NAME", "ros-processor"),
		EnvVal("LOG_LEVEL", cfg.Spec.ROS.Processor.LogLevel),
		EnvVal("PROMETHEUS_PORT", "9000"),
		EnvVal("SSL_CERT_FILE", "/etc/ssl/certs/ca-bundle/ca-bundle.crt"),
	)

	return &appsv1.Deployment{
		TypeMeta:   metav1.TypeMeta{APIVersion: "apps/v1", Kind: "Deployment"},
		ObjectMeta: metav1.ObjectMeta{Name: NameROSProcessor(cfg), Namespace: cfg.Namespace, Labels: allLabels},
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
						waitForROSDB(cfg),
						waitForKafka(cfg),
						waitForKruize(cfg),
						CACombineInitContainer(cfg),
					},
					Containers: []corev1.Container{{
						Name:            "ros-processor",
						Image:           image,
						ImagePullPolicy: pullPolicy(cfg),
						Command:         []string{"sh", "-c"},
						Args:            []string{"sleep 60 && ./rosocp start processor"},
						Ports:           []corev1.ContainerPort{{Name: "metrics", ContainerPort: rosMetricPort, Protocol: corev1.ProtocolTCP}},
						Env:             env,
						LivenessProbe: &corev1.Probe{
							ProbeHandler:        corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{Path: "/metrics", Port: intstr.FromInt32(rosMetricPort)}},
							InitialDelaySeconds: 120, PeriodSeconds: 30, TimeoutSeconds: 10, FailureThreshold: 5,
						},
						ReadinessProbe: &corev1.Probe{
							ProbeHandler:        corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{Path: "/metrics", Port: intstr.FromInt32(rosMetricPort)}},
							InitialDelaySeconds: 90, PeriodSeconds: 20, TimeoutSeconds: 10, FailureThreshold: 5,
						},
						Resources:       cfg.Spec.ROS.Processor.Resources,
						VolumeMounts:    mounts,
						SecurityContext: restrictedContainerSC(),
					}},
					Volumes: volumes,
				},
			},
		},
	}
}

// -----------------------------------------------------------------------------
// ROS Recommendation Poller
// -----------------------------------------------------------------------------

// ROSPollerDeployment builds the Recommendation Poller Deployment.
func ROSPollerDeployment(cfg *costv1alpha1.CostManagementServiceConfig) *appsv1.Deployment {
	image := cfg.Spec.ROS.Image.Repository + ":" + cfg.Spec.ROS.Image.Tag
	selLabels := SelectorLabels(cfg, "ros-recommendation-poller")
	allLabels := Labels(cfg, "ros-recommendation-poller")
	replicas := int32(1)
	falseVal := false

	cdappVol, cdappMount := cdappVolumeAndMount(cfg)
	volumes := []corev1.Volume{cdappVol}
	mounts := []corev1.VolumeMount{cdappMount}
	if v, m := kafkaTLSVolumeAndMount(cfg); v != nil {
		volumes = append(volumes, *v)
		mounts = append(mounts, *m)
	}

	env := rosDBEnv(cfg)
	env = rosKafkaEnv(cfg, env)
	env = append(env,
		EnvVal("KAFKA_CONSUMER_GROUP_ID", "ros-recommendation-poller"),
		EnvVal("KAFKA_AUTO_COMMIT", "false"),
		EnvVal("RECOMMENDATION_TOPIC", recommendationTopic),
		EnvVal("KRUIZE_HOST", NameKruize(cfg)),
		EnvVal("KRUIZE_PORT", "8080"),
		EnvVal("KRUIZE_WAIT_TIME", "120"),
		EnvVal("SERVICE_NAME", "ros-recommendation-poller"),
		EnvVal("LOG_LEVEL", cfg.Spec.ROS.RecommendationPoller.LogLevel),
		EnvVal("PROMETHEUS_PORT", "9000"),
	)

	return &appsv1.Deployment{
		TypeMeta:   metav1.TypeMeta{APIVersion: "apps/v1", Kind: "Deployment"},
		ObjectMeta: metav1.ObjectMeta{Name: NameROSPoller(cfg), Namespace: cfg.Namespace, Labels: allLabels},
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
						waitForROSDB(cfg),
						waitForKafka(cfg),
						waitForKruize(cfg),
					},
					Containers: []corev1.Container{{
						Name:            "ros-rec-poller",
						Image:           image,
						ImagePullPolicy: pullPolicy(cfg),
						Command:         []string{"sh", "-c"},
						Args:            []string{"sleep 60 && ./rosocp start recommendation-poller"},
						Ports:           []corev1.ContainerPort{{Name: "metrics", ContainerPort: rosMetricPort, Protocol: corev1.ProtocolTCP}},
						Env:             env,
						LivenessProbe: &corev1.Probe{
							ProbeHandler:        corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{Path: "/metrics", Port: intstr.FromInt32(rosMetricPort)}},
							InitialDelaySeconds: 120, PeriodSeconds: 30, TimeoutSeconds: 10, FailureThreshold: 5,
						},
						ReadinessProbe: &corev1.Probe{
							ProbeHandler:        corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{Path: "/metrics", Port: intstr.FromInt32(rosMetricPort)}},
							InitialDelaySeconds: 90, PeriodSeconds: 20, TimeoutSeconds: 10, FailureThreshold: 5,
						},
						Resources:       cfg.Spec.ROS.RecommendationPoller.Resources,
						VolumeMounts:    mounts,
						SecurityContext: restrictedContainerSC(),
					}},
					Volumes: volumes,
				},
			},
		},
	}
}

// -----------------------------------------------------------------------------
// ROS Housekeeper
// -----------------------------------------------------------------------------

// ROSHousekeeperDeployment builds the Housekeeper Deployment.
func ROSHousekeeperDeployment(cfg *costv1alpha1.CostManagementServiceConfig) *appsv1.Deployment {
	image := cfg.Spec.ROS.Image.Repository + ":" + cfg.Spec.ROS.Image.Tag
	selLabels := SelectorLabels(cfg, "ros-housekeeper")
	allLabels := Labels(cfg, "ros-housekeeper")
	replicas := int32(1)
	falseVal := false

	var volumes []corev1.Volume
	var mounts []corev1.VolumeMount
	if v, m := kafkaTLSVolumeAndMount(cfg); v != nil {
		volumes = append(volumes, *v)
		mounts = append(mounts, *m)
	}

	env := rosDBEnv(cfg)
	env = rosKafkaEnv(cfg, env)
	env = append(env,
		EnvVal("SOURCES_API_BASE_URL", fmt.Sprintf("http://%s:8000", NameKokuAPI(cfg))),
		EnvVal("SOURCES_API_PREFIX", "/api/cost-management/v1"),
		EnvVal("SOURCES_EVENT_TOPIC", "platform.sources.event-stream"),
		EnvVal("KRUIZE_HOST", NameKruize(cfg)),
		EnvVal("KRUIZE_PORT", "8080"),
		EnvVal("SERVICE_NAME", "ros-housekeeper-sources"),
		EnvVal("LOG_LEVEL", "INFO"),
	)

	return &appsv1.Deployment{
		TypeMeta:   metav1.TypeMeta{APIVersion: "apps/v1", Kind: "Deployment"},
		ObjectMeta: metav1.ObjectMeta{Name: NameROSHousekeeper(cfg), Namespace: cfg.Namespace, Labels: allLabels},
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
						waitForROSDB(cfg),
						waitForKafka(cfg),
						waitForKruize(cfg),
						waitForKoku(cfg),
					},
					Containers: []corev1.Container{{
						Name:            "ros-housekeeper",
						Image:           image,
						ImagePullPolicy: pullPolicy(cfg),
						Command:         []string{"sh", "-c"},
						Args:            []string{"sleep 60 && ./rosocp start housekeeper --sources"},
						Env:             env,
						Resources:       cfg.Spec.ROS.Housekeeper.Resources,
						VolumeMounts:    mounts,
						SecurityContext: restrictedContainerSC(),
					}},
					Volumes: volumes,
				},
			},
		},
	}
}

// -----------------------------------------------------------------------------
// ROS Partition Cleaner CronJob
// -----------------------------------------------------------------------------

// ROSPartitionCleanerCronJob builds the partition cleaner CronJob.
func ROSPartitionCleanerCronJob(cfg *costv1alpha1.CostManagementServiceConfig) *batchv1.CronJob {
	image := cfg.Spec.ROS.Image.Repository + ":" + cfg.Spec.ROS.Image.Tag
	pc := cfg.Spec.ROS.Housekeeper.PartitionCleaner
	schedule := pc.Schedule
	if schedule == "" {
		schedule = "0 0 */15 * *"
	}
	onFailure := corev1.RestartPolicyOnFailure

	env := rosDBEnv(cfg)
	env = append(env,
		EnvVal("SERVICE_NAME", "ros-housekeeper-partition"),
		EnvVal("LOG_LEVEL", "INFO"),
	)

	return &batchv1.CronJob{
		TypeMeta: metav1.TypeMeta{APIVersion: "batch/v1", Kind: "CronJob"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      cfg.Name + "-ros-partition-cleaner",
			Namespace: cfg.Namespace,
			Labels:    Labels(cfg, "ros-database-maintenance"),
		},
		Spec: batchv1.CronJobSpec{
			Schedule: schedule,
			JobTemplate: batchv1.JobTemplateSpec{
				Spec: batchv1.JobSpec{
					Template: corev1.PodTemplateSpec{
						ObjectMeta: metav1.ObjectMeta{Labels: Labels(cfg, "ros-database-maintenance")},
						Spec: corev1.PodSpec{
							ServiceAccountName: NameROSServiceAccount(cfg),
							RestartPolicy:      onFailure,
							SecurityContext:    nonRootPodSC(),
							ImagePullSecrets:   imagePullSecrets(cfg),
							Containers: []corev1.Container{{
								Name:            "ros-partition-cleaner",
								Image:           image,
								ImagePullPolicy: pullPolicy(cfg),
								Command:         []string{"sh", "-c"},
								Args:            []string{"./rosocp start housekeeper --partitions"},
								Env:             env,
								Resources:       pc.Resources,
								SecurityContext: restrictedContainerSC(),
							}},
						},
					},
				},
			},
		},
	}
}
