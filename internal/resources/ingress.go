package resources

import (
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	costv1alpha1 "github.com/project-koku/koku-service-operator/api/v1alpha1"
)

const (
	ingressHTTPPort    = int32(8080)
	ingressMetricsPort = int32(9000)
)

// ingressS3Endpoint returns the S3 endpoint in "host:port" format required by
// insights-ingress-go (INGRESS_MINIOENDPOINT). The full-URL form returned by
// S3Endpoint() is not accepted by the ingress binary.
func ingressS3Endpoint(cfg *costv1alpha1.CostManagementServiceConfig) string {
	host := cfg.Spec.ObjectStorage.Endpoint
	// Prefer the discovered endpoint when OBC/NooBaa resolved a hostname.
	if cfg.Status.DiscoveredConfig != nil && cfg.Status.DiscoveredConfig.S3 != nil {
		// DiscoveredConfig.S3.Endpoint is a full URL; strip the scheme.
		full := cfg.Status.DiscoveredConfig.S3.Endpoint
		for _, pfx := range []string{"https://", "http://"} {
			if len(full) > len(pfx) && full[:len(pfx)] == pfx {
				full = full[len(pfx):]
				break
			}
		}
		if full != "" {
			return full // already contains host:port from OBC/NooBaa
		}
	}
	port := cfg.Spec.ObjectStorage.Port
	if port == 0 {
		if costv1alpha1.BoolVal(cfg.Spec.ObjectStorage.UseSSL, true) {
			port = 443
		} else {
			port = 80
		}
	}
	return fmt.Sprintf("%s:%d", host, port)
}

// ingressS3UseSSL returns "true"/"false" for the INGRESS_USESSL env var.
func ingressS3UseSSL(cfg *costv1alpha1.CostManagementServiceConfig) string {
	if costv1alpha1.BoolVal(cfg.Spec.ObjectStorage.UseSSL, true) {
		return "true"
	}
	return "false"
}

// IngressDeployment builds the insights-ingress-go Deployment.
// Traffic arrives pre-authenticated from the Envoy JWT gateway, so the ingress
// binary trusts the X-Rh-Identity header injected by Envoy.
func IngressDeployment(cfg *costv1alpha1.CostManagementServiceConfig) *appsv1.Deployment {
	spec := cfg.Spec.Ingress
	image := spec.Image.Repository + ":" + spec.Image.Tag
	selLabels := SelectorLabels(cfg, "ingress")
	allLabels := Labels(cfg, "ingress")
	replicas := int32(1)
	falseVal := false

	storageSecret := NameStorageSecret(cfg)
	maxUpload := spec.MaxUploadSize
	if maxUpload == 0 {
		maxUpload = 104857600 // 100 MiB
	}
	validTypes := spec.ValidTypes
	if validTypes == "" {
		validTypes = "hccm"
	}
	ingressBucket := S3IngressBucket(cfg)

	env := []corev1.EnvVar{
		EnvVal("INGRESS_WEBPORT", int32String(ingressHTTPPort)),
		EnvVal("INGRESS_METRICSPORT", int32String(ingressMetricsPort)),
		EnvVal("INGRESS_DEBUG", "false"),
		EnvVal("INGRESS_DEFAULTMAXSIZE", fmt.Sprintf("%d", maxUpload)),
		EnvVal("INGRESS_MAXUPLOADMEM", "33554432"), // 32 MiB
		EnvVal("INGRESS_VALID_UPLOAD_TYPES", validTypes),
		// S3-compatible storage (insights-ingress-go uses MINIO env names)
		EnvVal("INGRESS_MINIOENDPOINT", ingressS3Endpoint(cfg)),
		EnvVal("INGRESS_STAGEBUCKET", ingressBucket),
		EnvVal("INGRESS_USESSL", ingressS3UseSSL(cfg)),
		EnvVal("INGRESS_STAGERIMPLEMENTATION", "s3"),
		EnvFromSecretOptional("INGRESS_MINIOACCESSKEY", storageSecret, "access-key"),
		EnvFromSecretOptional("INGRESS_MINIOSECRETKEY", storageSecret, "secret-key"),
		EnvVal("AWS_CONFIG_FILE", "/etc/aws/config"),
		// Kafka
		EnvVal("INGRESS_KAFKABROKERS", cfg.Spec.Kafka.BootstrapServers),
		EnvVal("INGRESS_KAFKAANNOUNCETOPIC", "platform.upload.announce"),
		EnvVal("INGRESS_KAFKAGROUPID", "ingress"),
		// Auth: JWT is handled by the Envoy gateway; trust the injected header.
		EnvVal("INGRESS_AUTH", "true"),
		EnvVal("INGRESS_LOGLEVEL", "INFO"),
		// Matches KokuVolumeMounts + combine-ca.sh output path.
		EnvVal("SSL_CERT_FILE", "/etc/pki/ca-trust/combined/ca-bundle.crt"),
	}
	// Kafka SASL
	if cfg.Spec.Kafka.SASL.Mechanism != "" {
		env = append(env, EnvVal("INGRESS_KAFKASASLMECHANISM", cfg.Spec.Kafka.SASL.Mechanism))
		if cfg.Spec.Kafka.SASL.ExistingSecret != "" {
			env = append(env,
				EnvFromSecret("INGRESS_KAFKASASLUSERNAME", cfg.Spec.Kafka.SASL.ExistingSecret, "username"),
				EnvFromSecret("INGRESS_KAFKASASLPASSWORD", cfg.Spec.Kafka.SASL.ExistingSecret, "password"),
			)
		}
	}
	if cfg.Spec.Kafka.SecurityProtocol != "" && cfg.Spec.Kafka.SecurityProtocol != "PLAINTEXT" {
		env = append(env, EnvVal("INGRESS_KAFKASECURITYPROTOCOL", cfg.Spec.Kafka.SecurityProtocol))
	}

	vols, mounts := ingressVolumes(cfg)

	probe := &corev1.Probe{
		ProbeHandler: corev1.ProbeHandler{
			HTTPGet: &corev1.HTTPGetAction{
				Path: "/",
				Port: intstr.FromInt32(ingressHTTPPort),
			},
		},
		InitialDelaySeconds: 15, PeriodSeconds: 10, TimeoutSeconds: 5, FailureThreshold: 3,
	}

	return &appsv1.Deployment{
		TypeMeta:   metav1.TypeMeta{APIVersion: "apps/v1", Kind: "Deployment"},
		ObjectMeta: metav1.ObjectMeta{Name: NameIngress(cfg), Namespace: cfg.Namespace, Labels: allLabels},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{MatchLabels: selLabels},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: allLabels},
				Spec: corev1.PodSpec{
					ServiceAccountName:           NameIngressServiceAccount(cfg),
					AutomountServiceAccountToken: &falseVal,
					SecurityContext:              nonRootPodSC(),
					ImagePullSecrets:             imagePullSecrets(cfg),
					InitContainers:               []corev1.Container{CACombineInitContainer(cfg)},
					Containers: []corev1.Container{{
						Name:            "ingress",
						Image:           image,
						ImagePullPolicy: pullPolicy(cfg),
						Ports: []corev1.ContainerPort{
							{Name: "http", ContainerPort: ingressHTTPPort, Protocol: corev1.ProtocolTCP},
							{Name: "metrics", ContainerPort: ingressMetricsPort, Protocol: corev1.ProtocolTCP},
						},
						Env:             env,
						LivenessProbe:   probe,
						ReadinessProbe:  probe,
						Resources:       spec.Resources,
						VolumeMounts:    mounts,
						SecurityContext: restrictedContainerSC(),
					}},
					Volumes: vols,
				},
			},
		},
	}
}

// IngressService exposes the ingress upload handler inside the cluster.
// Envoy routes /api/ingress/ traffic to this service on port 8080.
func IngressService(cfg *costv1alpha1.CostManagementServiceConfig) *corev1.Service {
	return &corev1.Service{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "Service"},
		ObjectMeta: metav1.ObjectMeta{Name: NameIngress(cfg), Namespace: cfg.Namespace, Labels: Labels(cfg, "ingress")},
		Spec: corev1.ServiceSpec{
			Selector: SelectorLabels(cfg, "ingress"),
			Ports: []corev1.ServicePort{
				{Name: "http", Port: ingressHTTPPort, TargetPort: intstr.FromString("http"), Protocol: corev1.ProtocolTCP},
				{Name: "metrics", Port: ingressMetricsPort, TargetPort: intstr.FromString("metrics"), Protocol: corev1.ProtocolTCP},
			},
		},
	}
}

func ingressVolumes(cfg *costv1alpha1.CostManagementServiceConfig) ([]corev1.Volume, []corev1.VolumeMount) {
	// CACombineInitContainer needs ca-scripts / ca-source / combined-ca-bundle.
	vols := KokuVolumes(cfg)
	mounts := KokuVolumeMounts(cfg)
	if v, m := kafkaTLSVolumeAndMount(cfg); v != nil {
		found := false
		for _, existing := range vols {
			if existing.Name == v.Name {
				found = true
				break
			}
		}
		if !found {
			vols = append(vols, *v)
			mounts = append(mounts, *m)
		}
	}
	return vols, mounts
}
