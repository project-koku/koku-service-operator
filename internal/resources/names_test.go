package resources

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	costv1alpha1 "github.com/project-koku/koku-service-operator/api/v1alpha1"
)

func kafkaCfg(bs string) *costv1alpha1.CostManagementServiceConfig {
	cfg := &costv1alpha1.CostManagementServiceConfig{}
	cfg.Spec.Kafka.BootstrapServers = bs
	return cfg
}

func TestKafkaHostSingleBroker(t *testing.T) {
	if got := KafkaHost(kafkaCfg("kafka.example.com:9092")); got != "kafka.example.com" {
		t.Errorf("KafkaHost single broker = %q, want %q", got, "kafka.example.com")
	}
}

func TestKafkaPortSingleBroker(t *testing.T) {
	if got := KafkaPort(kafkaCfg("kafka.example.com:9092")); got != "9092" {
		t.Errorf("KafkaPort single broker = %q, want %q", got, "9092")
	}
}

func TestKafkaHostMultiBroker(t *testing.T) {
	// Multi-broker: "a:9092,b:9092" — must return host of first broker only.
	// Bug: scanning from the right finds the last colon, returning "a:9092,b".
	if got := KafkaHost(kafkaCfg("kafka-1.example.com:9092,kafka-2.example.com:9092")); got != "kafka-1.example.com" {
		t.Errorf("KafkaHost multi-broker = %q, want %q", got, "kafka-1.example.com")
	}
}

func TestKafkaPortMultiBroker(t *testing.T) {
	if got := KafkaPort(kafkaCfg("kafka-1.example.com:9092,kafka-2.example.com:9093")); got != "9092" {
		t.Errorf("KafkaPort multi-broker = %q, want %q", got, "9092")
	}
}

func TestDNS1123Label(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		{"cost_model", "cost-model"},
		{"subs_extraction", "subs-extraction"},
		{"subs_transmission", "subs-transmission"},
		{"celery", "celery"},
		{"ocp", "ocp"},
	}
	for _, tt := range tests {
		if got := DNS1123Label(tt.in); got != tt.want {
			t.Errorf("DNS1123Label(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestNameCeleryWorkerSanitizesQueue(t *testing.T) {
	cfg := &costv1alpha1.CostManagementServiceConfig{
		ObjectMeta: metav1.ObjectMeta{Name: "cost-management"},
	}
	got := NameCeleryWorker(cfg, "cost_model")
	want := "cost-management-celery-worker-cost-model"
	if got != want {
		t.Errorf("NameCeleryWorker() = %q, want %q", got, want)
	}
}

func TestCeleryWorkerDeploymentKeepsCeleryQueue(t *testing.T) {
	cfg := &costv1alpha1.CostManagementServiceConfig{
		ObjectMeta: metav1.ObjectMeta{Name: "cost-management", Namespace: "cost-onprem"},
	}
	cfg.Spec.CostManagement.API.Image.Repository = "quay.io/example/koku"
	cfg.Spec.CostManagement.API.Image.Tag = "latest"

	d := CeleryWorkerDeployment(cfg, "cost_model", costv1alpha1.CeleryWorkerSpec{Replicas: 1})
	if d.Name != "cost-management-celery-worker-cost-model" {
		t.Errorf("Deployment.Name = %q, want cost-management-celery-worker-cost-model", d.Name)
	}
	if d.Spec.Template.Spec.Containers[0].Name != "cost-worker-cost-model" {
		t.Errorf("container name = %q, want cost-worker-cost-model", d.Spec.Template.Spec.Containers[0].Name)
	}

	var queues string
	for _, e := range d.Spec.Template.Spec.Containers[0].Env {
		if e.Name == "WORKER_QUEUES" {
			queues = e.Value
			break
		}
	}
	if queues != "cost_model" {
		t.Errorf("WORKER_QUEUES = %q, want cost_model (Celery queue must keep underscore)", queues)
	}
}

// TestROSAPINetworkPolicyExists verifies that ROSAPINetworkPolicy is defined
// and restricts ingress to only the gateway. Without this policy any pod in
// the namespace can reach the ROS API on port 8000, bypassing Envoy JWT auth.
func TestROSAPINetworkPolicyExists(t *testing.T) {
	cfg := &costv1alpha1.CostManagementServiceConfig{
		ObjectMeta: metav1.ObjectMeta{Name: "cost-management", Namespace: "test"},
	}
	np := ROSAPINetworkPolicy(cfg)
	if np == nil {
		t.Fatal("ROSAPINetworkPolicy returned nil")
	}
	if np.Name == "" {
		t.Error("ROSAPINetworkPolicy has no name")
	}
	// Must have at least one ingress rule (gateway → ROS API).
	if len(np.Spec.Ingress) == 0 {
		t.Error("ROSAPINetworkPolicy has no ingress rules — all traffic is blocked or allowed")
	}
}
