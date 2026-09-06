package rbac_seed

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func TestPermissionsConfigMapData(t *testing.T) {
	data, err := PermissionsConfigMapData()
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"cost-management.json", "sources.json"} {
		raw, ok := data[key]
		if !ok || raw == "" {
			t.Fatalf("missing %s", key)
		}
		if !json.Valid([]byte(raw)) {
			t.Fatalf("%s is not valid JSON", key)
		}
	}
}

func TestDefinitionsConfigMapData(t *testing.T) {
	data, err := DefinitionsConfigMapData()
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"cost-management.json", "sources.json"} {
		raw, ok := data[key]
		if !ok || raw == "" {
			t.Fatalf("missing %s", key)
		}
		if !json.Valid([]byte(raw)) {
			t.Fatalf("%s is not valid JSON", key)
		}
	}
	for _, want := range []string{"Cost Administrator", "Sources administrator", "admin_default"} {
		combined := data["cost-management.json"] + data["sources.json"]
		if !strings.Contains(combined, want) {
			t.Errorf("definitions missing %q", want)
		}
	}
}

func TestEmbeddedMatchesUpstreamRbacConfig(t *testing.T) {
	if os.Getenv("RBAC_SEED_SKIP_UPSTREAM") == "1" {
		t.Skip("RBAC_SEED_SKIP_UPSTREAM=1")
	}
	if err := CheckEmbeddedMatchesUpstream(nil); err != nil {
		t.Fatal(err)
	}
}

func TestConfigRefSet(t *testing.T) {
	ref, err := ConfigRef()
	if err != nil {
		t.Fatal(err)
	}
	if len(ref) != 40 {
		t.Fatalf("ConfigRef = %q, want 40-char commit SHA", ref)
	}
}
