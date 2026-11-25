# Pre-prod demo runbook (prerequisites → operator → UI)

`scripts/demo-preprod.sh` installs a complete Cost Management self-managed
**demo/evaluation environment** on any OpenShift cluster: the prerequisites
(AMQ Streams Kafka, PostgreSQL, Valkey, MinIO-compatible object storage, RHBK
Keycloak), the koku-service-operator, and the Cost Management UI.

The prerequisites are "BYOI" (bring your own infrastructure) in the operator's
design: the operator never provisions them — it validates and connects to
whatever the CR points at. For evaluation this demo deploys its own minimal
instances of the five services — real, working deployments, but single
replicas, no TLS, and removed wholesale by `--reset`. For production you
point the CR at your own managed services instead; see
[docs/install/production.md](../install/production.md).

> **Demo/evaluation profile — not for production.** Insecure settings are in
> use by design (demo realm user, Kafka PLAINTEXT, `sslMode: disable`, OIDC
> `insecureSkipVerify`). For production installs see
> [docs/install/production.md](../install/production.md).

## Quick start

```bash
./scripts/demo-preprod.sh                  # everything: prereqs + latest stable operator (quay.io/project-koku) + UI
./scripts/demo-preprod.sh --dry-run        # print the resolved plan and exit
./scripts/demo-preprod.sh --reset          # tear down first (CMSC → OLM objects → namespaces)
```

The default install source is `registry`: the latest **stable** operator
version, discovered from the published quay tags, installed via OLM
(CatalogSource → OperatorGroup → Subscription, Automatic approval). Nothing is
built locally.

## Choose the operator install source

| `--source=` | What happens | Use when |
|---|---|---|
| `registry` (default) | The published operator from `quay.io/project-koku` via OLM; version discovered or pinned | You want the product as a customer gets it |
| `local` | Build the operator from this checkout (docker/podman or in-cluster OpenShift build) and deploy with raw manifests | You are developing operator code |
| `manual` | Prerequisites and the rendered CR are prepared; **you** install the operator through the OpenShift console (instructions printed) | Console-guided demos, or no cluster-admin for automation |

### Version selection (registry / manual)

- Default: the highest `vX.Y.Z` tag on the catalog repository. Today that
  resolves to `v0.0.1` — the version the Red Hat Beta article pins.
- `--version` accepts `vX.Y.Z`, a commit SHA (short or long — every main build
  is tagged), `latest`/`main`, or a tag with an `@sha256:…` digest suffix for
  absolute pinning.
- `--catalog-repo` selects another quay namespace (e.g.
  `quay.io/martin_povolny`); `--channel` sets the OLM subscription channel.
- A repository with **no semver tag** falls back to `:latest` **with a loud
  warning** — expected for personal dev repos, never silent.
- An explicit version that does not exist is an error listing available tags.

> **Version-switching caveat:** every published bundle currently shares CSV
> version `0.0.1` (upstream
> [project-koku/koku-service-operator#220](https://github.com/project-koku/koku-service-operator/issues/220)),
> so OLM will not treat a different catalog tag as an upgrade. Re-running with
> a different `--version` on an installed cluster warns and points at
> `--reset`.

## Architecture handling

The cluster architecture is **autodiscovered** from the nodes (mixed-architecture
clusters warn and use the first node; anything other than amd64/arm64 fails).
Before anything is installed, the image-architecture preflight checks the
catalog image and every workload image of the CR; after the OLM install the
operator image the CSV deploys is checked too, and after the prerequisites are
deployed the running pod images are verified.

- **Mismatch → hard fail**, listing each offending image with the upstream
  [arm64 multi-arch issue](https://github.com/project-koku/koku-service-operator/issues/219)
  context (the published v0.0.1 operator and workload images are amd64-only)
  and the `DEMO_*_IMAGE` override pointers.
- Images that cannot be verified (private registries such as
  `registry.redhat.io` need credentials) produce a **warning**, not a failure.
- `ARCH_CHECK=0` skips the checks (not recommended).

For **arm64 clusters** until #219 lands: copy the opt-in
`scripts/demo-preprod.arm64.env` template (arm64 workload image rebuilds) into
`scripts/demo-preprod.local.env`. No flag auto-loads it.

## Install modes

```bash
./scripts/demo-preprod.sh --prereqs-only   # prerequisites only, then stop (prints next steps)
./scripts/demo-preprod.sh --skip-prereqs --cr-file <your-cmsc.yaml>
                                           # operator only, on your own infrastructure
./scripts/demo-preprod.sh --watch          # observe an existing install until the UI is ready
./scripts/demo-preprod.sh --console        # print the OpenShift console URL and username
```

`--cr-file` applies **your** CostManagementServiceConfig instead of the
rendered one (fail-fast if the file is missing; the architecture preflight
reads its workload images; with `--source=manual` the printed instructions
point at your file). See the `_production` sample and
[docs/install/production.md](../install/production.md) for the fields a
production CR needs.

## Small clusters: the single-node profile

`--single-node` (alias `--crc`) shrinks Kafka to 1 broker + 1
controller with 10Gi/5Gi volumes — **sizing only**. It never sets
`KUBE_CONTEXT`, never forces a build mode, never selects an architecture, and
never loads image overrides. All CRC specifics — host prerequisites, starting
and logging into the cluster, the minimal koku-only developer loop, arm64
image rebuilds, and troubleshooting — are in
[crc-testing.md](crc-testing.md).

### Local CRC (OpenShift Local): one-time setup

Start CRC and register a context named `crc` (full guide:
[crc-testing.md](crc-testing.md)):

```bash
crc start -p ~/.crc-secret.json
oc login -u kubeadmin -p "$(crc console --credentials | sed -n 's/.*-p \([^ ]*\).*/\1/p' | tail -1)" \
    https://api.crc.testing:6443 --insecure-skip-tls-verify=true
oc config set-context crc --cluster=api-crc-testing:6443 \
    --user=kubeadmin/api-crc-testing:6443 --namespace=default
```

Then run the demo against it:

```bash
./scripts/demo-preprod.sh --context crc --single-node
```

`oc login` refreshes the token; re-run it (not `set-context`) if the context
later reports `Unauthorized`.

## What you get

- Four namespaces (`cost-byoi`, `cost-byoi-infra`, `kafka`, `keycloak`; all
  parameterizable), the operator, and the full application stack.
- A **CMSC verification table** at the end of every run: every condition
  (`Available`, `UIReady`, `DatabaseReady`, …) with pass/fail and reasons —
  the same conditions the Red Hat Beta article lists. A failed install reports
  exactly which conditions are false and why.
- The UI Route and login: the realm user `admin` with a **random password**
  (generated at provisioning, stored in the Secret
  `{CR_NAME}-realm-admin` in the app namespace, printed once at the end;
  re-runs read it back from the Secret).
- Idempotent re-runs: every stage is skipped when already healthy.
- `--reset` teardown: the CMSC first (so the operator clears its finalizer),
  then the OLM objects (Subscription, CSV, OperatorGroup, CatalogSource), then
  the namespaces — with finalizer-stripping rescue for stuck deletions.
  `--reset` assumes a **single-tenant demo cluster**: the CatalogSource is
  removed from `openshift-marketplace` and labelled cluster-scoped RBAC is
  deleted regardless of which source installed the operator (a warning is
  printed when the source was not `registry`).

## Manual source: console install instructions

After `--source=manual` prepares everything, the script prints the exact
steps; in short: create a CatalogSource named `koku-service-operator-catalog`
in `openshift-marketplace` with the resolved catalog image (grpc), install
**Cost Management Service Operator** from the Software Catalog (channel
`beta`, All namespaces, the demo namespace, Automatic), wait for the CSV to be
`Succeeded`, then `oc apply -f` the rendered CR file, and observe with
`--watch`.

## Configuration

`./scripts/demo-preprod.sh --help-env` prints every settings variable with
its default or description (generated from the env example, so it cannot
drift). All flags have environment-variable equivalents; the precedence is real
environment > `scripts/demo-preprod.local.env` >
`scripts/demo-preprod.env.example` (first file wins per variable). See the
comments in `scripts/demo-preprod.env.example` for the full list (namespaces,
version selection, `CONTAINER_TOOL` docker/podman, `ARCH_CHECK`,
`REALM_ADMIN_PASSWORD`, `DEMO_SMOKE`, profile, and the `DEMO_*_IMAGE`
overrides).

## Troubleshooting

- **UI pod crash-loops after a full cluster restart** — nginx resolves its
  gateway upstream at boot; it self-recovers, or delete the UI pod once the
  gateway Service exists (upstream
  [#221](https://github.com/project-koku/koku-service-operator/issues/221)).
- **No semver tag / `:latest` fallback warning** — expected on personal dev
  repos; pin `--version` for reproducibility.
- **Permission errors in preflight** — the registry source needs cluster-admin
  (CatalogSource in `openshift-marketplace`); restricted users get clear
  early errors instead of a mid-run failure. Use `--source=manual` with your
  administrator.
- **`deploy-rhbk.sh` aborts with "curl TLS verification failed"** — the
  cluster's router CA does not match the served certificate (replaced ingress
  certificate, or a TLS-inspecting proxy in front of the `*.apps` domain).
  Set `RHBK_INSECURE_TLS=1` for such labs.
- **Build failures with podman** — `BUILD_MODE=docker` auto-detects docker
  (with buildx) or podman; the podman-docker shim is detected and avoided.
  Note that `BUILD_MODE=auto` prefers the in-cluster OpenShift build whenever
  `IMG` is the internal registry — set `BUILD_MODE=docker` explicitly (with
  `CONTAINER_TOOL` if you want to force the tool) to build locally.

## Related docs

- [docs/install/production.md](../install/production.md) — production installs
- [keycloak.md](keycloak.md) — Keycloak/RHBK details
- [crc-testing.md](crc-testing.md) — everything CRC: host setup, the demo
  flow, the minimal koku-only developer loop, arm64 rebuilds, troubleshooting
- `./scripts/demo-preprod.sh --help` — the full flag reference
