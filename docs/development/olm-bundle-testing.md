# Local OLM bundle testing (COST-7695)

Manual verification that the generated OLM bundle installs the operator via
`operator-sdk run bundle`. This validates **operator install**, not a full
Cost Management stack.

For out-of-cluster `go run` / CRC workflows, see
[crc-testing.md](crc-testing.md).

## Prerequisites

- OpenShift cluster access (`oc login` / CRC)
- `docker` (or compatible) logged into Quay
- `operator-sdk` (Makefile downloads it if missing)
- A Quay namespace you can push to (example below uses `quay.io/<user>/…`)

## Image naming

`IMAGE_TAG_BASE` is the **operator** image base. The bundle image appends
`-bundle`:

| Variable | Example |
|----------|---------|
| `IMAGE_TAG_BASE` | `quay.io/<user>/koku-service-operator` |
| `IMG` | `quay.io/<user>/koku-service-operator:v0.0.1-test` |
| `BUNDLE_IMG` | `quay.io/<user>/koku-service-operator-bundle:v0.0.1-test` |

Do **not** set `IMAGE_TAG_BASE` to the `*-bundle` repo name.

## Build and push (personal Quay)

Use a test version tag so you do not overwrite shared tags. Match the cluster
architecture (`linux/arm64` for CRC on Apple Silicon; `linux/amd64` for most
lab/cloud OpenShift nodes).

Disable BuildKit attestations — an OCI index with `unknown/unknown`
attestation manifests commonly breaks `operator-sdk run bundle` with
`no match for platform in manifest`.

```bash
export IMAGE_TAG_BASE=quay.io/<user>/koku-service-operator
export VERSION=0.0.1-test
# CRC on Apple Silicon:
export PLATFORM=linux/arm64
# Typical OpenShift lab/cloud:
# export PLATFORM=linux/amd64

# 1) Operator image (embedded into the CSV by `make bundle`)
docker build --platform "$PLATFORM" --provenance=false --sbom=false \
  -t "${IMAGE_TAG_BASE}:v${VERSION}" .
docker push "${IMAGE_TAG_BASE}:v${VERSION}"

# 2) Regenerate bundle so the CSV points at your operator image
make bundle IMAGE_TAG_BASE="$IMAGE_TAG_BASE" VERSION="$VERSION"

# 3) Bundle image
docker build --platform "$PLATFORM" --provenance=false --sbom=false \
  -f bundle.Dockerfile -t "${IMAGE_TAG_BASE}-bundle:v${VERSION}" .
docker push "${IMAGE_TAG_BASE}-bundle:v${VERSION}"
```

## Build and push catalog (personal Quay)

Use this when the cluster installs via **CatalogSource** + **Subscription** (IQE
OLM fixtures, cluster-bot) instead of `make bundle-run`.

### Mac ARM → amd64 cluster (cluster-bot, MCE, most cloud OpenShift)

`make catalog-build` runs `opm index add`, which calls your container engine
**without** `--platform`. On Apple Silicon the catalog image is **arm64** and
fails on amd64 workers with `exec /bin/opm: Exec format error`.

Use `make catalog-build-amd64` (generates `index.Dockerfile`, then builds with
`--platform linux/amd64`):

```bash
export IMAGE_TAG_BASE=quay.io/<user>/koku-service-operator
export VERSION=0.0.1-test
export BUNDLE_IMG="${IMAGE_TAG_BASE}-bundle:v${VERSION}"
export CATALOG_IMG="${IMAGE_TAG_BASE}-catalog:v${VERSION}"

# Bundle must already be pushed and listable (steps above).
make catalog-build-amd64 BUNDLE_IMGS="$BUNDLE_IMG" CATALOG_IMG="$CATALOG_IMG"

# Sanity check before pushing to a remote cluster:
docker inspect "$CATALOG_IMG" --format '{{.Architecture}}'   # expect: amd64

make catalog-push CATALOG_IMG="$CATALOG_IMG"
```

`DOCKER_DEFAULT_PLATFORM=linux/amd64` does **not** fix `make catalog-build` —
opm does not forward that to its internal build.

If your container engine adds attestation tags that break registry or cluster
pulls, rebuild the catalog image manually with `--provenance=false --sbom=false`
(same flags as the bundle steps above) instead of relying on `catalog-build-amd64`'s
internal `docker build`.

### Same architecture (CRC on Apple Silicon, local arm64)

```bash
make catalog-build BUNDLE_IMGS="$BUNDLE_IMG" CATALOG_IMG="$CATALOG_IMG"
make catalog-push CATALOG_IMG="$CATALOG_IMG"
```

Wire the cluster:

```bash
# CatalogSource spec.image = $CATALOG_IMG
# Subscription: package koku-service-operator, channel beta,
# source <your-catalog-name>, sourceNamespace openshift-marketplace
```

See also [clusterbot-operator-pytest.md](clusterbot-operator-pytest.md#iqe-olm-smoke-only-no-full-stack).

Optional check (single Docker v2 manifest, not an OCI index):

```bash
docker buildx imagetools inspect "${IMAGE_TAG_BASE}-bundle:v${VERSION}"
# Expect: MediaType application/vnd.docker.distribution.manifest.v2+json
```

## Install via OLM

```bash
# kubeconfig must point at the target cluster
make bundle-run BUNDLE_IMG="${IMAGE_TAG_BASE}-bundle:v${VERSION}"
```

What this does: unpacks the bundle image, creates temporary OLM catalog
wiring, Subscribes to package `koku-service-operator` (channel `beta`), and
lets OLM install the CSV (CRD, RBAC, controller Deployment). The CSV is
**AllNamespaces** only — `operator-sdk run bundle` and CatalogSource do not
need OwnNamespace. Suggested operator NS is `cost-onprem`.

### Verify operator install

```bash
oc get csv -A | grep koku-service-operator
oc get pods -A -l control-plane=controller-manager
oc get crd costmanagementserviceconfigs.service.costmanagement.openshift.io
```

Expect CSV `PHASE=Succeeded` and a Running `koku-service-operator-controller-manager` pod.

## Apply a sample CR

Create the target namespace if needed:

```bash
oc new-project cost-onprem || true
```

### Bundled sample (dev — operator deploys DB/cache)

Best first check after OLM install on CRC when external infra is not present:

```bash
oc apply -n cost-onprem \
  -f config/samples/service.costmanagement_v1alpha1_costmanagementserviceconfig.yaml

oc get cmsc -n cost-onprem
oc describe cmsc cost-onprem -n cost-onprem
```

Still requires external Kafka / OIDC / object storage for a full Ready stack.
See [crc-testing.md](crc-testing.md).

### BYOI sample (external infra)

```bash
oc apply -n cost-onprem \
  -f config/samples/service.costmanagement_v1alpha1_costmanagementserviceconfig_byoi.yaml
```

Without PostgreSQL, cache, Kafka, S3 credentials, and Keycloak reachable at the
hosts in that file, expect `phase: Progressing` with conditions such as:

- `StorageReady=False` (missing `my-s3-credentials`)
- `DatabaseReady=False` / `CacheReady=False` / `KafkaReady=False`
- `AuthenticationReady=False`

That still confirms the operator is reconciling. For a fuller BYOI fixture, see
`config/samples/byoi/README.md`.

## Cleanup

Delete the CR **before** `make bundle-cleanup`. Removing the CSV first kills
the operator and leaves the CR finalizer stuck. Full order and recovery:
[uninstall.md](../install/uninstall.md).

```bash
# --all covers both samples in this file (bundled `cost-onprem`, BYOI `cost-management`)
oc delete cmsc -n cost-onprem --all --timeout=180s --ignore-not-found
# Fail closed: if the list itself fails (auth, API down), do NOT fall through
# to bundle-cleanup on an empty-because-errored result.
if ! remaining=$(oc get cmsc -n cost-onprem --no-headers 2>/dev/null); then
  echo "Could not list CMSCs (oc get failed); not running bundle-cleanup." >&2
  exit 1
fi
if [ -n "$remaining" ]; then
  echo "CMSC still present; not running bundle-cleanup. See uninstall.md recovery." >&2
  exit 1
fi
make bundle-cleanup
```

## Checks:

- [ ] `make bundle` succeeds (`operator-sdk bundle validate` passes)
- [ ] Bundle channel metadata is `beta` (default channel `beta`)
- [ ] Operator + bundle images push to your Quay and are pullable by the cluster
- [ ] `make bundle-run` completes; CSV Succeeded; controller pod Running
- [ ] CRD `costmanagementserviceconfigs.service.costmanagement.openshift.io` exists
- [ ] Applying a sample CR is accepted; status conditions update (Ready not required without infra)

