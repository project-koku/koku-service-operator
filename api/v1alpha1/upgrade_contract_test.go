package v1alpha1

import (
	"os"
	"slices"
	"testing"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"sigs.k8s.io/yaml"
)

func TestUpgradeCRDContract(t *testing.T) {
	for _, install := range crdInstallPaths {
		t.Run(install.name, func(t *testing.T) {
			data, err := os.ReadFile(install.path)
			if err != nil {
				t.Fatal(err)
			}
			var crd apiextensionsv1.CustomResourceDefinition
			if err := yaml.Unmarshal(data, &crd); err != nil {
				t.Fatal(err)
			}
			schema := storedOpenAPIV3Schema(&crd)
			policy := navigateSchema(t, schema, "spec", "upgrade", "imagePolicy")
			if string(policy.Default.Raw) != `"Pinned"` {
				t.Fatal("existing installations must remain pinned by default")
			}
			if len(policy.Enum) != 2 {
				t.Fatal("image ownership policy must be constrained")
			}
			operation := navigateSchema(t, schema, "status", "upgrade", "operation")
			if !slices.Contains(operation.Required, "targetGeneration") {
				t.Fatal("frozen target generation missing")
			}
			snapshot := navigateSchema(t, schema, "status", "upgrade", "operation", "target", "spec")
			if snapshot.XPreserveUnknownFields == nil || !*snapshot.XPreserveUnknownFields {
				t.Fatal("frozen spec can be pruned by API server")
			}
		})
	}
}
