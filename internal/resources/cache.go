package resources

import (
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	costv1alpha1 "github.com/project-koku/koku-service-operator/api/v1alpha1"
)

// CacheDeployment builds the bundled Valkey Deployment (dev/CI only).
// Image comes from spec.cache.image (required when deploy is true).
//
// The server runs with --protected-mode no and no --requirepass / TLS flags.
// This is intentional: the bundled cache is not for production (BYOI model).
// spec.cache.auth and spec.cache.tls configure *client-side* env vars on
// Koku/RBAC containers for connecting to an *external* Redis/Valkey; they
// do not affect this bundled server. A CacheNetworkPolicy restricts ingress
// to operator-managed pods as defense in depth.
func CacheDeployment(cfg *costv1alpha1.CostManagementServiceConfig) *appsv1.Deployment {
	name := NameValkey(cfg)
	selLabels := SelectorLabels(cfg, "cache")
	allLabels := Labels(cfg, "cache")

	c := cfg.Spec.Cache
	image, _ := ImageRef(c.Image)

	port := c.Port
	if port == 0 {
		port = 6379
	}

	replicas := int32(1)
	recreate := appsv1.RecreateDeploymentStrategyType

	return &appsv1.Deployment{
		TypeMeta: metav1.TypeMeta{APIVersion: "apps/v1", Kind: "Deployment"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: cfg.Namespace,
			Labels:    allLabels,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Strategy: appsv1.DeploymentStrategy{Type: recreate},
			Selector: &metav1.LabelSelector{MatchLabels: selLabels},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: allLabels},
				Spec: corev1.PodSpec{
					SecurityContext:  cachePodSC(),
					ImagePullSecrets: imagePullSecrets(cfg),
					Containers: []corev1.Container{
						{
							Name:            "valkey",
							Image:           image,
							ImagePullPolicy: pullPolicy(cfg),
							Ports: []corev1.ContainerPort{
								{Name: "valkey", ContainerPort: port, Protocol: corev1.ProtocolTCP},
							},
							Command: []string{"valkey-server"},
							Args: []string{
								"--bind", "0.0.0.0",
								"--port", int32String(port),
								"--protected-mode", "no",
								"--save", "900 1 300 10 60 10000",
								"--appendonly", "yes",
								"--appendfsync", "everysec",
								"--dir", "/data",
								"--maxmemory", "512mb",
								"--maxmemory-policy", "allkeys-lru",
							},
							LivenessProbe: &corev1.Probe{
								ProbeHandler: corev1.ProbeHandler{
									Exec: &corev1.ExecAction{Command: []string{"valkey-cli", "ping"}},
								},
								InitialDelaySeconds: 30,
								PeriodSeconds:       10,
								TimeoutSeconds:      5,
								FailureThreshold:    3,
							},
							ReadinessProbe: &corev1.Probe{
								ProbeHandler: corev1.ProbeHandler{
									Exec: &corev1.ExecAction{Command: []string{"valkey-cli", "ping"}},
								},
								InitialDelaySeconds: 5,
								PeriodSeconds:       10,
								TimeoutSeconds:      5,
								FailureThreshold:    3,
							},
							Resources: c.Resources,
							VolumeMounts: []corev1.VolumeMount{
								{Name: "data", MountPath: "/data"},
							},
							SecurityContext: restrictedContainerSC(),
						},
					},
					Volumes: []corev1.Volume{
						{
							Name: "data",
							VolumeSource: corev1.VolumeSource{
								PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
									ClaimName: NameValkeyPVC(cfg),
								},
							},
						},
					},
				},
			},
		},
	}
}

// CacheService exposes the Valkey Deployment.
func CacheService(cfg *costv1alpha1.CostManagementServiceConfig) *corev1.Service {
	port := cfg.Spec.Cache.Port
	if port == 0 {
		port = 6379
	}
	return &corev1.Service{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Service"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      NameValkey(cfg),
			Namespace: cfg.Namespace,
			Labels:    Labels(cfg, "cache"),
		},
		Spec: corev1.ServiceSpec{
			Selector: SelectorLabels(cfg, "cache"),
			Ports: []corev1.ServicePort{
				{Name: "valkey", Port: port, Protocol: corev1.ProtocolTCP},
			},
		},
	}
}

// cachePodSC sets fsGroup so Kubernetes chowns the PVC to that GID on mount,
// allowing the container's non-root user to write to /data.
func cachePodSC() *corev1.PodSecurityContext {
	nonRoot := true
	fsGroup := int64(1000)
	return &corev1.PodSecurityContext{
		RunAsNonRoot: &nonRoot,
		FSGroup:      &fsGroup,
	}
}

// CachePVC builds the PersistentVolumeClaim for Valkey data.
func CachePVC(cfg *costv1alpha1.CostManagementServiceConfig) *corev1.PersistentVolumeClaim {
	size := cfg.Spec.Cache.Persistence.Size
	if size.IsZero() {
		size = resource.MustParse("5Gi")
	}
	sc := cfg.Spec.Global.StorageClass
	var scPtr *string
	if sc != "" {
		scPtr = &sc
	}
	return &corev1.PersistentVolumeClaim{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "PersistentVolumeClaim"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      NameValkeyPVC(cfg),
			Namespace: cfg.Namespace,
			Labels:    Labels(cfg, "cache"),
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			StorageClassName: scPtr,
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceStorage: size,
				},
			},
		},
	}
}
