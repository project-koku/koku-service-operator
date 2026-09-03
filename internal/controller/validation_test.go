package controller

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	costv1alpha1 "github.com/project-koku/koku-service-operator/api/v1alpha1"
)

// -----------------------------------------------------------------------------
// Low-level probe helpers
// -----------------------------------------------------------------------------

func TestTCPProbe(t *testing.T) {
	t.Run("reachable", func(t *testing.T) {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer ln.Close()
		if err := tcpProbe(ln.Addr().String(), time.Second); err != nil {
			t.Fatalf("expected success, got %v", err)
		}
	})

	t.Run("unreachable", func(t *testing.T) {
		if err := tcpProbe(localHost+":1", 300*time.Millisecond); err == nil {
			t.Fatal("expected error for unreachable addr")
		}
	})
}

func TestKafkaTCPProbe(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	good := ln.Addr().String()

	tests := []struct {
		name    string
		servers string
		wantErr bool
	}{
		{"single reachable", good, false},
		{"first bad second good", localHost + ":1," + good, false},
		{"all unreachable", localHost + ":1," + localHost + ":2", true},
		{"empty string", "", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := kafkaTCPProbe(tc.servers, 300*time.Millisecond)
			if (err != nil) != tc.wantErr {
				t.Errorf("kafkaTCPProbe(%q): got err=%v, wantErr=%v", tc.servers, err, tc.wantErr)
			}
		})
	}
}

func TestJWKSProbe(t *testing.T) {
	ctx := context.Background()
	validJWKS := `{"keys":[{"kty":"RSA","kid":"1"}]}`

	tests := []struct {
		name    string
		status  int
		body    string
		wantErr bool
	}{
		{"200 valid JWKS", http.StatusOK, validJWKS, false},
		{"200 invalid JSON", http.StatusOK, "not-json", true},
		{"200 empty keys", http.StatusOK, `{"keys":[]}`, true},
		{"200 missing keys", http.StatusOK, `{}`, true},
		{"401 Unauthorized", http.StatusUnauthorized, "", true},
		{"403 Forbidden", http.StatusForbidden, "", true},
		{"404 Not Found", http.StatusNotFound, "", true},
		{"503 Service Unavailable", http.StatusServiceUnavailable, "", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			t.Cleanup(srv.Close)
			err := jwksProbe(ctx, srv.URL, false, nil, time.Second)
			if (err != nil) != tc.wantErr {
				t.Errorf("jwksProbe status=%d body=%q: err=%v, wantErr=%v", tc.status, tc.body, err, tc.wantErr)
			}
		})
	}

	t.Run("TLS custom CA", func(t *testing.T) {
		srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(validJWKS))
		}))
		t.Cleanup(srv.Close)
		pool := x509.NewCertPool()
		pool.AddCert(srv.Certificate())
		if err := jwksProbe(ctx, srv.URL, false, pool, time.Second); err != nil {
			t.Fatalf("expected success with custom CA, got %v", err)
		}
	})

	t.Run("TLS verification failure without custom CA", func(t *testing.T) {
		srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(validJWKS))
		}))
		t.Cleanup(srv.Close)
		if err := jwksProbe(ctx, srv.URL, false, nil, time.Second); err == nil {
			t.Fatal("expected TLS verification error")
		}
	})

	t.Run("unreachable", func(t *testing.T) {
		if err := jwksProbe(ctx, "http://"+localHost+":1/jwks", false, nil, 200*time.Millisecond); err == nil {
			t.Fatal("expected error for unreachable server")
		}
	})

	t.Run("parent context cancelled", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(validJWKS))
		}))
		t.Cleanup(srv.Close)
		cancelled, cancel := context.WithCancel(context.Background())
		cancel()
		if err := jwksProbe(cancelled, srv.URL, false, nil, time.Second); err == nil {
			t.Fatal("expected error when parent context is cancelled")
		}
	})
}

// -----------------------------------------------------------------------------
// getSecret
// -----------------------------------------------------------------------------

func TestCheckSecretKeys(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: validationTestSecret, Namespace: testNamespace},
		Data: map[string][]byte{
			secretKeyUsername: []byte("admin"),
			secretKeyPassword: []byte("secret"),
		},
	}
	r := &CostManagementServiceConfigReconciler{
		Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(secret).Build(),
	}

	tests := []struct {
		name     string
		secret   string
		required []string
		wantErr  bool
	}{
		{"all keys present", validationTestSecret, []string{secretKeyUsername, secretKeyPassword}, false},
		{"missing key", validationTestSecret, []string{secretKeyUsername, secretKeyPassword, "token"}, true},
		{"secret not found", "no-such-secret", []string{secretKeyUsername}, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := r.getSecret(context.Background(), testNamespace, tc.secret, tc.required)
			if (err != nil) != tc.wantErr {
				t.Errorf("getSecret(%q): err=%v, wantErr=%v", tc.secret, err, tc.wantErr)
			}
			if !tc.wantErr && got == nil {
				t.Error("getSecret should return the Secret on success")
			}
			if !tc.wantErr && got != nil && got.Name != tc.secret {
				t.Errorf("returned Secret.Name = %q, want %q", got.Name, tc.secret)
			}
			if tc.wantErr && got != nil {
				t.Error("getSecret should return nil Secret on error")
			}
		})
	}
}

// -----------------------------------------------------------------------------
// reconcileValidation — condition-setting behaviour
// -----------------------------------------------------------------------------

const (
	validationTestSecret = "my-secret"
	secretKeyUsername    = "username"
	secretKeyPassword    = "password"
	localHost            = "127.0.0.1"
	testDBSecret         = "db-creds"
)

func newValidationReconciler(t *testing.T, objs ...client.Object) *CostManagementServiceConfigReconciler {
	t.Helper()
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = costv1alpha1.AddToScheme(scheme)
	return &CostManagementServiceConfigReconciler{
		Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build(),
	}
}

func truePtr() *bool  { v := true; return &v }
func falsePtr() *bool { v := false; return &v }

func listenLocalTCP(t *testing.T) *net.TCPListener {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	return ln.(*net.TCPListener)
}

func TestReconcileValidation_BundledInfra(t *testing.T) {
	// With bundled DB+Cache and no Kafka config, validation is a no-op and the
	// pipeline continues.
	cfg := &costv1alpha1.CostManagementServiceConfig{
		ObjectMeta: metav1.ObjectMeta{Name: testCRName, Namespace: testNamespace},
		Spec: costv1alpha1.CostManagementServiceConfigSpec{
			Database: costv1alpha1.DatabaseConfig{Deploy: truePtr()},
			Cache:    costv1alpha1.CacheConfig{Deploy: truePtr()},
			Kafka:    costv1alpha1.KafkaConfig{BootstrapServers: ""},
		},
	}

	r := newValidationReconciler(t)
	result, err := r.reconcileValidation(context.Background(), cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.IsZero() {
		t.Errorf("expected zero result (no external deps to probe), got %+v", result)
	}
}

func TestReconcileValidation_ExternalDBReachable(t *testing.T) {
	ln := listenLocalTCP(t)
	addr := ln.Addr().(*net.TCPAddr)

	cfg := &costv1alpha1.CostManagementServiceConfig{
		ObjectMeta: metav1.ObjectMeta{Name: testCRName, Namespace: testNamespace},
		Spec: costv1alpha1.CostManagementServiceConfigSpec{
			Database: costv1alpha1.DatabaseConfig{
				Deploy: falsePtr(),
				Host:   localHost,
				Port:   int32(addr.Port),
			},
			Cache: costv1alpha1.CacheConfig{Deploy: truePtr()},
			Kafka: costv1alpha1.KafkaConfig{BootstrapServers: ""},
		},
	}

	r := newValidationReconciler(t)
	result, err := r.reconcileValidation(context.Background(), cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.IsZero() {
		t.Errorf("expected zero result (DB reachable), got %+v", result)
	}
	dbCond := findCondition(cfg.Status.Conditions, costv1alpha1.ConditionDatabaseReady)
	if dbCond == nil || dbCond.Status != metav1.ConditionTrue {
		t.Errorf("expected DatabaseReady=True, got %+v", dbCond)
	}
}

func TestReconcileValidation_ExternalDBUnreachable(t *testing.T) {
	cfg := &costv1alpha1.CostManagementServiceConfig{
		ObjectMeta: metav1.ObjectMeta{Name: testCRName, Namespace: testNamespace},
		Spec: costv1alpha1.CostManagementServiceConfigSpec{
			Database: costv1alpha1.DatabaseConfig{
				Deploy: falsePtr(),
				Host:   localHost,
				Port:   1, // reserved; connection must fail
			},
			Cache: costv1alpha1.CacheConfig{Deploy: truePtr()},
			Kafka: costv1alpha1.KafkaConfig{BootstrapServers: ""},
		},
	}

	r := newValidationReconciler(t)
	result, err := r.reconcileValidation(context.Background(), cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.RequeueAfter == 0 {
		t.Error("expected non-zero requeue (unreachable external DB blocks pipeline)")
	}
	dbCond := findCondition(cfg.Status.Conditions, costv1alpha1.ConditionDatabaseReady)
	if dbCond == nil || dbCond.Status != metav1.ConditionFalse {
		t.Errorf("expected DatabaseReady=False, got %+v", dbCond)
	}
	if dbCond != nil && dbCond.Reason != "DatabaseUnreachable" {
		t.Errorf("unexpected reason %q", dbCond.Reason)
	}
	avail := findCondition(cfg.Status.Conditions, costv1alpha1.ConditionAvailable)
	if avail == nil || avail.Status != metav1.ConditionFalse || avail.Reason != "DependencyNotReady" {
		t.Errorf("expected Available=False reason=DependencyNotReady, got %+v", avail)
	}
	deg := findCondition(cfg.Status.Conditions, costv1alpha1.ConditionDegraded)
	if deg == nil || deg.Status != metav1.ConditionTrue || deg.Reason != "DependencyUnreachable" {
		t.Errorf("expected Degraded=True reason=DependencyUnreachable, got %+v", deg)
	}
	if cfg.Status.Phase != costv1alpha1.PhaseDegraded {
		t.Errorf("expected phase %q, got %q", costv1alpha1.PhaseDegraded, cfg.Status.Phase)
	}
}

func TestReconcileValidation_ExternalCacheUnreachable_SetsAvailableDegraded(t *testing.T) {
	cfg := &costv1alpha1.CostManagementServiceConfig{
		ObjectMeta: metav1.ObjectMeta{Name: testCRName, Namespace: testNamespace},
		Spec: costv1alpha1.CostManagementServiceConfigSpec{
			Database: costv1alpha1.DatabaseConfig{Deploy: truePtr()},
			Cache: costv1alpha1.CacheConfig{
				Deploy: falsePtr(),
				Host:   localHost,
				Port:   1,
			},
			Kafka: costv1alpha1.KafkaConfig{BootstrapServers: ""},
		},
	}
	// Stale Ready-era conditions that must be cleared on blocking failure.
	cfg.Status.Conditions = []metav1.Condition{
		{Type: costv1alpha1.ConditionAvailable, Status: metav1.ConditionTrue, Reason: "AllComponentsReady"},
		{Type: costv1alpha1.ConditionDegraded, Status: metav1.ConditionFalse, Reason: "ReconcileComplete"},
	}
	cfg.Status.Phase = costv1alpha1.PhaseReady

	r := newValidationReconciler(t)
	result, err := r.reconcileValidation(context.Background(), cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.RequeueAfter == 0 {
		t.Error("expected non-zero requeue (unreachable external cache blocks pipeline)")
	}
	cacheCond := findCondition(cfg.Status.Conditions, costv1alpha1.ConditionCacheReady)
	if cacheCond == nil || cacheCond.Status != metav1.ConditionFalse || cacheCond.Reason != "CacheUnreachable" {
		t.Errorf("expected CacheReady=False CacheUnreachable, got %+v", cacheCond)
	}
	avail := findCondition(cfg.Status.Conditions, costv1alpha1.ConditionAvailable)
	if avail == nil || avail.Status != metav1.ConditionFalse {
		t.Errorf("expected Available=False, got %+v", avail)
	}
	deg := findCondition(cfg.Status.Conditions, costv1alpha1.ConditionDegraded)
	if deg == nil || deg.Status != metav1.ConditionTrue {
		t.Errorf("expected Degraded=True, got %+v", deg)
	}
	if cfg.Status.Phase != costv1alpha1.PhaseDegraded {
		t.Errorf("expected phase %q, got %q", costv1alpha1.PhaseDegraded, cfg.Status.Phase)
	}
}

func TestReconcileValidation_KafkaReachable(t *testing.T) {
	ln := listenLocalTCP(t)

	cfg := &costv1alpha1.CostManagementServiceConfig{
		ObjectMeta: metav1.ObjectMeta{Name: testCRName, Namespace: testNamespace},
		Spec: costv1alpha1.CostManagementServiceConfigSpec{
			Database: costv1alpha1.DatabaseConfig{Deploy: truePtr()},
			Cache:    costv1alpha1.CacheConfig{Deploy: truePtr()},
			Kafka:    costv1alpha1.KafkaConfig{BootstrapServers: ln.Addr().String()},
		},
	}

	r := newValidationReconciler(t)
	result, err := r.reconcileValidation(context.Background(), cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.IsZero() {
		t.Errorf("expected zero result, got %+v", result)
	}
	kCond := findCondition(cfg.Status.Conditions, costv1alpha1.ConditionKafkaReady)
	if kCond == nil || kCond.Status != metav1.ConditionTrue {
		t.Errorf("expected KafkaReady=True, got %+v", kCond)
	}
}

func TestReconcileValidation_KafkaUnreachableNonBlocking(t *testing.T) {
	cfg := &costv1alpha1.CostManagementServiceConfig{
		ObjectMeta: metav1.ObjectMeta{Name: testCRName, Namespace: testNamespace},
		Spec: costv1alpha1.CostManagementServiceConfigSpec{
			Database: costv1alpha1.DatabaseConfig{Deploy: truePtr()},
			Cache:    costv1alpha1.CacheConfig{Deploy: truePtr()},
			Kafka:    costv1alpha1.KafkaConfig{BootstrapServers: localHost + ":1"},
		},
	}

	r := newValidationReconciler(t)
	result, err := r.reconcileValidation(context.Background(), cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Unreachable Kafka must NOT block the pipeline.
	if !result.IsZero() {
		t.Errorf("Kafka unreachable should not block pipeline, got result %+v", result)
	}
	kCond := findCondition(cfg.Status.Conditions, costv1alpha1.ConditionKafkaReady)
	if kCond == nil || kCond.Status != metav1.ConditionFalse {
		t.Errorf("expected KafkaReady=False, got %+v", kCond)
	}
}

func TestReconcileValidation_OIDCJWKS(t *testing.T) {
	tests := []struct {
		name   string
		body   string
		status metav1.ConditionStatus
		reason string
	}{
		{"valid JWKS", `{"keys":[{"kty":"RSA","kid":"1"}]}`, metav1.ConditionTrue, "OIDCReachable"},
		{"empty keys", `{"keys":[]}`, metav1.ConditionFalse, "OIDCUnreachable"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(tc.body))
			}))
			t.Cleanup(srv.Close)

			cfg := &costv1alpha1.CostManagementServiceConfig{
				ObjectMeta: metav1.ObjectMeta{Name: testCRName, Namespace: testNamespace},
				Spec:       bundledNoKafkaSpec(),
			}
			cfg.Spec.Auth.Keycloak.URL = srv.URL

			r := newValidationReconciler(t)
			result, err := r.reconcileValidation(context.Background(), cfg)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !result.IsZero() {
				t.Errorf("OIDC probe must not block the pipeline, got %+v", result)
			}
			found := findCondition(cfg.Status.Conditions, costv1alpha1.ConditionAuthReady)
			if found == nil || found.Status != tc.status {
				t.Fatalf("AuthenticationReady=%v, want %s", found, tc.status)
			}
			if found.Reason != tc.reason {
				t.Errorf("reason = %q, want %s", found.Reason, tc.reason)
			}
		})
	}
}

func TestReconcileValidation_OIDCEmptyURLSkipsProbe(t *testing.T) {
	cfg := &costv1alpha1.CostManagementServiceConfig{
		ObjectMeta: metav1.ObjectMeta{Name: testCRName, Namespace: testNamespace},
		Spec:       bundledNoKafkaSpec(),
	}

	r := newValidationReconciler(t)
	result, err := r.reconcileValidation(context.Background(), cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.IsZero() {
		t.Errorf("empty url must not block the pipeline, got %+v", result)
	}
	found := findCondition(cfg.Status.Conditions, costv1alpha1.ConditionAuthReady)
	if found != nil {
		t.Fatalf("empty url should skip OIDC probe (no AuthenticationReady), got %+v", found)
	}
}

func TestReconcileValidation_OIDCWithCustomCA(t *testing.T) {
	t.Run("custom CA", func(t *testing.T) {
		srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"keys":[{"kty":"RSA","kid":"1"}]}`))
		}))
		t.Cleanup(srv.Close)

		cfg := &costv1alpha1.CostManagementServiceConfig{
			ObjectMeta: metav1.ObjectMeta{Name: testCRName, Namespace: testNamespace},
			Spec:       bundledNoKafkaSpec(),
		}
		cfg.Spec.Auth.Keycloak.URL = srv.URL
		cfg.Spec.Auth.Keycloak.TLS.CACertSecretName = "keycloak-ca"
		caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})
		caSecret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "keycloak-ca", Namespace: testNamespace},
			Data:       map[string][]byte{caCertKey: caPEM},
		}

		r := newValidationReconciler(t, caSecret)
		if _, err := r.reconcileValidation(context.Background(), cfg); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		found := findCondition(cfg.Status.Conditions, costv1alpha1.ConditionAuthReady)
		if found == nil || found.Status != metav1.ConditionTrue || found.Reason != "OIDCReachable" {
			t.Fatalf("AuthenticationReady=%+v, want OIDCReachable", found)
		}
	})

	t.Run("missing CA Secret", func(t *testing.T) {
		cfg := &costv1alpha1.CostManagementServiceConfig{
			ObjectMeta: metav1.ObjectMeta{Name: testCRName, Namespace: testNamespace},
			Spec:       bundledNoKafkaSpec(),
		}
		cfg.Spec.Auth.Keycloak.URL = "https://keycloak.example.com"
		cfg.Spec.Auth.Keycloak.TLS.CACertSecretName = "missing-ca"

		r := newValidationReconciler(t)
		if _, err := r.reconcileValidation(context.Background(), cfg); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		found := findCondition(cfg.Status.Conditions, costv1alpha1.ConditionAuthReady)
		if found == nil || found.Status != metav1.ConditionFalse || found.Reason != "OIDCCACertInvalid" {
			t.Fatalf("AuthenticationReady=%+v, want OIDCCACertInvalid", found)
		}
	})

	t.Run("invalid CA data", func(t *testing.T) {
		cfg := &costv1alpha1.CostManagementServiceConfig{
			ObjectMeta: metav1.ObjectMeta{Name: testCRName, Namespace: testNamespace},
			Spec:       bundledNoKafkaSpec(),
		}
		cfg.Spec.Auth.Keycloak.URL = "https://keycloak.example.com"
		cfg.Spec.Auth.Keycloak.TLS.CACertSecretName = "invalid-ca"
		caSecret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "invalid-ca", Namespace: testNamespace},
			Data:       map[string][]byte{caCertKey: []byte("not-a-certificate")},
		}

		r := newValidationReconciler(t, caSecret)
		if _, err := r.reconcileValidation(context.Background(), cfg); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		found := findCondition(cfg.Status.Conditions, costv1alpha1.ConditionAuthReady)
		if found == nil || found.Status != metav1.ConditionFalse || found.Reason != "OIDCCACertInvalid" {
			t.Fatalf("AuthenticationReady=%+v, want OIDCCACertInvalid", found)
		}
	})

	t.Run("insecure skips CA Secret loading", func(t *testing.T) {
		srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"keys":[{"kty":"RSA","kid":"1"}]}`))
		}))
		t.Cleanup(srv.Close)

		cfg := &costv1alpha1.CostManagementServiceConfig{
			ObjectMeta: metav1.ObjectMeta{Name: testCRName, Namespace: testNamespace},
			Spec:       bundledNoKafkaSpec(),
		}
		cfg.Spec.Auth.Keycloak.URL = srv.URL
		cfg.Spec.Auth.Keycloak.TLS.CACertSecretName = "missing-ca"
		cfg.Spec.Auth.Keycloak.TLS.InsecureSkipVerify = true

		r := newValidationReconciler(t)
		if _, err := r.reconcileValidation(context.Background(), cfg); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		found := findCondition(cfg.Status.Conditions, costv1alpha1.ConditionAuthReady)
		if found == nil || found.Status != metav1.ConditionTrue || found.Reason != "OIDCReachable" {
			t.Fatalf("AuthenticationReady=%+v, want OIDCReachable", found)
		}
	})
}

func TestCertPoolFromPEM_AppendsToSystemPool(t *testing.T) {
	sys, err := x509.SystemCertPool()
	if err != nil || sys == nil || sys.Equal(x509.NewCertPool()) {
		t.Skip("system cert pool unavailable or empty")
	}

	srv := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	t.Cleanup(srv.Close)
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})

	pool, err := certPoolFromPEM(pemBytes)
	if err != nil {
		t.Fatalf("certPoolFromPEM: %v", err)
	}

	customOnly := x509.NewCertPool()
	if !customOnly.AppendCertsFromPEM(pemBytes) {
		t.Fatal("test CA PEM did not parse")
	}
	if pool.Equal(customOnly) {
		t.Fatal("cert pool replaced the system roots; custom CA should be appended")
	}
	if pool.Equal(sys) {
		t.Fatal("custom CA was not added to the system pool")
	}
}

func TestCertPoolFromPEM_RejectsInvalidPEM(t *testing.T) {
	if _, err := certPoolFromPEM([]byte("not-a-certificate")); err == nil {
		t.Fatal("expected error for invalid PEM")
	}
}

func TestReconcileValidation_DBSecretMissingKeys(t *testing.T) {
	ln := listenLocalTCP(t)
	addr := ln.Addr().(*net.TCPAddr)

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: testDBSecret, Namespace: testNamespace},
		Data: map[string][]byte{
			// Only one key present; koku-user, koku-password, etc. are missing.
			"postgres-user": []byte("admin"),
		},
	}

	cfg := &costv1alpha1.CostManagementServiceConfig{
		ObjectMeta: metav1.ObjectMeta{Name: testCRName, Namespace: testNamespace},
		Spec: costv1alpha1.CostManagementServiceConfigSpec{
			Database: costv1alpha1.DatabaseConfig{
				Deploy:     falsePtr(),
				Host:       "127.0.0.1",
				Port:       int32(addr.Port),
				SecretName: testDBSecret,
			},
			Cache: costv1alpha1.CacheConfig{Deploy: truePtr()},
			Kafka: costv1alpha1.KafkaConfig{BootstrapServers: ""},
		},
	}

	r := newValidationReconciler(t, secret)
	result, err := r.reconcileValidation(context.Background(), cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.RequeueAfter == 0 {
		t.Error("expected requeue (invalid secret blocks pipeline)")
	}
	dbCond := findCondition(cfg.Status.Conditions, costv1alpha1.ConditionDatabaseReady)
	if dbCond == nil || dbCond.Status != metav1.ConditionFalse {
		t.Errorf("expected DatabaseReady=False for invalid secret, got %+v", dbCond)
	}
	if dbCond != nil && dbCond.Reason != "DatabaseSecretInvalid" {
		t.Errorf("unexpected reason %q", dbCond.Reason)
	}
}

func findCondition(conditions []metav1.Condition, condType string) *metav1.Condition {
	for i := range conditions {
		if conditions[i].Type == condType {
			return &conditions[i]
		}
	}
	return nil
}

// TestDBSecretValidationRequiresKruizeCredentials verifies that the database
// Secret validation (getSecret) includes kruize-user and kruize-password.
// Kruize connects to the same PostgreSQL instance; missing its credentials
// causes Kruize pods to fail silently after migrations complete.
func TestDBSecretValidationRequiresKruizeCredentials(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)

	// Secret with all required keys EXCEPT kruize credentials.
	secretMissingKruize := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: testDBSecret, Namespace: testNamespace},
		Data: map[string][]byte{ //nolint:goconst // test data — key names must match real values
			"postgres-user": []byte("postgres"), "postgres-password": []byte("pgpass"),
			"koku-user": []byte("koku"), "koku-password": []byte("kokupass"),
			"ros-user": []byte("ros"), "ros-password": []byte("rospass"),
			"rbac-user": []byte("rbac"), "rbac-password": []byte("rbacpass"),
			// kruize-user and kruize-password intentionally absent
		},
	}
	r := &CostManagementServiceConfigReconciler{
		Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(secretMissingKruize).Build(),
	}

	// The required list in validation.go must include kruize credentials.
	requiredKeys := []string{
		"postgres-user", "postgres-password",
		"koku-user", "koku-password",
		"ros-user", "ros-password",
		"rbac-user", "rbac-password",
		"kruize-user", "kruize-password",
	}
	_, err := r.getSecret(context.Background(), testNamespace, testDBSecret, requiredKeys)
	if err == nil {
		t.Error("getSecret should fail when kruize-user/kruize-password are absent, got nil")
	}
}

// TestReconcileValidationChecksS3Secret verifies that reconcileValidation
// validates the S3 Secret when spec.objectStorage.secretName is explicitly set.
// Without this, a Secret with missing credentials silently passes validation —
// discovered only at runtime when koku/masu fails to write to S3.
func TestReconcileValidationChecksS3Secret(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = costv1alpha1.AddToScheme(scheme)

	// Secret exists but is missing secret-key.
	incomplete := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "s3-creds", Namespace: testNamespace},
		Data: map[string][]byte{
			"access-key": []byte("AKIAIOSFODNN7EXAMPLE"),
			// secret-key intentionally absent
		},
	}

	cfg := &costv1alpha1.CostManagementServiceConfig{
		ObjectMeta: metav1.ObjectMeta{Name: testCRName, Namespace: testNamespace},
		Spec: costv1alpha1.CostManagementServiceConfigSpec{
			// Bundled DB/Cache so those probes are skipped.
			Database: costv1alpha1.DatabaseConfig{Deploy: truePtr()},
			Cache:    costv1alpha1.CacheConfig{Deploy: truePtr()},
			// User-provided S3 secret with missing key.
			ObjectStorage: costv1alpha1.ObjectStorageConfig{
				SecretName: "s3-creds",
			},
		},
	}

	r := newValidationReconciler(t, incomplete)

	_, _ = r.reconcileValidation(context.Background(), cfg)

	// After the fix, StorageReady=False with StorageSecretInvalid reason must be set.
	found := findCondition(cfg.Status.Conditions, costv1alpha1.ConditionStorageReady)
	if found == nil {
		t.Fatal("StorageReady condition not set — reconcileValidation does not validate objectStorage.secretName")
		return
	}
	if found.Status != metav1.ConditionFalse {
		t.Errorf("StorageReady = %s, want False (secret-key is missing)", found.Status)
	}
	if found.Reason != "StorageSecretInvalid" {
		t.Errorf("StorageReady reason = %q, want StorageSecretInvalid", found.Reason)
	}
}
