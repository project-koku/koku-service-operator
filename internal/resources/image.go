package resources

import (
	"strings"

	costv1alpha1 "github.com/project-koku/koku-service-operator/api/v1alpha1"
)

// ImageRef returns repository:tag when both fields are set.
// Whitespace-only values are treated as unset. Builders must not invent a
// catalog pin; the reconciler rejects missing images before apply.
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

// MissingWorkloadImages returns spec paths whose image repository+tag must be
// set for this CR. Community vs product pins live on sample manifests, not here.
func MissingWorkloadImages(cfg *costv1alpha1.CostManagementServiceConfig) []string {
	var missing []string
	require := func(spec costv1alpha1.ImageSpec, field string) {
		if _, ok := ImageRef(spec); !ok {
			missing = append(missing, field)
		}
	}

	if costv1alpha1.BoolVal(cfg.Spec.Database.Deploy, true) {
		require(cfg.Spec.Database.Image, "spec.database.image")
	}
	if costv1alpha1.BoolVal(cfg.Spec.Cache.Deploy, true) {
		require(cfg.Spec.Cache.Image, "spec.cache.image")
	}
	require(cfg.Spec.Auth.Envoy.Image, "spec.auth.envoy.image")
	require(cfg.Spec.UI.OAuthProxy.Image, "spec.ui.oauthProxy.image")
	require(cfg.Spec.UI.App.Image, "spec.ui.app.image")
	require(cfg.Spec.CostManagement.API.Image, "spec.costManagement.api.image")
	require(cfg.Spec.RBAC.Image, "spec.rbac.image")
	require(cfg.Spec.Ingress.Image, "spec.ingress.image")
	if costv1alpha1.ROSEnabled(cfg) {
		require(cfg.Spec.ROS.Image, "spec.ros.image")
		require(cfg.Spec.Kruize.Image, "spec.kruize.image")
	}
	return missing
}
