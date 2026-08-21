package resources

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	costv1alpha1 "github.com/project-koku/koku-service-operator/api/v1alpha1"
)

// assertValidYAMLWithStringAudiences unmarshals the full EnvoyYAML output,
// walks the YAML to find the audiences list, and asserts every entry is a
// plain string — not a map (which would indicate YAML injection turned a
// list entry like "evil:" into {evil: nil}).
func assertValidYAMLWithStringAudiences(t *testing.T, yamlStr string) {
	t.Helper()

	var doc map[string]any
	if err := yaml.Unmarshal([]byte(yamlStr), &doc); err != nil {
		t.Fatalf("EnvoyYAML output is not valid YAML: %v", err)
	}

	// Walk: static_resources → listeners[0] → filter_chains[0] → filters[0]
	//       → typed_config → http_filters → (jwt_authn) → typed_config
	//       → providers → keycloak → audiences
	audiences := yamlWalk(doc,
		"static_resources", "listeners", 0, "filter_chains", 0,
		"filters", 0, "typed_config", "http_filters",
	)
	if audiences == nil {
		t.Fatal("could not walk YAML to http_filters")
		return
	}

	filters, ok := audiences.([]any)
	if !ok {
		t.Fatalf("http_filters is %T, want []any", audiences)
	}

	for _, f := range filters {
		fm, ok := f.(map[string]any)
		if !ok {
			continue
		}
		tc, ok := fm["typed_config"].(map[string]any)
		if !ok {
			continue
		}
		providers, ok := tc["providers"].(map[string]any)
		if !ok {
			continue
		}
		kc, ok := providers["keycloak"].(map[string]any)
		if !ok {
			continue
		}
		audList, ok := kc["audiences"].([]any)
		if !ok {
			t.Fatalf("audiences is %T, want []any", kc["audiences"])
		}
		for i, a := range audList {
			if _, ok := a.(string); !ok {
				t.Errorf("audiences[%d] is %T (%v), want string — YAML injection turned a list entry into a map", i, a, a)
			}
		}
		return
	}
	t.Fatal("could not find keycloak provider with audiences in YAML")
}

// yamlWalk navigates a nested map/slice structure by keys (string) and
// indices (int). Returns nil if any step fails.
func yamlWalk(v any, path ...any) any {
	for _, step := range path {
		switch s := step.(type) {
		case string:
			m, ok := v.(map[string]any)
			if !ok {
				return nil
			}
			v = m[s]
		case int:
			sl, ok := v.([]any)
			if !ok || s >= len(sl) {
				return nil
			}
			v = sl[s]
		default:
			return nil
		}
	}
	return v
}

func testCfg() *costv1alpha1.CostManagementServiceConfig {
	return &costv1alpha1.CostManagementServiceConfig{
		ObjectMeta: metav1.ObjectMeta{Name: "cost-management", Namespace: "cost-onprem"},
		Spec: costv1alpha1.CostManagementServiceConfigSpec{
			Auth: costv1alpha1.AuthConfig{
				Keycloak: costv1alpha1.KeycloakSpec{
					URL:       "https://keycloak.keycloak.svc.cluster.local",
					Realm:     "kubernetes",
					Audiences: []string{"cost-management-operator", "cost-management-ui"},
				},
				Envoy: costv1alpha1.EnvoySpec{
					Replicas: 2,
					Image: costv1alpha1.ImageSpec{
						Repository: "registry.redhat.io/openshift-service-mesh/proxyv2-rhel9",
						Tag:        "2.6",
					},
				},
			},
		},
	}
}

func TestKeycloakIssuerAndJWKS(t *testing.T) {
	cfg := testCfg()
	wantIssuer := "https://keycloak.keycloak.svc.cluster.local/realms/kubernetes"
	if got := KeycloakIssuerURL(cfg); got != wantIssuer {
		t.Errorf("KeycloakIssuerURL = %q, want %q", got, wantIssuer)
	}
	wantJWKS := wantIssuer + "/protocol/openid-connect/certs"
	if got := KeycloakJWKSURL(cfg); got != wantJWKS {
		t.Errorf("KeycloakJWKSURL = %q, want %q", got, wantJWKS)
	}
}

func TestKeycloakIssuerURLOverrideKeepsInClusterJWKS(t *testing.T) {
	cfg := testCfg()
	cfg.Spec.Auth.Keycloak.URL = "http://keycloak-service.keycloak.svc.cluster.local:8080"
	cfg.Spec.Auth.Keycloak.IssuerURL = "https://keycloak.apps.example.com"
	cfg.Spec.Auth.Keycloak.Realm = "kubernetes"

	wantIssuer := "https://keycloak.apps.example.com/realms/kubernetes"
	if got := KeycloakIssuerURL(cfg); got != wantIssuer {
		t.Errorf("KeycloakIssuerURL = %q, want %q", got, wantIssuer)
	}
	wantJWKS := "http://keycloak-service.keycloak.svc.cluster.local:8080/realms/kubernetes/protocol/openid-connect/certs"
	if got := KeycloakJWKSURL(cfg); got != wantJWKS {
		t.Errorf("KeycloakJWKSURL = %q, want %q", got, wantJWKS)
	}

	yaml := EnvoyYAML(cfg)
	if !strings.Contains(yaml, "issuer: "+wantIssuer) {
		t.Errorf("EnvoyYAML missing issuer %q", wantIssuer)
	}
	if !strings.Contains(yaml, "uri: "+wantJWKS) {
		t.Errorf("EnvoyYAML missing JWKS uri %q", wantJWKS)
	}
	// JWKS cluster must target the in-cluster Service, not the public hostname.
	if !strings.Contains(yaml, "address: keycloak-service.keycloak.svc.cluster.local") {
		t.Error("EnvoyYAML JWKS cluster should use in-cluster Keycloak Service host")
	}
	if strings.Contains(yaml, "transport_socket:") {
		t.Error("in-cluster http JWKS should not enable upstream TLS")
	}

	// Full issuer override (includes /realms/) is used as-is.
	cfg.Spec.Auth.Keycloak.IssuerURL = "https://keycloak.apps.example.com/realms/custom"
	if got := KeycloakIssuerURL(cfg); got != "https://keycloak.apps.example.com/realms/custom" {
		t.Errorf("full IssuerURL = %q", got)
	}
}

func TestKeycloakDefaults(t *testing.T) {
	cfg := &costv1alpha1.CostManagementServiceConfig{
		ObjectMeta: metav1.ObjectMeta{Name: "cm", Namespace: "ns"},
	}
	if got := KeycloakURL(cfg); got != defaultKeycloakURL {
		t.Errorf("KeycloakURL default = %q, want %q", got, defaultKeycloakURL)
	}
	if got := KeycloakRealm(cfg); got != defaultKeycloakRealm {
		t.Errorf("KeycloakRealm default = %q, want %q", got, defaultKeycloakRealm)
	}
	aud := KeycloakAudiences(cfg)
	if len(aud) != 2 || aud[0] != "cost-management-operator" {
		t.Errorf("KeycloakAudiences default = %v", aud)
	}
}

// envoyROSMarkers are Envoy YAML fragments present only when ROS is enabled.
// Keep the omit-ROS tests in sync with these so Cost-only configs cannot leak
// partial ROS routes/clusters.
var envoyROSMarkers = []string{
	"name: ros-api-backend",
	"cluster: ros-api-backend",
	"cluster_name: ros-api-backend",
	"/api/cost-management/v1/recommendations/openshift",
	"__ROS_ROUTE__",
	"__ROS_CLUSTER__",
}

func TestEnvoyYAMLContainsIssuerAudiencesAndKokuCluster(t *testing.T) {
	cfg := testCfg()
	enabled := true
	cfg.Spec.ROS.Enabled = &enabled
	yaml := EnvoyYAML(cfg)

	checks := []string{
		"issuer: https://keycloak.keycloak.svc.cluster.local/realms/kubernetes",
		"uri: https://keycloak.keycloak.svc.cluster.local/realms/kubernetes/protocol/openid-connect/certs",
		"- cost-management-operator",
		"- cost-management-ui",
		"address: cost-management-koku-api.cost-onprem.svc.cluster.local",
		"port_value: 8000",
		"X-Rh-Identity",
		"X-Bearer-Token",
		"address: keycloak.keycloak.svc.cluster.local",
		"port_value: 443",
		"transport_socket:",
	}
	for _, want := range checks {
		if !strings.Contains(yaml, want) {
			t.Errorf("EnvoyYAML missing %q", want)
		}
	}
	for _, marker := range envoyROSMarkers {
		if strings.HasPrefix(marker, "__") {
			continue // unsubstituted tokens must not appear when ROS is on
		}
		if !strings.Contains(yaml, marker) {
			t.Errorf("EnvoyYAML (ROS enabled) missing ROS marker %q", marker)
		}
	}
	// Backend ports must match ROS Service (8000) and RBAC Service (8080).
	rosIdx := strings.Index(yaml, "name: ros-api-backend")
	rbacIdx := strings.Index(yaml, "name: rbac-api-backend")
	kokuIdx := strings.Index(yaml, "name: koku-api-backend")
	if rosIdx < 0 || rbacIdx < 0 || kokuIdx < 0 {
		t.Fatal("missing ros/rbac/koku backend clusters")
	}
	rosBlock := yaml[rosIdx:kokuIdx]
	if !strings.Contains(rosBlock, "port_value: 8000") {
		t.Error("ros-api-backend should use port 8000")
	}
	rbacEnd := strings.Index(yaml[rbacIdx+1:], "\n  - name:")
	rbacBlock := yaml[rbacIdx:]
	if rbacEnd >= 0 {
		rbacBlock = yaml[rbacIdx : rbacIdx+1+rbacEnd]
	}
	if !strings.Contains(rbacBlock, "port_value: 8080") {
		t.Error("rbac-api-backend should use port 8080")
	}
	for _, tok := range []string{"__HTTP_PORT__", "__ISSUER__", "__LUA__", "__KOKU_HOST__", "__KC_TLS__", "__ROS_ROUTE__", "__ROS_CLUSTER__"} {
		if strings.Contains(yaml, tok) {
			t.Errorf("EnvoyYAML left unsubstituted token %q", tok)
		}
	}
}

func TestEnvoyYAMLHTTPKeycloakOmitsTLS(t *testing.T) {
	cfg := testCfg()
	cfg.Spec.Auth.Keycloak.URL = "http://keycloak.keycloak.svc.cluster.local:8080"
	yaml := EnvoyYAML(cfg)
	if strings.Contains(yaml, "transport_socket:") {
		t.Error("expected no TLS transport_socket for http:// Keycloak")
	}
	if !strings.Contains(yaml, "port_value: 8080") {
		t.Error("expected Keycloak port 8080")
	}
}

func assertEnvoyYAMLOmitsROS(t *testing.T, yaml string, path string) {
	t.Helper()
	for _, marker := range envoyROSMarkers {
		if strings.Contains(yaml, marker) {
			t.Errorf("EnvoyYAML should omit ROS marker %q when ros.enabled is %s", marker, path)
		}
	}
	if !strings.Contains(yaml, "name: koku-api-backend") {
		t.Errorf("koku-api-backend must still be present when ros.enabled is %s", path)
	}
}

func TestEnvoyYAMLOmitsROSWhenDisabled(t *testing.T) {
	cfg := testCfg()
	// nil / omitted Enabled also means off (beta default); exercise explicit false.
	disabled := false
	cfg.Spec.ROS.Enabled = &disabled
	assertEnvoyYAMLOmitsROS(t, EnvoyYAML(cfg), "explicitly false")

	cfgNil := testCfg()
	assertEnvoyYAMLOmitsROS(t, EnvoyYAML(cfgNil), "omitted (default false)")
}

func TestEnvoyYAMLParsesForROSEnabledAndDisabled(t *testing.T) {
	for _, enabled := range []bool{true, false} {
		cfg := testCfg()
		e := enabled
		cfg.Spec.ROS.Enabled = &e
		out := EnvoyYAML(cfg)
		assertValidYAMLWithStringAudiences(t, out)
	}
}

func TestEnvoyResourceNames(t *testing.T) {
	cfg := testCfg()
	cm := EnvoyConfigMap(cfg)
	if cm.Name != "cost-management-gateway-envoy-config" {
		t.Errorf("ConfigMap name = %q", cm.Name)
	}
	svc := EnvoyService(cfg)
	if svc.Name != "cost-management-gateway" {
		t.Errorf("Service name = %q", svc.Name)
	}
	if len(svc.Spec.Ports) != 2 || svc.Spec.Ports[0].Port != 80 {
		t.Errorf("Service ports = %+v", svc.Spec.Ports)
	}
	d := EnvoyDeployment(cfg)
	if d.Name != "cost-management-gateway" {
		t.Errorf("Deployment name = %q", d.Name)
	}
	if d.Spec.Template.Spec.ServiceAccountName != NameGatewayServiceAccount(cfg) {
		t.Errorf("ServiceAccountName = %q, want %q", d.Spec.Template.Spec.ServiceAccountName, NameGatewayServiceAccount(cfg))
	}
	if d.Spec.Template.Spec.AutomountServiceAccountToken == nil || *d.Spec.Template.Spec.AutomountServiceAccountToken {
		t.Errorf("AutomountServiceAccountToken = %v, want false", d.Spec.Template.Spec.AutomountServiceAccountToken)
	}
	if d.Spec.Template.Spec.Containers[0].Image != "registry.redhat.io/openshift-service-mesh/proxyv2-rhel9:2.6" {
		t.Errorf("image = %q", d.Spec.Template.Spec.Containers[0].Image)
	}
	if len(d.Spec.Template.Spec.InitContainers) != 1 || d.Spec.Template.Spec.InitContainers[0].Name != "prepare-ca-bundle" {
		t.Error("expected CA combine init container")
	}
}

// TestEnvoyDeploymentHasConfigHash verifies that EnvoyDeployment includes a
// content hash of the ConfigMap in the pod template annotations. Without this,
// Envoy pods are never restarted when the ConfigMap changes (e.g., OIDC URL
// update), so the gateway keeps running with stale JWT configuration.
func TestEnvoyDeploymentHasConfigHash(t *testing.T) {
	cfg := testCfg()
	cm := EnvoyConfigMap(cfg)
	dep := EnvoyDeployment(cfg)

	const hashAnnotation = "koku.costmanagement.io/envoy-config-hash"
	hash, ok := dep.Spec.Template.Annotations[hashAnnotation]
	if !ok {
		t.Fatalf("EnvoyDeployment pod template missing annotation %q — "+
			"ConfigMap changes will not trigger pod restarts", hashAnnotation)
	}
	if hash == "" {
		t.Fatalf("annotation %q is empty", hashAnnotation)
	}

	// Changing the ConfigMap content must change the hash.
	cfg2 := testCfg()
	cfg2.Spec.Auth.Keycloak.URL = "https://other-keycloak.example.com"
	cm2 := EnvoyConfigMap(cfg2)
	dep2 := EnvoyDeployment(cfg2)

	if cm.Data["envoy.yaml"] == cm2.Data["envoy.yaml"] {
		t.Skip("test configs produced identical ConfigMap content — adjust testCfg()")
	}

	hash2 := dep2.Spec.Template.Annotations[hashAnnotation]
	if hash == hash2 {
		t.Errorf("hash did not change when ConfigMap content changed: both = %q", hash)
	}
}

// TestEnvoyDeploymentMountsKeycloakCACert verifies that when
// auth.keycloak.tls.caCertSecretName is set, the Envoy Deployment mounts
// that Secret as an additional CA source so Envoy can verify the Keycloak
// Route certificate (router CA, not the service CA).
func TestEnvoyDeploymentMountsKeycloakCACert(t *testing.T) {
	cfg := testCfg()
	cfg.Spec.Auth.Keycloak.TLS.CACertSecretName = "my-router-ca"

	dep := EnvoyDeployment(cfg)

	if !volumeProjectsSecret(dep.Spec.Template.Spec.Volumes, "my-router-ca", "keycloak-ca.crt") {
		t.Errorf("EnvoyDeployment missing projected volume for keycloak caCertSecretName=%q — "+
			"Envoy will fail to verify Keycloak Route certificates", "my-router-ca")
	}
}

func TestEnvoyInitMountsKeycloakCAAtCaExtra(t *testing.T) {
	cfg := testCfg()
	cfg.Spec.Auth.Keycloak.TLS.CACertSecretName = "my-router-ca"
	dep := EnvoyDeployment(cfg)
	if len(dep.Spec.Template.Spec.InitContainers) == 0 {
		t.Fatal("missing prepare-ca-bundle init container")
	}
	init := dep.Spec.Template.Spec.InitContainers[0]
	found := false
	for _, m := range init.VolumeMounts {
		if m.MountPath == "/ca-extra" {
			found = true
			if m.Name != caExtraVolumeName {
				t.Errorf("ca-extra mount name = %q, want %q", m.Name, caExtraVolumeName)
			}
		}
	}
	if !found {
		t.Fatal("prepare-ca-bundle must mount /ca-extra so combine-ca.sh can merge user CAs")
	}
}

func TestEnvoyInitOmitsCaExtraWithoutCASecret(t *testing.T) {
	cfg := testCfg()
	dep := EnvoyDeployment(cfg)
	init := dep.Spec.Template.Spec.InitContainers[0]
	for _, m := range init.VolumeMounts {
		if m.MountPath == "/ca-extra" {
			t.Fatal("prepare-ca-bundle must not mount /ca-extra when caCertSecretName is empty")
		}
	}
}

func TestEnvoyYAMLSkipVerifyOmitsSANMatch(t *testing.T) {
	cfg := testCfg()
	cfg.Spec.Auth.Keycloak.URL = "https://keycloak.apps.example.com"
	cfg.Spec.Auth.Keycloak.TLS.InsecureSkipVerify = true
	y := EnvoyYAML(cfg)

	var doc map[string]any
	if err := yaml.Unmarshal([]byte(y), &doc); err != nil {
		t.Fatalf("EnvoyYAML is not valid YAML: %v", err)
	}
	vc := keycloakJWKSValidationContext(t, doc)
	if got, _ := vc["trust_chain_verification"].(string); got != "ACCEPT_UNTRUSTED" {
		t.Fatalf("trust_chain_verification = %v, want ACCEPT_UNTRUSTED", vc["trust_chain_verification"])
	}
	if _, ok := vc["match_typed_subject_alt_names"]; ok {
		t.Fatal("SAN match must not be required when skipping verify")
	}
}

func TestEnvoyYAMLCASecretWinsOverSkipVerify(t *testing.T) {
	cfg := testCfg()
	cfg.Spec.Auth.Keycloak.URL = "https://keycloak.apps.example.com"
	cfg.Spec.Auth.Keycloak.TLS.InsecureSkipVerify = true
	cfg.Spec.Auth.Keycloak.TLS.CACertSecretName = "my-router-ca"
	y := EnvoyYAML(cfg)

	var doc map[string]any
	if err := yaml.Unmarshal([]byte(y), &doc); err != nil {
		t.Fatalf("EnvoyYAML is not valid YAML: %v", err)
	}
	vc := keycloakJWKSValidationContext(t, doc)
	if _, ok := vc["trust_chain_verification"]; ok {
		t.Fatal("caCertSecretName must win over insecureSkipVerify")
	}
	trusted, ok := vc["trusted_ca"].(map[string]any)
	if !ok {
		t.Fatalf("expected trusted_ca map, got %T", vc["trusted_ca"])
	}
	if trusted["filename"] != "/etc/ca-certificates/ca-bundle.crt" {
		t.Fatalf("trusted_ca.filename = %v", trusted["filename"])
	}
}

func keycloakJWKSValidationContext(t *testing.T, doc map[string]any) map[string]any {
	t.Helper()
	clusters, ok := yamlWalk(doc, "static_resources", "clusters").([]any)
	if !ok {
		t.Fatalf("clusters is %T", yamlWalk(doc, "static_resources", "clusters"))
	}
	var cluster map[string]any
	for _, c := range clusters {
		m, ok := c.(map[string]any)
		if ok && m["name"] == "keycloak_jwks" {
			cluster = m
			break
		}
	}
	if cluster == nil {
		t.Fatal("keycloak_jwks cluster not found")
	}
	vc := yamlWalk(cluster, "transport_socket", "typed_config", "common_tls_context", "validation_context")
	m, ok := vc.(map[string]any)
	if !ok {
		t.Fatalf("validation_context is %T (%v)", vc, vc)
	}
	return m
}

// TestEnvoyYAMLRejectsInjectedAudience verifies that audience values containing
// control characters cannot inject YAML structure into Envoy's JWT filter config.
// Without escaping, a line-breaking control character breaks out of the audience
// list and the injected content becomes new YAML keys — which could override the
// remote_jwks endpoint and route token validation to an attacker-controlled server.
//
// Best practice is structural YAML generation (not string templates); this test
// guards the current escape-at-interpolation approach as defense-in-depth.
func TestEnvoyYAMLRejectsInjectedAudience(t *testing.T) {
	cases := []struct {
		name    string
		payload string
		banned  string
	}{
		{
			name:    "newline",
			payload: "evil\nremote_jwks:\n  http_uri:\n    uri: http://attacker.example.com/jwks",
			banned:  "\nremote_jwks:",
		},
		{
			name:    "carriage-return",
			payload: "evil\rremote_jwks:\r  http_uri:\r    uri: http://attacker.example.com/jwks",
			banned:  "\rremote_jwks:",
		},
		{
			name:    "nul-byte",
			payload: "evil\x00remote_jwks:\x00  http_uri:\x00    uri: http://attacker.example.com/jwks",
			banned:  "\x00remote_jwks:",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := testCfg()
			cfg.Spec.Auth.Keycloak.Audiences = []string{"legit-audience", tc.payload}
			out := EnvoyYAML(cfg)
			if strings.Contains(out, tc.banned) {
				t.Errorf("audience injection succeeded via %s: bare 'remote_jwks:' key in Envoy YAML", tc.name)
			}
			assertValidYAMLWithStringAudiences(t, out)
		})
	}
}

// TestEnvoyYAMLRejectsInjectedIssuer verifies the issuer URL is YAML-escaped.
// A newline in issuerURL injects structure into the JWT provider config block.
func TestEnvoyYAMLRejectsInjectedIssuer(t *testing.T) {
	cfg := testCfg()
	cfg.Spec.Auth.Keycloak.IssuerURL = "https://legit.example.com\nremote_jwks:\n  http_uri:\n    uri: http://attacker.example.com/jwks"

	out := EnvoyYAML(cfg)

	if strings.Contains(out, "\nremote_jwks:") {
		t.Error("issuer injection succeeded: bare 'remote_jwks:' key injected into Envoy YAML")
	}
	assertValidYAMLWithStringAudiences(t, out)
}

// TestEnvoyYAMLRejectsInjectedJWKSURI verifies the JWKS URI is YAML-escaped.
// The URI appears as a plain scalar in the remote_jwks http_uri block; a newline
// in auth.keycloak.url could inject arbitrary YAML keys at that indentation level.
func TestEnvoyYAMLRejectsInjectedJWKSURI(t *testing.T) {
	cfg := testCfg()
	cfg.Spec.Auth.Keycloak.URL = "https://keycloak.example.com\ncluster: attacker-controlled\naddress:"

	out := EnvoyYAML(cfg)

	if strings.Contains(out, "\ncluster: attacker-controlled") {
		t.Error("JWKS URI injection succeeded: bare YAML key injected via keycloak.url")
	}
	assertValidYAMLWithStringAudiences(t, out)
}

// TestEnvoyYAMLRejectsInjectedKCHost verifies the Keycloak cluster host is YAML-escaped.
// The extracted hostname appears in socket_address.address and in the TLS block's
// sni/match fields; a newline in auth.keycloak.url that fails url.Parse triggers the
// TrimPrefix fallback, which preserves the raw bytes — including any embedded newline.
func TestEnvoyYAMLRejectsInjectedKCHost(t *testing.T) {
	cfg := testCfg()
	// A literal \n causes url.Parse to return an error; keycloakHostPort() falls
	// back to strings.TrimPrefix, which does not URL-decode and returns the raw
	// string (including the newline) as kcHost.
	cfg.Spec.Auth.Keycloak.URL = "https://keycloak.example.com\ntype: LOGICAL_DNS\n"

	out := EnvoyYAML(cfg)

	if strings.Contains(out, "\ntype: LOGICAL_DNS") {
		t.Error("KC_HOST injection succeeded: bare YAML key injected via keycloak.url hostname")
	}
	assertValidYAMLWithStringAudiences(t, out)
}

// TestEnvoyYAMLEscapesColonSpace verifies that a colon+space or trailing colon
// in any CR-derived scalar is quoted. Without quoting, ": " or a bare ":" at
// end-of-value creates a YAML mapping-value indicator — silently turning a list
// entry into a one-key map, or causing a hard parse error in a scalar position.
// A trailing colon is equally dangerous because the template places a newline
// immediately after every interpolated value (yaml-cpp's EndScalar() trigger).
func TestEnvoyYAMLEscapesColonSpace(t *testing.T) {
	cases := []struct {
		name     string
		audience string
		banned   string
	}{
		{
			name:     "colon-space",
			audience: "evil: value",
			banned:   "\n                        - evil: value\n",
		},
		{
			name:     "trailing-colon",
			audience: "evil:",
			banned:   "\n                        - evil:\n",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := testCfg()
			cfg.Spec.Auth.Keycloak.Audiences = []string{tc.audience}
			out := EnvoyYAML(cfg)
			if strings.Contains(out, tc.banned) {
				t.Errorf("audience %q appeared as unquoted plain scalar in YAML", tc.audience)
			}
			assertValidYAMLWithStringAudiences(t, out)
		})
	}
}
