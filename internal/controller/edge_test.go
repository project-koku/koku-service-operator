package controller

import (
	"context"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"

	costv1alpha1 "github.com/project-koku/koku-service-operator/api/v1alpha1"
	"github.com/project-koku/koku-service-operator/internal/resources"
)

func TestReconcileEdge_EnvoyNotReady(t *testing.T) {
	scheme := ownershipScheme(t)
	cfg := minimalCR(testCRName, testNamespace)
	r := &CostManagementServiceConfigReconciler{
		Client:   fakeClientWithApplySupport(scheme),
		Scheme:   scheme,
		Recorder: &noopRecorder{},
	}

	result, err := r.reconcileEdge(context.Background(), cfg)
	if err != nil {
		t.Fatalf("reconcileEdge: %v", err)
	}
	if result.RequeueAfter == 0 {
		t.Fatal("expected non-zero RequeueAfter while Envoy is not ready")
	}

	mustExist(t, r.Client, testNamespace, resources.NameEnvoyConfigMap(cfg), &corev1.ConfigMap{})
	mustExist(t, r.Client, testNamespace, resources.NameEnvoy(cfg), &corev1.Service{})
	mustExist(t, r.Client, testNamespace, resources.NameEnvoy(cfg), &appsv1.Deployment{})

	// Isolation and cookie/nginx do not wait on Envoy readiness. UI Deploy does.
	mustIsolationNetworkPolicies(t, r.Client, cfg)
	mustExist(t, r.Client, testNamespace, resources.NameUICookieSecret(cfg), &corev1.Secret{})
	mustExist(t, r.Client, testNamespace, resources.NameUINginxConfigMap(cfg), &corev1.ConfigMap{})
	mustNotExist(t, r.Client, testNamespace, resources.NameUI(cfg), &appsv1.Deployment{})

	cond := findCondition(cfg.Status.Conditions, costv1alpha1.ConditionGatewayReady)
	if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != "WaitingForGateway" {
		t.Fatalf("expected GatewayReady=False WaitingForGateway, got %+v", cond)
	}
}

func TestReconcileEdge_EnvoyReady_NoClusterDomain(t *testing.T) {
	scheme := ownershipScheme(t)
	cfg := minimalCR(testCRName, testNamespace)
	c := fakeClientPreservingStatus(scheme)
	r := &CostManagementServiceConfigReconciler{
		Client:   c,
		Scheme:   scheme,
		Recorder: &noopRecorder{},
	}

	if _, err := r.reconcileEdge(context.Background(), cfg); err != nil {
		t.Fatalf("first pass: %v", err)
	}
	markDeploymentReady(t, c, testNamespace, resources.NameEnvoy(cfg))

	result, err := r.reconcileEdge(context.Background(), cfg)
	if err != nil {
		t.Fatalf("second pass: %v", err)
	}
	if result.RequeueAfter == 0 {
		t.Fatal("expected requeue when cluster domain is missing")
	}

	cond := findCondition(cfg.Status.Conditions, costv1alpha1.ConditionGatewayReady)
	if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != "ClusterDomainPending" {
		t.Fatalf("expected GatewayReady=False ClusterDomainPending, got %+v", cond)
	}

	// Isolation and cookie/nginx do not wait on cluster domain. UI Deploy does.
	mustIsolationNetworkPolicies(t, r.Client, cfg)
	mustExist(t, r.Client, testNamespace, resources.NameUICookieSecret(cfg), &corev1.Secret{})
	mustExist(t, r.Client, testNamespace, resources.NameUINginxConfigMap(cfg), &corev1.ConfigMap{})
	mustNotExist(t, r.Client, testNamespace, resources.NameUI(cfg), &appsv1.Deployment{})
}

func TestReconcileEdge_EnvoyReady_WithDomain_NoOAuth(t *testing.T) {
	scheme := ownershipScheme(t)
	cfg := minimalCR(testCRName, testNamespace)
	cfg.Status.DiscoveredConfig = &costv1alpha1.DiscoveredConfig{ClusterDomain: "apps.example.com"}
	c := fakeClientPreservingStatus(scheme)
	r := &CostManagementServiceConfigReconciler{
		Client:   c,
		Scheme:   scheme,
		Recorder: &noopRecorder{},
	}

	if _, err := r.reconcileEdge(context.Background(), cfg); err != nil {
		t.Fatalf("first pass: %v", err)
	}
	markDeploymentReady(t, c, testNamespace, resources.NameEnvoy(cfg))
	markRouteAdmitted(t, c, testNamespace, resources.NameAPIRoute(cfg))

	result, err := r.reconcileEdge(context.Background(), cfg)
	if err != nil {
		t.Fatalf("second pass: %v", err)
	}
	if result.RequeueAfter == 0 {
		t.Fatal("expected requeue while UIReady is False")
	}

	if !apimeta.IsStatusConditionTrue(cfg.Status.Conditions, costv1alpha1.ConditionGatewayReady) {
		t.Fatal("expected GatewayReady=True once Envoy is ready and route is admitted")
	}
	gwCond := findCondition(cfg.Status.Conditions, costv1alpha1.ConditionGatewayReady)
	if gwCond == nil || gwCond.Reason != "GatewayReady" {
		t.Fatalf("expected GatewayReady reason GatewayReady, got %+v", gwCond)
	}

	route := &unstructured.Unstructured{}
	route.SetGroupVersionKind(routeGVK())
	mustExist(t, r.Client, testNamespace, resources.NameAPIRoute(cfg), route)

	mustExist(t, r.Client, testNamespace, resources.NameUICookieSecret(cfg), &corev1.Secret{})
	mustExist(t, r.Client, testNamespace, resources.NameUINginxConfigMap(cfg), &corev1.ConfigMap{})
	mustNotExist(t, r.Client, testNamespace, resources.NameUI(cfg), &appsv1.Deployment{})

	uiCond := findCondition(cfg.Status.Conditions, costv1alpha1.ConditionUIReady)
	if uiCond == nil || uiCond.Status != metav1.ConditionFalse || uiCond.Reason != "OAuthClientSecretMissing" {
		t.Fatalf("expected UIReady=False OAuthClientSecretMissing, got %+v", uiCond)
	}

	mustIsolationNetworkPolicies(t, r.Client, cfg)
}

func TestReconcileEdge_DoesNotOverwriteOIDCUnreachable(t *testing.T) {
	scheme := ownershipScheme(t)
	cfg := minimalCR(testCRName, testNamespace)
	cfg.Status.DiscoveredConfig = &costv1alpha1.DiscoveredConfig{ClusterDomain: "apps.example.com"}
	apimeta.SetStatusCondition(&cfg.Status.Conditions, metav1.Condition{
		Type:   costv1alpha1.ConditionAuthReady,
		Status: metav1.ConditionFalse,
		Reason: "OIDCUnreachable",
	})
	c := fakeClientPreservingStatus(scheme)
	r := &CostManagementServiceConfigReconciler{Client: c, Scheme: scheme, Recorder: &noopRecorder{}}

	if _, err := r.reconcileEdge(context.Background(), cfg); err != nil {
		t.Fatalf("first pass: %v", err)
	}
	markDeploymentReady(t, c, testNamespace, resources.NameEnvoy(cfg))
	markRouteAdmitted(t, c, testNamespace, resources.NameAPIRoute(cfg))
	if _, err := r.reconcileEdge(context.Background(), cfg); err != nil {
		t.Fatalf("second pass: %v", err)
	}

	auth := findCondition(cfg.Status.Conditions, costv1alpha1.ConditionAuthReady)
	if auth == nil || auth.Status != metav1.ConditionFalse || auth.Reason != "OIDCUnreachable" {
		t.Fatalf("AuthenticationReady must stay OIDCUnreachable, got %+v", auth)
	}
	gw := findCondition(cfg.Status.Conditions, costv1alpha1.ConditionGatewayReady)
	if gw == nil || gw.Status != metav1.ConditionTrue || gw.Reason != "GatewayReady" {
		t.Fatalf("expected GatewayReady=True, got %+v", gw)
	}
}

func TestReconcileUI_ValidOAuthSecret(t *testing.T) {
	scheme := ownershipScheme(t)
	cfg := minimalCR(testCRName, testNamespace)
	cfg.Status.DiscoveredConfig = &costv1alpha1.DiscoveredConfig{ClusterDomain: "apps.example.com"}

	oauth := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      resources.NameUIOAuthClientSecret(cfg),
			Namespace: testNamespace,
		},
		Data: map[string][]byte{
			"client-id":     []byte("cost-ui"),
			"client-secret": []byte("s3cret"),
		},
	}

	r := &CostManagementServiceConfigReconciler{
		Client:   fakeClientWithApplySupport(scheme, oauth),
		Scheme:   scheme,
		Recorder: &noopRecorder{},
	}

	if err := r.reconcileUI(context.Background(), cfg); err != nil {
		t.Fatalf("reconcileUI: %v", err)
	}

	mustExist(t, r.Client, testNamespace, resources.NameUICookieSecret(cfg), &corev1.Secret{})
	mustExist(t, r.Client, testNamespace, resources.NameUINginxConfigMap(cfg), &corev1.ConfigMap{})
	mustExist(t, r.Client, testNamespace, resources.NameUI(cfg), &appsv1.Deployment{})
	mustExist(t, r.Client, testNamespace, resources.NameUI(cfg), &corev1.Service{})

	uiRoute := &unstructured.Unstructured{}
	uiRoute.SetGroupVersionKind(routeGVK())
	mustExist(t, r.Client, testNamespace, cfg.Name+"-ui", uiRoute)

	cl := &unstructured.Unstructured{}
	cl.SetGroupVersionKind(schema.GroupVersionKind{Group: "console.openshift.io", Version: "v1", Kind: "ConsoleLink"})
	mustNotExist(t, r.Client, "", resources.NameConsoleLink(cfg), cl)

	cond := findCondition(cfg.Status.Conditions, costv1alpha1.ConditionUIReady)
	if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != "RouteNotAdmitted" {
		t.Fatalf("expected UIReady=False RouteNotAdmitted until the UI Route is admitted, got %+v", cond)
	}
}

func TestReconcileUI_InvalidOAuthSecret(t *testing.T) {
	scheme := ownershipScheme(t)
	cfg := minimalCR(testCRName, testNamespace)
	cfg.Status.DiscoveredConfig = &costv1alpha1.DiscoveredConfig{ClusterDomain: "apps.example.com"}

	oauth := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      resources.NameUIOAuthClientSecret(cfg),
			Namespace: testNamespace,
		},
		Data: map[string][]byte{
			"client-id": []byte("cost-ui"),
			// client-secret missing
		},
	}

	r := &CostManagementServiceConfigReconciler{
		Client:   fakeClientWithApplySupport(scheme, oauth),
		Scheme:   scheme,
		Recorder: &noopRecorder{},
	}

	if err := r.reconcileUI(context.Background(), cfg); err != nil {
		t.Fatalf("reconcileUI should not error on invalid secret: %v", err)
	}

	mustExist(t, r.Client, testNamespace, resources.NameUICookieSecret(cfg), &corev1.Secret{})
	mustExist(t, r.Client, testNamespace, resources.NameUINginxConfigMap(cfg), &corev1.ConfigMap{})
	mustNotExist(t, r.Client, testNamespace, resources.NameUI(cfg), &appsv1.Deployment{})

	cond := findCondition(cfg.Status.Conditions, costv1alpha1.ConditionUIReady)
	if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != "OAuthClientSecretInvalid" {
		t.Fatalf("expected UIReady=False OAuthClientSecretInvalid, got %+v", cond)
	}
}

func TestReconcileEdge_RouteNotAdmitted_Requeues(t *testing.T) {
	scheme := ownershipScheme(t)
	cfg := minimalCR(testCRName, testNamespace)
	cfg.Status.DiscoveredConfig = &costv1alpha1.DiscoveredConfig{ClusterDomain: "apps.example.com"}
	c := fakeClientPreservingStatus(scheme)
	r := &CostManagementServiceConfigReconciler{
		Client:   c,
		Scheme:   scheme,
		Recorder: &noopRecorder{},
	}

	if _, err := r.reconcileEdge(context.Background(), cfg); err != nil {
		t.Fatalf("first pass: %v", err)
	}
	markDeploymentReady(t, c, testNamespace, resources.NameEnvoy(cfg))

	result, err := r.reconcileEdge(context.Background(), cfg)
	if err != nil {
		t.Fatalf("second pass: %v", err)
	}
	if result.RequeueAfter == 0 {
		t.Fatal("expected RequeueAfter while the API Route is not admitted")
	}

	cond := findCondition(cfg.Status.Conditions, costv1alpha1.ConditionGatewayReady)
	if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != "RouteNotAdmitted" {
		t.Fatalf("expected GatewayReady=False RouteNotAdmitted, got %+v", cond)
	}

	route := &unstructured.Unstructured{}
	route.SetGroupVersionKind(routeGVK())
	mustExist(t, r.Client, testNamespace, resources.NameAPIRoute(cfg), route)
	ingress, found, nerr := unstructured.NestedSlice(route.Object, "status", "ingress")
	if nerr != nil {
		t.Fatalf("NestedSlice status.ingress: %v", nerr)
	}
	if found && len(ingress) > 0 {
		t.Fatalf("expected empty status.ingress before admission, got %#v", ingress)
	}

	// Isolation and cookie/nginx apply before API Route admission. UI Deploy waits.
	mustIsolationNetworkPolicies(t, r.Client, cfg)
	mustExist(t, r.Client, testNamespace, resources.NameUICookieSecret(cfg), &corev1.Secret{})
	mustExist(t, r.Client, testNamespace, resources.NameUINginxConfigMap(cfg), &corev1.ConfigMap{})
	mustNotExist(t, r.Client, testNamespace, resources.NameUI(cfg), &appsv1.Deployment{})
}

func TestReconcileEdge_RouteAdmitted_GatewayReady(t *testing.T) {
	scheme := ownershipScheme(t)
	cfg := minimalCR(testCRName, testNamespace)
	cfg.Status.DiscoveredConfig = &costv1alpha1.DiscoveredConfig{ClusterDomain: "apps.example.com"}
	c := fakeClientPreservingStatus(scheme)
	r := &CostManagementServiceConfigReconciler{
		Client:   c,
		Scheme:   scheme,
		Recorder: &noopRecorder{},
	}

	if _, err := r.reconcileEdge(context.Background(), cfg); err != nil {
		t.Fatalf("first pass: %v", err)
	}
	markDeploymentReady(t, c, testNamespace, resources.NameEnvoy(cfg))
	markRouteAdmitted(t, c, testNamespace, resources.NameAPIRoute(cfg))

	if _, err := r.reconcileEdge(context.Background(), cfg); err != nil {
		t.Fatalf("second pass: %v", err)
	}

	live := &unstructured.Unstructured{}
	live.SetGroupVersionKind(routeGVK())
	mustExist(t, r.Client, testNamespace, resources.NameAPIRoute(cfg), live)
	if !routeAdmitted(live) {
		t.Fatal("seeded API Route should be admitted")
	}
	if !apimeta.IsStatusConditionTrue(cfg.Status.Conditions, costv1alpha1.ConditionGatewayReady) {
		t.Fatalf("expected GatewayReady=True after admission, got %+v",
			findCondition(cfg.Status.Conditions, costv1alpha1.ConditionGatewayReady))
	}
}

func TestReconcileEdge_UIRouteNotAdmitted(t *testing.T) {
	scheme := ownershipScheme(t)
	cfg := minimalCR(testCRName, testNamespace)
	cfg.Status.DiscoveredConfig = &costv1alpha1.DiscoveredConfig{ClusterDomain: "apps.example.com"}

	oauth := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      resources.NameUIOAuthClientSecret(cfg),
			Namespace: testNamespace,
		},
		Data: map[string][]byte{
			"client-id":     []byte("cost-ui"),
			"client-secret": []byte("s3cret"),
		},
	}
	c := fakeClientPreservingStatus(scheme, oauth)
	r := &CostManagementServiceConfigReconciler{
		Client:   c,
		Scheme:   scheme,
		Recorder: &noopRecorder{},
	}

	if _, err := r.reconcileEdge(context.Background(), cfg); err != nil {
		t.Fatalf("first pass: %v", err)
	}
	markDeploymentReady(t, c, testNamespace, resources.NameEnvoy(cfg))
	markRouteAdmitted(t, c, testNamespace, resources.NameAPIRoute(cfg))

	result, err := r.reconcileEdge(context.Background(), cfg)
	if err != nil {
		t.Fatalf("second pass: %v", err)
	}
	if result.RequeueAfter == 0 {
		t.Fatal("expected RequeueAfter while the UI Route is not admitted")
	}

	if !apimeta.IsStatusConditionTrue(cfg.Status.Conditions, costv1alpha1.ConditionGatewayReady) {
		t.Fatalf("expected GatewayReady=True, got %+v",
			findCondition(cfg.Status.Conditions, costv1alpha1.ConditionGatewayReady))
	}
	uiCond := findCondition(cfg.Status.Conditions, costv1alpha1.ConditionUIReady)
	if uiCond == nil || uiCond.Status != metav1.ConditionFalse || uiCond.Reason != "RouteNotAdmitted" {
		t.Fatalf("expected UIReady=False RouteNotAdmitted, got %+v", uiCond)
	}

	uiRoute := &unstructured.Unstructured{}
	uiRoute.SetGroupVersionKind(routeGVK())
	mustExist(t, r.Client, testNamespace, cfg.Name+"-ui", uiRoute)
	mustExist(t, r.Client, testNamespace, resources.NameUI(cfg), &appsv1.Deployment{})
	cl := &unstructured.Unstructured{}
	cl.SetGroupVersionKind(schema.GroupVersionKind{Group: "console.openshift.io", Version: "v1", Kind: "ConsoleLink"})
	mustNotExist(t, r.Client, "", resources.NameConsoleLink(cfg), cl)
}

func TestReconcileUI_AdmittedRoute_SetsUIReady(t *testing.T) {
	scheme := ownershipScheme(t)
	cfg := minimalCR(testCRName, testNamespace)
	cfg.Status.DiscoveredConfig = &costv1alpha1.DiscoveredConfig{ClusterDomain: "apps.example.com"}

	oauth := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      resources.NameUIOAuthClientSecret(cfg),
			Namespace: testNamespace,
		},
		Data: map[string][]byte{
			"client-id":     []byte("cost-ui"),
			"client-secret": []byte("s3cret"),
		},
	}
	uiRoute := resources.UIRoute(cfg)
	if uiRoute == nil {
		t.Fatal("expected UI Route")
	}
	if err := unstructured.SetNestedSlice(uiRoute.Object, []any{
		map[string]any{
			"host": "ui.apps.example.com",
			"conditions": []any{
				map[string]any{"type": routeAdmittedType, "status": "True"},
			},
		},
	}, "status", "ingress"); err != nil {
		t.Fatalf("seed UI Route status: %v", err)
	}

	r := &CostManagementServiceConfigReconciler{
		Client:   fakeClientPreservingStatus(scheme, oauth, uiRoute),
		Scheme:   scheme,
		Recorder: &noopRecorder{},
	}
	if err := r.reconcileUI(context.Background(), cfg); err != nil {
		t.Fatalf("reconcileUI: %v", err)
	}
	if !apimeta.IsStatusConditionTrue(cfg.Status.Conditions, costv1alpha1.ConditionUIReady) {
		t.Fatalf("expected UIReady=True after UI Route admission, got %+v",
			findCondition(cfg.Status.Conditions, costv1alpha1.ConditionUIReady))
	}
	cl := &unstructured.Unstructured{}
	cl.SetGroupVersionKind(schema.GroupVersionKind{Group: "console.openshift.io", Version: "v1", Kind: "ConsoleLink"})
	mustExist(t, r.Client, "", resources.NameConsoleLink(cfg), cl)
}

func mustIsolationNetworkPolicies(t *testing.T, c client.Client, cfg *costv1alpha1.CostManagementServiceConfig) {
	t.Helper()
	for _, suffix := range []string{
		"-gateway", "-ingress", "-rbac-api", "-koku-api",
		"-ui", "-listener", "-masu", "-cache", "-database",
	} {
		mustExist(t, c, testNamespace, cfg.Name+suffix, &networkingv1.NetworkPolicy{})
	}
}
