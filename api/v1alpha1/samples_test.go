package v1alpha1

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

const (
	sampleDefault    = "service.costmanagement_v1alpha1_costmanagementserviceconfig.yaml"
	sampleProduction = "service.costmanagement_v1alpha1_costmanagementserviceconfig_production.yaml"
	sampleCommunity  = "service.costmanagement_v1alpha1_costmanagementserviceconfig_community.yaml"

	redhatRegistry = "registry.redhat.io"
)

func samplePath(name string) string {
	return filepath.Join("..", "..", "config", "samples", name)
}

func loadSampleCR(t *testing.T, name string) *CostManagementServiceConfig {
	t.Helper()
	data, err := os.ReadFile(samplePath(name))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	var cfg CostManagementServiceConfig
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		t.Fatalf("unmarshal %s: %v", name, err)
	}
	if cfg.APIVersion != "service.costmanagement.openshift.io/v1alpha1" {
		t.Fatalf("%s apiVersion = %q", name, cfg.APIVersion)
	}
	if cfg.Kind != "CostManagementServiceConfig" {
		t.Fatalf("%s kind = %q", name, cfg.Kind)
	}
	return &cfg
}

func TestSampleCRs_DefaultAndProductionOauthAndEnvoyImages(t *testing.T) {
	t.Parallel()
	for _, name := range []string{sampleDefault, sampleProduction} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			cfg := loadSampleCR(t, name)
			oauth := cfg.Spec.UI.OAuthProxy.Image.Repository
			envoy := cfg.Spec.Auth.Envoy.Image.Repository
			if !strings.HasPrefix(oauth, redhatRegistry+"/") {
				t.Errorf("oauth2-proxy repository = %q, want %s", oauth, redhatRegistry)
			}
			if !strings.HasPrefix(envoy, redhatRegistry+"/") {
				t.Errorf("envoy repository = %q, want %s", envoy, redhatRegistry)
			}
		})
	}
}

func TestSampleCRs_ProductionDoesNotBundleDBCache(t *testing.T) {
	t.Parallel()
	cfg := loadSampleCR(t, sampleProduction)
	if BoolVal(cfg.Spec.Database.Deploy, true) {
		t.Error("production database.deploy: want false (BYOI)")
	}
	if BoolVal(cfg.Spec.Cache.Deploy, true) {
		t.Error("production cache.deploy: want false (BYOI)")
	}
}

func TestSampleCRs_DefaultDoesNotBundleDBCache(t *testing.T) {
	t.Parallel()
	cfg := loadSampleCR(t, sampleDefault)
	if BoolVal(cfg.Spec.Database.Deploy, true) {
		t.Error("default sample database.deploy: want false (BYOI)")
	}
	if BoolVal(cfg.Spec.Cache.Deploy, true) {
		t.Error("default sample cache.deploy: want false (BYOI)")
	}
}

func TestSampleCRs_DefaultLeavesExternalDBCacheValuesBlank(t *testing.T) {
	t.Parallel()
	cfg := loadSampleCR(t, sampleDefault)
	if cfg.Spec.Database.Host != "" {
		t.Errorf("default sample database.host = %q, want empty string", cfg.Spec.Database.Host)
	}
	if cfg.Spec.Database.SecretName != "" {
		t.Errorf("default sample database.secretName = %q, want empty string", cfg.Spec.Database.SecretName)
	}
	if cfg.Spec.Cache.Host != "" {
		t.Errorf("default sample cache.host = %q, want empty string", cfg.Spec.Cache.Host)
	}
	if cfg.Spec.Cache.Auth.SecretName != "" {
		t.Errorf("default sample cache.auth.secretName = %q, want empty string", cfg.Spec.Cache.Auth.SecretName)
	}
}

func TestSampleCRs_CommunityPublicImages(t *testing.T) {
	t.Parallel()
	cfg := loadSampleCR(t, sampleCommunity)

	oauth := cfg.Spec.UI.OAuthProxy.Image
	if strings.Contains(oauth.Repository, redhatRegistry) {
		t.Errorf("community oauth2-proxy repository = %q, must not use %s", oauth.Repository, redhatRegistry)
	}
	if oauth.Repository != "quay.io/oauth2-proxy/oauth2-proxy" || oauth.Tag != "v7.6.0" {
		t.Errorf("community oauth2-proxy = %s:%s, want quay.io/oauth2-proxy/oauth2-proxy:v7.6.0", oauth.Repository, oauth.Tag)
	}

	envoy := cfg.Spec.Auth.Envoy.Image
	if strings.Contains(envoy.Repository, redhatRegistry) {
		t.Errorf("community envoy repository = %q, must not use %s", envoy.Repository, redhatRegistry)
	}
	if envoy.Repository != "docker.io/envoyproxy/envoy" || envoy.Tag != "v1.32.13" {
		t.Errorf("community envoy = %s:%s, want docker.io/envoyproxy/envoy:v1.32.13", envoy.Repository, envoy.Tag)
	}

	db := cfg.Spec.Database.Image
	if strings.Contains(db.Repository, redhatRegistry) {
		t.Errorf("community database repository = %q, must not use %s", db.Repository, redhatRegistry)
	}
	if strings.Contains(db.Repository, "docker.io/library/postgres") {
		t.Errorf("community database repository = %q is not SCL-compatible with DatabaseStatefulSet", db.Repository)
	}
	if db.Repository != "quay.io/sclorg/postgresql-16-c10s" || db.Tag != "c10s" {
		t.Errorf("community database = %s:%s, want quay.io/sclorg/postgresql-16-c10s:c10s", db.Repository, db.Tag)
	}

	cache := cfg.Spec.Cache.Image
	if strings.Contains(cache.Repository, redhatRegistry) {
		t.Errorf("community cache repository = %q, must not use %s", cache.Repository, redhatRegistry)
	}
	if strings.Contains(cache.Repository, "docker.io/valkey/valkey") {
		t.Errorf("community cache repository = %q does not match operator fsGroup 1000", cache.Repository)
	}
	if cache.Repository != "quay.io/sclorg/valkey-8-c10s" || cache.Tag != "c10s" {
		t.Errorf("community cache = %s:%s, want quay.io/sclorg/valkey-8-c10s:c10s", cache.Repository, cache.Tag)
	}

	if !BoolVal(cfg.Spec.Database.Deploy, true) {
		t.Error("community sample database.deploy: want true (bundled/dev path)")
	}
	if !BoolVal(cfg.Spec.Cache.Deploy, true) {
		t.Error("community sample cache.deploy: want true (bundled/dev path)")
	}
}
