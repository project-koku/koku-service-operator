package resources

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
)

func TestFamilyServiceAccounts(t *testing.T) {
	cfg := testCfg()
	tests := []struct {
		name      string
		sa        *corev1.ServiceAccount
		wantName  string
		component string
	}{
		{"gateway", GatewayServiceAccount(cfg), NameGatewayServiceAccount(cfg), "gateway"},
		{"ingress", IngressServiceAccount(cfg), NameIngressServiceAccount(cfg), "ingress"},
		{"rbac", RBACServiceAccount(cfg), NameRBACServiceAccount(cfg), "rbac"},
		{"ui", UIServiceAccount(cfg), NameUIServiceAccount(cfg), "ui"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.sa.Name != tt.wantName {
				t.Errorf("Name = %q, want %q", tt.sa.Name, tt.wantName)
			}
			if tt.sa.Namespace != cfg.Namespace {
				t.Errorf("Namespace = %q, want %q", tt.sa.Namespace, cfg.Namespace)
			}
			if got := tt.sa.Labels[labelComponent]; got != tt.component {
				t.Errorf("component label = %q, want %q", got, tt.component)
			}
			if tt.sa.AutomountServiceAccountToken == nil || *tt.sa.AutomountServiceAccountToken {
				t.Errorf("AutomountServiceAccountToken = %v, want false", tt.sa.AutomountServiceAccountToken)
			}
		})
	}
}

func TestFamilyServiceAccountNames(t *testing.T) {
	cfg := testCfg()
	if got, want := NameGatewayServiceAccount(cfg), "cost-management-gateway"; got != want {
		t.Errorf("NameGatewayServiceAccount = %q, want %q", got, want)
	}
	if got, want := NameIngressServiceAccount(cfg), "cost-management-ingress"; got != want {
		t.Errorf("NameIngressServiceAccount = %q, want %q", got, want)
	}
	if got, want := NameRBACServiceAccount(cfg), "cost-management-rbac"; got != want {
		t.Errorf("NameRBACServiceAccount = %q, want %q", got, want)
	}
	if got, want := NameUIServiceAccount(cfg), "cost-management-ui"; got != want {
		t.Errorf("NameUIServiceAccount = %q, want %q", got, want)
	}
}
