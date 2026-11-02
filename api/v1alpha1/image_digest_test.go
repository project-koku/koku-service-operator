package v1alpha1

import (
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"sigs.k8s.io/yaml"
)

// Both installation paths must accept immutable pins without a tag and enforce
// digest syntax. Check the typed schema rather than searching generated YAML.
func TestImageDigestCRDContract(t *testing.T) {
	t.Parallel()
	for _, install := range crdInstallPaths {
		t.Run(install.name, func(t *testing.T) {
			t.Parallel()
			data, err := os.ReadFile(install.path)
			if err != nil {
				t.Fatal(err)
			}
			var crd apiextensionsv1.CustomResourceDefinition
			if err := yaml.Unmarshal(data, &crd); err != nil {
				t.Fatal(err)
			}
			schema := storedOpenAPIV3Schema(&crd)
			if schema == nil {
				t.Fatal("missing stored version schema")
			}
			for _, path := range [][]string{
				{"spec", "costManagement", "api", "image"}, {"spec", "costManagement", "masu", "image"},
				{"spec", "rbac", "image"}, {"spec", "ros", "image"}, {"spec", "ui", "app", "image"},
				{"spec", "auth", "envoy", "image"}, {"spec", "ingress", "image"},
			} {
				image := navigateSchema(t, schema, path...)
				if slices.Contains(image.Required, "tag") {
					t.Errorf("%s: digest-only pins must not require a tag", strings.Join(path, "."))
				}
				digest, ok := image.Properties["digest"]
				if !ok || digest.Type != "string" || digest.Pattern == "" {
					t.Fatalf("%s: missing typed digest validation", strings.Join(path, "."))
				}
				pattern := regexp.MustCompile(digest.Pattern)
				if !pattern.MatchString("sha256:"+strings.Repeat("a", 64)) || pattern.MatchString("sha256:invalid") {
					t.Errorf("%s: incorrect digest validation %q", strings.Join(path, "."), digest.Pattern)
				}
			}
		})
	}
}
