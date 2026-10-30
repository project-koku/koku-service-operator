package controller

import (
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	costv1alpha1 "github.com/project-koku/koku-service-operator/api/v1alpha1"
	"github.com/project-koku/koku-service-operator/internal/resources"
)

// D22: the CRD's CEL rule rejects CR names whose generated child resources
// would exceed Kubernetes name/label limits (see TestMaxCRNameFitsGeneratedNames
// for the derivation). Exercised against the real envtest apiserver.
var _ = Describe("CMSC metadata.name length (CRD CEL)", func() {
	cmsc := func(nameLen int, ros bool) *costv1alpha1.CostManagementServiceConfig {
		deployTrue := true
		return &costv1alpha1.CostManagementServiceConfig{
			ObjectMeta: metav1.ObjectMeta{Name: strings.Repeat("n", nameLen), Namespace: "default"},
			Spec: costv1alpha1.CostManagementServiceConfigSpec{
				Database: costv1alpha1.DatabaseConfig{Deploy: &deployTrue},
				Cache:    costv1alpha1.CacheConfig{Deploy: &deployTrue},
				Auth: costv1alpha1.AuthConfig{
					Keycloak: costv1alpha1.KeycloakSpec{URL: "http://keycloak.example.svc:8080"},
				},
				ROS: costv1alpha1.ROSConfig{Enabled: &ros},
			},
		}
	}

	DescribeTable("admission",
		func(nameLen int, ros, wantAccepted bool) {
			obj := cmsc(nameLen, ros)
			err := k8sClient.Create(ctx, obj)
			if wantAccepted {
				Expect(err).NotTo(HaveOccurred())
				Expect(k8sClient.Delete(ctx, obj)).To(Succeed())
				return
			}
			Expect(apierrors.IsInvalid(err)).To(BeTrue(), "want Invalid, got %v", err)
			Expect(err.Error()).To(ContainSubstring("metadata.name must be at most"))
		},
		Entry("ROS off, at cap", costv1alpha1.MaxNameLength, false, true),
		Entry("ROS off, cap+1", costv1alpha1.MaxNameLength+1, false, false),
		Entry("ROS on, at cap", costv1alpha1.MaxNameLengthWithROS, true, true),
		Entry("ROS on, cap+1", costv1alpha1.MaxNameLengthWithROS+1, true, false),
	)

	// Celery worker Deployments are always applied (also at 0 replicas); at the
	// cap the longest is {cr}-celery-worker-subs-transmission (65 chars).
	// Deployment names are DNS-1123 subdomains, so the apiserver admits them.
	It("admits every Celery worker Deployment for a CR at MaxNameLength", func() {
		cfg := minimalCR(strings.Repeat("c", costv1alpha1.MaxNameLength), "default")
		for _, d := range resources.CeleryWorkerDeployments(cfg) {
			for i := range d.Spec.Template.Spec.InitContainers {
				d.Spec.Template.Spec.InitContainers[i].Image = "quay.io/test/img:1"
			}
			for i := range d.Spec.Template.Spec.Containers {
				d.Spec.Template.Spec.Containers[i].Image = "quay.io/test/img:1"
			}
			Expect(k8sClient.Create(ctx, d)).To(Succeed(), "create %s (len %d)", d.Name, len(d.Name))
			Expect(k8sClient.Delete(ctx, d)).To(Succeed())
		}
	})
})
