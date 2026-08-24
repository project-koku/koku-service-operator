package resources

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	costv1alpha1 "github.com/project-koku/koku-service-operator/api/v1alpha1"
)

func uiTestCfg() *costv1alpha1.CostManagementServiceConfig {
	cfg := testCfg()
	cfg.Status.DiscoveredConfig = &costv1alpha1.DiscoveredConfig{ClusterDomain: "apps.example.com"}
	cfg.Spec.UI = costv1alpha1.UIConfig{
		ReplicaCount: 1,
		OAuthProxy: costv1alpha1.OAuthProxySpec{
			Image: costv1alpha1.ImageSpec{
				Repository: "registry.redhat.io/rhceph/oauth2-proxy-rhel9",
				Tag:        "v7.6.0",
			},
			CookieExpire: "720h",
		},
		App: costv1alpha1.UIAppSpec{
			Image: costv1alpha1.ImageSpec{
				Repository: "quay.io/insights-onprem/koku-ui-onprem",
				Tag:        "2f23c646581028bd385856b6713e6bf367baf953",
			},
		},
	}
	return cfg
}

func TestUIDeploymentServiceAccount(t *testing.T) {
	dep := UIDeployment(uiTestCfg())
	spec := dep.Spec.Template.Spec
	if spec.ServiceAccountName != NameUIServiceAccount(uiTestCfg()) {
		t.Errorf("ServiceAccountName = %q, want %q", spec.ServiceAccountName, NameUIServiceAccount(uiTestCfg()))
	}
	if spec.AutomountServiceAccountToken == nil || *spec.AutomountServiceAccountToken {
		t.Errorf("AutomountServiceAccountToken = %v, want false", spec.AutomountServiceAccountToken)
	}
}

func TestUIDeployment_DefaultOAuthProxyImageWhenUnset(t *testing.T) {
	cfg := uiTestCfg()
	cfg.Spec.UI.OAuthProxy.Image = costv1alpha1.ImageSpec{}
	proxy := containerByName(t, UIDeployment(cfg).Spec.Template.Spec.Containers, "oauth-proxy")
	const want = "quay.io/oauth2-proxy/oauth2-proxy:v7.6.0"
	if proxy.Image != want {
		t.Errorf("default oauth2-proxy image = %q, want %q", proxy.Image, want)
	}
}

func TestUIDeploymentOAuthProxySecurityContext(t *testing.T) {
	dep := UIDeployment(uiTestCfg())
	proxy := containerByName(t, dep.Spec.Template.Spec.Containers, "oauth-proxy")
	sc := proxy.SecurityContext
	if sc == nil {
		t.Fatal("oauth-proxy SecurityContext is nil")
		return
	}
	// RunAsUser must be absent — restricted-v2 SCC injects the namespace UID.
	if sc.RunAsUser != nil {
		t.Errorf("oauth-proxy RunAsUser = %d; want nil (no hardcoded UID)", *sc.RunAsUser)
	}
	if sc.RunAsNonRoot == nil || !*sc.RunAsNonRoot {
		t.Error("oauth-proxy must set runAsNonRoot=true")
	}
}

func TestUIDeploymentOAuthProxyHonorsInsecureSkipVerify(t *testing.T) {
	const skipVerifyArg = "--ssl-insecure-skip-verify=true"

	cfg := uiTestCfg()
	cfg.Spec.Auth.Keycloak.TLS.InsecureSkipVerify = true
	dep := UIDeployment(cfg)
	proxy := containerByName(t, dep.Spec.Template.Spec.Containers, "oauth-proxy")
	if !slices.Contains(proxy.Args, skipVerifyArg) {
		t.Fatal("oauth-proxy missing --ssl-insecure-skip-verify=true when auth.keycloak.tls.insecureSkipVerify=true")
	}

	cfgSecure := uiTestCfg()
	cfgSecure.Spec.Auth.Keycloak.TLS.InsecureSkipVerify = false
	depSecure := UIDeployment(cfgSecure)
	proxySecure := containerByName(t, depSecure.Spec.Template.Spec.Containers, "oauth-proxy")
	if slices.Contains(proxySecure.Args, skipVerifyArg) {
		t.Fatal("oauth-proxy must not set --ssl-insecure-skip-verify when insecureSkipVerify=false")
	}
}

func TestUIDeploymentAppHasWritableNginxPaths(t *testing.T) {
	dep := UIDeployment(uiTestCfg())
	app := containerByName(t, dep.Spec.Template.Spec.Containers, "app")

	mounts := map[string]string{}
	for _, m := range app.VolumeMounts {
		mounts[m.MountPath] = m.Name
	}
	for _, path := range []string{"/var/lib/nginx/tmp", "/var/log/nginx", "/tmp", "/run"} {
		if mounts[path] == "" {
			t.Errorf("app missing VolumeMount for %s (needed with readOnlyRootFilesystem)", path)
		}
	}

	vols := map[string]bool{}
	for _, v := range dep.Spec.Template.Spec.Volumes {
		if v.EmptyDir != nil {
			vols[v.Name] = true
		}
	}
	for _, name := range []string{mounts["/var/lib/nginx/tmp"], mounts["/var/log/nginx"], mounts["/tmp"]} {
		if name == "" {
			continue
		}
		if !vols[name] {
			t.Errorf("volume %q for nginx writable path is not emptyDir", name)
		}
	}

	sc := app.SecurityContext
	if sc == nil {
		t.Error("app SecurityContext is nil")
	} else if sc.RunAsUser != nil {
		t.Errorf("app RunAsUser = %d; want nil (no hardcoded UID)", *sc.RunAsUser)
	}
}

func TestNameUIOAuthClientSecretDefaultAndOverride(t *testing.T) {
	cfg := uiTestCfg()
	if got := NameUIOAuthClientSecret(cfg); got != "cost-management-ui-oauth-client" {
		t.Errorf("default NameUIOAuthClientSecret = %q", got)
	}
	cfg.Spec.UI.OAuthClientSecretRef = corev1.LocalObjectReference{Name: "my-ui-oauth"}
	if got := NameUIOAuthClientSecret(cfg); got != "my-ui-oauth" {
		t.Errorf("override NameUIOAuthClientSecret = %q", got)
	}
}

func TestValidateUIOAuthClientSecret(t *testing.T) {
	if err := ValidateUIOAuthClientSecret(nil); err == nil {
		t.Fatal("expected error for nil secret")
	}
	empty := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "s"}, Data: map[string][]byte{}}
	if err := ValidateUIOAuthClientSecret(empty); err == nil {
		t.Fatal("expected error for missing keys")
	}
	partial := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "s"},
		Data:       map[string][]byte{"client-id": []byte("id")},
	}
	if err := ValidateUIOAuthClientSecret(partial); err == nil {
		t.Fatal("expected error for missing client-secret")
	}
	ok := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "s"},
		Data: map[string][]byte{
			"client-id":     []byte("id"),
			"client-secret": []byte("sec"),
		},
	}
	if err := ValidateUIOAuthClientSecret(ok); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestUIDeploymentProfileResources(t *testing.T) {
	tests := []struct {
		name       string
		profile    costv1alpha1.Profile
		wantCPUReq string
		wantMemReq string
		wantCPULim string
		wantMemLim string
	}{
		{"unset", "", "50m", "64Mi", "100m", "128Mi"},
		{"standard", costv1alpha1.ProfileStandard, "50m", "64Mi", "100m", "128Mi"},
		{"ha", costv1alpha1.ProfileHA, "100m", "128Mi", "200m", "256Mi"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := uiTestCfg()
			cfg.Spec.Profile = tt.profile
			dep := UIDeployment(cfg)
			for _, name := range []string{"oauth-proxy", "app"} {
				c := containerByName(t, dep.Spec.Template.Spec.Containers, name)
				assertQuantity(t, name+" cpu request", c.Resources.Requests[corev1.ResourceCPU], tt.wantCPUReq)
				assertQuantity(t, name+" memory request", c.Resources.Requests[corev1.ResourceMemory], tt.wantMemReq)
				assertQuantity(t, name+" cpu limit", c.Resources.Limits[corev1.ResourceCPU], tt.wantCPULim)
				assertQuantity(t, name+" memory limit", c.Resources.Limits[corev1.ResourceMemory], tt.wantMemLim)
			}
		})
	}
}

func TestUIDeploymentHonorsResourceOverrides(t *testing.T) {
	cfg := uiTestCfg()
	cfg.Spec.Profile = costv1alpha1.ProfileHA
	cfg.Spec.UI.OAuthProxy.Resources = corev1.ResourceRequirements{
		Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("200m"), corev1.ResourceMemory: resource.MustParse("256Mi")},
		Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("300m"), corev1.ResourceMemory: resource.MustParse("512Mi")},
	}
	cfg.Spec.UI.App.Resources = corev1.ResourceRequirements{
		Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("10m"), corev1.ResourceMemory: resource.MustParse("32Mi")},
		Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("20m"), corev1.ResourceMemory: resource.MustParse("64Mi")},
	}
	dep := UIDeployment(cfg)
	proxy := containerByName(t, dep.Spec.Template.Spec.Containers, "oauth-proxy")
	assertQuantity(t, "oauth-proxy cpu request", proxy.Resources.Requests[corev1.ResourceCPU], "200m")
	assertQuantity(t, "oauth-proxy cpu limit", proxy.Resources.Limits[corev1.ResourceCPU], "300m")
	app := containerByName(t, dep.Spec.Template.Spec.Containers, "app")
	assertQuantity(t, "app cpu request", app.Resources.Requests[corev1.ResourceCPU], "10m")
	assertQuantity(t, "app cpu limit", app.Resources.Limits[corev1.ResourceCPU], "20m")
}

func TestUIDeploymentHonorsRequestsOnlyOverride(t *testing.T) {
	cfg := uiTestCfg()
	cfg.Spec.Profile = costv1alpha1.ProfileHA
	cfg.Spec.UI.OAuthProxy.Resources = corev1.ResourceRequirements{
		Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("200m"), corev1.ResourceMemory: resource.MustParse("256Mi")},
	}
	dep := UIDeployment(cfg)
	proxy := containerByName(t, dep.Spec.Template.Spec.Containers, "oauth-proxy")
	assertQuantity(t, "oauth-proxy cpu request", proxy.Resources.Requests[corev1.ResourceCPU], "200m")
	assertQuantity(t, "oauth-proxy memory request", proxy.Resources.Requests[corev1.ResourceMemory], "256Mi")
	if len(proxy.Resources.Limits) != 0 {
		t.Errorf("oauth-proxy limits = %v, want empty (do not fill profile limits)", proxy.Resources.Limits)
	}
}

func assertQuantity(t *testing.T, label string, got resource.Quantity, want string) {
	t.Helper()
	if got.Cmp(resource.MustParse(want)) != 0 {
		t.Errorf("%s = %s, want %s", label, got.String(), want)
	}
}

func TestUICookieSecret(t *testing.T) {
	cfg := uiTestCfg()
	secret := UICookieSecret(cfg)
	if secret.Name != NameUICookieSecret(cfg) {
		t.Errorf("Name = %q, want %q", secret.Name, NameUICookieSecret(cfg))
	}
	if secret.Labels[labelComponent] != "ui" {
		t.Errorf("component label = %q", secret.Labels[labelComponent])
	}
	val := secret.StringData["session-secret"]
	if val == "" {
		t.Error("session-secret must be non-empty")
	}
}

func TestUINginxConfigMap(t *testing.T) {
	cfg := uiTestCfg()
	cm := UINginxConfigMap(cfg)
	nginx := cm.Data["nginx.conf"]
	wantProxy := fmt.Sprintf("proxy_pass http://%s:80", NameEnvoy(cfg))
	if !strings.Contains(nginx, wantProxy) {
		t.Errorf("nginx config missing %q", wantProxy)
	}
	if !strings.Contains(nginx, "location /api/") {
		t.Error("nginx config missing /api/ location")
	}
}

func TestUIService(t *testing.T) {
	cfg := uiTestCfg()
	svc := UIService(cfg)
	if svc.Name != NameUI(cfg) {
		t.Errorf("Name = %q", svc.Name)
	}
	if len(svc.Spec.Ports) != 1 {
		t.Fatalf("ports = %+v", svc.Spec.Ports)
	}
	port := svc.Spec.Ports[0]
	if port.Name != "https" || port.Port != uiProxyPort {
		t.Errorf("port = %+v, want https/%d", port, uiProxyPort)
	}
	wantAnnot := NameUITLSSecret(cfg)
	if got := svc.Annotations["service.beta.openshift.io/serving-cert-secret-name"]; got != wantAnnot {
		t.Errorf("serving-cert annotation = %q, want %q", got, wantAnnot)
	}
}

func TestUIRoute_NilWithoutClusterDomain(t *testing.T) {
	cfg := testCfg()
	if UIRoute(cfg) != nil {
		t.Error("expected nil Route when cluster domain is missing")
	}
}

func TestUIRoute_GlobalClusterDomainFallback(t *testing.T) {
	cfg := uiTestCfg()
	cfg.Status.DiscoveredConfig = nil
	cfg.Spec.Global.ClusterDomain = "apps.from-spec.example.com"

	route := UIRoute(cfg)
	if route == nil {
		t.Fatal("expected Route when spec.global.clusterDomain is set")
	}
	wantHost := fmt.Sprintf("%s-ui-%s.%s", cfg.Name, cfg.Namespace, "apps.from-spec.example.com")
	host, found, err := unstructured.NestedString(route.Object, "spec", "host")
	if err != nil || !found {
		t.Fatalf("spec.host missing: found=%v err=%v", found, err)
	}
	if host != wantHost {
		t.Errorf("spec.host = %q, want %q", host, wantHost)
	}
	term, _, _ := unstructured.NestedString(route.Object, "spec", "tls", "termination")
	if term != "passthrough" {
		t.Errorf("tls.termination = %q, want passthrough", term)
	}

	cl := ConsoleLink(cfg)
	href, found, err := unstructured.NestedString(cl.Object, "spec", "href")
	if err != nil || !found {
		t.Fatalf("ConsoleLink spec.href missing: found=%v err=%v", found, err)
	}
	wantHref := fmt.Sprintf("https://%s/", wantHost)
	if href != wantHref {
		t.Errorf("ConsoleLink href = %q, want %q", href, wantHref)
	}

	wantRedirect := "--redirect-url=https://" + wantHost + "/oauth2/callback"
	proxy := containerByName(t, UIDeployment(cfg).Spec.Template.Spec.Containers, "oauth-proxy")
	if !slices.Contains(proxy.Args, wantRedirect) {
		t.Errorf("oauth2-proxy args missing %q\ngot %q", wantRedirect, proxy.Args)
	}

	// Discovered domain wins over spec.global when both are set.
	cfg.Status.DiscoveredConfig = &costv1alpha1.DiscoveredConfig{ClusterDomain: "apps.discovered.example.com"}
	route = UIRoute(cfg)
	host, _, _ = unstructured.NestedString(route.Object, "spec", "host")
	wantDiscovered := fmt.Sprintf("%s-ui-%s.%s", cfg.Name, cfg.Namespace, "apps.discovered.example.com")
	if host != wantDiscovered {
		t.Errorf("discovered must win: spec.host = %q, want %q", host, wantDiscovered)
	}
}

func TestUIRoute_Spec(t *testing.T) {
	cfg := uiTestCfg()
	route := UIRoute(cfg)
	if route == nil {
		t.Fatal("expected Route when cluster domain is set")
		return
	}
	if route.GroupVersionKind() != routeGVK {
		t.Errorf("GVK = %v, want %v", route.GroupVersionKind(), routeGVK)
	}
	wantHost := fmt.Sprintf("%s-ui-%s.%s", cfg.Name, cfg.Namespace, cfg.Status.DiscoveredConfig.ClusterDomain)
	host, _, _ := unstructured.NestedString(route.Object, "spec", "host")
	if host != wantHost {
		t.Errorf("spec.host = %q, want %q", host, wantHost)
	}
	svc, _, _ := unstructured.NestedString(route.Object, "spec", "to", "name")
	if svc != NameUI(cfg) {
		t.Errorf("spec.to.name = %q, want %q", svc, NameUI(cfg))
	}
	targetPort, _, _ := unstructured.NestedString(route.Object, "spec", "port", "targetPort")
	if targetPort != "https" {
		t.Errorf("spec.port.targetPort = %q, want https", targetPort)
	}
	term, _, _ := unstructured.NestedString(route.Object, "spec", "tls", "termination")
	if term != "passthrough" {
		t.Errorf("tls.termination = %q, want passthrough", term)
	}
}

func TestUIDeploymentUsesOAuthClientSecretRef(t *testing.T) {
	cfg := uiTestCfg()
	cfg.Spec.UI.OAuthClientSecretRef = corev1.LocalObjectReference{Name: "custom-oauth"}
	dep := UIDeployment(cfg)
	proxy := containerByName(t, dep.Spec.Template.Spec.Containers, "oauth-proxy")
	foundID, foundSecret := false, false
	for _, e := range proxy.Env {
		if e.ValueFrom == nil || e.ValueFrom.SecretKeyRef == nil {
			continue
		}
		if e.Name == "OAUTH2_PROXY_CLIENT_ID" {
			foundID = true
			if e.ValueFrom.SecretKeyRef.Name != "custom-oauth" || e.ValueFrom.SecretKeyRef.Key != "client-id" {
				t.Errorf("CLIENT_ID secretRef = %s/%s", e.ValueFrom.SecretKeyRef.Name, e.ValueFrom.SecretKeyRef.Key)
			}
		}
		if e.Name == "OAUTH2_PROXY_CLIENT_SECRET" {
			foundSecret = true
			if e.ValueFrom.SecretKeyRef.Name != "custom-oauth" {
				t.Errorf("CLIENT_SECRET secret name = %s", e.ValueFrom.SecretKeyRef.Name)
			}
		}
	}
	if !foundID || !foundSecret {
		t.Fatal("oauth-proxy missing OAUTH2_PROXY_CLIENT_ID/SECRET envFrom")
	}
}

func containerByName(t *testing.T, containers []corev1.Container, name string) corev1.Container {
	t.Helper()
	for i := range containers {
		if containers[i].Name == name {
			return containers[i]
		}
	}
	t.Fatalf("container %q not found", name)
	return corev1.Container{}
}
