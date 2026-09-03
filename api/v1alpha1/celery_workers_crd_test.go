package v1alpha1

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"sigs.k8s.io/yaml"
)

const cmscCRDFile = "service.costmanagement.openshift.io_costmanagementserviceconfigs.yaml"

var saasCeleryWorkerFields = []string{"hcs", "subsExtraction", "subsTransmission"}

var crdInstallPaths = []struct {
	name string
	path string
}{
	{name: "config CRD", path: filepath.Join("..", "..", "config", "crd", "bases", cmscCRDFile)},
	{name: "OLM bundle CRD", path: filepath.Join("..", "..", "bundle", "manifests", cmscCRDFile)},
}

// TestSaaSCeleryWorkerCRDDefaultReplicas locks the apiserver defaulting contract
// for SaaS-only Celery queues. Builder unit tests only see in-memory Go zero
// values; OLM installs read bundle/manifests, not config/crd alone.
func TestSaaSCeleryWorkerCRDDefaultReplicas(t *testing.T) {
	t.Parallel()

	for _, install := range crdInstallPaths {
		t.Run(install.name, func(t *testing.T) {
			t.Parallel()

			data, err := os.ReadFile(install.path)
			if err != nil {
				t.Fatalf("read CRD %s: %v", install.path, err)
			}

			var crd apiextensionsv1.CustomResourceDefinition
			if err := yaml.Unmarshal(data, &crd); err != nil {
				t.Fatalf("unmarshal CRD: %v", err)
			}

			schema := storedOpenAPIV3Schema(&crd)
			if schema == nil {
				t.Fatal("missing openAPIV3Schema on stored CRD version")
			}

			workers := navigateSchema(t, schema, "spec", "costManagement", "celery", "workers")
			for _, field := range saasCeleryWorkerFields {
				if got := replicasDefault(t, workers, field); got != 0 {
					t.Errorf("workers.%s.replicas default = %d, want 0", field, got)
				}
			}
		})
	}
}

func TestKeycloakURLRequiredCEL(t *testing.T) {
	t.Parallel()

	for _, install := range crdInstallPaths {
		t.Run(install.name, func(t *testing.T) {
			t.Parallel()

			data, err := os.ReadFile(install.path)
			if err != nil {
				t.Fatalf("read CRD %s: %v", install.path, err)
			}

			var crd apiextensionsv1.CustomResourceDefinition
			if err := yaml.Unmarshal(data, &crd); err != nil {
				t.Fatalf("unmarshal CRD: %v", err)
			}

			schema := storedOpenAPIV3Schema(&crd)
			if schema == nil {
				t.Fatal("missing openAPIV3Schema on stored CRD version")
			}

			kc := navigateSchema(t, schema, "spec", "auth", "keycloak")
			if _, ok := kc.Properties["namespace"]; ok {
				t.Error("keycloak.namespace must not appear on the CRD")
			}

			nestedURL := findCELRule(kc.XValidations, "has(self.url)", "size(self.url)")
			if nestedURL == nil {
				t.Fatal("missing KeycloakSpec x-kubernetes-validations rule requiring non-empty url")
			}
			if !strings.Contains(nestedURL.Message, "url") {
				t.Errorf("CEL message should name url, got %q", nestedURL.Message)
			}
			if !strings.Contains(nestedURL.Message, "JWKS") {
				t.Errorf("CEL message should mention JWKS, got %q", nestedURL.Message)
			}
			if !strings.Contains(nestedURL.Message, "auto-detect") {
				t.Errorf("CEL message should say operator does not auto-detect Keycloak, got %q", nestedURL.Message)
			}

			scheme := findCELRule(kc.XValidations, "startsWith('http://')", "startsWith('https://')")
			if scheme == nil {
				t.Fatal("missing KeycloakSpec CEL rule requiring url to start with http:// or https://")
			}

			// Nested KeycloakSpec CEL does not run when spec.auth.keycloak is
			// omitted. The parent spec rule must fail that case.
			spec := navigateSchema(t, schema, "spec")
			parent := findCELRule(spec.XValidations, "has(self.auth.keycloak)", "self.auth.keycloak.url")
			if parent == nil {
				t.Fatal("missing spec-level CEL rule requiring auth.keycloak (omitted-keycloak must fail)")
			}
			if !strings.Contains(parent.Rule, "has(self.auth)") {
				t.Errorf("parent CEL must require has(self.auth) so omitting auth fails, got %q", parent.Rule)
			}
			if !strings.Contains(parent.Rule, "size(self.auth.keycloak.url)") {
				t.Errorf("parent CEL must require non-empty auth.keycloak.url, got %q", parent.Rule)
			}
			if !strings.Contains(parent.Message, "JWKS") {
				t.Errorf("parent CEL message should mention JWKS, got %q", parent.Message)
			}
			if !strings.Contains(parent.Message, "auto-detect") {
				t.Errorf("parent CEL message should say operator does not auto-detect Keycloak, got %q", parent.Message)
			}
		})
	}
}

func findCELRule(rules []apiextensionsv1.ValidationRule, mustContain ...string) *apiextensionsv1.ValidationRule {
	for i := range rules {
		ok := true
		for _, s := range mustContain {
			if !strings.Contains(rules[i].Rule, s) {
				ok = false
				break
			}
		}
		if ok {
			return &rules[i]
		}
	}
	return nil
}

func storedOpenAPIV3Schema(crd *apiextensionsv1.CustomResourceDefinition) *apiextensionsv1.JSONSchemaProps {
	for i := range crd.Spec.Versions {
		if crd.Spec.Versions[i].Storage {
			return crd.Spec.Versions[i].Schema.OpenAPIV3Schema
		}
	}
	if len(crd.Spec.Versions) == 0 {
		return nil
	}
	return crd.Spec.Versions[0].Schema.OpenAPIV3Schema
}

func navigateSchema(t *testing.T, root *apiextensionsv1.JSONSchemaProps, path ...string) *apiextensionsv1.JSONSchemaProps {
	t.Helper()

	cur := root
	for _, seg := range path {
		if cur.Properties == nil {
			t.Fatalf("missing properties while navigating to %q", seg)
		}
		next, ok := cur.Properties[seg]
		if !ok {
			t.Fatalf("missing schema property %q", seg)
		}
		cur = &next
	}
	return cur
}

func replicasDefault(t *testing.T, workers *apiextensionsv1.JSONSchemaProps, queue string) int64 {
	t.Helper()

	replicasSchema := navigateSchema(t, navigateSchema(t, workers, queue), "replicas")
	if replicasSchema.Default == nil {
		t.Fatalf("workers.%s.replicas missing OpenAPI default", queue)
	}

	var asInt int64
	if err := json.Unmarshal(replicasSchema.Default.Raw, &asInt); err == nil {
		return asInt
	}

	var asFloat float64
	if err := json.Unmarshal(replicasSchema.Default.Raw, &asFloat); err != nil {
		t.Fatalf("workers.%s.replicas default is not numeric: %v", queue, err)
	}
	return int64(asFloat)
}
