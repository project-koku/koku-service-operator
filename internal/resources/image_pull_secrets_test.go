package resources

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	costv1alpha1 "github.com/project-koku/koku-service-operator/api/v1alpha1"
)

func TestImagePullSecretsAppliedToWorkloads(t *testing.T) {
	cfg := &costv1alpha1.CostManagementServiceConfig{
		ObjectMeta: metav1.ObjectMeta{Name: "cost-management", Namespace: "cost-onprem"},
		Spec: costv1alpha1.CostManagementServiceConfigSpec{
			Global: costv1alpha1.GlobalConfig{
				ImagePullSecrets: []corev1.LocalObjectReference{{Name: "rh-pull-secret"}},
			},
			Auth: costv1alpha1.AuthConfig{
				Keycloak: costv1alpha1.KeycloakSpec{
					URL: "http://keycloak.keycloak.svc:8080",
				},
			},
		},
	}
	cfg.Spec.CostManagement.API.Image.Repository = "quay.io/koku/koku"
	cfg.Spec.CostManagement.API.Image.Tag = "test"
	cfg.Spec.Auth.Envoy.Image.Repository = "registry.redhat.io/openshift-service-mesh/proxyv2-rhel9"
	cfg.Spec.Auth.Envoy.Image.Tag = "2.6"

	want := []corev1.LocalObjectReference{{Name: "rh-pull-secret"}}

	checks := []struct {
		name string
		got  []corev1.LocalObjectReference
	}{
		{"koku-api", KokuAPIDeployment(cfg).Spec.Template.Spec.ImagePullSecrets},
		{"envoy", EnvoyDeployment(cfg).Spec.Template.Spec.ImagePullSecrets},
		{"database", DatabaseStatefulSet(cfg).Spec.Template.Spec.ImagePullSecrets},
		{"cache", CacheDeployment(cfg).Spec.Template.Spec.ImagePullSecrets},
		{"migration", MigrationJob(cfg, "test").Spec.Template.Spec.ImagePullSecrets},
	}
	for _, tc := range checks {
		if len(tc.got) != 1 || tc.got[0].Name != want[0].Name {
			t.Errorf("%s ImagePullSecrets = %#v, want %#v", tc.name, tc.got, want)
		}
	}
}

func TestImagePullSecretsEmptyByDefault(t *testing.T) {
	cfg := &costv1alpha1.CostManagementServiceConfig{
		ObjectMeta: metav1.ObjectMeta{Name: "cost-management", Namespace: "cost-onprem"},
	}
	if got := imagePullSecrets(cfg); len(got) != 0 {
		t.Fatalf("expected empty imagePullSecrets, got %#v", got)
	}
}
