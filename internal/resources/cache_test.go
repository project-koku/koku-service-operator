package resources

import (
	"slices"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	costv1alpha1 "github.com/project-koku/koku-service-operator/api/v1alpha1"
)

func cacheCfg() *costv1alpha1.CostManagementServiceConfig {
	cfg := testCfg()
	cfg.Spec.Global.StorageClass = "gp3-csi"
	cfg.Spec.Cache.Image.Repository = "registry.redhat.io/rhel10/valkey-8"
	cfg.Spec.Cache.Image.Tag = "10.1"
	return cfg
}

func TestCacheDeployment_Defaults(t *testing.T) {
	cfg := cacheCfg()
	d := CacheDeployment(cfg)

	if d.Name != "cost-management-valkey" {
		t.Errorf("Name = %q", d.Name)
	}
	if d.Namespace != "cost-onprem" {
		t.Errorf("Namespace = %q", d.Namespace)
	}
	if d.Spec.Strategy.Type != appsv1.RecreateDeploymentStrategyType {
		t.Errorf("Strategy = %q, want Recreate", d.Spec.Strategy.Type)
	}
	c := d.Spec.Template.Spec.Containers[0]
	if c.Name != "valkey" {
		t.Errorf("container name = %q", c.Name)
	}
	if c.Image != "registry.redhat.io/rhel10/valkey-8:10.1" {
		t.Errorf("image = %q", c.Image)
	}
	if c.Command[0] != "valkey-server" {
		t.Errorf("command = %v", c.Command)
	}
	if len(c.Ports) != 1 || c.Ports[0].ContainerPort != 6379 {
		t.Errorf("ports = %+v, want 6379", c.Ports)
	}
	wantArgs := []string{"--bind", "0.0.0.0", "--port", "6379", "--dir", "/data", "--appendonly", "yes"}
	for _, want := range wantArgs {
		if !slices.Contains(c.Args, want) {
			t.Errorf("Args missing %q: %v", want, c.Args)
		}
	}
	if d.Spec.Template.Spec.SecurityContext == nil || d.Spec.Template.Spec.SecurityContext.FSGroup == nil {
		t.Fatal("expected pod FSGroup for PVC ownership")
	}
	if *d.Spec.Template.Spec.SecurityContext.FSGroup != 1000 {
		t.Errorf("FSGroup = %d, want 1000", *d.Spec.Template.Spec.SecurityContext.FSGroup)
	}
	if len(d.Spec.Template.Spec.Volumes) != 1 || d.Spec.Template.Spec.Volumes[0].PersistentVolumeClaim == nil {
		t.Fatalf("expected PVC volume, got %+v", d.Spec.Template.Spec.Volumes)
	}
	if d.Spec.Template.Spec.Volumes[0].PersistentVolumeClaim.ClaimName != NameValkeyPVC(cfg) {
		t.Errorf("ClaimName = %q", d.Spec.Template.Spec.Volumes[0].PersistentVolumeClaim.ClaimName)
	}
}

func TestCacheDeployment_DefaultImageWhenUnset(t *testing.T) {
	cfg := testCfg()
	d := CacheDeployment(cfg)
	if got := d.Spec.Template.Spec.Containers[0].Image; got != "registry.redhat.io/rhel10/valkey-8:10.1" {
		t.Errorf("default image = %q", got)
	}
}

func TestCacheService_DefaultPort(t *testing.T) {
	cfg := cacheCfg()
	svc := CacheService(cfg)
	if svc.Name != NameValkey(cfg) {
		t.Errorf("Name = %q", svc.Name)
	}
	if len(svc.Spec.Ports) != 1 || svc.Spec.Ports[0].Port != 6379 {
		t.Errorf("ports = %+v", svc.Spec.Ports)
	}
	if svc.Spec.Selector[labelComponent] != "cache" {
		t.Errorf("selector = %v", svc.Spec.Selector)
	}
}

func TestCachePVC_DefaultSizeAndStorageClass(t *testing.T) {
	cfg := cacheCfg()
	pvc := CachePVC(cfg)
	if pvc.Name != NameValkeyPVC(cfg) {
		t.Errorf("Name = %q", pvc.Name)
	}
	want := resource.MustParse("5Gi")
	got := pvc.Spec.Resources.Requests[corev1.ResourceStorage]
	if got.Cmp(want) != 0 {
		t.Errorf("storage request = %s, want %s", got.String(), want.String())
	}
	if pvc.Spec.StorageClassName == nil || *pvc.Spec.StorageClassName != "gp3-csi" {
		t.Errorf("StorageClassName = %v", pvc.Spec.StorageClassName)
	}
	if len(pvc.Spec.AccessModes) != 1 || pvc.Spec.AccessModes[0] != corev1.ReadWriteOnce {
		t.Errorf("AccessModes = %v", pvc.Spec.AccessModes)
	}
}

func TestCachePVC_CustomSize(t *testing.T) {
	cfg := cacheCfg()
	cfg.Spec.Cache.Persistence.Size = resource.MustParse("20Gi")
	pvc := CachePVC(cfg)
	got := pvc.Spec.Resources.Requests[corev1.ResourceStorage]
	if got.Cmp(resource.MustParse("20Gi")) != 0 {
		t.Errorf("storage request = %s", got.String())
	}
}
