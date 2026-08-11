package controller

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	costv1alpha1 "github.com/project-koku/koku-service-operator/api/v1alpha1"
)

const testClusterDomain = "apps.example.com"

func objectBucketClaim(name, ns, phase string) *unstructured.Unstructured {
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(objectBucketClaimGVK())
	obj.SetName(name)
	obj.SetNamespace(ns)
	_ = unstructured.SetNestedField(obj.Object, phase, "status", "phase")
	return obj
}

func obcConfigMap(name, ns, host, port, bucket string) *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Data: map[string]string{
			"BUCKET_HOST": host,
			"BUCKET_PORT": port,
			"BUCKET_NAME": bucket,
		},
	}
}

func obcSecret(name, ns, accessKey, secretKey string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Data: map[string][]byte{
			"AWS_ACCESS_KEY_ID":     []byte(accessKey),
			"AWS_SECRET_ACCESS_KEY": []byte(secretKey),
		},
	}
}

func noobaaAdminSecret(accessKey, secretKey string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "noobaa-admin",
			Namespace: "openshift-storage",
		},
		Data: map[string][]byte{
			"AWS_ACCESS_KEY_ID":     []byte(accessKey),
			"AWS_SECRET_ACCESS_KEY": []byte(secretKey),
		},
	}
}

func boolPtr(b bool) *bool { return &b }

func assertStorageCondition(t *testing.T, cfg *costv1alpha1.CostManagementServiceConfig, wantStatus metav1.ConditionStatus, wantReason string) {
	t.Helper()
	for _, cond := range cfg.Status.Conditions {
		if cond.Type == costv1alpha1.ConditionStorageReady {
			if cond.Status != wantStatus {
				t.Errorf("StorageReady status: got %s, want %s", cond.Status, wantStatus)
			}
			if wantReason != "" && cond.Reason != wantReason {
				t.Errorf("StorageReady reason: got %s, want %s", cond.Reason, wantReason)
			}
			return
		}
	}
	t.Error("StorageReady condition not set")
}

func TestResolveS3_UserProvided(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	r := &CostManagementServiceConfigReconciler{Client: c}
	cfg := &costv1alpha1.CostManagementServiceConfig{
		ObjectMeta: metav1.ObjectMeta{Name: testCRName, Namespace: testNamespace},
		Spec: costv1alpha1.CostManagementServiceConfigSpec{
			ObjectStorage: costv1alpha1.ObjectStorageConfig{
				Endpoint:   "minio.cost-byoi-infra.svc.cluster.local",
				Port:       9000,
				UseSSL:     boolPtr(false),
				SecretName: "byoi-s3-credentials",
				S3:         costv1alpha1.S3Options{Region: defaultS3Region},
			},
		},
	}

	got, err := r.resolveS3(context.Background(), cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Endpoint != "http://minio.cost-byoi-infra.svc.cluster.local:9000" {
		t.Errorf("Endpoint: got %q", got.Endpoint)
	}
	if got.SecretName != "byoi-s3-credentials" {
		t.Errorf("SecretName: got %q", got.SecretName)
	}
	if got.Region != defaultS3Region {
		t.Errorf("Region: got %q", got.Region)
	}
}

func TestResolveS3_OBC(t *testing.T) {
	obcName := "ros-data-ceph"
	c := fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithObjects(
			objectBucketClaim(obcName, testNamespace, "Bound"),
			obcConfigMap(obcName, testNamespace, "rook-ceph-rgw.openshift-storage.svc", "443", "ros-data"),
			obcSecret(obcName, testNamespace, "ak-obc", "sk-obc"),
		).
		Build()

	r := &CostManagementServiceConfigReconciler{Client: c}
	cfg := &costv1alpha1.CostManagementServiceConfig{
		ObjectMeta: metav1.ObjectMeta{Name: testCRName, Namespace: testNamespace},
	}

	got, err := r.resolveS3(context.Background(), cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Endpoint != "https://rook-ceph-rgw.openshift-storage.svc:443" {
		t.Errorf("Endpoint: got %q", got.Endpoint)
	}
	wantSecret := testCRName + "-storage-credentials"
	if got.SecretName != wantSecret {
		t.Errorf("SecretName: got %q, want %q", got.SecretName, wantSecret)
	}

	// Credentials should be copied into the app storage secret.
	sec := &corev1.Secret{}
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: testNamespace, Name: wantSecret}, sec); err != nil {
		t.Fatalf("storage secret not created: %v", err)
	}
	if string(sec.Data["access-key"]) != "ak-obc" || string(sec.Data["secret-key"]) != "sk-obc" {
		t.Errorf("secret data: access=%q secret=%q", sec.Data["access-key"], sec.Data["secret-key"])
	}
}

func TestResolveS3_NooBaa(t *testing.T) {
	c := fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithObjects(noobaaAdminSecret("ak-nb", "sk-nb")).
		Build()

	r := &CostManagementServiceConfigReconciler{Client: c}
	cfg := &costv1alpha1.CostManagementServiceConfig{
		ObjectMeta: metav1.ObjectMeta{Name: testCRName, Namespace: testNamespace},
	}

	got, err := r.resolveS3(context.Background(), cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Endpoint != "https://s3.openshift-storage.svc.cluster.local:443" {
		t.Errorf("Endpoint: got %q", got.Endpoint)
	}
	wantSecret := testCRName + "-storage-credentials"
	if got.SecretName != wantSecret {
		t.Errorf("SecretName: got %q, want %q", got.SecretName, wantSecret)
	}
	sec := &corev1.Secret{}
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: testNamespace, Name: wantSecret}, sec); err != nil {
		t.Fatalf("storage secret not created: %v", err)
	}
	if string(sec.Data["access-key"]) != "ak-nb" {
		t.Errorf("access-key: got %q", sec.Data["access-key"])
	}
}

// TestResolveS3_NooBaaPrefersAPIReader simulates OwnNamespace cache: the
// cached Client cannot see openshift-storage/noobaa-admin, but APIReader can.
func TestResolveS3_NooBaaPrefersAPIReader(t *testing.T) {
	scheme := testScheme(t)
	cached := fake.NewClientBuilder().WithScheme(scheme).Build()
	apiReader := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(noobaaAdminSecret("ak-api", "sk-api")).
		Build()

	r := &CostManagementServiceConfigReconciler{Client: cached, APIReader: apiReader}
	cfg := &costv1alpha1.CostManagementServiceConfig{
		ObjectMeta: metav1.ObjectMeta{Name: testCRName, Namespace: testNamespace},
	}

	got, err := r.resolveS3(context.Background(), cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Endpoint != "https://s3.openshift-storage.svc.cluster.local:443" {
		t.Errorf("Endpoint: got %q", got.Endpoint)
	}
	wantSecret := testCRName + "-storage-credentials"
	sec := &corev1.Secret{}
	if err := cached.Get(context.Background(), types.NamespacedName{Namespace: testNamespace, Name: wantSecret}, sec); err != nil {
		t.Fatalf("storage secret not created in watch NS: %v", err)
	}
	if string(sec.Data["access-key"]) != "ak-api" || string(sec.Data["secret-key"]) != "sk-api" {
		t.Errorf("secret data: access=%q secret=%q", sec.Data["access-key"], sec.Data["secret-key"])
	}
}

func TestResolveS3_None(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	r := &CostManagementServiceConfigReconciler{Client: c}
	cfg := &costv1alpha1.CostManagementServiceConfig{
		ObjectMeta: metav1.ObjectMeta{Name: testCRName, Namespace: testNamespace},
	}

	_, err := r.resolveS3(context.Background(), cfg)
	if err == nil {
		t.Fatal("expected error when no S3 backend found")
	}
}

func TestReconcileDiscovery_UserProvidedS3_SetsStorageReady(t *testing.T) {
	c := fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithObjects(
			openShiftIngress(testClusterDomain),
			defaultStorageClass("gp3-csi"),
		).
		Build()

	r := &CostManagementServiceConfigReconciler{Client: c}
	cfg := &costv1alpha1.CostManagementServiceConfig{
		ObjectMeta: metav1.ObjectMeta{Name: testCRName, Namespace: testNamespace},
		Spec: costv1alpha1.CostManagementServiceConfigSpec{
			Global: costv1alpha1.GlobalConfig{
				ClusterDomain: testClusterDomain,
				StorageClass:  "gp3-csi",
			},
			ObjectStorage: costv1alpha1.ObjectStorageConfig{
				Endpoint:   "minio.example.svc",
				Port:       9000,
				UseSSL:     boolPtr(false),
				SecretName: "my-s3",
			},
		},
	}

	result, err := r.reconcileDiscovery(context.Background(), cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.IsZero() {
		t.Errorf("expected zero result, got %+v", result)
	}
	assertDiscoveryCondition(t, cfg, metav1.ConditionTrue, "Discovered")
	assertStorageCondition(t, cfg, metav1.ConditionTrue, "UserProvided")
	if cfg.Status.DiscoveredConfig.S3 == nil || cfg.Status.DiscoveredConfig.S3.SecretName != "my-s3" {
		t.Fatalf("DiscoveredConfig.S3 = %+v", cfg.Status.DiscoveredConfig.S3)
	}
}

func TestReconcileDiscovery_NoS3_SetsStorageReadyFalse_Continues(t *testing.T) {
	c := fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithObjects(
			openShiftIngress(testClusterDomain),
			defaultStorageClass("gp3-csi"),
		).
		Build()

	r := &CostManagementServiceConfigReconciler{Client: c}
	cfg := &costv1alpha1.CostManagementServiceConfig{
		ObjectMeta: metav1.ObjectMeta{Name: testCRName, Namespace: testNamespace},
		Spec: costv1alpha1.CostManagementServiceConfigSpec{
			Global: costv1alpha1.GlobalConfig{
				ClusterDomain: testClusterDomain,
				StorageClass:  "gp3-csi",
			},
		},
	}

	result, err := r.reconcileDiscovery(context.Background(), cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Pipeline continues — clusterbot / BYOI often has no ODF.
	if !result.IsZero() {
		t.Errorf("expected zero result (continue), got %+v", result)
	}
	assertDiscoveryCondition(t, cfg, metav1.ConditionTrue, "Discovered")
	assertStorageCondition(t, cfg, metav1.ConditionFalse, "S3NotFound")
}
