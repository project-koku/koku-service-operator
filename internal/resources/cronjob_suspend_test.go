package resources

import (
	"testing"

	batchv1 "k8s.io/api/batch/v1"
)

// Upgrade quiescence suspends CronJobs with a merge patch. Builders must set
// suspend explicitly so the operator's server-side apply owns the field and
// resumes schedules after rollout; an omitted field would stay suspended.
func TestCronJobsAssertNotSuspended(t *testing.T) {
	t.Parallel()
	cfg := minimalCRForResources("cm", "ns")
	cfg.Spec.RBAC.KeycloakSync.Enabled = true
	cfg.Spec.RBAC.KeycloakSync.ClientSecretRef.Name = "keycloak-sync"
	for name, job := range map[string]*batchv1.CronJob{
		"keycloak sync":          KeycloakSyncCronJob(cfg),
		"ros partition cleaner":  ROSPartitionCleanerCronJob(cfg),
		"kruize partition purge": KruizeDeletePartitionsCronJob(cfg),
	} {
		if job == nil {
			continue
		}
		if job.Spec.Suspend == nil || *job.Spec.Suspend {
			t.Errorf("%s CronJob must set spec.suspend=false explicitly", name)
		}
	}
}
