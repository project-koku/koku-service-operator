package v1alpha1

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/util/validation/field"
)

func TestValidateResourceRequirements(t *testing.T) {
	t.Parallel()

	path := field.NewPath("spec", "database", "resources")

	t.Run("valid requests within limits", func(t *testing.T) {
		t.Parallel()
		res := corev1.ResourceRequirements{
			Requests: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("100m"),
				corev1.ResourceMemory: resource.MustParse("128Mi"),
			},
			Limits: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("500m"),
				corev1.ResourceMemory: resource.MustParse("512Mi"),
			},
		}
		if errs := validateResourceRequirements(path, res); len(errs) != 0 {
			t.Fatalf("expected no errors, got %v", errs)
		}
	})

	t.Run("request exceeds limit", func(t *testing.T) {
		t.Parallel()
		res := corev1.ResourceRequirements{
			Requests: corev1.ResourceList{
				corev1.ResourceCPU: resource.MustParse("2"),
			},
			Limits: corev1.ResourceList{
				corev1.ResourceCPU: resource.MustParse("500m"),
			},
		}
		errs := validateResourceRequirements(path, res)
		if len(errs) != 1 {
			t.Fatalf("expected 1 error, got %d: %v", len(errs), errs)
		}
		if got := errs[0].Error(); !strings.Contains(got, "2 must be less than or equal to 500m") {
			t.Fatalf("unexpected error message: %q", got)
		}
	})

	t.Run("request without matching limit is allowed", func(t *testing.T) {
		t.Parallel()
		res := corev1.ResourceRequirements{
			Requests: corev1.ResourceList{
				corev1.ResourceCPU: resource.MustParse("100m"),
			},
		}
		if errs := validateResourceRequirements(path, res); len(errs) != 0 {
			t.Fatalf("expected no errors, got %v", errs)
		}
	})

	t.Run("empty resources", func(t *testing.T) {
		t.Parallel()
		if errs := validateResourceRequirements(path, corev1.ResourceRequirements{}); len(errs) != 0 {
			t.Fatalf("expected no errors, got %v", errs)
		}
	})
}

func TestValidateCostManagementServiceConfigResources(t *testing.T) {
	t.Parallel()

	cfg := &CostManagementServiceConfig{}
	cfg.Name = "test"
	cfg.Spec.CostManagement.API.Resources = corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceMemory: resource.MustParse("1Gi"),
		},
		Limits: corev1.ResourceList{
			corev1.ResourceMemory: resource.MustParse("512Mi"),
		},
	}

	err := cfg.validateCostManagementServiceConfig()
	if err == nil {
		t.Fatal("expected validation error")
	}
}
