package resources

import (
	"strconv"
	"strings"

	costv1alpha1 "github.com/project-koku/koku-service-operator/api/v1alpha1"
)

const (
	UBIMinimalImage = "registry.access.redhat.com/ubi9/ubi-minimal:9.7"

	KokuDBName   = "costonprem_koku"
	RosDBName    = "costonprem_ros"
	RbacDBName   = "costonprem_rbac"
	KruizeDBName = "costonprem_kruize"
)

// Names derives resource names from the CR name so they are deterministic
// and consistent across reconcile loops.

func NameDatabase(cfg *costv1alpha1.CostManagementServiceConfig) string {
	return cfg.Name + "-database"
}

func NameValkey(cfg *costv1alpha1.CostManagementServiceConfig) string {
	return cfg.Name + "-valkey"
}

func NameValkeyPVC(cfg *costv1alpha1.CostManagementServiceConfig) string {
	return cfg.Name + "-valkey-data"
}

func NameDBCredentials(cfg *costv1alpha1.CostManagementServiceConfig) string {
	if cfg.Spec.Database.SecretName != "" {
		return cfg.Spec.Database.SecretName
	}
	return cfg.Name + "-db-credentials"
}

func NameDBInitConfigMap(cfg *costv1alpha1.CostManagementServiceConfig) string {
	return cfg.Name + "-db-init"
}

func NameDjangoSecret(cfg *costv1alpha1.CostManagementServiceConfig) string {
	return cfg.Name + "-django-secret"
}

func NameStorageSecret(cfg *costv1alpha1.CostManagementServiceConfig) string {
	if cfg.Spec.ObjectStorage.SecretName != "" {
		return cfg.Spec.ObjectStorage.SecretName
	}
	return cfg.Name + "-storage-credentials"
}

func NameAWSConfigMap(cfg *costv1alpha1.CostManagementServiceConfig) string {
	return cfg.Name + "-aws-config"
}

func NameCACombineConfigMap(cfg *costv1alpha1.CostManagementServiceConfig) string {
	return cfg.Name + "-ca-combine"
}

func NameServiceCAConfigMap(cfg *costv1alpha1.CostManagementServiceConfig) string {
	return cfg.Name + "-service-ca"
}

func NameKokuMigration(cfg *costv1alpha1.CostManagementServiceConfig) string {
	return cfg.Name + "-koku-migrate"
}

func NameKokuAPI(cfg *costv1alpha1.CostManagementServiceConfig) string {
	return cfg.Name + "-koku-api"
}

func NameKokuServiceAccount(cfg *costv1alpha1.CostManagementServiceConfig) string {
	if cfg.Spec.CostManagement.ServiceAccount.Name != "" {
		return cfg.Spec.CostManagement.ServiceAccount.Name
	}
	return cfg.Name + "-koku"
}

func NameMasu(cfg *costv1alpha1.CostManagementServiceConfig) string {
	return cfg.Name + "-koku-masu"
}

func NameListener(cfg *costv1alpha1.CostManagementServiceConfig) string {
	return cfg.Name + "-koku-listener"
}

func NameCeleryBeat(cfg *costv1alpha1.CostManagementServiceConfig) string {
	return cfg.Name + "-celery-beat"
}

func NameCeleryWorker(cfg *costv1alpha1.CostManagementServiceConfig, queue string) string {
	// Celery queue names may contain underscores (e.g. cost_model); Kubernetes
	// Deployment/container names must be RFC 1123 (alphanumeric + '-' only).
	return cfg.Name + "-celery-worker-" + DNS1123Label(queue)
}

// DNS1123Label converts a Celery queue (or similar) name into a DNS-1123 label
// suitable for Kubernetes resource and container names.
func DNS1123Label(s string) string {
	return strings.ReplaceAll(s, "_", "-")
}

// DatabaseHost returns the hostname of the database that all services should connect to.
func DatabaseHost(cfg *costv1alpha1.CostManagementServiceConfig) string {
	if costv1alpha1.BoolVal(cfg.Spec.Database.Deploy, true) {
		return NameDatabase(cfg)
	}
	return cfg.Spec.Database.Host
}

// cachePortStr returns the cache port as a string, defaulting to "6379".
func cachePortStr(cfg *costv1alpha1.CostManagementServiceConfig) string {
	if cfg.Spec.Cache.Port != 0 {
		return int32String(cfg.Spec.Cache.Port)
	}
	return "6379"
}

// CacheHost returns the hostname of the Valkey/Redis instance.
func CacheHost(cfg *costv1alpha1.CostManagementServiceConfig) string {
	if costv1alpha1.BoolVal(cfg.Spec.Cache.Deploy, true) {
		return NameValkey(cfg)
	}
	return cfg.Spec.Cache.Host
}

// firstBroker returns the first broker from a comma-separated bootstrap servers
// string. "a:9092,b:9093" → "a:9092".
func firstBroker(bootstrapServers string) string {
	first, _, _ := strings.Cut(bootstrapServers, ",")
	return first
}

// KafkaHost returns the hostname of the first Kafka broker.
// BootstrapServers may be comma-separated ("a:9092,b:9092"); only the first
// broker is used for template values that need a single host string.
func KafkaHost(cfg *costv1alpha1.CostManagementServiceConfig) string {
	bs := firstBroker(cfg.Spec.Kafka.BootstrapServers)
	for i := len(bs) - 1; i >= 0; i-- {
		if bs[i] == ':' {
			return bs[:i]
		}
	}
	return bs
}

// KafkaPort returns the port of the first Kafka broker.
func KafkaPort(cfg *costv1alpha1.CostManagementServiceConfig) string {
	bs := firstBroker(cfg.Spec.Kafka.BootstrapServers)
	for i := len(bs) - 1; i >= 0; i-- {
		if bs[i] == ':' {
			return bs[i+1:]
		}
	}
	return "9092"
}

// S3Endpoint returns the S3 endpoint URL including protocol.
// When the user did not set objectStorage.secretName and Discovery populated
// status.discoveredConfig.s3, the discovered full endpoint URL is preferred.
func S3Endpoint(cfg *costv1alpha1.CostManagementServiceConfig) string {
	if cfg.Spec.ObjectStorage.SecretName == "" &&
		cfg.Status.DiscoveredConfig != nil &&
		cfg.Status.DiscoveredConfig.S3 != nil &&
		cfg.Status.DiscoveredConfig.S3.Endpoint != "" {
		return cfg.Status.DiscoveredConfig.S3.Endpoint
	}
	return S3EndpointFromSpec(cfg)
}

// S3EndpointFromSpec builds the S3 URL from spec.objectStorage host, port, and
// UseSSL without consulting status.discoveredConfig.
func S3EndpointFromSpec(cfg *costv1alpha1.CostManagementServiceConfig) string {
	s := cfg.Spec.ObjectStorage
	scheme := "http"
	port := s.Port
	if costv1alpha1.BoolVal(s.UseSSL, true) {
		scheme = "https"
		if port == 0 {
			port = 443
		}
	} else if port == 0 {
		port = 80
	}
	host := s.Endpoint
	if host == "" {
		host = "s3.openshift-storage.svc.cluster.local"
	}
	return scheme + "://" + host + ":" + int32String(port)
}

// S3Bucket returns the object-store bucket name for Koku REQUESTED_BUCKET.
// A non-empty status.discoveredConfig.s3.bucket is preferred over
// spec.costManagement.storage.bucketName, including when the user supplied a Secret.
func S3Bucket(cfg *costv1alpha1.CostManagementServiceConfig) string {
	if cfg.Status.DiscoveredConfig != nil &&
		cfg.Status.DiscoveredConfig.S3 != nil &&
		cfg.Status.DiscoveredConfig.S3.Bucket != "" {
		return cfg.Status.DiscoveredConfig.S3.Bucket
	}
	return cfg.Spec.CostManagement.Storage.BucketName
}

func int32String(n int32) string {
	return strconv.Itoa(int(n))
}

func NameRBACAPI(cfg *costv1alpha1.CostManagementServiceConfig) string {
	return cfg.Name + "-rbac-api"
}

func NameRBACWorker(cfg *costv1alpha1.CostManagementServiceConfig) string {
	return cfg.Name + "-rbac-worker"
}

func NameRBACKeycloakSync(cfg *costv1alpha1.CostManagementServiceConfig) string {
	return cfg.Name + "-rbac-keycloak-sync"
}

func NameRBACKeycloakSyncConfigMap(cfg *costv1alpha1.CostManagementServiceConfig) string {
	return cfg.Name + "-rbac-keycloak-sync-script"
}

func NameROSAPI(cfg *costv1alpha1.CostManagementServiceConfig) string {
	return cfg.Name + "-ros-api"
}

func NameROSProcessor(cfg *costv1alpha1.CostManagementServiceConfig) string {
	return cfg.Name + "-ros-processor"
}

func NameROSPoller(cfg *costv1alpha1.CostManagementServiceConfig) string {
	return cfg.Name + "-ros-recommendation-poller"
}

func NameROSHousekeeper(cfg *costv1alpha1.CostManagementServiceConfig) string {
	return cfg.Name + "-ros-housekeeper"
}

func NameKruize(cfg *costv1alpha1.CostManagementServiceConfig) string {
	return cfg.Name + "-kruize"
}

func NameEnvoy(cfg *costv1alpha1.CostManagementServiceConfig) string {
	return cfg.Name + "-gateway"
}

func NameGatewayServiceAccount(cfg *costv1alpha1.CostManagementServiceConfig) string {
	return cfg.Name + "-gateway"
}

func NameIngressServiceAccount(cfg *costv1alpha1.CostManagementServiceConfig) string {
	return cfg.Name + "-ingress"
}

func NameRBACServiceAccount(cfg *costv1alpha1.CostManagementServiceConfig) string {
	return cfg.Name + "-rbac"
}

func NameUIServiceAccount(cfg *costv1alpha1.CostManagementServiceConfig) string {
	return cfg.Name + "-ui"
}

func NameEnvoyConfigMap(cfg *costv1alpha1.CostManagementServiceConfig) string {
	return cfg.Name + "-gateway-envoy-config"
}

func NameAPIRoute(cfg *costv1alpha1.CostManagementServiceConfig) string {
	return cfg.Name + "-api"
}

func NameUI(cfg *costv1alpha1.CostManagementServiceConfig) string {
	return cfg.Name + "-ui"
}

func NameIngress(cfg *costv1alpha1.CostManagementServiceConfig) string {
	return cfg.Name + "-ingress"
}
