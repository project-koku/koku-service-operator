package resources

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	costv1alpha1 "github.com/project-koku/koku-service-operator/api/v1alpha1"
)

func ingressCfg() *costv1alpha1.CostManagementServiceConfig {
	cfg := testCfg()
	cfg.Spec.Ingress.Image.Repository = "quay.io/cloudservices/insights-ingress"
	cfg.Spec.Ingress.Image.Tag = "latest"
	cfg.Spec.ObjectStorage.Endpoint = "minio.cost-byoi-infra.svc"
	cfg.Spec.ObjectStorage.Port = 9000
	useSSL := false
	cfg.Spec.ObjectStorage.UseSSL = &useSSL
	cfg.Spec.Kafka.BootstrapServers = "my-cluster-kafka-bootstrap.kafka.svc:9092"
	return cfg
}

func TestIngressS3EndpointFromSpec(t *testing.T) {
	cfg := ingressCfg()
	if got := ingressS3Endpoint(cfg); got != "minio.cost-byoi-infra.svc:9000" {
		t.Errorf("ingressS3Endpoint = %q", got)
	}
}

func TestIngressS3EndpointFromDiscovered(t *testing.T) {
	cfg := ingressCfg()
	cfg.Status.DiscoveredConfig = &costv1alpha1.DiscoveredConfig{
		S3: &costv1alpha1.DiscoveredS3{Endpoint: "https://s3.openshift-storage.svc:443"},
	}
	if got := ingressS3Endpoint(cfg); got != "s3.openshift-storage.svc:443" {
		t.Errorf("ingressS3Endpoint discovered = %q", got)
	}
}

func TestIngressS3EndpointDiscoveredTakesPrecedence(t *testing.T) {
	cfg := ingressCfg()
	// Spec still has minio…:9000 from ingressCfg(); discovered must win.
	cfg.Status.DiscoveredConfig = &costv1alpha1.DiscoveredConfig{
		S3: &costv1alpha1.DiscoveredS3{Endpoint: "http://obc-bucket.openshift-storage.svc:443"},
	}
	if got := ingressS3Endpoint(cfg); got != "obc-bucket.openshift-storage.svc:443" {
		t.Errorf("discovered should win over spec; got %q", got)
	}
}

func TestIngressS3UseSSL(t *testing.T) {
	cfg := ingressCfg()
	if got := ingressS3UseSSL(cfg); got != "false" {
		t.Errorf("UseSSL=false → %q", got)
	}
	cfg.Spec.ObjectStorage.UseSSL = nil // default true
	if got := ingressS3UseSSL(cfg); got != "true" {
		t.Errorf("UseSSL default → %q", got)
	}
}

func TestIngressDeployment(t *testing.T) {
	cfg := ingressCfg()
	d := IngressDeployment(cfg)
	if d.Name != NameIngress(cfg) {
		t.Errorf("Name = %q", d.Name)
	}
	c := d.Spec.Template.Spec.Containers[0]
	if c.Name != "ingress" {
		t.Errorf("container = %q", c.Name)
	}
	if c.Image != "quay.io/cloudservices/insights-ingress:latest" {
		t.Errorf("image = %q", c.Image)
	}
	if len(c.Ports) != 2 {
		t.Fatalf("ports = %+v", c.Ports)
	}
	env := envValues(c)
	checks := map[string]string{
		"INGRESS_WEBPORT":              "8080",
		"INGRESS_METRICSPORT":          "9000",
		"INGRESS_MINIOENDPOINT":        "minio.cost-byoi-infra.svc:9000",
		"INGRESS_USESSL":               "false",
		"INGRESS_VALID_UPLOAD_TYPES":   "hccm",
		"INGRESS_STAGERIMPLEMENTATION": "s3",
		"INGRESS_AUTH":                 "true",
		"INGRESS_KAFKABROKERS":         "my-cluster-kafka-bootstrap.kafka.svc:9092",
	}
	for k, want := range checks {
		if env[k] != want {
			t.Errorf("env %s = %q, want %q", k, env[k], want)
		}
	}
	var sawAccess, sawSecret bool
	for _, e := range c.Env {
		if e.Name == "INGRESS_MINIOACCESSKEY" && e.ValueFrom != nil && e.ValueFrom.SecretKeyRef != nil {
			sawAccess = e.ValueFrom.SecretKeyRef.Key == "access-key"
		}
		if e.Name == "INGRESS_MINIOSECRETKEY" && e.ValueFrom != nil && e.ValueFrom.SecretKeyRef != nil {
			sawSecret = e.ValueFrom.SecretKeyRef.Key == "secret-key"
		}
	}
	if !sawAccess || !sawSecret {
		t.Error("expected MINIO access/secret keys from Secret")
	}
	if len(d.Spec.Template.Spec.InitContainers) == 0 {
		t.Error("expected CA combine init container")
	}
}

func TestIngressService(t *testing.T) {
	cfg := ingressCfg()
	svc := IngressService(cfg)
	if svc.Name != NameIngress(cfg) {
		t.Errorf("Name = %q", svc.Name)
	}
	if len(svc.Spec.Ports) != 2 {
		t.Fatalf("ports = %+v", svc.Spec.Ports)
	}
	if svc.Spec.Ports[0].Port != 8080 || svc.Spec.Ports[1].Port != 9000 {
		t.Errorf("ports = %+v", svc.Spec.Ports)
	}
}

func TestIngressDeploymentStorageCredentialEnv(t *testing.T) {
	cfg := &costv1alpha1.CostManagementServiceConfig{
		ObjectMeta: metav1.ObjectMeta{Name: "cost-management", Namespace: "cost-onprem"},
		Spec: costv1alpha1.CostManagementServiceConfigSpec{
			ObjectStorage: costv1alpha1.ObjectStorageConfig{
				SecretName: "user-s3-creds",
			},
			Ingress: costv1alpha1.IngressConfig{
				Image: costv1alpha1.ImageSpec{
					Repository: "quay.io/cloudservices/insights-ingress",
					Tag:        "latest",
				},
			},
		},
	}

	dep := IngressDeployment(cfg)
	byName := map[string]string{}
	for _, c := range dep.Spec.Template.Spec.Containers {
		for _, e := range c.Env {
			if e.ValueFrom != nil && e.ValueFrom.SecretKeyRef != nil {
				byName[e.Name] = e.ValueFrom.SecretKeyRef.Name + "/" + e.ValueFrom.SecretKeyRef.Key
			}
		}
	}

	want := map[string]string{
		"INGRESS_MINIOACCESSKEY": "user-s3-creds/access-key",
		"INGRESS_MINIOSECRETKEY": "user-s3-creds/secret-key",
	}
	for name, wantRef := range want {
		if got := byName[name]; got != wantRef {
			t.Errorf("%s: got %q, want %q", name, got, wantRef)
		}
	}
}
