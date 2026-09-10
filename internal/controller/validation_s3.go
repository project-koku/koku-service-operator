package controller

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	smithyhttp "github.com/aws/smithy-go/transport/http"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	costv1alpha1 "github.com/project-koku/koku-service-operator/api/v1alpha1"
	"github.com/project-koku/koku-service-operator/internal/resources"
)

const (
	caCertKey                        = "ca.crt"
	storageReasonUnreachable         = "StorageUnreachable"
	storageReasonBucketInaccessible  = "StorageBucketInaccessible"
	storageReasonBucketsAccessible   = "StorageBucketsAccessible"
	storageReasonBucketConfigInvalid = "StorageBucketConfigInvalid"
)

// validateObjectStorage checks S3 credentials then probes the object store.
// Non-blocking: failures set StorageReady=False but do not gate Migration.
//
// Secret resolution matches Discovery: user spec.objectStorage.secretName, else
// status.discoveredConfig.s3.secretName. Missing keys fail before any network
// call (G2). HeadBucket on each effective bucket confirms endpoint reachability,
// TLS, credential acceptance (G1), and the bucket contract the operands rely on
// — using only the narrow per-bucket permission the operands actually need. It
// deliberately avoids ListBuckets, which requires account-wide
// s3:ListAllMyBuckets that BYOI least-privilege credentials commonly lack.
func (r *CostManagementServiceConfigReconciler) validateObjectStorage(ctx context.Context, cfg *costv1alpha1.CostManagementServiceConfig) {
	secretName := cfg.Spec.ObjectStorage.SecretName
	if secretName == "" && cfg.Status.DiscoveredConfig != nil && cfg.Status.DiscoveredConfig.S3 != nil {
		secretName = cfg.Status.DiscoveredConfig.S3.SecretName
	}
	if secretName == "" {
		return
	}

	secret, err := r.getSecret(ctx, cfg.Namespace, secretName, s3SecretKeys)
	if err != nil {
		r.setCondition(cfg, costv1alpha1.ConditionStorageReady, metav1.ConditionFalse,
			"StorageSecretInvalid", err.Error())
		return
	}

	var caCertPool *x509.CertPool
	if caName := cfg.Spec.ObjectStorage.CACertSecretName; caName != "" && !cfg.Spec.ObjectStorage.InsecureSkipVerify {
		caSecret, err := r.getSecret(ctx, cfg.Namespace, caName, []string{caCertKey})
		if err != nil {
			r.setCondition(cfg, costv1alpha1.ConditionStorageReady, metav1.ConditionFalse,
				"StorageCACertInvalid", err.Error())
			return
		}
		caCertPool, err = certPoolFromPEM(caSecret.Data[caCertKey])
		if err != nil {
			r.setCondition(cfg, costv1alpha1.ConditionStorageReady, metav1.ConditionFalse,
				"StorageCACertInvalid", fmt.Sprintf("secret %q key %q contains no valid PEM certificates", caName, caCertKey))
			return
		}
	}

	endpoint := resources.S3Endpoint(cfg)
	region := s3Region(cfg)
	accessKey := string(secret.Data[s3SecretKeys[0]])
	secretKey := string(secret.Data[s3SecretKeys[1]])
	buckets, err := requiredStorageBuckets(cfg)
	if err != nil {
		r.setCondition(cfg, costv1alpha1.ConditionStorageReady, metav1.ConditionFalse,
			storageReasonBucketConfigInvalid, err.Error())
		return
	}
	if err := s3BucketContractProbe(ctx, endpoint, region, accessKey, secretKey, validationTimeout, cfg.Spec.ObjectStorage.InsecureSkipVerify, caCertPool, buckets); err != nil {
		// A HeadBucket that reached the endpoint (any HTTP status, e.g. 403/404)
		// proves the endpoint is reachable and the fault is the bucket or its
		// permissions. A transport failure (DNS/TCP/TLS/timeout, no HTTP
		// response) means the endpoint itself is unreachable.
		reason := storageReasonUnreachable
		if s3EndpointResponded(err) {
			reason = storageReasonBucketInaccessible
		}
		r.setCondition(cfg, costv1alpha1.ConditionStorageReady, metav1.ConditionFalse,
			reason, err.Error())
		return
	}
	r.setCondition(cfg, costv1alpha1.ConditionStorageReady, metav1.ConditionTrue,
		storageReasonBucketsAccessible, fmt.Sprintf("validated buckets [%s] via %s", strings.Join(buckets, ", "), endpoint))
}

type s3BucketAccessError struct {
	bucket string
	err    error
}

func (e *s3BucketAccessError) Error() string {
	return fmt.Sprintf("HeadBucket %q: %v", e.bucket, e.err)
}

func (e *s3BucketAccessError) Unwrap() error {
	return e.err
}

func requiredStorageBuckets(cfg *costv1alpha1.CostManagementServiceConfig) ([]string, error) {
	kokuBucket := strings.TrimSpace(resources.S3Bucket(cfg))
	if kokuBucket == "" {
		return nil, fmt.Errorf("spec.objectStorage.buckets.koku is required unless discovery resolves the primary Koku bucket")
	}

	buckets := make([]string, 0, 3)
	seen := make(map[string]struct{}, 3)
	add := func(bucket string) {
		bucket = strings.TrimSpace(bucket)
		if bucket == "" {
			return
		}
		if _, ok := seen[bucket]; ok {
			return
		}
		seen[bucket] = struct{}{}
		buckets = append(buckets, bucket)
	}

	add(kokuBucket)
	// Ingress inherits the Koku bucket unless explicitly overridden, so this is
	// non-empty whenever koku resolves; add() dedups the shared-bucket case.
	add(resources.S3IngressBucket(cfg))
	if costv1alpha1.ROSEnabled(cfg) {
		rosBucket := strings.TrimSpace(resources.S3ROSBucket(cfg))
		if rosBucket == "" {
			return nil, fmt.Errorf("spec.objectStorage.buckets.ros is required when ros.enabled is true")
		}
		add(rosBucket)
	}
	return buckets, nil
}

// s3BucketContractProbe validates every effective bucket with HeadBucket against
// endpoint using path-style addressing (required for MinIO / NooBaa / Ceph RGW).
//
// It deliberately does not call ListBuckets: that requires account-wide
// s3:ListAllMyBuckets, which BYOI credentials scoped to named buckets
// legitimately lack. HeadBucket on the buckets the operands actually use proves
// endpoint reachability, TLS, and credential acceptance with only the narrow
// permission those operands need.
func s3BucketContractProbe(ctx context.Context, endpoint, region, accessKey, secretKey string, timeout time.Duration, insecureSkipVerify bool, caCertPool *x509.CertPool, buckets []string) error {
	if region == "" {
		region = defaultS3Region
	}
	client, err := newS3ValidationClient(endpoint, region, accessKey, secretKey, timeout, insecureSkipVerify, caCertPool)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	for _, bucket := range buckets {
		if _, err := client.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: aws.String(bucket)}); err != nil {
			return &s3BucketAccessError{bucket: bucket, err: err}
		}
	}
	return nil
}

// s3EndpointResponded reports whether err carries a real HTTP response from the
// S3 endpoint. A response with a status code (even 403/404) means the endpoint
// is reachable and the fault is bucket-level. A transport failure (DNS/TCP/TLS/
// timeout) means the endpoint itself is unreachable — note the retryer still
// wraps send failures in a ResponseError with StatusCode 0, so the status code
// must be checked, not merely the error type.
func s3EndpointResponded(err error) bool {
	var respErr *smithyhttp.ResponseError
	if !errors.As(err, &respErr) || respErr.Response == nil || respErr.Response.Response == nil {
		return false
	}
	return respErr.HTTPStatusCode() != 0
}

func newS3ValidationClient(endpoint, region, accessKey, secretKey string, timeout time.Duration, insecureSkipVerify bool, caCertPool *x509.CertPool) (*s3.Client, error) {
	u, err := url.Parse(endpoint)
	if err != nil {
		return nil, fmt.Errorf("parse S3 endpoint %q: %w", endpoint, err)
	}
	if u.Scheme == "" || u.Host == "" {
		return nil, fmt.Errorf("S3 endpoint %q must include scheme and host", endpoint)
	}

	base, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		base = &http.Transport{}
	}
	transport := base.Clone()
	if transport.TLSClientConfig == nil {
		transport.TLSClientConfig = &tls.Config{}
	}
	transport.TLSClientConfig.InsecureSkipVerify = insecureSkipVerify //nolint:gosec // user-controlled via CR spec
	transport.TLSClientConfig.RootCAs = caCertPool
	httpClient := &http.Client{Timeout: timeout, Transport: transport}
	return s3.New(s3.Options{
		Region:       region,
		Credentials:  aws.NewCredentialsCache(credentials.NewStaticCredentialsProvider(accessKey, secretKey, "")),
		BaseEndpoint: aws.String(endpoint),
		UsePathStyle: true,
		HTTPClient:   httpClient,
		EndpointOptions: s3.EndpointResolverOptions{
			DisableHTTPS: u.Scheme == "http",
		},
	}), nil
}
