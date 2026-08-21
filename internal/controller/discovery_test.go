package controller

import (
	"context"
	"testing"

	storagev1 "k8s.io/api/storage/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	costv1alpha1 "github.com/project-koku/koku-service-operator/api/v1alpha1"
)

const (
	testNamespace = "default"
	testCRName    = "test"
)

// testScheme registers the types the fake client needs.
func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatalf("add client-go scheme: %v", err)
	}
	if err := costv1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("add costv1alpha1 scheme: %v", err)
	}
	return s
}

// openShiftIngress builds an unstructured config.openshift.io/v1 Ingress object.
func openShiftIngress(domain string) *unstructured.Unstructured {
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   openShiftConfigAPIGroup,
		Version: "v1",
		Kind:    "Ingress",
	})
	obj.SetName("cluster")
	_ = unstructured.SetNestedField(obj.Object, domain, "spec", "domain")
	return obj
}

// defaultStorageClass builds a StorageClass marked as the cluster default.
func defaultStorageClass(name string) *storagev1.StorageClass {
	return &storagev1.StorageClass{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
			Annotations: map[string]string{
				"storageclass.kubernetes.io/is-default-class": annotationTrue,
			},
		},
	}
}

// ---------------------------------------------------------------------------
// discoverClusterDomain
// ---------------------------------------------------------------------------

func TestDiscoverClusterDomain_Found(t *testing.T) {
	c := fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithObjects(openShiftIngress("apps.crc.testing")).
		Build()

	domain, err := discoverClusterDomain(context.Background(), c)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if domain != "apps.crc.testing" {
		t.Errorf("got %q, want %q", domain, "apps.crc.testing")
	}
}

func TestDiscoverClusterDomain_Missing(t *testing.T) {
	// No Ingress object in the cluster.
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()

	_, err := discoverClusterDomain(context.Background(), c)
	if err == nil {
		t.Fatal("expected error when Ingress/cluster is absent, got nil")
	}
}

func TestDiscoverClusterDomain_EmptyDomain(t *testing.T) {
	c := fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithObjects(openShiftIngress("")).
		Build()

	_, err := discoverClusterDomain(context.Background(), c)
	if err == nil {
		t.Fatal("expected error when spec.domain is empty, got nil")
	}
}

// ---------------------------------------------------------------------------
// discoverDefaultStorageClass
// ---------------------------------------------------------------------------

func TestDiscoverDefaultStorageClass_Found(t *testing.T) {
	sc := defaultStorageClass("crc-csi-hostpath-provisioner")
	c := fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithObjects(sc).
		Build()

	name, err := discoverDefaultStorageClass(context.Background(), c)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if name != "crc-csi-hostpath-provisioner" {
		t.Errorf("got %q, want %q", name, "crc-csi-hostpath-provisioner")
	}
}

func TestDiscoverDefaultStorageClass_NoneDefault(t *testing.T) {
	sc := &storagev1.StorageClass{
		ObjectMeta: metav1.ObjectMeta{Name: "non-default"},
	}
	c := fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithObjects(sc).
		Build()

	_, err := discoverDefaultStorageClass(context.Background(), c)
	if err == nil {
		t.Fatal("expected error when no default StorageClass exists, got nil")
	}
}

func TestDiscoverDefaultStorageClass_Empty(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()

	_, err := discoverDefaultStorageClass(context.Background(), c)
	if err == nil {
		t.Fatal("expected error when no StorageClasses exist, got nil")
	}
}

func TestDiscoverDefaultStorageClass_MultiplePicksDefault(t *testing.T) {
	c := fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithObjects(
			&storagev1.StorageClass{ObjectMeta: metav1.ObjectMeta{Name: "slow"}},
			defaultStorageClass("fast"),
			&storagev1.StorageClass{ObjectMeta: metav1.ObjectMeta{Name: "archive"}},
		).
		Build()

	name, err := discoverDefaultStorageClass(context.Background(), c)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if name != "fast" {
		t.Errorf("got %q, want %q", name, "fast")
	}
}

// assertDiscoveryCondition checks that status.conditions contains a
// DiscoveryComplete entry with the expected status and reason.
func assertDiscoveryCondition(t *testing.T, cfg *costv1alpha1.CostManagementServiceConfig, wantStatus metav1.ConditionStatus, wantReason string) {
	t.Helper()
	for _, cond := range cfg.Status.Conditions {
		if cond.Type == costv1alpha1.ConditionDiscoveryComplete {
			if cond.Status != wantStatus {
				t.Errorf("DiscoveryComplete status: got %s, want %s", cond.Status, wantStatus)
			}
			if wantReason != "" && cond.Reason != wantReason {
				t.Errorf("DiscoveryComplete reason: got %s, want %s", cond.Reason, wantReason)
			}
			return
		}
	}
	t.Error("DiscoveryComplete condition not set")
}

// ---------------------------------------------------------------------------
// reconcileDiscovery — full stage integration via fake client
// ---------------------------------------------------------------------------

func TestReconcileDiscovery_BothDiscovered(t *testing.T) {
	c := fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithObjects(
			openShiftIngress(testClusterDomain),
			defaultStorageClass("ocs-storagecluster-ceph-rbd"),
		).
		Build()

	r := &CostManagementServiceConfigReconciler{Client: c, Recorder: record.NewFakeRecorder(10)}
	cfg := &costv1alpha1.CostManagementServiceConfig{
		ObjectMeta: metav1.ObjectMeta{Name: testCRName, Namespace: testNamespace},
	}

	result, err := r.reconcileDiscovery(context.Background(), cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.IsZero() {
		t.Errorf("expected zero result (continue pipeline), got %+v", result)
	}
	if cfg.Status.DiscoveredConfig == nil {
		t.Fatal("DiscoveredConfig is nil")
	}
	if cfg.Status.DiscoveredConfig.ClusterDomain != testClusterDomain {
		t.Errorf("ClusterDomain: got %q, want %q", cfg.Status.DiscoveredConfig.ClusterDomain, testClusterDomain)
	}
	if cfg.Status.DiscoveredConfig.StorageClass != "ocs-storagecluster-ceph-rbd" {
		t.Errorf("StorageClass: got %q, want %q", cfg.Status.DiscoveredConfig.StorageClass, "ocs-storagecluster-ceph-rbd")
	}
	assertDiscoveryCondition(t, cfg, metav1.ConditionTrue, "Discovered")
}

func TestReconcileDiscovery_UserOverride(t *testing.T) {
	// User sets both values explicitly — no cluster queries needed.
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()

	r := &CostManagementServiceConfigReconciler{Client: c, Recorder: record.NewFakeRecorder(10)}
	cfg := &costv1alpha1.CostManagementServiceConfig{
		ObjectMeta: metav1.ObjectMeta{Name: testCRName, Namespace: testNamespace},
		Spec: costv1alpha1.CostManagementServiceConfigSpec{
			Global: costv1alpha1.GlobalConfig{
				ClusterDomain: "apps.custom.example.com",
				StorageClass:  "my-storage-class",
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
	if cfg.Status.DiscoveredConfig.ClusterDomain != "apps.custom.example.com" {
		t.Errorf("ClusterDomain: got %q, want %q", cfg.Status.DiscoveredConfig.ClusterDomain, "apps.custom.example.com")
	}
	if cfg.Status.DiscoveredConfig.StorageClass != "my-storage-class" {
		t.Errorf("StorageClass: got %q, want %q", cfg.Status.DiscoveredConfig.StorageClass, "my-storage-class")
	}
}

func TestReconcileDiscovery_DomainMissing_RequeuesWithCondition(t *testing.T) {
	c := fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithObjects(defaultStorageClass("fast")).
		Build()

	r := &CostManagementServiceConfigReconciler{Client: c, Recorder: record.NewFakeRecorder(10)}
	cfg := &costv1alpha1.CostManagementServiceConfig{
		ObjectMeta: metav1.ObjectMeta{Name: testCRName, Namespace: testNamespace},
	}

	result, err := r.reconcileDiscovery(context.Background(), cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.RequeueAfter == 0 {
		t.Error("expected non-zero RequeueAfter when domain not found")
	}
	assertDiscoveryCondition(t, cfg, metav1.ConditionFalse, "ClusterDomainNotFound")
}

func TestReconcileDiscovery_StorageClassMissing_RequeuesWithCondition(t *testing.T) {
	c := fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithObjects(openShiftIngress(testClusterDomain)).
		Build()

	r := &CostManagementServiceConfigReconciler{Client: c, Recorder: record.NewFakeRecorder(10)}
	cfg := &costv1alpha1.CostManagementServiceConfig{
		ObjectMeta: metav1.ObjectMeta{Name: testCRName, Namespace: testNamespace},
	}

	result, err := r.reconcileDiscovery(context.Background(), cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.RequeueAfter == 0 {
		t.Error("expected non-zero RequeueAfter when StorageClass not found")
	}
	assertDiscoveryCondition(t, cfg, metav1.ConditionFalse, "StorageClassNotFound")
}
