package v1alpha1

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation/field"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/webhook"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

// SetupWebhookWithManager registers the mutating and validating webhooks.
func (c *CostManagementServiceConfig) SetupWebhookWithManager(mgr ctrl.Manager) error {
	return ctrl.NewWebhookManagedBy(mgr).
		For(c).
		WithDefaulter(c).
		WithValidator(c).
		Complete()
}

// +kubebuilder:webhook:path=/mutate-service-costmanagement-openshift-io-v1alpha1-costmanagementserviceconfig,mutating=true,failurePolicy=fail,sideEffects=None,groups=service.costmanagement.openshift.io,resources=costmanagementserviceconfigs,verbs=create;update,versions=v1alpha1,name=mcostmanagementserviceconfig.kb.io,admissionReviewVersions=v1

var _ webhook.CustomDefaulter = &CostManagementServiceConfig{}

// Default implements webhook.CustomDefaulter.
// spec.profile and spec.monitoring.enabled are already defaulted via CRD OpenAPI markers.
// dataRetentionMonths defaulting will be added when COST-7678 G3 (#16) merges.
func (c *CostManagementServiceConfig) Default(_ context.Context, obj runtime.Object) error {
	_, ok := obj.(*CostManagementServiceConfig)
	if !ok {
		return fmt.Errorf("expected CostManagementServiceConfig, got %T", obj)
	}
	return nil
}

// +kubebuilder:webhook:path=/validate-service-costmanagement-openshift-io-v1alpha1-costmanagementserviceconfig,mutating=false,failurePolicy=fail,sideEffects=None,groups=service.costmanagement.openshift.io,resources=costmanagementserviceconfigs,verbs=create;update,versions=v1alpha1,name=vcostmanagementserviceconfig.kb.io,admissionReviewVersions=v1

var _ webhook.CustomValidator = &CostManagementServiceConfig{}

// ValidateCreate implements webhook.CustomValidator.
func (c *CostManagementServiceConfig) ValidateCreate(_ context.Context, obj runtime.Object) (admission.Warnings, error) {
	cms, ok := obj.(*CostManagementServiceConfig)
	if !ok {
		return nil, fmt.Errorf("expected CostManagementServiceConfig, got %T", obj)
	}
	return nil, cms.validateCostManagementServiceConfig()
}

// ValidateUpdate implements webhook.CustomValidator.
func (c *CostManagementServiceConfig) ValidateUpdate(_ context.Context, _, newObj runtime.Object) (admission.Warnings, error) {
	cms, ok := newObj.(*CostManagementServiceConfig)
	if !ok {
		return nil, fmt.Errorf("expected CostManagementServiceConfig, got %T", newObj)
	}
	return nil, cms.validateCostManagementServiceConfig()
}

// ValidateDelete implements webhook.CustomValidator.
func (c *CostManagementServiceConfig) ValidateDelete(context.Context, runtime.Object) (admission.Warnings, error) {
	return nil, nil
}

func (c *CostManagementServiceConfig) validateCostManagementServiceConfig() error {
	allErrs := make(field.ErrorList, 0, 32)

	specPath := field.NewPath("spec")
	spec := &c.Spec

	allErrs = append(allErrs, validateResourceRequirements(specPath.Child("database", "resources"), spec.Database.Resources)...)
	allErrs = append(allErrs, validateResourceRequirements(specPath.Child("cache", "resources"), spec.Cache.Resources)...)
	allErrs = append(allErrs, validateResourceRequirements(specPath.Child("auth", "envoy", "resources"), spec.Auth.Envoy.Resources)...)
	allErrs = append(allErrs, validateResourceRequirements(specPath.Child("rbac", "api", "resources"), spec.RBAC.API.Resources)...)
	allErrs = append(allErrs, validateResourceRequirements(specPath.Child("rbac", "worker", "resources"), spec.RBAC.Worker.Resources)...)
	allErrs = append(allErrs, validateResourceRequirements(specPath.Child("rbac", "keycloakSync", "resources"), spec.RBAC.KeycloakSync.Resources)...)

	cm := specPath.Child("costManagement")
	allErrs = append(allErrs, validateResourceRequirements(cm.Child("api", "resources"), spec.CostManagement.API.Resources)...)
	allErrs = append(allErrs, validateResourceRequirements(cm.Child("masu", "resources"), spec.CostManagement.Masu.Resources)...)
	allErrs = append(allErrs, validateResourceRequirements(cm.Child("listener", "resources"), spec.CostManagement.Listener.Resources)...)

	workers := cm.Child("celery", "workers")
	w := spec.CostManagement.Celery.Workers
	allErrs = append(allErrs, validateResourceRequirements(workers.Child("default", "resources"), w.Default.Resources)...)
	allErrs = append(allErrs, validateResourceRequirements(workers.Child("priority", "resources"), w.Priority.Resources)...)
	allErrs = append(allErrs, validateResourceRequirements(workers.Child("summary", "resources"), w.Summary.Resources)...)
	allErrs = append(allErrs, validateResourceRequirements(workers.Child("ocp", "resources"), w.OCP.Resources)...)
	allErrs = append(allErrs, validateResourceRequirements(workers.Child("costModel", "resources"), w.CostModel.Resources)...)
	allErrs = append(allErrs, validateResourceRequirements(workers.Child("refresh", "resources"), w.Refresh.Resources)...)
	allErrs = append(allErrs, validateResourceRequirements(workers.Child("hcs", "resources"), w.HCS.Resources)...)
	allErrs = append(allErrs, validateResourceRequirements(workers.Child("download", "resources"), w.Download.Resources)...)
	allErrs = append(allErrs, validateResourceRequirements(workers.Child("subsExtraction", "resources"), w.SubsExtraction.Resources)...)
	allErrs = append(allErrs, validateResourceRequirements(workers.Child("subsTransmission", "resources"), w.SubsTransmission.Resources)...)

	ros := specPath.Child("ros")
	allErrs = append(allErrs, validateResourceRequirements(ros.Child("api", "resources"), spec.ROS.API.Resources)...)
	allErrs = append(allErrs, validateResourceRequirements(ros.Child("processor", "resources"), spec.ROS.Processor.Resources)...)
	allErrs = append(allErrs, validateResourceRequirements(ros.Child("recommendationPoller", "resources"), spec.ROS.RecommendationPoller.Resources)...)
	allErrs = append(allErrs, validateResourceRequirements(ros.Child("housekeeper", "resources"), spec.ROS.Housekeeper.Resources)...)
	allErrs = append(allErrs, validateResourceRequirements(ros.Child("housekeeper", "partitionCleaner", "resources"), spec.ROS.Housekeeper.PartitionCleaner.Resources)...)

	allErrs = append(allErrs, validateResourceRequirements(specPath.Child("kruize", "resources"), spec.Kruize.Resources)...)
	allErrs = append(allErrs, validateResourceRequirements(specPath.Child("kruize", "partitions", "resources"), spec.Kruize.Partitions.Resources)...)
	allErrs = append(allErrs, validateResourceRequirements(specPath.Child("ingress", "resources"), spec.Ingress.Resources)...)
	allErrs = append(allErrs, validateResourceRequirements(specPath.Child("ui", "oauthProxy", "resources"), spec.UI.OAuthProxy.Resources)...)
	allErrs = append(allErrs, validateResourceRequirements(specPath.Child("ui", "app", "resources"), spec.UI.App.Resources)...)

	if len(allErrs) == 0 {
		return nil
	}

	return apierrors.NewInvalid(
		schema.GroupKind{Group: "service.costmanagement.openshift.io", Kind: "CostManagementServiceConfig"},
		c.Name,
		allErrs,
	)
}
