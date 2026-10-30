package controller

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	costv1alpha1 "github.com/project-koku/koku-service-operator/api/v1alpha1"
)

// CEL and pattern rules on spec.auth.envoy.ingress*Timeout can only be proven
// against a real API server (envtest), using server-side dry-run creates.
var _ = Describe("Envoy ingress timeout CRD validation", func() {
	newCR := func(timeout, perTry string) *costv1alpha1.CostManagementServiceConfig {
		deploy := true
		return &costv1alpha1.CostManagementServiceConfig{
			ObjectMeta: metav1.ObjectMeta{Name: "envoy-duration-probe", Namespace: "default"},
			Spec: costv1alpha1.CostManagementServiceConfigSpec{
				Database: costv1alpha1.DatabaseConfig{Deploy: &deploy},
				Cache:    costv1alpha1.CacheConfig{Deploy: &deploy},
				Auth: costv1alpha1.AuthConfig{
					Keycloak: costv1alpha1.KeycloakSpec{URL: "http://keycloak.example.svc:8080"},
					Envoy: costv1alpha1.EnvoySpec{
						IngressTimeout:       timeout,
						IngressPerTryTimeout: perTry,
					},
				},
			},
		}
	}

	DescribeTable("ingressTimeout",
		func(value string, valid bool) {
			err := k8sClient.Create(ctx, newCR(value, ""), client.DryRunAll)
			if valid {
				Expect(err).NotTo(HaveOccurred())
				return
			}
			Expect(apierrors.IsInvalid(err)).To(BeTrue(), "want Invalid, got %v", err)
		},
		Entry("accepts 10m", "10m", true),
		Entry("accepts 1h30m", "1h30m", true),
		Entry("accepts 500ms", "500ms", true),
		Entry("rejects 0s", "0s", false),
		Entry("rejects 0m0s", "0m0s", false),
		Entry("rejects non-duration abc", "abc", false),
		Entry("rejects overflowing value", "99999999999999999h", false),
	)

	DescribeTable("ingressPerTryTimeout",
		func(value string, valid bool) {
			err := k8sClient.Create(ctx, newCR("", value), client.DryRunAll)
			if valid {
				Expect(err).NotTo(HaveOccurred())
				return
			}
			Expect(apierrors.IsInvalid(err)).To(BeTrue(), "want Invalid, got %v", err)
		},
		Entry("accepts 300s", "300s", true),
		Entry("rejects 0s", "0s", false),
		Entry("rejects abc", "abc", false),
		Entry("rejects overflowing value", "99999999999999999h", false),
	)
})
