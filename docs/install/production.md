# Production deployment

How to run a lasting BYOI deploy of Cost Management on OpenShift. This is
**not** a claim that every Jira “HA / monitoring / NetworkPolicy” item is
implemented. Read the honesty notes in each section.

**Beta.** Keep `spec.ros.enabled: false`. Do not treat `spec.profile: ha` as a
full-stack HA switch.

Prerequisites and a first CR: [prerequisites.md](prerequisites.md),
[quickstart.md](quickstart.md).

## Production-shaped sample

Use
[`config/samples/service.costmanagement_v1alpha1_costmanagementserviceconfig_production.yaml`](../../config/samples/service.costmanagement_v1alpha1_costmanagementserviceconfig_production.yaml)
as the template. It demonstrates:

- External DB, cache, Kafka, S3, OIDC (`deploy: false`)
- `spec.profile: ha` (see [HA profile](#ha-profile) — UI only today)
- `monitoring.enabled: true`
- Explicit `spec.*.resources` and replica counts on core workloads
- SASL_SSL Kafka, cache TLS, S3 CA
- `ros.enabled: false`

Pin **release** image tags. The sample’s ingress tag `master` is a placeholder
and is not a production pin.

## HA profile

`spec.profile` is `standard` (default) or `ha`.

**Today `ha` only raises default UI CPU/memory** when `spec.ui.*.resources` are
unset. Koku API, Masu, Listener, RBAC, Celery, Envoy, and Ingress do **not**
read the profile. Size those with explicit `spec.*.replicas` and
`spec.*.resources` (as the production sample does).

Shared profile maps for the rest of the stack are not in this operator yet.
Until they are, copy the production sample’s resource blocks rather than
expecting `profile: ha` to resize workers.

For true HA of Postgres, Kafka, and object storage, use your infrastructure
operators (AMQ Streams, ODF, and so on). This operator does not manage them.

## Resource tuning

Set requests/limits on the components you care about. The production sample
uses replica `2` on API, Masu, Listener, Envoy, RBAC, and UI, and raises Celery
worker replica counts on the on-prem queues.

SaaS-only Celery queues (`hcs`, `subsExtraction`, `subsTransmission`) default
to `replicas: 0`. Leave them at zero.

`spec.costManagement.dataRetentionMonths` (default `4`, range 1–60) is how many
months of cost report data Koku retains. That is application retention, not a
database backup policy.

## TLS

Minimum crypto controls for production BYOI. The
[production sample](../../config/samples/service.costmanagement_v1alpha1_costmanagementserviceconfig_production.yaml)
already enables TLS on database, cache, Kafka, and S3 — copy those fields
rather than inventing a weaker config.

Prefer CA Secrets over `insecureSkipVerify` / `sslMode: disable`.
`insecureSkipVerify: true` and `sslMode: disable` are **never** acceptable in
production.

| Path | Required | Recommended | Secret key |
|------|----------|-------------|------------|
| PostgreSQL | `database.sslMode: require` | `verify-ca` or `verify-full` with a server CA | — |
| Cache | `cache.tls.enabled: true` | `caCertSecretName` so the client verifies the cache cert | `ca.crt` |
| Kafka | `securityProtocol: SASL_SSL` (or `SSL`) | `kafka.tls.caCertSecret` for broker CA verification | `ca.crt` |
| S3 | `objectStorage.useSSL: true` | `caCertSecretName` when the endpoint uses a custom CA | `ca.crt` |
| Keycloak / issuer | `auth.keycloak.url` must use HTTPS; `issuerURL` must use HTTPS when set | `auth.keycloak.tls.caCertSecretName` (skip CA only in lab) | `ca.crt` |
| API Route | Edge TLS + `insecureEdgeTerminationPolicy: Redirect` (CRD allows `edge` only) | Custom router cert via OpenShift Route / ingress controller | OpenShift router |
| UI Route | Passthrough so oauth2-proxy terminates TLS | Leave as-is; Service CA on the UI Service | Service CA |

CMMO uploads to this instance must use an HTTPS `api_url` against the gateway
Route. See [cmmo.md](cmmo.md).

Gateway Route host defaults to `{name}-gateway-{namespace}.{clusterDomain}`
with path `/api` and a 180s router timeout.

## Encryption at rest

At-rest encryption is **customer-owned (BYOI)**. This operator does not
provision encrypted volumes, buckets, or etcd. It connects to infrastructure
you already hardened. Enable encryption on each store **before** you apply the
CR; the TLS settings above only protect data in transit.

| Store | Recommendation | Who owns it |
|-------|----------------|-------------|
| PostgreSQL volumes | OpenShift ODF encrypted StorageClass, or LUKS-backed block storage under the database | Your DBA / storage operator |
| Object storage buckets | Provider bucket encryption (for example AWS SSE-S3/KMS, NooBaa, Ceph RGW) | Your S3 / ODF admin — this operator does not create buckets |
| Kafka topics | Broker / disk encryption via AMQ Streams (or equivalent) | Your Kafka operator |
| Kubernetes Secrets | [OpenShift etcd encryption](https://docs.openshift.com/container-platform/latest/security/encrypting-etcd.html) so credential Secrets are not plaintext in etcd | Cluster admin |
| Operator-generated Secrets | Same etcd encryption; also export `{cr}-django-secret` and `{cr}-ui-cookie-secret` outside the cluster (see [Backup and restore](#backup-and-restore)) | You |

Do not put database, cache, Kafka, or S3 passwords in the CR spec. Use
`database.secretName`, `cache.auth.secretName`, `kafka.sasl.existingSecret`,
and `objectStorage.secretName` to reference Kubernetes Secrets. Bundled
`database.deploy` / `cache.deploy` volumes are for lab and CI; they are not a
production at-rest story.

## NetworkPolicies

The operator applies **ingress** NetworkPolicies for:

- Gateway (Envoy)
- Ingress
- RBAC API
- Koku API
- UI
- Listener
- Masu

It does **not** cover Celery workers/beat. External Postgres, Kafka, cache, and
S3 in other namespaces are **your** NetworkPolicies (the operator does not
watch those namespaces).

Bundled DB/cache NetworkPolicies exist only when `database.deploy` /
`cache.deploy` are true (dev). Production BYOI should restrict those services
on the infrastructure side.

ROS/Kruize policies are applied only when `ros.enabled` is true — leave them
off for beta.

## Monitoring

`spec.monitoring.enabled` defaults to **true**. When true, the operator applies
a `ServiceMonitor` and a `PrometheusRule` **if** the Prometheus Operator CRDs
exist. If those CRDs are absent, the stage is skipped and reconcile continues.

What exists today:

- App ServiceMonitor (port `metrics` / 9000) selecting several application
  Services
- Five alert rules, including `CostManagementMigrationFailed` and
  `CostManagementDegraded`
- A Kruize ServiceMonitor is still applied even with ROS off (no scrape target
  in beta)

What is **not** a complete in-cluster monitoring story yet: many scrape
targets do not match Service ports/labels, custom operator metrics are not
shipped, and you should not expect every workload to appear in Prometheus.
Use CR conditions and Kubernetes Events (`Ready`, `MigrationStarted` /
`Complete` / `Failed`, `ReconcileError`) as the supported operator signals.

To skip Prometheus CRs entirely:

```yaml
spec:
  monitoring:
    enabled: false
```

## Backup and restore

The operator does **not** back up or restore anything. You own:

| Data | Typical backup |
|------|----------------|
| PostgreSQL (`costonprem_koku`, `costonprem_rbac`; plus ROS/Kruize DBs only if enabled) | Your DBA / Postgres operator |
| Object storage buckets | Bucket versioning / replication on the S3 provider |
| Kafka topics | AMQ Streams / Kafka ops (offset and topic config) |
| OIDC (Keycloak) | RHBK backup |
| Operator-generated Secrets (`{cr}-django-secret`, `{cr}-ui-cookie-secret`) | Export and store outside the cluster; the operator will not recreate them if you delete them after first create |

Restore order: restore Postgres and buckets, restore Secrets into the CR
namespace, then re-apply the CR (or let the operator reconcile). Schema Jobs
are not re-run for a tag that already succeeded.

## Operator behavior you should plan for

**Server-Side Apply with force.** The operator owns every field it sets on
namespace-scoped objects. Manual `kubectl edit` of those fields is reverted on
the next reconcile (including a 5-minute drift requeue). Change desired state
in the CR, not on child Deployments.

**Secrets are create-only.** User Secrets (`my-db-credentials`, …) are never
overwritten. Operator-generated Secrets are created once. Updating a password
in a Secret you own takes effect on the next pod restart; the operator does not
roll pods on Secret rotation yet.

**Images.** Changing a service image tag can retrigger that service’s
migration Job. Pin tags (and digests when you can).

**Pause.** Annotation
`costmanagementserviceconfigs.service.costmanagement.openshift.io/pause=true`
on the CR (key `…/pause`, value `true`) halts reconcile (`Paused` condition).
Remove it to resume.

## Uninstall

Delete the `CostManagementServiceConfig` and wait until it is gone **before**
deleting the namespace or the operator. The operator lives in the CR namespace;
killing it first leaves the CR finalizer stuck and leaks the ConsoleLink.
Full order and recovery: [uninstall.md](uninstall.md).

## Next

Point reporting clusters at this instance: [cmmo.md](cmmo.md).
