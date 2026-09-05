package controller

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
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
	return noobaaAdminSecretIn("openshift-storage", accessKey, secretKey)
}

func noobaaAdminSecretIn(ns, accessKey, secretKey string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "noobaa-admin",
			Namespace: ns,
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
	r := &CostManagementServiceConfigReconciler{Client: c, Recorder: record.NewFakeRecorder(10)}
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
			CostManagement: costv1alpha1.CostManagementConfig{
				Storage: costv1alpha1.CostManagementStorageSpec{
					BucketName: "koku-bucket",
				},
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
	if got.Bucket != "koku-bucket" {
		t.Errorf("Bucket: got %q, want koku-bucket", got.Bucket)
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

	r := &CostManagementServiceConfigReconciler{Client: c, Recorder: record.NewFakeRecorder(10)}
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
	if got.Bucket != "ros-data" {
		t.Errorf("Bucket: got %q, want ros-data", got.Bucket)
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

	r := &CostManagementServiceConfigReconciler{Client: c, Recorder: record.NewFakeRecorder(10)}
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
	if got.Bucket != "" {
		t.Errorf("Bucket: got %q, want empty (spec bucketName unset)", got.Bucket)
	}
}

// TestResolveS3_NooBaaPrefersAPIReader simulates a cache miss: the
// cached Client cannot see openshift-storage/noobaa-admin, but APIReader can.
func TestResolveS3_NooBaaPrefersAPIReader(t *testing.T) {
	scheme := testScheme(t)
	cached := fake.NewClientBuilder().WithScheme(scheme).Build()
	apiReader := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(noobaaAdminSecret("ak-api", "sk-api")).
		Build()

	r := &CostManagementServiceConfigReconciler{Client: cached, APIReader: apiReader, Recorder: record.NewFakeRecorder(10)}
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

func TestDiscoverOBC_MissingBucketName(t *testing.T) {
	obcName := "ros-data-ceph"
	c := fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithObjects(
			objectBucketClaim(obcName, testNamespace, "Bound"),
			&corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{Name: obcName, Namespace: testNamespace},
				Data: map[string]string{
					"BUCKET_HOST": "rook-ceph-rgw.openshift-storage.svc",
					"BUCKET_PORT": "443",
				},
			},
			obcSecret(obcName, testNamespace, "ak-obc", "sk-obc"),
		).
		Build()

	r := &CostManagementServiceConfigReconciler{Client: c, Recorder: record.NewFakeRecorder(10)}
	cfg := &costv1alpha1.CostManagementServiceConfig{
		ObjectMeta: metav1.ObjectMeta{Name: testCRName, Namespace: testNamespace},
	}

	got, err := r.discoverOBC(context.Background(), cfg)
	if err == nil {
		t.Fatal("expected error when OBC ConfigMap lacks BUCKET_NAME")
	}
	if got != nil {
		t.Fatalf("expected nil DiscoveredS3 from OBC path, got %+v", got)
	}
}

func TestDiscoverOBC_EmptyBucketName(t *testing.T) {
	obcName := "ros-data-ceph"
	c := fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithObjects(
			objectBucketClaim(obcName, testNamespace, "Bound"),
			obcConfigMap(obcName, testNamespace, "rook-ceph-rgw.openshift-storage.svc", "443", ""),
			obcSecret(obcName, testNamespace, "ak-obc", "sk-obc"),
		).
		Build()

	r := &CostManagementServiceConfigReconciler{Client: c, Recorder: record.NewFakeRecorder(10)}
	cfg := &costv1alpha1.CostManagementServiceConfig{
		ObjectMeta: metav1.ObjectMeta{Name: testCRName, Namespace: testNamespace},
	}

	got, err := r.discoverOBC(context.Background(), cfg)
	if err == nil {
		t.Fatal("expected error when OBC ConfigMap BUCKET_NAME is empty")
	}
	if got != nil {
		t.Fatalf("expected nil DiscoveredS3 from OBC path, got %+v", got)
	}
}

func TestResolveS3_OBCMissingBucketName(t *testing.T) {
	obcName := "ros-data-ceph"
	c := fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithObjects(
			objectBucketClaim(obcName, testNamespace, "Bound"),
			&corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{Name: obcName, Namespace: testNamespace},
				Data: map[string]string{
					"BUCKET_HOST": "rook-ceph-rgw.openshift-storage.svc",
					"BUCKET_PORT": "443",
				},
			},
			obcSecret(obcName, testNamespace, "ak-obc", "sk-obc"),
		).
		Build()

	r := &CostManagementServiceConfigReconciler{Client: c, Recorder: record.NewFakeRecorder(10)}
	cfg := &costv1alpha1.CostManagementServiceConfig{
		ObjectMeta: metav1.ObjectMeta{Name: testCRName, Namespace: testNamespace},
	}

	got, err := r.resolveS3(context.Background(), cfg)
	if err == nil {
		if got != nil && got.Bucket == "" {
			t.Fatalf("OBC path must not succeed with empty Bucket: %+v", got)
		}
		t.Fatalf("expected resolveS3 to fail without NooBaa fallback, got %+v", got)
	}
}

func TestNoobaaNamespace(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		ns   string
		want string
	}{
		{name: "unset defaults to openshift-storage", want: "openshift-storage"},
		{name: "explicit noobaa", ns: "noobaa", want: "noobaa"},
		{name: "explicit openshift-storage", ns: "openshift-storage", want: "openshift-storage"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cfg := &costv1alpha1.CostManagementServiceConfig{}
			cfg.Spec.ObjectStorage.NoobaaNamespace = tt.ns
			if got := noobaaNamespace(cfg); got != tt.want {
				t.Errorf("noobaaNamespace() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestNoobaaEndpoint(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		ns       string
		endpoint string
		port     int32
		useSSL   *bool
		want     string
	}{
		{
			name: "unset host and ns uses default in-cluster service",
			want: "https://s3.openshift-storage.svc.cluster.local:443",
		},
		{
			name: "custom ns builds s3.<ns>.svc",
			ns:   "noobaa",
			want: "https://s3.noobaa.svc.cluster.local:443",
		},
		{
			name:     "CRD default host is not a custom route",
			ns:       "noobaa",
			endpoint: "s3.openshift-storage.svc.cluster.local",
			want:     "https://s3.noobaa.svc.cluster.local:443",
		},
		{
			name:     "custom host uses spec port and SSL",
			endpoint: "s3.apps.example.com",
			port:     443,
			useSSL:   boolPtr(true),
			want:     "https://s3.apps.example.com:443",
		},
		{
			name:     "custom host honors UseSSL false and port",
			endpoint: "s3-noobaa.apps.example.com",
			port:     80,
			useSSL:   boolPtr(false),
			want:     "http://s3-noobaa.apps.example.com:80",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cfg := &costv1alpha1.CostManagementServiceConfig{}
			cfg.Spec.ObjectStorage.NoobaaNamespace = tt.ns
			cfg.Spec.ObjectStorage.Endpoint = tt.endpoint
			cfg.Spec.ObjectStorage.Port = tt.port
			cfg.Spec.ObjectStorage.UseSSL = tt.useSSL
			if got := noobaaEndpoint(cfg); got != tt.want {
				t.Errorf("noobaaEndpoint() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestNoobaaEndpointIgnoresDiscoveredStatus(t *testing.T) {
	t.Parallel()
	cfg := &costv1alpha1.CostManagementServiceConfig{}
	cfg.Spec.ObjectStorage.Endpoint = "s3.apps.example.com"
	cfg.Spec.ObjectStorage.Port = 443
	cfg.Spec.ObjectStorage.UseSSL = boolPtr(true)
	cfg.Status.DiscoveredConfig = &costv1alpha1.DiscoveredConfig{
		S3: &costv1alpha1.DiscoveredS3{Endpoint: "https://s3.openshift-storage.svc.cluster.local:443"},
	}
	got := noobaaEndpoint(cfg)
	if got != "https://s3.apps.example.com:443" {
		t.Errorf("noobaaEndpoint() = %q, want custom host (must not reuse status.discoveredConfig.s3)", got)
	}
}

func TestResolveS3_NooBaaCustomNamespace(t *testing.T) {
	c := fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithObjects(noobaaAdminSecretIn("noobaa", "ak-nb", "sk-nb")).
		Build()

	r := &CostManagementServiceConfigReconciler{Client: c, Recorder: record.NewFakeRecorder(10)}
	cfg := &costv1alpha1.CostManagementServiceConfig{
		ObjectMeta: metav1.ObjectMeta{Name: testCRName, Namespace: testNamespace},
		Spec: costv1alpha1.CostManagementServiceConfigSpec{
			ObjectStorage: costv1alpha1.ObjectStorageConfig{
				NoobaaNamespace: "noobaa",
			},
		},
	}

	got, err := r.resolveS3(context.Background(), cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Endpoint != "https://s3.noobaa.svc.cluster.local:443" {
		t.Errorf("Endpoint: got %q, want https://s3.noobaa.svc.cluster.local:443", got.Endpoint)
	}
}

func TestResolveS3_NooBaaUnsetNamespaceMissesOtherNS(t *testing.T) {
	c := fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithObjects(noobaaAdminSecretIn("noobaa", "ak-nb", "sk-nb")).
		Build()

	r := &CostManagementServiceConfigReconciler{Client: c, Recorder: record.NewFakeRecorder(10)}
	cfg := &costv1alpha1.CostManagementServiceConfig{
		ObjectMeta: metav1.ObjectMeta{Name: testCRName, Namespace: testNamespace},
	}

	got, err := r.resolveS3(context.Background(), cfg)
	if err == nil {
		t.Fatalf("expected NooBaa path to fail when secret is only in noobaa, got %+v", got)
	}
}

func TestNoobaaNamespaceAllowed(t *testing.T) {
	t.Parallel()
	tests := []struct {
		ns   string
		want bool
	}{
		{ns: "openshift-storage", want: true},
		{ns: "noobaa", want: true},
		{ns: "kube-system", want: false},
		{ns: "openshift-monitoring", want: false},
		{ns: testNamespace, want: false},
	}
	for _, tt := range tests {
		t.Run(tt.ns, func(t *testing.T) {
			t.Parallel()
			if got := noobaaNamespaceAllowed(tt.ns); got != tt.want {
				t.Errorf("noobaaNamespaceAllowed(%q) = %v, want %v", tt.ns, got, tt.want)
			}
		})
	}
}

func TestResolveS3_NooBaaDisallowedNamespaceDoesNotCopySecret(t *testing.T) {
	const foreignNS = "kube-system"
	c := fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithObjects(noobaaAdminSecretIn(foreignNS, "ak-stolen", "sk-stolen")).
		Build()

	r := &CostManagementServiceConfigReconciler{Client: c, Recorder: record.NewFakeRecorder(10)}
	cfg := &costv1alpha1.CostManagementServiceConfig{
		ObjectMeta: metav1.ObjectMeta{Name: testCRName, Namespace: testNamespace},
		Spec: costv1alpha1.CostManagementServiceConfigSpec{
			ObjectStorage: costv1alpha1.ObjectStorageConfig{
				NoobaaNamespace: foreignNS,
			},
		},
	}

	got, err := r.resolveS3(context.Background(), cfg)
	if err == nil {
		t.Fatalf("expected disallowed noobaaNamespace to fail, got %+v", got)
	}
	wantSecret := testCRName + "-storage-credentials"
	sec := &corev1.Secret{}
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: testNamespace, Name: wantSecret}, sec); err == nil {
		t.Fatalf("must not copy noobaa-admin from %s into %s/%s", foreignNS, testNamespace, wantSecret)
	}
}

func TestResolveS3_NooBaaCustomEndpoint(t *testing.T) {
	c := fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithObjects(noobaaAdminSecret("ak-nb", "sk-nb")).
		Build()

	r := &CostManagementServiceConfigReconciler{Client: c, Recorder: record.NewFakeRecorder(10)}
	cfg := &costv1alpha1.CostManagementServiceConfig{
		ObjectMeta: metav1.ObjectMeta{Name: testCRName, Namespace: testNamespace},
		Spec: costv1alpha1.CostManagementServiceConfigSpec{
			ObjectStorage: costv1alpha1.ObjectStorageConfig{
				Endpoint: "s3.apps.example.com",
				Port:     443,
				UseSSL:   boolPtr(true),
			},
		},
	}

	got, err := r.resolveS3(context.Background(), cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Endpoint != "https://s3.apps.example.com:443" {
		t.Errorf("Endpoint: got %q, want custom route", got.Endpoint)
	}
}

func TestResolveS3_NooBaaCopiesSpecBucket(t *testing.T) {
	c := fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithObjects(noobaaAdminSecret("ak-nb", "sk-nb")).
		Build()

	r := &CostManagementServiceConfigReconciler{Client: c, Recorder: record.NewFakeRecorder(10)}
	cfg := &costv1alpha1.CostManagementServiceConfig{
		ObjectMeta: metav1.ObjectMeta{Name: testCRName, Namespace: testNamespace},
		Spec: costv1alpha1.CostManagementServiceConfigSpec{
			CostManagement: costv1alpha1.CostManagementConfig{
				Storage: costv1alpha1.CostManagementStorageSpec{
					BucketName: "from-spec",
				},
			},
		},
	}

	got, err := r.resolveS3(context.Background(), cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Bucket != "from-spec" {
		t.Errorf("Bucket: got %q, want from-spec", got.Bucket)
	}
}

func TestResolveS3_None(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	r := &CostManagementServiceConfigReconciler{Client: c, Recorder: record.NewFakeRecorder(10)}
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

	r := &CostManagementServiceConfigReconciler{Client: c, Recorder: record.NewFakeRecorder(10)}
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

	r := &CostManagementServiceConfigReconciler{Client: c, Recorder: record.NewFakeRecorder(10)}
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
