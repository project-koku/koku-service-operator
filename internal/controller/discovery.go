package controller

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	costv1alpha1 "github.com/project-koku/koku-service-operator/api/v1alpha1"
)

const openShiftConfigAPIGroup = "config.openshift.io"

// reconcileDiscovery is Stage 0 of the reconcile pipeline.
// It auto-detects the cluster domain and default StorageClass from the cluster
// and writes the results into status.discoveredConfig.
//
// If the user has already populated spec.global.clusterDomain or
// spec.global.storageClass, those values take precedence over auto-detection.
func (r *CostManagementServiceConfigReconciler) reconcileDiscovery(ctx context.Context, cfg *costv1alpha1.CostManagementServiceConfig) (Result, error) {
	if cfg.Status.DiscoveredConfig == nil {
		cfg.Status.DiscoveredConfig = &costv1alpha1.DiscoveredConfig{}
	}

	// Cluster domain
	domain, err := r.resolveClusterDomain(ctx, cfg)
	if err != nil {
		r.setCondition(cfg, costv1alpha1.ConditionDiscoveryComplete, metav1.ConditionFalse,
			"ClusterDomainNotFound",
			fmt.Sprintf("%v — set spec.global.clusterDomain to override", err))
		return Result{RequeueAfter: requeueSlow}, nil
	}
	cfg.Status.DiscoveredConfig.ClusterDomain = domain

	// Default StorageClass
	sc, err := r.resolveStorageClass(ctx, cfg)
	if err != nil {
		r.setCondition(cfg, costv1alpha1.ConditionDiscoveryComplete, metav1.ConditionFalse,
			"StorageClassNotFound",
			fmt.Sprintf("%v — set spec.global.storageClass to override", err))
		return Result{RequeueAfter: requeueSlow}, nil
	}
	cfg.Status.DiscoveredConfig.StorageClass = sc

	// S3 / object storage (COST-7683). User-provided → OBC → NooBaa.
	// Failure sets StorageReady=False but does not block the pipeline —
	// clusters without ODF (e.g. BYOI + MinIO with secretName set) still proceed.
	s3, err := r.resolveS3(ctx, cfg)
	if err != nil {
		cfg.Status.DiscoveredConfig.S3 = nil
		r.setCondition(cfg, costv1alpha1.ConditionStorageReady, metav1.ConditionFalse,
			"S3NotFound", err.Error())
	} else {
		cfg.Status.DiscoveredConfig.S3 = s3
		reason := "Discovered"
		if cfg.Spec.ObjectStorage.SecretName != "" {
			reason = "UserProvided"
		}
		r.setCondition(cfg, costv1alpha1.ConditionStorageReady, metav1.ConditionTrue,
			reason, fmt.Sprintf("endpoint=%s secret=%s", s3.Endpoint, s3.SecretName))
	}

	if !apimeta.IsStatusConditionTrue(cfg.Status.Conditions, costv1alpha1.ConditionDiscoveryComplete) {
		r.Recorder.Eventf(cfg, corev1.EventTypeNormal, "DiscoveryComplete",
			"clusterDomain=%s storageClass=%s", domain, sc)
	}
	r.setCondition(cfg, costv1alpha1.ConditionDiscoveryComplete, metav1.ConditionTrue,
		"Discovered",
		fmt.Sprintf("clusterDomain=%s storageClass=%s", domain, sc))
	return Result{}, nil
}

// resolveClusterDomain returns the cluster ingress domain.
// If spec.global.clusterDomain is set and non-default, it is returned as-is.
// Otherwise the OpenShift Ingress cluster config is queried.
func (r *CostManagementServiceConfigReconciler) resolveClusterDomain(ctx context.Context, cfg *costv1alpha1.CostManagementServiceConfig) (string, error) {
	explicit := cfg.Spec.Global.ClusterDomain
	if explicit != "" && explicit != "apps.cluster.local" {
		return explicit, nil
	}
	return discoverClusterDomain(ctx, r.Client)
}

// resolveStorageClass returns the StorageClass name to use.
// If spec.global.storageClass is set, it is returned as-is.
// Otherwise the cluster default StorageClass is queried.
func (r *CostManagementServiceConfigReconciler) resolveStorageClass(ctx context.Context, cfg *costv1alpha1.CostManagementServiceConfig) (string, error) {
	if cfg.Spec.Global.StorageClass != "" {
		return cfg.Spec.Global.StorageClass, nil
	}
	return discoverDefaultStorageClass(ctx, r.Client)
}

// discoverClusterDomain reads the OpenShift cluster Ingress config and returns
// the base domain. Uses unstructured access so openshift/api is not a dependency.
func discoverClusterDomain(ctx context.Context, c client.Client) (string, error) {
	ingress := &unstructured.Unstructured{}
	ingress.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   openShiftConfigAPIGroup,
		Version: "v1",
		Kind:    "Ingress",
	})
	if err := c.Get(ctx, types.NamespacedName{Name: "cluster"}, ingress); err != nil {
		return "", fmt.Errorf("get %s/v1 Ingress/cluster: %w", openShiftConfigAPIGroup, err)
	}
	domain, found, err := unstructured.NestedString(ingress.Object, "spec", "domain")
	if err != nil {
		return "", fmt.Errorf("reading Ingress spec.domain: %w", err)
	}
	if !found || domain == "" {
		return "", fmt.Errorf("ingress/cluster spec.domain is empty")
	}
	return domain, nil
}

// discoverDefaultStorageClass lists all StorageClasses and returns the one
// annotated as the cluster default.
func discoverDefaultStorageClass(ctx context.Context, c client.Client) (string, error) {
	list := &storagev1.StorageClassList{}
	if err := c.List(ctx, list); err != nil {
		return "", fmt.Errorf("listing StorageClasses: %w", err)
	}
	for i := range list.Items {
		sc := &list.Items[i]
		if sc.Annotations["storageclass.kubernetes.io/is-default-class"] == annotationTrue {
			return sc.Name, nil
		}
	}
	return "", fmt.Errorf("no StorageClass with annotation storageclass.kubernetes.io/is-default-class=true found")
}
