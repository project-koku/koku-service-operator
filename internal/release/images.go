// Package release owns the operand references shipped with the operator.
// Resource builders never silently select a release: only FollowOperator opts in.
package release

import (
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"

	costv1alpha1 "github.com/project-koku/koku-service-operator/api/v1alpha1"
	"github.com/project-koku/koku-service-operator/internal/resources"
)

//go:embed images.json
var shipped []byte

const (
	kruizeImage = "kruize"
	rosImage    = "ros"
)

type Bundle map[string]string

func Current() (Bundle, error) {
	return parseBundle(shipped)
}

func parseBundle(data []byte) (Bundle, error) {
	var bundle Bundle
	if err := json.Unmarshal(data, &bundle); err != nil {
		return nil, fmt.Errorf("decode embedded images.json: %w", err)
	}
	if bundle == nil {
		return nil, fmt.Errorf("decode embedded images.json: expected an image map")
	}
	for _, key := range []string{"gateway", "ingress", "koku", kruizeImage, "oauthProxy", "rbac", rosImage, "ui"} {
		ref, ok := bundle[key]
		if !ok {
			return nil, fmt.Errorf("embedded images.json is missing required image %q", key)
		}
		if _, err := ParseImage(ref); err != nil {
			return nil, fmt.Errorf("embedded images.json image %q: %w", key, err)
		}
	}
	return bundle, nil
}

func (b Bundle) ID() string {
	data, _ := json.Marshal(b)
	hash := sha256.Sum256(data)
	return hex.EncodeToString(hash[:])
}

// Resolve fills only omitted application references in an effective copy.
// Existing pins and the user's stored spec remain authoritative and unchanged.
func (b Bundle) Resolve(cfg *costv1alpha1.CostManagementServiceConfig) error {
	policy := cfg.Spec.Upgrade.ImagePolicy
	if policy == "" || policy == costv1alpha1.ImagePolicyPinned {
		return nil
	}
	if policy != costv1alpha1.ImagePolicyFollowOperator {
		return fmt.Errorf("unknown upgrade image policy %q", policy)
	}
	for key, image := range ImageFields(cfg) {
		if hasReference(*image) {
			if _, valid := resources.ImageRef(*image); !valid {
				return fmt.Errorf("%s image override requires a complete repository and tag or digest", key)
			}
			continue
		}
		if (key == rosImage || key == kruizeImage) && !costv1alpha1.ROSEnabled(cfg) {
			continue
		}
		resolved, err := ParseImage(b[key])
		if err != nil {
			return fmt.Errorf("release bundle %s: %w", key, err)
		}
		resolved.PullPolicy = image.PullPolicy
		*image = resolved
	}
	return nil
}

func hasReference(image costv1alpha1.ImageSpec) bool {
	return image.Repository != "" || image.Tag != "" || image.Digest != ""
}

func ParseImage(ref string) (costv1alpha1.ImageSpec, error) {
	var image costv1alpha1.ImageSpec
	if repo, digest, found := strings.Cut(ref, "@"); found {
		image.Repository, image.Digest = repo, digest
	} else if colon := strings.LastIndex(ref, ":"); colon > strings.LastIndex(ref, "/") {
		image.Repository, image.Tag = ref[:colon], ref[colon+1:]
	}
	if _, ok := resources.ImageRef(image); !ok {
		return image, fmt.Errorf("invalid release image reference %q", ref)
	}
	return image, nil
}

// ImageFields excludes Masu: an omitted override inherits the effective Koku pin.
// External infrastructure and development-only database/cache images are separate.
func ImageFields(cfg *costv1alpha1.CostManagementServiceConfig) map[string]*costv1alpha1.ImageSpec {
	return map[string]*costv1alpha1.ImageSpec{
		"koku": &cfg.Spec.CostManagement.API.Image, "rbac": &cfg.Spec.RBAC.Image,
		rosImage: &cfg.Spec.ROS.Image, kruizeImage: &cfg.Spec.Kruize.Image,
		"ingress": &cfg.Spec.Ingress.Image, "gateway": &cfg.Spec.Auth.Envoy.Image,
		"ui": &cfg.Spec.UI.App.Image, "oauthProxy": &cfg.Spec.UI.OAuthProxy.Image,
	}
}

//go:embed compatible-from.json
var compatibleSources []byte

// Matches distinguishes a complete shipped application combination from custom
// pins. A release ID alone must not certify arbitrary explicit overrides.
func (b Bundle) Matches(cfg *costv1alpha1.CostManagementServiceConfig) bool {
	for key, image := range ImageFields(cfg) {
		if (key == rosImage || key == kruizeImage) && !costv1alpha1.ROSEnabled(cfg) {
			continue
		}
		ref, ok := resources.ImageRef(*image)
		if !ok || ref != b[key] {
			return false
		}
	}
	if hasReference(cfg.Spec.CostManagement.Masu.Image) {
		ref, _ := resources.ImageRef(cfg.Spec.CostManagement.Masu.Image)
		if ref != b["koku"] {
			return false
		}
	}
	return true
}

// ValidateTransition only applies release edges when an omitted field actually
// changes an installed image. Explicit pin edits remain administrator-selected.
func (b Bundle) ValidateTransition(
	requested *costv1alpha1.CostManagementServiceConfig, source *costv1alpha1.ApplicationRevision, target map[string]string,
) error {
	if source == nil {
		return fmt.Errorf("installed source revision is required before an upgrade")
	}
	if requested.Spec.Upgrade.ImagePolicy != costv1alpha1.ImagePolicyFollowOperator {
		return nil
	}
	automatic := false
	for key, image := range ImageFields(requested) {
		ref, enabled := target[key]
		if enabled && !hasReference(*image) && ref != source.Images[key] {
			automatic = true
		}
	}
	if !automatic {
		return nil
	}
	for key, ref := range target {
		expected, exists := b[key]
		if key == "masu" {
			expected, exists = b["koku"]
		}
		if !exists {
			return fmt.Errorf("release bundle is missing required image %q", key)
		}
		if ref != expected {
			return fmt.Errorf("operator-managed upgrade would mix custom pins with a different release bundle; " +
				"select a complete explicit target or the shipped application combination")
		}
	}
	var sources []string
	if err := json.Unmarshal(compatibleSources, &sources); err != nil {
		return err
	}
	for _, id := range sources {
		if source.ReleaseManaged && id == source.ReleaseID {
			return nil
		}
	}
	return fmt.Errorf("no declared operator-managed upgrade edge from application release %s; "+
		"keep explicit pins until a supported bridge release is available", source.ReleaseID)
}
