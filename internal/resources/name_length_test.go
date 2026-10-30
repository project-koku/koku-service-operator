package resources

import (
	"fmt"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/util/validation"

	costv1alpha1 "github.com/project-koku/koku-service-operator/api/v1alpha1"
)

// longNameCfg enables every feature that adds a length-sensitive child
// resource (bundled database, Keycloak sync CronJob, ROS/Kruize CronJobs).
func longNameCfg(n int, ros bool) *costv1alpha1.CostManagementServiceConfig {
	cfg := keycloakSyncCfg()
	cfg.Name = strings.Repeat("a", n)
	cfg.Spec.ROS.Enabled = new(ros)
	return cfg
}

// nameViolations reports every generated name or label value that exceeds its
// Kubernetes limit for cfg.
func nameViolations(cfg *costv1alpha1.CostManagementServiceConfig) []string {
	var out []string
	check := func(kind, name string, errs []string) {
		if len(errs) > 0 {
			out = append(out, fmt.Sprintf("%s %q: %s", kind, name, strings.Join(errs, "; ")))
		}
	}
	// CronJob names: the Job controller appends an 11-char suffix, so the
	// apiserver caps CronJob names at 52.
	cronJob := func(name string) []string {
		errs := validation.IsDNS1123Subdomain(name)
		if len(name) > 52 {
			errs = append(errs, fmt.Sprintf("length %d > 52", len(name)))
		}
		return errs
	}

	check("label app.kubernetes.io/instance", cfg.Name, validation.IsValidLabelValue(cfg.Name))
	check("CronJob", KeycloakSyncCronJob(cfg).Name, cronJob(KeycloakSyncCronJob(cfg).Name))
	if costv1alpha1.ROSEnabled(cfg) {
		check("CronJob", KruizeDeletePartitionsCronJob(cfg).Name, cronJob(KruizeDeletePartitionsCronJob(cfg).Name))
		check("CronJob", ROSPartitionCleanerCronJob(cfg).Name, cronJob(ROSPartitionCleanerCronJob(cfg).Name))
		check("Job", NameROSMigration(cfg), validation.IsValidLabelValue(NameROSMigration(cfg)))
		check("Service", ROSAPIService(cfg).Name, validation.IsDNS1035Label(ROSAPIService(cfg).Name))
		check("Service", KruizeService(cfg).Name, validation.IsDNS1035Label(KruizeService(cfg).Name))
	}
	// StatefulSet pods carry controller-revision-hash = "<name>-<hash>" (≤63).
	sts := DatabaseStatefulSet(cfg).Name
	check("StatefulSet", sts, validation.IsValidLabelValue(sts+"-0123456789"))
	// Jobs carry job-name = <name> as a label.
	for _, j := range []string{NameKokuMigration(cfg), NameRBACMigration(cfg)} {
		check("Job", j, validation.IsValidLabelValue(j))
	}
	// Deployment names are DNS-1123 subdomains (≤253); their ReplicaSets
	// (<name>-<hash>) are too, and generated Pod names are truncated by the
	// apiserver. Container names and Pod labels are DNS-1123 labels (≤63).
	deployments := append([]*appsv1.Deployment{
		CacheDeployment(cfg), CeleryBeatDeployment(cfg), EnvoyDeployment(cfg),
		IngressDeployment(cfg), KokuAPIDeployment(cfg), ListenerDeployment(cfg),
		MasuDeployment(cfg), RBACAPIDeployment(cfg), RBACWorkerDeployment(cfg),
		UIDeployment(cfg),
	}, CeleryWorkerDeployments(cfg)...)
	if costv1alpha1.ROSEnabled(cfg) {
		deployments = append(deployments, KruizeDeployment(cfg), ROSAPIDeployment(cfg),
			ROSHousekeeperDeployment(cfg), ROSPollerDeployment(cfg), ROSProcessorDeployment(cfg))
	}
	for _, d := range deployments {
		if d == nil {
			continue // not created for this config
		}
		check("Deployment", d.Name, validation.IsDNS1123Subdomain(d.Name))
		pod := d.Spec.Template.Spec
		for _, c := range append(pod.InitContainers, pod.Containers...) {
			check("container in "+d.Name, c.Name, validation.IsDNS1123Label(c.Name))
		}
		for k, v := range d.Spec.Template.Labels {
			check("pod label "+k+" in "+d.Name, v, validation.IsValidLabelValue(v))
		}
	}
	for _, svc := range []string{
		CacheService(cfg).Name, CeleryWorkersService(cfg).Name, DatabaseService(cfg).Name,
		EnvoyService(cfg).Name, IngressService(cfg).Name, KokuAPIService(cfg).Name,
		MasuService(cfg).Name, RBACAPIService(cfg).Name, UIService(cfg).Name,
	} {
		check("Service", svc, validation.IsDNS1035Label(svc))
	}
	return out
}

// D22: the CRD caps metadata.name (MaxNameLength / MaxNameLengthWithROS).
// At the cap every generated name is valid; one character more breaks at
// least one, so the cap is the tightest correct value.
func TestMaxCRNameFitsGeneratedNames(t *testing.T) {
	for _, tc := range []struct {
		name string
		ros  bool
		max  int
	}{
		{name: "ROS disabled", ros: false, max: costv1alpha1.MaxNameLength},
		{name: "ROS enabled", ros: true, max: costv1alpha1.MaxNameLengthWithROS},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Celery workers are always built (also at 0 replicas); make sure
			// the Deployment checks are not skipped as nil.
			for _, d := range CeleryWorkerDeployments(longNameCfg(tc.max, tc.ros)) {
				if d == nil {
					t.Fatal("CeleryWorkerDeployments returned nil; Deployment names would go unchecked")
				}
			}
			if v := nameViolations(longNameCfg(tc.max, tc.ros)); len(v) > 0 {
				t.Fatalf("name length %d (the cap) produces invalid resources:\n%s", tc.max, strings.Join(v, "\n"))
			}
			if v := nameViolations(longNameCfg(tc.max+1, tc.ros)); len(v) == 0 {
				t.Fatalf("name length %d still fits every resource; the cap %d is not tight", tc.max+1, tc.max)
			}
		})
	}
}
