package controller

import (
	"context"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	costv1alpha1 "github.com/project-koku/koku-service-operator/api/v1alpha1"
)

// This spec proves the behavioral heart of AllNamespaces: the real cache +
// SetupWithManager() wiring observes and reconciles a CostManagementServiceConfig
// in a namespace the operator does not live in. That is the reason a
// ClusterRoleBinding (not a namespaced RoleBinding) is required, and the
// regression this guards against — a DefaultNamespaces pin, a namespace
// field-selector, or a scoped For()/Owns() predicate — is invisible to the
// structural TestCacheOptionsForNamespace_* / TestManagerRoleBinding_* tests.
//
// Scope, stated honestly: envtest runs the test client as admin and does NOT
// enforce the operator ServiceAccount's RBAC, so this cannot prove RBAC
// *enforcement*. That half is covered statically by the rbac_manifest_test.go
// suite (ClusterRoleBinding shape, grant set) and can only be exercised live in
// e2e. Here we prove cross-namespace *observation + reconcile*.
//
// The probe CMSC is paused: reconcile registers the finalizer
// (costmanagementserviceconfig_controller.go, top of Reconcile) and then
// short-circuits on the pause annotation before any operand/OpenShift API work.
// So the assertion needs only the CMSC CRD — no OpenShift CRDs, no operand
// churn, no flake — while still requiring the cluster-wide cache to have
// delivered the CR from a foreign namespace.
var _ = Describe("cross-namespace reconcile (AllNamespaces)", func() {
	const probeNamespace = "xns-beta"
	probeKey := types.NamespacedName{Name: "probe", Namespace: probeNamespace}

	It("reconciles a CMSC in a namespace the operator does not live in", func() {
		// Cluster-wide cache: no DefaultNamespaces pin. Mirrors cmd's
		// cacheOptionsForNamespace("") — empty watchNamespace = watch all.
		mgr, err := ctrl.NewManager(cfg, ctrl.Options{
			Scheme:  k8sClient.Scheme(),
			Cache:   cache.Options{},
			Metrics: metricsserver.Options{BindAddress: "0"},
		})
		Expect(err).NotTo(HaveOccurred())

		// The REAL controller wiring — this is the code that could silently
		// scope the watch and break cross-namespace reconcile.
		Expect((&CostManagementServiceConfigReconciler{
			Client:   mgr.GetClient(),
			Scheme:   mgr.GetScheme(),
			Recorder: record.NewFakeRecorder(16),
		}).SetupWithManager(mgr)).To(Succeed())

		mgrCtx, stopMgr := context.WithCancel(ctx)
		mgrDone := make(chan error, 1)
		go func() {
			defer GinkgoRecover()
			mgrDone <- mgr.Start(mgrCtx)
		}()

		// Cleanup: stop the manager and WAIT for it to fully return before
		// touching the CMSC. stopMgr() only cancels the context; without waiting
		// on mgrDone an in-flight reconcile could re-add the finalizer after we
		// strip it. Once the manager is drained it cannot re-add the finalizer or
		// run reconcileDelete (which deletes a ConsoleLink — a CRD absent from
		// envtest — and would wedge on the error), so we can strip the finalizer
		// directly and delete, asserting each step, leaving no residue.
		DeferCleanup(func() {
			stopMgr()
			Eventually(mgrDone).WithTimeout(30 * time.Second).Should(Receive(Succeed()))
			got := &costv1alpha1.CostManagementServiceConfig{}
			if err := k8sClient.Get(ctx, probeKey, got); err == nil {
				got.SetFinalizers(nil)
				Expect(k8sClient.Update(ctx, got)).To(Succeed())
				Expect(k8sClient.Delete(ctx, got)).To(Succeed())
			} else {
				Expect(apierrors.IsNotFound(err)).To(BeTrue(), "unexpected Get error during cleanup: %v", err)
			}
		})

		By("creating a second namespace and a paused CMSC in it")
		Expect(k8sClient.Create(ctx, &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{Name: probeNamespace},
		})).To(Succeed())

		probe := &costv1alpha1.CostManagementServiceConfig{
			ObjectMeta: metav1.ObjectMeta{
				Name:        probeKey.Name,
				Namespace:   probeKey.Namespace,
				Annotations: map[string]string{pauseAnnotation: annotationTrue},
			},
			Spec: costv1alpha1.CostManagementServiceConfigSpec{
				Auth: costv1alpha1.AuthConfig{
					Keycloak: costv1alpha1.KeycloakSpec{
						URL: "http://keycloak.example.svc:8080",
					},
				},
			},
		}
		Expect(k8sClient.Create(ctx, probe)).To(Succeed())

		By("the cluster-wide cache delivering the CR and the controller reconciling it")
		Eventually(func(g Gomega) {
			got := &costv1alpha1.CostManagementServiceConfig{}
			g.Expect(k8sClient.Get(ctx, probeKey, got)).To(Succeed())
			g.Expect(controllerutil.ContainsFinalizer(got, finalizerName)).To(BeTrue())
		}).WithTimeout(30 * time.Second).WithPolling(500 * time.Millisecond).Should(Succeed())
	})
})
