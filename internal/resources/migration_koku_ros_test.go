package resources

import (
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	costv1alpha1 "github.com/project-koku/koku-service-operator/api/v1alpha1"
)

// ---------------------------------------------------------------------------
// MigrationJob (Koku Django migration)
// ---------------------------------------------------------------------------

func TestMigrationJobBuildsValidJob(t *testing.T) {
	old := OperatorImage
	OperatorImage = "quay.io/project-koku/koku-service-operator:test"
	defer func() { OperatorImage = old }()
	deploy := true

	cfg := &costv1alpha1.CostManagementServiceConfig{
		ObjectMeta: metav1.ObjectMeta{Name: "cost-onprem", Namespace: "cost-tests"},
		Spec: costv1alpha1.CostManagementServiceConfigSpec{
			Database: costv1alpha1.DatabaseConfig{
				Deploy: &deploy,
			},
			CostManagement: costv1alpha1.CostManagementConfig{
				API: costv1alpha1.KokuAPISpec{
					Image: costv1alpha1.ImageSpec{Repository: "quay.io/example/koku", Tag: "v1"},
				},
			},
		},
	}

	job := MigrationJob(cfg, "v1")

	if job == nil {
		t.Fatal("MigrationJob returned nil")
		return
	}
	if job.Name != "cost-onprem-koku-migrate" {
		t.Errorf("Name = %q, want cost-onprem-koku-migrate", job.Name)
	}
	if job.Namespace != "cost-tests" {
		t.Errorf("Namespace = %q, want cost-tests", job.Namespace)
	}
	if got := job.Annotations["koku.costmanagement.io/image-tag"]; got != "v1" {
		t.Errorf("image-tag annotation = %q, want v1", got)
	}

	spec := job.Spec.Template.Spec

	// BackoffLimit and deadline
	if job.Spec.BackoffLimit == nil || *job.Spec.BackoffLimit != MigrationBackoffLimit {
		t.Errorf("BackoffLimit = %v, want %d", job.Spec.BackoffLimit, MigrationBackoffLimit)
	}
	if job.Spec.ActiveDeadlineSeconds == nil || *job.Spec.ActiveDeadlineSeconds != MigrationDeadlineSeconds {
		t.Errorf("ActiveDeadlineSeconds = %v, want %d", job.Spec.ActiveDeadlineSeconds, MigrationDeadlineSeconds)
	}

	// Init containers: CACombine + waitForPostgres
	if len(spec.InitContainers) != 2 {
		t.Fatalf("want 2 init containers, got %d", len(spec.InitContainers))
	}
	if spec.InitContainers[0].Name != "prepare-ca-bundle" {
		t.Errorf("init[0].Name = %q, want prepare-ca-bundle", spec.InitContainers[0].Name)
	}
	if spec.InitContainers[1].Name != "wait-for-postgres" {
		t.Errorf("init[1].Name = %q, want wait-for-postgres", spec.InitContainers[1].Name)
	}

	// Main container
	if len(spec.Containers) != 1 {
		t.Fatalf("want 1 container, got %d", len(spec.Containers))
	}
	c := spec.Containers[0]
	if c.Image != "quay.io/example/koku:v1" {
		t.Errorf("Image = %q", c.Image)
	}
	if c.Name != "migrate" {
		t.Errorf("container name = %q, want migrate", c.Name)
	}

	// Script should be in Command[2] (bash -c <script>)
	if len(c.Command) < 3 || c.Command[0] != "bash" || c.Command[1] != "-c" {
		t.Fatalf("Command = %v, want [bash -c <script>]", c.Command)
	}
	script := c.Command[2]
	if !strings.Contains(script, "manage.py migrate") {
		t.Error("script missing manage.py migrate")
	}
	if strings.Contains(script, "/dev/tcp") {
		t.Error("script should not contain /dev/tcp — DB wait is in init container")
	}

	// Key env vars
	env := map[string]string{}
	for _, e := range c.Env {
		env[e.Name] = e.Value
	}
	if env["MASU"] != "false" {
		t.Errorf("MASU = %q, want false", env["MASU"])
	}
	if env["PROMETHEUS_MULTIPROC_DIR"] != "/tmp" {
		t.Errorf("PROMETHEUS_MULTIPROC_DIR = %q, want /tmp", env["PROMETHEUS_MULTIPROC_DIR"])
	}

	// Volumes: must include tmp at minimum (Koku standard volumes)
	volNames := map[string]bool{}
	for _, v := range spec.Volumes {
		volNames[v.Name] = true
	}
	if !volNames["tmp"] {
		t.Error("missing tmp volume")
	}

	// Security context on pod
	if spec.SecurityContext == nil || spec.SecurityContext.RunAsNonRoot == nil || !*spec.SecurityContext.RunAsNonRoot {
		t.Error("pod SecurityContext.RunAsNonRoot should be true")
	}

	// ServiceAccount
	if spec.ServiceAccountName == "" {
		t.Error("ServiceAccountName is empty")
	}

	// RestartPolicy
	if spec.RestartPolicy != "OnFailure" {
		t.Errorf("RestartPolicy = %q, want OnFailure", spec.RestartPolicy)
	}
}

func TestMigrationJobEmptyImageReturnsNil(t *testing.T) {
	old := OperatorImage
	OperatorImage = "quay.io/project-koku/koku-service-operator:test"
	defer func() { OperatorImage = old }()

	cfg := &costv1alpha1.CostManagementServiceConfig{
		ObjectMeta: metav1.ObjectMeta{Name: "cm", Namespace: "ns"},
	}
	if job := MigrationJob(cfg, "latest"); job != nil {
		t.Errorf("empty API image must not build a Job, got image %q", job.Spec.Template.Spec.Containers[0].Image)
	}
}

func TestMigrationJobPartialImageReturnsNil(t *testing.T) {
	old := OperatorImage
	OperatorImage = "quay.io/project-koku/koku-service-operator:test"
	defer func() { OperatorImage = old }()

	cfg := &costv1alpha1.CostManagementServiceConfig{
		ObjectMeta: metav1.ObjectMeta{Name: "cm", Namespace: "ns"},
	}
	cfg.Spec.CostManagement.API.Image.Repository = "quay.io/example/koku"
	if job := MigrationJob(cfg, "v1"); job != nil {
		t.Errorf("repository without tag must not build a Job, got image %q", job.Spec.Template.Spec.Containers[0].Image)
	}

	cfg.Spec.CostManagement.API.Image.Repository = ""
	cfg.Spec.CostManagement.API.Image.Tag = "v1"
	if job := MigrationJob(cfg, "v1"); job != nil {
		t.Errorf("tag without repository must not build a Job, got image %q", job.Spec.Template.Spec.Containers[0].Image)
	}
}

func TestKokuWorkloadsShareAPIImage(t *testing.T) {
	old := OperatorImage
	OperatorImage = "quay.io/project-koku/koku-service-operator:test"
	defer func() { OperatorImage = old }()

	cfg := &costv1alpha1.CostManagementServiceConfig{
		ObjectMeta: metav1.ObjectMeta{Name: "cm", Namespace: "ns"},
	}
	cfg.Spec.CostManagement.API.Image = costv1alpha1.ImageSpec{
		Repository: "quay.io/example/koku",
		Tag:        "abc123",
	}

	want, ok := KokuImage(cfg)
	if !ok || want != "quay.io/example/koku:abc123" {
		t.Fatalf("KokuImage = %q, ok=%v", want, ok)
	}

	got := map[string]string{
		"api":       KokuAPIDeployment(cfg).Spec.Template.Spec.Containers[0].Image,
		"masu":      MasuDeployment(cfg).Spec.Template.Spec.Containers[0].Image,
		"listener":  ListenerDeployment(cfg).Spec.Template.Spec.Containers[0].Image,
		"celery":    CeleryBeatDeployment(cfg).Spec.Template.Spec.Containers[0].Image,
		"migration": MigrationJob(cfg, "abc123").Spec.Template.Spec.Containers[0].Image,
	}
	for name, image := range got {
		if image != want {
			t.Errorf("%s image = %q, want %q (must match API image)", name, image, want)
		}
	}
}

func TestMigrationJobExternalDB(t *testing.T) {
	old := OperatorImage
	OperatorImage = "quay.io/project-koku/koku-service-operator:test"
	defer func() { OperatorImage = old }()

	f := false
	cfg := &costv1alpha1.CostManagementServiceConfig{
		ObjectMeta: metav1.ObjectMeta{Name: "cm", Namespace: "ns"},
		Spec: costv1alpha1.CostManagementServiceConfigSpec{
			Database: costv1alpha1.DatabaseConfig{
				Deploy: &f,
				Host:   "external-pg.example.com",
				Port:   15432,
			},
			CostManagement: costv1alpha1.CostManagementConfig{
				API: costv1alpha1.KokuAPISpec{
					Image: costv1alpha1.ImageSpec{Repository: "koku", Tag: "v1"},
				},
			},
		},
	}
	job := MigrationJob(cfg, "v1")

	initC := job.Spec.Template.Spec.InitContainers
	// With external DB, waitForPostgres should use waitForTCP (no pg_isready).
	var waitInit *int
	for i, c := range initC {
		if c.Name == "wait-for-db" || c.Name == "wait-for-postgres" {
			waitInit = &i
			break
		}
	}
	if waitInit == nil {
		t.Fatal("no wait-for init container found")
		return
	}
	wc := initC[*waitInit]
	// External DB path uses /wait-for binary (waitForTCP), not pg_isready.
	if len(wc.Command) == 0 {
		t.Fatal("wait init container has empty Command")
		return
	}
	if wc.Command[0] != "/wait-for" {
		t.Errorf("external DB wait command[0] = %q, want /wait-for", wc.Command[0])
	}
	// Verify host and port are passed as arguments, not embedded in shell text.
	cmdStr := strings.Join(wc.Command, " ")
	if !strings.Contains(cmdStr, "external-pg.example.com") {
		t.Error("external host not found in wait command args")
	}
	if !strings.Contains(cmdStr, "15432") {
		t.Error("external port not found in wait command args")
	}
}

func TestMigrationJobDefaultPort(t *testing.T) {
	old := OperatorImage
	OperatorImage = "quay.io/project-koku/koku-service-operator:test"
	defer func() { OperatorImage = old }()
	deploy := true

	cfg := &costv1alpha1.CostManagementServiceConfig{
		ObjectMeta: metav1.ObjectMeta{Name: "cm", Namespace: "ns"},
		Spec: costv1alpha1.CostManagementServiceConfigSpec{
			Database: costv1alpha1.DatabaseConfig{
				Deploy: &deploy,
			},
			CostManagement: costv1alpha1.CostManagementConfig{
				API: costv1alpha1.KokuAPISpec{
					Image: costv1alpha1.ImageSpec{Repository: "koku", Tag: "v1"},
				},
			},
		},
	}
	job := MigrationJob(cfg, "v1")

	// Bundled DB: waitForPostgres uses pg_isready with port as positional arg.
	initC := job.Spec.Template.Spec.InitContainers
	for _, c := range initC {
		if c.Name == "wait-for-postgres" {
			cmdStr := strings.Join(c.Command, " ")
			if !strings.Contains(cmdStr, "5432") {
				t.Error("default port 5432 not found in wait-for-postgres command")
			}
			return
		}
	}
	t.Error("wait-for-postgres init container not found")
}

// ---------------------------------------------------------------------------
// ROSMigrationJob (ROS schema migration)
// ---------------------------------------------------------------------------

func TestROSMigrationJobBuildsValidJob(t *testing.T) {
	old := OperatorImage
	OperatorImage = "quay.io/project-koku/koku-service-operator:test"
	defer func() { OperatorImage = old }()
	deploy := true

	cfg := &costv1alpha1.CostManagementServiceConfig{
		ObjectMeta: metav1.ObjectMeta{Name: "cost-onprem", Namespace: "cost-tests"},
		Spec: costv1alpha1.CostManagementServiceConfigSpec{
			Database: costv1alpha1.DatabaseConfig{
				Deploy: &deploy,
			},
			ROS: costv1alpha1.ROSConfig{
				Image: costv1alpha1.ImageSpec{Repository: "quay.io/example/ros", Tag: "v2"},
			},
		},
	}

	job := ROSMigrationJob(cfg, "v2")

	if job == nil {
		t.Fatal("ROSMigrationJob returned nil")
		return
	}
	if job.Name != "cost-onprem-ros-migrate" {
		t.Errorf("Name = %q, want cost-onprem-ros-migrate", job.Name)
	}
	if job.Namespace != "cost-tests" {
		t.Errorf("Namespace = %q, want cost-tests", job.Namespace)
	}
	if got := job.Annotations["koku.costmanagement.io/image-tag"]; got != "v2" {
		t.Errorf("image-tag annotation = %q, want v2", got)
	}

	spec := job.Spec.Template.Spec

	// Init containers: waitForPostgres only (no CACombine for ROS)
	if len(spec.InitContainers) != 1 {
		t.Fatalf("want 1 init container, got %d", len(spec.InitContainers))
	}
	if spec.InitContainers[0].Name != "wait-for-postgres" {
		t.Errorf("init[0].Name = %q, want wait-for-postgres", spec.InitContainers[0].Name)
	}

	// Main container
	if len(spec.Containers) != 1 {
		t.Fatalf("want 1 container, got %d", len(spec.Containers))
	}
	c := spec.Containers[0]
	if c.Image != "quay.io/example/ros:v2" {
		t.Errorf("Image = %q", c.Image)
	}
	if c.Name != "migrate" {
		t.Errorf("container name = %q, want migrate", c.Name)
	}

	// Script
	if len(c.Command) < 3 || c.Command[0] != "bash" || c.Command[1] != "-c" {
		t.Fatalf("Command = %v, want [bash -c <script>]", c.Command)
	}
	script := c.Command[2]
	if !strings.Contains(script, "rosocp db migrate up") {
		t.Error("script missing rosocp db migrate up")
	}
	if strings.Contains(script, "/dev/tcp") {
		t.Error("script should not contain /dev/tcp — DB wait is in init container")
	}

	// Env: DB connection vars
	env := map[string]string{}
	for _, e := range c.Env {
		env[e.Name] = e.Value
	}
	if env["CLOWDER_ENABLED"] != "false" {
		t.Errorf("CLOWDER_ENABLED = %q, want false", env["CLOWDER_ENABLED"])
	}
	if env["DB_NAME"] != RosDBName {
		t.Errorf("DB_NAME = %q, want %s", env["DB_NAME"], RosDBName)
	}
	if env["LOG_LEVEL"] != "INFO" {
		t.Errorf("LOG_LEVEL = %q, want INFO", env["LOG_LEVEL"])
	}

	// DB_USER and DB_PASSWORD must come from the credentials secret
	envSecrets := map[string]string{}
	for _, e := range c.Env {
		if e.ValueFrom != nil && e.ValueFrom.SecretKeyRef != nil {
			envSecrets[e.Name] = e.ValueFrom.SecretKeyRef.Key
		}
	}
	if envSecrets["DB_USER"] != "ros-user" {
		t.Errorf("DB_USER secret key = %q, want ros-user", envSecrets["DB_USER"])
	}
	if envSecrets["DB_PASSWORD"] != "ros-password" {
		t.Errorf("DB_PASSWORD secret key = %q, want ros-password", envSecrets["DB_PASSWORD"])
	}

	// Volumes: tmp
	volNames := map[string]bool{}
	for _, v := range spec.Volumes {
		volNames[v.Name] = true
	}
	if !volNames["tmp"] {
		t.Error("missing tmp volume")
	}

	// Volume mounts on main container
	mountNames := map[string]bool{}
	for _, m := range c.VolumeMounts {
		mountNames[m.Name] = true
	}
	if !mountNames["tmp"] {
		t.Error("missing tmp volume mount")
	}
}

func TestROSMigrationJobExternalDB(t *testing.T) {
	old := OperatorImage
	OperatorImage = "quay.io/project-koku/koku-service-operator:test"
	defer func() { OperatorImage = old }()

	f := false
	cfg := &costv1alpha1.CostManagementServiceConfig{
		ObjectMeta: metav1.ObjectMeta{Name: "cm", Namespace: "ns"},
		Spec: costv1alpha1.CostManagementServiceConfigSpec{
			Database: costv1alpha1.DatabaseConfig{
				Deploy: &f,
				Host:   "ext-db.example.com",
				Port:   25432,
			},
			ROS: costv1alpha1.ROSConfig{
				Image: costv1alpha1.ImageSpec{Repository: "ros", Tag: "v1"},
			},
		},
	}
	job := ROSMigrationJob(cfg, "v1")

	wc := job.Spec.Template.Spec.InitContainers[0]
	if wc.Command[0] != "/wait-for" {
		t.Errorf("external DB wait command[0] = %q, want /wait-for", wc.Command[0])
	}
	cmdStr := strings.Join(wc.Command, " ")
	if !strings.Contains(cmdStr, "ext-db.example.com") {
		t.Error("external host not found in wait command args")
	}
	if !strings.Contains(cmdStr, "25432") {
		t.Error("external port not found in wait command args")
	}
}

func TestROSMigrationJobDefaultPort(t *testing.T) {
	old := OperatorImage
	OperatorImage = "quay.io/project-koku/koku-service-operator:test"
	defer func() { OperatorImage = old }()

	cfg := &costv1alpha1.CostManagementServiceConfig{
		ObjectMeta: metav1.ObjectMeta{Name: "cm", Namespace: "ns"},
		Spec: costv1alpha1.CostManagementServiceConfigSpec{
			ROS: costv1alpha1.ROSConfig{
				Image: costv1alpha1.ImageSpec{Repository: "ros", Tag: "v1"},
			},
		},
	}
	job := ROSMigrationJob(cfg, "v1")

	wc := job.Spec.Template.Spec.InitContainers[0]
	cmdStr := strings.Join(wc.Command, " ")
	if !strings.Contains(cmdStr, "5432") {
		t.Error("default port 5432 not found in wait-for-postgres command")
	}
}

// ---------------------------------------------------------------------------
// NameROSMigration (0% coverage in Jordi's report)
// ---------------------------------------------------------------------------

func TestNameROSMigration(t *testing.T) {
	cfg := &costv1alpha1.CostManagementServiceConfig{
		ObjectMeta: metav1.ObjectMeta{Name: "cost-onprem"},
	}
	if got := NameROSMigration(cfg); got != "cost-onprem-ros-migrate" {
		t.Errorf("NameROSMigration = %q, want cost-onprem-ros-migrate", got)
	}
}
