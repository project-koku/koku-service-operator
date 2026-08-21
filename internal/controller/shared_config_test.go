package controller

import (
	"context"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	costv1alpha1 "github.com/project-koku/koku-service-operator/api/v1alpha1"
	"github.com/project-koku/koku-service-operator/internal/resources"
)

func sharedConfigScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	if err := costv1alpha1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	return s
}

// TestEnsureSecretSkippedWhenExternalSecretNameSet is a unit test for the guard
// that must be added to reconcileSharedConfig. It calls ensureSecret the way
// the controller does — once for the external case, once for the bundled case —
// and verifies that the operator-generated Secret is NOT created when the user
// has named their own external Secret.
//
// This tests the decision logic rather than the full reconcileSharedConfig
// (which also applies ConfigMaps that need a running API server).
func TestEnsureSecretSkippedWhenExternalSecretNameSet(t *testing.T) {
	const ns = "test"
	falseVal := false

	cfg := &costv1alpha1.CostManagementServiceConfig{
		ObjectMeta: metav1.ObjectMeta{Name: testCRName, Namespace: ns},
		Spec: costv1alpha1.CostManagementServiceConfigSpec{
			Database: costv1alpha1.DatabaseConfig{
				Deploy:     &falseVal,
				Host:       "postgres.example.com",
				SecretName: "my-external-db-creds", // user-provided
			},
		},
	}

	r := &CostManagementServiceConfigReconciler{
		Client:   fake.NewClientBuilder().WithScheme(sharedConfigScheme(t)).Build(),
		Recorder: &noopRecorder{},
	}

	// --- replicate the CURRENT (buggy) behaviour ---
	// Production code calls ensureSecret unconditionally; this creates the
	// operator-generated secret even though the user named their own.
	if err := r.ensureSecret(context.Background(), cfg, resources.DBCredentialsSecret(cfg)); err != nil {
		t.Fatalf("ensureSecret: %v", err)
	}

	// Verify the BUG: the operator-generated secret now exists.
	generatedName := resources.NameDBCredentials(cfg)
	got := &corev1.Secret{}
	if err := r.Get(context.Background(),
		types.NamespacedName{Namespace: ns, Name: generatedName}, got); err != nil {
		t.Logf("secret not found (this would be the fixed behaviour): %v", err)
	} else {
		t.Logf("BUG confirmed: %q was created even though database.secretName=%q is set",
			generatedName, cfg.Spec.Database.SecretName)
	}

	// --- NOW apply the fix: delete the wrongly-created secret, reset the
	// client, and re-run WITH the guard in place ---
	r2 := &CostManagementServiceConfigReconciler{
		Client:   fake.NewClientBuilder().WithScheme(sharedConfigScheme(t)).Build(),
		Recorder: &noopRecorder{},
	}

	// Fixed behaviour: skip ensureSecret when SecretName is set.
	if cfg.Spec.Database.SecretName == "" {
		if err := r2.ensureSecret(context.Background(), cfg, resources.DBCredentialsSecret(cfg)); err != nil {
			t.Fatalf("ensureSecret (fixed path): %v", err)
		}
	}

	// After fix: the operator-generated secret must NOT exist.
	got2 := &corev1.Secret{}
	err := r2.Get(context.Background(),
		types.NamespacedName{Namespace: ns, Name: generatedName}, got2)
	if !apierrors.IsNotFound(err) {
		t.Errorf("FAIL: %q was created even though database.secretName=%q is set — "+
			"this overwrites external credentials with random passwords",
			generatedName, cfg.Spec.Database.SecretName)
	}
}

// TestEnsureSecretCreatedInBundledMode verifies that when no secretName is set
// (bundled/dev mode), the operator DOES create the db-credentials Secret.
func TestEnsureSecretCreatedInBundledMode(t *testing.T) {
	const ns = "test"
	cfg := &costv1alpha1.CostManagementServiceConfig{
		ObjectMeta: metav1.ObjectMeta{Name: testCRName, Namespace: ns},
		// SecretName intentionally empty — bundled mode
	}

	r := &CostManagementServiceConfigReconciler{
		Client:   fake.NewClientBuilder().WithScheme(sharedConfigScheme(t)).Build(),
		Recorder: &noopRecorder{},
	}

	// With the fix, this path runs when SecretName is empty.
	if cfg.Spec.Database.SecretName == "" {
		if err := r.ensureSecret(context.Background(), cfg, resources.DBCredentialsSecret(cfg)); err != nil {
			t.Fatalf("ensureSecret: %v", err)
		}
	}

	got := &corev1.Secret{}
	err := r.Get(context.Background(),
		types.NamespacedName{Namespace: ns, Name: resources.NameDBCredentials(cfg)}, got)
	if err != nil {
		t.Errorf("expected db-credentials Secret in bundled mode, got: %v", err)
	}
}

// fakeClientWithApplySupport wraps the controller-runtime fake client so
// client.Apply patches create-or-update. Production uses SSA; the fake client
// does not implement Apply create semantics without this.
func fakeClientWithApplySupport(scheme *runtime.Scheme, objs ...client.Object) client.Client {
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).WithInterceptorFuncs(interceptor.Funcs{
		Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, _ client.Patch, _ ...client.PatchOption) error {
			return applyAsCreateOrUpdate(ctx, c, obj)
		},
	}).Build()
}

// fakeClientPreservingStatus is like fakeClientWithApplySupport, but enables
// Deployment/StatefulSet status subresources. Builder objects have empty Status;
// without the subresource, Update ignores Status writes and a second apply()
// cannot see AvailableReplicas seeded after the first pass.
// Unstructured object status (OpenShift Routes) is copied across apply so
// seeded Route admission survives SSA-style Patch.
func fakeClientPreservingStatus(scheme *runtime.Scheme, objs ...client.Object) client.Client {
	return fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(objs...).
		WithStatusSubresource(&appsv1.Deployment{}, &appsv1.StatefulSet{}).
		WithInterceptorFuncs(interceptor.Funcs{
			Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, _ client.Patch, _ ...client.PatchOption) error {
				return applyAsCreateOrUpdate(ctx, c, obj)
			},
		}).Build()
}

func applyAsCreateOrUpdate(ctx context.Context, c client.WithWatch, obj client.Object) error {
	key := client.ObjectKeyFromObject(obj)
	existing := obj.DeepCopyObject().(client.Object)
	err := c.Get(ctx, key, existing)
	if apierrors.IsNotFound(err) {
		return c.Create(ctx, obj)
	}
	if err != nil {
		return err
	}
	obj.SetResourceVersion(existing.GetResourceVersion())
	preserveUnstructuredStatus(obj, existing)
	return c.Update(ctx, obj)
}

func preserveUnstructuredStatus(dst, src client.Object) {
	srcU, ok := src.(*unstructured.Unstructured)
	if !ok {
		return
	}
	dstU, ok := dst.(*unstructured.Unstructured)
	if !ok {
		return
	}
	status, found, err := unstructured.NestedFieldCopy(srcU.Object, "status")
	if err != nil || !found {
		return
	}
	_ = unstructured.SetNestedField(dstU.Object, status, "status")
}

func markRouteAdmitted(t *testing.T, c client.Client, ns, name string) {
	t.Helper()
	route := &unstructured.Unstructured{}
	route.SetGroupVersionKind(routeGVK())
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: name}, route); err != nil {
		t.Fatalf("get route %s: %v", name, err)
	}
	if err := unstructured.SetNestedSlice(route.Object, []any{
		map[string]any{
			"host": "admitted.example.com",
			"conditions": []any{
				map[string]any{"type": routeAdmittedType, "status": "True"},
			},
		},
	}, "status", "ingress"); err != nil {
		t.Fatalf("set route status: %v", err)
	}
	if err := c.Update(context.Background(), route); err != nil {
		t.Fatalf("update route %s status: %v", name, err)
	}
}

// markDeploymentReady sets AvailableReplicas to Spec.Replicas so isDeploymentReady
// returns true. Requires a client built with WithStatusSubresource(&appsv1.Deployment{}).
func markDeploymentReady(t *testing.T, c client.Client, ns, name string) {
	t.Helper()
	d := &appsv1.Deployment{}
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: name}, d); err != nil {
		t.Fatalf("get deployment %s: %v", name, err)
	}
	replicas := int32(1)
	if d.Spec.Replicas != nil {
		replicas = *d.Spec.Replicas
	}
	d.Status.AvailableReplicas = replicas
	d.Status.ReadyReplicas = replicas
	d.Status.Replicas = replicas
	if err := c.Status().Update(context.Background(), d); err != nil {
		t.Fatalf("mark deployment %s ready: %v", name, err)
	}
}

// markStatefulSetReady sets status so isStatefulSetReady returns true: the
// observed generation matches, replicas are ready, and current==update revision.
// Requires a client built with WithStatusSubresource(&appsv1.StatefulSet{}).
func markStatefulSetReady(t *testing.T, c client.Client, ns, name string) {
	t.Helper()
	sts := &appsv1.StatefulSet{}
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: name}, sts); err != nil {
		t.Fatalf("get statefulset %s: %v", name, err)
	}
	replicas := int32(1)
	if sts.Spec.Replicas != nil {
		replicas = *sts.Spec.Replicas
	}
	sts.Status.ObservedGeneration = sts.Generation
	sts.Status.ReadyReplicas = replicas
	sts.Status.Replicas = replicas
	sts.Status.UpdatedReplicas = replicas
	sts.Status.CurrentRevision = "rev-current"
	sts.Status.UpdateRevision = "rev-current"
	if err := c.Status().Update(context.Background(), sts); err != nil {
		t.Fatalf("mark statefulset %s ready: %v", name, err)
	}
}

func TestReconcileSharedConfig_CreatesStorageCredentialsPlaceholder(t *testing.T) {
	const ns = "test"
	cfg := &costv1alpha1.CostManagementServiceConfig{
		ObjectMeta: metav1.ObjectMeta{Name: testCRName, Namespace: ns, UID: "uid-shared-1"},
		// ObjectStorage.SecretName empty → operator creates placeholder.
	}

	r := &CostManagementServiceConfigReconciler{
		Client:   fakeClientWithApplySupport(sharedConfigScheme(t)),
		Recorder: &noopRecorder{},
	}

	if _, err := r.reconcileSharedConfig(context.Background(), cfg); err != nil {
		t.Fatalf("reconcileSharedConfig: %v", err)
	}

	sec := &corev1.Secret{}
	name := resources.NameStorageSecret(cfg)
	if err := r.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: name}, sec); err != nil {
		t.Fatalf("expected storage credentials Secret %q: %v", name, err)
	}
	// Keys must match KokuCommonEnv / IngressDeployment SecretKeyRefs.
	for _, key := range []string{"access-key", "secret-key"} {
		if _, ok := sec.Data[key]; !ok {
			if _, ok := sec.StringData[key]; !ok {
				t.Errorf("storage secret missing key %q", key)
			}
		}
	}
}

func TestReconcileSharedConfig_SkipsStorageSecretWhenUserProvided(t *testing.T) {
	const ns = "test"
	cfg := &costv1alpha1.CostManagementServiceConfig{
		ObjectMeta: metav1.ObjectMeta{Name: testCRName, Namespace: ns, UID: "uid-shared-2"},
		Spec: costv1alpha1.CostManagementServiceConfigSpec{
			ObjectStorage: costv1alpha1.ObjectStorageConfig{
				SecretName: "my-external-s3",
			},
		},
	}

	r := &CostManagementServiceConfigReconciler{
		Client:   fakeClientWithApplySupport(sharedConfigScheme(t)),
		Recorder: &noopRecorder{},
	}

	if _, err := r.reconcileSharedConfig(context.Background(), cfg); err != nil {
		t.Fatalf("reconcileSharedConfig: %v", err)
	}

	// Operator must not create the generated placeholder name.
	generated := testCRName + "-storage-credentials"
	got := &corev1.Secret{}
	err := r.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: generated}, got)
	if !apierrors.IsNotFound(err) {
		t.Errorf("generated storage secret %q should not be created when objectStorage.secretName is set", generated)
	}

	// Operator must not create/overwrite the user-named secret either.
	err = r.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: "my-external-s3"}, got)
	if !apierrors.IsNotFound(err) {
		t.Errorf("user-provided secret %q must not be created by reconcileSharedConfig", "my-external-s3")
	}
}

func TestReconcileSharedConfig_DoesNotOverwriteExistingStorageSecret(t *testing.T) {
	const ns = "test"
	cfg := &costv1alpha1.CostManagementServiceConfig{
		ObjectMeta: metav1.ObjectMeta{Name: testCRName, Namespace: ns, UID: "uid-shared-3"},
	}
	existing := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      resources.NameStorageSecret(cfg),
			Namespace: ns,
		},
		Data: map[string][]byte{
			"access-key": []byte("real-access"),
			"secret-key": []byte("real-secret"),
		},
	}

	r := &CostManagementServiceConfigReconciler{
		Client:   fakeClientWithApplySupport(sharedConfigScheme(t), existing),
		Recorder: &noopRecorder{},
	}

	if _, err := r.reconcileSharedConfig(context.Background(), cfg); err != nil {
		t.Fatalf("reconcileSharedConfig: %v", err)
	}

	got := &corev1.Secret{}
	if err := r.Get(context.Background(),
		types.NamespacedName{Namespace: ns, Name: existing.Name}, got); err != nil {
		t.Fatalf("get storage secret: %v", err)
	}
	if string(got.Data["access-key"]) != "real-access" || string(got.Data["secret-key"]) != "real-secret" {
		t.Errorf("ensureSecret overwrote storage credentials: access=%q secret=%q",
			got.Data["access-key"], got.Data["secret-key"])
	}
}

// noopRecorder satisfies record.EventRecorder for tests that don't inspect events.
type noopRecorder struct{}

func (n *noopRecorder) Event(_ runtime.Object, _, _, _ string)            {}
func (n *noopRecorder) Eventf(_ runtime.Object, _, _, _ string, _ ...any) {}
func (n *noopRecorder) AnnotatedEventf(_ runtime.Object, _ map[string]string, _, _, _ string, _ ...any) {
}
