package resources

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
)

func TestDatabaseStatefulSet_DefaultImageAndSCLContract(t *testing.T) {
	cfg := testCfg()
	sts := DatabaseStatefulSet(cfg)
	c := sts.Spec.Template.Spec.Containers[0]
	if c.Image != defaultDatabaseImage {
		t.Errorf("default image = %q, want %q", c.Image, defaultDatabaseImage)
	}

	foundAdmin := false
	for _, e := range c.Env {
		if e.Name == "POSTGRESQL_ADMIN_PASSWORD" {
			foundAdmin = true
			break
		}
	}
	if !foundAdmin {
		t.Error("missing POSTGRESQL_ADMIN_PASSWORD (RHEL/SCL contract; docker official Postgres will not boot)")
	}

	foundData, foundInit := false, false
	for _, m := range c.VolumeMounts {
		if m.MountPath == "/var/lib/pgsql/data" {
			foundData = true
		}
		if m.MountPath == "/opt/app-root/src/postgresql-init" {
			foundInit = true
		}
	}
	if !foundData {
		t.Error("missing data mount /var/lib/pgsql/data")
	}
	if !foundInit {
		t.Error("missing init mount /opt/app-root/src/postgresql-init")
	}

	sc := sts.Spec.Template.Spec.SecurityContext
	if sc == nil || sc.FSGroup == nil || *sc.FSGroup != 26 {
		t.Errorf("FSGroup = %v, want 26", sc)
	}
}

func TestDatabaseStatefulSet_ExplicitImage(t *testing.T) {
	cfg := testCfg()
	cfg.Spec.Database.Image.Repository = "registry.redhat.io/rhel10/postgresql-16"
	cfg.Spec.Database.Image.Tag = "10.1"
	sts := DatabaseStatefulSet(cfg)
	got := sts.Spec.Template.Spec.Containers[0].Image
	if got != "registry.redhat.io/rhel10/postgresql-16:10.1" {
		t.Errorf("image = %q", got)
	}
}

func TestDatabaseService_DefaultPort(t *testing.T) {
	cfg := testCfg()
	svc := DatabaseService(cfg)
	if svc.Name != NameDatabase(cfg) {
		t.Errorf("Name = %q", svc.Name)
	}
	if svc.Spec.ClusterIP != "None" {
		t.Errorf("ClusterIP = %q, want None (headless)", svc.Spec.ClusterIP)
	}
	if svc.Spec.Selector[labelComponent] != "database" {
		t.Errorf("selector component = %q", svc.Spec.Selector[labelComponent])
	}
	if len(svc.Spec.Ports) != 1 {
		t.Fatalf("ports = %+v", svc.Spec.Ports)
	}
	port := svc.Spec.Ports[0]
	if port.Name != "postgres" || port.Port != 5432 || port.Protocol != corev1.ProtocolTCP {
		t.Errorf("port = %+v, want postgres/5432/TCP", port)
	}
}

func TestDatabaseService_CustomPort(t *testing.T) {
	cfg := testCfg()
	cfg.Spec.Database.Port = 5433
	svc := DatabaseService(cfg)
	if len(svc.Spec.Ports) != 1 || svc.Spec.Ports[0].Port != 5433 {
		t.Errorf("ports = %+v, want 5433", svc.Spec.Ports)
	}
}
