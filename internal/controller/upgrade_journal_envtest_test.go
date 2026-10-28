package controller

import (
	"encoding/json"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	cost "github.com/project-koku/koku-service-operator/api/v1alpha1"
)

var _ = Describe("Upgrade journal storage", func() {
	It("preserves frozen revisions and schema risk across API reads and spec edits", func() {
		cfg := &cost.CostManagementServiceConfig{
			ObjectMeta: metav1.ObjectMeta{Name: "upgrade-journal", Namespace: "default"},
			Spec: cost.CostManagementServiceConfigSpec{
				Upgrade:  cost.UpgradePolicy{ImagePolicy: cost.ImagePolicyFollowOperator},
				Auth:     cost.AuthConfig{Keycloak: cost.KeycloakSpec{URL: "https://keycloak.example.com"}},
				Database: cost.DatabaseConfig{Host: "postgresql.example.com", SecretName: "database-credentials"},
				Cache: cost.CacheConfig{
					Host: "cache.example.com", Auth: cost.CacheAuthSpec{Enabled: true, SecretName: "cache-credentials"},
				},
			},
		}
		Expect(k8sClient.Create(ctx, cfg)).To(Succeed())
		DeferCleanup(func() { Expect(k8sClient.Delete(ctx, cfg)).To(Succeed()) })
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(cfg), cfg)).To(Succeed())
		effective := cfg.DeepCopy()
		bundle := mustCurrentReleaseBundle()
		Expect(bundle.Resolve(effective)).To(Succeed())
		revision := applicationRevision(effective, bundle)
		cfg.Status.Upgrade = cost.UpgradeStatus{
			Installed: revision.DeepCopy(),
			Operation: &cost.UpgradeOperation{
				ID: "persisted-operation", TargetGeneration: cfg.Generation,
				Source: revision.DeepCopy(), Target: *revision, Stage: cost.UpgradeStageMigrating,
				StartedAt: metav1.Now(), StageStartedAt: metav1.Now(), SchemaMutationPossible: true,
			},
		}
		Expect(k8sClient.Status().Update(ctx, cfg)).To(Succeed())
		cfg.Spec.CostManagement.API.Image = cost.ImageSpec{Repository: "quay.io/custom/koku", Tag: "pending"}
		Expect(k8sClient.Update(ctx, cfg)).To(Succeed())
		restored := &cost.CostManagementServiceConfig{}
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(cfg), restored)).To(Succeed())
		Expect(restored.Status.Upgrade.Operation).NotTo(BeNil())
		op := restored.Status.Upgrade.Operation
		Expect(op.TargetGeneration).To(BeNumerically("<", restored.Generation))
		Expect(op.SchemaMutationPossible).To(BeTrue())
		Expect(op.Source.Images).To(Equal(revision.Images))
		var frozen cost.CostManagementServiceConfigSpec
		Expect(json.Unmarshal(op.Target.Spec.Raw, &frozen)).To(Succeed())
		Expect(frozen.CostManagement.API.Image).To(Equal(effective.Spec.CostManagement.API.Image))
		Expect(restored.Spec.CostManagement.API.Image.Tag).To(Equal("pending"))
	})
})
