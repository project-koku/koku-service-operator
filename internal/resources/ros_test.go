package resources

import (
	"strings"
	"testing"

	costv1alpha1 "github.com/project-koku/koku-service-operator/api/v1alpha1"
)

func rosCfg() *costv1alpha1.CostManagementServiceConfig {
	cfg := testCfg()
	enabled := true
	deploy := false
	cfg.Spec.ROS.Enabled = &enabled
	cfg.Spec.ROS.Image.Repository = "quay.io/cloudservices/ros-ocp-backend"
	cfg.Spec.ROS.Image.Tag = "latest"
	cfg.Spec.Database.Deploy = &deploy
	cfg.Spec.Database.Host = "postgres.example.svc"
	cfg.Spec.Database.Port = 5432
	cfg.Spec.Kafka.BootstrapServers = "kafka.example.com:9092"
	return cfg
}

func TestROSServiceAccount(t *testing.T) {
	cfg := rosCfg()
	sa := ROSServiceAccount(cfg)
	if sa.Name != NameROSServiceAccount(cfg) {
		t.Errorf("Name = %q", sa.Name)
	}
	if sa.Namespace != cfg.Namespace {
		t.Errorf("Namespace = %q", sa.Namespace)
	}
}

func TestCdappConfigMap_ContainsDBAndKafka(t *testing.T) {
	cfg := rosCfg()
	cm := CdappConfigMap(cfg)
	if cm.Name != NameCdappConfigMap(cfg) {
		t.Errorf("Name = %q", cm.Name)
	}
	data := cm.Data["cdappconfig.json"]
	for _, want := range []string{
		`"hostname": "postgres.example.svc"`,
		`"name": "costonprem_ros"`,
		`"hostname": "kafka.example.com"`,
		`"port": 9092`,
		uploadTopic,
		recommendationTopic,
	} {
		if !strings.Contains(data, want) {
			t.Errorf("cdappconfig missing %q\n%s", want, data)
		}
	}
}

func TestROSAPIDeployment_Shape(t *testing.T) {
	cfg := rosCfg()
	cfg.Spec.ROS.API.LogLevel = "DEBUG"
	d := ROSAPIDeployment(cfg)
	if d.Name != NameROSAPI(cfg) {
		t.Errorf("Name = %q", d.Name)
	}
	if d.Spec.Template.Spec.ServiceAccountName != NameROSServiceAccount(cfg) {
		t.Errorf("SA = %q", d.Spec.Template.Spec.ServiceAccountName)
	}
	if len(d.Spec.Template.Spec.InitContainers) < 2 {
		t.Fatalf("expected DB+Kafka init containers, got %d", len(d.Spec.Template.Spec.InitContainers))
	}
	c := d.Spec.Template.Spec.Containers[0]
	if c.Image != "quay.io/cloudservices/ros-ocp-backend:latest" {
		t.Errorf("image = %q", c.Image)
	}
	if len(c.Ports) != 2 || c.Ports[0].ContainerPort != rosAPIPort || c.Ports[1].ContainerPort != rosMetricPort {
		t.Errorf("ports = %+v", c.Ports)
	}
	env := envValues(c)
	if env["DB_NAME"] != rosDBName {
		t.Errorf("DB_NAME = %q", env["DB_NAME"])
	}
	if env["RBACHOST"] != NameRBACAPI(cfg) {
		t.Errorf("RBACHOST = %q", env["RBACHOST"])
	}
	if env["SERVICE_NAME"] != "ros-api" {
		t.Errorf("SERVICE_NAME = %q", env["SERVICE_NAME"])
	}
	if env["LOG_LEVEL"] != "DEBUG" {
		t.Errorf("LOG_LEVEL = %q, want DEBUG from spec.ros.api.logLevel", env["LOG_LEVEL"])
	}
	if c.LivenessProbe == nil || c.LivenessProbe.HTTPGet == nil || c.LivenessProbe.HTTPGet.Path != "/status" {
		t.Errorf("liveness probe = %+v", c.LivenessProbe)
	}
}

func TestROSAPIService_Ports(t *testing.T) {
	cfg := rosCfg()
	svc := ROSAPIService(cfg)
	if svc.Name != NameROSAPI(cfg) {
		t.Errorf("Name = %q", svc.Name)
	}
	if len(svc.Spec.Ports) != 2 || svc.Spec.Ports[0].Port != rosAPIPort || svc.Spec.Ports[1].Port != rosMetricPort {
		t.Errorf("ports = %+v", svc.Spec.Ports)
	}
	wantSel := SelectorLabels(cfg, "ros-api")
	if svc.Spec.Selector[labelComponent] != wantSel[labelComponent] {
		t.Errorf("selector[%s] = %q, want %q", labelComponent, svc.Spec.Selector[labelComponent], wantSel[labelComponent])
	}
}

func TestROSProcessorDeployment_Shape(t *testing.T) {
	cfg := rosCfg()
	cfg.Spec.ROS.Processor.Replicas = 2
	cfg.Spec.ROS.Processor.LogLevel = "DEBUG"
	d := ROSProcessorDeployment(cfg)
	if d.Name != NameROSProcessor(cfg) {
		t.Errorf("Name = %q", d.Name)
	}
	if d.Spec.Replicas == nil || *d.Spec.Replicas != 2 {
		t.Errorf("replicas = %v", d.Spec.Replicas)
	}
	c := d.Spec.Template.Spec.Containers[0]
	env := envValues(c)
	if env["SERVICE_NAME"] != "ros-processor" {
		t.Errorf("SERVICE_NAME = %q", env["SERVICE_NAME"])
	}
	if env["UPLOAD_TOPIC"] != uploadTopic {
		t.Errorf("UPLOAD_TOPIC = %q", env["UPLOAD_TOPIC"])
	}
	if env["LOG_LEVEL"] != "DEBUG" {
		t.Errorf("LOG_LEVEL = %q", env["LOG_LEVEL"])
	}
	if env["KRUIZE_HOST"] != NameKruize(cfg) {
		t.Errorf("KRUIZE_HOST = %q", env["KRUIZE_HOST"])
	}
	// Must wait for DB, Kafka, and Kruize before starting.
	if len(d.Spec.Template.Spec.InitContainers) < 3 {
		t.Fatalf("expected ≥3 init containers, got %d", len(d.Spec.Template.Spec.InitContainers))
	}
}

func TestROSPollerDeployment_Shape(t *testing.T) {
	cfg := rosCfg()
	d := ROSPollerDeployment(cfg)
	if d.Name != NameROSPoller(cfg) {
		t.Errorf("Name = %q", d.Name)
	}
	c := d.Spec.Template.Spec.Containers[0]
	if c.Name != "ros-rec-poller" {
		t.Errorf("container = %q", c.Name)
	}
	env := envValues(c)
	if env["SERVICE_NAME"] != "ros-recommendation-poller" {
		t.Errorf("SERVICE_NAME = %q", env["SERVICE_NAME"])
	}
	if env["RECOMMENDATION_TOPIC"] != recommendationTopic {
		t.Errorf("RECOMMENDATION_TOPIC = %q", env["RECOMMENDATION_TOPIC"])
	}
}

func TestROSHousekeeperDeployment_WaitsForKoku(t *testing.T) {
	cfg := rosCfg()
	d := ROSHousekeeperDeployment(cfg)
	if d.Name != NameROSHousekeeper(cfg) {
		t.Errorf("Name = %q", d.Name)
	}
	if len(d.Spec.Template.Spec.InitContainers) < 4 {
		t.Fatalf("expected DB+Kafka+Kruize+Koku inits, got %d", len(d.Spec.Template.Spec.InitContainers))
	}
	env := envValues(d.Spec.Template.Spec.Containers[0])
	if env["SERVICE_NAME"] != "ros-housekeeper-sources" {
		t.Errorf("SERVICE_NAME = %q", env["SERVICE_NAME"])
	}
	wantURL := "http://" + NameKokuAPI(cfg) + ":8000"
	if env["SOURCES_API_BASE_URL"] != wantURL {
		t.Errorf("SOURCES_API_BASE_URL = %q, want %q", env["SOURCES_API_BASE_URL"], wantURL)
	}
}

func TestROSPartitionCleanerCronJob_DefaultSchedule(t *testing.T) {
	cfg := rosCfg()
	cj := ROSPartitionCleanerCronJob(cfg)
	if cj.Name != cfg.Name+"-ros-partition-cleaner" {
		t.Errorf("Name = %q", cj.Name)
	}
	if cj.Spec.Schedule != "0 0 */15 * *" {
		t.Errorf("Schedule = %q", cj.Spec.Schedule)
	}
	c := cj.Spec.JobTemplate.Spec.Template.Spec.Containers[0]
	if c.Name != "ros-partition-cleaner" {
		t.Errorf("container = %q", c.Name)
	}
}
