package resources

import (
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	costv1alpha1 "github.com/project-koku/koku-service-operator/api/v1alpha1"
)

func servicePortsByName(ports []corev1.ServicePort) map[string]corev1.ServicePort {
	out := make(map[string]corev1.ServicePort, len(ports))
	for _, p := range ports {
		out[p.Name] = p
	}
	return out
}

func TestKokuAPIService(t *testing.T) {
	cfg := testCfg()
	svc := KokuAPIService(cfg)
	if svc.Name != NameKokuAPI(cfg) {
		t.Errorf("Name = %q, want %q", svc.Name, NameKokuAPI(cfg))
	}
	if svc.Namespace != cfg.Namespace {
		t.Errorf("Namespace = %q", svc.Namespace)
	}
	if svc.Spec.Selector[labelComponent] != "cost-management-api" {
		t.Errorf("selector component = %q", svc.Spec.Selector[labelComponent])
	}
	if len(svc.Spec.Ports) != 2 {
		t.Fatalf("ports = %+v, want http + metrics", svc.Spec.Ports)
	}

	byName := servicePortsByName(svc.Spec.Ports)
	httpPort, ok := byName["http"]
	if !ok {
		t.Fatal("missing port named http")
	}
	if httpPort.Port != 8000 || httpPort.Protocol != corev1.ProtocolTCP {
		t.Errorf("http port = %+v, want port 8000/TCP", httpPort)
	}
	if httpPort.TargetPort != intstr.FromString("http") {
		t.Errorf("http TargetPort = %+v, want named http", httpPort.TargetPort)
	}

	metricsPort, ok := byName["metrics"]
	if !ok {
		t.Fatal("missing port named metrics")
	}
	if metricsPort.Port != 9000 || metricsPort.Protocol != corev1.ProtocolTCP {
		t.Errorf("metrics port = %+v, want port 9000/TCP", metricsPort)
	}
	if metricsPort.TargetPort != intstr.FromString("metrics") {
		t.Errorf("metrics TargetPort = %+v, want named metrics", metricsPort.TargetPort)
	}
}

func TestMasuService(t *testing.T) {
	cfg := testCfg()
	svc := MasuService(cfg)
	if svc.Name != NameMasu(cfg) {
		t.Errorf("Name = %q, want %q", svc.Name, NameMasu(cfg))
	}
	if svc.Namespace != cfg.Namespace {
		t.Errorf("Namespace = %q", svc.Namespace)
	}
	if svc.Spec.Selector[labelComponent] != "cost-processor" {
		t.Errorf("selector component = %q", svc.Spec.Selector[labelComponent])
	}
	if len(svc.Spec.Ports) != 2 {
		t.Fatalf("ports = %+v, want 2 ports", svc.Spec.Ports)
	}

	byName := servicePortsByName(svc.Spec.Ports)
	httpPort, ok := byName["http"]
	if !ok {
		t.Fatal("missing port named http")
	}
	if httpPort.Port != 8000 || httpPort.Protocol != corev1.ProtocolTCP {
		t.Errorf("http port = %+v, want http/8000/TCP", httpPort)
	}
	if httpPort.TargetPort != intstr.FromString("http") {
		t.Errorf("http TargetPort = %+v, want named http", httpPort.TargetPort)
	}
	metricsPort, ok := byName["metrics"]
	if !ok {
		t.Fatal("missing port named metrics")
	}
	if metricsPort.Port != 9000 || metricsPort.Protocol != corev1.ProtocolTCP {
		t.Errorf("metrics port = %+v, want metrics/9000/TCP", metricsPort)
	}
	if metricsPort.TargetPort != intstr.FromString("metrics") {
		t.Errorf("metrics TargetPort = %+v, want named metrics", metricsPort.TargetPort)
	}
}

func celeryWorkerCfg() *costv1alpha1.CostManagementServiceConfig {
	cfg := testCfg()
	cfg.Spec.CostManagement.API.Image.Repository = "quay.io/example/koku"
	cfg.Spec.CostManagement.API.Image.Tag = "latest"
	cfg.Spec.CostManagement.Celery.Workers.Default.Replicas = 1
	cfg.Spec.CostManagement.Celery.Workers.Priority.Replicas = 1
	return cfg
}

func celeryDeploymentReplicas(t *testing.T, cfg *costv1alpha1.CostManagementServiceConfig, deps []*appsv1.Deployment, queue string) int32 {
	t.Helper()
	wantName := NameCeleryWorker(cfg, queue)
	for _, d := range deps {
		if d.Name != wantName {
			continue
		}
		if d.Spec.Replicas == nil {
			t.Fatalf("deployment %q has nil replicas", d.Name)
		}
		return *d.Spec.Replicas
	}
	t.Fatalf("deployment for queue %q not found", queue)
	return 0
}

func TestCeleryWorkerDeployments_SaaSQueuesDefaultZero(t *testing.T) {
	cfg := celeryWorkerCfg()
	deps := CeleryWorkerDeployments(cfg)

	for _, queue := range []string{"hcs", "subs_extraction", "subs_transmission"} {
		if got := celeryDeploymentReplicas(t, cfg, deps, queue); got != 0 {
			t.Errorf("queue %q replicas = %d, want 0", queue, got)
		}
	}
	if got := celeryDeploymentReplicas(t, cfg, deps, "priority"); got != 1 {
		t.Errorf("on-prem queue priority replicas = %d, want 1", got)
	}
}

func TestCeleryWorkerDeployments_SaaSQueuesOptIn(t *testing.T) {
	cfg := celeryWorkerCfg()
	cfg.Spec.CostManagement.Celery.Workers.HCS.Replicas = 2
	cfg.Spec.CostManagement.Celery.Workers.SubsExtraction.Replicas = 1

	deps := CeleryWorkerDeployments(cfg)
	if got := celeryDeploymentReplicas(t, cfg, deps, "hcs"); got != 2 {
		t.Errorf("hcs replicas = %d, want 2", got)
	}
	if got := celeryDeploymentReplicas(t, cfg, deps, "subs_extraction"); got != 1 {
		t.Errorf("subs_extraction replicas = %d, want 1", got)
	}
}

func TestCeleryBeatDeployment_ContainerResources(t *testing.T) {
	cfg := testCfg()
	cfg.Spec.CostManagement.API.Image.Repository = "quay.io/example/koku"
	cfg.Spec.CostManagement.API.Image.Tag = "latest"

	dep := CeleryBeatDeployment(cfg)
	container := dep.Spec.Template.Spec.Containers[0]

	cpuReq := container.Resources.Requests[corev1.ResourceCPU]
	memReq := container.Resources.Requests[corev1.ResourceMemory]
	cpuLim := container.Resources.Limits[corev1.ResourceCPU]
	memLim := container.Resources.Limits[corev1.ResourceMemory]

	if cpuReq.String() != "50m" {
		t.Errorf("CPU request = %s, want 50m", cpuReq.String())
	}
	if memReq.String() != "200Mi" {
		t.Errorf("Memory request = %s, want 200Mi", memReq.String())
	}
	if cpuLim.String() != "100m" {
		t.Errorf("CPU limit = %s, want 100m", cpuLim.String())
	}
	if memLim.String() != "400Mi" {
		t.Errorf("Memory limit = %s, want 400Mi", memLim.String())
	}
}
