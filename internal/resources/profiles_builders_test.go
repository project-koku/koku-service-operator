package resources

import (
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	costv1alpha1 "github.com/project-koku/koku-service-operator/api/v1alpha1"
)

func builderCfg(p costv1alpha1.Profile) *costv1alpha1.CostManagementServiceConfig {
	cfg := profileCfg(p)
	cfg.Name = "cost-management"
	cfg.Namespace = "cost-onprem"
	cfg.Spec.ROS.Enabled = new(true)
	return cfg
}

func depRes(d *appsv1.Deployment) corev1.ResourceRequirements {
	return d.Spec.Template.Spec.Containers[0].Resources
}

func cronRes(c *batchv1.CronJob) corev1.ResourceRequirements {
	return c.Spec.JobTemplate.Spec.Template.Spec.Containers[0].Resources
}

// builtResources renders every sized workload builder and returns the
// container resources keyed by Workload.
func builtResources(t *testing.T, cfg *costv1alpha1.CostManagementServiceConfig) map[Workload]corev1.ResourceRequirements {
	t.Helper()
	out := map[Workload]corev1.ResourceRequirements{
		WorkloadKokuAPI:             depRes(KokuAPIDeployment(cfg)),
		WorkloadMasu:                depRes(MasuDeployment(cfg)),
		WorkloadListener:            depRes(ListenerDeployment(cfg)),
		WorkloadCeleryBeat:          depRes(CeleryBeatDeployment(cfg)),
		WorkloadRBACAPI:             depRes(RBACAPIDeployment(cfg)),
		WorkloadRBACWorker:          depRes(RBACWorkerDeployment(cfg)),
		WorkloadKeycloakSync:        cronRes(KeycloakSyncCronJob(cfg)),
		WorkloadEnvoy:               depRes(EnvoyDeployment(cfg)),
		WorkloadIngress:             depRes(IngressDeployment(cfg)),
		WorkloadROSAPI:              depRes(ROSAPIDeployment(cfg)),
		WorkloadROSPoller:           depRes(ROSPollerDeployment(cfg)),
		WorkloadROSHousekeeper:      depRes(ROSHousekeeperDeployment(cfg)),
		WorkloadROSProcessor:        depRes(ROSProcessorDeployment(cfg)),
		WorkloadROSPartitionCleaner: cronRes(ROSPartitionCleanerCronJob(cfg)),
		WorkloadKruize:              depRes(KruizeDeployment(cfg)),
		WorkloadKruizePartitions:    cronRes(KruizeDeletePartitionsCronJob(cfg)),
	}
	for _, d := range CeleryWorkerDeployments(cfg) {
		q := d.Spec.Template.Spec.Containers[0].Env
		for _, e := range q {
			if e.Name == "WORKER_QUEUES" {
				out[CeleryWorkload(e.Value)] = depRes(d)
			}
		}
	}
	return out
}

func assertResources(t *testing.T, name string, got corev1.ResourceRequirements, c profileCell) {
	t.Helper()
	want := map[corev1.ResourceName][2]string{
		corev1.ResourceCPU:    {c.cpuReq, c.cpuLim},
		corev1.ResourceMemory: {c.memReq, c.memLim},
	}
	for rn, w := range want {
		if q := got.Requests[rn]; q.Cmp(resource.MustParse(w[0])) != 0 {
			t.Errorf("%s requests.%s = %s, want %s", name, rn, q.String(), w[0])
		}
		if q := got.Limits[rn]; q.Cmp(resource.MustParse(w[1])) != 0 {
			t.Errorf("%s limits.%s = %s, want %s", name, rn, q.String(), w[1])
		}
	}
}

func TestBuildersApplySmallProfileResources(t *testing.T) {
	got := builtResources(t, builderCfg(costv1alpha1.ProfileSmall))
	for _, c := range smallCells() {
		if c.w == WorkloadUIOAuthProxy || c.w == WorkloadUIApp {
			continue // wired and tested with the UI builder
		}
		r, ok := got[c.w]
		if !ok {
			t.Errorf("no builder output for %s", c.w)
			continue
		}
		assertResources(t, string(c.w), r, c)
	}
}

func TestKruizePartitionsInitContainerUsesProfile(t *testing.T) {
	d := KruizeDeployment(builderCfg(costv1alpha1.ProfileSmall))
	for _, ic := range d.Spec.Template.Spec.InitContainers {
		if ic.Name == "create-kruize-partitions" {
			assertResources(t, ic.Name, ic.Resources, noReplicas(WorkloadKruizePartitions, "100m", "300m", "256Mi", "512Mi"))
			return
		}
	}
	t.Fatal("create-kruize-partitions init container not found")
}

func TestSaaSCeleryQueuesPassThroughEmpty(t *testing.T) {
	for _, d := range CeleryWorkerDeployments(builderCfg(costv1alpha1.ProfileSmall)) {
		for _, e := range d.Spec.Template.Spec.Containers[0].Env {
			if e.Name == "WORKER_QUEUES" && (e.Value == "hcs" || e.Value == "subs_extraction" || e.Value == "subs_transmission") {
				if r := depRes(d); len(r.Requests) != 0 || len(r.Limits) != 0 {
					t.Errorf("queue %s: expected empty resources, got %+v", e.Value, r)
				}
			}
		}
	}
}

func TestBuildersApplyXLargeDeltas(t *testing.T) {
	got := builtResources(t, builderCfg(costv1alpha1.ProfileXLarge))
	for _, c := range []profileCell{
		cell(CeleryWorkload("ocp"), 3, "1000m", "2000m", "2Gi", "4Gi"),
		cell(CeleryWorkload("summary"), 3, "1000m", "2000m", "1Gi", "2Gi"),
		noReplicas(WorkloadIngress, "500m", "1", "2Gi", "4Gi"),
		noReplicas(WorkloadROSAPI, "500m", "1", "1Gi", "1Gi"),
		cell(WorkloadListener, 3, "150m", "300m", "2Gi", "4Gi"),
		cell(WorkloadKruize, 1, "1000m", "2000m", "2Gi", "4Gi"),
	} {
		assertResources(t, string(c.w), got[c.w], c)
	}
}

func TestBuilderExplicitResourcesWin(t *testing.T) {
	cfg := builderCfg(costv1alpha1.ProfileSmall)
	cfg.Spec.CostManagement.Masu.Resources = corev1.ResourceRequirements{
		Limits: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("3")},
	}
	r := depRes(MasuDeployment(cfg))
	if len(r.Requests) != 0 || len(r.Limits) != 1 {
		t.Fatalf("explicit block not used as-is: %+v", r)
	}
	if q := r.Limits[corev1.ResourceCPU]; q.Cmp(resource.MustParse("3")) != 0 {
		t.Errorf("cpu limit = %s, want 3", q.String())
	}
}

func TestProfileSwitchRerenders(t *testing.T) {
	cfg := builderCfg(costv1alpha1.ProfileSmall)
	assertResources(t, "listener/small", depRes(ListenerDeployment(cfg)),
		cell(WorkloadListener, 2, "150m", "300m", "300Mi", "600Mi"))
	cfg.Spec.Profile = costv1alpha1.ProfileLarge
	assertResources(t, "listener/large", depRes(ListenerDeployment(cfg)),
		cell(WorkloadListener, 3, "150m", "300m", "2Gi", "4Gi"))
}
