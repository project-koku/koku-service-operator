package resources

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	costv1alpha1 "github.com/project-koku/koku-service-operator/api/v1alpha1"
)

// TestKruizeClusterRoleDoesNotGrantSecretsAccess verifies that the Kruize
// ClusterRole does not include cluster-wide read access to Secrets.
// Secrets access allows reading every credential in every namespace —
// a significant security over-privilege for a workload optimizer.
func TestKruizeClusterRoleDoesNotGrantSecretsAccess(t *testing.T) {
	cfg := &costv1alpha1.CostManagementServiceConfig{
		ObjectMeta: metav1.ObjectMeta{Name: "cost-management", Namespace: "test"},
	}
	cr := KruizeClusterRole(cfg)

	for _, rule := range cr.Rules {
		for _, res := range rule.Resources {
			if res == "secrets" {
				t.Errorf("KruizeClusterRole grants access to %q — "+
					"this allows reading every Secret cluster-wide; "+
					"remove 'secrets' from the ClusterRole rules", res)
			}
		}
	}
}

// TestKruizeClusterRoleNameIsNamespaceScoped verifies that the ClusterRole name
// includes the namespace so two CRs with the same name in different namespaces
// do not collide on the same cluster-scoped object.
func TestKruizeClusterRoleNameIsNamespaceScoped(t *testing.T) {
	cfg1 := &costv1alpha1.CostManagementServiceConfig{
		ObjectMeta: metav1.ObjectMeta{Name: "cost-management", Namespace: "ns-a"},
	}
	cfg2 := &costv1alpha1.CostManagementServiceConfig{
		ObjectMeta: metav1.ObjectMeta{Name: "cost-management", Namespace: "ns-b"},
	}
	name1 := NameKruizeClusterRole(cfg1)
	name2 := NameKruizeClusterRole(cfg2)
	if name1 == name2 {
		t.Errorf("NameKruizeClusterRole collision: both 'cost-management' CRs in different namespaces produce %q — "+
			"deleting one CR would remove the ClusterRole used by the other", name1)
	}
	// Names must stay under the 253-char Kubernetes resource name limit.
	for _, name := range []string{name1, name2} {
		if len(name) > 253 {
			t.Errorf("NameKruizeClusterRole %q is %d chars, exceeds 253-char limit", name, len(name))
		}
	}
}
