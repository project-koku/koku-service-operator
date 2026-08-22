package resources

import (
	"testing"

	costv1alpha1 "github.com/project-koku/koku-service-operator/api/v1alpha1"
)

func TestImageRef(t *testing.T) {
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
			got, ok := ImageRef(tt.img)
			if got != tt.want || ok != tt.ok {
				t.Errorf("ImageRef() = %q, %v; want %q, %v", got, ok, tt.want, tt.ok)
			}
		})
	}
}

func TestKokuImage(t *testing.T) {
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
