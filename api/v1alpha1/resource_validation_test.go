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

func TestValidateKeycloakURL(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name            string
		url             string
		wantErr         bool
		wantRequiredMsg bool
		wantSchemeMsg   bool
	}{
		{name: "empty", url: "", wantErr: true, wantRequiredMsg: true},
		{name: "whitespace", url: "   ", wantErr: true, wantRequiredMsg: true},
		{name: "missing scheme", url: "keycloak.example.com", wantErr: true, wantSchemeMsg: true},
		{name: "valid Service URL", url: "http://keycloak.example.svc:8080", wantErr: false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			cfg := &CostManagementServiceConfig{}
			cfg.Name = "test"
			cfg.Spec.Auth.Keycloak.URL = tc.url

			err := cfg.validateCostManagementServiceConfig()
			if !tc.wantErr {
				if err != nil {
					t.Fatalf("valid url must not fail this check: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("expected validation error")
			}
			got := err.Error()
			if !strings.Contains(got, "spec.auth.keycloak.url") {
				t.Errorf("error should name spec.auth.keycloak.url, got %q", got)
			}
			if tc.wantRequiredMsg {
				if !strings.Contains(got, "JWKS fetch URL") {
					t.Errorf("error should mention JWKS fetch URL, got %q", got)
				}
				if !strings.Contains(got, "auto-detect") {
					t.Errorf("error should say operator does not auto-detect Keycloak, got %q", got)
				}
			}
			if tc.wantSchemeMsg {
				if !strings.Contains(got, "http://") || !strings.Contains(got, "https://") {
					t.Errorf("error should require http:// or https:// prefix, got %q", got)
				}
			}
		})
	}
}

func TestValidateCostManagementServiceConfigResources(t *testing.T) {
	t.Parallel()

	cfg := &CostManagementServiceConfig{}
	cfg.Name = "test"
	cfg.Spec.Auth.Keycloak.URL = "http://keycloak.example.svc:8080"
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
