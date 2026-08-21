package metrics

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestSetCondition_MirrorsActiveStatus(t *testing.T) {
	SetCondition("ns", "cm", "Degraded", metav1.ConditionTrue)
	if got := testutil.ToFloat64(Condition.WithLabelValues("ns", "cm", "Degraded", "True")); got != 1 {
		t.Fatalf("Degraded True = %v, want 1", got)
	}
	if got := testutil.ToFloat64(Condition.WithLabelValues("ns", "cm", "Degraded", "False")); got != 0 {
		t.Fatalf("Degraded False = %v, want 0", got)
	}

	SetCondition("ns", "cm", "Degraded", metav1.ConditionFalse)
	if got := testutil.ToFloat64(Condition.WithLabelValues("ns", "cm", "Degraded", "True")); got != 0 {
		t.Fatalf("after flip True = %v, want 0", got)
	}
	if got := testutil.ToFloat64(Condition.WithLabelValues("ns", "cm", "Degraded", "False")); got != 1 {
		t.Fatalf("after flip False = %v, want 1", got)
	}

	SetCondition("ns", "cm", "Empty", "")
	if got := testutil.ToFloat64(Condition.WithLabelValues("ns", "cm", "Empty", "Unknown")); got != 1 {
		t.Fatalf("empty status Unknown = %v, want 1", got)
	}
}

func TestSetMigrationJobFailed(t *testing.T) {
	SetMigrationJobFailed("ns", "cm", "cm-koku-migrate", true)
	if got := testutil.ToFloat64(MigrationJobFailed.WithLabelValues("ns", "cm", "cm-koku-migrate")); got != 1 {
		t.Fatalf("failed gauge = %v, want 1", got)
	}
	ClearMigrationJobFailed("ns", "cm", "cm-koku-migrate")
	if got := testutil.ToFloat64(MigrationJobFailed.WithLabelValues("ns", "cm", "cm-koku-migrate")); got != 0 {
		t.Fatalf("cleared gauge = %v, want 0", got)
	}
}

func TestClearConditionAndMigrationMetrics(t *testing.T) {
	SetCondition("ns", "cm", "Degraded", metav1.ConditionTrue)
	SetCondition("ns", "cm", "Available", metav1.ConditionFalse)
	SetMigrationJobFailed("ns", "cm", "cm-koku-migrate", true)
	SetMigrationJobFailed("ns", "cm", "cm-ros-migrate", true)

	if n := ClearConditionMetrics("ns", "cm"); n < 2 {
		t.Fatalf("ClearConditionMetrics deleted %d, want ≥2", n)
	}
	if Condition.DeleteLabelValues("ns", "cm", "Degraded", "True") {
		t.Fatal("expected Degraded series deleted")
	}
	if n := ClearMigrationJobFailedAll("ns", "cm"); n != 2 {
		t.Fatalf("ClearMigrationJobFailedAll deleted %d, want 2", n)
	}
	if MigrationJobFailed.DeleteLabelValues("ns", "cm", "cm-koku-migrate") {
		t.Fatal("expected koku-migrate series deleted")
	}
}

func TestClearManagedPodRestarts(t *testing.T) {
	ManagedPodRestarts.WithLabelValues("ns", "cm", "pod-a", "app").Set(3)
	ManagedPodRestarts.WithLabelValues("ns", "cm", "pod-b", "app").Set(1)
	if n := ClearManagedPodRestarts("ns", "cm"); n != 2 {
		t.Fatalf("ClearManagedPodRestarts deleted %d, want 2", n)
	}
	if ManagedPodRestarts.DeleteLabelValues("ns", "cm", "pod-a", "app") {
		t.Fatal("expected pod-a series deleted")
	}
	if ManagedPodRestarts.DeleteLabelValues("ns", "cm", "pod-b", "app") {
		t.Fatal("expected pod-b series deleted")
	}
}
