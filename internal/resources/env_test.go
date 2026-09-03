package resources

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	costv1alpha1 "github.com/project-koku/koku-service-operator/api/v1alpha1"
)

func envValue(env []corev1.EnvVar, name string) (string, bool) {
	for _, e := range env {
		if e.Name == name {
			return e.Value, true
		}
	}
	return "", false
}

func TestKokuCommonEnvRequestedBucketPrefersDiscovered(t *testing.T) {
	cfg := &costv1alpha1.CostManagementServiceConfig{
		ObjectMeta: metav1.ObjectMeta{Name: "cost-management", Namespace: "cost-onprem"},
		Spec: costv1alpha1.CostManagementServiceConfigSpec{
			ObjectStorage: costv1alpha1.ObjectStorageConfig{
				Buckets: costv1alpha1.ObjectStorageBucketsSpec{
					Koku: "koku-bucket",
					ROS:  "ros-data",
				},
			},
		},
		Status: costv1alpha1.CostManagementServiceConfigStatus{
			DiscoveredConfig: &costv1alpha1.DiscoveredConfig{
				S3: &costv1alpha1.DiscoveredS3{Bucket: "obc-provisioned-bucket"},
			},
		},
	}

	env := KokuCommonEnv(cfg)
	got, ok := envValue(env, "REQUESTED_BUCKET")
	if !ok {
		t.Fatal("missing REQUESTED_BUCKET")
	}
	if got != "obc-provisioned-bucket" {
		t.Errorf("REQUESTED_BUCKET = %q, want discovered obc-provisioned-bucket", got)
	}
	ros, ok := envValue(env, "REQUESTED_ROS_BUCKET")
	if !ok {
		t.Fatal("missing REQUESTED_ROS_BUCKET")
	}
	if ros != "ros-data" {
		t.Errorf("REQUESTED_ROS_BUCKET = %q, want spec ros-data (unchanged)", ros)
	}
}

func TestKokuCommonEnvS3CredentialNames(t *testing.T) {
	cfg := &costv1alpha1.CostManagementServiceConfig{
		ObjectMeta: metav1.ObjectMeta{Name: "cost-management", Namespace: "cost-onprem"},
		Spec: costv1alpha1.CostManagementServiceConfigSpec{
			ObjectStorage: costv1alpha1.ObjectStorageConfig{
				SecretName: "user-s3-creds",
			},
		},
	}
	storageSecret := NameStorageSecret(cfg)
	if storageSecret != "user-s3-creds" {
		t.Fatalf("NameStorageSecret = %q, want user-s3-creds", storageSecret)
	}

	env := KokuCommonEnv(cfg)
	byName := make(map[string]corev1.EnvVar, len(env))
	for _, e := range env {
		if _, dup := byName[e.Name]; dup {
			t.Fatalf("duplicate env var %q", e.Name)
		}
		byName[e.Name] = e
	}

	// Contract: koku EnvConfigurator reads S3_ACCESS_KEY / S3_SECRET into
	// settings.S3_*; AWS_* remain for the boto3 default credential chain.
	want := map[string]string{
		"S3_ACCESS_KEY":         "access-key",
		"S3_SECRET":             "secret-key",
		"AWS_ACCESS_KEY_ID":     "access-key",
		"AWS_SECRET_ACCESS_KEY": "secret-key",
	}
	for name, wantKey := range want {
		e, ok := byName[name]
		if !ok {
			t.Fatalf("missing env var %s", name)
		}
		if e.ValueFrom == nil || e.ValueFrom.SecretKeyRef == nil {
			t.Fatalf("%s: expected secretKeyRef, got %#v", name, e)
		}
		ref := e.ValueFrom.SecretKeyRef
		if ref.Name != storageSecret {
			t.Errorf("%s secret name: got %q, want %q", name, ref.Name, storageSecret)
		}
		if ref.Key != wantKey {
			t.Errorf("%s secret key: got %q, want %q", name, ref.Key, wantKey)
		}
		if ref.Optional == nil || !*ref.Optional {
			t.Errorf("%s: expected Optional=true secretKeyRef", name)
		}
	}
}

func TestKokuWorkloadsCarryS3CredentialEnv(t *testing.T) {
	cfg := &costv1alpha1.CostManagementServiceConfig{
		ObjectMeta: metav1.ObjectMeta{Name: "cost-management", Namespace: "cost-onprem"},
		Spec: costv1alpha1.CostManagementServiceConfigSpec{
			ObjectStorage: costv1alpha1.ObjectStorageConfig{
				SecretName: "user-s3-creds",
			},
			CostManagement: costv1alpha1.CostManagementConfig{
				API: costv1alpha1.KokuAPISpec{
					Image: costv1alpha1.ImageSpec{Repository: "quay.io/example/koku", Tag: "v1"},
				},
			},
		},
	}
	wantSecret := NameStorageSecret(cfg)
	want := map[string]string{
		"S3_ACCESS_KEY":         "access-key",
		"S3_SECRET":             "secret-key",
		"AWS_ACCESS_KEY_ID":     "access-key",
		"AWS_SECRET_ACCESS_KEY": "secret-key",
	}

	workloads := []struct {
		name       string
		containers []corev1.Container
	}{
		{"koku-api", KokuAPIDeployment(cfg).Spec.Template.Spec.Containers},
		{"masu", MasuDeployment(cfg).Spec.Template.Spec.Containers},
		{"listener", ListenerDeployment(cfg).Spec.Template.Spec.Containers},
		{"celery-beat", CeleryBeatDeployment(cfg).Spec.Template.Spec.Containers},
		{"celery-worker", CeleryWorkerDeployment(cfg, "celery", costv1alpha1.CeleryWorkerSpec{Replicas: 1}).Spec.Template.Spec.Containers},
		{"migration", MigrationJob(cfg, "latest").Spec.Template.Spec.Containers},
	}
	for _, wl := range workloads {
		byName := map[string]corev1.EnvVar{}
		for _, c := range wl.containers {
			for _, e := range c.Env {
				byName[e.Name] = e
			}
		}
		for name, wantKey := range want {
			e, ok := byName[name]
			if !ok {
				t.Errorf("%s: missing env var %s", wl.name, name)
				continue
			}
			if e.ValueFrom == nil || e.ValueFrom.SecretKeyRef == nil {
				t.Errorf("%s: %s expected secretKeyRef, got %#v", wl.name, name, e)
				continue
			}
			ref := e.ValueFrom.SecretKeyRef
			if ref.Name != wantSecret {
				t.Errorf("%s: %s secret name = %q, want %q", wl.name, name, ref.Name, wantSecret)
			}
			if ref.Key != wantKey {
				t.Errorf("%s: %s secret key = %q, want %q", wl.name, name, ref.Key, wantKey)
			}
		}
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

func TestKokuCommonEnvEnhancedOrgAdmin(t *testing.T) {
	cfg := &costv1alpha1.CostManagementServiceConfig{
		ObjectMeta: metav1.ObjectMeta{Name: "cost-management", Namespace: "cost-onprem"},
	}
	env := KokuCommonEnv(cfg)
	got, ok := envValue(env, "ENHANCED_ORG_ADMIN")
	if !ok {
		t.Fatal("missing ENHANCED_ORG_ADMIN")
	}
	if got != "False" {
		t.Errorf("ENHANCED_ORG_ADMIN = %q, want False", got)
	}
}

func TestKokuCommonEnvSharedDefaults(t *testing.T) {
	cfg := &costv1alpha1.CostManagementServiceConfig{
		ObjectMeta: metav1.ObjectMeta{Name: "cost-management", Namespace: "cost-onprem"},
		Spec: costv1alpha1.CostManagementServiceConfigSpec{
			ObjectStorage: costv1alpha1.ObjectStorageConfig{
				S3: costv1alpha1.S3Options{
					Region: "onprem",
				},
			},
			CostManagement: costv1alpha1.CostManagementConfig{
				ReportDownloadSchedule: "*/5 * * * *",
			},
		},
	}
	env := KokuCommonEnv(cfg)

	byName := make(map[string]string, len(env))
	for _, e := range env {
		if _, dup := byName[e.Name]; dup {
			t.Fatalf("duplicate env var %q", e.Name)
		}
		if e.Value != "" {
			byName[e.Name] = e.Value
		}
	}

	// Shared defaults from KokuCommonEnv (match chart defaults)
	want := map[string]string{
		"ONPREM":                    "True",
		"DATABASE_SERVICE_NAME":     "database",
		"DATABASE_ENGINE":           "postgresql",
		"DATABASE_NAME":             "costonprem_koku",
		"DATABASE_SERVICE_PORT":     "5432",
		"REDIS_PORT":                "6379",
		"S3_REGION":                 "onprem",
		"AWS_CONFIG_FILE":           "/etc/aws/config",
		"SCHEDULE_REPORT_CHECKS":    "True",
		"REPORT_DOWNLOAD_SCHEDULE":  "*/5 * * * *",
		"RETAIN_NUM_MONTHS":         "4",
		"RBAC_SERVICE_HOST":         "cost-management-rbac-api",
		"RBAC_SERVICE_PORT":         "8080",
		"RBAC_SERVICE_PATH":         "/api/rbac/v1/access/",
		"RBAC_SERVICE_PROTOCOL":     "http",
		"ENHANCED_ORG_ADMIN":        "False",
		"GUNICORN_LOG_LEVEL":        "INFO",
		"DJANGO_LOG_LEVEL":          "INFO",
		"DJANGO_LOG_FORMATTER":      "simple",
		"DJANGO_LOG_HANDLERS":       "console",
		"DEVELOPMENT":               "False",
		"KOKU_ENABLE_SENTRY":        "False",
		"CACHED_VIEWS_DISABLED":     "False",
		"NOTIFICATION_CHECK_TIME":   "24",
		"RBAC_CACHE_TIMEOUT":        "300",
		"CACHE_TIMEOUT":             "3600",
		"TAG_ENABLED_LIMIT":         "200",
		"USE_READREPLICA":           "False",
		"POLLING_TIMER":             "300",
		"INITIAL_INGEST_NUM_MONTHS": "2",
		"INITIAL_INGEST_OVERRIDE":   "False",
		"CELERY_RESULT_EXPIRES":     "28800",
	}

	for name, wantVal := range want {
		gotVal, ok := byName[name]
		if !ok {
			t.Errorf("missing shared env var %s", name)
			continue
		}
		if gotVal != wantVal {
			t.Errorf("%s: got %q, want %q", name, gotVal, wantVal)
		}
	}

	// Verify KOKU_LOG_LEVEL is NOT in shared defaults (set per-workload)
	if _, ok := byName["KOKU_LOG_LEVEL"]; ok {
		t.Error("KOKU_LOG_LEVEL should not be in KokuCommonEnv shared defaults; set per-workload")
	}
}

func TestWorkloadKokuLogLevelOverrides(t *testing.T) {
	cfg := &costv1alpha1.CostManagementServiceConfig{
		ObjectMeta: metav1.ObjectMeta{Name: "cost-management", Namespace: "cost-onprem"},
		Spec: costv1alpha1.CostManagementServiceConfigSpec{
			CostManagement: costv1alpha1.CostManagementConfig{
				API: costv1alpha1.KokuAPISpec{
					Image: costv1alpha1.ImageSpec{Repository: "quay.io/example/koku", Tag: "v1"},
				},
			},
		},
	}

	workloads := []struct {
		name             string
		containers       []corev1.Container
		wantKokuLogLevel string
	}{
		{"koku-api", KokuAPIDeployment(cfg).Spec.Template.Spec.Containers, "INFO"},
		{"masu", MasuDeployment(cfg).Spec.Template.Spec.Containers, "DEBUG"},
		{"listener", ListenerDeployment(cfg).Spec.Template.Spec.Containers, "INFO"},
		{"celery-beat", CeleryBeatDeployment(cfg).Spec.Template.Spec.Containers, "INFO"},
		{"celery-worker", CeleryWorkerDeployment(cfg, "celery", costv1alpha1.CeleryWorkerSpec{Replicas: 1}).Spec.Template.Spec.Containers, "INFO"},
		{"migration", MigrationJob(cfg, "latest").Spec.Template.Spec.Containers, "INFO"},
	}

	for _, wl := range workloads {
		byName := make(map[string]string)
		for _, c := range wl.containers {
			for _, e := range c.Env {
				if e.Value != "" {
					byName[e.Name] = e.Value
				}
			}
		}
		got, ok := byName["KOKU_LOG_LEVEL"]
		if !ok {
			t.Errorf("%s: missing KOKU_LOG_LEVEL", wl.name)
			continue
		}
		if got != wl.wantKokuLogLevel {
			t.Errorf("%s: KOKU_LOG_LEVEL = %q, want %q", wl.name, got, wl.wantKokuLogLevel)
		}
	}
}
