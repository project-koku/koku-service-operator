# Quickstart

Apply a minimal `CostManagementServiceConfig` and wait until core Cost
Management is `Available`. This walkthrough is for **beta** (`ros.enabled` off)
and assumes [prerequisites](prerequisites.md) are already met.

**Time:** under 30 minutes after the operator is running and Secrets/buckets
exist. Provisioning Postgres, Kafka, S3, and Keycloak is **not** part of this
clock.

**Beta.** Do not set `spec.ros.enabled: true`.

## Install the operator

The operator watches **every** namespace (AllNamespaces). Suggested install
NS is `cost-onprem`. OwnNamespace is not supported. Two current vehicles:

1. **OLM bundle** (preferred when you have a bundle image) — follow
   [olm-bundle-testing.md](../development/olm-bundle-testing.md)
   (`make bundle-run`). Create the target namespace first and install into it.
2. **In-cluster Deployment** from this repo —
   `IMG=<your-image> ./hack/deploy-incluster.sh "$NAMESPACE"` after CRDs/RBAC
   (`./hack/deploy-dev.sh "$NAMESPACE"`).

The generated CSV advertises AllNamespaces and the manager watches every
namespace. Recommended: put the CR in `cost-onprem` (same as the suggested
operator install NS). BYOI infra may live in other namespaces.

Do not run `make run` on a laptop against `*.svc.cluster.local` hosts. Database
and cache probes will stay False.

Confirm the manager is up:

```bash
export NAMESPACE=cost-onprem
oc -n "$NAMESPACE" get deploy,pods
oc get crd costmanagementserviceconfigs.service.costmanagement.openshift.io
```

## 1. Namespace and Secrets

```bash
export NAMESPACE=cost-onprem
oc get ns "$NAMESPACE" >/dev/null 2>&1 || oc new-project "$NAMESPACE"
```

Create the Secrets from [prerequisites](prerequisites.md) in `$NAMESPACE`
(`my-db-credentials`, `my-cache-credentials`, `my-s3-credentials`, and the UI
OAuth client Secret). Kafka SASL/TLS Secrets are only needed if you enabled
those protocols.

## 2. Copy and edit the minimal CR

The checked-in sample is
[`config/samples/service.costmanagement_v1alpha1_costmanagementserviceconfig_minimal.yaml`](../../config/samples/service.costmanagement_v1alpha1_costmanagementserviceconfig_minimal.yaml).

Replace at least:

- `metadata.namespace`
- `spec.global.clusterDomain` (or omit it and let discovery fill
  `status.discoveredConfig.clusterDomain` from the cluster Ingress)
- `spec.database.host` / `secretName`
- `spec.cache.host` / `auth.secretName`
- `spec.kafka.bootstrapServers`
- `spec.objectStorage.endpoint` / `secretName`
- `spec.auth.keycloak.url` (and `issuerURL` if token `iss` is the public Route)
- Image `repository` / `tag` values for your environment

Leave `spec.database.deploy` and `spec.cache.deploy` **false**. Leave
monitoring off for this pass (`monitoring.enabled: false` in the sample). Do
not add a `ros` block; the CRD default is `enabled: false`.

Example shape (hosts are placeholders):

```yaml
apiVersion: service.costmanagement.openshift.io/v1alpha1
kind: CostManagementServiceConfig
metadata:
  name: cost-management-minimal
  namespace: cost-onprem
spec:
  global:
    clusterDomain: "apps.cluster.example.com"
  database:
    deploy: false
    host: "postgresql.databases.svc.cluster.local"
    port: 5432
    sslMode: "require"
    secretName: "my-db-credentials"
  cache:
    deploy: false
    host: "redis.cache.svc.cluster.local"
    port: 6379
    auth:
      enabled: true
      secretName: "my-cache-credentials"
  kafka:
    bootstrapServers: "kafka-kafka-bootstrap.kafka.svc.cluster.local:9092"
  objectStorage:
    endpoint: "s3.openshift-storage.svc.cluster.local"
    port: 443
    useSSL: true
    secretName: "my-s3-credentials"
  auth:
    keycloak:
      url: "https://keycloak.auth.svc.cluster.local"
      realm: "kubernetes"
  # image repository/tag blocks: copy from the sample and pin your tags
  monitoring:
    enabled: false
```

Do not apply that abbreviated snippet (it omits required image blocks). Copy the
sample file, edit hosts and tags, then:

```bash
oc apply -f config/samples/service.costmanagement_v1alpha1_costmanagementserviceconfig_minimal.yaml
```

## 3. Watch conditions (not Phase)

Conditions are the API. `status.phase` stays `Progressing` until the UI is
ready, even when APIs are already up.

```bash
oc -n "$NAMESPACE" get cmsc
oc -n "$NAMESPACE" get cmsc cost-management-minimal -o jsonpath='{range .status.conditions[*]}{.type}={.status} ({.reason}){"\n"}{end}'
oc -n "$NAMESPACE" describe cmsc cost-management-minimal
```

| Condition | Day-one goal |
|-----------|----------------|
| `DiscoveryComplete` | True |
| `DatabaseReady`, `CacheReady` | True (blocking if False) |
| `StorageReady`, `KafkaReady` | True for a working upload path |
| `SchemaUpToDate` | True (migrations finished) |
| `RBACReady`, `IngressReady`, `GatewayReady` | True |
| `Available` | True (`KokuAvailable`) — **success for this quickstart** |
| `ROSEnabled` | False |
| `AuthenticationReady` | True once JWKS is reachable |
| `UIReady` | True only after the UI OAuth Secret exists |

If `DatabaseReady` or `CacheReady` is False, fix the Secret keys or network
path before waiting on Deployments.

## 4. Confirm workloads and URLs

```bash
oc -n "$NAMESPACE" get deploy,job,route
```

With CR name `cost-management-minimal` in namespace `cost-onprem`:

| URL | Host pattern |
|-----|----------------|
| API gateway | `https://cost-management-minimal-gateway-cost-onprem.{apps-domain}` (Route path `/api`) |
| UI | `https://cost-management-minimal-ui-cost-onprem.{apps-domain}/` |

The OpenShift console Application menu also gets a **Cost Management**
ConsoleLink when the UI Route exists.

## If something stays False

| Symptom | Typical cause |
|---------|----------------|
| `DatabaseSecretInvalid` / `CacheSecretInvalid` | Wrong Secret keys — see [prerequisites](prerequisites.md) (`redis-password`, all ten DB keys) |
| `DatabaseUnreachable` | Host/port not reachable from the operator pod (NetworkPolicy, wrong Service DNS) |
| `SchemaUpToDate` never True | Migration Job failed. List Jobs, then logs for the failed one: `oc -n "$NAMESPACE" get jobs` then `oc -n "$NAMESPACE" logs job/<cr>-koku-migrate` (beta Cost-only; RBAC is `{cr}-rbac-migrate`) |
| `Available` True but `UIReady` False | Missing `{cr}-ui-oauth-client` with `client-id` / `client-secret` |
| `StorageReady` False | Missing `access-key` / `secret-key`, or bucket does not exist |
| Uploads return 500 | Bucket `koku-bucket` (or your `bucketName`) was never created |

## Next

- Size and TLS: [production.md](production.md)
- Point Cost Management Metrics Operator at this instance: [cmmo.md](cmmo.md)
