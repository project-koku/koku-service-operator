package controller

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"io"
	"log"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	costv1alpha1 "github.com/project-koku/koku-service-operator/api/v1alpha1"
	"github.com/project-koku/koku-service-operator/internal/resources"
)

// Uses only local TLS and a fake Kubernetes client. Native-FIPS execution of
// these tests verifies the release crypto path without requiring envtest.
func newHostnameTLSServer(t *testing.T, handler http.Handler) (*httptest.Server, string, *corev1.Secret) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		DNSNames:     []string{"localhost"}, // Deliberately excludes the listener IP.
		NotBefore:    time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true,
		KeyUsage:    x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewUnstartedServer(handler)
	srv.Config.ErrorLog = log.New(io.Discard, "", 0) // Expected TLS rejections.
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	_, port, err := net.SplitHostPort(srv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	trustedURL := "https://" + net.JoinHostPort("localhost", port)
	ca := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "integration-ca", Namespace: testNamespace},
		Data:       map[string][]byte{caCertKey: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})},
	}
	return srv, trustedURL, ca
}

func assertIntegrationCondition(t *testing.T, cfg *costv1alpha1.CostManagementServiceConfig, conditionType string, status metav1.ConditionStatus, reason string) {
	t.Helper()
	condition := findCondition(cfg.Status.Conditions, conditionType)
	if condition == nil || condition.Status != status || condition.Reason != reason {
		t.Fatalf("%s = %+v, want %s %s", conditionType, condition, status, reason)
	}
}

func TestGeneratedSecretsPreservedAcrossReconcilerRestart(t *testing.T) {
	ctx := context.Background()
	cfg := &costv1alpha1.CostManagementServiceConfig{ObjectMeta: metav1.ObjectMeta{
		Name: testCRName, Namespace: testNamespace, UID: types.UID("credential-owner"),
	}}
	for _, tc := range []struct {
		name  string
		build func(*costv1alpha1.CostManagementServiceConfig) *corev1.Secret
	}{
		{"database", resources.DBCredentialsSecret},
		{"Django", resources.DjangoSecret},
		{"UI cookie", resources.UICookieSecret},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reconciler := newValidationReconciler(t)
			initial := tc.build(cfg)
			// The fake client does not implement API-server StringData conversion.
			initial.Data = make(map[string][]byte, len(initial.StringData))
			for key, value := range initial.StringData {
				initial.Data[key] = []byte(value)
			}
			initial.StringData = nil
			if err := reconciler.ensureSecret(ctx, cfg, initial); err != nil {
				t.Fatal(err)
			}
			stored := &corev1.Secret{}
			objectKey := types.NamespacedName{Namespace: initial.Namespace, Name: initial.Name}
			if err := reconciler.Get(ctx, objectKey, stored); err != nil {
				t.Fatal(err)
			}
			baseline := stored.DeepCopy()
			for attempt := range 3 {
				if attempt == 1 {
					// Reconstruct all controller state while retaining persisted API data,
					// as on a restart or image upgrade.
					reconciler = &CostManagementServiceConfigReconciler{Client: reconciler.Client}
				}
				if err := reconciler.ensureSecret(ctx, cfg, tc.build(cfg)); err != nil {
					t.Fatal(err)
				}
				if err := reconciler.Get(ctx, objectKey, stored); err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(baseline, stored) {
					t.Fatal("create-only reconciliation changed the persisted credential Secret")
				}
			}
			// Explicit administrator rotation must also survive reconciliation.
			for key := range stored.Data {
				stored.Data[key] = []byte("administrator-replacement")
			}
			if err := reconciler.Update(ctx, stored); err != nil {
				t.Fatal(err)
			}
			baseline = stored.DeepCopy()
			if err := reconciler.ensureSecret(ctx, cfg, tc.build(cfg)); err != nil {
				t.Fatal(err)
			}
			if err := reconciler.Get(ctx, objectKey, stored); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(baseline, stored) {
				t.Fatal("reconciliation overwrote administrator-supplied credentials")
			}
		})
	}
}

func TestS3TLSRejectionAndRecovery(t *testing.T) {
	var deny atomic.Bool
	handler := fakeS3Buckets(http.StatusOK, map[string]int{"/koku-bucket": http.StatusOK})
	srv, _, ca := newHostnameTLSServer(t, http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if deny.Load() {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		handler(w, req)
	}))
	pool, err := certPoolFromPEM(ca.Data[caCertKey])
	if err != nil {
		t.Fatal(err)
	}
	// An IP endpoint with a trusted CA must still fail hostname verification;
	// transport failures must not be misclassified as bucket access failures.
	err = s3BucketContractProbe(context.Background(), srv.URL, defaultS3Region, s3TestAccessKey, s3TestSecretKey, 10*time.Second, false, pool, []string{"koku-bucket"})
	var hostnameErr x509.HostnameError
	if !errors.As(err, &hostnameErr) {
		t.Fatalf("expected hostname verification error, got %v", err)
	}
	if s3EndpointResponded(err) {
		t.Fatal("TLS rejection was classified as an HTTP response")
	}

	cfg := &costv1alpha1.CostManagementServiceConfig{
		ObjectMeta: metav1.ObjectMeta{Name: testCRName, Namespace: testNamespace}, Spec: bundledNoKafkaSpec(),
	}
	cfg.Spec.ObjectStorage = objectStorageForServer(t, srv, s3TestSecret)
	cfg.Spec.ObjectStorage.Buckets.Koku = "koku-bucket"
	r := newValidationReconciler(t, s3CredsSecret(s3TestSecret), ca)
	for _, step := range []struct {
		name, caName, host string
		deny               bool
		status             metav1.ConditionStatus
		reason, message    string
	}{
		{"untrusted CA", "", "localhost", false, metav1.ConditionFalse, "StorageUnreachable", ""},
		{"trusted CA wrong hostname", ca.Name, "127.0.0.1", false, metav1.ConditionFalse, "StorageUnreachable", ""},
		{"trusted recovery", ca.Name, "localhost", false, metav1.ConditionTrue, "StorageBucketsAccessible", ""},
		{"authorization rejection", ca.Name, "localhost", true, metav1.ConditionFalse, "StorageBucketInaccessible", "403"},
		{"authorization recovery", ca.Name, "localhost", false, metav1.ConditionTrue, "StorageBucketsAccessible", ""},
	} {
		t.Run(step.name, func(t *testing.T) {
			cfg.Spec.ObjectStorage.Endpoint = step.host
			cfg.Spec.ObjectStorage.CACertSecretName = step.caName
			deny.Store(step.deny)
			result, err := r.reconcileValidation(context.Background(), cfg)
			if err != nil || !result.IsZero() {
				t.Fatalf("nonblocking S3 validation: result=%+v err=%v", result, err)
			}
			assertIntegrationCondition(t, cfg, costv1alpha1.ConditionStorageReady, step.status, step.reason)
			condition := findCondition(cfg.Status.Conditions, costv1alpha1.ConditionStorageReady)
			if !strings.Contains(condition.Message, step.message) {
				t.Fatalf("condition does not describe %s", step.name)
			}
		})
	}
}

func TestOIDCTLSRejectionAndRecovery(t *testing.T) {
	srv, trustedURL, ca := newHostnameTLSServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"keys":[{"kty":"RSA","kid":"test"}]}`)
	}))
	pool, err := certPoolFromPEM(ca.Data[caCertKey])
	if err != nil {
		t.Fatal(err)
	}
	var hostnameErr x509.HostnameError
	if err := jwksProbe(context.Background(), srv.URL, false, pool, time.Second); !errors.As(err, &hostnameErr) {
		t.Fatalf("expected hostname verification error, got %v", err)
	}
	cfg := &costv1alpha1.CostManagementServiceConfig{
		ObjectMeta: metav1.ObjectMeta{Name: testCRName, Namespace: testNamespace}, Spec: bundledNoKafkaSpec(),
	}
	r := newValidationReconciler(t, ca)
	for _, step := range []struct {
		name, endpoint, caName string
		status                 metav1.ConditionStatus
		reason, message        string
	}{
		{"untrusted CA", trustedURL, "", metav1.ConditionFalse, "OIDCUnreachable", "certificate"},
		{"trusted CA wrong hostname", srv.URL, ca.Name, metav1.ConditionFalse, "OIDCUnreachable", "certificate"},
		{"trusted recovery", trustedURL, ca.Name, metav1.ConditionTrue, "OIDCReachable", ""},
	} {
		t.Run(step.name, func(t *testing.T) {
			cfg.Spec.Auth.Keycloak.URL = step.endpoint
			cfg.Spec.Auth.Keycloak.TLS.CACertSecretName = step.caName
			result, err := r.reconcileValidation(context.Background(), cfg)
			if err != nil || !result.IsZero() {
				t.Fatalf("nonblocking OIDC validation: result=%+v err=%v", result, err)
			}
			assertIntegrationCondition(t, cfg, costv1alpha1.ConditionAuthReady, step.status, step.reason)
			condition := findCondition(cfg.Status.Conditions, costv1alpha1.ConditionAuthReady)
			if !strings.Contains(condition.Message, step.message) {
				t.Fatalf("condition does not describe %s", step.name)
			}
		})
	}
}
