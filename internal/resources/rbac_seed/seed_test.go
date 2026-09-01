package rbac_seed

import (
	"net/http"
	"os"
	"strings"
	"testing"
)

func TestCostManagementSeedPythonContainsExpected(t *testing.T) {
	script, err := CostManagementSeedPython()
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"sources:*:*",
		"Cost Administrator",
		"Sources administrator",
		`("cost-management", "*", "*")`,
		`("sources", "*", "*")`,
		"admin_default",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("seed script missing %q", want)
		}
	}
}

func TestEmbeddedMatchesUpstreamRbacConfig(t *testing.T) {
	if os.Getenv("RBAC_SEED_SKIP_UPSTREAM") == "1" {
		t.Skip("RBAC_SEED_SKIP_UPSTREAM=1")
	}
	if err := CheckEmbeddedMatchesUpstream(http.DefaultClient); err != nil {
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
