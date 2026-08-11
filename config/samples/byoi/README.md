# BYOI test fixture

Dev-only manifests that stand up external infrastructure in one namespace and a
separate `CostManagementServiceConfig` (BYOI mode) in another. The operator
does **not** provision DB/cache; it only connects.

| Namespace | Contents |
|-----------|----------|
| `cost-byoi-infra` | PostgreSQL, Valkey, MinIO (+ optional Redpanda — see below) |
| `kafka` | AMQ Streams (recommended) — via `deploy-kafka.sh`, not the infra kustomize |
| `cost-byoi` | App Secrets + `CostManagementServiceConfig` **and** the OwnNamespace operator instance |

The operator install NS must be the CR NS (`cost-byoi` here). BYOI infra in
`cost-byoi-infra` / `kafka` is connected via CR fields; the operator does not
watch or own those namespaces. See
[docs/development/ownnamespace.md](../../docs/development/ownnamespace.md).

**Kafka options**

1. **AMQ Streams (recommended)** — `./config/samples/byoi/deploy-kafka.sh`  
   Required for chart infra tests (`strimzi.io/kind=Kafka`, KafkaTopics).
2. **Lightweight Redpanda** — `config/samples/byoi/infra/kafka.yaml`  
   Kafka-API compatible, emptyDir, no OLM. Fine for smoke connectivity; **not**
   enough for AMQ Streams–specific pytest. Not in the default infra kustomize.

Keycloak/OIDC is external (BYOI), same as Kafka. The sample CR has a
placeholder `spec.auth.keycloak.url`. The operator never deploys RHBK and
never invents the UI OAuth client Secret — that Secret must be mirrored from
Keycloak into the CR namespace (see **UI OAuth Secret** below). The operator
only creates the UI cookie Secret and sets `UIReady` when the client Secret
is present.

**Credentials in these YAMLs are fixed test values — not for production.**

## Prerequisites

- Operator installed and running on the cluster
- `kubectl` (or `oc`) with cluster-admin (or enough rights for Deployments/PVCs/SCC)
- Default StorageClass available
- AMQ Streams Kafka cluster (see below)

On OpenShift, grant `anyuid` to the infra ServiceAccount (official postgres /
MinIO images need it):

```bash
oc adm policy add-scc-to-user anyuid -z byoi-infra -n cost-byoi-infra
```

## Kafka (AMQ Streams)

Deploy Kafka **before** applying the BYOI CR. Use the copy of the chart’s
deploy script vendored next to this fixture (see source comment in the file):

```bash
# From the operator repo root. Defaults: KAFKA_NAMESPACE=kafka, cluster=cost-onprem-kafka
STORAGE_CLASS=gp3-csi LOG_LEVEL=INFO ./config/samples/byoi/deploy-kafka.sh
# Bootstrap written to /tmp/kafka-bootstrap-servers.env — typically:
#   cost-onprem-kafka-kafka-bootstrap.kafka.svc:9092
```

Point `spec.kafka.bootstrapServers` at that bootstrap address (the sample CR
already uses the default). For chart pytest against an operator deploy:

```bash
export KAFKA_NAMESPACE=kafka
```

Tear down Kafka separately when finished:

```bash
./config/samples/byoi/deploy-kafka.sh cleanup
```

### Lightweight alternative (Redpanda)

Single-node Redpanda in `cost-byoi-infra` — optional, not part of
`kubectl apply -k config/samples/byoi/infra`:

```bash
# Needs anyuid on byoi-infra (same as postgres/minio)
kubectl apply -f config/samples/byoi/infra/kafka.yaml
kubectl -n cost-byoi-infra rollout status deploy/kafka --timeout=180s
```

Point the CR at Redpanda instead of AMQ Streams:

```yaml
spec:
  kafka:
    bootstrapServers: "kafka.cost-byoi-infra.svc.cluster.local:9092"
    securityProtocol: "PLAINTEXT"
```

Do **not** use `KAFKA_NAMESPACE=cost-byoi-infra` for chart kafka suite expectations
that require Strimzi; use AMQ Streams for those.

## UI OAuth Secret

After Keycloak/RHBK is up (e.g. chart `scripts/deploy-rhbk.sh`), mirror the
UI confidential client into the CR namespace before expecting `UIReady=True`:

```bash
# Defaults: KEYCLOAK_NAMESPACE=keycloak, NAMESPACE=cost-byoi, CR_NAME=cost-management
# Target Secret: {CR_NAME}-ui-oauth-client (keys client-id / client-secret)
./config/samples/byoi/mirror-ui-oauth-secret.sh

# Chart pytest / hybrid CR example:
NAMESPACE=cost-tests CR_NAME=cost-onprem ./config/samples/byoi/mirror-ui-oauth-secret.sh
```

Override the Secret name with `spec.ui.oauthClientSecretRef.name` if needed.
Cookie session Secret (`{cr}-ui-cookie-secret`) is still created by the operator.

## Apply

```bash
# 0. Kafka — AMQ Streams (recommended) or Redpanda (lightweight); see above
# 0b. Keycloak (external) + mirror UI OAuth Secret — see UI OAuth Secret above

# 1. Infrastructure (Postgres, Valkey, MinIO)
kubectl apply -k config/samples/byoi/infra

# Wait until pods are Ready (adjust timeout as needed)
kubectl -n cost-byoi-infra rollout status deploy/postgresql --timeout=180s
kubectl -n cost-byoi-infra rollout status deploy/valkey --timeout=120s
kubectl -n cost-byoi-infra rollout status deploy/minio --timeout=120s
kubectl -n cost-byoi-infra wait --for=condition=complete job/minio-init --timeout=120s

# 2. App secrets FIRST (must exist before the operator reconciles the CR),
#    then the BYOI CostManagementServiceConfig
kubectl apply -f config/samples/byoi/app/secrets.yaml
kubectl apply -f config/samples/byoi/app/costmanagementserviceconfig.yaml
# Or together after secrets are present:
# kubectl apply -k config/samples/byoi/app
```

## Watch

```bash
kubectl -n cost-byoi get cmsc cost-management -w
kubectl -n cost-byoi describe cmsc cost-management
kubectl -n op-sdk-scaffold-system logs deploy/op-sdk-scaffold-controller-manager -f
```

## Monitoring (optional)

Standalone Prometheus + Grafana in `cost-byoi-infra`. No Prometheus Operator required — uses static scrape configs targeting port 9000 on each service.

```bash
kubectl apply -k config/samples/byoi/monitoring
```

Access Grafana (anonymous admin, no login needed):
```bash
kubectl -n cost-byoi-infra port-forward svc/grafana 3000:3000
# open http://localhost:3000
```

Access Prometheus:
```bash
kubectl -n cost-byoi-infra port-forward svc/prometheus 9090:9090
# open http://localhost:9090
```

> **Note:** Scrape targets reference CR name `cost-management` in namespace `cost-byoi`.
> If your CR name or namespace differs, edit `monitoring/prometheus.yaml` before applying.

When COST-7692 ServiceMonitors are implemented, this fixture can be replaced by enabling
OpenShift user workload monitoring (`enableUserWorkload: true` in `cluster-monitoring-config`).

## Tear down

```bash
kubectl delete -k config/samples/byoi/app --ignore-not-found
kubectl delete -k config/samples/byoi/infra --ignore-not-found
kubectl delete -k config/samples/byoi/monitoring --ignore-not-found
# Kafka:
#   ./config/samples/byoi/deploy-kafka.sh cleanup
```

## Endpoints (from `cost-byoi`)

| Service | Address |
|---------|---------|
| PostgreSQL | `postgresql.cost-byoi-infra.svc.cluster.local:5432` |
| Valkey | `valkey.cost-byoi-infra.svc.cluster.local:6379` |
| Kafka (AMQ Streams, recommended) | `cost-onprem-kafka-kafka-bootstrap.kafka.svc.cluster.local:9092` |
| Kafka (Redpanda, optional) | `kafka.cost-byoi-infra.svc.cluster.local:9092` |
| MinIO (S3) | `minio.cost-byoi-infra.svc.cluster.local:9000` (HTTP) |
| Prometheus | `prometheus.cost-byoi-infra.svc.cluster.local:9090` (when monitoring applied) |
| Grafana | `grafana.cost-byoi-infra.svc.cluster.local:3000` (when monitoring applied) |
