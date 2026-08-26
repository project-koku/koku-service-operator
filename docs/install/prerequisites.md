# Prerequisites

What must exist **before** you apply a `CostManagementServiceConfig`. The
operator does not provision production PostgreSQL, Kafka, object storage, or
OIDC. It connects to services you already run.

**Beta.** Leave `spec.ros.enabled: false`. ROS and Kruize workloads are not
supported. Cost-only application databases are `costonprem_koku` and
`costonprem_rbac`. Do not create a database named `postgres` for Cost
Management. The operator currently still validates unused Secret keys
(`postgres-user`, `ros-*`, `kruize-*`) even when ROS is off.

Companion guides: [quickstart](quickstart.md), [production](production.md),
[CMMO](cmmo.md).

## What you need

| Dependency | Who owns it | Operator role |
|------------|-------------|----------------|
| OpenShift cluster | You | Discovers cluster domain and default StorageClass |
| PostgreSQL | You | TCP probe + credentials; runs schema Jobs |
| Redis-compatible cache (Valkey/Redis) | You | TCP probe + credentials |
| Kafka (AMQ Streams recommended) | You | TCP probe; Listener consumes `platform.upload.announce` |
| S3-compatible object storage | You | Validates `access-key` / `secret-key`; does **not** create buckets |
| OIDC (Keycloak / RHBK) | You | JWKS probe; Envoy + UI oauth2-proxy |
| This operator | You, in the CR namespace | Deploys Cost Management application services |

`spec.database.deploy: true` and `spec.cache.deploy: true` are for local
development and CI only. Do not use them in production. Kafka is never bundled.

## OpenShift

- Cluster-admin (or equivalent) to install the operator and create the CR
  namespace
- Ability to pull application images (`registry.redhat.io`, Quay) via the
  cluster pull secret
- A default StorageClass is only required if you later enable bundled
  Postgres/Valkey (dev). Production BYOI does not need it for Cost Management
  itself

Install the operator into the **same namespace** as the CR. Runtime is
OwnNamespace even though a published OperatorHub catalog is not available yet.
See [quickstart.md](quickstart.md#install-the-operator).

## PostgreSQL

The operator connects to a single host (`spec.database.host`, default port
`5432`). `spec.database.deploy` defaults to `false`. Application workloads use
**named databases** with **separate application users**. They do **not** use
PostgreSQL's maintenance database `postgres`.

| Database | User Secret keys | Used when |
|----------|------------------|-----------|
| `costonprem_koku` | `koku-user`, `koku-password` | Always (API, Masu, Listener, Celery, koku migrate) |
| `costonprem_rbac` | `rbac-user`, `rbac-password` | Always (RBAC API, worker, rbac migrate) |
| `costonprem_ros` | `ros-user`, `ros-password` | Only if `ros.enabled: true` |
| `costonprem_kruize` | `kruize-user`, `kruize-password` | Only if `ros.enabled: true` |

Beta (`ros.enabled: false`) needs only the koku and rbac databases. Do not
create `postgres`, `costonprem_ros`, or `costonprem_kruize` as Cost Management
application databases.

The operator still requires extra Secret keys today (`postgres-user` /
`postgres-password`, plus `ros-*` and `kruize-*` even when ROS is off).
`postgres-user` is an admin/bootstrap pair for bundled DB init and Kruize
partition init — not a fifth database. Schema migration Jobs use the
application users (`koku-user`, `rbac-user`).

`spec.database.sslMode` is one of `disable`, `require`, `verify-ca`,
`verify-full` (CRD default `disable`). Use `require` or stricter in production.

### Secret: `spec.database.secretName`

Create this Secret in the **CR namespace**. All ten keys are required today,
including unused `postgres-*`, `ros-*`, and `kruize-*` keys. Missing keys set
`DatabaseReady=False` with reason `DatabaseSecretInvalid` and block
migrations.

| Key | Purpose |
|-----|---------|
| `koku-user` | Owner of `costonprem_koku` |
| `koku-password` | |
| `rbac-user` | Owner of `costonprem_rbac` |
| `rbac-password` | |
| `postgres-user` | Admin / bootstrap only (bundled init, Kruize). Unused by Cost-only workloads |
| `postgres-password` | |
| `ros-user` | Owner of `costonprem_ros` (validated even when ROS is off) |
| `ros-password` | |
| `kruize-user` | Owner of `costonprem_kruize` (validated even when ROS is off) |
| `kruize-password` | |

```bash
kubectl -n "$NAMESPACE" create secret generic my-db-credentials \
  --from-literal=postgres-user=postgres \
  --from-literal=postgres-password='<password>' \
  --from-literal=koku-user=koku \
  --from-literal=koku-password='<password>' \
  --from-literal=rbac-user=rbac_user \
  --from-literal=rbac-password='<password>' \
  --from-literal=ros-user=ros_user \
  --from-literal=ros-password='<password>' \
  --from-literal=kruize-user=kruize_user \
  --from-literal=kruize-password='<password>'
```

Missing keys set `DatabaseReady=False` with reason `DatabaseSecretInvalid` and
block migrations.

## Cache (Valkey / Redis)

Point `spec.cache.host` at an existing Redis protocol endpoint (default port
`6379`). Set `spec.cache.auth.enabled: true` and `spec.cache.auth.secretName`.

### Secret: `spec.cache.auth.secretName`

| Key | Required |
|-----|----------|
| `redis-password` | Yes |
| `redis-username` | Optional (ACL username) |

The key name is `redis-password`, not `password`.

```bash
kubectl -n "$NAMESPACE" create secret generic my-cache-credentials \
  --from-literal=redis-password='<password>'
```

Optional TLS: `spec.cache.tls.enabled: true` and
`spec.cache.tls.caCertSecretName` with key `ca.crt`.

Unreachable cache or a missing `redis-password` sets `CacheReady=False` and
blocks the pipeline.

## Kafka

Kafka is always external. `spec.kafka.bootstrapServers` is **required** (no
CRD default). Set it to the AMQ Streams bootstrap (for example
`cost-onprem-kafka-kafka-bootstrap.kafka.svc.cluster.local:9092`).

**Required topic (Cost / beta):** `platform.upload.announce`  
Ingress publishes upload announcements here; the Listener consumes it. Create
the topic before applying the CR. The operator does not create KafkaTopic CRs.

Additional topics used only on the ROS-enabled operator path
(`hccm.ros.events`, `rosocp.kruize.recommendations`), plus the
Sources-listener topic `platform.sources.event-stream`, are unused on the
Cost-only path. `platform.payload-status` is also unused there.

`spec.kafka.securityProtocol` is one of `PLAINTEXT`, `SSL`, `SASL_PLAINTEXT`,
`SASL_SSL` (default `PLAINTEXT`).

### Secret: `spec.kafka.sasl.existingSecret` (when SASL is enabled)

| Key | Required |
|-----|----------|
| `username` | Yes |
| `password` | Yes |

```bash
kubectl -n "$NAMESPACE" create secret generic my-kafka-sasl-credentials \
  --from-literal=username='<user>' \
  --from-literal=password='<password>'
```

Optional TLS CA: `spec.kafka.tls.enabled: true` and
`spec.kafka.tls.caCertSecret` with key `ca.crt`.

Kafka probe failures set `KafkaReady=False` but do **not** block later stages
(pods retry with init containers). A missing SASL Secret still fails
`KafkaReady`.

## Object storage (S3-compatible)

Set `spec.objectStorage.endpoint` (hostname only, no scheme or port),
`port`, `useSSL`, and `secretName`. For beta, provide credentials yourself;
do not rely on OBC/NooBaa auto-detection.

**The operator never creates buckets.** Create them before uploads, or Ingress
returns HTTP 500. When `secretName` is set, `endpoint`, `buckets.koku`, and
`buckets.ingress` are required.

| Bucket | Spec field | Beta |
|--------|------------|------|
| Cost / Koku | `spec.objectStorage.buckets.koku` | Required |
| Ingress uploads | `spec.objectStorage.buckets.ingress` | Required |
| ROS | `spec.objectStorage.buckets.ros` | Required only if `ros.enabled: true` |

### Secret: `spec.objectStorage.secretName`

| Key | Required |
|-----|----------|
| `access-key` | Yes |
| `secret-key` | Yes |

```bash
kubectl -n "$NAMESPACE" create secret generic my-s3-credentials \
  --from-literal=access-key='<key>' \
  --from-literal=secret-key='<secret>'
```

Optional TLS CA: `spec.objectStorage.caCertSecretName` with key `ca.crt`.
`insecureSkipVerify` is for lab self-signed endpoints only.

Missing keys set `StorageReady=False` (`StorageSecretInvalid`). S3 failures do
not block the rest of the pipeline, but uploads will not work.

## OIDC (Keycloak / RHBK)

The operator never deploys Keycloak. Configure a realm (samples use
`kubernetes`) and JWT audiences that match Envoy:

- `cost-management-operator`
- `cost-management-ui`

| Spec field | Purpose |
|------------|---------|
| `spec.auth.keycloak.url` | **Required.** In-cluster base URL used to fetch JWKS (prefer a Service URL so Envoy does not depend on the router). Must be `http://` or `https://` |
| `spec.auth.keycloak.issuerURL` | Token `iss` value. Set this to the public Route URL when RHBK issues tokens with that `iss` even if clients talk to the in-cluster Service |
| `spec.auth.keycloak.realm` | Default `kubernetes` |
| `spec.auth.keycloak.audiences` | Default `cost-management-operator`, `cost-management-ui` |

Optional TLS: `spec.auth.keycloak.tls.caCertSecretName` with key `ca.crt`
(needed when `issuerURL` is a Route and the cluster service CA does not trust
it). `insecureSkipVerify` is lab-only.

When `url` is missing, the webhook/CRD reject the CR. When `url` is set, a
failed JWKS probe sets `AuthenticationReady=False`. That does not by itself
block `GatewayReady`.

### UI OAuth client Secret

The UI sidecar (oauth2-proxy) needs a confidential OIDC client. Create a
Secret in the CR namespace. Default name: `{metadata.name}-ui-oauth-client`
(override with `spec.ui.oauthClientSecretRef.name`).

| Key | Required |
|-----|----------|
| `client-id` | Yes |
| `client-secret` | Yes |

Redirect URI the client must allow:

`https://{cr-name}-ui-{namespace}.{apps-domain}/oauth2/callback`

Until this Secret exists with both keys, `UIReady` stays False
(`OAuthClientSecretMissing`) and `status.phase` stays `Progressing`. Core APIs
can still become `Available`.

```bash
kubectl -n "$NAMESPACE" create secret generic cost-management-minimal-ui-oauth-client \
  --from-literal=client-id='<ui-client-id>' \
  --from-literal=client-secret='<ui-client-secret>'
```

Name the Secret `{cr-name}-ui-oauth-client` to match the CR `metadata.name`, or
set `spec.ui.oauthClientSecretRef`.

## Optional: RBAC bootstrap admin

If you set `spec.rbac.bootstrapAdmin.enabled: true`, also create a Secret
referenced by `spec.rbac.bootstrapAdmin.secretRef`:

| Key | Required |
|-----|----------|
| `org-id` | Yes |
| `account-number` | Yes |
| `username` | Yes |

Skip this for a minimal Cost-only start. Leave `enabled` unset/false.

## Secrets the operator creates (do not pre-create unless you intend to keep them)

These are **create-only**: the operator writes them if absent and never
overwrites them on later reconciles.

| Secret | Keys | Purpose |
|--------|------|---------|
| `{cr}-django-secret` | `secret-key` | Django `SECRET_KEY` |
| `{cr}-ui-cookie-secret` | `session-secret` | oauth2-proxy cookie encryption |

Rotate them yourself if needed; there is no operator annotation to roll them
yet.

## Versions tested in the lab fixture (not a support matrix)

These are what the in-repo BYOI fixture and Kafka script use. They are **not**
certified support statements.

| Component | Lab fixture |
|-----------|-------------|
| PostgreSQL | 16 |
| Valkey | 8 |
| AMQ Streams channel / Kafka | `amq-streams-3.1.x` / Kafka 4.1.0 |
| OpenShift | whatever cluster you install the operator on |

## Next

1. Install the operator in the CR namespace — [quickstart](quickstart.md#install-the-operator)
2. Apply the minimal CR — [quickstart](quickstart.md)
