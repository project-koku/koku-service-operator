package resources

import (
	"maps"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	costv1alpha1 "github.com/project-koku/koku-service-operator/api/v1alpha1"
)

// Workload identifies one sized workload in the profile table.
type Workload string

const (
	WorkloadKokuAPI             Workload = "koku-api"
	WorkloadMasu                Workload = "masu"
	WorkloadListener            Workload = "listener"
	WorkloadCeleryBeat          Workload = "celery-beat"
	WorkloadRBACAPI             Workload = "rbac-api"
	WorkloadRBACWorker          Workload = "rbac-worker"
	WorkloadKeycloakSync        Workload = "keycloak-sync"
	WorkloadEnvoy               Workload = "envoy"
	WorkloadUIOAuthProxy        Workload = "ui-oauth-proxy"
	WorkloadUIApp               Workload = "ui-app"
	WorkloadIngress             Workload = "ingress"
	WorkloadROSAPI              Workload = "ros-api"
	WorkloadROSPoller           Workload = "ros-poller"
	WorkloadROSHousekeeper      Workload = "ros-housekeeper"
	WorkloadROSProcessor        Workload = "ros-processor"
	WorkloadROSPartitionCleaner Workload = "ros-partition-cleaner"
	WorkloadKruize              Workload = "kruize"
	WorkloadKruizePartitions    Workload = "kruize-partitions"
)

// CeleryWorkload returns the Workload for a Celery worker queue
// (celery, priority, summary, ocp, cost_model, refresh, download).
func CeleryWorkload(queue string) Workload {
	return Workload("celery-worker-" + queue)
}

// ProfileKnobs holds the non-resource settings that vary per profile.
type ProfileKnobs struct {
	MaxUploadSize             int64
	MaxMemory                 int64
	EnvoyIngressTimeout       string
	EnvoyIngressPerTryTimeout string
	RouteTimeout              string
}

type sizing struct {
	Replicas  *int32
	Resources corev1.ResourceRequirements
}

func res(cpuReq, cpuLim, memReq, memLim string) corev1.ResourceRequirements {
	return corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse(cpuReq),
			corev1.ResourceMemory: resource.MustParse(memReq),
		},
		Limits: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse(cpuLim),
			corev1.ResourceMemory: resource.MustParse(memLim),
		},
	}
}

func sz(replicas int32, r corev1.ResourceRequirements) sizing {
	return sizing{Replicas: new(replicas), Resources: r}
}

func szNoReplicas(r corev1.ResourceRequirements) sizing {
	return sizing{Resources: r}
}

// Sizing values come from chart values.yaml and the values-{medium,large,xlarge}.yaml
// overlays (docs/design/profile-sizing-maps.md section 5).
var profileTable = buildProfileTable()

var profileKnobs = map[costv1alpha1.Profile]ProfileKnobs{
	costv1alpha1.ProfileSmall:  {MaxUploadSize: 104857600, MaxMemory: 33554432, EnvoyIngressTimeout: "180s", EnvoyIngressPerTryTimeout: "60s", RouteTimeout: "180s"},
	costv1alpha1.ProfileMedium: {MaxUploadSize: 209715200, MaxMemory: 67108864, EnvoyIngressTimeout: "180s", EnvoyIngressPerTryTimeout: "180s", RouteTimeout: "180s"},
	costv1alpha1.ProfileLarge:  {MaxUploadSize: 524288000, MaxMemory: 134217728, EnvoyIngressTimeout: "600s", EnvoyIngressPerTryTimeout: "300s", RouteTimeout: "600s"},
	costv1alpha1.ProfileXLarge: {MaxUploadSize: 524288000, MaxMemory: 134217728, EnvoyIngressTimeout: "600s", EnvoyIngressPerTryTimeout: "300s", RouteTimeout: "600s"},
}

func buildProfileTable() map[costv1alpha1.Profile]map[Workload]sizing {
	// D19: celery/cost_model/refresh/download memory deliberately deviates from
	// the chart values.yaml (200Mi/400Mi, 256Mi/512Mi, 512Mi/1Gi): those limits
	// OOM at boot on current koku images; healthy workers idle near 750Mi.
	small := map[Workload]sizing{
		WorkloadKokuAPI:              sz(1, res("250m", "1", "1Gi", "2Gi")),
		WorkloadMasu:                 sz(1, res("250m", "500m", "1Gi", "2Gi")),
		WorkloadListener:             sz(2, res("150m", "300m", "300Mi", "600Mi")),
		WorkloadCeleryBeat:           szNoReplicas(res("50m", "100m", "200Mi", "400Mi")),
		CeleryWorkload("celery"):     sz(1, res("100m", "200m", "1Gi", "2Gi")),
		CeleryWorkload("priority"):   sz(1, res("250m", "500m", "1Gi", "2Gi")),
		CeleryWorkload("refresh"):    sz(0, res("100m", "200m", "1Gi", "2Gi")),
		CeleryWorkload("summary"):    sz(2, res("250m", "500m", "1Gi", "2Gi")),
		CeleryWorkload("download"):   sz(0, res("200m", "400m", "1Gi", "2Gi")),
		CeleryWorkload("ocp"):        sz(2, res("250m", "500m", "512Mi", "1Gi")),
		CeleryWorkload("cost_model"): sz(1, res("100m", "200m", "1Gi", "2Gi")),
		WorkloadRBACAPI:              sz(1, res("250m", "500m", "512Mi", "1Gi")),
		WorkloadRBACWorker:           sz(1, res("100m", "500m", "512Mi", "2Gi")),
		WorkloadKeycloakSync:         szNoReplicas(res("100m", "250m", "256Mi", "512Mi")),
		WorkloadEnvoy:                sz(2, res("100m", "500m", "128Mi", "256Mi")),
		WorkloadUIOAuthProxy:         sz(1, res("50m", "100m", "64Mi", "128Mi")),
		WorkloadUIApp:                szNoReplicas(res("50m", "100m", "64Mi", "128Mi")),
		WorkloadIngress:              szNoReplicas(res("500m", "1", "1Gi", "1Gi")),
		WorkloadROSAPI:               szNoReplicas(res("500m", "1", "1Gi", "1Gi")),
		WorkloadROSPoller:            szNoReplicas(res("500m", "1", "1Gi", "1Gi")),
		WorkloadROSHousekeeper:       szNoReplicas(res("500m", "1", "1Gi", "1Gi")),
		WorkloadROSProcessor:         sz(1, res("500m", "1", "1Gi", "1Gi")),
		WorkloadROSPartitionCleaner:  szNoReplicas(res("100m", "300m", "256Mi", "512Mi")),
		WorkloadKruize:               sz(1, res("1000m", "2000m", "1Gi", "2Gi")),
		WorkloadKruizePartitions:     szNoReplicas(res("100m", "300m", "256Mi", "512Mi")),
	}

	// values-medium.yaml
	medium := deriveProfile(small, map[Workload]sizing{
		WorkloadListener:          sz(2, res("150m", "300m", "1Gi", "2Gi")),
		CeleryWorkload("ocp"):     sz(2, res("250m", "1000m", "512Mi", "2Gi")),
		CeleryWorkload("summary"): sz(2, res("250m", "1000m", "1Gi", "2Gi")),
		WorkloadROSProcessor:      sz(2, res("500m", "1", "2Gi", "4Gi")),
		WorkloadKruize:            sz(1, res("1000m", "2000m", "1Gi", "2Gi")),
		WorkloadIngress:           szNoReplicas(res("500m", "1", "1Gi", "2Gi")),
	})

	// values-large.yaml
	large := deriveProfile(small, map[Workload]sizing{
		WorkloadListener:          sz(3, res("150m", "300m", "2Gi", "4Gi")),
		CeleryWorkload("ocp"):     sz(3, res("500m", "1000m", "1Gi", "2Gi")),
		CeleryWorkload("summary"): sz(3, res("500m", "1000m", "1Gi", "2Gi")),
		WorkloadROSProcessor:      sz(3, res("500m", "1", "2Gi", "4Gi")),
		WorkloadKruize:            sz(1, res("1000m", "2000m", "2Gi", "4Gi")),
		WorkloadIngress:           szNoReplicas(res("500m", "1", "2Gi", "4Gi")),
	})

	// values-xlarge.yaml: large plus bigger ocp and summary workers.
	xlarge := deriveProfile(large, map[Workload]sizing{
		CeleryWorkload("ocp"):     sz(3, res("1000m", "2000m", "2Gi", "4Gi")),
		CeleryWorkload("summary"): sz(3, res("1000m", "2000m", "1Gi", "2Gi")),
	})

	return map[costv1alpha1.Profile]map[Workload]sizing{
		costv1alpha1.ProfileSmall:  small,
		costv1alpha1.ProfileMedium: medium,
		costv1alpha1.ProfileLarge:  large,
		costv1alpha1.ProfileXLarge: xlarge,
	}
}

// deriveProfile copies base and applies deltas, so untouched workloads
// visibly inherit from the base tier.
func deriveProfile(base, deltas map[Workload]sizing) map[Workload]sizing {
	out := make(map[Workload]sizing, len(base))
	maps.Copy(out, base)
	maps.Copy(out, deltas)
	return out
}

// EffectiveProfile returns the configured profile, or ProfileSmall when it is
// empty or unknown.
func EffectiveProfile(cfg *costv1alpha1.CostManagementServiceConfig) costv1alpha1.Profile {
	if _, ok := profileTable[cfg.Spec.Profile]; ok {
		return cfg.Spec.Profile
	}
	return costv1alpha1.ProfileSmall
}

// ProfileResources returns explicit when it sets any requests or limits;
// otherwise the profile's entry for w (a deep copy). A workload missing from
// the table passes explicit through.
func ProfileResources(cfg *costv1alpha1.CostManagementServiceConfig, w Workload, explicit corev1.ResourceRequirements) corev1.ResourceRequirements {
	if len(explicit.Requests) > 0 || len(explicit.Limits) > 0 {
		return explicit
	}
	s, ok := profileTable[EffectiveProfile(cfg)][w]
	if !ok {
		return explicit
	}
	return *s.Resources.DeepCopy()
}

// ProfileReplicas returns *explicit when set, otherwise the profile's replica
// count for w, or 1 when the table has no replica entry.
func ProfileReplicas(cfg *costv1alpha1.CostManagementServiceConfig, w Workload, explicit *int32) int32 {
	if explicit != nil {
		return *explicit
	}
	if s, ok := profileTable[EffectiveProfile(cfg)][w]; ok && s.Replicas != nil {
		return *s.Replicas
	}
	return 1
}

// ProfileKnobsFor returns the non-resource knobs for the effective profile.
func ProfileKnobsFor(cfg *costv1alpha1.CostManagementServiceConfig) ProfileKnobs {
	return profileKnobs[EffectiveProfile(cfg)]
}
