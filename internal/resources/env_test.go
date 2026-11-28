package resources

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

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

func TestKokuCommonEnvRequestedBucketFromUserProvidedObjectStorage(t *testing.T) {
	// When the customer configures object storage (secretName + buckets.koku),
	// Discovery mirrors buckets.koku into discoveredConfig.s3.bucket
	// (userProvidedS3), so REQUESTED_BUCKET — and the ingress upload bucket that
	// inherits it — must resolve to exactly the bucket the customer named.
	cfg := &costv1alpha1.CostManagementServiceConfig{
		ObjectMeta: metav1.ObjectMeta{Name: "cost-management", Namespace: "cost-onprem"},
		Spec: costv1alpha1.CostManagementServiceConfigSpec{
			ObjectStorage: costv1alpha1.ObjectStorageConfig{
				SecretName: "customer-s3-credentials",
				Buckets:    costv1alpha1.ObjectStorageBucketsSpec{Koku: "customer-koku-bucket"},
			},
		},
		Status: costv1alpha1.CostManagementServiceConfigStatus{
			DiscoveredConfig: &costv1alpha1.DiscoveredConfig{
				S3: &costv1alpha1.DiscoveredS3{Bucket: "customer-koku-bucket"},
			},
		},
	}

	env := KokuCommonEnv(cfg)
	if got, _ := envValue(env, "REQUESTED_BUCKET"); got != "customer-koku-bucket" {
		t.Errorf("REQUESTED_BUCKET = %q, want customer-koku-bucket", got)
	}
	if got := S3IngressBucket(cfg); got != "customer-koku-bucket" {
		t.Errorf("S3IngressBucket = %q, want customer-koku-bucket (inherits koku)", got)
	}
}

func TestKokuCommonEnvS3RegionHonorsOverride(t *testing.T) {
	cfg := &costv1alpha1.CostManagementServiceConfig{
		ObjectMeta: metav1.ObjectMeta{Name: "cost-management", Namespace: "cost-onprem"},
		Spec: costv1alpha1.CostManagementServiceConfigSpec{
			ObjectStorage: costv1alpha1.ObjectStorageConfig{
				S3: costv1alpha1.S3Options{Region: "eu-west-1"},
			},
		},
	}
	if got, _ := envValue(KokuCommonEnv(cfg), "S3_REGION"); got != "eu-west-1" {
		t.Errorf("S3_REGION = %q, want eu-west-1 (explicit override passes through)", got)
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
		{"celery-worker", CeleryWorkerDeployment(cfg, "celery", costv1alpha1.CeleryWorkerSpec{Replicas: ptr.To[int32](1)}).Spec.Template.Spec.Containers},
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

func TestSetEnvReplacesOrAppends(t *testing.T) {
	base := []corev1.EnvVar{
		EnvVal("KEEP", "yes"),
		EnvVal("REPLACE_ME", "old"),
	}
	got := SetEnv(base, EnvFromSecret("REPLACE_ME", "sec", "token"))
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2 (replace in place)", len(got))
	}
	if got[1].Value != "" || got[1].ValueFrom == nil || got[1].ValueFrom.SecretKeyRef == nil {
		t.Fatalf("REPLACE_ME not secretKeyRef: %#v", got[1])
	}
	got = SetEnv(got, EnvVal("NEW", "x"))
	if len(got) != 3 || got[2].Name != "NEW" {
		t.Fatalf("append failed: %#v", got)
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
		"S3_REGION":                 "us-east-1",
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
		{"celery-worker", CeleryWorkerDeployment(cfg, "celery", costv1alpha1.CeleryWorkerSpec{Replicas: ptr.To[int32](1)}).Spec.Template.Spec.Containers, "INFO"},
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

func TestKokuCommonEnvCostEventsWiring(t *testing.T) {
	baseCfg := func() *costv1alpha1.CostManagementServiceConfig {
		return &costv1alpha1.CostManagementServiceConfig{
			ObjectMeta: metav1.ObjectMeta{Name: "cost-management", Namespace: "cost-onprem"},
			Spec: costv1alpha1.CostManagementServiceConfigSpec{
				CostManagement: costv1alpha1.CostManagementConfig{
					API: costv1alpha1.KokuAPISpec{
						Image: costv1alpha1.ImageSpec{Repository: "quay.io/example/koku", Tag: "v1"},
					},
				},
			},
		}
	}

	// Disabled by default: neither COST_EVENTS_URL nor COST_EVENTS_AUTH_TOKEN present.
	cfgOff := baseCfg()
	if _, ok := envValue(KokuCommonEnv(cfgOff), "COST_EVENTS_URL"); ok {
		t.Error("COST_EVENTS_URL should be omitted when costEvents.enabled is false")
	}
	if _, ok := envValue(KokuCommonEnv(cfgOff), "COST_EVENTS_AUTH_TOKEN"); ok {
		t.Error("COST_EVENTS_AUTH_TOKEN should be omitted when costEvents.enabled is false")
	}

	// Enabled without authSecretRef: COST_EVENTS_URL set, no auth token.
	cfgNoAuth := baseCfg()
	cfgNoAuth.Spec.CostEvents.Enabled = new(true)
	envNoAuth := KokuCommonEnv(cfgNoAuth)
	if got, ok := envValue(envNoAuth, "COST_EVENTS_URL"); !ok || got != "http://cost-event-consumer.cost-onprem.svc.cluster.local:8020" {
		t.Errorf("COST_EVENTS_URL = %q (ok=%v), want default http URL", got, ok)
	}
	if _, ok := envValue(envNoAuth, "COST_EVENTS_AUTH_TOKEN"); ok {
		t.Error("COST_EVENTS_AUTH_TOKEN should be omitted when authSecretRef is nil")
	}

	// Enabled with TLS, custom host/port, and authSecretRef; literal CR env override must not win.
	cfg := baseCfg()
	cfg.Spec.CostEvents = costv1alpha1.CostEventsConfig{
		Enabled: new(true),
		Host:    "cost-event-consumer.cost-mgmt.svc.cluster.local",
		Port:    8443,
		TLS:     costv1alpha1.CostEventsTLSSpec{Enabled: true},
		AuthSecretRef: &costv1alpha1.SecretKeyRef{
			Name: "cost-events-pull-secret",
			Key:  "export-token",
		},
	}
	cfg.Spec.CostManagement.API.Env = map[string]string{"COST_EVENTS_AUTH_TOKEN": "literal-override"}
	cfg.Spec.CostManagement.Masu.Env = map[string]string{"COST_EVENTS_AUTH_TOKEN": "literal-override"}
	cfg.Spec.CostManagement.Listener.Env = map[string]string{"COST_EVENTS_AUTH_TOKEN": "literal-override"}

	const wantURL = "https://cost-event-consumer.cost-mgmt.svc.cluster.local:8443"
	workloads := []struct {
		name string
		env  []corev1.EnvVar
	}{
		{"koku-api", KokuAPIDeployment(cfg).Spec.Template.Spec.Containers[0].Env},
		{"masu", MasuDeployment(cfg).Spec.Template.Spec.Containers[0].Env},
		{"listener", ListenerDeployment(cfg).Spec.Template.Spec.Containers[0].Env},
		{"celery-worker", CeleryWorkerDeployment(cfg, "celery", costv1alpha1.CeleryWorkerSpec{Replicas: ptr.To[int32](1)}).Spec.Template.Spec.Containers[0].Env},
	}
	for _, wl := range workloads {
		if got, ok := envValue(wl.env, "COST_EVENTS_URL"); !ok || got != wantURL {
			t.Errorf("%s: COST_EVENTS_URL = %q (ok=%v), want %q", wl.name, got, ok, wantURL)
		}
		var tokCount int
		for _, e := range wl.env {
			if e.Name != "COST_EVENTS_AUTH_TOKEN" {
				continue
			}
			tokCount++
			if e.Value != "" {
				t.Errorf("%s: COST_EVENTS_AUTH_TOKEN must not be literal, got %q", wl.name, e.Value)
			}
			if e.ValueFrom == nil || e.ValueFrom.SecretKeyRef == nil ||
				e.ValueFrom.SecretKeyRef.Name != "cost-events-pull-secret" ||
				e.ValueFrom.SecretKeyRef.Key != "export-token" {
				t.Errorf("%s: COST_EVENTS_AUTH_TOKEN secretKeyRef = %#v, want cost-events-pull-secret/export-token", wl.name, e.ValueFrom)
			}
		}
		if tokCount != 1 {
			t.Errorf("%s: COST_EVENTS_AUTH_TOKEN count = %d, want 1", wl.name, tokCount)
		}
	}

	// Empty key in authSecretRef defaults to "token".
	cfgDefaultKey := baseCfg()
	cfgDefaultKey.Spec.CostEvents.Enabled = new(true)
	cfgDefaultKey.Spec.CostEvents.AuthSecretRef = &costv1alpha1.SecretKeyRef{Name: "my-secret"}
	for _, e := range MasuDeployment(cfgDefaultKey).Spec.Template.Spec.Containers[0].Env {
		if e.Name == "COST_EVENTS_AUTH_TOKEN" {
			if e.ValueFrom == nil || e.ValueFrom.SecretKeyRef == nil || e.ValueFrom.SecretKeyRef.Key != "token" {
				t.Errorf("default authSecretRef key = %#v, want token", e.ValueFrom)
			}
		}
	}
}
