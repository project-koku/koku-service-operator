package resources

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/yaml"

	costv1alpha1 "github.com/project-koku/koku-service-operator/api/v1alpha1"
)

func TestImageRef(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		img  costv1alpha1.ImageSpec
		want string
		ok   bool
	}{
		{"both set", costv1alpha1.ImageSpec{Repository: "quay.io/example/koku", Tag: "abc123"}, "quay.io/example/koku:abc123", true},
		{"empty", costv1alpha1.ImageSpec{}, "", false},
		{"repo only", costv1alpha1.ImageSpec{Repository: "quay.io/example/koku"}, "", false},
		{"tag only", costv1alpha1.ImageSpec{Tag: "abc123"}, "", false},
		{"whitespace", costv1alpha1.ImageSpec{Repository: "  ", Tag: " v1 "}, "", false},
		{"trimmed", costv1alpha1.ImageSpec{Repository: " quay.io/example/koku ", Tag: " v1 "}, "quay.io/example/koku:v1", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, ok := ImageRef(tt.img)
			if got != tt.want || ok != tt.ok {
				t.Errorf("ImageRef() = %q, %v; want %q, %v", got, ok, tt.want, tt.ok)
			}
		})
	}
}

func TestKokuImage(t *testing.T) {
	t.Parallel()
	cfg := &costv1alpha1.CostManagementServiceConfig{}
	if _, ok := KokuImage(cfg); ok {
		t.Fatal("empty API image should be unset")
	}
	cfg.Spec.CostManagement.API.Image = costv1alpha1.ImageSpec{
		Repository: "quay.io/example/koku",
		Tag:        "abc123",
	}
	got, ok := KokuImage(cfg)
	if !ok || got != "quay.io/example/koku:abc123" {
		t.Errorf("KokuImage() = %q, %v", got, ok)
	}
}

func TestMissingWorkloadImages_EmptyCR(t *testing.T) {
	t.Parallel()
	missing := MissingWorkloadImages(&costv1alpha1.CostManagementServiceConfig{})
	want := []string{
		"spec.database.image",
		"spec.cache.image",
		"spec.auth.envoy.image",
		"spec.ui.oauthProxy.image",
		"spec.ui.app.image",
		"spec.costManagement.api.image",
		"spec.rbac.image",
		"spec.ingress.image",
	}
	if !slices.Equal(missing, want) {
		t.Errorf("missing = %v, want %v", missing, want)
	}
}

func TestMissingWorkloadImages_BYOISkipsBundledDBCache(t *testing.T) {
	t.Parallel()
	f := false
	cfg := &costv1alpha1.CostManagementServiceConfig{
		Spec: costv1alpha1.CostManagementServiceConfigSpec{
			Database: costv1alpha1.DatabaseConfig{Deploy: &f},
			Cache:    costv1alpha1.CacheConfig{Deploy: &f},
			Auth: costv1alpha1.AuthConfig{
				Envoy: costv1alpha1.EnvoySpec{Image: costv1alpha1.ImageSpec{Repository: "e", Tag: "1"}},
			},
			UI: costv1alpha1.UIConfig{
				OAuthProxy: costv1alpha1.OAuthProxySpec{Image: costv1alpha1.ImageSpec{Repository: "o", Tag: "1"}},
				App:        costv1alpha1.UIAppSpec{Image: costv1alpha1.ImageSpec{Repository: "u", Tag: "1"}},
			},
			CostManagement: costv1alpha1.CostManagementConfig{
				API: costv1alpha1.KokuAPISpec{Image: costv1alpha1.ImageSpec{Repository: "k", Tag: "1"}},
			},
			RBAC:    costv1alpha1.RBACConfig{Image: costv1alpha1.ImageSpec{Repository: "r", Tag: "1"}},
			Ingress: costv1alpha1.IngressConfig{Image: costv1alpha1.ImageSpec{Repository: "i", Tag: "1"}},
		},
	}
	if missing := MissingWorkloadImages(cfg); len(missing) != 0 {
		t.Errorf("BYOI with pins set: missing = %v", missing)
	}
}

func TestMissingWorkloadImages_ROSRequiresImages(t *testing.T) {
	t.Parallel()
	on := true
	cfg := &costv1alpha1.CostManagementServiceConfig{
		ObjectMeta: metav1.ObjectMeta{Name: "cm"},
		Spec: costv1alpha1.CostManagementServiceConfigSpec{
			ROS: costv1alpha1.ROSConfig{Enabled: &on},
		},
	}
	missing := MissingWorkloadImages(cfg)
	if !slices.Contains(missing, "spec.ros.image") || !slices.Contains(missing, "spec.kruize.image") {
		t.Errorf("ROS enabled missing = %v, want spec.ros.image and spec.kruize.image", missing)
	}
}

func TestSampleCRs_RequiredWorkloadImages(t *testing.T) {
	t.Parallel()
	samples := []string{
		"config/samples/service.costmanagement_v1alpha1_costmanagementserviceconfig.yaml",
		"config/samples/service.costmanagement_v1alpha1_costmanagementserviceconfig_production.yaml",
		"config/samples/service.costmanagement_v1alpha1_costmanagementserviceconfig_community.yaml",
		"config/samples/service.costmanagement_v1alpha1_costmanagementserviceconfig_minimal.yaml",
		"config/samples/service.costmanagement_v1alpha1_costmanagementserviceconfig_byoi.yaml",
		"config/samples/byoi/app/costmanagementserviceconfig.yaml",
		"config/samples/byoi/app/costmanagementserviceconfig-smoke.yaml",
	}
	root := filepath.Join("..", "..")
	for _, rel := range samples {
		t.Run(rel, func(t *testing.T) {
			t.Parallel()
			data, err := os.ReadFile(filepath.Join(root, rel))
			if err != nil {
				t.Fatalf("read: %v", err)
			}
			var cfg costv1alpha1.CostManagementServiceConfig
			if err := yaml.Unmarshal(data, &cfg); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if missing := MissingWorkloadImages(&cfg); len(missing) > 0 {
				t.Errorf("sample is missing required images: %v", missing)
			}
		})
	}
}
