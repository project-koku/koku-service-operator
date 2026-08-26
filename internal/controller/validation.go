package controller

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	costv1alpha1 "github.com/project-koku/koku-service-operator/api/v1alpha1"
	"github.com/project-koku/koku-service-operator/internal/resources"
)

const (
	validationTimeout = 5 * time.Second
	jwksBodyLimit     = 256 * 1024
)

// accessKeyKey would be less readable than the key name itself.
//
//nolint:goconst // Secret key names are intentionally literal — a constant like
var (
	// s3SecretKeys are the required keys in an objectStorage Secret.
	s3SecretKeys = []string{"access-key", "secret-key"}

	// dbSecretKeys lists the credential keys the operator expects in an
	// externally-provided database Secret (spec.database.secretName).
	dbSecretKeys = []string{
		"postgres-user", "postgres-password",
		"koku-user", "koku-password",
		"ros-user", "ros-password",
		"rbac-user", "rbac-password",
		"kruize-user", "kruize-password",
	}
)

// reconcileValidation probes all external dependencies and validates referenced
// Secrets before the migration gate. DB and Cache failures block the pipeline;
// Kafka, OIDC, and S3 set conditions without blocking (init containers inside
// pods handle late-starting infrastructure).
func (r *CostManagementServiceConfigReconciler) reconcileValidation(ctx context.Context, cfg *costv1alpha1.CostManagementServiceConfig) (Result, error) {
	allReady := true

	// --- External DB ---
	// Bundled DB is already gated in reconcileInfrastructure; probe only when external.
	if !costv1alpha1.BoolVal(cfg.Spec.Database.Deploy, false) {
		host := resources.DatabaseHost(cfg)
		port := cfg.Spec.Database.Port
		if port == 0 {
			port = 5432
		}
		addr := net.JoinHostPort(host, fmt.Sprintf("%d", port))
		if err := tcpProbe(addr, validationTimeout); err != nil {
			msg := fmt.Sprintf("TCP probe %s: %v", addr, err)
			r.emitDependencyFailed(cfg, costv1alpha1.ConditionDatabaseReady, msg)
			r.setCondition(cfg, costv1alpha1.ConditionDatabaseReady, metav1.ConditionFalse,
				"DatabaseUnreachable", msg)
			allReady = false
		} else {
			// Validate secret keys when the user provided their own secret.
			if cfg.Spec.Database.SecretName != "" {
				required := dbSecretKeys
				if _, err := r.getSecret(ctx, cfg.Namespace, cfg.Spec.Database.SecretName, required); err != nil {
					r.setCondition(cfg, costv1alpha1.ConditionDatabaseReady, metav1.ConditionFalse,
						"DatabaseSecretInvalid", err.Error())
					allReady = false
				} else {
					r.setCondition(cfg, costv1alpha1.ConditionDatabaseReady, metav1.ConditionTrue,
						"DatabaseReachable", addr)
				}
			} else {
				r.setCondition(cfg, costv1alpha1.ConditionDatabaseReady, metav1.ConditionTrue,
					"DatabaseReachable", addr)
			}
		}
	}

	// --- External Cache ---
	if !costv1alpha1.BoolVal(cfg.Spec.Cache.Deploy, false) {
		host := resources.CacheHost(cfg)
		port := cfg.Spec.Cache.Port
		if port == 0 {
			port = 6379
		}
		addr := net.JoinHostPort(host, fmt.Sprintf("%d", port))
		if err := tcpProbe(addr, validationTimeout); err != nil {
			msg := fmt.Sprintf("TCP probe %s: %v", addr, err)
			r.emitDependencyFailed(cfg, costv1alpha1.ConditionCacheReady, msg)
			r.setCondition(cfg, costv1alpha1.ConditionCacheReady, metav1.ConditionFalse,
				"CacheUnreachable", msg)
			allReady = false
		} else {
			if cfg.Spec.Cache.Auth.SecretName != "" {
				if _, err := r.getSecret(ctx, cfg.Namespace, cfg.Spec.Cache.Auth.SecretName, []string{"redis-password"}); err != nil {
					r.setCondition(cfg, costv1alpha1.ConditionCacheReady, metav1.ConditionFalse,
						"CacheSecretInvalid", err.Error())
					allReady = false
				} else {
					r.setCondition(cfg, costv1alpha1.ConditionCacheReady, metav1.ConditionTrue,
						"CacheReachable", addr)
				}
			} else {
				r.setCondition(cfg, costv1alpha1.ConditionCacheReady, metav1.ConditionTrue,
					"CacheReachable", addr)
			}
		}
	}

	// --- Kafka (always external; non-blocking) ---
	r.validateKafka(ctx, cfg)

	// --- S3 / ObjectStorage (non-blocking) ---
	// G2: Secret exists with access-key / secret-key.
	// G1: Signed ListBuckets against the resolved endpoint (user or discovered).
	r.validateObjectStorage(ctx, cfg)

	// --- OIDC / Keycloak (non-blocking; skipped when URL not explicitly set) ---
	r.validateOIDC(ctx, cfg)

	if !allReady {
		// Blocking DB/Cache failures must not leave stale Available=True /
		// Degraded=False from a prior Ready pass (COST-8107).
		msg := blockingDependencyMessage(cfg)
		r.setCondition(cfg, costv1alpha1.ConditionAvailable, metav1.ConditionFalse,
			"DependencyNotReady", msg)
		r.setCondition(cfg, costv1alpha1.ConditionDegraded, metav1.ConditionTrue,
			"DependencyUnreachable", msg)
		cfg.Status.Phase = costv1alpha1.PhaseDegraded
		return Result{RequeueAfter: requeueSlow}, nil
	}
	return Result{}, nil
}

// validateOIDC probes the configured Keycloak JWKS endpoint. It is
// non-blocking for the reconciliation pipeline, but records authentication
// readiness and invalid custom CA configuration in status.
func (r *CostManagementServiceConfigReconciler) validateOIDC(ctx context.Context, cfg *costv1alpha1.CostManagementServiceConfig) {
	if strings.TrimSpace(cfg.Spec.Auth.Keycloak.URL) == "" {
		r.setCondition(cfg, costv1alpha1.ConditionAuthReady, metav1.ConditionFalse,
			"OIDCConfigMissing", "spec.auth.keycloak.url is required to validate the external identity provider")
		return
	}

	jwksURL := resources.KeycloakJWKSURL(cfg)
	insecure := cfg.Spec.Auth.Keycloak.TLS.InsecureSkipVerify
	caCertPool, err := r.keycloakCACertPool(ctx, cfg)
	if err != nil {
		r.setCondition(cfg, costv1alpha1.ConditionAuthReady, metav1.ConditionFalse,
			"OIDCCACertInvalid", fmt.Errorf("load Keycloak CA Secret: %w", err).Error())
		return
	}
	if err := jwksProbe(ctx, jwksURL, insecure, caCertPool, validationTimeout); err != nil {
		r.setCondition(cfg, costv1alpha1.ConditionAuthReady, metav1.ConditionFalse,
			"OIDCUnreachable", fmt.Sprintf("JWKS %s: %v", jwksURL, err))
		return
	}
	r.setCondition(cfg, costv1alpha1.ConditionAuthReady, metav1.ConditionTrue,
		"OIDCReachable", jwksURL)
}

// keycloakCACertPool loads the configured Keycloak CA unless TLS verification
// was explicitly disabled. A nil pool preserves the system CA behavior.
func (r *CostManagementServiceConfigReconciler) keycloakCACertPool(ctx context.Context, cfg *costv1alpha1.CostManagementServiceConfig) (*x509.CertPool, error) {
	if cfg.Spec.Auth.Keycloak.TLS.InsecureSkipVerify {
		return nil, nil
	}
	caName := cfg.Spec.Auth.Keycloak.TLS.CACertSecretName
	if caName == "" {
		return nil, nil
	}
	caSecret, err := r.getSecret(ctx, cfg.Namespace, caName, []string{caCertKey})
	if err != nil {
		return nil, err
	}
	pool, err := certPoolFromPEM(caSecret.Data[caCertKey])
	if err != nil {
		return nil, fmt.Errorf("secret %q key %q contains no valid PEM certificates", caName, caCertKey)
	}
	return pool, nil
}

// certPoolFromPEM starts from the system CA pool and appends the given PEM
// certificates so a custom CA does not drop public roots.
func certPoolFromPEM(pem []byte) (*x509.CertPool, error) {
	pool, err := x509.SystemCertPool()
	if err != nil || pool == nil {
		pool = x509.NewCertPool()
	}
	if !pool.AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("PEM data contains no valid certificates")
	}
	return pool, nil
}

// blockingDependencyMessage summarizes why DB/Cache validation blocked reconcile.
func blockingDependencyMessage(cfg *costv1alpha1.CostManagementServiceConfig) string {
	var parts []string
	for _, typ := range []string{costv1alpha1.ConditionDatabaseReady, costv1alpha1.ConditionCacheReady} {
		c := apimeta.FindStatusCondition(cfg.Status.Conditions, typ)
		if c != nil && c.Status == metav1.ConditionFalse {
			parts = append(parts, fmt.Sprintf("%s: %s", typ, c.Message))
		}
	}
	if len(parts) == 0 {
		return "blocking dependency validation failed"
	}
	return strings.Join(parts, "; ")
}

// tcpProbe opens and immediately closes a TCP connection to addr.
func tcpProbe(addr string, timeout time.Duration) error {
	conn, err := net.DialTimeout("tcp", addr, timeout)
	if err != nil {
		return err
	}
	_ = conn.Close()
	return nil
}

// kafkaTCPProbe probes each broker in a comma-separated bootstrap-servers list
// and returns nil as soon as one is reachable.
func kafkaTCPProbe(bootstrapServers string, timeout time.Duration) error {
	var last error
	for b := range strings.SplitSeq(bootstrapServers, ",") {
		b = strings.TrimSpace(b)
		if b == "" {
			continue
		}
		if err := tcpProbe(b, timeout); err != nil {
			last = err
			continue
		}
		return nil // first reachable broker
	}
	if last != nil {
		return fmt.Errorf("no reachable Kafka broker in %q: %w", bootstrapServers, last)
	}
	return fmt.Errorf("bootstrap-servers %q is empty", bootstrapServers)
}

// jwksProbe GETs the OIDC JWKS URL and requires HTTP 2xx plus a JSON body
// with a non-empty keys array (what Envoy needs to validate JWTs).
// 4xx is an error: 401/403 means the endpoint is misconfigured; 404 means
// the JWKS URL or realm is wrong. Uses the reconcile context so shutdown
// cancels an in-flight probe. When caCertPool is non-nil it is installed as
// RootCAs (callers should include system roots plus any custom CA).
func jwksProbe(ctx context.Context, rawURL string, insecureSkipVerify bool, caCertPool *x509.CertPool, timeout time.Duration) error {
	base, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		base = &http.Transport{}
	}
	transport := base.Clone()
	if insecureSkipVerify {
		if transport.TLSClientConfig == nil {
			transport.TLSClientConfig = &tls.Config{}
		}
		transport.TLSClientConfig.InsecureSkipVerify = true //nolint:gosec // user-opted-in
	}
	if caCertPool != nil {
		if transport.TLSClientConfig == nil {
			transport.TLSClientConfig = &tls.Config{}
		}
		transport.TLSClientConfig.RootCAs = caCertPool
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return err
	}
	client := &http.Client{Transport: transport}
	resp, err := client.Do(req)
	if err != nil {
		transport.CloseIdleConnections()
		return err
	}
	defer func() {
		_ = resp.Body.Close()
		transport.CloseIdleConnections()
	}()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("unexpected HTTP status %d (want 2xx)", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, jwksBodyLimit+1))
	if err != nil {
		return fmt.Errorf("read JWKS body: %w", err)
	}
	if len(body) > jwksBodyLimit {
		return fmt.Errorf("JWKS response exceeds %d bytes", jwksBodyLimit)
	}
	var doc struct {
		Keys []json.RawMessage `json:"keys"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		return fmt.Errorf("JWKS is not valid JSON: %w", err)
	}
	if len(doc.Keys) == 0 {
		return fmt.Errorf("JWKS keys array is empty")
	}
	return nil
}

// getSecret verifies that a named Secret exists and contains all required keys
// with non-empty values. Returns the fetched Secret on success so callers can
// read credential data without a second API server round-trip.
func (r *CostManagementServiceConfigReconciler) getSecret(ctx context.Context, namespace, name string, required []string) (*corev1.Secret, error) {
	secret := &corev1.Secret{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, secret); err != nil {
		if errors.IsNotFound(err) {
			return nil, fmt.Errorf("secret %q not found", name)
		}
		return nil, fmt.Errorf("get secret %q: %w", name, err)
	}
	var missing []string
	for _, key := range required {
		if len(secret.Data[key]) == 0 {
			missing = append(missing, key)
		}
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("secret %q missing keys: %s", name, strings.Join(missing, ", "))
	}
	return secret, nil
}
