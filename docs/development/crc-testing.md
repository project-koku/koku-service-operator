# Testing on CRC (CodeReady Containers / OpenShift Local)

CRC provides a single-node OpenShift cluster for local development. The
operator runs locally (out-of-cluster) and talks to CRC via kubeconfig.

Two paths:

| Path | What it exercises | Section |
|------|-------------------|---------|
| **`./hack/demo-preprod.sh --crc`** | Full BYOI stack (AMQ Streams + Keycloak + koku + RBAC + ingress + Envoy + UI) with the **in-cluster** operator — the same flow as the `clusterbot` demo, on the local arm64 node | [Full BYOI demo on CRC](#full-byoi-demo-on-crc) |
| **`make run` + a bundled sample CR** | Operator out-of-cluster, operator-provisioned Postgres/Valkey, no Kafka/Keycloak | rest of this document |

**Scope:** use the minimal koku-only path for **operator + koku API/celery**
iteration. For full UI, Kafka, Keycloak, and nginx timeout E2E, use the
[Full BYOI demo on CRC](#full-byoi-demo-on-crc) below or
[clusterbot.md](clusterbot.md).

## Quick start (minimal koku-only)

Two terminals, one step at a time:

```bash
# 0. Preflight (every new shell)
export KUBECONFIG=${HOME}/.crc/machines/crc/kubeconfig
oc get --raw /healthz   # must print: ok

# 1. Bootstrap CRD/RBAC (once per CRC restart)
make crc-dev CRC_NAMESPACE=cost-onprem

# 2. Build + push operator image (init containers need a real image in-cluster)
make crc-operator-image CRC_NAMESPACE=cost-onprem

# 3. Terminal A — operator
NAMESPACE=cost-onprem IMG=default-route-openshift-image-registry.apps-crc.testing/cost-onprem/koku-service-operator:dev make run

# 4. Terminal B — minimal CR (bundled DB/cache; no UI/Kafka/Keycloak)
oc apply -n cost-onprem \
  -f config/samples/dev/service.costmanagement_v1alpha1_costmanagementserviceconfig_crc_minimal.yaml

oc get pods -n cost-onprem -w
```

Custom koku build (feature branch on Apple Silicon):

```bash
docker build --platform linux/arm64 -t default-route-openshift-image-registry.apps-crc.testing/cost-onprem/koku:my-tag .
./hack/push-image-crc.sh default-route-openshift-image-registry.apps-crc.testing/cost-onprem/koku:my-tag
# patch spec.costManagement.api/masu.image in the CR, then:
oc delete job -n cost-onprem cost-management-koku-migrate --ignore-not-found
```

## Full BYOI demo on CRC

`./hack/demo-preprod.sh --crc` runs the [pre-prod demo](pre-prod-install.md)
against local CRC. It layers `hack/demo-preprod.crc.env` under the normal
settings:

- `KUBE_CONTEXT=crc` (create it once — see below)
- `BUILD_MODE=openshift` — the operator image is built **on** the CRC node, so
  it is natively arm64 (no cross build, no external registry)
- **arm64 workload image overrides** — the BYOI sample pins amd64-only builds of
  koku, `insights-rbac`, ingress and `koku-ui-onprem` that segfault / fail to
  pull on the arm64 node; the profile points them at arm64 rebuilds under
  `quay.io/martin_povolny/*`. Envoy (`proxyv2-rhel9`) and `oauth2-proxy-rhel9`
  are Red Hat multi-arch and are left as-is.
- **1+1 Kafka** (`KAFKA_BROKER_REPLICAS`/`KAFKA_CONTROLLER_REPLICAS=1`,
  10Gi/5Gi PVCs) instead of 3+3 / 360Gi, which never binds on the single CRC
  disk.

The amd64 / `clusterbot` path is unaffected: without `--crc` the profile file is
never read and the rendered CR is byte-identical to before.

### One-time: create the `crc` kube context

```bash
crc start -p ~/.crc-secret.json
oc login -u kubeadmin -p "$(crc console --credentials | sed -n 's/.*-p \([^ ]*\).*/\1/p' | tail -1)" \
    https://api.crc.testing:6443 --insecure-skip-tls-verify=true
oc config set-context crc --cluster=api-crc-testing:6443 \
    --user=kubeadmin/api-crc-testing:6443 --namespace=default
```

`oc login` refreshes the token; re-run it (not `set-context`) if the context
later reports `Unauthorized`.

### Run it

```bash
# needs the sibling cost-onprem-chart checkout for scripts/deploy-rhbk.sh
./hack/demo-preprod.sh --crc --dry-run     # show the plan
./hack/demo-preprod.sh --crc               # tmux: steps + two klock panes
./hack/demo-preprod.sh --crc --reset       # tear the four namespaces down first
```

Override any image from the profile with a matching env var, e.g.
`DEMO_KOKU_IMAGE=quay.io/you/koku DEMO_KOKU_TAG=arm64 ./hack/demo-preprod.sh --crc`.
Rebuilding the arm64 workload images: see
[the profile file](../../hack/demo-preprod.crc.env) for the source repos.

## Prerequisites

- CRC installed (`brew install --cask crc` or from [developers.redhat.com](https://developers.redhat.com/products/openshift-local))
- Pull secret at `~/.crc-secret.json` (download from Red Hat console)
- `oc` CLI available (CRC ships one at `~/.crc/bin/oc/oc`)
- `skopeo` for pushing images (`brew install skopeo`)

## Start CRC

```bash
crc setup                          # first-time setup (downloads ~4 GB bundle)
crc start -p ~/.crc-secret.json    # start the cluster (~5–10 min)
```

Check status:

```bash
crc status
```

If the cluster becomes unreachable after being left running for a long time,
restart it:

```bash
crc stop && sleep 30 && crc start -p ~/.crc-secret.json
export KUBECONFIG=${HOME}/.crc/machines/crc/kubeconfig
oc get --raw /healthz
```

Nuclear option (loses cluster state):

```bash
pkill -f vfkit; pkill -f "crc daemon"
crc delete --force && crc setup && crc start -p ~/.crc-secret.json
```

## Log in

`eval "$(crc oc-env)"` may not export `KUBECONFIG` on all shells. Prefer:

```bash
export KUBECONFIG=${HOME}/.crc/machines/crc/kubeconfig
crc console --credentials          # prints kubeadmin password

oc login -u kubeadmin -p <password> https://api.crc.testing:6443 \
  --insecure-skip-tls-verify
```

## Install CRDs and RBAC

```bash
./hack/deploy-dev.sh cost-onprem
# Alias (same script): ./hack/deploy-crc.sh cost-onprem
# or: make crc-dev CRC_NAMESPACE=cost-onprem
```

This script installs the CRD and OwnNamespace RBAC. Run it once per CRC restart.

## Run the operator

`--operator-image` is **required** for wait-for init containers. The tag
`quay.io/project-koku/koku-service-operator:v0.0.1` is **not** published — build
and push to the CRC internal registry instead:

```bash
make crc-operator-image CRC_NAMESPACE=cost-onprem
NAMESPACE=cost-onprem IMG=default-route-openshift-image-registry.apps-crc.testing/cost-onprem/koku-service-operator:dev make run
```

`--dev` skips admission webhook registration (no TLS certs needed on the laptop).

**Cluster Bot / remote OpenShift:** do not use `make run` with BYOI
`*.svc.cluster.local` hosts — use [clusterbot.md](clusterbot.md) /
`./hack/deploy-incluster.sh` instead.

## Apply a sample CR

| Goal | Sample |
|------|--------|
| Koku API + celery only (recommended on CRC) | `config/samples/dev/service.costmanagement_v1alpha1_costmanagementserviceconfig_crc_minimal.yaml` |
| BYOI template — fill in external infra (Kafka, Keycloak, S3, UI secrets) before applying; rejected by admission unedited | `config/samples/service.costmanagement_v1alpha1_costmanagementserviceconfig.yaml` |
| Turnkey bundled stack (Postgres/Valkey + public images) | `config/samples/service.costmanagement_v1alpha1_costmanagementserviceconfig_community.yaml` |
| BYOI smoke | `config/samples/byoi/app/costmanagementserviceconfig-smoke.yaml` |

```bash
eval "$(crc oc-env)"

# Bundled mode (DB + Cache provisioned by operator — dev only). The community
# sample (name: cost-onprem-community) is turnkey: bundled Postgres/Valkey +
# public images + discovery-resolved storage.
oc apply -n cost-onprem \
  -f config/samples/service.costmanagement_v1alpha1_costmanagementserviceconfig_community.yaml

# The default sample is a BYOI template with empty external-infra fields and is
# rejected by admission until you fill them in — do not apply it unedited.

# Watch reconciliation
oc get cmsc -n cost-onprem -w
oc describe cmsc cost-onprem-community -n cost-onprem
```

### Do you need Kafka on CRC?

**No** for koku-only API tests (e.g. async source create). Kafka is required for
the listener and SaaS-style event paths. The minimal sample does **not** disable
the listener: the operator treats `listener.replicas: 0` (and `auth.envoy.replicas: 0`)
as **2** replicas (`internal/resources/koku.go`, `envoy.go`). Without a reachable
Kafka bootstrap, listener pods may **CrashLoopBackOff** and `KafkaReady` stays
False — that is expected for API/celery-only smoke tests.

If you do deploy Kafka on CRC, use smaller PVCs and enable Strimzi node pools:

```bash
STORAGE_CLASS=crc-csi-hostpath-provisioner CRC=1 \
  KAFKA_BROKER_STORAGE=20Gi KAFKA_CONTROLLER_STORAGE=20Gi \
  ./config/samples/byoi/deploy-kafka.sh
```

The deploy script sets `strimzi.io/node-pools: enabled` on the Kafka CR.

### Push images to the CRC registry

Direct `docker push` to the registry Route often fails TLS verification on macOS.
Use the helper script (port-forward + skopeo):

```bash
./hack/push-image-crc.sh <host>/<namespace>/<image>:<tag>
```

### UI OAuth client Secret (Keycloak stays external)

Bundled DB/cache does **not** include Keycloak. The UI needs a same-namespace
Secret with keys `client-id` and `client-secret`. Until it exists, `UIReady`
stays False. The **minimal CRC sample** sets `ui.replicaCount: 0` to skip UI.

## Image note: arm64 vs amd64

CRC on Apple Silicon runs an **arm64** node. Konflux koku images are often
**amd64-only** and crash with `Illegal instruction` or segfault.

Use an arm64 build:

```yaml
costManagement:
  api:
    image:
      repository: quay.io/martin_povolny/koku   # arm64 community build
      tag: "latest"
```

When building locally: `docker build --platform linux/arm64 …`

For **clusterbot / typical OpenShift** (amd64 nodes), build with
`--platform linux/amd64`. See [pre-prod-install.md](pre-prod-install.md).

## Storage class

CRC's default storage class is `crc-csi-hostpath-provisioner`. Leave
`global.storageClass` empty in the CR.

## CRC vs cluster-bot

| | CRC (local) | Cluster-bot |
|--|-------------|-------------|
| Best for | Operator dev, koku API/celery smoke | Full stack, amd64, UI/nginx E2E |
| arm64 Mac | Build/push arm64 images yourself | Uses amd64 nodes |
| Setup time | High first time; fragile under load | Queue + automated BYOI |
| Kafka/UI | Optional / skip with minimal sample | Usually included |

## Common issues

| Symptom | Cause | Fix |
|---------|-------|-----|
| `TLS handshake timeout` | CRC hung under load | `crc stop && crc start`; wait for `oc get --raw /healthz` → `ok` |
| `Missing or incomplete configuration` | `KUBECONFIG` not set | `export KUBECONFIG=$HOME/.crc/machines/crc/kubeconfig` |
| `ImagePullBackOff` on init container | `--operator-image` points to missing Quay tag | `make crc-operator-image` |
| `Illegal instruction` on migrate | amd64 koku image on arm64 CRC | arm64 image; `docker build --platform linux/arm64` |
| Registry push TLS / EOF | Route cert / podman VM networking | `./hack/push-image-crc.sh` |
| Kafka brokers `Pending` forever | 100Gi PVC on CRC disk | `KAFKA_BROKER_STORAGE=20Gi` + `CRC=1` |
| Kafka pods never created | Missing `strimzi.io/node-pools: enabled` | Fixed in `deploy-kafka.sh` |
| BYOI CR `Progressing`, no pods | External DB/Kafka hosts don't exist | Delete stale CRs; use bundled or minimal sample |
| `StorageReady` False | No S3/ODF on CRC | Expected for minimal dev; koku may still run migrations |
| Listener `CrashLoopBackOff` / `Available=False` | No Kafka on CRC minimal sample | Expected; koku API/celery can still run migrations and serve API |
| Migrate job not deleted after image patch | Wrong Job label selector | `oc delete job -n cost-onprem cost-management-koku-migrate --ignore-not-found` |
| Deployment still on old image after CR patch | Operator did not roll image | `oc set image deploy/... *=<new-image>` or delete Deployment |

Legacy fixes (already in operator):

| Symptom | Fix |
|---------|-----|
| `No module named listener` | Uses `python manage.py listener` |
| Django file log on read-only FS | `readOnlyRootFilesystem` not set on koku pods |
| BYOI probes fail under `make run` | `*.svc` not resolvable from laptop — use cluster-bot |
