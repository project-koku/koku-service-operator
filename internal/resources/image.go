package resources

import (
	"strings"

	costv1alpha1 "github.com/project-koku/koku-service-operator/api/v1alpha1"
)

// ImageRef returns repository:tag when both fields are set.
// Whitespace-only values are treated as unset.
func ImageRef(img costv1alpha1.ImageSpec) (string, bool) {
	repo := strings.TrimSpace(img.Repository)
	tag := strings.TrimSpace(img.Tag)
	if repo == "" || tag == "" {
		return "", false
	}
	return repo + ":" + tag, true
}

// KokuImage is the container image for API, Masu, listener, celery, and the
// Koku migration Job. All of those workloads must use spec.costManagement.api.image.
func KokuImage(cfg *costv1alpha1.CostManagementServiceConfig) (string, bool) {
	return ImageRef(cfg.Spec.CostManagement.API.Image)
}
