package resources

import (
	"crypto/rand"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	costv1alpha1 "github.com/project-koku/koku-service-operator/api/v1alpha1"
)

// djangoKeyCharset is the character set specified by the COST-7694 ticket for
// Django secret keys. It matches Django's own recommendation for SECRET_KEY.
const djangoKeyCharset = "abcdefghijklmnopqrstuvwxyz0123456789!@#$%^&*(-_=+)"

// DBCredentialsSecret builds the Secret that holds all database passwords.
// If the secret already exists the caller should not overwrite it
// (use SSA with a create-only strategy or check existence first).
func DBCredentialsSecret(cfg *costv1alpha1.CostManagementServiceConfig) *corev1.Secret {
	return &corev1.Secret{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "v1",
			Kind:       "Secret",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      NameDBCredentials(cfg),
			Namespace: cfg.Namespace,
			Labels:    Labels(cfg, "database"),
		},
		StringData: map[string]string{
			"postgres-user":     "postgres",
			"postgres-password": randomPassword(),
			"koku-user":         "koku_user",
			"koku-password":     randomPassword(),
			"ros-user":          "ros_user",
			"ros-password":      randomPassword(),
			"kruize-user":       "kruize_user",
			"kruize-password":   randomPassword(),
			"rbac-user":         "rbac_user",
			"rbac-password":     randomPassword(),
		},
	}
}

// DjangoSecret builds the Secret holding the Django secret key.
func DjangoSecret(cfg *costv1alpha1.CostManagementServiceConfig) *corev1.Secret {
	return &corev1.Secret{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "v1",
			Kind:       "Secret",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      NameDjangoSecret(cfg),
			Namespace: cfg.Namespace,
			Labels:    Labels(cfg, "cost-management"),
		},
		StringData: map[string]string{
			"secret-key": djangoSecretKey(50),
		},
	}
}

// StorageCredentialsSecret builds a placeholder Secret for S3 credentials.
// When no real secretName is set, the operator creates this so pods can start;
// the admin should update it with real values before S3-dependent features work.
func StorageCredentialsSecret(cfg *costv1alpha1.CostManagementServiceConfig) *corev1.Secret {
	return &corev1.Secret{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Secret"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      NameStorageSecret(cfg),
			Namespace: cfg.Namespace,
			Labels:    Labels(cfg, "object-storage"),
			Annotations: map[string]string{
				"koku.costmanagement.io/placeholder": "true",
			},
		},
		StringData: map[string]string{
			"access-key": "REPLACE_WITH_REAL_S3_ACCESS_KEY",
			"secret-key": "REPLACE_WITH_REAL_S3_SECRET_KEY",
		},
	}
}

// djangoSecretKey generates an n-character key using djangoKeyCharset.
// Django uses this key for security-sensitive signing. crypto/rand supplies
// the random bytes; mapping them to this alphabet introduces modulo bias.
func djangoSecretKey(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	key := make([]byte, n)
	for i, v := range b {
		key[i] = djangoKeyCharset[int(v)%len(djangoKeyCharset)]
	}
	return string(key)
}

func randomPassword() string {
	const n = 32
	b := make([]byte, n)
	_, _ = rand.Read(b)
	key := make([]byte, n)
	// base64URL alphabet (A-Za-z0-9_-) is safe for all database password fields.
	const alpha = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789_-"
	for i, v := range b {
		key[i] = alpha[int(v)%len(alpha)]
	}
	return string(key)
}

// EnvFromSecret returns a corev1.EnvVar that reads a value from a named Secret key.
func EnvFromSecret(envName, secretName, secretKey string) corev1.EnvVar {
	return corev1.EnvVar{
		Name: envName,
		ValueFrom: &corev1.EnvVarSource{
			SecretKeyRef: &corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: secretName},
				Key:                  secretKey,
			},
		},
	}
}

// EnvFromSecretOptional is like EnvFromSecret but marks the reference optional.
func EnvFromSecretOptional(envName, secretName, secretKey string) corev1.EnvVar {
	opt := true
	return corev1.EnvVar{
		Name: envName,
		ValueFrom: &corev1.EnvVarSource{
			SecretKeyRef: &corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: secretName},
				Key:                  secretKey,
				Optional:             &opt,
			},
		},
	}
}

// EnvVal is a convenience constructor for a plain string env var.
func EnvVal(name, value string) corev1.EnvVar {
	return corev1.EnvVar{Name: name, Value: value}
}

// EnvFromFieldRef returns an env var populated from a pod field (e.g. resource limits).
func EnvFromFieldRef(envName, containerName, resource string) corev1.EnvVar {
	return corev1.EnvVar{
		Name: envName,
		ValueFrom: &corev1.EnvVarSource{
			ResourceFieldRef: &corev1.ResourceFieldSelector{
				ContainerName: containerName,
				Resource:      resource,
			},
		},
	}
}
