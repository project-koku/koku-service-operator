package controller

import (
	"context"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	costv1alpha1 "github.com/project-koku/koku-service-operator/api/v1alpha1"
	"github.com/project-koku/koku-service-operator/internal/resources"
)

func TestReconcileInfrastructure_ApplyStatefulSetCreate(t *testing.T) {
	scheme := ownershipScheme(t)
	cfg := minimalCR(testCRName, testNamespace)
	cfg.Spec.Cache.Deploy = boolPtr(false)
	// Database.Deploy defaults true → applyStatefulSet create path.

	r := &CostManagementServiceConfigReconciler{
		Client:   fakeClientWithApplySupport(scheme),
		Scheme:   scheme,
		Recorder: &noopRecorder{},
	}

	result, err := r.reconcileInfrastructure(context.Background(), cfg)
	if err != nil {
		t.Fatalf("reconcileInfrastructure: %v", err)
	}
	if result.RequeueAfter == 0 {
		t.Fatal("expected requeue while StatefulSet is not ready")
	}

	mustExist(t, r.Client, testNamespace, resources.NameDatabase(cfg), &corev1.Service{})
	sts := &appsv1.StatefulSet{}
	mustExist(t, r.Client, testNamespace, resources.NameDatabase(cfg), sts)
	if len(sts.OwnerReferences) != 1 {
		t.Fatalf("apply() should set ownerRef on the StatefulSet, got %d", len(sts.OwnerReferences))
	}

	cond := findCondition(cfg.Status.Conditions, costv1alpha1.ConditionDatabaseReady)
	if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != "WaitingForDatabase" {
		t.Fatalf("expected DatabaseReady=False WaitingForDatabase, got %+v", cond)
	}
}

func TestReconcileInfrastructure_WaitsForStatefulSetRollout(t *testing.T) {
	scheme := ownershipScheme(t)
	cfg := minimalCR(testCRName, testNamespace)
	cfg.Spec.Cache.Deploy = boolPtr(false)

	existing := resources.DatabaseStatefulSet(cfg)
	existing.Generation = 2
	replicas := int32(1)
	existing.Spec.Replicas = &replicas
	existing.Status.ObservedGeneration = 2
	existing.Status.ReadyReplicas = 1
	existing.Status.Replicas = 1
	existing.Status.UpdatedReplicas = 0
	existing.Status.CurrentRevision = "rev-old"
	existing.Status.UpdateRevision = "rev-new"

	c := fakeClientPreservingStatus(scheme, existing)
	r := &CostManagementServiceConfigReconciler{
		Client:   c,
		Scheme:   scheme,
		Recorder: &noopRecorder{},
	}

	result, err := r.reconcileInfrastructure(context.Background(), cfg)
	if err != nil {
		t.Fatalf("reconcileInfrastructure: %v", err)
	}
	if result.RequeueAfter == 0 {
		t.Fatal("expected requeue while the new StatefulSet revision is rolling out")
	}
	cond := findCondition(cfg.Status.Conditions, costv1alpha1.ConditionDatabaseReady)
	if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != "WaitingForDatabase" {
		t.Fatalf("expected DatabaseReady=False WaitingForDatabase during rollout, got %+v", cond)
	}
}

func TestApplyStatefulSet_UpdatePreservesVolumeClaimTemplates(t *testing.T) {
	scheme := ownershipScheme(t)
	cfg := minimalCR(testCRName, testNamespace)

	existing := resources.DatabaseStatefulSet(cfg)
	existing.Spec.Template.Spec.Containers[0].Image = "postgres:old"
	existing.Spec.VolumeClaimTemplates[0].Spec.Resources.Requests[corev1.ResourceStorage] = resource.MustParse("10Gi")
	replicas := int32(1)
	existing.Spec.Replicas = &replicas

	rec := record.NewFakeRecorder(10)
	r := &CostManagementServiceConfigReconciler{
		Client:   fakeClientWithApplySupport(scheme, existing),
		Scheme:   scheme,
		Recorder: rec,
	}

	desired := resources.DatabaseStatefulSet(cfg)
	desired.Spec.Template.Spec.Containers[0].Image = "postgres:new"
	wantReplicas := int32(2)
	desired.Spec.Replicas = &wantReplicas
	// Attempt to change VCT size — applyStatefulSet must detect this and set condition.
	desired.Spec.VolumeClaimTemplates[0].Spec.Resources.Requests[corev1.ResourceStorage] = resource.MustParse("99Gi")

	if err := r.applyStatefulSet(context.Background(), cfg, desired); err != nil && err != ErrStorageConfigChanged {
		t.Fatalf("applyStatefulSet: %v", err)
	}

	// VCT mismatch should set condition and NOT apply changes.
	cond := findCondition(cfg.Status.Conditions, costv1alpha1.ConditionDatabaseReady)
	if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != "StorageConfigChanged" {
		t.Fatalf("expected DatabaseReady=False StorageConfigChanged, got %+v", cond)
	}
	if !strings.Contains(cond.Message, "10Gi") || !strings.Contains(cond.Message, "99Gi") {
		t.Errorf("condition message should include size diff 10Gi -> 99Gi, got %q", cond.Message)
	}
	if !strings.Contains(cond.Message, "revert") || !strings.Contains(cond.Message, "delete") {
		t.Errorf("condition message should say revert the CR field vs delete STS/PVC, got %q", cond.Message)
	}
	assertEvent(t, rec, "StorageConfigChanged")
	avail := findCondition(cfg.Status.Conditions, costv1alpha1.ConditionAvailable)
	if avail == nil || avail.Status != metav1.ConditionFalse || avail.Reason != "StorageConfigChanged" {
		t.Fatalf("expected Available=False StorageConfigChanged, got %+v", avail)
	}
	deg := findCondition(cfg.Status.Conditions, costv1alpha1.ConditionDegraded)
	if deg == nil || deg.Status != metav1.ConditionTrue || deg.Reason != "StorageConfigChanged" {
		t.Fatalf("expected Degraded=True StorageConfigChanged, got %+v", deg)
	}

	// Verify no changes were applied.
	got := &appsv1.StatefulSet{}
	if err := r.Get(context.Background(), types.NamespacedName{
		Namespace: testNamespace,
		Name:      resources.NameDatabase(cfg),
	}, got); err != nil {
		t.Fatalf("Get StatefulSet: %v", err)
	}

	if got.Spec.Template.Spec.Containers[0].Image != "postgres:old" {
		t.Errorf("image should not be updated when VCT differs: got %q", got.Spec.Template.Spec.Containers[0].Image)
	}
	if got.Spec.Replicas == nil || *got.Spec.Replicas != 1 {
		t.Errorf("replicas should not be updated when VCT differs: got %v", got.Spec.Replicas)
	}
	size := got.Spec.VolumeClaimTemplates[0].Spec.Resources.Requests[corev1.ResourceStorage]
	if !size.Equal(resource.MustParse("10Gi")) {
		t.Errorf("VolumeClaimTemplates must be immutable; got size %s want 10Gi", size.String())
	}
}

func TestApplyStatefulSet_SSAUpdatesMutableFields(t *testing.T) {
	scheme := ownershipScheme(t)
	cfg := minimalCR(testCRName, testNamespace)

	existing := resources.DatabaseStatefulSet(cfg)
	existing.Spec.Template.Spec.Containers[0].Image = "postgres:old"
	existing.Spec.VolumeClaimTemplates[0].Spec.Resources.Requests[corev1.ResourceStorage] = resource.MustParse("10Gi")
	replicas := int32(1)
	existing.Spec.Replicas = &replicas

	r := &CostManagementServiceConfigReconciler{
		Client:   fakeClientWithApplySupport(scheme, existing),
		Scheme:   scheme,
		Recorder: &noopRecorder{},
	}

	// Desired has SAME VCT but different mutable fields.
	desired := resources.DatabaseStatefulSet(cfg)
	desired.Spec.Template.Spec.Containers[0].Image = "postgres:new"
	wantReplicas := int32(2)
	desired.Spec.Replicas = &wantReplicas
	// VCT matches existing (10Gi)
	desired.Spec.VolumeClaimTemplates[0].Spec.Resources.Requests[corev1.ResourceStorage] = resource.MustParse("10Gi")

	if err := r.applyStatefulSet(context.Background(), cfg, desired); err != nil {
		t.Fatalf("applyStatefulSet: %v", err)
	}

	// No condition should be set for VCT mismatch.
	cond := findCondition(cfg.Status.Conditions, costv1alpha1.ConditionDatabaseReady)
	if cond != nil && cond.Reason == "StorageConfigChanged" {
		t.Fatalf("unexpected StorageConfigChanged condition: %+v", cond)
	}

	// Verify SSA applied the mutable fields.
	got := &appsv1.StatefulSet{}
	if err := r.Get(context.Background(), types.NamespacedName{
		Namespace: testNamespace,
		Name:      resources.NameDatabase(cfg),
	}, got); err != nil {
		t.Fatalf("Get StatefulSet: %v", err)
	}

	if got.Spec.Template.Spec.Containers[0].Image != "postgres:new" {
		t.Errorf("image not updated via SSA: got %q", got.Spec.Template.Spec.Containers[0].Image)
	}
	if got.Spec.Replicas == nil || *got.Spec.Replicas != 2 {
		t.Errorf("replicas not updated via SSA: got %v", got.Spec.Replicas)
	}
	size := got.Spec.VolumeClaimTemplates[0].Spec.Resources.Requests[corev1.ResourceStorage]
	if !size.Equal(resource.MustParse("10Gi")) {
		t.Errorf("VolumeClaimTemplates must be immutable; got size %s want 10Gi", size.String())
	}
}

func TestApplyStatefulSet_SSADriftCorrection(t *testing.T) {
	scheme := ownershipScheme(t)
	cfg := minimalCR(testCRName, testNamespace)

	// Start with desired state applied via SSA.
	desired := resources.DatabaseStatefulSet(cfg)
	desired.Spec.VolumeClaimTemplates[0].Spec.Resources.Requests[corev1.ResourceStorage] = resource.MustParse("10Gi")

	r := &CostManagementServiceConfigReconciler{
		Client:   fakeClientWithApplySupport(scheme, desired),
		Scheme:   scheme,
		Recorder: &noopRecorder{},
	}

	// First reconcile applies the desired state.
	if err := r.applyStatefulSet(context.Background(), cfg, desired); err != nil {
		t.Fatalf("first applyStatefulSet: %v", err)
	}

	// Simulate manual edit: change a managed field (livenessProbe) to something different.
	// We need to directly modify the object in the fake client.
	manualEdited := &appsv1.StatefulSet{}
	if err := r.Get(context.Background(), types.NamespacedName{
		Namespace: testNamespace,
		Name:      resources.NameDatabase(cfg),
	}, manualEdited); err != nil {
		t.Fatalf("Get for manual edit: %v", err)
	}
	// Modify livenessProbe - this is a field managed by SSA
	if len(manualEdited.Spec.Template.Spec.Containers) > 0 {
		manualEdited.Spec.Template.Spec.Containers[0].LivenessProbe = &corev1.Probe{
			ProbeHandler: corev1.ProbeHandler{
				Exec: &corev1.ExecAction{Command: []string{"/bin/false"}},
			},
			InitialDelaySeconds: 999,
		}
	}
	if err := r.Update(context.Background(), manualEdited); err != nil {
		t.Fatalf("Update for manual edit: %v", err)
	}

	// Second reconcile (drift correction) - should revert the manual edit via SSA.
	if err := r.applyStatefulSet(context.Background(), cfg, desired); err != nil {
		t.Fatalf("second applyStatefulSet (drift): %v", err)
	}

	// Verify the manual edit was reverted.
	got := &appsv1.StatefulSet{}
	if err := r.Get(context.Background(), types.NamespacedName{
		Namespace: testNamespace,
		Name:      resources.NameDatabase(cfg),
	}, got); err != nil {
		t.Fatalf("Get after drift correction: %v", err)
	}

	// The livenessProbe should be back to the desired state (pg_isready)
	if len(got.Spec.Template.Spec.Containers) == 0 {
		t.Fatal("no containers in StatefulSet")
	}
	probe := got.Spec.Template.Spec.Containers[0].LivenessProbe
	if probe == nil {
		t.Fatal("livenessProbe is nil after drift correction")
		return
	}
	if probe.Exec == nil || len(probe.Exec.Command) == 0 || probe.Exec.Command[0] != "/bin/sh" {
		t.Errorf("livenessProbe not reverted to desired state: got %+v", probe)
	}
	if probe.InitialDelaySeconds != 30 {
		t.Errorf("livenessProbe InitialDelaySeconds not reverted: got %d want 30", probe.InitialDelaySeconds)
	}
}

func TestVolumeClaimTemplatesEqual(t *testing.T) {
	stringPtr := func(s string) *string { return &s }
	tests := []struct {
		name     string
		a        []corev1.PersistentVolumeClaim
		b        []corev1.PersistentVolumeClaim
		expected bool
	}{
		{
			name: "equal VCTs",
			a: []corev1.PersistentVolumeClaim{
				{
					ObjectMeta: metav1.ObjectMeta{Name: "test"},
					Spec: corev1.PersistentVolumeClaimSpec{
						StorageClassName: stringPtr("fast"),
						Resources: corev1.VolumeResourceRequirements{
							Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("10Gi")},
						},
						AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
					},
				},
			},
			b: []corev1.PersistentVolumeClaim{
				{
					ObjectMeta: metav1.ObjectMeta{Name: "test"},
					Spec: corev1.PersistentVolumeClaimSpec{
						StorageClassName: stringPtr("fast"),
						Resources: corev1.VolumeResourceRequirements{
							Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("10Gi")},
						},
						AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
					},
				},
			},
			expected: true,
		},
		{
			name: "different storage size",
			a: []corev1.PersistentVolumeClaim{{
				ObjectMeta: metav1.ObjectMeta{Name: "test"},
				Spec: corev1.PersistentVolumeClaimSpec{
					Resources:   corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("10Gi")}},
					AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
				},
			}},
			b: []corev1.PersistentVolumeClaim{{
				ObjectMeta: metav1.ObjectMeta{Name: "test"},
				Spec: corev1.PersistentVolumeClaimSpec{
					Resources:   corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("20Gi")}},
					AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
				},
			}},
			expected: false,
		},
		{
			name: "different storage class",
			a: []corev1.PersistentVolumeClaim{{
				ObjectMeta: metav1.ObjectMeta{Name: "test"},
				Spec: corev1.PersistentVolumeClaimSpec{
					StorageClassName: stringPtr("fast"),
					Resources:        corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("10Gi")}},
					AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
				},
			}},
			b: []corev1.PersistentVolumeClaim{{
				ObjectMeta: metav1.ObjectMeta{Name: "test"},
				Spec: corev1.PersistentVolumeClaimSpec{
					StorageClassName: stringPtr("slow"),
					Resources:        corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("10Gi")}},
					AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
				},
			}},
			expected: false,
		},
		{
			name: "nil vs empty storage class",
			a: []corev1.PersistentVolumeClaim{{
				ObjectMeta: metav1.ObjectMeta{Name: "test"},
				Spec: corev1.PersistentVolumeClaimSpec{
					StorageClassName: nil,
					Resources:        corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("10Gi")}},
					AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
				},
			}},
			b: []corev1.PersistentVolumeClaim{{
				ObjectMeta: metav1.ObjectMeta{Name: "test"},
				Spec: corev1.PersistentVolumeClaimSpec{
					StorageClassName: stringPtr(""),
					Resources:        corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("10Gi")}},
					AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
				},
			}},
			expected: false,
		},
		{
			name: "different access modes",
			a: []corev1.PersistentVolumeClaim{{
				ObjectMeta: metav1.ObjectMeta{Name: "test"},
				Spec: corev1.PersistentVolumeClaimSpec{
					Resources:   corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("10Gi")}},
					AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
				},
			}},
			b: []corev1.PersistentVolumeClaim{{
				ObjectMeta: metav1.ObjectMeta{Name: "test"},
				Spec: corev1.PersistentVolumeClaimSpec{
					Resources:   corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("10Gi")}},
					AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteMany},
				},
			}},
			expected: false,
		},
		{
			name: "different length",
			a: []corev1.PersistentVolumeClaim{
				{ObjectMeta: metav1.ObjectMeta{Name: "test1"}},
				{ObjectMeta: metav1.ObjectMeta{Name: "test2"}},
			},
			b: []corev1.PersistentVolumeClaim{
				{ObjectMeta: metav1.ObjectMeta{Name: "test1"}},
			},
			expected: false,
		},
		{
			name:     "different names",
			a:        []corev1.PersistentVolumeClaim{{ObjectMeta: metav1.ObjectMeta{Name: "test1"}}},
			b:        []corev1.PersistentVolumeClaim{{ObjectMeta: metav1.ObjectMeta{Name: "test2"}}},
			expected: false,
		},
		{
			name: "different labels",
			a: []corev1.PersistentVolumeClaim{{
				ObjectMeta: metav1.ObjectMeta{Name: "test", Labels: map[string]string{"app": "db"}},
			}},
			b: []corev1.PersistentVolumeClaim{{
				ObjectMeta: metav1.ObjectMeta{Name: "test", Labels: map[string]string{"app": "other"}},
			}},
			expected: false,
		},
		{
			name:     "nil vs empty labels",
			a:        []corev1.PersistentVolumeClaim{{ObjectMeta: metav1.ObjectMeta{Name: "test"}}},
			b:        []corev1.PersistentVolumeClaim{{ObjectMeta: metav1.ObjectMeta{Name: "test", Labels: map[string]string{}}}},
			expected: true,
		},
		{
			name: "different annotations",
			a: []corev1.PersistentVolumeClaim{{
				ObjectMeta: metav1.ObjectMeta{Name: "test", Annotations: map[string]string{"pv.kubernetes.io/bind-completed": "yes"}},
			}},
			b: []corev1.PersistentVolumeClaim{{
				ObjectMeta: metav1.ObjectMeta{Name: "test"},
			}},
			expected: false,
		},
		{
			name:     "nil vs empty annotations",
			a:        []corev1.PersistentVolumeClaim{{ObjectMeta: metav1.ObjectMeta{Name: "test"}}},
			b:        []corev1.PersistentVolumeClaim{{ObjectMeta: metav1.ObjectMeta{Name: "test", Annotations: map[string]string{}}}},
			expected: true,
		},
		{
			name: "different DataSourceRef namespace",
			a: []corev1.PersistentVolumeClaim{{
				ObjectMeta: metav1.ObjectMeta{Name: "test"},
				Spec: corev1.PersistentVolumeClaimSpec{
					DataSourceRef: &corev1.TypedObjectReference{
						APIGroup:  stringPtr("snapshot.storage.k8s.io"),
						Kind:      "VolumeSnapshot",
						Name:      "snap-1",
						Namespace: stringPtr("ns-a"),
					},
				},
			}},
			b: []corev1.PersistentVolumeClaim{{
				ObjectMeta: metav1.ObjectMeta{Name: "test"},
				Spec: corev1.PersistentVolumeClaimSpec{
					DataSourceRef: &corev1.TypedObjectReference{
						APIGroup:  stringPtr("snapshot.storage.k8s.io"),
						Kind:      "VolumeSnapshot",
						Name:      "snap-1",
						Namespace: stringPtr("ns-b"),
					},
				},
			}},
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := volumeClaimTemplatesEqual(tt.a, tt.b)
			if result != tt.expected {
				t.Errorf("volumeClaimTemplatesEqual() = %v, want %v", result, tt.expected)
			}
		})
	}
}

func TestResourceRequirementsEqual(t *testing.T) {
	tests := []struct {
		name     string
		a        corev1.VolumeResourceRequirements
		b        corev1.VolumeResourceRequirements
		expected bool
	}{
		{
			name:     "both nil",
			a:        corev1.VolumeResourceRequirements{},
			b:        corev1.VolumeResourceRequirements{},
			expected: true,
		},
		{
			name:     "both empty",
			a:        corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{}},
			b:        corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{}},
			expected: true,
		},
		{
			name: "equal resources",
			a: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("10Gi")},
			},
			b: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("10Gi")},
			},
			expected: true,
		},
		{
			name: "different resources",
			a: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("10Gi")},
			},
			b: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("20Gi")},
			},
			expected: false,
		},
		{
			name: "a nil b not",
			a:    corev1.VolumeResourceRequirements{},
			b: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("10Gi")},
			},
			expected: false,
		},
		{
			name: "extra resource in b",
			a: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("10Gi")},
			},
			b: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceStorage: resource.MustParse("10Gi"),
					corev1.ResourceCPU:     resource.MustParse("100m"),
				},
			},
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := resourceRequirementsEqual(tt.a, tt.b)
			if result != tt.expected {
				t.Errorf("resourceRequirementsEqual() = %v, want %v", result, tt.expected)
			}
		})
	}
}

func TestAccessModesEqual(t *testing.T) {
	tests := []struct {
		name     string
		a        []corev1.PersistentVolumeAccessMode
		b        []corev1.PersistentVolumeAccessMode
		expected bool
	}{
		{
			name:     "both nil",
			a:        nil,
			b:        nil,
			expected: true,
		},
		{
			name:     "both empty",
			a:        []corev1.PersistentVolumeAccessMode{},
			b:        []corev1.PersistentVolumeAccessMode{},
			expected: true,
		},
		{
			name:     "equal",
			a:        []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			b:        []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			expected: true,
		},
		{
			name:     "different",
			a:        []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			b:        []corev1.PersistentVolumeAccessMode{corev1.ReadWriteMany},
			expected: false,
		},
		{
			name:     "different length",
			a:        []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			b:        []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce, corev1.ReadOnlyMany},
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := accessModesEqual(tt.a, tt.b)
			if result != tt.expected {
				t.Errorf("accessModesEqual() = %v, want %v", result, tt.expected)
			}
		})
	}
}

func TestVolumeModeEqual(t *testing.T) {
	tests := []struct {
		name     string
		a        *corev1.PersistentVolumeMode
		b        *corev1.PersistentVolumeMode
		expected bool
	}{
		{name: "both nil", a: nil, b: nil, expected: true},
		{name: "nil vs Filesystem", a: nil, b: ptr(corev1.PersistentVolumeFilesystem), expected: true},
		{name: "Filesystem vs nil", a: ptr(corev1.PersistentVolumeFilesystem), b: nil, expected: true},
		{name: "both Filesystem", a: ptr(corev1.PersistentVolumeFilesystem), b: ptr(corev1.PersistentVolumeFilesystem), expected: true},
		{name: "Block vs Filesystem", a: ptr(corev1.PersistentVolumeBlock), b: ptr(corev1.PersistentVolumeFilesystem), expected: false},
		{name: "Filesystem vs Block", a: ptr(corev1.PersistentVolumeFilesystem), b: ptr(corev1.PersistentVolumeBlock), expected: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := volumeModeEqual(tt.a, tt.b)
			if result != tt.expected {
				t.Errorf("volumeModeEqual() = %v, want %v", result, tt.expected)
			}
		})
	}
}

func TestLabelSelectorEqual(t *testing.T) {
	tests := []struct {
		name     string
		a        *metav1.LabelSelector
		b        *metav1.LabelSelector
		expected bool
	}{
		{name: "both nil", a: nil, b: nil, expected: true},
		{name: "one nil", a: nil, b: &metav1.LabelSelector{}, expected: false},
		{name: "empty vs empty", a: &metav1.LabelSelector{}, b: &metav1.LabelSelector{}, expected: true},
		{name: "equal matchLabels", a: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "test"}}, b: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "test"}}, expected: true},
		{name: "different matchLabels", a: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "test"}}, b: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "other"}}, expected: false},
		{name: "equal matchExpressions", a: &metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{{Key: "env", Operator: metav1.LabelSelectorOpIn, Values: []string{"prod"}}}}, b: &metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{{Key: "env", Operator: metav1.LabelSelectorOpIn, Values: []string{"prod"}}}}, expected: true},
		{name: "different matchExpressions", a: &metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{{Key: "env", Operator: metav1.LabelSelectorOpIn, Values: []string{"prod"}}}}, b: &metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{{Key: "env", Operator: metav1.LabelSelectorOpIn, Values: []string{"dev"}}}}, expected: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := labelSelectorEqual(tt.a, tt.b)
			if result != tt.expected {
				t.Errorf("labelSelectorEqual() = %v, want %v", result, tt.expected)
			}
		})
	}
}

func TestVolumeNameEqual(t *testing.T) {
	tests := []struct {
		name     string
		a        string
		b        string
		expected bool
	}{
		{name: "both empty", a: "", b: "", expected: true},
		{name: "equal", a: "pvc-123", b: "pvc-123", expected: true},
		{name: "different", a: "pvc-123", b: "pvc-456", expected: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := volumeNameEqual(tt.a, tt.b)
			if result != tt.expected {
				t.Errorf("volumeNameEqual() = %v, want %v", result, tt.expected)
			}
		})
	}
}

// nolint:dupl
func TestDataSourceEqual(t *testing.T) {
	tests := []struct {
		name     string
		a        *corev1.TypedLocalObjectReference
		b        *corev1.TypedLocalObjectReference
		expected bool
	}{
		{name: "both nil", a: nil, b: nil, expected: true},
		{name: "one nil", a: nil, b: &corev1.TypedLocalObjectReference{}, expected: false},
		{name: "equal", a: &corev1.TypedLocalObjectReference{APIGroup: ptr(""), Kind: "VolumeSnapshot", Name: "snap-1"}, b: &corev1.TypedLocalObjectReference{APIGroup: ptr(""), Kind: "VolumeSnapshot", Name: "snap-1"}, expected: true},
		{name: "different name", a: &corev1.TypedLocalObjectReference{APIGroup: ptr(""), Kind: "VolumeSnapshot", Name: "snap-1"}, b: &corev1.TypedLocalObjectReference{APIGroup: ptr(""), Kind: "VolumeSnapshot", Name: "snap-2"}, expected: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := dataSourceEqual(tt.a, tt.b)
			if result != tt.expected {
				t.Errorf("dataSourceEqual() = %v, want %v", result, tt.expected)
			}
		})
	}
}

// nolint:dupl
func TestDataSourceRefEqual(t *testing.T) {
	tests := []struct {
		name     string
		a        *corev1.TypedObjectReference
		b        *corev1.TypedObjectReference
		expected bool
	}{
		{name: "both nil", a: nil, b: nil, expected: true},
		{name: "one nil", a: nil, b: &corev1.TypedObjectReference{}, expected: false},
		{name: "equal", a: &corev1.TypedObjectReference{APIGroup: ptr(""), Kind: "VolumeSnapshot", Name: "snap-1"}, b: &corev1.TypedObjectReference{APIGroup: ptr(""), Kind: "VolumeSnapshot", Name: "snap-1"}, expected: true},
		{name: "different kind", a: &corev1.TypedObjectReference{APIGroup: ptr(""), Kind: "VolumeSnapshot", Name: "snap-1"}, b: &corev1.TypedObjectReference{APIGroup: ptr(""), Kind: "PVC", Name: "snap-1"}, expected: false},
		{name: "different namespace", a: &corev1.TypedObjectReference{APIGroup: ptr(""), Kind: "VolumeSnapshot", Name: "snap-1", Namespace: ptr("ns-a")}, b: &corev1.TypedObjectReference{APIGroup: ptr(""), Kind: "VolumeSnapshot", Name: "snap-1", Namespace: ptr("ns-b")}, expected: false},
		{name: "equal including namespace", a: &corev1.TypedObjectReference{APIGroup: ptr(""), Kind: "VolumeSnapshot", Name: "snap-1", Namespace: ptr("ns-a")}, b: &corev1.TypedObjectReference{APIGroup: ptr(""), Kind: "VolumeSnapshot", Name: "snap-1", Namespace: ptr("ns-a")}, expected: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := dataSourceRefEqual(tt.a, tt.b)
			if result != tt.expected {
				t.Errorf("dataSourceRefEqual() = %v, want %v", result, tt.expected)
			}
		})
	}
}

func TestVolumeAttributesClassNameEqual(t *testing.T) {
	tests := []struct {
		name     string
		a        *string
		b        *string
		expected bool
	}{
		{name: "both nil", a: nil, b: nil, expected: true},
		{name: "one nil", a: nil, b: ptr("class-1"), expected: false},
		{name: "equal", a: ptr("class-1"), b: ptr("class-1"), expected: true},
		{name: "different", a: ptr("class-1"), b: ptr("class-2"), expected: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := volumeAttributesClassNameEqual(tt.a, tt.b)
			if result != tt.expected {
				t.Errorf("volumeAttributesClassNameEqual() = %v, want %v", result, tt.expected)
			}
		})
	}
}

func TestVolumeClaimTemplateDiff(t *testing.T) {
	stringPtr := func(s string) *string { return &s }
	tests := []struct {
		name string
		live []corev1.PersistentVolumeClaim
		want []corev1.PersistentVolumeClaim
		sub  string
	}{
		{
			name: "size change",
			live: []corev1.PersistentVolumeClaim{{
				ObjectMeta: metav1.ObjectMeta{Name: "postgres-storage"},
				Spec: corev1.PersistentVolumeClaimSpec{
					Resources: corev1.VolumeResourceRequirements{
						Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("10Gi")},
					},
				},
			}},
			want: []corev1.PersistentVolumeClaim{{
				ObjectMeta: metav1.ObjectMeta{Name: "postgres-storage"},
				Spec: corev1.PersistentVolumeClaimSpec{
					Resources: corev1.VolumeResourceRequirements{
						Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("20Gi")},
					},
				},
			}},
			sub: "postgres-storage size 10Gi -> 20Gi",
		},
		{
			name: "storage class change",
			live: []corev1.PersistentVolumeClaim{{
				ObjectMeta: metav1.ObjectMeta{Name: "postgres-storage"},
				Spec:       corev1.PersistentVolumeClaimSpec{StorageClassName: stringPtr("gp2")},
			}},
			want: []corev1.PersistentVolumeClaim{{
				ObjectMeta: metav1.ObjectMeta{Name: "postgres-storage"},
				Spec:       corev1.PersistentVolumeClaimSpec{StorageClassName: stringPtr("gp3")},
			}},
			sub: "postgres-storage storageClass gp2 -> gp3",
		},
		{
			name: "count change",
			live: []corev1.PersistentVolumeClaim{{ObjectMeta: metav1.ObjectMeta{Name: "a"}}},
			want: []corev1.PersistentVolumeClaim{
				{ObjectMeta: metav1.ObjectMeta{Name: "a"}},
				{ObjectMeta: metav1.ObjectMeta{Name: "b"}},
			},
			sub: "count 1 -> 2",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := volumeClaimTemplateDiff(tt.live, tt.want)
			if !strings.Contains(got, tt.sub) {
				t.Errorf("volumeClaimTemplateDiff() = %q, want substring %q", got, tt.sub)
			}
		})
	}
}

func TestPtrEqual(t *testing.T) {
	tests := []struct {
		name     string
		a, b     *string
		expected bool
	}{
		{name: "both nil", expected: true},
		{name: "one nil", b: ptr("gp3"), expected: false},
		{name: "equal", a: ptr("gp3"), b: ptr("gp3"), expected: true},
		{name: "different", a: ptr("gp3"), b: ptr("gp2"), expected: false},
		{name: "nil vs empty string", a: nil, b: ptr(""), expected: false},
		{name: "both empty string", a: ptr(""), b: ptr(""), expected: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ptrEqual(tt.a, tt.b); got != tt.expected {
				t.Errorf("ptrEqual() = %v, want %v", got, tt.expected)
			}
		})
	}
}

func TestReconcileInfrastructure_VCTMismatchOnReadyDB(t *testing.T) {
	scheme := ownershipScheme(t)
	cfg := minimalCR(testCRName, testNamespace)
	cfg.Spec.Cache.Deploy = boolPtr(false)

	// Pre-create a ready StatefulSet with 10Gi storage
	existing := resources.DatabaseStatefulSet(cfg)
	existing.Spec.VolumeClaimTemplates[0].Spec.Resources.Requests[corev1.ResourceStorage] = resource.MustParse("10Gi")
	replicas := int32(1)
	existing.Spec.Replicas = &replicas

	// Mark the DB as ready in status so reconcileInfrastructure would normally set DatabaseAvailable
	c := fakeClientPreservingStatus(scheme, existing)
	markStatefulSetReady(t, c, testNamespace, resources.NameDatabase(cfg))

	r := &CostManagementServiceConfigReconciler{
		Client:   c,
		Scheme:   scheme,
		Recorder: &noopRecorder{},
	}

	// Simulate user changing storage size to 20Gi in spec
	cfg.Spec.Database.Storage.Size = resource.MustParse("20Gi")

	result, err := r.reconcileInfrastructure(context.Background(), cfg)
	if err != nil {
		t.Fatalf("reconcileInfrastructure: %v", err)
	}

	// Should requeue and NOT overwrite StorageConfigChanged condition
	if result.RequeueAfter == 0 {
		t.Fatal("expected requeue on VCT mismatch")
	}

	cond := findCondition(cfg.Status.Conditions, costv1alpha1.ConditionDatabaseReady)
	if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != "StorageConfigChanged" {
		t.Fatalf("expected DatabaseReady=False StorageConfigChanged, got %+v", cond)
	}
	avail := findCondition(cfg.Status.Conditions, costv1alpha1.ConditionAvailable)
	if avail == nil || avail.Status != metav1.ConditionFalse || avail.Reason != "StorageConfigChanged" {
		t.Fatalf("expected Available=False StorageConfigChanged, got %+v", avail)
	}
	deg := findCondition(cfg.Status.Conditions, costv1alpha1.ConditionDegraded)
	if deg == nil || deg.Status != metav1.ConditionTrue || deg.Reason != "StorageConfigChanged" {
		t.Fatalf("expected Degraded=True StorageConfigChanged, got %+v", deg)
	}
}

// TestReconcile_VCTMismatchPersistsStatus goes through Reconcile (not just
// reconcileInfrastructure) and reads the CR back from the client. Reconcile
// patchStatus runs after runPhases even when the result is RequeueAfter/nil.
func TestReconcile_VCTMismatchPersistsStatus(t *testing.T) {
	scheme := ownershipScheme(t)
	cfg := minimalCR(testCRName, testNamespace)
	cfg.Spec.Cache.Deploy = boolPtr(false)
	cfg.Spec.Global.ClusterDomain = "apps.example.com"
	cfg.Spec.Global.StorageClass = "gp3"
	cfg.Spec.ObjectStorage.SecretName = "s3-creds"
	pinTestImages(cfg)
	controllerutil.AddFinalizer(cfg, finalizerName)

	existing := resources.DatabaseStatefulSet(cfg)
	existing.Spec.VolumeClaimTemplates[0].Spec.Resources.Requests[corev1.ResourceStorage] = resource.MustParse("10Gi")
	replicas := int32(1)
	existing.Spec.Replicas = &replicas

	cfg.Spec.Database.Storage.Size = resource.MustParse("20Gi")

	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(cfg, existing).
		WithStatusSubresource(cfg, &appsv1.StatefulSet{}).
		WithInterceptorFuncs(interceptor.Funcs{
			Patch: func(ctx context.Context, inner client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
				if patch.Type() != types.ApplyPatchType {
					return inner.Patch(ctx, obj, patch, opts...)
				}
				key := client.ObjectKeyFromObject(obj)
				got := obj.DeepCopyObject().(client.Object)
				err := inner.Get(ctx, key, got)
				if apierrors.IsNotFound(err) {
					return inner.Create(ctx, obj)
				}
				if err != nil {
					return err
				}
				obj.SetResourceVersion(got.GetResourceVersion())
				return inner.Update(ctx, obj)
			},
		}).Build()

	r := &CostManagementServiceConfigReconciler{
		Client:   c,
		Scheme:   scheme,
		Recorder: &noopRecorder{},
	}

	result, err := r.Reconcile(context.Background(), reconcile.Request{
		NamespacedName: types.NamespacedName{Name: testCRName, Namespace: testNamespace},
	})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if result.RequeueAfter == 0 {
		t.Fatal("expected requeue on VCT mismatch")
	}

	updated := &costv1alpha1.CostManagementServiceConfig{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: testCRName, Namespace: testNamespace}, updated); err != nil {
		t.Fatalf("Get CR: %v", err)
	}
	cond := findCondition(updated.Status.Conditions, costv1alpha1.ConditionDatabaseReady)
	if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != "StorageConfigChanged" {
		t.Fatalf("status not persisted: DatabaseReady=%+v", cond)
	}
	avail := findCondition(updated.Status.Conditions, costv1alpha1.ConditionAvailable)
	if avail == nil || avail.Status != metav1.ConditionFalse || avail.Reason != "StorageConfigChanged" {
		t.Fatalf("status not persisted: Available=%+v", avail)
	}
	deg := findCondition(updated.Status.Conditions, costv1alpha1.ConditionDegraded)
	if deg == nil || deg.Status != metav1.ConditionTrue || deg.Reason != "StorageConfigChanged" {
		t.Fatalf("status not persisted: Degraded=%+v", deg)
	}
	if updated.Status.Phase != costv1alpha1.PhaseDegraded {
		t.Errorf("phase = %q, want %q", updated.Status.Phase, costv1alpha1.PhaseDegraded)
	}
	if !strings.Contains(cond.Message, "10Gi") || !strings.Contains(cond.Message, "20Gi") {
		t.Errorf("persisted message should include size diff, got %q", cond.Message)
	}
	prog := findCondition(updated.Status.Conditions, costv1alpha1.ConditionProgressing)
	if prog == nil || prog.Status != metav1.ConditionFalse || prog.Reason != "StorageConfigChanged" {
		t.Fatalf("expected Progressing=False StorageConfigChanged, got %+v", prog)
	}
}

func TestApplyStatefulSet_SSACopiesLiveVCTBeforeApply(t *testing.T) {
	scheme := ownershipScheme(t)
	cfg := minimalCR(testCRName, testNamespace)

	fs := corev1.PersistentVolumeFilesystem
	existing := resources.DatabaseStatefulSet(cfg)
	existing.Spec.VolumeClaimTemplates[0].Spec.Resources.Requests[corev1.ResourceStorage] = resource.MustParse("10Gi")
	existing.Spec.VolumeClaimTemplates[0].Spec.VolumeMode = &fs
	replicas := int32(1)
	existing.Spec.Replicas = &replicas

	r := &CostManagementServiceConfigReconciler{
		Client:   fakeClientWithApplySupport(scheme, existing),
		Scheme:   scheme,
		Recorder: &noopRecorder{},
	}

	desired := resources.DatabaseStatefulSet(cfg)
	desired.Spec.Template.Spec.Containers[0].Image = "postgres:new"
	desired.Spec.VolumeClaimTemplates[0].Spec.Resources.Requests[corev1.ResourceStorage] = resource.MustParse("10Gi")
	desired.Spec.VolumeClaimTemplates[0].Spec.VolumeMode = nil

	if err := r.applyStatefulSet(context.Background(), cfg, desired); err != nil {
		t.Fatalf("applyStatefulSet: %v", err)
	}

	got := &appsv1.StatefulSet{}
	if err := r.Get(context.Background(), types.NamespacedName{
		Namespace: testNamespace,
		Name:      resources.NameDatabase(cfg),
	}, got); err != nil {
		t.Fatalf("Get StatefulSet: %v", err)
	}
	if got.Spec.Template.Spec.Containers[0].Image != "postgres:new" {
		t.Errorf("image not updated: got %q", got.Spec.Template.Spec.Containers[0].Image)
	}
	mode := got.Spec.VolumeClaimTemplates[0].Spec.VolumeMode
	if mode == nil || *mode != corev1.PersistentVolumeFilesystem {
		t.Errorf("live VolumeMode should be preserved across SSA: got %+v", mode)
	}
}

func ptr[T any](v T) *T {
	return &v
}
