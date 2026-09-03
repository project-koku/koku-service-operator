package rbac_seed

import (
	"fmt"
	"os"
	"strings"
	"testing"
)

func TestCostManagementSeedPythonMatchesEmbeddedSnapshots(t *testing.T) {
	script, err := CostManagementSeedPython()
	if err != nil {
		t.Fatal(err)
	}

	cmPerms, err := costManagementPermissionTuples()
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range cmPerms {
		want := fmt.Sprintf("(%q, %q)", p.resource, p.verb)
		if !strings.Contains(script, want) {
			t.Errorf("seed script missing cost-management permission tuple %s", want)
		}
	}

	srcPerms, err := sourcesPermissionTuples()
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range srcPerms {
		want := fmt.Sprintf("(%q, %q)", p.resource, p.verb)
		if !strings.Contains(script, want) {
			t.Errorf("seed script missing sources permission tuple %s", want)
		}
	}

	roles, err := roleSeedTuples()
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range roles {
		want := roleTuplePython(r)
		if !strings.Contains(script, want) {
			t.Errorf("seed script missing role tuple %s", want)
		}
	}
}

func TestCostManagementSeedPythonUsesPythonBoolLiterals(t *testing.T) {
	script, err := CostManagementSeedPython()
	if err != nil {
		t.Fatal(err)
	}

	const costAdminTuple = `("Cost Administrator", "Perform any available operation on cost management resources.", True, False, [("cost-management", "*", "*")])`
	if !strings.Contains(script, costAdminTuple) {
		t.Errorf("seed script missing Cost Administrator tuple with Python bool literals:\n%s", costAdminTuple)
	}

	start := strings.Index(script, "roles = [")
	end := strings.Index(script, "]\n\nrole_count")
	if start < 0 || end < 0 || end <= start {
		t.Fatal("could not locate roles list in generated script")
	}
	rolesSection := script[start:end]
	for _, bad := range []string{", true,", ", false,"} {
		if strings.Contains(rolesSection, bad) {
			t.Errorf("roles list contains Go bool literal %q", bad)
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
