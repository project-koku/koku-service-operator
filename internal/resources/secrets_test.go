package resources

import (
	"encoding/base64"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	costv1alpha1 "github.com/project-koku/koku-service-operator/api/v1alpha1"
)

// These assertions verify consumer formats, not randomness or FIPS activation.
// The release-toolchain and live-cluster checks establish the crypto backend.
func TestGeneratedCredentialFormats(t *testing.T) {
	cfg := &costv1alpha1.CostManagementServiceConfig{
		ObjectMeta: metav1.ObjectMeta{Name: "cost-management", Namespace: "cost-onprem"},
	}
	const passwordAlphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789_-"
	checkFormat := func(t *testing.T, value, alphabet string, length int) {
		t.Helper()
		if len(value) != length {
			t.Fatalf("credential length = %d, want %d", len(value), length)
		}
		for _, char := range value {
			if !strings.ContainsRune(alphabet, char) {
				t.Fatal("credential contains a character outside the consumer alphabet")
			}
		}
	}
	db := DBCredentialsSecret(cfg)
	for _, component := range []string{"postgres", "koku", "ros", "kruize", "rbac"} {
		t.Run(component+" password", func(t *testing.T) {
			checkFormat(t, db.StringData[component+"-password"], passwordAlphabet, 32)
		})
	}
	t.Run("Django signing key", func(t *testing.T) {
		checkFormat(t, DjangoSecret(cfg).StringData["secret-key"], djangoKeyCharset, 50)
	})
	t.Run("oauth2-proxy cookie key", func(t *testing.T) {
		value := UICookieSecret(cfg).StringData["session-secret"]
		checkFormat(t, value, passwordAlphabet, 32)
		// oauth2-proxy decodes valid URL-base64 cookie secrets before choosing
		// the AES key size; 32 alphabet characters decode to 24 bytes (AES-192).
		decoded, err := base64.RawURLEncoding.DecodeString(value)
		if err != nil || len(decoded) != 24 {
			t.Fatal("cookie secret must decode to a supported AES key size")
		}
	})
}

func TestStorageCredentialsSecretKeys(t *testing.T) {
	cfg := &costv1alpha1.CostManagementServiceConfig{
		ObjectMeta: metav1.ObjectMeta{Name: "cost-management", Namespace: "cost-onprem"},
	}

	sec := StorageCredentialsSecret(cfg)
	if sec.Name != NameStorageSecret(cfg) {
		t.Errorf("secret name: got %q, want %q", sec.Name, NameStorageSecret(cfg))
	}
	if sec.Namespace != cfg.Namespace {
		t.Errorf("namespace: got %q, want %q", sec.Namespace, cfg.Namespace)
	}
	// KokuCommonEnv and IngressDeployment SecretKeyRefs depend on these exact keys.
	for _, key := range []string{"access-key", "secret-key"} {
		if _, ok := sec.StringData[key]; !ok {
			t.Errorf("StorageCredentialsSecret missing key %q required by S3 env wiring", key)
		}
	}
}

func TestNameStorageSecretUsesUserProvided(t *testing.T) {
	cfg := &costv1alpha1.CostManagementServiceConfig{
		ObjectMeta: metav1.ObjectMeta{Name: "cost-management"},
		Spec: costv1alpha1.CostManagementServiceConfigSpec{
			ObjectStorage: costv1alpha1.ObjectStorageConfig{
				SecretName: "my-s3-creds",
			},
		},
	}
	if got := NameStorageSecret(cfg); got != "my-s3-creds" {
		t.Fatalf("NameStorageSecret = %q, want my-s3-creds", got)
	}
}
