package resources

import (
	"slices"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"

	costv1alpha1 "github.com/project-koku/koku-service-operator/api/v1alpha1"
)

// Every consumer must render the same immutable image selected by the shared
// resolver, including scheduled workloads that do not pass through deployment().
func TestWorkloads_UseDigestPins(t *testing.T) {
	t.Parallel()
	cfg := minimalCRForResources("cm", "ns")
	pin := func(component string) costv1alpha1.ImageSpec {
		return costv1alpha1.ImageSpec{Repository: "quay.io/test/" + component, Tag: "descriptive", Digest: "sha256:" + strings.Repeat("a", 64)}
	}
	cfg.Spec.CostManagement.API.Image = pin("koku")
	cfg.Spec.CostManagement.Masu.Image = pin("masu")
	cfg.Spec.RBAC.Image = pin("rbac")
	cfg.Spec.ROS.Image = pin("ros")
	cfg.Spec.Kruize.Image = pin("kruize")
	cfg.Spec.Ingress.Image = pin("ingress")
	cfg.Spec.UI.App.Image = pin("ui")
	cfg.Spec.UI.OAuthProxy.Image = pin("oauth-proxy")
	cfg.Spec.Auth.Envoy.Image = pin("envoy")
	cfg.Spec.Database.Image = pin("postgres")
	cfg.Spec.Cache.Image = pin("cache")
	cfg.Spec.Database.Deploy = new(true)
	cfg.Spec.Cache.Deploy = new(true)
	cfg.Spec.ROS.Enabled = new(true)

	if missing := MissingWorkloadImages(cfg); len(missing) != 0 {
		t.Fatalf("digest pins should satisfy required images: %v", missing)
	}
	assertImages := func(name string, containers []corev1.Container, components ...string) {
		t.Helper()
		got := make([]string, 0, len(containers))
		want := make([]string, 0, len(components))
		for _, container := range containers {
			got = append(got, container.Image)
		}
		for _, component := range components {
			want = append(want, "quay.io/test/"+component+"@sha256:"+strings.Repeat("a", 64))
		}
		slices.Sort(got)
		slices.Sort(want)
		if !slices.Equal(got, want) {
			t.Errorf("%s images = %v, want %v", name, got, want)
		}
	}
	for _, workload := range []struct {
		deployment *appsv1.Deployment
		components []string
	}{
		{KokuAPIDeployment(cfg), []string{"koku"}}, {MasuDeployment(cfg), []string{"masu"}},
		{ListenerDeployment(cfg), []string{"koku"}}, {CeleryBeatDeployment(cfg), []string{"koku"}},
		{CeleryWorkerDeployment(cfg, "celery", costv1alpha1.CeleryWorkerSpec{}), []string{"koku"}},
		{RBACAPIDeployment(cfg), []string{"rbac"}}, {RBACWorkerDeployment(cfg), []string{"rbac"}},
		{ROSAPIDeployment(cfg), []string{"ros"}}, {ROSProcessorDeployment(cfg), []string{"ros"}},
		{ROSPollerDeployment(cfg), []string{"ros"}}, {ROSHousekeeperDeployment(cfg), []string{"ros"}},
		{KruizeDeployment(cfg), []string{"kruize"}}, {IngressDeployment(cfg), []string{"ingress"}},
		{UIDeployment(cfg), []string{"ui", "oauth-proxy"}}, {EnvoyDeployment(cfg), []string{"envoy"}},
		{CacheDeployment(cfg), []string{"cache"}},
	} {
		assertImages(workload.deployment.Name, workload.deployment.Spec.Template.Spec.Containers, workload.components...)
	}
	for _, workload := range []struct {
		cronJob   *batchv1.CronJob
		component string
	}{
		{KeycloakSyncCronJob(cfg), "rbac"}, {ROSPartitionCleanerCronJob(cfg), "ros"},
		{KruizeDeletePartitionsCronJob(cfg), "kruize"},
	} {
		assertImages(workload.cronJob.Name, workload.cronJob.Spec.JobTemplate.Spec.Template.Spec.Containers, workload.component)
	}
	for _, workload := range []struct {
		job       *batchv1.Job
		component string
	}{
		{MigrationJob(cfg, "descriptive"), "koku"}, {RBACMigrationJob(cfg, "descriptive"), "rbac"},
		{ROSMigrationJob(cfg, "descriptive"), "ros"},
	} {
		assertImages(workload.job.Name, workload.job.Spec.Template.Spec.Containers, workload.component)
	}
	assertImages("database", DatabaseStatefulSet(cfg).Spec.Template.Spec.Containers, "postgres")
	assertImages("bundled database wait", []corev1.Container{waitForPostgres(cfg, "postgres", "5432")}, "postgres")
}
