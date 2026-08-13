package resources

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	costv1alpha1 "github.com/project-koku/koku-service-operator/api/v1alpha1"
)

func TestRBACEnvAPIPathPrefix(t *testing.T) {
	cfg := &costv1alpha1.CostManagementServiceConfig{}
	cfg.Name = "cost-onprem"
	cfg.Namespace = "cost-tests"

	var got string
	for _, e := range rbacEnv(cfg) {
		if e.Name == "API_PATH_PREFIX" {
			got = e.Value
			break
		}
	}
	// Must match cost-onprem-chart cost-onprem.rbac.apiPathPrefix (/api/rbac).
	if got != "/api/rbac" {
		t.Fatalf("API_PATH_PREFIX = %q, want /api/rbac", got)
	}
}

// TestRBACEnvUsesConfiguredCachePort verifies that RBAC pods use the cache
// port from spec.cache.port rather than a hardcoded 6379. Users running
// Redis/Valkey on a non-default port would silently get the wrong connection.
func TestRBACEnvUsesConfiguredCachePort(t *testing.T) {
	cfg := &costv1alpha1.CostManagementServiceConfig{
		ObjectMeta: metav1.ObjectMeta{Name: "cost-management", Namespace: "test"},
		Spec: costv1alpha1.CostManagementServiceConfigSpec{
			Cache: costv1alpha1.CacheConfig{
				Host: "my-redis.example.com",
				Port: 6380, // non-default port
			},
			RBAC: costv1alpha1.RBACConfig{
				Image: costv1alpha1.ImageSpec{Repository: "rbac", Tag: "test"},
			},
		},
	}

	dep := RBACAPIDeployment(cfg)
	var redisPort string
	for _, e := range dep.Spec.Template.Spec.Containers[0].Env {
		if e.Name == "REDIS_PORT" {
			redisPort = e.Value
			break
		}
	}
	if redisPort != "6380" {
		t.Errorf("REDIS_PORT = %q, want %q (spec.cache.port is not honoured)", redisPort, "6380")
	}
}

// TestRBACEnvDefaultsCachePort verifies that when spec.cache.port is zero,
// REDIS_PORT defaults to 6379 (the standard Redis port).
func TestRBACEnvDefaultsCachePort(t *testing.T) {
	cfg := &costv1alpha1.CostManagementServiceConfig{
		ObjectMeta: metav1.ObjectMeta{Name: "cost-management", Namespace: "test"},
		Spec: costv1alpha1.CostManagementServiceConfigSpec{
			Cache: costv1alpha1.CacheConfig{
				Host: "my-redis.example.com",
				// Port deliberately zero — should default to 6379
			},
			RBAC: costv1alpha1.RBACConfig{
				Image: costv1alpha1.ImageSpec{Repository: "rbac", Tag: "test"},
			},
		},
	}

	dep := RBACAPIDeployment(cfg)
	var redisPort string
	for _, e := range dep.Spec.Template.Spec.Containers[0].Env {
		if e.Name == "REDIS_PORT" {
			redisPort = e.Value
			break
		}
	}
	if redisPort != "6379" {
		t.Errorf("REDIS_PORT = %q, want default 6379 when spec.cache.port is unset", redisPort)
	}
}
