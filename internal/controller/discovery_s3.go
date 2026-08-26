package controller

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	costv1alpha1 "github.com/project-koku/koku-service-operator/api/v1alpha1"
	"github.com/project-koku/koku-service-operator/internal/resources"
)

const (
	defaultOBCName            = "ros-data-ceph"
	defaultS3Region           = "us-east-1"
	objectBucketAPIGroup      = "objectbucket.io"
	objectBucketAPIVersion    = "v1alpha1"
	noobaaAdminNamespace      = "openshift-storage"
	noobaaStandaloneNamespace = "noobaa"
	noobaaAdminSecretName     = "noobaa-admin"
	noobaaDefaultEndpoint     = "s3.openshift-storage.svc.cluster.local"
	s3SourceAnnotation        = "koku.costmanagement.io/s3-source"
)

func objectBucketClaimGVK() schema.GroupVersionKind {
	return schema.GroupVersionKind{
		Group:   objectBucketAPIGroup,
		Version: objectBucketAPIVersion,
		Kind:    "ObjectBucketClaim",
	}
}

func objectBucketClaimListGVK() schema.GroupVersionKind {
	return schema.GroupVersionKind{
		Group:   objectBucketAPIGroup,
		Version: objectBucketAPIVersion,
		Kind:    "ObjectBucketClaimList",
	}
}

// resolveS3 resolves object storage with strict precedence:
//  1. User-provided Spec.ObjectStorage.SecretName
//  2. Bound ObjectBucketClaim (Direct Ceph RGW) in the CR namespace
//  3. NooBaa admin credentials in spec.objectStorage.noobaaNamespace
//     (default openshift-storage)
func (r *CostManagementServiceConfigReconciler) resolveS3(ctx context.Context, cfg *costv1alpha1.CostManagementServiceConfig) (*costv1alpha1.DiscoveredS3, error) {
	if cfg.Spec.ObjectStorage.SecretName != "" {
		return userProvidedS3(cfg), nil
	}

	if s3, err := r.discoverOBC(ctx, cfg); err == nil {
		return s3, nil
	}

	if s3, err := r.discoverNooBaa(ctx, cfg); err == nil {
		return s3, nil
	}

	return nil, fmt.Errorf("no S3 backend found — set spec.objectStorage.secretName (and endpoint), create a Bound ObjectBucketClaim in %s, or install ODF/NooBaa", cfg.Namespace)
}

func userProvidedS3(cfg *costv1alpha1.CostManagementServiceConfig) *costv1alpha1.DiscoveredS3 {
	return &costv1alpha1.DiscoveredS3{
		Endpoint:   resources.S3Endpoint(cfg),
		SecretName: cfg.Spec.ObjectStorage.SecretName,
		Region:     s3Region(cfg),
		Bucket:     cfg.Spec.ObjectStorage.Buckets.Koku,
	}
}

func s3Region(cfg *costv1alpha1.CostManagementServiceConfig) string {
	if cfg.Spec.ObjectStorage.S3.Region != "" {
		return cfg.Spec.ObjectStorage.S3.Region
	}
	return defaultS3Region
}

func (r *CostManagementServiceConfigReconciler) discoverOBC(ctx context.Context, cfg *costv1alpha1.CostManagementServiceConfig) (*costv1alpha1.DiscoveredS3, error) {
	obcName, err := r.findBoundOBC(ctx, cfg.Namespace)
	if err != nil {
		return nil, err
	}

	cm := &corev1.ConfigMap{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: cfg.Namespace, Name: obcName}, cm); err != nil {
		return nil, fmt.Errorf("OBC ConfigMap %s/%s: %w", cfg.Namespace, obcName, err)
	}
	host := cm.Data["BUCKET_HOST"]
	port := cm.Data["BUCKET_PORT"]
	bucket := cm.Data["BUCKET_NAME"]
	if port == "" {
		port = "443"
	}
	if host == "" {
		return nil, fmt.Errorf("OBC ConfigMap %s missing BUCKET_HOST", obcName)
	}
	if bucket == "" {
		return nil, fmt.Errorf("OBC ConfigMap %s missing BUCKET_NAME", obcName)
	}

	src := &corev1.Secret{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: cfg.Namespace, Name: obcName}, src); err != nil {
		return nil, fmt.Errorf("OBC Secret %s/%s: %w", cfg.Namespace, obcName, err)
	}
	accessKey := string(src.Data["AWS_ACCESS_KEY_ID"])
	secretKey := string(src.Data["AWS_SECRET_ACCESS_KEY"])
	if accessKey == "" || secretKey == "" {
		return nil, fmt.Errorf("OBC Secret %s missing AWS_ACCESS_KEY_ID/AWS_SECRET_ACCESS_KEY", obcName)
	}

	destName := resources.NameStorageSecret(cfg)
	if err := r.upsertStorageCredentials(ctx, cfg, destName, accessKey, secretKey, "obc:"+obcName); err != nil {
		return nil, err
	}

	return &costv1alpha1.DiscoveredS3{
		Endpoint:   fmt.Sprintf("https://%s:%s", host, port),
		SecretName: destName,
		Region:     s3Region(cfg),
		Bucket:     bucket,
	}, nil
}

func (r *CostManagementServiceConfigReconciler) findBoundOBC(ctx context.Context, namespace string) (string, error) {
	// Prefer the conventional Direct Ceph RGW claim name used by the Helm chart.
	obc := &unstructured.Unstructured{}
	obc.SetGroupVersionKind(objectBucketClaimGVK())
	err := r.Get(ctx, types.NamespacedName{Namespace: namespace, Name: defaultOBCName}, obc)
	if err == nil {
		phase, _, _ := unstructured.NestedString(obc.Object, "status", "phase")
		if phase == "Bound" {
			return defaultOBCName, nil
		}
	} else if !apierrors.IsNotFound(err) {
		// CRD may be absent on clusters without ODF — treat as not found.
		return "", fmt.Errorf("no bound ObjectBucketClaim found")
	}

	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(objectBucketClaimListGVK())
	if err := r.List(ctx, list, client.InNamespace(namespace)); err != nil {
		return "", fmt.Errorf("no bound ObjectBucketClaim found")
	}
	for i := range list.Items {
		item := &list.Items[i]
		phase, _, _ := unstructured.NestedString(item.Object, "status", "phase")
		if phase == "Bound" {
			return item.GetName(), nil
		}
	}
	return "", fmt.Errorf("no bound ObjectBucketClaim found")
}

func noobaaNamespace(cfg *costv1alpha1.CostManagementServiceConfig) string {
	if ns := cfg.Spec.ObjectStorage.NoobaaNamespace; ns != "" {
		return ns
	}
	return noobaaAdminNamespace
}

func noobaaNamespaceAllowed(ns string) bool {
	return ns == noobaaAdminNamespace || ns == noobaaStandaloneNamespace
}

// noobaaEndpoint is the discovered S3 URL for path 3.
// Default: https://s3.<noobaa-namespace>.svc.cluster.local:443.
// A non-empty spec.objectStorage.endpoint that is not the CRD default host
// (s3.openshift-storage.svc.cluster.local) is treated as a custom route and
// wins, using spec port/SSL. The CRD default host is ignored so that setting
// noobaaNamespace still produces s3.<ns>.svc DNS.
func noobaaEndpoint(cfg *costv1alpha1.CostManagementServiceConfig) string {
	host := cfg.Spec.ObjectStorage.Endpoint
	if host != "" && host != noobaaDefaultEndpoint {
		return resources.S3EndpointFromSpec(cfg)
	}
	return fmt.Sprintf("https://s3.%s.svc.cluster.local:443", noobaaNamespace(cfg))
}

func (r *CostManagementServiceConfigReconciler) discoverNooBaa(ctx context.Context, cfg *costv1alpha1.CostManagementServiceConfig) (*costv1alpha1.DiscoveredS3, error) {
	src := &corev1.Secret{}
	ns := noobaaNamespace(cfg)
	if !noobaaNamespaceAllowed(ns) {
		return nil, fmt.Errorf("spec.objectStorage.noobaaNamespace %q is not allowed (want %s or %s); for other namespaces set spec.objectStorage.secretName", ns, noobaaAdminNamespace, noobaaStandaloneNamespace)
	}
	// Use APIReader: noobaa-admin lives outside the OwnNamespace informer
	// cache (Cache.DefaultNamespaces), typically in openshift-storage.
	reader := r.APIReader
	if reader == nil {
		reader = r.Client
	}
	if err := reader.Get(ctx, types.NamespacedName{Namespace: ns, Name: noobaaAdminSecretName}, src); err != nil {
		return nil, fmt.Errorf("noobaa-admin secret %s/%s: %w", ns, noobaaAdminSecretName, err)
	}
	accessKey := string(src.Data["AWS_ACCESS_KEY_ID"])
	secretKey := string(src.Data["AWS_SECRET_ACCESS_KEY"])
	if accessKey == "" || secretKey == "" {
		return nil, fmt.Errorf("noobaa-admin secret missing AWS credentials")
	}

	destName := resources.NameStorageSecret(cfg)
	if err := r.upsertStorageCredentials(ctx, cfg, destName, accessKey, secretKey, "noobaa"); err != nil {
		return nil, err
	}

	return &costv1alpha1.DiscoveredS3{
		Endpoint:   noobaaEndpoint(cfg),
		SecretName: destName,
		Region:     s3Region(cfg),
		Bucket:     cfg.Spec.ObjectStorage.Buckets.Koku,
	}, nil
}

func (r *CostManagementServiceConfigReconciler) upsertStorageCredentials(ctx context.Context, cfg *costv1alpha1.CostManagementServiceConfig, name, accessKey, secretKey, source string) error {
	desired := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: cfg.Namespace,
			Labels:    resources.Labels(cfg, "object-storage"),
			Annotations: map[string]string{
				s3SourceAnnotation: source,
			},
		},
		Type: corev1.SecretTypeOpaque,
		// Use Data (not StringData) so the controller-runtime fake client
		// round-trips credentials correctly in unit tests.
		Data: map[string][]byte{
			s3SecretKeys[0]: []byte(accessKey),
			s3SecretKeys[1]: []byte(secretKey),
		},
	}
	setOwnerRef(cfg, desired)

	existing := &corev1.Secret{}
	err := r.Get(ctx, types.NamespacedName{Namespace: cfg.Namespace, Name: name}, existing)
	if apierrors.IsNotFound(err) {
		return r.Create(ctx, desired)
	}
	if err != nil {
		return err
	}
	existing.Data = desired.Data
	if existing.Annotations == nil {
		existing.Annotations = map[string]string{}
	}
	existing.Annotations[s3SourceAnnotation] = source
	return r.Update(ctx, existing)
}
