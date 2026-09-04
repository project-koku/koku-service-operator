package resources

import (
	"crypto/sha256"
	"fmt"
	"net/url"
	"strconv"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	costv1alpha1 "github.com/project-koku/koku-service-operator/api/v1alpha1"
)

const (
	envoyHTTPPort  int32 = 9080
	envoyAdminPort int32 = 9901
	envoyComponent       = "gateway"

	defaultKeycloakRealm = "kubernetes"
)

// KeycloakURL returns the Keycloak base URL from the CR, with trailing slashes
// trimmed. Empty when spec.auth.keycloak.url is unset or whitespace-only.
// The operator does not auto-detect Keycloak.
func KeycloakURL(cfg *costv1alpha1.CostManagementServiceConfig) string {
	if u := strings.TrimSpace(cfg.Spec.Auth.Keycloak.URL); u != "" {
		return strings.TrimRight(u, "/")
	}
	return ""
}

// KeycloakRealm returns the realm name (default kubernetes).
func KeycloakRealm(cfg *costv1alpha1.CostManagementServiceConfig) string {
	if r := strings.TrimSpace(cfg.Spec.Auth.Keycloak.Realm); r != "" {
		return r
	}
	return defaultKeycloakRealm
}

// KeycloakIssuerURL is the JWT iss Envoy validates.
// Prefer spec.auth.keycloak.issuerURL when set (RHBK public hostname); otherwise
// derive from url + realm. JWKS still uses KeycloakURL so in-cluster fetch works.
func KeycloakIssuerURL(cfg *costv1alpha1.CostManagementServiceConfig) string {
	if iss := strings.TrimSpace(cfg.Spec.Auth.Keycloak.IssuerURL); iss != "" {
		iss = strings.TrimRight(iss, "/")
		// Allow either a full issuer (.../realms/<realm>) or a Keycloak base URL.
		if strings.Contains(iss, "/realms/") {
			return iss
		}
		return iss + "/realms/" + KeycloakRealm(cfg)
	}
	base := KeycloakURL(cfg)
	if base == "" {
		return ""
	}
	return base + "/realms/" + KeycloakRealm(cfg)
}

// KeycloakJWKSURL is the OIDC JWKS endpoint used by Envoy's remote_jwks fetch.
// Always derived from url (not issuerURL) so JWKS can stay on the in-cluster Service
// while iss matches the public RHBK hostname.
func KeycloakJWKSURL(cfg *costv1alpha1.CostManagementServiceConfig) string {
	base := KeycloakURL(cfg)
	if base == "" {
		return ""
	}
	return base + "/realms/" + KeycloakRealm(cfg) + "/protocol/openid-connect/certs"
}

// KeycloakAudiences returns JWT audiences (CR defaults apply via kubebuilder when empty).
func KeycloakAudiences(cfg *costv1alpha1.CostManagementServiceConfig) []string {
	if len(cfg.Spec.Auth.Keycloak.Audiences) > 0 {
		return cfg.Spec.Auth.Keycloak.Audiences
	}
	return []string{"cost-management-operator", "cost-management-ui"}
}

// keycloakHostPort returns host and port for the Envoy JWKS cluster.
func keycloakHostPort(cfg *costv1alpha1.CostManagementServiceConfig) (host string, port int32, useTLS bool) {
	raw := KeycloakURL(cfg)
	if raw == "" {
		return "", 0, false
	}
	useTLS = strings.HasPrefix(raw, "https://")
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		host = strings.TrimPrefix(strings.TrimPrefix(raw, "https://"), "http://")
		if useTLS {
			return host, 443, true
		}
		return host, 80, false
	}
	host = u.Hostname()
	if u.Port() != "" {
		p, err := strconv.ParseInt(u.Port(), 10, 32)
		if err == nil {
			return host, int32(p), useTLS
		}
	}
	if useTLS {
		return host, 443, true
	}
	return host, 80, false
}

func serviceFQDN(name, namespace string) string {
	return name + "." + namespace + ".svc.cluster.local"
}

// envoyConfigHash returns a short SHA-256 digest of the rendered envoy.yaml.
// Embedded in the pod template so ConfigMap changes trigger a rolling restart.
func envoyConfigHash(cfg *costv1alpha1.CostManagementServiceConfig) string {
	h := sha256.Sum256([]byte(EnvoyYAML(cfg)))
	return fmt.Sprintf("%x", h[:8])
}

// EnvoyConfigMap builds the ConfigMap containing envoy.yaml.
func EnvoyConfigMap(cfg *costv1alpha1.CostManagementServiceConfig) *corev1.ConfigMap {
	return &corev1.ConfigMap{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "ConfigMap"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      NameEnvoyConfigMap(cfg),
			Namespace: cfg.Namespace,
			Labels:    Labels(cfg, envoyComponent),
		},
		Data: map[string]string{
			"envoy.yaml": EnvoyYAML(cfg),
		},
	}
}

// EnvoyService exposes the gateway HTTP and admin ports.
func EnvoyService(cfg *costv1alpha1.CostManagementServiceConfig) *corev1.Service {
	return &corev1.Service{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Service"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      NameEnvoy(cfg),
			Namespace: cfg.Namespace,
			Labels:    Labels(cfg, envoyComponent),
		},
		Spec: corev1.ServiceSpec{
			Selector: SelectorLabels(cfg, envoyComponent),
			Ports: []corev1.ServicePort{
				{Name: "http", Port: 80, TargetPort: intstr.FromString("http"), Protocol: corev1.ProtocolTCP},
				{Name: "admin", Port: envoyAdminPort, TargetPort: intstr.FromString("admin"), Protocol: corev1.ProtocolTCP},
			},
		},
	}
}

// EnvoyDeployment builds the Envoy JWT gateway Deployment.
func EnvoyDeployment(cfg *costv1alpha1.CostManagementServiceConfig) *appsv1.Deployment {
	name := NameEnvoy(cfg)
	sel := SelectorLabels(cfg, envoyComponent)
	all := Labels(cfg, envoyComponent)

	image, _ := ImageRef(cfg.Spec.Auth.Envoy.Image)

	replicas := cfg.Spec.Auth.Envoy.Replicas
	if replicas == 0 {
		replicas = 2
	}

	falseVal := false
	init := CACombineInitContainer(cfg)
	return &appsv1.Deployment{
		TypeMeta: metav1.TypeMeta{APIVersion: "apps/v1", Kind: "Deployment"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: cfg.Namespace,
			Labels:    all,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{MatchLabels: sel},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: all,
					// Hash of the rendered envoy.yaml content so that ConfigMap
					// changes (e.g. OIDC issuer URL update) trigger a rolling restart.
					// Envoy reads its config at startup only (static --config flag),
					// so a pod restart is the correct mechanism for config propagation.
					Annotations: map[string]string{
						"koku.costmanagement.io/envoy-config-hash": envoyConfigHash(cfg),
					},
				},
				Spec: corev1.PodSpec{
					ServiceAccountName:           NameGatewayServiceAccount(cfg),
					AutomountServiceAccountToken: &falseVal,
					SecurityContext:              nonRootPodSC(),
					ImagePullSecrets:             imagePullSecrets(cfg),
					InitContainers: []corev1.Container{
						init,
					},
					Containers: []corev1.Container{{
						Name:            "envoy",
						Image:           image,
						ImagePullPolicy: pullPolicy(cfg),
						Command:         []string{"/usr/local/bin/envoy"},
						Args:            []string{"-c", "/etc/envoy/envoy.yaml", "--log-level", "info"},
						Ports: []corev1.ContainerPort{
							{Name: "http", ContainerPort: envoyHTTPPort, Protocol: corev1.ProtocolTCP},
							{Name: "admin", ContainerPort: envoyAdminPort, Protocol: corev1.ProtocolTCP},
						},
						VolumeMounts: []corev1.VolumeMount{
							{Name: "envoy-config", MountPath: "/etc/envoy", ReadOnly: true},
							{Name: "combined-ca-bundle", MountPath: "/etc/ca-certificates", ReadOnly: true},
							{Name: "tmp", MountPath: "/tmp"},
						},
						LivenessProbe: &corev1.Probe{
							ProbeHandler: corev1.ProbeHandler{
								HTTPGet: &corev1.HTTPGetAction{
									Path: "/ready",
									Port: intstr.FromInt32(envoyAdminPort),
								},
							},
							InitialDelaySeconds: 15,
							PeriodSeconds:       20,
							TimeoutSeconds:      3,
						},
						ReadinessProbe: &corev1.Probe{
							ProbeHandler: corev1.ProbeHandler{
								HTTPGet: &corev1.HTTPGetAction{
									Path: "/ready",
									Port: intstr.FromInt32(envoyAdminPort),
								},
							},
							InitialDelaySeconds: 5,
							PeriodSeconds:       10,
							TimeoutSeconds:      3,
						},
						Resources:       cfg.Spec.Auth.Envoy.Resources,
						SecurityContext: restrictedContainerSC(),
					}},
					Volumes: envoyVolumes(cfg),
				},
			},
		},
	}
}

// envoyVolumes builds the volume list for the Envoy Deployment.
// User CA secrets (Keycloak, Kafka, cache, object storage) are projected at
// /ca-extra so CACombineInitContainer merges them into the bundle Envoy uses
// for JWKS TLS verification.
func envoyVolumes(cfg *costv1alpha1.CostManagementServiceConfig) []corev1.Volume {
	vols := []corev1.Volume{
		{
			Name: "envoy-config",
			VolumeSource: corev1.VolumeSource{
				ConfigMap: &corev1.ConfigMapVolumeSource{
					LocalObjectReference: corev1.LocalObjectReference{Name: NameEnvoyConfigMap(cfg)},
					Items:                []corev1.KeyToPath{{Key: "envoy.yaml", Path: "envoy.yaml"}},
				},
			},
		},
		{
			Name: "ca-scripts",
			VolumeSource: corev1.VolumeSource{
				ConfigMap: &corev1.ConfigMapVolumeSource{
					LocalObjectReference: corev1.LocalObjectReference{Name: NameCACombineConfigMap(cfg)},
					Items: []corev1.KeyToPath{
						{Key: "combine-ca.sh", Path: "combine-ca.sh", Mode: int32Ptr(0755)},
					},
				},
			},
		},
		{
			Name: "ca-source",
			VolumeSource: corev1.VolumeSource{
				ConfigMap: &corev1.ConfigMapVolumeSource{
					LocalObjectReference: corev1.LocalObjectReference{Name: NameServiceCAConfigMap(cfg)},
				},
			},
		},
		{Name: "combined-ca-bundle", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
		{Name: "tmp", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
	}
	if extra := UserCAExtraVolume(cfg); extra != nil {
		vols = append(vols, *extra)
	}
	return vols
}

// yamlScalar returns a YAML-safe representation of s for inline embedding.
// Two classes of characters break plain YAML scalars:
//   - Line-break characters (\n, \r, \x00): break out of the current line and
//     allow the remainder to be parsed as new YAML structure (injection).
//   - Colon+space/tab/end-of-value (": ", ":\t", or trailing ":"): YAML treats
//     this as a mapping-value indicator even inside a scalar — silently changes
//     the type of a list entry to a mapping, or causes a parse error in a scalar
//     position. A trailing colon triggers this because the template always places
//     a newline immediately after each interpolated value.
//
// Affected strings are double-quoted using strconv.Quote, which escapes all
// control characters as \n, \r, \x00, etc. Everything else is returned as-is.
func yamlScalar(s string) string {
	for i, c := range s {
		switch {
		case c == '\n' || c == '\r' || c == '\x00':
			return strconv.Quote(s)
		case c == ':' && (i+1 == len(s) || s[i+1] == ' ' || s[i+1] == '\t'):
			return strconv.Quote(s)
		}
	}
	return s
}

// EnvoyYAML renders the Envoy static config from the CR (ported from the Helm chart).
// Uses token replacement (not fmt.Sprintf) so Lua source with %s is left intact.
func EnvoyYAML(cfg *costv1alpha1.CostManagementServiceConfig) string {
	issuer := yamlScalar(KeycloakIssuerURL(cfg))
	jwks := yamlScalar(KeycloakJWKSURL(cfg))
	kcHost, kcPort, useTLS := keycloakHostPort(cfg)
	kcHost = yamlScalar(kcHost)

	var audYAML strings.Builder
	for _, a := range KeycloakAudiences(cfg) {
		audYAML.WriteString("                        - ")
		audYAML.WriteString(yamlScalar(a))
		audYAML.WriteByte('\n')
	}

	ns := cfg.Namespace
	tlsBlock := ""
	if useTLS {
		skipVerify := cfg.Spec.Auth.Keycloak.TLS.InsecureSkipVerify &&
			cfg.Spec.Auth.Keycloak.TLS.CACertSecretName == ""
		if skipVerify {
			tlsBlock = fmt.Sprintf(`
    transport_socket:
      name: envoy.transport_sockets.tls
      typed_config:
        "@type": type.googleapis.com/envoy.extensions.transport_sockets.tls.v3.UpstreamTlsContext
        sni: %s
        common_tls_context:
          tls_params:
            tls_minimum_protocol_version: TLSv1_2
          validation_context:
            trust_chain_verification: ACCEPT_UNTRUSTED`, kcHost)
		} else {
			tlsBlock = fmt.Sprintf(`
    transport_socket:
      name: envoy.transport_sockets.tls
      typed_config:
        "@type": type.googleapis.com/envoy.extensions.transport_sockets.tls.v3.UpstreamTlsContext
        sni: %s
        common_tls_context:
          tls_params:
            tls_minimum_protocol_version: TLSv1_2
          validation_context:
            trusted_ca:
              filename: /etc/ca-certificates/ca-bundle.crt
            match_typed_subject_alt_names:
              - san_type: DNS
                matcher:
                  exact: %s`, kcHost, kcHost)
		}
	}

	rosRoute := ""
	rosCluster := ""
	if costv1alpha1.ROSEnabled(cfg) {
		rosRoute = `              - match:
                  prefix: "/api/cost-management/v1/recommendations/openshift"
                route:
                  cluster: ros-api-backend
                  timeout: 30s
                  retry_policy:
                    retry_on: 5xx,reset,connect-failure,refused-stream
                    num_retries: 2
                    per_try_timeout: 15s
`
		rosCluster = `  - name: ros-api-backend
    connect_timeout: 5s
    type: STRICT_DNS
    load_assignment:
      cluster_name: ros-api-backend
      endpoints:
      - lb_endpoints:
        - endpoint:
            address:
              socket_address:
                address: ` + serviceFQDN(NameROSAPI(cfg), ns) + `
                port_value: 8000
`
	}

	replacer := strings.NewReplacer(
		"__HTTP_PORT__", strconv.Itoa(int(envoyHTTPPort)),
		"__ADMIN_PORT__", strconv.Itoa(int(envoyAdminPort)),
		"__ISSUER__", issuer,
		"__AUDIENCES__", audYAML.String(),
		"__JWKS_URI__", jwks,
		"__LUA__", indentLua(envoyLuaFilter),
		"__ROS_ROUTE__", rosRoute,
		"__ROS_CLUSTER__", rosCluster,
		"__KOKU_HOST__", serviceFQDN(NameKokuAPI(cfg), ns),
		"__INGRESS_HOST__", serviceFQDN(NameIngress(cfg), ns),
		"__RBAC_HOST__", serviceFQDN(NameRBACAPI(cfg), ns),
		"__KC_HOST__", kcHost,
		"__KC_PORT__", strconv.Itoa(int(kcPort)),
		"__KC_TLS__", tlsBlock,
	)
	return replacer.Replace(envoyYAMLTemplate)
}

func indentLua(s string) string {
	lines := strings.Split(s, "\n")
	for i, line := range lines {
		if line == "" {
			continue
		}
		lines[i] = "                      " + line
	}
	return strings.Join(lines, "\n")
}

const envoyYAMLTemplate = `static_resources:
  listeners:
  - name: listener_0
    address:
      socket_address:
        address: 0.0.0.0
        port_value: __HTTP_PORT__
    per_connection_buffer_limit_bytes: 32768
    filter_chains:
    - filters:
      - name: envoy.filters.network.http_connection_manager
        typed_config:
          "@type": type.googleapis.com/envoy.extensions.filters.network.http_connection_manager.v3.HttpConnectionManager
          stat_prefix: gateway_http
          codec_type: AUTO
          request_timeout: 60s
          stream_idle_timeout: 300s
          use_remote_address: true
          access_log:
          - name: envoy.access_loggers.file
            typed_config:
              "@type": type.googleapis.com/envoy.extensions.access_loggers.file.v3.FileAccessLog
              path: /dev/stdout
          route_config:
            name: local_route
            virtual_hosts:
            - name: backend
              domains: ["*"]
              routes:
__ROS_ROUTE__              - match:
                  prefix: "/api/rbac/"
                route:
                  cluster: rbac-api-backend
                  timeout: 30s
                  retry_policy:
                    retry_on: 5xx,reset,connect-failure,refused-stream
                    num_retries: 2
                    per_try_timeout: 15s
              - match:
                  prefix: "/api/cost-management/"
                route:
                  cluster: koku-api-backend
                  timeout: 60s
                  retry_policy:
                    retry_on: 5xx,reset,connect-failure,refused-stream
                    num_retries: 2
                    per_try_timeout: 30s
              - match:
                  path: "/api/ingress/ready"
                route:
                  cluster: ingress-backend
                  timeout: 10s
                  prefix_rewrite: "/"
              - match:
                  prefix: "/api/ingress/"
                route:
                  cluster: ingress-backend
                  timeout: 180s
                  retry_policy:
                    retry_on: 5xx,reset,connect-failure,refused-stream
                    num_retries: 2
                    per_try_timeout: 60s
          http_filters:
          - name: envoy.filters.http.jwt_authn
            typed_config:
              "@type": type.googleapis.com/envoy.extensions.filters.http.jwt_authn.v3.JwtAuthentication
              providers:
                keycloak:
                  issuer: __ISSUER__
                  audiences:
__AUDIENCES__                  remote_jwks:
                    http_uri:
                      uri: __JWKS_URI__
                      cluster: keycloak_jwks
                      timeout: 5s
                    cache_duration:
                      seconds: 300
                  forward: true
                  payload_in_metadata: keycloak
              rules:
              - match:
                  prefix: "/"
                requires:
                  provider_name: keycloak
          - name: envoy.filters.http.lua
            typed_config:
              "@type": type.googleapis.com/envoy.extensions.filters.http.lua.v3.Lua
              default_source_code:
                inline_string: |
__LUA__
          - name: envoy.filters.http.router
            typed_config:
              "@type": type.googleapis.com/envoy.extensions.filters.http.router.v3.Router
  clusters:
__ROS_CLUSTER__  - name: koku-api-backend
    connect_timeout: 5s
    type: STRICT_DNS
    load_assignment:
      cluster_name: koku-api-backend
      endpoints:
      - lb_endpoints:
        - endpoint:
            address:
              socket_address:
                address: __KOKU_HOST__
                port_value: 8000
  - name: ingress-backend
    connect_timeout: 5s
    type: STRICT_DNS
    load_assignment:
      cluster_name: ingress-backend
      endpoints:
      - lb_endpoints:
        - endpoint:
            address:
              socket_address:
                address: __INGRESS_HOST__
                port_value: 8080
  - name: rbac-api-backend
    connect_timeout: 5s
    type: STRICT_DNS
    load_assignment:
      cluster_name: rbac-api-backend
      endpoints:
      - lb_endpoints:
        - endpoint:
            address:
              socket_address:
                address: __RBAC_HOST__
                port_value: 8080
  - name: keycloak_jwks
    connect_timeout: 5s
    type: STRICT_DNS
    load_assignment:
      cluster_name: keycloak_jwks
      endpoints:
      - lb_endpoints:
        - endpoint:
            address:
              socket_address:
                address: __KC_HOST__
                port_value: __KC_PORT____KC_TLS__
admin:
  address:
    socket_address:
      address: 0.0.0.0
      port_value: __ADMIN_PORT__
`

// envoyLuaFilter injects X-Rh-Identity from validated JWT claims (from cost-onprem-chart).
const envoyLuaFilter = `local function validate_identifier(value, field_name, max_length)
  max_length = max_length or 128
  if value == "" then
    return false, field_name .. " is empty"
  end
  if #value > max_length then
    return false, field_name .. " exceeds maximum length of " .. max_length
  end
  if not string.match(value, "^[a-zA-Z0-9._-]+$") then
    return false, field_name .. " contains invalid characters"
  end
  return true, nil
end

local function has_realm_role(jwt_data, role_name)
  local realm_access = jwt_data["realm_access"]
  if realm_access == nil then return false end
  local roles = realm_access["roles"]
  if roles == nil then return false end
  for _, role in ipairs(roles) do
    if role == role_name then return true end
  end
  return false
end

local function build_xrhid_json(org_id, account_number, user_type, username, email, is_org_admin)
  local function escape_json(str)
    if type(str) ~= "string" then
      str = tostring(str)
    end
    str = string.gsub(str, "\\", "\\\\")
    str = string.gsub(str, '"', '\\"')
    str = string.gsub(str, "\n", "\\n")
    str = string.gsub(str, "\r", "\\r")
    str = string.gsub(str, "\t", "\\t")
    return str
  end
  org_id = escape_json(org_id)
  account_number = escape_json(account_number)
  user_type = escape_json(user_type)
  username = escape_json(username or "user")
  email = escape_json(email or (username .. "@noreply.local"))
  local admin_str = is_org_admin and "true" or "false"
  return string.format(
    '{"org_id":"%s","identity":{"org_id":"%s","account_number":"%s","type":"%s","user":{"username":"%s","email":"%s","is_org_admin":%s}},"entitlements":{"cost_management":{"is_entitled":true}}}',
    org_id, org_id, account_number, user_type, username, email, admin_str
  )
end

function envoy_on_request(request_handle)
  local success, err = pcall(function()
    local metadata = request_handle:streamInfo():dynamicMetadata()
    local jwt_metadata = metadata:get("envoy.filters.http.jwt_authn")
    if jwt_metadata == nil then
      request_handle:respond({[":status"] = "401"}, "Unauthorized: No JWT metadata found")
      return
    end
    local jwt_data = jwt_metadata["keycloak"]
    if jwt_data == nil then
      request_handle:respond({[":status"] = "401"}, "Unauthorized: No JWT data found for keycloak provider")
      return
    end
    local org_id = jwt_data["org_id"]
    if org_id == nil or org_id == "" then
      request_handle:respond({[":status"] = "401"}, "Unauthorized: Missing org_id in JWT claims")
      return
    end
    org_id = tostring(org_id)
    local valid, err_msg = validate_identifier(org_id, "org_id")
    if not valid then
      request_handle:respond({[":status"] = "401"}, "Unauthorized: Invalid org_id")
      return
    end
    local account_number = jwt_data["account_number"]
    if account_number == nil or account_number == "" then
      request_handle:respond({[":status"] = "401"}, "Unauthorized: Missing account_number in JWT claims")
      return
    end
    account_number = tostring(account_number)
    valid, err_msg = validate_identifier(account_number, "account_number")
    if not valid then
      request_handle:respond({[":status"] = "401"}, "Unauthorized: Invalid account_number")
      return
    end
    local username = jwt_data["preferred_username"] or jwt_data["sub"] or "user"
    username = tostring(username)
    local email = jwt_data["email"]
    if email then email = tostring(email) end
    if not email or email == "" then
      email = username .. "@example.com"
    end
    local is_org_admin = has_realm_role(jwt_data, "org-admin")
    local xrhid_json = build_xrhid_json(org_id, account_number, "User", username, email, is_org_admin)
    local xrhid_b64 = request_handle:base64Escape(xrhid_json)
    request_handle:headers():replace("X-Rh-Identity", xrhid_b64)
    local auth_header = request_handle:headers():get("authorization")
    if not auth_header then
      auth_header = request_handle:headers():get("Authorization")
    end
    if auth_header then
      local bearer_token = string.match(auth_header, "^[Bb]earer%s+(.+)$")
      if bearer_token and bearer_token ~= "" then
        request_handle:headers():replace("X-Bearer-Token", bearer_token)
      end
    end
  end)
  if not success then
    request_handle:logErr("Lua filter error: " .. tostring(err))
    request_handle:respond({[":status"] = "500"}, "Internal Server Error: Authentication processing failed")
  end
end
`
