package controller

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/util/yaml"
)

func rbacManifestPath(t *testing.T, name string) string {
	t.Helper()
	return filepath.Join("..", "..", "config", "rbac", name)
}

func bundleCSVPath(t *testing.T) string {
	t.Helper()
	return filepath.Join("..", "..", "bundle", "manifests", "koku-service-operator.clusterserviceversion.yaml")
}

func decodeYAMLFile(t *testing.T, path string, into any) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if err := yaml.Unmarshal(data, into); err != nil {
		t.Fatalf("decode %s: %v", path, err)
	}
}

// olmCSVInstallPermissions is the CSV fragment we round-trip. Avoids adding
// operator-framework/api just to inspect install.spec.{permissions,clusterPermissions}.
type olmCSVInstallPermissions struct {
	Spec struct {
		Install struct {
			Spec struct {
				ClusterPermissions []olmCSVPermissionRules `json:"clusterPermissions"`
				Permissions        []olmCSVPermissionRules `json:"permissions"`
			} `json:"spec"`
		} `json:"install"`
	} `json:"spec"`
}

type olmCSVPermissionRules struct {
	Rules []rbacv1.PolicyRule `json:"rules"`
}

func csvPolicyRules(perms []olmCSVPermissionRules) []rbacv1.PolicyRule {
	var rules []rbacv1.PolicyRule
	for _, p := range perms {
		rules = append(rules, p.Rules...)
	}
	return rules
}

func assertObjectBucketClaimGetList(t *testing.T, source string, rules []rbacv1.PolicyRule) {
	t.Helper()
	const (
		wantGroup    = "objectbucket.io"
		wantResource = "objectbucketclaims"
	)

	var found bool
	for _, rule := range rules {
		if !slices.Contains(rule.APIGroups, wantGroup) {
			continue
		}
		if !slices.Contains(rule.Resources, wantResource) {
			continue
		}

		hasGet, hasList := false, false
		for _, v := range rule.Verbs {
			switch v {
			case "get":
				hasGet = true
			case "list":
				hasList = true
			default:
				t.Errorf("%s objectbucketclaims rule must not include verb %q: %+v", source, v, rule)
			}
		}
		if !hasGet || !hasList {
			t.Errorf("%s objectbucketclaims rule missing get and/or list: %+v", source, rule)
			continue
		}
		found = true
	}
	if !found {
		t.Fatalf("%s must grant get+list on objectbucket.io/objectbucketclaims", source)
	}
}

func TestManagerRoleBinding_IsClusterRoleBinding(t *testing.T) {
	var rb rbacv1.ClusterRoleBinding
	decodeYAMLFile(t, rbacManifestPath(t, "role_binding.yaml"), &rb)
	if rb.Kind != "ClusterRoleBinding" {
		t.Fatalf("manager binding kind: got %q, want ClusterRoleBinding (AllNamespaces)", rb.Kind)
	}
	if rb.RoleRef.Kind != "ClusterRole" || rb.RoleRef.Name != "manager-role" {
		t.Fatalf("roleRef: got %+v, want ClusterRole/manager-role", rb.RoleRef)
	}
}

func TestClusterAccessRole_NarrowNooBaaSecretGet(t *testing.T) {
	var cr rbacv1.ClusterRole
	decodeYAMLFile(t, rbacManifestPath(t, "cluster_access_role.yaml"), &cr)
	if cr.Name != "manager-cluster-role" {
		t.Fatalf("cluster access role name: got %q", cr.Name)
	}

	var foundNoobaa bool
	for _, rule := range cr.Rules {
		for _, res := range rule.Resources {
			if res != "secrets" {
				continue
			}
			// Blanket secrets list/watch must not appear on the cluster role.
			for _, v := range rule.Verbs {
				if v == "list" || v == "watch" || v == "create" || v == "delete" || v == "update" || v == "patch" {
					t.Errorf("cluster access secrets rule must not include verb %q: %+v", v, rule)
				}
			}
			if len(rule.ResourceNames) == 1 && rule.ResourceNames[0] == "noobaa-admin" {
				foundNoobaa = true
				hasGet := false
				for _, v := range rule.Verbs {
					if v == "get" {
						hasGet = true
					}
				}
				if !hasGet {
					t.Errorf("noobaa-admin rule missing get: %+v", rule)
				}
			} else if len(rule.ResourceNames) == 0 {
				t.Errorf("cluster access must not grant unnamed secrets: %+v", rule)
			}
		}
	}
	if !foundNoobaa {
		t.Fatal("expected secrets get with resourceNames=[noobaa-admin] on manager-cluster-role")
	}
}

func TestManagerRole_GrantsObjectBucketClaimGetList(t *testing.T) {
	var cr rbacv1.ClusterRole
	decodeYAMLFile(t, rbacManifestPath(t, "role.yaml"), &cr)
	if cr.Name != "manager-role" {
		t.Fatalf("manager role name: got %q", cr.Name)
	}
	assertObjectBucketClaimGetList(t, "manager-role", cr.Rules)

	var clusterCR rbacv1.ClusterRole
	decodeYAMLFile(t, rbacManifestPath(t, "cluster_access_role.yaml"), &clusterCR)
	for _, rule := range clusterCR.Rules {
		if slices.Contains(rule.APIGroups, "objectbucket.io") {
			t.Errorf("manager-cluster-role must not grant objectbucket.io: %+v", rule)
		}
	}

	// OLM installs from the CSV, not role.yaml. AllNamespaces binds
	// manager-role via ClusterRoleBinding, so OBC get+list lives in
	// clusterPermissions. CSV permissions stay leader-election (namespaced).
	var csv olmCSVInstallPermissions
	decodeYAMLFile(t, bundleCSVPath(t), &csv)
	if len(csv.Spec.Install.Spec.ClusterPermissions) == 0 {
		t.Fatal("CSV spec.install.spec.clusterPermissions is empty (unmarshal failed or field moved)")
	}
	assertObjectBucketClaimGetList(t, "CSV clusterPermissions", csvPolicyRules(csv.Spec.Install.Spec.ClusterPermissions))
}

// TestManagerRole_SecretsNoListWatch pins the AllNamespaces least-privilege
// contract: the cluster-wide ClusterRoleBinding grants get + CRUD on unnamed
// Secrets (needed to manage operands in any CMSC namespace) but NOT list or
// watch. Without list/watch a compromised operator cannot enumerate or stream
// Secret contents cluster-wide — it must already know a name. This depends on
// the manager not running a Secret informer (no Owns(&corev1.Secret{}); cache
// DisableFor Secret — see cmd/main.go and SetupWithManager).
func TestManagerRole_SecretsNoListWatch(t *testing.T) {
	var cr rbacv1.ClusterRole
	decodeYAMLFile(t, rbacManifestPath(t, "role.yaml"), &cr)
	if cr.Name != "manager-role" {
		t.Fatalf("manager role name: got %q", cr.Name)
	}
	var foundSecrets bool
	for _, rule := range cr.Rules {
		if len(rule.ResourceNames) != 0 || !slices.Contains(rule.Resources, "secrets") {
			continue
		}
		foundSecrets = true
		for _, v := range rule.Verbs {
			if v == "list" || v == "watch" {
				t.Errorf("manager-role unnamed secrets rule must not include %q (no cluster-wide Secret enumeration): %+v", v, rule)
			}
		}
		for _, want := range []string{"get", "create", "update", "patch", "delete"} {
			if !slices.Contains(rule.Verbs, want) {
				t.Errorf("manager-role unnamed secrets rule missing verb %q: %+v", want, rule)
			}
		}
	}
	if !foundSecrets {
		t.Fatal("manager-role must grant get+CRUD on unnamed secrets (AllNamespaces ClusterRoleBinding)")
	}

	// OLM installs from the CSV. AllNamespaces binds manager-role cluster-wide,
	// so the unnamed-secrets rule lands in clusterPermissions and must carry the
	// same grant exactly — present, with get+CRUD, and no list/watch. assertExactVerbs
	// requires the rule (fails if absent) and rejects any extra verb, so an omitted
	// secrets grant or a smuggled list/watch both break the build.
	var csv olmCSVInstallPermissions
	decodeYAMLFile(t, bundleCSVPath(t), &csv)
	assertExactVerbs(t, "CSV clusterPermissions", "", "secrets",
		csvPolicyRules(csv.Spec.Install.Spec.ClusterPermissions),
		"get", "create", "update", "patch", "delete")
}

// ruleGrants reports whether a PolicyRule applies to the requested (group,
// resource). Kubernetes RBAC treats "*" in apiGroups/resources as matching any
// value, so a wildcard rule really does grant the pair — the helpers must see it
// that way or a wildcard grant slips past assertNoGrant / hides verbs from
// assertExactVerbs.
func ruleGrants(rule rbacv1.PolicyRule, group, resource string) bool {
	matchGroup := slices.Contains(rule.APIGroups, group) || slices.Contains(rule.APIGroups, "*")
	matchResource := slices.Contains(rule.Resources, resource) || slices.Contains(rule.Resources, "*")
	return matchGroup && matchResource
}

// verbSet returns the union of verbs granted on the (group, resource) pair
// across rules with no resourceNames restriction. Wildcard rules count.
func verbSet(rules []rbacv1.PolicyRule, group, resource string) map[string]bool {
	out := map[string]bool{}
	for _, rule := range rules {
		if len(rule.ResourceNames) != 0 {
			continue
		}
		if !ruleGrants(rule, group, resource) {
			continue
		}
		for _, v := range rule.Verbs {
			out[v] = true
		}
	}
	return out
}

// assertExactVerbs fails if group/resource is absent, missing a wanted verb, or
// carries any verb beyond want. Exact matching is the point: a widened grant
// (an added list/watch/update) must break the build, not slip through.
func assertExactVerbs(t *testing.T, source, group, resource string, rules []rbacv1.PolicyRule, want ...string) {
	t.Helper()
	got := verbSet(rules, group, resource)
	if len(got) == 0 {
		t.Fatalf("%s: no rule grants %s/%s", source, group, resource)
	}
	wantSet := map[string]bool{}
	for _, w := range want {
		wantSet[w] = true
		if !got[w] {
			t.Errorf("%s: %s/%s missing verb %q (want exactly %v)", source, group, resource, w, want)
		}
	}
	for g := range got {
		if !wantSet[g] {
			t.Errorf("%s: %s/%s has unexpected verb %q (want exactly %v)", source, group, resource, g, want)
		}
	}
}

// assertNoGrant fails if any rule grants the (group, resource) pair, including
// via a wildcard apiGroups/resources entry.
func assertNoGrant(t *testing.T, source, group, resource string, rules []rbacv1.PolicyRule) {
	t.Helper()
	for _, rule := range rules {
		if ruleGrants(rule, group, resource) {
			t.Errorf("%s must not grant %s/%s (no code path constructs one): %+v", source, group, resource, rule)
		}
	}
}

// TestManagerRole_TierAGrantsTrimmed locks the Tier A attack-surface trims,
// checked in role.yaml AND the CSV clusterPermissions (what OLM actually
// installs — a rule silently missing from the CSV must still fail):
//
//   - roles/rolebindings: no grant at all. Nothing constructs a namespaced Role
//     or RoleBinding; under the cluster-wide ClusterRoleBinding this would be an
//     unused escalation primitive (mint bindings in any namespace).
//   - servicemonitors/prometheusrules: Server-Side Apply + delete only, so
//     create;delete;get;patch — no list;watch (no cluster-wide enumeration) and
//     no update (SSA uses patch).
//   - costmanagementserviceconfigs (the owned CR): watched via For() and Updated
//     only to toggle the finalizer, so get;list;watch;update. Users create,
//     delete, and edit the CR — the operator does not.
func TestManagerRole_TierAGrantsTrimmed(t *testing.T) {
	var cr rbacv1.ClusterRole
	decodeYAMLFile(t, rbacManifestPath(t, "role.yaml"), &cr)
	if cr.Name != "manager-role" {
		t.Fatalf("manager role name: got %q", cr.Name)
	}

	var csv olmCSVInstallPermissions
	decodeYAMLFile(t, bundleCSVPath(t), &csv)
	csvRules := csvPolicyRules(csv.Spec.Install.Spec.ClusterPermissions)
	if len(csvRules) == 0 {
		t.Fatal("CSV clusterPermissions empty (unmarshal failed or field moved)")
	}

	for _, tc := range []struct {
		source string
		rules  []rbacv1.PolicyRule
	}{
		{"manager-role", cr.Rules},
		{"CSV clusterPermissions", csvRules},
	} {
		// A1: no namespaced roles/rolebindings grant. (clusterroles/
		// clusterrolebindings are separate exact strings and stay — Kruize.)
		assertNoGrant(t, tc.source, "rbac.authorization.k8s.io", "roles", tc.rules)
		assertNoGrant(t, tc.source, "rbac.authorization.k8s.io", "rolebindings", tc.rules)

		// A2: monitoring kinds are SSA-applied — no list/watch/update.
		assertExactVerbs(t, tc.source, "monitoring.coreos.com", "servicemonitors", tc.rules, "create", "delete", "get", "patch")
		assertExactVerbs(t, tc.source, "monitoring.coreos.com", "prometheusrules", tc.rules, "create", "delete", "get", "patch")

		// A3: the owned CR — watch + finalizer Update only.
		assertExactVerbs(t, tc.source, "service.costmanagement.openshift.io", "costmanagementserviceconfigs", tc.rules, "get", "list", "watch", "update")
	}
}

// clusterScopedResources belong in cluster_access_role.yaml. manager-role is
// bound cluster-wide (AllNamespaces), so cluster-scoped kinds in role.yaml
// would be a real cluster grant — keep them in cluster_access_role.yaml.
//
// This is a denylist, not the complete K8s cluster-scoped set (that is only
// knowable via live API discovery). The first group is the kinds the operator
// actually touches; the second is high-value cluster-scoped kinds the operator
// must NEVER grant, listed so an accidental widening is caught even though no
// current code path emits them.
var clusterScopedResources = map[string]struct{}{
	"consolelinks":        {},
	"clusterroles":        {},
	"clusterrolebindings": {},
	"storageclasses":      {},
	// noobaa-admin is a Secret resourceName, not a resource — see
	// clusterScopedViolations. The CLAUDE.md grep uses noobaa-admin
	// for the same reason.

	// Must-never-grant cluster-scoped kinds (no operator code path builds one).
	"nodes":                     {},
	"namespaces":                {},
	"persistentvolumes":         {},
	"customresourcedefinitions": {},
}

// exclusivelyClusterScopedAPIGroups have no namespaced resources. A
// resources=["*"] grant on these groups is equivalent to naming the
// cluster-scoped kinds.
var exclusivelyClusterScopedAPIGroups = map[string]struct{}{
	"console.openshift.io":  {},
	openShiftConfigAPIGroup: {},
	"storage.k8s.io":        {},
}

func clusterScopedViolations(rules []rbacv1.PolicyRule) []string {
	var out []string
	for _, rule := range rules {
		if slices.Contains(rule.ResourceNames, "noobaa-admin") {
			out = append(out, fmt.Sprintf("noobaa-admin resourceName (belongs in cluster_access_role.yaml): %+v", rule))
		}

		wildcardGroup := slices.Contains(rule.APIGroups, "*")
		if slices.Contains(rule.Resources, "*") {
			for _, g := range rule.APIGroups {
				_, exclusive := exclusivelyClusterScopedAPIGroups[g]
				// rbac.authorization.k8s.io also has namespaced roles/rolebindings
				// in role.yaml; "*" on that group would include clusterroles.
				if exclusive || g == "*" || g == "rbac.authorization.k8s.io" {
					out = append(out, fmt.Sprintf("wildcard resources on API group %q (belongs in cluster_access_role.yaml): %+v", g, rule))
				}
			}
		}

		for _, res := range rule.Resources {
			if _, forbidden := clusterScopedResources[res]; forbidden {
				out = append(out, fmt.Sprintf("cluster-scoped resource %q (belongs in cluster_access_role.yaml): %+v", res, rule))
			}
			// OpenShift Ingress/cluster is cluster-scoped; networking.k8s.io
			// Ingress is namespaced and would be fine in role.yaml.
			if res == "ingresses" && (slices.Contains(rule.APIGroups, openShiftConfigAPIGroup) || wildcardGroup) {
				out = append(out, fmt.Sprintf("%s/ingresses (belongs in cluster_access_role.yaml): %+v", openShiftConfigAPIGroup, rule))
			}
		}
	}
	return out
}

func assertNoClusterScopedResources(t *testing.T, source string, rules []rbacv1.PolicyRule) {
	t.Helper()
	for _, msg := range clusterScopedViolations(rules) {
		t.Errorf("%s must not grant %s", source, msg)
	}
}

func TestClusterScopedViolations(t *testing.T) {
	tests := []struct {
		name    string
		rule    rbacv1.PolicyRule
		wantHit bool
	}{
		{
			name:    "explicit storageclasses",
			rule:    rbacv1.PolicyRule{APIGroups: []string{"storage.k8s.io"}, Resources: []string{"storageclasses"}},
			wantHit: true,
		},
		{
			name:    "wildcard storage.k8s.io",
			rule:    rbacv1.PolicyRule{APIGroups: []string{"storage.k8s.io"}, Resources: []string{"*"}},
			wantHit: true,
		},
		{
			name:    "wildcard all API groups",
			rule:    rbacv1.PolicyRule{APIGroups: []string{"*"}, Resources: []string{"*"}},
			wantHit: true,
		},
		{
			name:    "wildcard rbac.authorization.k8s.io includes clusterroles",
			rule:    rbacv1.PolicyRule{APIGroups: []string{"rbac.authorization.k8s.io"}, Resources: []string{"*"}},
			wantHit: true,
		},
		{
			name:    "noobaa-admin named secret",
			rule:    rbacv1.PolicyRule{APIGroups: []string{""}, Resources: []string{"secrets"}, ResourceNames: []string{"noobaa-admin"}},
			wantHit: true,
		},
		{
			name:    "config.openshift.io ingresses",
			rule:    rbacv1.PolicyRule{APIGroups: []string{openShiftConfigAPIGroup}, Resources: []string{"ingresses"}},
			wantHit: true,
		},
		{
			name:    "wildcard group with ingresses",
			rule:    rbacv1.PolicyRule{APIGroups: []string{"*"}, Resources: []string{"ingresses"}},
			wantHit: true,
		},
		{
			name:    "namespaced secrets without resourceNames",
			rule:    rbacv1.PolicyRule{APIGroups: []string{""}, Resources: []string{"secrets"}},
			wantHit: false,
		},
		{
			name:    "namespaced roles and rolebindings",
			rule:    rbacv1.PolicyRule{APIGroups: []string{"rbac.authorization.k8s.io"}, Resources: []string{"roles", "rolebindings"}},
			wantHit: false,
		},
		{
			name:    "wildcard networking.k8s.io is namespaced",
			rule:    rbacv1.PolicyRule{APIGroups: []string{"networking.k8s.io"}, Resources: []string{"*"}},
			wantHit: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := clusterScopedViolations([]rbacv1.PolicyRule{tt.rule})
			if tt.wantHit && len(got) == 0 {
				t.Fatalf("expected a cluster-scoped violation, got none")
			}
			if !tt.wantHit && len(got) > 0 {
				t.Fatalf("unexpected violation: %v", got)
			}
		})
	}
}

func TestManagerRole_NoClusterScopedResources(t *testing.T) {
	var cr rbacv1.ClusterRole
	decodeYAMLFile(t, rbacManifestPath(t, "role.yaml"), &cr)
	if cr.Name != "manager-role" {
		t.Fatalf("manager role name: got %q", cr.Name)
	}
	assertNoClusterScopedResources(t, "manager-role", cr.Rules)

	// Leader-election RoleBinding stays namespaced. Cluster-scoped kinds
	// (consolelinks, storageclasses, …) must not appear there.
	var csv olmCSVInstallPermissions
	decodeYAMLFile(t, bundleCSVPath(t), &csv)
	if len(csv.Spec.Install.Spec.Permissions) == 0 {
		t.Fatal("CSV spec.install.spec.permissions is empty (unmarshal failed or field moved)")
	}
	assertNoClusterScopedResources(t, "CSV permissions", csvPolicyRules(csv.Spec.Install.Spec.Permissions))
}

func TestCSV_AllNamespacesInstallMode(t *testing.T) {
	type csvInstallContract struct {
		Metadata struct {
			Annotations map[string]string `json:"annotations"`
		} `json:"metadata"`
		Spec struct {
			InstallModes []struct {
				Type      string `json:"type"`
				Supported bool   `json:"supported"`
			} `json:"installModes"`
		} `json:"spec"`
	}
	var csv csvInstallContract
	decodeYAMLFile(t, bundleCSVPath(t), &csv)

	want := map[string]bool{
		"OwnNamespace":    false,
		"SingleNamespace": false,
		"MultiNamespace":  false,
		"AllNamespaces":   true,
	}
	got := map[string]bool{}
	for _, m := range csv.Spec.InstallModes {
		got[m.Type] = m.Supported
	}
	for mode, supported := range want {
		if got[mode] != supported {
			t.Errorf("installModes %s: got %v, want %v", mode, got[mode], supported)
		}
	}
	if csv.Metadata.Annotations["operatorframework.io/suggested-namespace"] != "cost-onprem" {
		t.Errorf("suggested-namespace: got %q, want cost-onprem", csv.Metadata.Annotations["operatorframework.io/suggested-namespace"])
	}
	tmpl := csv.Metadata.Annotations["operatorframework.io/suggested-namespace-template"]
	if tmpl == "" {
		t.Fatal("missing operatorframework.io/suggested-namespace-template")
	}
	var ns corev1.Namespace
	if err := yaml.Unmarshal([]byte(tmpl), &ns); err != nil {
		t.Fatalf("suggested-namespace-template unmarshal: %v\n%s", err, tmpl)
	}
	if ns.Name != "cost-onprem" {
		t.Errorf("suggested-namespace-template metadata.name: got %q, want cost-onprem", ns.Name)
	}
	for _, key := range []string{
		"pod-security.kubernetes.io/enforce",
		"pod-security.kubernetes.io/audit",
		"pod-security.kubernetes.io/warn",
	} {
		if ns.Labels[key] != "restricted" {
			t.Errorf("suggested-namespace-template label %s: got %q, want restricted", key, ns.Labels[key])
		}
	}
}
