# Testing on CRC (CodeReady Containers / OpenShift Local)

CRC provides a single-node OpenShift cluster for local development. The
operator runs locally (out-of-cluster) and talks to CRC via kubeconfig.

Two paths:

| Path | What it exercises | Section |
|------|-------------------|---------|
| **`./hack/demo-preprod.sh --crc`** | Full BYOI stack (AMQ Streams + Keycloak + koku + RBAC + ingress + Envoy + UI) with the **in-cluster** operator — the same flow as the `clusterbot` demo, on the local arm64 node | [Full BYOI demo on CRC](#full-byoi-demo-on-crc) |
| **`make run` + a bundled sample CR** | Operator out-of-cluster, operator-provisioned Postgres/Valkey, no Kafka/Keycloak | rest of this document |

## Full BYOI demo on CRC

`./hack/demo-preprod.sh --crc` runs the [pre-prod demo](../development/pre-prod-install.md)
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
pkill -f vfkit; pkill -f "crc daemon"
crc delete --force && crc setup && crc start -p ~/.crc-secret.json
```

## Log in

```bash
eval "$(crc oc-env)"
crc console --credentials          # prints kubeadmin password

oc login -u kubeadmin -p <password> https://api.crc.testing:6443 \
  --insecure-skip-tls-verify
```

## Install CRDs and RBAC

```bash
./hack/deploy-dev.sh cost-onprem
# Alias (same script): ./hack/deploy-crc.sh cost-onprem
```

This script installs the CRD and OwnNamespace RBAC: `manager-role` via a
**RoleBinding** in the target namespace, plus `manager-cluster-role` via a
**ClusterRoleBinding** (StorageClass/Ingress discovery, ConsoleLink, Kruize,
narrow NooBaa `noobaa-admin` Secret get). Run it once per CRC restart.

Alternatively, do it manually:

```bash
oc new-project cost-onprem
make install   # regenerates manifests and applies CRDs via config/crd kustomize
oc apply -f config/rbac/role.yaml
oc apply -f config/rbac/cluster_access_role.yaml
oc create rolebinding koku-operator-dev \
  --clusterrole=manager-role \
  --serviceaccount=cost-onprem:default \
  -n cost-onprem
oc create clusterrolebinding koku-operator-dev-cluster \
  --clusterrole=manager-cluster-role \
  --serviceaccount=cost-onprem:default
oc adm policy add-scc-to-user anyuid -z default -n cost-onprem
```

## Run the operator

OwnNamespace requires a watch namespace. Prefer `NAMESPACE=… IMG=… make run`
(`make run` passes `--dev` and `--operator-image=$(IMG)`):

```bash
NAMESPACE=cost-onprem IMG=quay.io/project-koku/koku-service-operator:v0.0.1 make run
# or:
NAMESPACE=cost-onprem go run ./cmd/main.go --dev \
  --operator-image=quay.io/project-koku/koku-service-operator:v0.0.1 \
  --health-probe-bind-address=:8082 \
  --metrics-bind-address=:8083
```

`--dev` skips admission webhook registration (no TLS certs needed on the
laptop). `--operator-image` is **required** for wait-for init containers.

The operator reads `~/.kube/config` (set by `eval "$(crc oc-env)"`) and
restricts its informer cache to the `cost-onprem` namespace. See
[ownnamespace.md](ownnamespace.md).

**Cluster Bot / remote OpenShift:** do not use `make run` with BYOI
`*.svc.cluster.local` hosts — use [clusterbot.md](clusterbot.md) /
`./hack/deploy-incluster.sh` instead.

## Apply a sample CR

In a second terminal:

```bash
eval "$(crc oc-env)"

# Bundled mode (DB + Cache provisioned by operator — dev only)
oc apply -n cost-onprem \
  -f config/samples/service.costmanagement_v1alpha1_costmanagementserviceconfig.yaml

# Alternative sample with public images:
# oc apply -n cost-onprem \
#   -f config/samples/service.costmanagement_v1alpha1_costmanagementserviceconfig_community.yaml

# Watch reconciliation
oc get cmsc -n cost-onprem -w
oc describe cmsc cost-management -n cost-onprem
```

### UI OAuth client Secret (Keycloak stays external)

Bundled DB/cache does **not** include Keycloak. The UI needs a same-namespace
Secret with keys `client-id` and `client-secret` (default name
`{cr}-ui-oauth-client`). Until it exists, condition `UIReady` stays False and
the UI Deployment is not applied. Cookie Secret is operator-created.

```bash
# After deploy-rhbk.sh (or equivalent) has created
# keycloak-client-secret-cost-management-ui in the keycloak namespace:
NAMESPACE=cost-onprem CR_NAME=cost-management \
  ./config/samples/byoi/mirror-ui-oauth-secret.sh
```

Align RHBK redirect URIs with the UI host **before** expecting login to work:

```bash
export COST_MGMT_NAMESPACE=cost-onprem   # CR namespace
export COST_MGMT_RELEASE_NAME=cost-management
# or: export COST_MGMT_UI_BASE_URL=https://cost-management-ui-cost-onprem.apps.crc.testing
```

For RHBK Route TLS/OIDC, also set `spec.auth.keycloak.issuerURL` to the public
issuer (`iss`) and either `spec.auth.keycloak.tls.caCertSecretName` or
`insecureSkipVerify` as needed for JWKS fetch.

Set `ui.app.image` and `ui.oauthProxy.image` (repository **and** tag on each).
The operator does not default either field. See the sample CRs and
[pre-prod-install.md](pre-prod-install.md).

## Image note: arm64 vs amd64

CRC on Apple Silicon runs an **arm64** node. The production koku image
(`quay.io/redhat-services-prod/cost-mgmt-dev-tenant/koku:768be82`) is
**amd64-only** and segfaults under QEMU emulation.

Use the local arm64 build for testing:

```yaml
costManagement:
  api:
    image:
      repository: quay.io/martin_povolny/koku
      tag: "latest"
```

The bundled sample CR (`config/samples/..._costmanagementserviceconfig.yaml`)
already uses this image.

For **clusterbot / typical OpenShift** (amd64 nodes), do the opposite: build
and push the operator with `--platform linux/amd64` and use amd64 app images
from the BYOI sample. See [pre-prod-install.md](pre-prod-install.md).

## Storage class

CRC's default storage class is `crc-csi-hostpath-provisioner`. The production
sample CR defaults to `ocs-storagecluster-ceph-rbd` (ODF). For CRC, leave
`global.storageClass` empty or set it explicitly:

```yaml
global:
  storageClass: ""   # uses cluster default (crc-csi-hostpath-provisioner)
```

## Common issues

| Symptom | Cause | Fix |
|---------|-------|-----|
| `TLS handshake timeout` | CRC cluster hung under load | Restart CRC |
| `Permission denied` on PVC mount | fsGroup not set | `anyuid` SCC already granted by deploy-dev.sh; check `fsGroup` in pod SC |
| `No module named listener` | Wrong container command | Fixed — uses `python manage.py listener` |
| `Unable to configure handler 'file'` | Django file log handler on read-only FS | Fixed — `kokuAppContainerSC()` does not set `readOnlyRootFilesystem` |
| Migration segfault | amd64 image on arm64 node | Use arm64 image (see above) |
| BYOI probes fail under `make run` | `*.svc` not resolvable from laptop | Use [pre-prod-install.md](pre-prod-install.md) / `deploy-incluster.sh` |
