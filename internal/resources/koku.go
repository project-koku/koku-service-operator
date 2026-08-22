package resources

import (
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	costv1alpha1 "github.com/project-koku/koku-service-operator/api/v1alpha1"
)

// -----------------------------------------------------------------------------
// Koku API
// -----------------------------------------------------------------------------

// KokuAPIDeployment builds the Koku API Deployment.
func KokuAPIDeployment(cfg *costv1alpha1.CostManagementServiceConfig) *appsv1.Deployment {
	spec := cfg.Spec.CostManagement.API
	image, _ := KokuImage(cfg)
	replicas := spec.Replicas
	if replicas == 0 {
		replicas = 1
	}

	const containerName = "koku-api"

	env := KokuCommonEnv(cfg)
	env = append(env,
		EnvVal("API_PATH_PREFIX", "/api/cost-management"),
		EnvVal("MASU", "false"),
		// Chart defaults: without an explicit GUNICORN_WORKERS, gunicorn uses
		// POD_CPU_LIMIT*2+1. With no container CPU limit, OpenShift exposes the
		// node allocatable as POD_CPU_LIMIT (often 4+), spawning too many workers
		// and starving the API under concurrent load (gateway 503s).
		EnvVal("GUNICORN_WORKERS", "2"),
		EnvVal("GUNICORN_THREADS", "4"),
		EnvVal("PROMETHEUS_MULTIPROC_DIR", "/tmp"),
		EnvFromFieldRef("POD_CPU_LIMIT", containerName, "limits.cpu"),
	)
	env = MergeEnv(env, spec.Env)

	d := deploymentWithContainerName(cfg, NameKokuAPI(cfg), "cost-management-api", containerName,
		image, replicas, spec.Resources,
		kokuAPIProbe("/livez"), kokuAPIProbe("/readyz"), env,
		[]string{"/bin/bash", "-c", "cd $APP_HOME && exec gunicorn -c gunicorn_conf.py --max-requests=1000 koku.wsgi"},
	)
	// gunicorn serves the API on 8000 and Prometheus /metrics on 9000.
	d.Spec.Template.Spec.Containers[0].Ports = []corev1.ContainerPort{
		{Name: "http", ContainerPort: 8000, Protocol: corev1.ProtocolTCP},
		{Name: "metrics", ContainerPort: 9000, Protocol: corev1.ProtocolTCP},
	}
	return d
}

// KokuAPIService exposes the Koku API (http) and Prometheus metrics.
func KokuAPIService(cfg *costv1alpha1.CostManagementServiceConfig) *corev1.Service {
	return &corev1.Service{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Service"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      NameKokuAPI(cfg),
			Namespace: cfg.Namespace,
			Labels:    Labels(cfg, "cost-management-api"),
		},
		Spec: corev1.ServiceSpec{
			Selector: SelectorLabels(cfg, "cost-management-api"),
			Ports: []corev1.ServicePort{
				{Name: "http", Port: 8000, TargetPort: intstr.FromString("http"), Protocol: corev1.ProtocolTCP},
				{Name: "metrics", Port: 9000, TargetPort: intstr.FromString("metrics"), Protocol: corev1.ProtocolTCP},
			},
		},
	}
}

// -----------------------------------------------------------------------------
// Masu
// -----------------------------------------------------------------------------

// MasuDeployment builds the Masu data processor Deployment.
func MasuDeployment(cfg *costv1alpha1.CostManagementServiceConfig) *appsv1.Deployment {
	spec := cfg.Spec.CostManagement.Masu
	image, ok := ImageRef(spec.Image)
	if !ok {
		image, _ = KokuImage(cfg)
	}
	replicas := spec.Replicas
	if replicas == 0 {
		replicas = 1
	}

	const containerName = "masu"

	env := KokuCommonEnv(cfg)
	env = append(env,
		EnvVal("MASU", "true"),
		EnvVal("API_PATH_PREFIX", "/api/cost-management"),
		EnvVal("KAFKA_CONNECT", "true"),
		EnvVal("GUNICORN_WORKERS", "2"),
		EnvVal("PROMETHEUS_MULTIPROC_DIR", "/tmp"),
		EnvFromFieldRef("POD_CPU_LIMIT", containerName, "limits.cpu"),
	)
	env = MergeEnv(env, spec.Env)

	d := deploymentWithContainerName(cfg, NameMasu(cfg), "cost-processor", containerName,
		image, replicas, spec.Resources,
		masuProbe("/livez"), masuProbe("/readyz"), env,
		[]string{"/bin/bash", "-c", "cd $APP_HOME && exec gunicorn -c gunicorn_conf.py --max-requests=1000 koku.wsgi"},
	)
	// Same dual-port shape as Koku API: gunicorn HTTP + Prometheus /metrics.
	d.Spec.Template.Spec.Containers[0].Ports = []corev1.ContainerPort{
		{Name: "http", ContainerPort: 8000, Protocol: corev1.ProtocolTCP},
		{Name: "metrics", ContainerPort: 9000, Protocol: corev1.ProtocolTCP},
	}
	return d
}

// MasuService exposes Masu internally with HTTP (8000) and metrics (9000) ports.
// The metrics port is named "metrics" so the App ServiceMonitor can scrape /metrics.
func MasuService(cfg *costv1alpha1.CostManagementServiceConfig) *corev1.Service {
	return &corev1.Service{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Service"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      NameMasu(cfg),
			Namespace: cfg.Namespace,
			Labels:    Labels(cfg, "cost-processor"),
		},
		Spec: corev1.ServiceSpec{
			Selector: SelectorLabels(cfg, "cost-processor"),
			Ports: []corev1.ServicePort{
				{Name: "http", Port: 8000, TargetPort: intstr.FromString("http"), Protocol: corev1.ProtocolTCP},
				{Name: "metrics", Port: 9000, TargetPort: intstr.FromString("metrics"), Protocol: corev1.ProtocolTCP},
			},
		},
	}
}

// -----------------------------------------------------------------------------
// Kafka Listener
// -----------------------------------------------------------------------------

// ListenerDeployment builds the Kafka Listener Deployment.
func ListenerDeployment(cfg *costv1alpha1.CostManagementServiceConfig) *appsv1.Deployment {
	spec := cfg.Spec.CostManagement.Listener
	image, _ := KokuImage(cfg)
	replicas := spec.Replicas
	if replicas == 0 {
		replicas = 2
	}

	env := KokuCommonEnv(cfg)
	env = append(env,
		EnvVal("LISTENER_TOPIC", "platform.upload.announce"),
		EnvVal("KAFKA_CONNECT", "true"),
	)
	env = MergeEnv(env, spec.Env)

	return deployment(cfg, NameListener(cfg), "listener", image, replicas, spec.Resources,
		nil, nil, env,
		[]string{"/bin/bash", "-c", "cd $APP_HOME && exec python manage.py listener"},
	)
}

// -----------------------------------------------------------------------------
// Celery Beat
// -----------------------------------------------------------------------------

// CeleryBeatDeployment builds the Celery Beat scheduler Deployment.
func CeleryBeatDeployment(cfg *costv1alpha1.CostManagementServiceConfig) *appsv1.Deployment {
	image, _ := KokuImage(cfg)
	replicas := int32(1)

	env := KokuCommonEnv(cfg)
	env = append(env, EnvVal("CELERY_LOG_LEVEL", "info"))

	resources := corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("50m"),
			corev1.ResourceMemory: resource.MustParse("200Mi"),
		},
		Limits: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("100m"),
			corev1.ResourceMemory: resource.MustParse("400Mi"),
		},
	}

	return deployment(cfg, NameCeleryBeat(cfg), "cost-scheduler", image, replicas,
		resources, nil, nil, env,
		[]string{"/bin/sh", "-c", "cd $APP_HOME && PYTHONPATH=$APP_HOME celery -A koku beat -l info"},
	)
}

// -----------------------------------------------------------------------------
// Celery Workers
// -----------------------------------------------------------------------------

// CeleryWorkerDeployment builds a Celery worker Deployment for the given queue.
func CeleryWorkerDeployment(cfg *costv1alpha1.CostManagementServiceConfig, queue string, spec costv1alpha1.CeleryWorkerSpec) *appsv1.Deployment {
	image, _ := KokuImage(cfg)
	replicas := spec.Replicas
	concurrency := spec.Concurrency
	if concurrency == 0 {
		concurrency = 5
	}

	// Keep the Celery queue name (may include '_') for -Q / WORKER_QUEUES, but
	// sanitize for Deployment metadata.name and container name (RFC 1123).
	component := "cost-worker-" + DNS1123Label(queue)
	env := KokuCommonEnv(cfg)
	env = append(env,
		EnvVal("CELERY_LOG_LEVEL", "info"),
		EnvVal("WORKER_QUEUES", queue),
		EnvVal("CELERY_WORKER_CONCURRENCY", int32String(concurrency)),
	)

	return deployment(cfg, NameCeleryWorker(cfg, queue), component, image, replicas, spec.Resources,
		nil, nil, env,
		[]string{
			"/bin/sh", "-c",
			"cd $APP_HOME && PYTHONPATH=$APP_HOME celery -A koku worker --without-gossip -E -l ${CELERY_LOG_LEVEL:-info} -Q ${WORKER_QUEUES}",
		},
	)
}

// CeleryWorkerDeployments returns all Celery worker Deployments.
// Workers with replicas == 0 are still returned (with 0 replicas) so SSA can
// manage their lifecycle; Kubernetes simply schedules no pods for them.
func CeleryWorkerDeployments(cfg *costv1alpha1.CostManagementServiceConfig) []*appsv1.Deployment {
	w := cfg.Spec.CostManagement.Celery.Workers
	return []*appsv1.Deployment{
		CeleryWorkerDeployment(cfg, "celery", w.Default),
		CeleryWorkerDeployment(cfg, "priority", w.Priority),
		CeleryWorkerDeployment(cfg, "summary", w.Summary),
		CeleryWorkerDeployment(cfg, "ocp", w.OCP),
		CeleryWorkerDeployment(cfg, "cost_model", w.CostModel),
		CeleryWorkerDeployment(cfg, "refresh", w.Refresh),
		CeleryWorkerDeployment(cfg, "hcs", w.HCS.CeleryWorkerSpec()),
		CeleryWorkerDeployment(cfg, "download", w.Download),
		CeleryWorkerDeployment(cfg, "subs_extraction", w.SubsExtraction.CeleryWorkerSpec()),
		CeleryWorkerDeployment(cfg, "subs_transmission", w.SubsTransmission.CeleryWorkerSpec()),
	}
}

// KokuServiceAccount builds the ServiceAccount used by all Koku pods.
func KokuServiceAccount(cfg *costv1alpha1.CostManagementServiceConfig) *corev1.ServiceAccount {
	return &corev1.ServiceAccount{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "ServiceAccount"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      NameKokuServiceAccount(cfg),
			Namespace: cfg.Namespace,
			Labels:    Labels(cfg, "cost-management"),
		},
		AutomountServiceAccountToken: boolPtr(false),
	}
}

// -----------------------------------------------------------------------------
// Shared helpers
// -----------------------------------------------------------------------------

// deploymentWithContainerName is like deployment() but allows the container
// name to differ from the label component value (needed when env resourceFieldRef
// must reference a specific container name, e.g. "koku-api" vs "cost-management-api").
func deploymentWithContainerName(
	cfg *costv1alpha1.CostManagementServiceConfig,
	name, component, containerName, image string,
	replicas int32,
	resources corev1.ResourceRequirements,
	liveness, readiness *corev1.Probe,
	env []corev1.EnvVar,
	command []string,
) *appsv1.Deployment {
	selLabels := SelectorLabels(cfg, component)
	allLabels := Labels(cfg, component)
	falseVal := false
	return &appsv1.Deployment{
		TypeMeta:   metav1.TypeMeta{APIVersion: "apps/v1", Kind: "Deployment"},
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: cfg.Namespace, Labels: allLabels},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{MatchLabels: selLabels},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: allLabels},
				Spec: corev1.PodSpec{
					ServiceAccountName:           NameKokuServiceAccount(cfg),
					AutomountServiceAccountToken: &falseVal,
					SecurityContext:              nonRootPodSC(),
					ImagePullSecrets:             imagePullSecrets(cfg),
					InitContainers: []corev1.Container{
						CACombineInitContainer(cfg),
						WaitForValkeyInitContainer(cfg),
					},
					Containers: []corev1.Container{{
						Name:            containerName,
						Image:           image,
						ImagePullPolicy: pullPolicy(cfg),
						Command:         command,
						Env:             env,
						Resources:       resources,
						VolumeMounts:    KokuVolumeMounts(cfg),
						LivenessProbe:   liveness,
						ReadinessProbe:  readiness,
						SecurityContext: kokuAppContainerSC(),
					}},
					Volumes: KokuVolumes(cfg),
				},
			},
		},
	}
}

func deployment(
	cfg *costv1alpha1.CostManagementServiceConfig,
	name, component, image string,
	replicas int32,
	resources corev1.ResourceRequirements,
	liveness, readiness *corev1.Probe,
	env []corev1.EnvVar,
	command []string,
) *appsv1.Deployment {
	selLabels := SelectorLabels(cfg, component)
	allLabels := Labels(cfg, component)

	falseVal := false
	return &appsv1.Deployment{
		TypeMeta: metav1.TypeMeta{APIVersion: "apps/v1", Kind: "Deployment"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: cfg.Namespace,
			Labels:    allLabels,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{MatchLabels: selLabels},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: allLabels},
				Spec: corev1.PodSpec{
					ServiceAccountName:           NameKokuServiceAccount(cfg),
					AutomountServiceAccountToken: &falseVal,
					SecurityContext:              nonRootPodSC(),
					ImagePullSecrets:             imagePullSecrets(cfg),
					InitContainers: []corev1.Container{
						CACombineInitContainer(cfg),
						WaitForValkeyInitContainer(cfg),
					},
					Containers: []corev1.Container{
						{
							Name:            component,
							Image:           image,
							ImagePullPolicy: pullPolicy(cfg),
							Command:         command,
							Env:             env,
							Resources:       resources,
							VolumeMounts:    KokuVolumeMounts(cfg),
							LivenessProbe:   liveness,
							ReadinessProbe:  readiness,
							SecurityContext: kokuAppContainerSC(),
						},
					},
					Volumes: KokuVolumes(cfg),
				},
			},
		},
	}
}

func kokuAPIProbe(path string) *corev1.Probe {
	return &corev1.Probe{
		ProbeHandler: corev1.ProbeHandler{
			HTTPGet: &corev1.HTTPGetAction{
				Path:   path,
				Port:   intstr.FromInt32(9000),
				Scheme: corev1.URISchemeHTTP,
			},
		},
		InitialDelaySeconds: 30,
		PeriodSeconds:       20,
		TimeoutSeconds:      3,
		FailureThreshold:    5,
		SuccessThreshold:    1,
	}
}

func masuProbe(path string) *corev1.Probe {
	return kokuAPIProbe(path)
}
