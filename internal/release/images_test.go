package release

import (
	"maps"
	"os"
	"strings"
	"testing"

	cost "github.com/project-koku/koku-service-operator/api/v1alpha1"
	"github.com/project-koku/koku-service-operator/internal/resources"
	"sigs.k8s.io/yaml"
)

func mustCurrentBundle(t *testing.T) Bundle {
	t.Helper()
	bundle, err := Current()
	if err != nil {
		t.Fatal(err)
	}
	return bundle
}

func TestBundleRelatedImagesMatchRelease(t *testing.T) {
	data, err := os.ReadFile("../../bundle/manifests/koku-service-operator.clusterserviceversion.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var csv struct {
		Spec struct {
			RelatedImages []struct{ Name, Image string } `json:"relatedImages"`
		} `json:"spec"`
	}
	if err := yaml.Unmarshal(data, &csv); err != nil {
		t.Fatal(err)
	}
	actual := map[string]string{}
	for _, image := range csv.Spec.RelatedImages {
		if image.Name != "koku-service-operator" {
			actual[image.Name] = image.Image
		}
	}
	if !maps.Equal(actual, map[string]string(mustCurrentBundle(t))) {
		t.Fatal("bundle operands differ from the operator's shipped release manifest; regenerate the bundle")
	}
}

func TestResolveImageOwnership(t *testing.T) {
	for _, policy := range []string{"", "Pinned", "FollowOperator"} {
		t.Run(policy, func(t *testing.T) {
			cfg := &cost.CostManagementServiceConfig{}
			cfg.Spec.Upgrade.ImagePolicy = policy
			if err := mustCurrentBundle(t).Resolve(cfg); err != nil {
				t.Fatal(err)
			}
			_, present := resources.KokuImage(cfg)
			if present != (policy == "FollowOperator") {
				t.Fatalf("policy %q unexpectedly resolved Koku: %v", policy, present)
			}
		})
	}
	cfg := &cost.CostManagementServiceConfig{}
	cfg.Spec.Upgrade.ImagePolicy = "FollowOperator"
	cfg.Spec.CostManagement.API.Image = cost.ImageSpec{Repository: "quay.io/custom/koku", Digest: "sha256:" + strings.Repeat("a", 64)}
	if err := mustCurrentBundle(t).Resolve(cfg); err != nil {
		t.Fatal(err)
	}
	image, _ := resources.KokuImage(cfg)
	if !strings.HasPrefix(image, "quay.io/custom/koku@") {
		t.Fatal("explicit pin overwritten")
	}
	if cfg.Spec.CostManagement.Masu.Image.Repository != "" {
		t.Fatal("omitted Masu must inherit the effective API image")
	}
	cfg.Spec.RBAC.Image = cost.ImageSpec{Repository: "partial"}
	if err := mustCurrentBundle(t).Resolve(cfg); err == nil {
		t.Fatal("partial override accepted")
	}
	cfg.Spec.Upgrade.ImagePolicy = "invalid"
	if err := mustCurrentBundle(t).Resolve(cfg); err == nil {
		t.Fatal("invalid policy accepted")
	}
}
func TestReleaseBundleChanges(t *testing.T) {
	a, b := mustCurrentBundle(t), mustCurrentBundle(t)
	b["koku"] = "quay.io/custom/koku:v2"
	if a.ID() == b.ID() {
		t.Fatal("release change did not change identity")
	}
	cfg := &cost.CostManagementServiceConfig{}
	cfg.Spec.Upgrade.ImagePolicy = "FollowOperator"
	if err := (Bundle{}).Resolve(cfg); err == nil {
		t.Fatal("missing release metadata must fail closed")
	}
	for _, ref := range mustCurrentBundle(t) {
		image, err := ParseImage(ref)
		if err != nil {
			t.Fatal(err)
		}
		if got, _ := resources.ImageRef(image); got != ref {
			t.Fatalf("image reference changed: %s", got)
		}
	}
	for _, ref := range []string{"", "quay.io/image", "quay.io/image@sha256:bad"} {
		if _, err := ParseImage(ref); err == nil {
			t.Fatalf("invalid reference %q accepted", ref)
		}
	}
	if _, err := ParseImage("localhost:5000/image@sha256:" + strings.Repeat("b", 64)); err != nil {
		t.Fatal(err)
	}
}

func TestParseBundleReturnsContextualErrors(t *testing.T) {
	if _, err := parseBundle([]byte("{")); err == nil || !strings.Contains(err.Error(), "decode embedded images.json") {
		t.Fatalf("malformed embedded JSON error = %v", err)
	}
	_, err := parseBundle([]byte(`{"koku":"quay.io/project-koku/koku:v1"}`))
	if err == nil || !strings.Contains(err.Error(), `missing required image "gateway"`) {
		t.Fatalf("incomplete embedded image map error = %v", err)
	}
	data := []byte(`{"gateway":"invalid","ingress":"x","koku":"x","kruize":"x","oauthProxy":"x","rbac":"x","ros":"x","ui":"x"}`)
	_, err = parseBundle(data)
	if err == nil || !strings.Contains(err.Error(), `image "gateway"`) {
		t.Fatalf("invalid embedded image reference error = %v", err)
	}
}

func TestAutomaticReleaseEdges(t *testing.T) {
	original := &cost.CostManagementServiceConfig{}
	original.Spec.Upgrade.ImagePolicy = "FollowOperator"
	effective := original.DeepCopy()
	old := mustCurrentBundle(t)
	if err := old.Resolve(effective); err != nil {
		t.Fatal(err)
	}
	source := &cost.ApplicationRevision{ReleaseID: old.ID(), ReleaseManaged: old.Matches(effective), Images: map[string]string{}}
	for key, image := range ImageFields(effective) {
		if key == "ros" || key == "kruize" {
			continue
		}
		source.Images[key], _ = resources.ImageRef(*image)
	}
	source.Images["masu"] = source.Images["koku"]
	target := map[string]string{}
	maps.Copy(target, source.Images)
	next := mustCurrentBundle(t)
	next["koku"] = "quay.io/project-koku/koku:v2"
	target["koku"], target["masu"] = next["koku"], next["koku"]
	if err := next.ValidateTransition(original, source, target); err != nil {
		t.Fatal(err)
	}
	missingImage := Bundle{}
	maps.Copy(missingImage, next)
	delete(missingImage, "ui")
	err := missingImage.ValidateTransition(original, source, target)
	if err == nil || !strings.Contains(err.Error(), `missing required image "ui"`) {
		t.Fatalf("missing target image diagnostic = %v", err)
	}
	source.ReleaseManaged = false
	if err := next.ValidateTransition(original, source, target); err == nil {
		t.Fatal("custom source accepted as a certified release")
	}
	source.ReleaseManaged = true
	source.ReleaseID = "unknown"
	if err := next.ValidateTransition(original, source, target); err == nil {
		t.Fatal("unknown source release accepted")
	}
	source.ReleaseID = old.ID()
	target["ui"] = "custom/ui:v1"
	if err := next.ValidateTransition(original, source, target); err == nil {
		t.Fatal("mixed target accepted as a shipped combination")
	}
}

func TestDisablingROSDoesNotSelectNewImages(t *testing.T) {
	requested := &cost.CostManagementServiceConfig{}
	requested.Spec.Upgrade.ImagePolicy = cost.ImagePolicyFollowOperator
	effective := requested.DeepCopy()
	if err := mustCurrentBundle(t).Resolve(effective); err != nil {
		t.Fatal(err)
	}
	target := map[string]string{}
	for key, image := range ImageFields(effective) {
		if key != "ros" && key != "kruize" {
			target[key], _ = resources.ImageRef(*image)
		}
	}
	target["masu"] = target["koku"]
	source := &cost.ApplicationRevision{ReleaseID: "custom-release", Images: map[string]string{}}
	maps.Copy(source.Images, target)
	source.Images["ros"], source.Images["kruize"] = "custom/ros:v1", "custom/kruize:v1"
	if err := mustCurrentBundle(t).ValidateTransition(requested, source, target); err != nil {
		t.Fatalf("removing ROS does not automatically select any new application version: %v", err)
	}
}
