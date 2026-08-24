package resources

import (
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/intstr"

	costv1alpha1 "github.com/project-koku/koku-service-operator/api/v1alpha1"
)

const (
	uiProxyPort = int32(8443)
	uiAppPort   = int32(8080)

	defaultOAuthProxyImage = "quay.io/oauth2-proxy/oauth2-proxy"
	defaultOAuthProxyTag   = "v7.6.0"
)

// NameUINginxConfigMap returns the nginx ConfigMap name for the UI.
func NameUINginxConfigMap(cfg *costv1alpha1.CostManagementServiceConfig) string {
	return cfg.Name + "-ui-nginx-config"
}

// NameUICookieSecret returns the name of the cookie secret (operator-generated).
func NameUICookieSecret(cfg *costv1alpha1.CostManagementServiceConfig) string {
	return cfg.Name + "-ui-cookie-secret"
}

// NameUIOAuthClientSecret returns the name of the OAuth client secret (user-provided).
// Uses spec.ui.oauthClientSecretRef.name when set; otherwise {name}-ui-oauth-client.
func NameUIOAuthClientSecret(cfg *costv1alpha1.CostManagementServiceConfig) string {
	if name := cfg.Spec.UI.OAuthClientSecretRef.Name; name != "" {
		return name
	}
	return cfg.Name + "-ui-oauth-client"
}

// ValidateUIOAuthClientSecret checks that the Secret has non-empty client-id and
// client-secret keys required by oauth2-proxy.
func ValidateUIOAuthClientSecret(secret *corev1.Secret) error {
	if secret == nil {
		return fmt.Errorf("secret is nil")
	}
	id := secret.Data["client-id"]
	sec := secret.Data["client-secret"]
	if len(id) == 0 || len(sec) == 0 {
		return fmt.Errorf("secret %q must contain non-empty keys client-id and client-secret", secret.Name)
	}
	return nil
}

// NameUITLSSecret returns the name of the TLS secret auto-created by the OpenShift service CA.
func NameUITLSSecret(cfg *costv1alpha1.CostManagementServiceConfig) string {
	return cfg.Name + "-ui-tls"
}

// NameConsoleLink is the cluster-scoped ConsoleLink resource name.
func NameConsoleLink(cfg *costv1alpha1.CostManagementServiceConfig) string {
	return cfg.Name + "-cost-management"
}

// UICookieSecret generates a random cookie encryption secret for oauth2-proxy.
// It is created once and never regenerated (COST-7694 rotation is out of scope here).
func UICookieSecret(cfg *costv1alpha1.CostManagementServiceConfig) *corev1.Secret {
	return &corev1.Secret{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Secret"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      NameUICookieSecret(cfg),
			Namespace: cfg.Namespace,
			Labels:    Labels(cfg, "ui"),
		},
		StringData: map[string]string{
			"session-secret": randomPassword(),
		},
	}
}

// UINginxConfigMap builds the nginx config used by the UI app container.
// API calls are proxied to the Envoy gateway; static files are served locally.
func UINginxConfigMap(cfg *costv1alpha1.CostManagementServiceConfig) *corev1.ConfigMap {
	gatewayService := NameEnvoy(cfg)

	nginx := fmt.Sprintf(`location /api/me {
  default_type application/json;
  return 200 "{\"username\":\"$http_x_auth_request_preferred_username\", \"email\":\"$http_x_forwarded_email\"}";
}

location /api/ {
  proxy_pass http://%s:80;
  proxy_set_header Host $host;
  proxy_set_header X-Real-IP $remote_addr;
  proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
  proxy_http_version 1.1;
  proxy_set_header Connection "";
  proxy_read_timeout 60s;
  proxy_send_timeout 60s;
}

location / {
  root /opt/app-root/src/onprem;
  try_files $uri $uri/ /index.html;
  add_header X-Frame-Options "SAMEORIGIN";
  add_header X-Content-Type-Options "nosniff";
  add_header Referrer-Policy "strict-origin-when-cross-origin";
}

location /costManagement/ {
  alias /opt/app-root/src/costManagement/;
  try_files $uri $uri/ /index.html;
}

location /costManagementRos/ {
  alias /opt/app-root/src/costManagementRos/;
  try_files $uri $uri/ /index.html;
}

location /sources/ {
  alias /opt/app-root/src/sources/;
  try_files $uri $uri/ /index.html;
}

location /rbac/ {
  alias /opt/app-root/src/rbac/;
  try_files $uri $uri/ /index.html;
}

location = /logout {
  absolute_redirect off;
  return 302 /oauth2/sign_out?rd=/oauth2/start;
}
`, gatewayService)

	return &corev1.ConfigMap{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "ConfigMap"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      NameUINginxConfigMap(cfg),
			Namespace: cfg.Namespace,
			Labels:    Labels(cfg, "ui"),
		},
		Data: map[string]string{"nginx.conf": nginx},
	}
}

func uiProfileResources(profile costv1alpha1.Profile) corev1.ResourceRequirements {
	switch profile {
	case costv1alpha1.ProfileHA:
		return corev1.ResourceRequirements{
			Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m"), corev1.ResourceMemory: resource.MustParse("128Mi")},
			Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("200m"), corev1.ResourceMemory: resource.MustParse("256Mi")},
		}
	case costv1alpha1.ProfileStandard:
		fallthrough
	default: // unset
		return corev1.ResourceRequirements{
			Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("50m"), corev1.ResourceMemory: resource.MustParse("64Mi")},
			Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m"), corev1.ResourceMemory: resource.MustParse("128Mi")},
		}
	}
}

// oauthProxyImage returns spec.ui.oauthProxy.image, defaulting empty
// repository/tag to the public community pin (product CRs set registry.redhat.io).
func oauthProxyImage(spec costv1alpha1.OAuthProxySpec) string {
	repo := spec.Image.Repository
	tag := spec.Image.Tag
	if repo == "" {
		repo = defaultOAuthProxyImage
	}
	if tag == "" {
		tag = defaultOAuthProxyTag
	}
	return repo + ":" + tag
}

// UIDeployment builds the UI Deployment with the oauth2-proxy sidecar and nginx app.
func UIDeployment(cfg *costv1alpha1.CostManagementServiceConfig) *appsv1.Deployment {
	spec := cfg.Spec.UI
	selLabels := SelectorLabels(cfg, "ui")
	allLabels := Labels(cfg, "ui")
	replicas := spec.ReplicaCount
	if replicas == 0 {
		replicas = 1
	}

	issuerURL := KeycloakIssuerURL(cfg)
	domain := clusterDomain(cfg)
	redirectURL := fmt.Sprintf("https://%s-ui-%s.%s/oauth2/callback", cfg.Name, cfg.Namespace, domain)
	backendLogoutURL := issuerURL + "/protocol/openid-connect/logout?id_token_hint={id_token}"
	upstream := fmt.Sprintf("http://localhost:%d", uiAppPort)

	proxyImage := oauthProxyImage(spec.OAuthProxy)
	appImage := spec.App.Image.Repository + ":" + spec.App.Image.Tag

	proxyResources := spec.OAuthProxy.Resources
	if len(proxyResources.Requests) == 0 && len(proxyResources.Limits) == 0 {
		proxyResources = uiProfileResources(cfg.Spec.Profile)
	}
	appResources := spec.App.Resources
	if len(appResources.Requests) == 0 && len(appResources.Limits) == 0 {
		appResources = uiProfileResources(cfg.Spec.Profile)
	}

	proxyArgs := []string{
		fmt.Sprintf("--https-address=:%d", uiProxyPort),
		"--provider=keycloak-oidc",
		"--oidc-issuer-url=" + issuerURL,
		"--redirect-url=" + redirectURL,
		"--backend-logout-url=" + backendLogoutURL,
		"--tls-cert-file=/etc/tls/private/tls.crt",
		"--tls-key-file=/etc/tls/private/tls.key",
		"--upstream=" + upstream,
		"--pass-host-header=false",
		"--skip-provider-button",
		"--skip-auth-preflight",
		"--pass-authorization-header",
		"--code-challenge-method=S256",
		"--set-xauthrequest",
		"--email-domain=*",
		"--cookie-secure=true",
		"--cookie-expire=" + spec.OAuthProxy.CookieExpire,
		"--provider-ca-file=/etc/keycloak-ca/ca.crt",
	}
	// Public Keycloak Routes use the OpenShift router cert, not the service CA
	// mounted at provider-ca-file. Honor auth.keycloak.tls.insecureSkipVerify
	// so oauth2-proxy can complete OIDC discovery in that setup.
	if cfg.Spec.Auth.Keycloak.TLS.InsecureSkipVerify {
		proxyArgs = append(proxyArgs, "--ssl-insecure-skip-verify=true")
	}

	vols := []corev1.Volume{
		{
			Name: "proxy-tls",
			VolumeSource: corev1.VolumeSource{
				Secret: &corev1.SecretVolumeSource{SecretName: NameUITLSSecret(cfg)},
			},
		},
		{
			Name: "nginx-config",
			VolumeSource: corev1.VolumeSource{
				ConfigMap: &corev1.ConfigMapVolumeSource{
					LocalObjectReference: corev1.LocalObjectReference{Name: NameUINginxConfigMap(cfg)},
				},
			},
		},
		keycloakCAVolume(cfg),
		// Writable paths for nginx under readOnlyRootFilesystem.
		{Name: "nginx-tmp", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
		{Name: "nginx-log", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
		{Name: "nginx-run", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
		{Name: "tmp", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
	}

	probe := &corev1.Probe{
		ProbeHandler: corev1.ProbeHandler{
			HTTPGet: &corev1.HTTPGetAction{
				Path:   "/ping",
				Port:   intstr.FromString("https"),
				Scheme: corev1.URISchemeHTTPS,
			},
		},
		InitialDelaySeconds: 30, PeriodSeconds: 10, TimeoutSeconds: 5, FailureThreshold: 3,
	}

	return &appsv1.Deployment{
		TypeMeta:   metav1.TypeMeta{APIVersion: "apps/v1", Kind: "Deployment"},
		ObjectMeta: metav1.ObjectMeta{Name: NameUI(cfg), Namespace: cfg.Namespace, Labels: allLabels},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{MatchLabels: selLabels},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: allLabels},
				Spec: corev1.PodSpec{
					SecurityContext:              nonRootPodSC(),
					ServiceAccountName:           NameUIServiceAccount(cfg),
					AutomountServiceAccountToken: boolPtr(false),
					ImagePullSecrets:             imagePullSecrets(cfg),
					Containers: []corev1.Container{
						{
							Name:            "oauth-proxy",
							Image:           proxyImage,
							ImagePullPolicy: pullPolicy(cfg),
							Ports:           []corev1.ContainerPort{{Name: "https", ContainerPort: uiProxyPort}},
							Args:            proxyArgs,
							Env: []corev1.EnvVar{
								EnvFromSecret("OAUTH2_PROXY_COOKIE_SECRET", NameUICookieSecret(cfg), "session-secret"),
								EnvFromSecret("OAUTH2_PROXY_CLIENT_ID", NameUIOAuthClientSecret(cfg), "client-id"),
								EnvFromSecret("OAUTH2_PROXY_CLIENT_SECRET", NameUIOAuthClientSecret(cfg), "client-secret"),
							},
							LivenessProbe:  probe,
							ReadinessProbe: probe,
							Resources:      proxyResources,
							VolumeMounts: []corev1.VolumeMount{
								{Name: "proxy-tls", MountPath: "/etc/tls/private", ReadOnly: true},
								{Name: "keycloak-ca", MountPath: "/etc/keycloak-ca", ReadOnly: true},
								{Name: "tmp", MountPath: "/tmp"},
							},
							SecurityContext: uiOAuthProxyContainerSC(),
						},
						{
							Name:            "app",
							Image:           appImage,
							ImagePullPolicy: pullPolicy(cfg),
							Command:         []string{"nginx", "-g", "daemon off;"},
							Ports:           []corev1.ContainerPort{{Name: "http", ContainerPort: uiAppPort}},
							LivenessProbe: &corev1.Probe{
								ProbeHandler:        corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{Path: "/", Port: intstr.FromString("http")}},
								InitialDelaySeconds: 30, PeriodSeconds: 10, TimeoutSeconds: 5, FailureThreshold: 3,
							},
							ReadinessProbe: &corev1.Probe{
								ProbeHandler:        corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{Path: "/", Port: intstr.FromString("http")}},
								InitialDelaySeconds: 30, PeriodSeconds: 10, TimeoutSeconds: 5, FailureThreshold: 3,
							},
							Resources: appResources,
							VolumeMounts: []corev1.VolumeMount{
								{Name: "nginx-config", MountPath: "/opt/app-root/etc/nginx.default.d/nginx.conf", SubPath: "nginx.conf"},
								{Name: "nginx-tmp", MountPath: "/var/lib/nginx/tmp"},
								{Name: "nginx-log", MountPath: "/var/log/nginx"},
								// pid /run/nginx.pid from the image's nginx.conf
								{Name: "nginx-run", MountPath: "/run"},
								{Name: "tmp", MountPath: "/tmp"},
							},
							SecurityContext: uiAppContainerSC(),
						},
					},
					Volumes: vols,
				},
			},
		},
	}
}

// UIService exposes the UI oauth-proxy on HTTPS.
// The annotation triggers OpenShift's service CA to auto-create the TLS secret.
func UIService(cfg *costv1alpha1.CostManagementServiceConfig) *corev1.Service {
	return &corev1.Service{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Service"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      NameUI(cfg),
			Namespace: cfg.Namespace,
			Labels:    Labels(cfg, "ui"),
			Annotations: map[string]string{
				// OpenShift service CA creates NameUITLSSecret automatically.
				"service.beta.openshift.io/serving-cert-secret-name": NameUITLSSecret(cfg),
			},
		},
		Spec: corev1.ServiceSpec{
			Selector: SelectorLabels(cfg, "ui"),
			Ports: []corev1.ServicePort{
				{Name: "https", Port: uiProxyPort, TargetPort: intstr.FromString("https"), Protocol: corev1.ProtocolTCP},
			},
		},
	}
}

// UIRoute builds the OpenShift Route that exposes the UI externally.
// TLS is passthrough since the oauth-proxy terminates it.
func UIRoute(cfg *costv1alpha1.CostManagementServiceConfig) *unstructured.Unstructured {
	domain := clusterDomain(cfg)
	if domain == "" {
		return nil // defer until cluster domain is discovered or set on spec.global
	}
	host := fmt.Sprintf("%s-ui-%s.%s", cfg.Name, cfg.Namespace, domain)

	route := &unstructured.Unstructured{}
	route.SetGroupVersionKind(schema.GroupVersionKind{
		Group: "route.openshift.io", Version: "v1", Kind: "Route",
	})
	route.SetName(cfg.Name + "-ui")
	route.SetNamespace(cfg.Namespace)
	route.SetLabels(Labels(cfg, "ui"))

	_ = unstructured.SetNestedField(route.Object, host, "spec", "host")
	// RouteTargetReference has no "port" field — use spec.port.targetPort (same as GatewayAPIRoute).
	_ = unstructured.SetNestedMap(route.Object, map[string]any{
		"kind":   "Service",
		"name":   NameUI(cfg),
		"weight": int64(100),
	}, "spec", "to")
	_ = unstructured.SetNestedField(route.Object, "https", "spec", "port", "targetPort")
	_ = unstructured.SetNestedMap(route.Object, map[string]any{
		"termination":                   "passthrough",
		"insecureEdgeTerminationPolicy": "Redirect",
	}, "spec", "tls")
	return route
}

// ConsoleLink builds the cluster-scoped ConsoleLink that adds Cost Management
// to the OpenShift Application Menu.
// NOTE: cluster-scoped — no ownerRef; cleaned up via the CR finalizer (COST-7681).
func ConsoleLink(cfg *costv1alpha1.CostManagementServiceConfig) *unstructured.Unstructured {
	domain := clusterDomain(cfg)
	href := ""
	if domain != "" {
		href = fmt.Sprintf("https://%s-ui-%s.%s/", cfg.Name, cfg.Namespace, domain)
	}

	cl := &unstructured.Unstructured{}
	cl.SetGroupVersionKind(schema.GroupVersionKind{
		Group: "console.openshift.io", Version: "v1", Kind: "ConsoleLink",
	})
	cl.SetName(NameConsoleLink(cfg))
	cl.SetLabels(Labels(cfg, "ui"))

	_ = unstructured.SetNestedField(cl.Object, "ApplicationMenu", "spec", "location")
	_ = unstructured.SetNestedField(cl.Object, "Cost Management", "spec", "text")
	_ = unstructured.SetNestedField(cl.Object, href, "spec", "href")
	_ = unstructured.SetNestedField(cl.Object, map[string]any{
		"section":  "Red Hat Applications",
		"imageURL": "static/assets/public/imgs/logos/redhat.svg",
	}, "spec", "applicationMenu")

	return cl
}

// keycloakCAVolume returns the CA volume for oauth2-proxy --provider-ca-file.
// Prefer auth.keycloak.tls.caCertSecretName (router/ingress CA, chart parity);
// otherwise fall back to the OpenShift service-CA ConfigMap (in-cluster only).
func keycloakCAVolume(cfg *costv1alpha1.CostManagementServiceConfig) corev1.Volume {
	if secret := cfg.Spec.Auth.Keycloak.TLS.CACertSecretName; secret != "" {
		return corev1.Volume{
			Name: "keycloak-ca",
			VolumeSource: corev1.VolumeSource{
				Secret: &corev1.SecretVolumeSource{
					SecretName: secret,
					Items:      []corev1.KeyToPath{{Key: "ca.crt", Path: "ca.crt"}},
				},
			},
		}
	}
	return corev1.Volume{
		Name: "keycloak-ca",
		VolumeSource: corev1.VolumeSource{
			ConfigMap: &corev1.ConfigMapVolumeSource{
				LocalObjectReference: corev1.LocalObjectReference{Name: NameServiceCAConfigMap(cfg)},
				Items: []corev1.KeyToPath{
					// inject-cabundle writes service-ca.crt; oauth2-proxy expects ca.crt.
					{Key: "service-ca.crt", Path: "ca.crt"},
				},
			},
		},
	}
}
