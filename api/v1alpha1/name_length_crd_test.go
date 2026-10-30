package v1alpha1

import (
	"fmt"
	"os"
	"strings"
	"testing"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"sigs.k8s.io/yaml"
)

// TestNameLengthRuleInCRDs locks the root metadata.name CEL rule (D22) in both
// the config CRD and the OLM bundle CRD, and keeps its limits in sync with
// MaxNameLength / MaxNameLengthWithROS.
func TestNameLengthRuleInCRDs(t *testing.T) {
	t.Parallel()
	wantRule := fmt.Sprintf("? %d : %d)", MaxNameLengthWithROS, MaxNameLength)

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
			for _, r := range schema.XValidations {
				if strings.Contains(r.Rule, "size(self.metadata.name)") {
					if !strings.Contains(r.Rule, wantRule) {
						t.Fatalf("metadata.name rule %q does not use limits %q", r.Rule, wantRule)
					}
					return
				}
			}
			t.Fatal("missing root metadata.name length rule")
		})
	}
}
