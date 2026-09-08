# Pre-production install guide (BYOI → operator → UI)

End-to-end path for a **pre-prod / lab** OpenShift cluster: optional fixture
dependencies (BYOI), then the mandatory operator + `CostManagementServiceConfig`,
ending at a working Cost Management UI login.

Customer install/config (prerequisites, Secret keys, quickstart, production, CMMO):
[docs/install/](../install/README.md).

This is **not** an OLM Catalog / production packaging guide (COST-7695). It uses
the OwnNamespace model: **operator install namespace == CR namespace**. BYOI
infra may live elsewhere and is referenced only via CR connection fields.
See [ownnamespace.md](ownnamespace.md).

For a shorter **Cluster Bot day-one** path (Redpanda, no AMQ Streams / Keycloak),
see [clusterbot.md](clusterbot.md).

## Conventions (parameterized)

The worked example uses the checked-in BYOI sample names. Override as needed
(chart pytest often uses `cost-tests` / `cost-onprem`).

| Variable | Sample default | Meaning |
|----------|----------------|---------|
| `NAMESPACE` | `cost-byoi` | CR **and** operator namespace |
| `CR_NAME` | `cost-management` | `CostManagementServiceConfig` metadata.name |
| `INFRA_NAMESPACE` | `cost-byoi-infra` | Postgres, Valkey, MinIO (`./hack/deploy-byoi.sh`) |
| `KAFKA_NAMESPACE` | `kafka` | AMQ Streams cluster |
| `KEYCLOAK_NAMESPACE` | `keycloak` | RHBK (external; never owned by this operator) |

```bash
export NAMESPACE=cost-byoi
export CR_NAME=cost-management
export INFRA_NAMESPACE=cost-byoi-infra
# Chart-style / lab alternative:
# export NAMESPACE=cost-gold CR_NAME=cost-onprem INFRA_NAMESPACE=cost-gold-infra
```

UI Route host pattern:

`https://${CR_NAME}-ui-${NAMESPACE}.<apps-domain>/`

## Prerequisites

- OpenShift cluster with `oc` / `kubectl` and cluster-admin (or equivalent)
- Default StorageClass
- Ability to pull Red Hat registry images (`registry.redhat.io/…`) via the
  cluster pull secret
- A place to push a **linux/amd64** operator image (typical OCP nodes are amd64;
  Apple Silicon CRC is the opposite case — see [crc-testing.md](crc-testing.md))

Clone this repo and work from the root.

---

## Part A — Optional dependencies (BYOI)

Skip this part if you already have equivalent customer-managed services and can
point the CR at those endpoints.

### One-shot (recommended)

From the operator repo root (Keycloak script comes from the sibling
`cost-onprem-chart` checkout — override with `CHART_ROOT` / `RHBK_SCRIPT`):

```bash
export NAMESPACE=cost-byoi
export CR_NAME=cost-management
export INFRA_NAMESPACE=cost-byoi-infra
# Optional overrides: KAFKA_NAMESPACE, KEYCLOAK_NAMESPACE, STORAGE_CLASS, CHART_ROOT

./hack/deploy-byoi.sh
```

This runs A1–A4 plus app Secrets in `$NAMESPACE`:

1. AMQ Streams Kafka (`config/samples/byoi/deploy-kafka.sh`)
2. Postgres / Valkey / MinIO (`config/samples/byoi/infra`, namespace-overridable)
3. Keycloak / RHBK (`cost-onprem-chart` `scripts/deploy-rhbk.sh`) with
   `COST_MGMT_NAMESPACE` / `COST_MGMT_RELEASE_NAME` / UI redirect URL aligned
4. Mirror UI OAuth client Secret → `${NAMESPACE}/${CR_NAME}-ui-oauth-client`
5. Apply `byoi-*-credentials` Secrets into `$NAMESPACE`

Skip individual steps with `SKIP_KAFKA=1`, `SKIP_INFRA=1`, `SKIP_KEYCLOAK=1`,
or `SKIP_OAUTH_MIRROR=1`.

Credentials in the fixture YAMLs are **fixed test values — not for production**.

### Manual steps (same as the script)

<details>
<summary>Expand for A1–A4 run by hand</summary>

#### A1. Kafka (AMQ Streams)

```bash
STORAGE_CLASS=<your-sc> LOG_LEVEL=INFO ./config/samples/byoi/deploy-kafka.sh
# Bootstrap (default):
#   cost-onprem-kafka-kafka-bootstrap.kafka.svc:9092
```

Lightweight Redpanda alternative: see [config/samples/byoi/README.md](../../config/samples/byoi/README.md).

#### A2. Postgres, Valkey, MinIO

```bash
oc adm policy add-scc-to-user anyuid -z byoi-infra -n cost-byoi-infra 2>/dev/null || true
kubectl apply -k config/samples/byoi/infra

kubectl -n cost-byoi-infra rollout status deploy/postgresql --timeout=180s
kubectl -n cost-byoi-infra rollout status deploy/valkey --timeout=120s
kubectl -n cost-byoi-infra rollout status deploy/minio --timeout=120s
kubectl -n cost-byoi-infra wait --for=condition=complete job/minio-init --timeout=120s
```

#### A3. Keycloak / RHBK (required for UI login)

The operator never deploys Keycloak. From the **cost-onprem-chart** checkout:

```bash
export COST_MGMT_NAMESPACE="$NAMESPACE"
export COST_MGMT_RELEASE_NAME="$CR_NAME"
# Optional explicit override:
# export COST_MGMT_UI_BASE_URL="https://${CR_NAME}-ui-${NAMESPACE}.apps.example.com"

LOG_LEVEL=INFO ./scripts/deploy-rhbk.sh
```

Default realm user: **`admin` / `admin`**. Secret
`keycloak-client-secret-cost-management-ui` is created in the Keycloak namespace.

#### A4. Mirror UI OAuth client Secret

```bash
kubectl get ns "$NAMESPACE" >/dev/null 2>&1 || kubectl create ns "$NAMESPACE"

NAMESPACE="$NAMESPACE" CR_NAME="$CR_NAME" \
  ./config/samples/byoi/mirror-ui-oauth-secret.sh
```

Without this Secret, `UIReady` stays False and the UI Deployment is not applied.

</details>

When `$INFRA_NAMESPACE` is not `cost-byoi-infra`, set matching hosts in the CR
(`postgresql.${INFRA_NAMESPACE}.svc…`, etc.) before Part B.

---

## Part B — Mandatory: operator + CR

### B1. Build and push an amd64 operator image

```bash
export IMG=quay.io/<your-org>/koku-service-operator:preprod
docker buildx build --platform linux/amd64 -t "$IMG" --push .
```

### B2. Install CRDs, OwnNamespace RBAC, and run the operator in-cluster

`make run` / out-of-cluster controllers **cannot** resolve `*.svc.cluster.local`
BYOI hosts from a laptop. Use the in-cluster helper (binds `default` SA in
`$NAMESPACE`, same as `./hack/deploy-dev.sh` / `./hack/deploy-crc.sh`):

```bash
IMG="$IMG" ./hack/deploy-incluster.sh "$NAMESPACE"
```

That script:

1. Applies CRDs + `manager-role` / `manager-cluster-role` via `deploy-dev.sh`
2. Creates RoleBinding + ClusterRoleBinding for `$NAMESPACE:default`
3. Creates a lab-only TLS Secret (`koku-webhook-server-cert`) and mounts it at
   `/tmp/k8s-webhook-server/serving-certs` (required — the manager registers
   webhooks and CrashLoops without `tls.crt` / `tls.key`)
4. Deploys `koku-service-operator` in `$NAMESPACE` with `NAMESPACE` from the pod
   and `--operator-image=$IMG` (required for wait-for init containers)

This path does **not** install `ValidatingWebhookConfiguration` /
`MutatingWebhookConfiguration` (OLM + cert-manager do that for packaged
installs). The mount only lets the webhook *server* start so reconcile can run.

Watch logs:

```bash
kubectl -n "$NAMESPACE" logs -f deploy/koku-service-operator
```

`hack/deploy-incluster.sh` / `hack/deploy-dev.sh` bind `default` SA to
`manager-role`, `manager-cluster-role`, and the namespaced
`leader-election-role` (leases). Requires `openssl` on the machine running the
script (for the lab webhook cert).

Day-one Cluster Bot (Redpanda, no Keycloak): [clusterbot.md](clusterbot.md).

### B3. App Secrets, then the CR

If you used `./hack/deploy-byoi.sh`, app Secrets are already in `$NAMESPACE`.
Otherwise apply them first (retarget `metadata.namespace` if needed):

```bash
kubectl apply -f config/samples/byoi/app/secrets.yaml
```

Then apply the CR (edit hosts / domain / Keycloak issuer to match Part A):

```bash
# Edit before apply:
#   - metadata.namespace / metadata.name  → $NAMESPACE / $CR_NAME
#   - spec.global.clusterDomain           → apps.<your-cluster>
#   - database/cache/objectStorage hosts  → *.${INFRA_NAMESPACE}.svc…
#   - kafka.bootstrapServers              → bootstrap in $KAFKA_NAMESPACE
#   - spec.auth.keycloak.issuerURL        → public Keycloak issuer (iss)
#   - spec.auth.keycloak.tls              → caCertSecretName or insecureSkipVerify (dev)
kubectl apply -f config/samples/byoi/app/costmanagementserviceconfig.yaml
```

Required image fields (`repository` and `tag`). The operator does **not**
default workload images; omit a required field and reconcile sets
`Degraded=True` with reason `ImageNotSet`:

| Spec path | Purpose |
|-----------|---------|
| `database.image` | Bundled Postgres (`deploy: true` only) |
| `cache.image` | Bundled Valkey (`deploy: true` only) |
| `costManagement.api.image` | Koku (`masu.image` may inherit this) |
| `rbac.image` | Insights RBAC |
| `auth.envoy.image` | Gateway |
| `ingress.image` | Upload handler |
| `ui.app.image` | UI |
| `ui.oauthProxy.image` | oauth2-proxy sidecar |
| `ros.image` / `kruize.image` | Required only when `ros.enabled: true` |

Pin product or community images on the CR (see `config/samples`).

**CRD default and samples:** `spec.ros.enabled` defaults to **`false`** (beta is
Cost-only — no ROS/Kruize). Samples set `enabled: false` explicitly. That skips
ROS schema migrate, Kruize, and ROS workers so install paths do not need
ROS/Kruize images or ClusterRole escalation rights beyond what
`manager-cluster-role` already grants for cleanup.
Set `ros.enabled: true` (and fill ROS/Kruize images) only when you intentionally
opt in.

### B4. Wait for reconcile

```bash
kubectl -n "$NAMESPACE" get cmsc "$CR_NAME" -w
kubectl -n "$NAMESPACE" describe cmsc "$CR_NAME"
```

Useful conditions: `DatabaseReady`, `CacheReady`, `KafkaReady`,
`SchemaUpToDate`, `RBACReady` (API — this is what `Available` waits on),
`RBACWorkerReady` (Celery worker; does **not** gate `Available`),
`AuthenticationReady` (OIDC), `GatewayReady`, `IngressReady`,
`UIReady`, `Available`.

`Available=True` means the RBAC **API** and Koku API are up. A down RBAC
worker shows up as `RBACWorkerReady=False` and does not flip `Available`.

Phase is human-readable only — prefer conditions.

---

## Part C — Open the UI

```bash
oc -n "$NAMESPACE" get route "${CR_NAME}-ui" \
  -o jsonpath='https://{.spec.host}{"\n"}'
```

Open that URL. oauth2-proxy redirects to Keycloak; sign in with the realm user
(**`admin` / `admin`** from `deploy-rhbk.sh` defaults).

Sanity checks:

```bash
# Should 302 to Keycloak
curl -skI "https://$(oc -n "$NAMESPACE" get route "${CR_NAME}-ui" -o jsonpath='{.spec.host}')/"
```

---

## Part D — Seed test data (optional)

The UI is empty until a source has uploaded cost data. `./scripts/seed-test-data.sh`
registers an OpenShift source, generates NISE OCP data, and uploads it through
the gateway/ingress (no pytest):

```bash
NAMESPACE="$NAMESPACE" HELM_RELEASE_NAME="$CR_NAME" KEYCLOAK_NAMESPACE="$KEYCLOAK_NAMESPACE" \
  ./scripts/seed-test-data.sh --days 7
```

It sets up the venv from `test/pytest/requirements.txt`, installs `koku-nise`,
and reuses the `test/pytest` helpers. masu processes the upload asynchronously
off Kafka; data shows in the UI a few minutes later. The E2E suite
(`./scripts/run-pytest.sh --e2e` with `E2E_CLEANUP_*=false`) also seeds as a
side effect. See
[test/pytest/README.md](../../test/pytest/README.md#data-generation) and
[ui-development.md](ui-development.md).

---

## Common failures

| Symptom | Cause | Fix |
|---------|-------|-----|
| Probes fail / DB unreachable from laptop `make run` | No cluster DNS out-of-cluster | Use `./hack/deploy-incluster.sh` |
| `InvalidImageName` / image `:` | Missing `repository`/`tag` on UI, oauth-proxy, ingress, ROS, … | Set images in the CR (see sample) |
| Stuck creating Kruize `ClusterRole` (RBAC escalation) | ROS/Kruize applied with insufficient SA rights | Keep `ros.enabled: false` for UI smoke, or grant/hold the verbs Kruize’s role needs |
| `UIReady=False` | OAuth client Secret missing | Re-run `mirror-ui-oauth-secret.sh` |
| Login redirect_uri mismatch | Keycloak client built for wrong UI host | Re-run RHBK with `COST_MGMT_NAMESPACE` / `COST_MGMT_RELEASE_NAME` / `COST_MGMT_UI_BASE_URL` |
| ImagePullBackOff on amd64 node | arm64-only image | Rebuild with `--platform linux/amd64` |
| StorageClass list/watch forbidden | Stale cluster role | Re-apply `config/rbac/cluster_access_role.yaml` (`get;list;watch`) |
| CrashLoop: `open …/serving-certs/tls.crt: no such file` | Webhook server has no TLS mount | Re-run `./hack/deploy-incluster.sh` (creates/mounts `koku-webhook-server-cert`), or mount a Secret with `tls.crt`/`tls.key` at `/tmp/k8s-webhook-server/serving-certs` |
| Namespace stuck `Terminating` after `oc delete ns` | Operator died before the CR finalizer ran | [uninstall.md](../install/uninstall.md#if-the-namespace-is-already-terminating) |

## Tear down

Delete the CR first while the operator is still running. `hack/demo-preprod.sh --reset` already does this (and strips the finalizer if the operator is gone). Manual order and recovery: [uninstall.md](../install/uninstall.md).

```bash
oc -n "$NAMESPACE" delete cmsc "$CR_NAME" --timeout=180s
if oc -n "$NAMESPACE" get cmsc "$CR_NAME" >/dev/null 2>&1; then
  echo "CMSC still present; not deleting the namespace. See uninstall.md recovery." >&2
  exit 1
fi
oc delete ns "$NAMESPACE" "$INFRA_NAMESPACE" --ignore-not-found
```

## Related docs

- [ownnamespace.md](ownnamespace.md) — install/watch model and RBAC shape
- [crc-testing.md](crc-testing.md) — local CRC / out-of-cluster `make run`
- [uninstall.md](../install/uninstall.md) — CR-first uninstall and stuck-namespace recovery
- [config/samples/byoi/README.md](../../config/samples/byoi/README.md) — fixture details, monitoring, teardown
