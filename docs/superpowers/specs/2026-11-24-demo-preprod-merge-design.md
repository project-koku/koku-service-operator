# Merged pre-prod installation script — design

Date: 2026-11-24
Branch: `pgarciaq-installation-script-enhancements`
Upstream issues: project-koku/koku-service-operator#219 (arm64), #220 (release versioning)

## Intent

One customer-facing script that installs a demo/evaluation Cost Management
self-managed environment on any OpenShift cluster: prerequisites, operator,
and UI — with the operator's install source selectable (published registry
image, local source build, or console-guided), architecture-aware,
version-selectable, idempotent, and resettable. `scripts/demo-preprod.sh`
absorbs `scripts/demo-preprod-olm.sh`, which is removed.

This is a **demo/evaluation installer for end users (customers)**, not a
production installer. It must be bulletproof: fail early with clear messages,
never deploy something that cannot run, and never surprise the user with a
different version than requested.

## Why

- Two overlapping scripts (`demo-preprod.sh`: local build + in-cluster deploy;
  `demo-preprod-olm.sh`: manual OLM console instructions) confuse users and
  drift apart.
- The default experience should match the published product —
  `quay.io/project-koku`, latest stable, OLM install — which is what the Red
  Hat Beta article (access.redhat.com/articles/7148128) describes.
- Customers evaluate on varied clusters (amd64/arm64, single-node lab vs.
  multi-node); the script must detect and adapt instead of requiring modes.

## Goals

1. Single script replaces both; `demo-preprod-olm.sh` is deleted (tests and
   runbook ported/rewritten).
2. Install source selectable: `--source=registry|local|console` (default
   `registry`).
3. Architecture autodiscovery from the target cluster + image-arch preflight
   validation of everything the install will reference.
4. Version selection: latest stable by default (discovered from registry
   tags), or a specific build via tag/SHA/digest; catalog repository
   overridable.
5. `--prereqs-only` (prerequisites alone) and `--skip-prereqs --cr-file`
   (operator on user-provided infrastructure via the user's own CR).
6. Idempotent re-runs (skip-if-ready), `--reset` teardown extended to OLM
   objects.
7. Demo-profile hardening: no test-only routes, no external API calls by
   default, demo banners, random Keycloak admin password.
8. Preserve existing capabilities: `--dry-run`, `--no-tmux`, `--no-open`,
   `--rebuild`, `--watch`, tmux observers, env-file layering
   (`demo-preprod.local.env`), `KUBE_CONTEXT`, `WATCHER`, `LOG_LEVEL`,
   `KUBECTL`, `RHBK_SCRIPT` override.

## Non-goals

- Production installs — the `_production` sample and `docs/install/production.md`
  remain the production path.
- Parameterizing user-provided prerequisites beyond the `--cr-file` escape
  hatch (per-component host/secret/TLS overrides are out of scope).
- Solving arm64 workload images in this repo (upstream #219); the script
  detects the gap and fails with a clear explanation instead.
- OLM v1 `ClusterExtension` installs (sample exists in `config/samples/olmv1/`
  but classic OLM is what the article and consoles use).
- In-place version upgrades between equal CSV versions (upstream #220); the
  script detects the situation and advises `--reset`.

## Anti-goals

- Never silently install a different version than requested; the only
  fallback (`:latest` when no semver tag exists) prints a loud warning.
- Never deploy a stack whose images cannot run on the target architecture
  without an explicit user override.
- Never enable test-only routes (`IQE_TEST_RUN`) or external API calls
  (`CURRENCY_URL`) in the default CR render.

## Confirmed decisions

| # | Decision | Choice |
|---|---|---|
| 1 | Default install source | `registry` (latest stable from `quay.io/project-koku`) |
| 2 | Version discovery | Registry tag listing (quay API), filter `^v?\d+\.\d+\.\d+$`, highest wins |
| 3 | Version fallback | `:latest` with loud warning when no semver tag exists |
| 4 | Build identifier | Catalog image tag: `vX.Y.Z` \| commit SHA (short/long) \| `latest`/`main`; optional `@sha256:` digest suffix |
| 5 | Alternate registry | `--catalog-repo=<quay namespace>` (image name `koku-service-operator-catalog` fixed) |
| 6 | Local source build | Keep current build modes (`docker`/`openshift`/`auto`), arch-aware platform selection |
| 7 | Console mode | Keep (renamed from "manual" to `console`); mirrors article path |
| 8 | Arch mismatch behavior | Hard fail listing offending images; escape hatch via env override |
| 9 | User-provided prereqs | `--skip-prereqs` + `--cr-file <path>` only |
| 10 | Teardown | `--reset` extended to OLM objects |
| 11 | `--crc` | Becomes a single-node/small-cluster **sizing profile**: Kafka 1+1/15Gi. Does **not** force `BUILD_MODE`, architecture, or image overrides. `--profile=single-node` is the primary name; `--crc` stays as an alias. |
| 12 | Demo hardening | Strip `IQE_TEST_RUN` and `CURRENCY_URL` from default render; demo banner at start/end; final summary lists insecure settings; random Keycloak admin password (printed once at the end) |

## Design

### CLI

```
./scripts/demo-preprod.sh [flags]

  --source=registry|local|console   install source (default: registry)
  --version=<tag>                   catalog tag: vX.Y.Z | <commit-sha> | latest (default: discovered latest stable)
  --catalog-repo=quay.io/project-koku   quay namespace for the catalog image
  --channel=beta                    OLM subscription channel
  --prereqs-only                    install BYOI prerequisites, no operator
  --skip-prereqs                    operator only, on existing infrastructure
  --cr-file=<path>                  apply this CostManagementServiceConfig instead of the rendered one
  --profile=single-node | --crc     small-cluster sizing profile (alias)
  --reset                           teardown: CMSC, OLM objects, namespaces
  --rebuild                         force operator image rebuild (local source)
  --watch                           resume/observe mode (console source)
  --dry-run                         print the plan and exit
  --no-tmux, --no-open              disable tmux layout / browser launch
```

Every flag has an environment-variable equivalent (existing convention:
`SOURCE`, `VERSION`, `CATALOG_REPO`, `CHANNEL`, `PREREQS_ONLY`, …), loaded
from the environment, then `scripts/demo-preprod.local.env`, then
`scripts/demo-preprod.env.example` (first wins — same `load_env_file`
semantics as today). The `--skip-prereqs`/`--cr-file` combination is
documented in the usage header and in comments in the env example file.

### Flow

1. **Preflight** — kube context, cluster reachability, apps domain, cluster
   architecture discovery, version resolution, image-arch validation, demo
   banner. `--dry-run` prints the resolved plan (source, version, catalog
   image, arch, profile) and exits.
2. **Reset (optional)** — delete CMSC first while the operator can clear
   finalizers (existing logic), then OLM objects (Subscription, CSV,
   OperatorGroup in the app namespace; CatalogSource
   `koku-service-operator-catalog` in `openshift-marketplace`), then the
   four namespaces with the existing stuck-Terminating rescue.
3. **Prerequisites (BYOI)** — unchanged delegation to `deploy-byoi.sh` with
   the existing skip-if-ready pre-checks (Kafka/infra/Keycloak/OAuth secret).
   Skipped entirely with `--skip-prereqs`. Exits after this step with
   `--prereqs-only`.
4. **Operator install** — by source:
   - **registry**: `oc apply` CatalogSource (image
     `${CATALOG_REPO}/koku-service-operator-catalog:${VERSION}`, grpc,
     `openshift-marketplace`), OperatorGroup (AllNamespaces) + Subscription
     (channel `beta`, Automatic) in the app namespace; wait for CSV
     `Succeeded`; verify the installed CSV's bundle matches the requested
     catalog image (detect equal-version-different-bundle and advise
     `--reset`).
   - **local**: existing build path (`BUILD_MODE=auto|docker|openshift`),
     with `docker` mode using the discovered node arch for
     `buildx --platform`; then `deploy-incluster.sh`.
   - **console**: render the CR to a persistent file, print article-style
     console instructions naming the resolved catalog image and tag, exit 0.
     `--watch` resumes and waits for the CMSC/CSV to appear.
5. **Apply CR** — rendered CR (see hardening below) or the user's `--cr-file`.
6. **Wait for UI** — the deadline-bounded wait (15 min, CMSC conditions +
   deployment rollout) from `demo-preprod.sh`, replacing the OLM script's
   unbounded loop.
7. **Verify** — check the CMSC condition set (`Available`, `UIReady`,
   `DatabaseReady`, `CacheReady`, `KafkaReady`, `StorageReady`,
   `AuthenticationReady`, `SchemaUpToDate`, …) and print a pass/fail table
   (mirrors the article's verification tables).
8. **Finish** — demo banner, insecure-settings summary, login credentials
   (random admin password), open the UI Route.

### Version discovery and selection

- Query the registry tag API (public quay, unauthenticated; pagination
  handled) for `${CATALOG_REPO}/koku-service-operator-catalog`; select the
  highest tag matching `^v?\d+\.\d+\.\d+$`.
- `--version` accepts `vX.Y.Z`, a commit SHA (short or long — every main
  build is tagged), `latest`/`main`, or a tag with an `@sha256:…` digest
  suffix for absolute pinning.
- No semver tag found → fall back to `:latest` **with a loud warning**
  (expected for personal dev repos like `quay.io/martin_povolny`).
- Each catalog image contains exactly one bundle, so the catalog tag pins
  the operator build; CSV contents are never parsed for version selection.

### Architecture discovery and validation

- **Cluster arch**: `oc get nodes -o jsonpath='{.items[*].status.nodeInfo.architecture}'`
  over worker nodes; mixed arch → warning, first worker wins; anything other
  than amd64/arm64 (s390x/ppc64le) → unsupported, hard fail.
- **Image arch**: `skopeo inspect --raw docker://<image>` when skopeo is
  installed, else a python3 Docker-registry-v2 client (anonymous token flow;
  python3 is already a hard dependency). Returns the set of architectures
  from the manifest (list). Private or unreachable registries degrade to
  "arch could not be verified" warnings, not failures.
- **Checked images**: the catalog image (registry source) and every workload
  image referenced by the rendered CR. Local source: the operator image is
  built for the node arch (openshift mode is native; docker mode uses
  `--platform`), so only workloads are checked.
- **Mismatch**: hard fail, listing each offending image and the cluster arch,
  with the upstream #219 context; escape hatch: `ARCH_CHECK=0`.

### Demo-profile hardening

- `render_preprod_cr` no longer emits `IQE_TEST_RUN` (masu) or `CURRENCY_URL`
  (koku api) in the default path — those lines are dropped from the rendered
  CR; the CI smoke variant (`costmanagementserviceconfig-smoke.yaml`) keeps
  them for IQE.
- Start and end banners: "Demo/evaluation profile — not for production."
- Final summary lists every insecure setting in use: Keycloak credentials,
  Kafka PLAINTEXT, `sslMode: disable`, `insecureSkipVerify`, and points to
  `docs/install/production.md`.
- `deploy-rhbk.sh` generates a random admin password (stored in a Secret,
  printed once at the end of the install) instead of `admin/admin`.

### Single-node profile

`--profile=single-node` (alias `--crc`) applies only sizing: Kafka
1 broker + 1 controller, 10Gi + 5Gi storage (replication factor and
min.insync follow the broker count, as today). It does **not** set
`KUBE_CONTEXT`, **not** force `BUILD_MODE`, **not** select architecture,
and **not** override workload images — arch is autodiscovered, and image
overrides remain available through `DEMO_*_IMAGE` env vars for development
workflows.

### Reset (teardown)

Existing sequence (CMSC first for finalizer clearing, ConsoleLink and
cluster-scoped RBAC, per-namespace CR deletion with finalizer stripping,
namespace deletion with stuck-Terminating rescue) plus OLM objects:
Subscription `koku-service-operator`, `clusterserviceversion` (by label),
OperatorGroup in the app namespace, and the CatalogSource in
`openshift-marketplace`.

### Documentation and tests

- `demo-preprod-olm.sh` deleted; `demo-preprod-olm_test.sh` checks ported to
  the merged script's help/dry-run output.
- `demo-preprod_test.sh` extended: version resolution (mocked registry API),
  image-arch helper, default CR render asserts absence of `IQE_TEST_RUN` and
  `CURRENCY_URL`, profile flag, `--cr-file` passthrough.
- `docs/development/pre-prod-install.md` and
  `pre-prod-olm-demo-runbook.md` merged into one runbook for the merged
  script; `crc-testing.md` and `scripts/README.md` updated.

## Left to the builder

- Exact env-var naming table (flag ↔ env ↔ env-file key).
- OperatorGroup/Subscription manifest details (API group versions, labels).
- Wait/timeout constants for the OLM install path.
- Test structure for the registry-API mock (offline, no network).
