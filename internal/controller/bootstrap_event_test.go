package controller

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	costv1alpha1 "github.com/project-koku/koku-service-operator/api/v1alpha1"
	"github.com/project-koku/koku-service-operator/internal/resources"
)

func bootstrapCR(enabled bool, secretName string) *costv1alpha1.CostManagementServiceConfig {
	cfg := minimalCR(testCRName, testNamespace)
	cfg.Spec.RBAC.BootstrapAdmin.Enabled = enabled
	cfg.Spec.RBAC.BootstrapAdmin.SecretRef = corev1.LocalObjectReference{Name: secretName}
	cfg.Spec.RBAC.Image.Repository = "quay.io/test/rbac"
	cfg.Spec.RBAC.Image.Tag = "v1"
	cfg.Spec.CostManagement.API.Image.Repository = "quay.io/test/koku"
	cfg.Spec.CostManagement.API.Image.Tag = "v1"
	return cfg
}

func TestBootstrapAdminSkippedEvent_EnabledEmptySecret(t *testing.T) {
	cfg := bootstrapCR(true, "")
	scheme := ownershipScheme(t)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cfg).Build()
	rec := record.NewFakeRecorder(10)
	r := &CostManagementServiceConfigReconciler{Client: c, Scheme: scheme, Recorder: rec}

	_, _ = r.reconcileMigration(context.Background(), cfg)

	select {
	case event := <-rec.Events:
		if !strings.Contains(event, "BootstrapAdminSkipped") {
			t.Errorf("expected BootstrapAdminSkipped event, got: %s", event)
		}
		if !strings.Contains(event, corev1.EventTypeWarning) {
			t.Errorf("expected Warning event type, got: %s", event)
		}
	default:
		t.Error("expected BootstrapAdminSkipped event, got none")
	}
}

func TestBootstrapAdminNoEvent_EnabledWithSecret(t *testing.T) {
	cfg := bootstrapCR(true, "my-admin-secret")
	scheme := ownershipScheme(t)
	// Create the secret so the Job can reference it
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "my-admin-secret", Namespace: testNamespace},
		Data: map[string][]byte{
			"org-id":         []byte("org123"),
			"account-number": []byte("acct456"),
			"username":       []byte("admin"),
		},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cfg, secret).Build()
	rec := record.NewFakeRecorder(10)
	r := &CostManagementServiceConfigReconciler{Client: c, Scheme: scheme, Recorder: rec}

	_, _ = r.reconcileMigration(context.Background(), cfg)

	// Drain events — none should be BootstrapAdminSkipped
	for {
		select {
		case event := <-rec.Events:
			if strings.Contains(event, "BootstrapAdminSkipped") {
				t.Errorf("unexpected BootstrapAdminSkipped event: %s", event)
			}
		default:
			return
		}
	}
}

func TestBootstrapAdminNoEvent_Disabled(t *testing.T) {
	cfg := bootstrapCR(false, "")
	scheme := ownershipScheme(t)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cfg).Build()
	rec := record.NewFakeRecorder(10)
	r := &CostManagementServiceConfigReconciler{Client: c, Scheme: scheme, Recorder: rec}

	_, _ = r.reconcileMigration(context.Background(), cfg)

	for {
		select {
		case event := <-rec.Events:
			if strings.Contains(event, "BootstrapAdminSkipped") {
				t.Errorf("unexpected BootstrapAdminSkipped event when disabled: %s", event)
			}
		default:
			return
		}
	}
}

func TestAdminBootstrapJobCreated_WithSecretRef(t *testing.T) {
	cfg := bootstrapCR(true, "my-admin-secret")
	job := resources.AdminBootstrapJob(cfg, "v1")
	if job == nil {
		t.Fatal("AdminBootstrapJob should not be nil when enabled with secretRef")
	}
	if job.Name != resources.NameRBACAdminBootstrap(cfg) {
		t.Errorf("job name = %q", job.Name)
	}
}

func TestAdminBootstrapJobNil_EmptySecretRef(t *testing.T) {
	cfg := bootstrapCR(true, "")
	job := resources.AdminBootstrapJob(cfg, "v1")
	if job != nil {
		t.Error("AdminBootstrapJob should be nil when secretRef.name is empty")
	}
}

func TestAdminBootstrapJobNil_Disabled(t *testing.T) {
	cfg := bootstrapCR(false, "")
	job := resources.AdminBootstrapJob(cfg, "v1")
	if job != nil {
		t.Error("AdminBootstrapJob should be nil when disabled")
	}
}
