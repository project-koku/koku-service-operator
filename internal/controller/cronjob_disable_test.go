package controller

import (
	"context"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	costv1alpha1 "github.com/project-koku/koku-service-operator/api/v1alpha1"
	"github.com/project-koku/koku-service-operator/internal/resources"
)

func cronJobScheme(t *testing.T) *runtime.Scheme {
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

// TestKruizeCronJobDeletedWhenDisabled verifies that when
// spec.kruize.partitions.deleteEnabled is set to false, any existing
// Kruize delete-partitions CronJob is deleted from the cluster.
//
// Bug (D8): the reconciler only stops adding the CronJob to the apply list;
// it never deletes an already-existing one, leaving it running indefinitely.
func TestKruizeCronJobDeletedWhenDisabled(t *testing.T) {
	const ns = "test"
	falseVal := false

	cfg := &costv1alpha1.CostManagementServiceConfig{
		ObjectMeta: metav1.ObjectMeta{Name: testCRName, Namespace: ns},
		Spec: costv1alpha1.CostManagementServiceConfigSpec{
			Kruize: costv1alpha1.KruizeConfig{
				Partitions: costv1alpha1.KruizePartitionsSpec{
					DeleteEnabled: &falseVal, // disabled
				},
			},
		},
	}

	// Pre-create the CronJob as if it was created during a previous reconcile
	// when the feature was enabled.
	existingCJ := resources.KruizeDeletePartitionsCronJob(cfg)
	existingCJ.Namespace = ns

	r := &CostManagementServiceConfigReconciler{
		Client:   fake.NewClientBuilder().WithScheme(cronJobScheme(t)).WithObjects(cfg, existingCJ).Build(),
		Recorder: &noopRecorder{},
	}

	// After the fix, reconcileWorkers must delete the CronJob when disabled.
	// For now (bug): run the reconciler and verify the CronJob still exists.
	_, _ = r.reconcileWorkers(context.Background(), cfg)

	cj := &batchv1.CronJob{}
	err := r.Get(context.Background(),
		types.NamespacedName{Namespace: ns, Name: existingCJ.Name}, cj)
	if err == nil {
		t.Errorf("BUG(D8): CronJob %q still exists after being disabled — "+
			"it should have been deleted by reconcileWorkers", existingCJ.Name)
	}
}
