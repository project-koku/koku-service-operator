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

func TestSampleCRs_ProductOauthAndEnvoyStayRedHat(t *testing.T) {
	t.Parallel()
	for _, name := range []string{sampleDefault, sampleProduction} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			cfg := loadSampleCR(t, name)
			oauth := cfg.Spec.UI.OAuthProxy.Image.Repository
			envoy := cfg.Spec.Auth.Envoy.Image.Repository
			if !strings.Contains(oauth, redhatRegistry) {
				t.Errorf("oauth2-proxy repository = %q, want %s", oauth, redhatRegistry)
			}
			if !strings.Contains(envoy, redhatRegistry) {
				t.Errorf("envoy repository = %q, want %s", envoy, redhatRegistry)
			}
		})
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

	if !BoolVal(cfg.Spec.Database.Deploy, true) {
		t.Error("community sample database.deploy: want true (bundled/dev path)")
	}
	if !BoolVal(cfg.Spec.Cache.Deploy, true) {
		t.Error("community sample cache.deploy: want true (bundled/dev path)")
	}
}
