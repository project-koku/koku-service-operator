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
	sampleMinimal    = "install/service.costmanagement_v1alpha1_costmanagementserviceconfig_minimal.yaml"
	sampleProduction = "install/service.costmanagement_v1alpha1_costmanagementserviceconfig_production.yaml"
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

func TestSampleCRs_DefaultLeavesAuthKeycloakURLBlank(t *testing.T) {
	t.Parallel()
	cfg := loadSampleCR(t, sampleDefault)
	if cfg.Spec.Auth.Keycloak.URL != "" {
		t.Fatalf("default sample auth.keycloak.url = %q, want empty string", cfg.Spec.Auth.Keycloak.URL)
	}
}

func TestSampleCRs_DefaultLeavesObjectStorageValuesBlank(t *testing.T) {
	t.Parallel()
	cfg := loadSampleCR(t, sampleDefault)
	if cfg.Spec.ObjectStorage.Endpoint != "" {
		t.Fatalf("default sample objectStorage.endpoint = %q, want empty string", cfg.Spec.ObjectStorage.Endpoint)
	}
	if cfg.Spec.ObjectStorage.SecretName != "" {
		t.Fatalf("default sample objectStorage.secretName = %q, want empty string", cfg.Spec.ObjectStorage.SecretName)
	}
	if cfg.Spec.ObjectStorage.Buckets.Koku != "" {
		t.Fatalf("default sample objectStorage.buckets.koku = %q, want empty string", cfg.Spec.ObjectStorage.Buckets.Koku)
	}
	if cfg.Spec.ObjectStorage.Buckets.Ingress != "" {
		t.Fatalf("default sample objectStorage.buckets.ingress = %q, want empty string", cfg.Spec.ObjectStorage.Buckets.Ingress)
	}
}

func TestSampleCRs_DefaultShowsObjectStorageBucketsShape(t *testing.T) {
	t.Parallel()

	data, err := os.ReadFile(samplePath(sampleDefault))
	if err != nil {
		t.Fatalf("read %s: %v", sampleDefault, err)
	}
	// Decode into a generic mapping so key *presence* is distinguishable from an
	// empty value: the default template must show koku as an explicit blank
	// field while omitting the optional ingress/ros buckets. A typed decode
	// collapses "" and omitted into the same zero value, so this inspects the
	// parsed mapping keys rather than the raw text.
	var doc map[string]any
	if err := yaml.Unmarshal(data, &doc); err != nil {
		t.Fatalf("unmarshal %s: %v", sampleDefault, err)
	}

	buckets := nestedMap(t, doc, "spec", "objectStorage", "buckets")
	koku, ok := buckets["koku"]
	if !ok {
		t.Fatalf("%s must show objectStorage.buckets.koku as an explicit field", sampleDefault)
	}
	if koku != "" {
		t.Fatalf("%s objectStorage.buckets.koku = %v, want an explicit blank field", sampleDefault, koku)
	}
	// ingress and ros buckets are optional (ingress inherits koku; ros only when
	// ros.enabled). The default template omits them to avoid suggesting fields
	// users would blindly fill in.
	if _, ok := buckets["ingress"]; ok {
		t.Fatalf("%s must omit objectStorage.buckets.ingress (optional, inherits koku)", sampleDefault)
	}
	if _, ok := buckets["ros"]; ok {
		t.Fatalf("%s must omit objectStorage.buckets.ros (optional, only when ros.enabled)", sampleDefault)
	}

	// Legacy bucket fields are removed in favor of objectStorage.buckets.
	if ingress := optionalMap(doc, "spec", "ingress"); ingress != nil {
		if _, ok := ingress["stagingBucket"]; ok {
			t.Fatalf("%s must not use legacy ingress.stagingBucket", sampleDefault)
		}
	}
	if storage := optionalMap(doc, "spec", "costManagement", "storage"); storage != nil {
		if _, ok := storage["bucketName"]; ok {
			t.Fatalf("%s must not use legacy costManagement.storage.bucketName", sampleDefault)
		}
	}
}

// optionalMap walks a decoded YAML document to the mapping at the given key
// path, returning nil if any segment is absent or not a mapping (so callers can
// assert that a subtree is omitted).
func optionalMap(doc map[string]any, path ...string) map[string]any {
	cur := doc
	for _, key := range path {
		next, ok := cur[key].(map[string]any)
		if !ok {
			return nil
		}
		cur = next
	}
	return cur
}

// nestedMap is optionalMap with a fatal assertion that the mapping exists.
func nestedMap(t *testing.T, doc map[string]any, path ...string) map[string]any {
	t.Helper()
	m := optionalMap(doc, path...)
	if m == nil {
		t.Fatalf("path %q is missing or not a mapping", strings.Join(path, "."))
	}
	return m
}

func TestSampleCRs_MinimalAndProductionOmitDistinctIngressBucket(t *testing.T) {
	t.Parallel()
	// buckets.ingress inherits buckets.koku; the minimal and production samples
	// must not reintroduce a distinct upload bucket (only the _byoi sample
	// documents that override). They must still set buckets.koku so uploads and
	// reads resolve to a real bucket.
	for _, name := range []string{sampleMinimal, sampleProduction} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			cfg := loadSampleCR(t, name)
			if cfg.Spec.ObjectStorage.Buckets.Koku == "" {
				t.Errorf("%s objectStorage.buckets.koku is empty, want a koku bucket", name)
			}
			if cfg.Spec.ObjectStorage.Buckets.Ingress != "" {
				t.Errorf("%s objectStorage.buckets.ingress = %q, want empty (inherits koku)",
					name, cfg.Spec.ObjectStorage.Buckets.Ingress)
			}
		})
	}
}

func TestSampleCRs_DefaultLeavesKafkaBootstrapBlank(t *testing.T) {
	t.Parallel()
	cfg := loadSampleCR(t, sampleDefault)
	if cfg.Spec.Kafka.BootstrapServers != "" {
		t.Fatalf("default sample kafka.bootstrapServers = %q, want empty string", cfg.Spec.Kafka.BootstrapServers)
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
