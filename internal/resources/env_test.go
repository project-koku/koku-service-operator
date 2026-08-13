package resources

import (
	"testing"

	corev1 "k8s.io/api/core/v1"

	costv1alpha1 "github.com/project-koku/koku-service-operator/api/v1alpha1"
)

func envVal(env []corev1.EnvVar, name string) (string, bool) {
	for _, e := range env {
		if e.Name == name {
			return e.Value, true
		}
	}
	return "", false
}

func TestKokuCommonEnvRetainNumMonthsDefaultsWhenZero(t *testing.T) {
	// Simulates a CR persisted before DataRetentionMonths existed: the
	// field reads back as the Go zero value, not the CRD default of 4.
	cfg := &costv1alpha1.CostManagementServiceConfig{}
	cfg.Spec.CostManagement.DataRetentionMonths = 0

	env := KokuCommonEnv(cfg)

	got, ok := envVal(env, "RETAIN_NUM_MONTHS")
	if !ok {
		t.Fatal("RETAIN_NUM_MONTHS not set")
	}
	if got != "4" {
		t.Fatalf("RETAIN_NUM_MONTHS: got %q, want %q (must not be \"0\", koku treats that as literal zero-month retention)", got, "4")
	}
}

func TestKokuCommonEnvRetainNumMonthsRespectsExplicitValue(t *testing.T) {
	cfg := &costv1alpha1.CostManagementServiceConfig{}
	cfg.Spec.CostManagement.DataRetentionMonths = 12

	env := KokuCommonEnv(cfg)

	got, ok := envVal(env, "RETAIN_NUM_MONTHS")
	if !ok {
		t.Fatal("RETAIN_NUM_MONTHS not set")
	}
	if got != "12" {
		t.Fatalf("RETAIN_NUM_MONTHS: got %q, want %q", got, "12")
	}
}

func TestMergeEnvStableOrder(t *testing.T) {
	overrides := map[string]string{
		"Z_LAST":  "z",
		"A_FIRST": "a",
		"M_MID":   "m",
	}
	var first []string
	for i := range 20 {
		merged := MergeEnv(nil, overrides)
		names := make([]string, len(merged))
		for j, e := range merged {
			names[j] = e.Name
		}
		if i == 0 {
			first = names
			continue
		}
		for j := range names {
			if names[j] != first[j] {
				t.Fatalf("unstable env order on iteration %d: got %v want %v", i, names, first)
			}
		}
	}
	want := []string{"A_FIRST", "M_MID", "Z_LAST"}
	for i, name := range want {
		if first[i] != name {
			t.Fatalf("expected sorted keys %v, got %v", want, first)
		}
	}
}

func TestMergeEnvOverrideReplacesBase(t *testing.T) {
	base := []corev1.EnvVar{
		EnvVal("REDIS_HOST", "operator-default"),
		EnvVal("DB_HOST", "keep-this"),
	}
	overrides := map[string]string{
		"REDIS_HOST": "user-override",
		"NEW_VAR":    "new-value",
	}

	merged := MergeEnv(base, overrides)

	vals := make(map[string]string, len(merged))
	for _, e := range merged {
		if _, dup := vals[e.Name]; dup {
			t.Fatalf("duplicate env var %q", e.Name)
		}
		vals[e.Name] = e.Value
	}

	if vals["REDIS_HOST"] != "user-override" {
		t.Fatalf("REDIS_HOST: got %q, want %q", vals["REDIS_HOST"], "user-override")
	}
	if vals["DB_HOST"] != "keep-this" {
		t.Fatalf("DB_HOST: got %q, want %q", vals["DB_HOST"], "keep-this")
	}
	if vals["NEW_VAR"] != "new-value" {
		t.Fatalf("NEW_VAR: got %q, want %q", vals["NEW_VAR"], "new-value")
	}
	if len(merged) != 3 {
		t.Fatalf("expected 3 env vars, got %d", len(merged))
	}
}
