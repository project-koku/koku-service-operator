package resources

import (
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	costv1alpha1 "github.com/project-koku/koku-service-operator/api/v1alpha1"
)

// DatabaseStatefulSet builds the PostgreSQL StatefulSet.
// Image comes from spec.database.image (required when deploy is true).
// The container contract is SCL/RHEL (POSTGRESQL_ADMIN_PASSWORD,
// /var/lib/pgsql/data); docker.io/library/postgres is not compatible.
func DatabaseStatefulSet(cfg *costv1alpha1.CostManagementServiceConfig) *appsv1.StatefulSet {
	name := NameDatabase(cfg)
	selLabels := SelectorLabels(cfg, "database")
	allLabels := Labels(cfg, "database")
	dbSecret := NameDBCredentials(cfg)

	dbSpec := cfg.Spec.Database
	image, _ := ImageRef(dbSpec.Image)

	storageSize := dbSpec.Storage.Size
	if storageSize.IsZero() {
		storageSize = resource.MustParse("30Gi")
	}

	port := dbSpec.Port
	if port == 0 {
		port = 5432
	}

	storageClass := cfg.Spec.Global.StorageClass
	var scPtr *string
	if storageClass != "" {
		scPtr = &storageClass
	}

	replicas := int32(1)

	return &appsv1.StatefulSet{
		TypeMeta: metav1.TypeMeta{APIVersion: "apps/v1", Kind: "StatefulSet"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: cfg.Namespace,
			Labels:    allLabels,
		},
		Spec: appsv1.StatefulSetSpec{
			ServiceName: name,
			Replicas:    &replicas,
			Selector: &metav1.LabelSelector{
				MatchLabels: selLabels,
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: allLabels},
				Spec: corev1.PodSpec{
					SecurityContext:  dbPodSC(),
					ImagePullSecrets: imagePullSecrets(cfg),
					Containers: []corev1.Container{
						{
							Name:            "postgres",
							Image:           image,
							ImagePullPolicy: pullPolicy(cfg),
							Ports: []corev1.ContainerPort{
								{Name: "postgres", ContainerPort: port, Protocol: corev1.ProtocolTCP},
							},
							Env: []corev1.EnvVar{
								EnvFromSecret("POSTGRESQL_ADMIN_PASSWORD", dbSecret, "postgres-password"),
								EnvFromSecret("POSTGRES_USER", dbSecret, "postgres-user"),
								EnvFromSecret("ROS_USER", dbSecret, "ros-user"),
								EnvFromSecret("ROS_PASSWORD", dbSecret, "ros-password"),
								EnvFromSecret("KRUIZE_USER", dbSecret, "kruize-user"),
								EnvFromSecret("KRUIZE_PASSWORD", dbSecret, "kruize-password"),
								EnvFromSecret("KOKU_USER", dbSecret, "koku-user"),
								EnvFromSecret("KOKU_PASSWORD", dbSecret, "koku-password"),
								EnvFromSecret("RBAC_USER", dbSecret, "rbac-user"),
								EnvFromSecret("RBAC_PASSWORD", dbSecret, "rbac-password"),
							},
							VolumeMounts: []corev1.VolumeMount{
								{Name: "postgres-storage", MountPath: "/var/lib/pgsql/data"},
								{Name: "init-scripts", MountPath: "/opt/app-root/src/postgresql-init", ReadOnly: true},
							},
							LivenessProbe: &corev1.Probe{
								ProbeHandler: corev1.ProbeHandler{
									Exec: &corev1.ExecAction{
										Command: []string{"/bin/sh", "-c", `pg_isready -U "$POSTGRES_USER"`},
									},
								},
								InitialDelaySeconds: 30,
								PeriodSeconds:       10,
								TimeoutSeconds:      5,
								FailureThreshold:    3,
							},
							ReadinessProbe: &corev1.Probe{
								ProbeHandler: corev1.ProbeHandler{
									Exec: &corev1.ExecAction{
										Command: []string{"/bin/sh", "-c", `pg_isready -U "$POSTGRES_USER"`},
									},
								},
								InitialDelaySeconds: 5,
								PeriodSeconds:       10,
								TimeoutSeconds:      5,
								FailureThreshold:    3,
							},
							Resources: dbSpec.Resources,
							// PostgreSQL writes to /var/run, /tmp, and the data dir;
							// readOnlyRootFilesystem would break it.
							SecurityContext: dbContainerSC(),
						},
					},
					Volumes: []corev1.Volume{
						{
							Name: "init-scripts",
							VolumeSource: corev1.VolumeSource{
								ConfigMap: &corev1.ConfigMapVolumeSource{
									LocalObjectReference: corev1.LocalObjectReference{Name: NameDBInitConfigMap(cfg)},
								},
							},
						},
					},
				},
			},
			VolumeClaimTemplates: []corev1.PersistentVolumeClaim{
				{
					ObjectMeta: metav1.ObjectMeta{Name: "postgres-storage"},
					Spec: corev1.PersistentVolumeClaimSpec{
						AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
						StorageClassName: scPtr,
						Resources: corev1.VolumeResourceRequirements{
							Requests: corev1.ResourceList{
								corev1.ResourceStorage: storageSize,
							},
						},
					},
				},
			},
		},
	}
}

// DatabaseService exposes the StatefulSet's headless service.
func DatabaseService(cfg *costv1alpha1.CostManagementServiceConfig) *corev1.Service {
	name := NameDatabase(cfg)
	port := cfg.Spec.Database.Port
	if port == 0 {
		port = 5432
	}
	return &corev1.Service{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Service"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: cfg.Namespace,
			Labels:    Labels(cfg, "database"),
		},
		Spec: corev1.ServiceSpec{
			// ClusterNone = headless; used as the StatefulSet service name for stable DNS.
			ClusterIP: "None",
			Selector:  SelectorLabels(cfg, "database"),
			Ports: []corev1.ServicePort{
				{Name: "postgres", Port: port, Protocol: corev1.ProtocolTCP},
			},
		},
	}
}

// dbContainerSC returns a security context suitable for the PostgreSQL
// dbPodSC returns the pod-level security context for PostgreSQL.
// fsGroup:26 causes Kubernetes to chown mounted volumes to the postgres GID
// so the image (which runs as UID/GID 26) can write to the PVC.
func dbPodSC() *corev1.PodSecurityContext {
	fsGroup := int64(26)
	return &corev1.PodSecurityContext{
		FSGroup: &fsGroup,
	}
}

// dbContainerSC returns the container-level security context for PostgreSQL:
// no privilege escalation, readOnlyRootFilesystem left false because
// PostgreSQL writes to /var/run and /tmp inside the container filesystem.
func dbContainerSC() *corev1.SecurityContext {
	f := false
	return &corev1.SecurityContext{
		AllowPrivilegeEscalation: &f,
		Privileged:               &f,
		Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
	}
}
