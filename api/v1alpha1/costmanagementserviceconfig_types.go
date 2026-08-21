package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// -----------------------------------------------------------------------------
// Condition type constants
// Top-level conditions follow the OpenShift/Kubernetes operator convention.
// Component conditions are set as entries in status.conditions alongside them.
// -----------------------------------------------------------------------------

const (
	// Top-level conditions (machine-readable primary API)
	ConditionAvailable   = "Available"
	ConditionProgressing = "Progressing"
	ConditionDegraded    = "Degraded"

	// Component-level conditions
	ConditionDiscoveryComplete = "DiscoveryComplete"
	ConditionDatabaseReady     = "DatabaseReady"
	ConditionCacheReady        = "CacheReady"
	ConditionStorageReady      = "StorageReady"
	ConditionKafkaReady        = "KafkaReady"
	ConditionAuthReady         = "AuthenticationReady"
	ConditionSchemaUpToDate    = "SchemaUpToDate"
	// ConditionRBACReady reports RBAC API Deployment readiness (not the worker).
	ConditionRBACReady = "RBACReady"
	// ConditionRBACWorkerReady reports the RBAC Celery worker Deployment. It
	// does not gate Available — Koku/Envoy call the API, not the worker.
	ConditionRBACWorkerReady = "RBACWorkerReady"
	ConditionUIReady         = "UIReady"
	// ConditionGatewayReady reports Envoy Deployment + API Route readiness.
	// Distinct from AuthenticationReady, which is the OIDC JWKS probe.
	ConditionGatewayReady = "GatewayReady"
	// ConditionIngressReady reports insights-ingress-go Deployment readiness.
	ConditionIngressReady = "IngressReady"
	// ConditionROSEnabled reports whether ROS/Kruize are active per spec.ros.enabled.
	ConditionROSEnabled = "ROSEnabled"
	// ConditionPaused is True when reconciliation is halted via the pause annotation.
	ConditionPaused = "Paused"
)

// -----------------------------------------------------------------------------
// Profile
// -----------------------------------------------------------------------------

// Profile selects a pre-defined resource sizing tier.
// Currently applies to UI containers only; other workloads use per-component spec.*.resources fields.
// +kubebuilder:validation:Enum=standard;ha
type Profile string

const (
	// ProfileStandard is the default sizing tier (chart-equivalent UI footprint).
	ProfileStandard Profile = "standard"
	// ProfileHA raises UI CPU/memory defaults for a redundant footprint.
	ProfileHA Profile = "ha"
)

// -----------------------------------------------------------------------------
// Shared primitives
// -----------------------------------------------------------------------------

type ImageSpec struct {
	Repository string `json:"repository"`
	Tag        string `json:"tag"`
	// +kubebuilder:default:=IfNotPresent
	PullPolicy corev1.PullPolicy `json:"pullPolicy,omitempty"`
}

type ServiceAccountSpec struct {
	// Create controls whether the operator creates and owns the ServiceAccount.
	// When false, the operator references Name (or the default name) without
	// applying or adopting the object — the SA must already exist.
	// +kubebuilder:default:=true
	Create *bool `json:"create,omitempty"`
	// Name is the ServiceAccount name used by pods. When empty, a default
	// name derived from the CR is used.
	Name string `json:"name,omitempty"`
}

// SecretKeyRef points to a key inside a named Secret.
type SecretKeyRef struct {
	Name string `json:"name"`
	Key  string `json:"key"`
}

// BoolVal reads a *bool field, returning defaultVal when the pointer is nil.
// Use this to read fields that have a +kubebuilder:default:=true annotation.
func BoolVal(b *bool, defaultVal bool) bool {
	if b == nil {
		return defaultVal
	}
	return *b
}

// -----------------------------------------------------------------------------
// GlobalConfig
// -----------------------------------------------------------------------------

type GlobalConfig struct {
	// +kubebuilder:default:=IfNotPresent
	PullPolicy corev1.PullPolicy `json:"pullPolicy,omitempty"`
	// ImagePullSecrets are applied to every Pod the operator creates (Deployments,
	// StatefulSets, Jobs, CronJobs) so workloads can pull from private registries.
	ImagePullSecrets []corev1.LocalObjectReference `json:"imagePullSecrets,omitempty"`
	// Cluster base domain used for Route hostname generation.
	// Auto-detected by the Discovery phase when empty.
	// +kubebuilder:default:="apps.cluster.local"
	ClusterDomain string `json:"clusterDomain,omitempty"`
	// StorageClass for all PVCs. Auto-detected by the Discovery phase when empty.
	StorageClass string `json:"storageClass,omitempty"`
}

// -----------------------------------------------------------------------------
// DatabaseConfig
// -----------------------------------------------------------------------------

// +kubebuilder:validation:XValidation:rule="self.deploy != false || (size(self.host) > 0 && size(self.secretName) > 0)",message="host and secretName are required when database.deploy is false"
type DatabaseConfig struct {
	// Deploy the bundled PostgreSQL StatefulSet (dev/CI only — not for production).
	// Set false to connect to an external database.
	// +kubebuilder:default:=true
	Deploy  *bool               `json:"deploy,omitempty"`
	Image   ImageSpec           `json:"image,omitempty"`
	Storage DatabaseStorageSpec `json:"storage,omitempty"`

	// Host for an external PostgreSQL instance (only used when Deploy is false).
	Host string `json:"host,omitempty"`
	// +kubebuilder:default:=5432
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	Port int32 `json:"port,omitempty"`
	// +kubebuilder:default:=disable
	// +kubebuilder:validation:Enum=disable;require;verify-ca;verify-full
	SSLMode string `json:"sslMode,omitempty"`

	// Name of an existing Secret containing DB credentials.
	// Keys: postgres-user, postgres-password, koku-user, koku-password,
	// ros-user, ros-password, kruize-user, kruize-password, rbac-user, rbac-password.
	// When empty the operator generates credentials into <cr-name>-db-credentials.
	SecretName string `json:"secretName,omitempty"`

	Resources corev1.ResourceRequirements `json:"resources,omitempty"`
}

type DatabaseStorageSpec struct {
	// +kubebuilder:default:="30Gi"
	Size resource.Quantity `json:"size,omitempty"`
}

// -----------------------------------------------------------------------------
// CacheConfig (Valkey / Redis)
// -----------------------------------------------------------------------------

// +kubebuilder:validation:XValidation:rule="self.deploy != false || (size(self.host) > 0 && has(self.auth.secretName) && size(self.auth.secretName) > 0)",message="host and auth.secretName are required when cache.deploy is false"
type CacheConfig struct {
	// Deploy the bundled Valkey instance (dev/CI only — not for production).
	// Set false to connect to an external Redis/Valkey endpoint.
	// +kubebuilder:default:=true
	Deploy *bool     `json:"deploy,omitempty"`
	Image  ImageSpec `json:"image,omitempty"`

	// Host for an external cache (only used when Deploy is false).
	Host string `json:"host,omitempty"`
	// +kubebuilder:default:=6379
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	Port int32 `json:"port,omitempty"`

	Auth        CacheAuthSpec               `json:"auth,omitempty"`
	TLS         CacheTLSSpec                `json:"tls,omitempty"`
	Persistence CachePersistenceSpec        `json:"persistence,omitempty"`
	Resources   corev1.ResourceRequirements `json:"resources,omitempty"`
}

type CacheAuthSpec struct {
	Enabled bool `json:"enabled,omitempty"`
	// Name of a Secret with key redis-password (and optionally redis-username).
	SecretName string `json:"secretName,omitempty"`
}

type CacheTLSSpec struct {
	Enabled bool `json:"enabled,omitempty"`
	// Name of a Secret with key ca.crt for certificate verification.
	CACertSecretName string `json:"caCertSecretName,omitempty"`
}

type CachePersistenceSpec struct {
	// +kubebuilder:default:="5Gi"
	Size resource.Quantity `json:"size,omitempty"`
}

// -----------------------------------------------------------------------------
// KafkaConfig — connection only; Kafka is managed by AMQ Streams externally
// -----------------------------------------------------------------------------

type KafkaConfig struct {
	// Bootstrap servers for the Kafka cluster.
	// +kubebuilder:default:="cost-onprem-kafka-kafka-bootstrap.kafka.svc.cluster.local:9092"
	BootstrapServers string `json:"bootstrapServers"`
	// +kubebuilder:default:=PLAINTEXT
	// +kubebuilder:validation:Enum=PLAINTEXT;SSL;SASL_PLAINTEXT;SASL_SSL
	SecurityProtocol string        `json:"securityProtocol,omitempty"`
	SASL             KafkaSASLSpec `json:"sasl,omitempty"`
	TLS              KafkaTLSSpec  `json:"tls,omitempty"`
}

type KafkaSASLSpec struct {
	// +kubebuilder:validation:Enum=PLAIN;SCRAM-SHA-256;SCRAM-SHA-512;""
	Mechanism string `json:"mechanism,omitempty"`
	// Name of a Secret with keys: username, password.
	ExistingSecret string `json:"existingSecret,omitempty"`
}

type KafkaTLSSpec struct {
	Enabled bool `json:"enabled,omitempty"`
	// Name of a Secret with key ca.crt.
	CACertSecret string `json:"caCertSecret,omitempty"`
}

// -----------------------------------------------------------------------------
// ObjectStorageConfig (S3-compatible)
// -----------------------------------------------------------------------------

type ObjectStorageConfig struct {
	// S3 endpoint hostname (without protocol or port).
	// Auto-detected by the Discovery phase (OBC → NooBaa → user-provided).
	// +kubebuilder:default:="s3.openshift-storage.svc.cluster.local"
	Endpoint string `json:"endpoint,omitempty"`
	// +kubebuilder:default:=443
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	Port int32 `json:"port,omitempty"`
	// +kubebuilder:default:=true
	UseSSL *bool `json:"useSSL,omitempty"`
	// InsecureSkipVerify disables TLS certificate verification for the S3
	// endpoint. Use for dev/CRC setups with self-signed certs.
	// Prefer CACertSecretName for production.
	// +kubebuilder:default:=false
	InsecureSkipVerify bool `json:"insecureSkipVerify,omitempty"`
	// CACertSecretName names a Secret with key ca.crt containing the CA
	// certificate used to verify the S3 endpoint TLS. Required for on-prem
	// S3 endpoints (MinIO, Ceph RGW, NooBaa HTTPS) using private CAs.
	CACertSecretName string `json:"caCertSecretName,omitempty"`
	// Name of an existing Secret with keys: access-key, secret-key.
	// When empty the operator creates or detects the secret via ODF/NooBaa.
	SecretName string `json:"secretName,omitempty"`
	// Namespace of the noobaa-admin Secret used by NooBaa auto-detection.
	// Ignored when secretName is set. Allowed values are openshift-storage
	// (ODF) and noobaa (standalone noobaa-operator). Other namespaces must
	// use secretName instead — the operator must not fetch noobaa-admin
	// from an arbitrary namespace chosen in the CR.
	// +kubebuilder:default:="openshift-storage"
	// +kubebuilder:validation:Enum=openshift-storage;noobaa
	NoobaaNamespace string    `json:"noobaaNamespace,omitempty"`
	S3              S3Options `json:"s3,omitempty"`
}

type S3Options struct {
	// +kubebuilder:default:=onprem
	Region string `json:"region,omitempty"`
	// +kubebuilder:default:=path
	// +kubebuilder:validation:Enum=path;auto;virtual
	AddressingStyle string `json:"addressingStyle,omitempty"`
}

// -----------------------------------------------------------------------------
// AuthConfig (JWT via Envoy + Keycloak/RHBK)
// -----------------------------------------------------------------------------

type AuthConfig struct {
	Envoy    EnvoySpec    `json:"envoy,omitempty"`
	Keycloak KeycloakSpec `json:"keycloak,omitempty"`
}

type EnvoySpec struct {
	Image ImageSpec `json:"image,omitempty"`
	// +kubebuilder:default:=2
	Replicas  int32                       `json:"replicas,omitempty"`
	Resources corev1.ResourceRequirements `json:"resources,omitempty"`
}

// +kubebuilder:validation:XValidation:rule="!has(self.issuerURL) || size(self.issuerURL) == 0 || self.issuerURL.startsWith('https://')",message="issuerURL must use https when set"
// +kubebuilder:validation:XValidation:rule="!has(self.audiences) || size(self.audiences) > 0",message="audiences must not be empty when set"
type KeycloakSpec struct {
	// Full URL of the Keycloak instance used for JWKS fetch (and issuer when
	// issuerURL is unset). Prefer an in-cluster http(s) Service URL so Envoy
	// can reach JWKS without depending on the OpenShift router.
	// Example: http://keycloak-service.keycloak.svc.cluster.local:8080
	// +kubebuilder:validation:Pattern=`^[^\x00-\x1f\x7f]*$`
	URL string `json:"url,omitempty"`
	// IssuerURL is the JWT iss value Envoy validates (must match tokens).
	// RHBK with a configured hostname issues tokens with the public Route URL
	// as iss even when clients obtain them via the in-cluster Service — set
	// this to that frontend base URL (or the full .../realms/<realm> issuer).
	// When empty, issuer is derived from url + realm.
	// +kubebuilder:validation:Pattern=`^[^\x00-\x1f\x7f]*$`
	IssuerURL string `json:"issuerURL,omitempty"`
	// Keycloak namespace. Defaults to "keycloak".
	Namespace string `json:"namespace,omitempty"`
	// +kubebuilder:default:=kubernetes
	// +kubebuilder:validation:Pattern=`^[^\x00-\x1f\x7f]*$`
	Realm string `json:"realm,omitempty"`
	// JWT audiences accepted by the gateway.
	// +kubebuilder:default:={"cost-management-operator","cost-management-ui"}
	// +kubebuilder:validation:items:Pattern=`^[^\x00-\x1f\x7f]*$`
	Audiences []string        `json:"audiences,omitempty"`
	TLS       KeycloakTLSSpec `json:"tls,omitempty"`
}

type KeycloakTLSSpec struct {
	// InsecureSkipVerify disables TLS verification for Keycloak OIDC/JWKS.
	// Prefer CACertSecretName for production.
	InsecureSkipVerify bool `json:"insecureSkipVerify,omitempty"`
	// CACertSecretName is an existing Secret with key ca.crt used to verify
	// Keycloak TLS (typically the OpenShift router CA when issuerURL is a
	// public Route). When empty, oauth-proxy falls back to the cluster
	// service CA ConfigMap — which does not trust Route certificates.
	CACertSecretName string `json:"caCertSecretName,omitempty"`
}

// -----------------------------------------------------------------------------
// RBACConfig (insights-rbac)
// -----------------------------------------------------------------------------

type RBACConfig struct {
	Image          ImageSpec          `json:"image,omitempty"`
	API            RBACComponentSpec  `json:"api,omitempty"`
	Worker         RBACComponentSpec  `json:"worker,omitempty"`
	BootstrapAdmin BootstrapAdminSpec `json:"bootstrapAdmin,omitempty"`
	KeycloakSync   KeycloakSyncSpec   `json:"keycloakSync,omitempty"`
}

type RBACComponentSpec struct {
	// +kubebuilder:default:=1
	Replicas  int32                       `json:"replicas,omitempty"`
	Resources corev1.ResourceRequirements `json:"resources,omitempty"`
}

type BootstrapAdminSpec struct {
	Enabled bool `json:"enabled,omitempty"`
	// SecretRef references a Secret containing the bootstrap admin identity.
	// Required keys: org-id, account-number, username.
	SecretRef corev1.LocalObjectReference `json:"secretRef,omitempty"`
}

type KeycloakSyncSpec struct {
	Enabled bool `json:"enabled,omitempty"`
	// +kubebuilder:default:="*/15 * * * *"
	Schedule         string `json:"schedule,omitempty"`
	OrgGroupPrefix   string `json:"orgGroupPrefix,omitempty"`
	OrgAdminSubgroup string `json:"orgAdminSubgroup,omitempty"`
	// PruneOrphans deletes RBAC Principals that no longer exist in the
	// org's Keycloak group. Matches the Helm chart default (true).
	// +kubebuilder:default:=true
	PruneOrphans *bool `json:"pruneOrphans,omitempty"`
	// +kubebuilder:default:="rbac-keycloak-sync"
	ClientID        string                      `json:"clientId,omitempty"`
	ClientSecretRef SecretKeyRef                `json:"clientSecretRef,omitempty"`
	Resources       corev1.ResourceRequirements `json:"resources,omitempty"`
}

// -----------------------------------------------------------------------------
// IngressConfig (insights-ingress-go upload handler)
// -----------------------------------------------------------------------------

type IngressConfig struct {
	Image ImageSpec `json:"image,omitempty"`
	// Maximum upload size in bytes.
	// +kubebuilder:default:=104857600
	MaxUploadSize int64 `json:"maxUploadSize,omitempty"`
	// Comma-separated list of valid upload content types.
	// +kubebuilder:default:="hccm"
	ValidTypes string `json:"validTypes,omitempty"`
	// Staging bucket name for uploads.
	// When empty, the operator uses the same bucket as Koku REQUESTED_BUCKET
	// (status.discoveredConfig.s3.bucket, else spec.costManagement.storage.bucketName),
	// then "koku-bucket".
	// The bucket must already exist; the operator will not create it.
	StagingBucket string                      `json:"stagingBucket,omitempty"`
	Resources     corev1.ResourceRequirements `json:"resources,omitempty"`
}

// -----------------------------------------------------------------------------
// KruizeConfig (resource optimization engine, used by ROS)
// -----------------------------------------------------------------------------

type KruizeConfig struct {
	// +kubebuilder:default:=1
	Replicas   int32                       `json:"replicas,omitempty"`
	Image      ImageSpec                   `json:"image,omitempty"`
	Resources  corev1.ResourceRequirements `json:"resources,omitempty"`
	Partitions KruizePartitionsSpec        `json:"partitions,omitempty"`
}

type KruizePartitionsSpec struct {
	// +kubebuilder:default:=true
	CreateEnabled *bool `json:"createEnabled,omitempty"`
	// +kubebuilder:default:=true
	DeleteEnabled *bool `json:"deleteEnabled,omitempty"`
	// +kubebuilder:default:="0 0 * * *"
	DeleteSchedule string `json:"deleteSchedule,omitempty"`
	// +kubebuilder:default:="16"
	DeletePartitionsThreshold string                      `json:"deletePartitionsThreshold,omitempty"`
	Resources                 corev1.ResourceRequirements `json:"resources,omitempty"`
}

// -----------------------------------------------------------------------------
// ROSConfig (Resource Optimization Service)
// -----------------------------------------------------------------------------

type ROSConfig struct {
	// When false (the default), the operator skips ROS and Kruize (ROS-only
	// dependency): migrations, Deployments/Services, CronJobs, NetworkPolicies,
	// Envoy routes, and cluster-scoped Kruize RBAC.
	// Beta ships Cost Management without ROS/Kruize — set enabled: true only when
	// you intentionally opt in (and provide ROS/Kruize images).
	// +kubebuilder:default:=false
	Enabled *bool `json:"enabled,omitempty"`

	Image                ImageSpec          `json:"image,omitempty"`
	ServiceAccount       ServiceAccountSpec `json:"serviceAccount,omitempty"`
	API                  ROSAPISpec         `json:"api,omitempty"`
	Processor            ROSProcessorSpec   `json:"processor,omitempty"`
	RecommendationPoller ROSPollerSpec      `json:"recommendationPoller,omitempty"`
	Housekeeper          ROSHousekeeperSpec `json:"housekeeper,omitempty"`
}

// ROSEnabled reports whether ROS (and Kruize) should be deployed for this CR.
// Omitted / nil defaults to false (beta: Cost-only).
func ROSEnabled(cfg *CostManagementServiceConfig) bool {
	return BoolVal(cfg.Spec.ROS.Enabled, false)
}

type ROSAPISpec struct {
	Resources corev1.ResourceRequirements `json:"resources,omitempty"`
	// +kubebuilder:default:=INFO
	LogLevel string `json:"logLevel,omitempty"`
}

type ROSProcessorSpec struct {
	// +kubebuilder:default:=1
	Replicas  int32                       `json:"replicas,omitempty"`
	Resources corev1.ResourceRequirements `json:"resources,omitempty"`
	// +kubebuilder:default:=INFO
	LogLevel string `json:"logLevel,omitempty"`
}

type ROSPollerSpec struct {
	Resources corev1.ResourceRequirements `json:"resources,omitempty"`
	// +kubebuilder:default:=INFO
	LogLevel string `json:"logLevel,omitempty"`
}

type ROSHousekeeperSpec struct {
	Resources        corev1.ResourceRequirements `json:"resources,omitempty"`
	PartitionCleaner ROSPartitionCleanerSpec     `json:"partitionCleaner,omitempty"`
}

type ROSPartitionCleanerSpec struct {
	// +kubebuilder:default:=true
	Enabled *bool `json:"enabled,omitempty"`
	// +kubebuilder:default:="0 0 */15 * *"
	Schedule  string                      `json:"schedule,omitempty"`
	Resources corev1.ResourceRequirements `json:"resources,omitempty"`
}

// -----------------------------------------------------------------------------
// CostManagementConfig (Koku API, Masu, Celery, Listener)
// -----------------------------------------------------------------------------

type CostManagementConfig struct {
	// +kubebuilder:default:=true
	ScheduleReportChecks *bool `json:"scheduleReportChecks,omitempty"`
	// Cron expression for report download checks.
	// +kubebuilder:default:="*/5 * * * *"
	ReportDownloadSchedule string `json:"reportDownloadSchedule,omitempty"`
	// DataRetentionMonths is how many months of cost report data to retain.
	// +kubebuilder:default:=4
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=60
	DataRetentionMonths int32 `json:"dataRetentionMonths,omitempty"`

	Storage        CostManagementStorageSpec `json:"storage,omitempty"`
	API            KokuAPISpec               `json:"api,omitempty"`
	Masu           MasuSpec                  `json:"masu,omitempty"`
	Listener       ListenerSpec              `json:"listener,omitempty"`
	Celery         CelerySpec                `json:"celery,omitempty"`
	ServiceAccount ServiceAccountSpec        `json:"serviceAccount,omitempty"`
}

type CostManagementStorageSpec struct {
	// Bucket name for Cost Management object storage (Koku REQUESTED_BUCKET).
	// The bucket must already exist; the operator will not create it.
	// +kubebuilder:default:="koku-bucket"
	BucketName string `json:"bucketName,omitempty"`
	// ROS object-storage bucket. Required when ros.enabled is true.
	// The bucket must already exist; the operator will not create it.
	// +kubebuilder:default:="ros-data"
	ROSBucketName string `json:"rosBucketName,omitempty"`
}

type KokuAPISpec struct {
	// +kubebuilder:default:=true
	Enabled *bool     `json:"enabled,omitempty"`
	Image   ImageSpec `json:"image,omitempty"`
	// +kubebuilder:default:=1
	Replicas  int32                       `json:"replicas,omitempty"`
	Resources corev1.ResourceRequirements `json:"resources,omitempty"`
	// Environment variable overrides merged over operator-managed defaults.
	// Must not contain secret values — use Secret references for credentials.
	Env map[string]string `json:"env,omitempty"`
}

type MasuSpec struct {
	// +kubebuilder:default:=true
	Enabled *bool     `json:"enabled,omitempty"`
	Image   ImageSpec `json:"image,omitempty"`
	// +kubebuilder:default:=1
	Replicas  int32                       `json:"replicas,omitempty"`
	Resources corev1.ResourceRequirements `json:"resources,omitempty"`
	Env       map[string]string           `json:"env,omitempty"`
}

type ListenerSpec struct {
	// +kubebuilder:default:=true
	Enabled *bool `json:"enabled,omitempty"`
	// +kubebuilder:default:=2
	Replicas  int32                       `json:"replicas,omitempty"`
	Resources corev1.ResourceRequirements `json:"resources,omitempty"`
	Env       map[string]string           `json:"env,omitempty"`
}

type CelerySpec struct {
	Workers CeleryWorkersSpec `json:"workers,omitempty"`
}

type CeleryWorkersSpec struct {
	Default   CeleryWorkerSpec `json:"default,omitempty"`
	Priority  CeleryWorkerSpec `json:"priority,omitempty"`
	Summary   CeleryWorkerSpec `json:"summary,omitempty"`
	OCP       CeleryWorkerSpec `json:"ocp,omitempty"`
	CostModel CeleryWorkerSpec `json:"costModel,omitempty"`
	Refresh   CeleryWorkerSpec `json:"refresh,omitempty"`
	Download  CeleryWorkerSpec `json:"download,omitempty"`
	// SaaS-only queues — disabled by default for on-prem (COST-7687).
	HCS              SaaSCeleryWorkerSpec `json:"hcs,omitempty"`
	SubsExtraction   SaaSCeleryWorkerSpec `json:"subsExtraction,omitempty"`
	SubsTransmission SaaSCeleryWorkerSpec `json:"subsTransmission,omitempty"`
}

type CeleryWorkerSpec struct {
	// +kubebuilder:default:=1
	Replicas int32 `json:"replicas,omitempty"`
	// +kubebuilder:default:=5
	Concurrency int32                       `json:"concurrency,omitempty"`
	Resources   corev1.ResourceRequirements `json:"resources,omitempty"`
}

// SaaSCeleryWorkerSpec configures cloud/SaaS Celery queues (hcs, subs_*).
// On-prem installs should leave these at the default of 0 replicas.
type SaaSCeleryWorkerSpec struct {
	// +kubebuilder:default:=0
	Replicas int32 `json:"replicas,omitempty"`
	// +kubebuilder:default:=5
	Concurrency int32                       `json:"concurrency,omitempty"`
	Resources   corev1.ResourceRequirements `json:"resources,omitempty"`
}

// CeleryWorkerSpec returns the shared worker shape used by resource builders.
func (s SaaSCeleryWorkerSpec) CeleryWorkerSpec() CeleryWorkerSpec {
	return CeleryWorkerSpec(s)
}

// -----------------------------------------------------------------------------
// UIConfig
// -----------------------------------------------------------------------------

type UIConfig struct {
	// +kubebuilder:default:=1
	ReplicaCount int32          `json:"replicaCount,omitempty"`
	OAuthProxy   OAuthProxySpec `json:"oauthProxy,omitempty"`
	App          UIAppSpec      `json:"app,omitempty"`
	// OAuthClientSecretRef names a Secret in the CR namespace with keys
	// client-id and client-secret for oauth2-proxy.
	// When empty, defaults to {metadata.name}-ui-oauth-client.
	OAuthClientSecretRef corev1.LocalObjectReference `json:"oauthClientSecretRef,omitempty"`
}

type OAuthProxySpec struct {
	Image     ImageSpec                   `json:"image,omitempty"`
	Resources corev1.ResourceRequirements `json:"resources,omitempty"`
	// Cookie session expiration (e.g. "720h").
	// +kubebuilder:default:="720h"
	CookieExpire string `json:"cookieExpire,omitempty"`
}

type UIAppSpec struct {
	Image     ImageSpec                   `json:"image,omitempty"`
	Resources corev1.ResourceRequirements `json:"resources,omitempty"`
}

// -----------------------------------------------------------------------------
// GatewayRouteConfig (OpenShift Route for the Envoy gateway)
// -----------------------------------------------------------------------------

type GatewayRouteConfig struct {
	// Annotations are copied onto the OpenShift Route. The operator injects
	// haproxy.router.openshift.io/timeout=180s unless this map sets that key.
	// Omitting the key no longer means OpenShift's 30s default; set "30s"
	// explicitly to restore it.
	Annotations map[string]string `json:"annotations,omitempty"`
	// Custom hostname for the OpenShift Route. Leave empty to derive from clusterDomain.
	Host string       `json:"host,omitempty"`
	TLS  RouteTLSSpec `json:"tls,omitempty"`
}

type RouteTLSSpec struct {
	// Envoy's backend listener is plaintext HTTP, so only edge termination
	// is valid — passthrough/reencrypt would break the TLS handshake.
	// +kubebuilder:default:=edge
	// +kubebuilder:validation:Enum=edge
	Termination string `json:"termination,omitempty"`
	// +kubebuilder:default:=Redirect
	// +kubebuilder:validation:Enum=Allow;Redirect;None
	InsecureEdgeTerminationPolicy string `json:"insecureEdgeTerminationPolicy,omitempty"`
}

// -----------------------------------------------------------------------------
// MonitoringConfig
// -----------------------------------------------------------------------------

type MonitoringConfig struct {
	// +kubebuilder:default:=true
	Enabled *bool `json:"enabled,omitempty"`
}

// -----------------------------------------------------------------------------
// Top-level Spec
// -----------------------------------------------------------------------------

type CostManagementServiceConfigSpec struct {
	// Profile selects a pre-defined resource sizing tier.
	// Currently applies to UI containers only; other workloads use per-component spec.*.resources fields.
	// +kubebuilder:default:=standard
	Profile        Profile              `json:"profile,omitempty"`
	Global         GlobalConfig         `json:"global,omitempty"`
	Database       DatabaseConfig       `json:"database,omitempty"`
	Cache          CacheConfig          `json:"cache,omitempty"`
	Kafka          KafkaConfig          `json:"kafka,omitempty"`
	ObjectStorage  ObjectStorageConfig  `json:"objectStorage,omitempty"`
	Auth           AuthConfig           `json:"auth,omitempty"`
	RBAC           RBACConfig           `json:"rbac,omitempty"`
	CostManagement CostManagementConfig `json:"costManagement,omitempty"`
	ROS            ROSConfig            `json:"ros,omitempty"`
	Kruize         KruizeConfig         `json:"kruize,omitempty"`
	Ingress        IngressConfig        `json:"ingress,omitempty"`
	UI             UIConfig             `json:"ui,omitempty"`
	GatewayRoute   GatewayRouteConfig   `json:"gatewayRoute,omitempty"`
	Monitoring     MonitoringConfig     `json:"monitoring,omitempty"`
}

// -----------------------------------------------------------------------------
// Status
// -----------------------------------------------------------------------------

// Phase is a human-readable convenience field summarising operator progress.
// Conditions are the primary machine-readable status API.
//
// +kubebuilder:validation:Enum=Pending;Progressing;Ready;Degraded
type Phase string

const (
	// PhasePending means the CR was just created and reconciliation has not started.
	PhasePending Phase = "Pending"
	// PhaseProgressing means the operator is actively working toward desired state.
	// Check the Progressing condition's reason and message for details.
	PhaseProgressing Phase = "Progressing"
	// PhaseReady means all components are running and healthy.
	PhaseReady Phase = "Ready"
	// PhaseDegraded means the operator cannot make progress without intervention.
	// Check the Degraded condition's reason and message for details.
	PhaseDegraded Phase = "Degraded"
)

// DiscoveredConfig holds values auto-detected by the Discovery phase.
// These are populated before workloads are created and can be used
// to verify what the operator resolved.
type DiscoveredConfig struct {
	// ClusterDomain detected from the OpenShift cluster configuration.
	ClusterDomain string `json:"clusterDomain,omitempty"`
	// StorageClass detected as the cluster default.
	StorageClass string `json:"storageClass,omitempty"`
	// S3 holds resolved object storage connection details.
	S3 *DiscoveredS3 `json:"s3,omitempty"`
}

// DiscoveredS3 holds the resolved S3 endpoint, object-store bucket name, and credentials reference.
type DiscoveredS3 struct {
	// Endpoint in the form scheme://host:port.
	Endpoint string `json:"endpoint,omitempty"`
	// SecretName of the Secret containing access-key and secret-key.
	SecretName string `json:"secretName,omitempty"`
	// Region used for S3 signature generation.
	Region string `json:"region,omitempty"`
	// Bucket is the object-store bucket name (not a Secret).
	Bucket string `json:"bucket,omitempty"`
}

type CostManagementServiceConfigStatus struct {
	// Phase is a human-readable summary of overall operator state.
	// Conditions are the authoritative machine-readable status.
	// +kubebuilder:default=Pending
	Phase Phase `json:"phase,omitempty"`

	// Conditions is the canonical status API.
	// Standard conditions: Available, Progressing, Degraded.
	// Component conditions: DatabaseReady, CacheReady, StorageReady,
	// KafkaReady, AuthenticationReady, SchemaUpToDate, DiscoveryComplete,
	// GatewayReady, IngressReady, UIReady.
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// DiscoveredConfig holds values auto-detected during the Discovery phase.
	// +optional
	DiscoveredConfig *DiscoveredConfig `json:"discoveredConfig,omitempty"`

	// ObservedGeneration is the metadata.generation last reconciled.
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
}

// -----------------------------------------------------------------------------
// Root object
// -----------------------------------------------------------------------------

// CostManagementServiceConfig is the schema for the on-premise Cost Management deployment.
//
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=cmsc
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type CostManagementServiceConfig struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   CostManagementServiceConfigSpec   `json:"spec,omitempty"`
	Status CostManagementServiceConfigStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
type CostManagementServiceConfigList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []CostManagementServiceConfig `json:"items"`
}

func init() {
	SchemeBuilder.Register(&CostManagementServiceConfig{}, &CostManagementServiceConfigList{})
}
