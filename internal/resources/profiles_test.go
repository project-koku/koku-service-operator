package resources

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/utils/ptr"

	costv1alpha1 "github.com/project-koku/koku-service-operator/api/v1alpha1"
)

func profileCfg(p costv1alpha1.Profile) *costv1alpha1.CostManagementServiceConfig {
	cfg := &costv1alpha1.CostManagementServiceConfig{}
	cfg.Spec.Profile = p
	return cfg
}

type profileCell struct {
	w              Workload
	replicas       int32
	cpuReq, cpuLim string
	memReq, memLim string
}

func cell(w Workload, replicas int32, cpuReq, cpuLim, memReq, memLim string) profileCell {
	return profileCell{w, replicas, cpuReq, cpuLim, memReq, memLim}
}

func noReplicas(w Workload, cpuReq, cpuLim, memReq, memLim string) profileCell {
	return profileCell{w, 1, cpuReq, cpuLim, memReq, memLim}
}

// smallCells is the executable copy of spec section 5.1.
func smallCells() []profileCell {
	return []profileCell{
		cell(WorkloadKokuAPI, 1, "250m", "1", "1Gi", "2Gi"),
		cell(WorkloadMasu, 1, "250m", "500m", "1Gi", "2Gi"),
		cell(WorkloadListener, 2, "150m", "300m", "300Mi", "600Mi"),
		noReplicas(WorkloadCeleryBeat, "50m", "100m", "200Mi", "400Mi"),
		cell(CeleryWorkload("celery"), 1, "100m", "200m", "1Gi", "2Gi"),
		cell(CeleryWorkload("priority"), 1, "250m", "500m", "1Gi", "2Gi"),
		cell(CeleryWorkload("refresh"), 0, "100m", "200m", "1Gi", "2Gi"),
		cell(CeleryWorkload("summary"), 2, "250m", "500m", "1Gi", "2Gi"),
		cell(CeleryWorkload("download"), 0, "200m", "400m", "1Gi", "2Gi"),
		cell(CeleryWorkload("ocp"), 2, "250m", "500m", "512Mi", "1Gi"),
		cell(CeleryWorkload("cost_model"), 1, "100m", "200m", "1Gi", "2Gi"),
		cell(WorkloadRBACAPI, 1, "250m", "500m", "512Mi", "1Gi"),
		cell(WorkloadRBACWorker, 1, "100m", "500m", "512Mi", "2Gi"),
		noReplicas(WorkloadKeycloakSync, "100m", "250m", "256Mi", "512Mi"),
		cell(WorkloadEnvoy, 2, "100m", "500m", "128Mi", "256Mi"),
		cell(WorkloadUIOAuthProxy, 1, "50m", "100m", "64Mi", "128Mi"),
		noReplicas(WorkloadUIApp, "50m", "100m", "64Mi", "128Mi"),
		noReplicas(WorkloadIngress, "500m", "1", "1Gi", "1Gi"),
		noReplicas(WorkloadROSAPI, "500m", "1", "1Gi", "1Gi"),
		noReplicas(WorkloadROSPoller, "500m", "1", "1Gi", "1Gi"),
		noReplicas(WorkloadROSHousekeeper, "500m", "1", "1Gi", "1Gi"),
		cell(WorkloadROSProcessor, 1, "500m", "1", "1Gi", "1Gi"),
		noReplicas(WorkloadROSPartitionCleaner, "100m", "300m", "256Mi", "512Mi"),
		cell(WorkloadKruize, 1, "1000m", "2000m", "1Gi", "2Gi"),
		noReplicas(WorkloadKruizePartitions, "100m", "300m", "256Mi", "512Mi"),
	}
}

// override replaces the cell for the same workload in a copy of cells.
func override(cells []profileCell, over ...profileCell) []profileCell {
	out := append([]profileCell(nil), cells...)
	for _, o := range over {
		for i := range out {
			if out[i].w == o.w {
				out[i] = o
			}
		}
	}
	return out
}

func TestProfileTable(t *testing.T) {
	small := smallCells()
	// medium: spec section 5.2
	medium := override(small,
		cell(WorkloadListener, 2, "150m", "300m", "1Gi", "2Gi"),
		cell(CeleryWorkload("ocp"), 2, "250m", "1000m", "512Mi", "2Gi"),
		cell(CeleryWorkload("summary"), 2, "250m", "1000m", "1Gi", "2Gi"),
		cell(WorkloadROSProcessor, 2, "500m", "1", "2Gi", "4Gi"),
		cell(WorkloadKruize, 1, "1000m", "2000m", "1Gi", "2Gi"),
		noReplicas(WorkloadIngress, "500m", "1", "1Gi", "2Gi"),
	)
	// large: spec section 5.3
	large := override(small,
		cell(WorkloadListener, 3, "150m", "300m", "2Gi", "4Gi"),
		cell(CeleryWorkload("ocp"), 3, "500m", "1000m", "1Gi", "2Gi"),
		cell(CeleryWorkload("summary"), 3, "500m", "1000m", "1Gi", "2Gi"),
		cell(WorkloadROSProcessor, 3, "500m", "1", "2Gi", "4Gi"),
		cell(WorkloadKruize, 1, "1000m", "2000m", "2Gi", "4Gi"),
		noReplicas(WorkloadIngress, "500m", "1", "2Gi", "4Gi"),
	)
	// xlarge: spec section 5.4 (large plus ocp/summary CPU and ocp memory)
	xlarge := override(large,
		cell(CeleryWorkload("ocp"), 3, "1000m", "2000m", "2Gi", "4Gi"),
		cell(CeleryWorkload("summary"), 3, "1000m", "2000m", "1Gi", "2Gi"),
	)

	tiers := []struct {
		profile costv1alpha1.Profile
		cells   []profileCell
	}{
		{costv1alpha1.ProfileSmall, small},
		{costv1alpha1.ProfileMedium, medium},
		{costv1alpha1.ProfileLarge, large},
		{costv1alpha1.ProfileXLarge, xlarge},
	}
	for _, tier := range tiers {
		cfg := profileCfg(tier.profile)
		for _, c := range tier.cells {
			t.Run(string(tier.profile)+"/"+string(c.w), func(t *testing.T) {
				got := ProfileResources(cfg, c.w, corev1.ResourceRequirements{})
				checks := []struct {
					name string
					got  resource.Quantity
					want string
				}{
					{"cpu request", got.Requests[corev1.ResourceCPU], c.cpuReq},
					{"cpu limit", got.Limits[corev1.ResourceCPU], c.cpuLim},
					{"memory request", got.Requests[corev1.ResourceMemory], c.memReq},
					{"memory limit", got.Limits[corev1.ResourceMemory], c.memLim},
				}
				for _, ch := range checks {
					want := resource.MustParse(ch.want)
					if ch.got.Cmp(want) != 0 {
						t.Errorf("%s = %s, want %s", ch.name, ch.got.String(), ch.want)
					}
				}
				if gotR := ProfileReplicas(cfg, c.w, nil); gotR != c.replicas {
					t.Errorf("replicas = %d, want %d", gotR, c.replicas)
				}
			})
		}
	}
}

func TestEffectiveProfile_EmptyAndUnknownAreSmall(t *testing.T) {
	for _, p := range []costv1alpha1.Profile{"", "ha", "standard"} {
		if got := EffectiveProfile(profileCfg(p)); got != costv1alpha1.ProfileSmall {
			t.Errorf("EffectiveProfile(%q) = %q, want small", p, got)
		}
	}
	if got := EffectiveProfile(profileCfg(costv1alpha1.ProfileLarge)); got != costv1alpha1.ProfileLarge {
		t.Errorf("EffectiveProfile(large) = %q", got)
	}
}

func TestProfileResources_ExplicitBlockWins(t *testing.T) {
	explicit := corev1.ResourceRequirements{
		Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("7m"), corev1.ResourceMemory: resource.MustParse("9Mi")},
		Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("8m"), corev1.ResourceMemory: resource.MustParse("10Mi")},
	}
	got := ProfileResources(profileCfg(costv1alpha1.ProfileLarge), WorkloadKokuAPI, explicit)
	for name, want := range explicit.Requests {
		if g := got.Requests[name]; g.Cmp(want) != 0 {
			t.Errorf("request %s = %s, want %s", name, g.String(), want.String())
		}
	}
	for name, want := range explicit.Limits {
		if g := got.Limits[name]; g.Cmp(want) != 0 {
			t.Errorf("limit %s = %s, want %s", name, g.String(), want.String())
		}
	}
}

func TestProfileResources_PartialExplicitBlockUsedAsIs(t *testing.T) {
	explicit := corev1.ResourceRequirements{
		Limits: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("3Gi")},
	}
	got := ProfileResources(profileCfg(costv1alpha1.ProfileSmall), WorkloadKokuAPI, explicit)
	if len(got.Requests) != 0 {
		t.Errorf("requests = %v, want empty", got.Requests)
	}
	if len(got.Limits) != 1 {
		t.Fatalf("limits = %v, want only memory", got.Limits)
	}
	if g := got.Limits[corev1.ResourceMemory]; g.Cmp(resource.MustParse("3Gi")) != 0 {
		t.Errorf("memory limit = %s, want 3Gi", g.String())
	}
}

func TestProfileResources_ReturnsDeepCopy(t *testing.T) {
	cfg := profileCfg(costv1alpha1.ProfileSmall)
	first := ProfileResources(cfg, WorkloadKokuAPI, corev1.ResourceRequirements{})
	first.Requests[corev1.ResourceCPU] = resource.MustParse("99")
	delete(first.Limits, corev1.ResourceMemory)

	second := ProfileResources(cfg, WorkloadKokuAPI, corev1.ResourceRequirements{})
	if g := second.Requests[corev1.ResourceCPU]; g.Cmp(resource.MustParse("250m")) != 0 {
		t.Errorf("cpu request = %s, want 250m (table was mutated)", g.String())
	}
	if g := second.Limits[corev1.ResourceMemory]; g.Cmp(resource.MustParse("2Gi")) != 0 {
		t.Errorf("memory limit = %s, want 2Gi (table was mutated)", g.String())
	}
}

func TestProfileResources_UnknownWorkloadPassesThrough(t *testing.T) {
	got := ProfileResources(profileCfg(costv1alpha1.ProfileLarge), CeleryWorkload("hcs"), corev1.ResourceRequirements{})
	if len(got.Requests) != 0 || len(got.Limits) != 0 {
		t.Errorf("got %v, want empty", got)
	}
}

func TestProfileReplicas_ExplicitZeroWins(t *testing.T) {
	if got := ProfileReplicas(profileCfg(costv1alpha1.ProfileLarge), WorkloadListener, ptr.To[int32](0)); got != 0 {
		t.Errorf("replicas = %d, want 0", got)
	}
}

func TestProfileReplicas_Defaults(t *testing.T) {
	small := profileCfg(costv1alpha1.ProfileSmall)
	cases := []struct {
		w    Workload
		want int32
	}{
		{CeleryWorkload("ocp"), 2},
		{CeleryWorkload("summary"), 2},
		{CeleryWorkload("download"), 0},
		{CeleryWorkload("refresh"), 0},
		{WorkloadListener, 2},
		{WorkloadEnvoy, 2},
		{WorkloadIngress, 1},
	}
	for _, c := range cases {
		if got := ProfileReplicas(small, c.w, nil); got != c.want {
			t.Errorf("small %s replicas = %d, want %d", c.w, got, c.want)
		}
	}
	if got := ProfileReplicas(profileCfg(costv1alpha1.ProfileLarge), WorkloadListener, nil); got != 3 {
		t.Errorf("large listener replicas = %d, want 3", got)
	}
}

func TestProfileKnobsFor(t *testing.T) {
	cases := []struct {
		profile costv1alpha1.Profile
		want    ProfileKnobs
	}{
		{costv1alpha1.ProfileSmall, ProfileKnobs{MaxUploadSize: 104857600, MaxMemory: 33554432, EnvoyIngressTimeout: "180s", EnvoyIngressPerTryTimeout: "60s", RouteTimeout: "180s"}},
		{costv1alpha1.ProfileMedium, ProfileKnobs{MaxUploadSize: 209715200, MaxMemory: 67108864, EnvoyIngressTimeout: "180s", EnvoyIngressPerTryTimeout: "180s", RouteTimeout: "180s"}},
		{costv1alpha1.ProfileLarge, ProfileKnobs{MaxUploadSize: 524288000, MaxMemory: 134217728, EnvoyIngressTimeout: "600s", EnvoyIngressPerTryTimeout: "300s", RouteTimeout: "600s"}},
		{costv1alpha1.ProfileXLarge, ProfileKnobs{MaxUploadSize: 524288000, MaxMemory: 134217728, EnvoyIngressTimeout: "600s", EnvoyIngressPerTryTimeout: "300s", RouteTimeout: "600s"}},
		{"", ProfileKnobs{MaxUploadSize: 104857600, MaxMemory: 33554432, EnvoyIngressTimeout: "180s", EnvoyIngressPerTryTimeout: "60s", RouteTimeout: "180s"}},
	}
	for _, c := range cases {
		if got := ProfileKnobsFor(profileCfg(c.profile)); got != c.want {
			t.Errorf("%q knobs = %+v, want %+v", c.profile, got, c.want)
		}
	}
}
