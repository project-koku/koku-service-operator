# CMSC operator lifecycle e2e (COST-7698)

Go e2e tests in `test/e2e/cmsc_*.go` exercise reconciler behavior on a **live,
reconciled** `CostManagementServiceConfig` stack. They are **not** run in GitHub
Actions Kind CI (`make test-e2e`); CI gate is deferred to
[COST-7699](https://redhat.atlassian.net/browse/COST-7699).

## Quick start

```bash
# 1. Deploy stack (see clusterbot-operator-pytest.md)
IMG=quay.io/.../koku-service-operator:<tag> ./hack/deploy-test-operator.sh --namespace cost-onprem --skip-test

# 2. Confirm day-one gate
oc get cmsc -n cost-onprem -o jsonpath='SchemaUpToDate={.status.conditions[?(@.type=="SchemaUpToDate")].status} Available={.status.conditions[?(@.type=="Available")].status}{"\n"}'

# 3. Run suite (OpenShift labs: set KUBECTL=oc)
export E2E_CLUSTER=1
export NAMESPACE=cost-onprem
export CMSC_NAME=cost-onprem
export KUBECTL=oc
make test-e2e-cmsc
```

Build tag: tests compile only with `-tags cluster_e2e` (the Makefile sets this).

After `deploy-test-operator.sh --skip-test`, run the Go lifecycle suite with the
commands above (pytest is separate; see [clusterbot-operator-pytest.md](clusterbot-operator-pytest.md)).

## Default suite (`make test-e2e-cmsc`)

With no extra env vars beyond `E2E_CLUSTER=1`:

| Runs | Skips |
|------|-------|
| OP-E2E-001/004 (pause halts drift) | OP-E2E-005 (needs `E2E_KOKU_UPGRADE_TAG`) |
| OP-E2E-002 (resume + drift correction) | OP-E2E-006 (needs `E2E_RBAC_UPGRADE_TAG`) |
| OP-E2E-003 (drift correction) | OP-E2E-007b / 008b (need `E2E_BUNDLED_INFRA_PROBE=1`) |
| OP-E2E-005b (downgrade gating, ~10–20 min) | OP-E2E-009 (blocked on COST-7694) |
| OP-E2E-007 / 008 (dependency validation) | |

## Cluster prerequisites (all specs)

| Requirement | Detail |
|-------------|--------|
| OwnNamespace | Operator, CMSC, and `NAMESPACE` env must match (e.g. `cost-onprem`) |
| CMSC CRD + operator | Installed and watching the app namespace |
| Day-one conditions | `BeforeSuite` waits for `SchemaUpToDate=True` and `Available=True` |
| `cost-onprem-koku-api` Deployment | Required for pause/drift specs (name = `{CMSC_NAME}-koku-api`) |
| Active `oc` / `kubectl` session | Tests use the current kubeconfig context |
| Time budget | Default suite ~15–25 min without upgrade; **add ~11 min** for downgrade (`005b`); drift waits up to **6 min** each |

## Environment variables

| Variable | Required | Default | Used by |
|----------|----------|---------|---------|
| `E2E_CLUSTER` | **Yes** | — | Entire suite (`TestCMSCE2E` skips unless `1`) |
| `NAMESPACE` | No | `cost-onprem` | CMSC and workload namespace |
| `CMSC_NAME` | No | `cost-onprem` | CMSC `metadata.name`; falls back to `CR_NAME` |
| `CR_NAME` | No | — | Alias for `CMSC_NAME` when set |
| `KUBECONFIG` | No | default kubeconfig | API client |
| `KUBE_CONTEXT` | No | current context | Pin cluster context |
| `KUBECTL` | No | `kubectl` | CLI for jobs/logs (`oc` works if `KUBECTL=oc`) |
| `E2E_KOKU_UPGRADE_TAG` | For OP-E2E-005 only | — | **Newer** pullable `spec.costManagement.api.image.tag`; spec **skipped** if unset |
| `E2E_KOKU_DOWNGRADE_TAG` | No | `5432d06` | OP-E2E-005b downgrade target; must differ from current tag |
| `E2E_RBAC_UPGRADE_TAG` | For OP-E2E-006 only | — | Pullable `spec.rbac.image.tag`; spec **skipped** if unset |
| `E2E_BUNDLED_INFRA_PROBE` | For 007b/008b only | unset (off) | Set to `1` to run bundled pod-delete readiness specs |
| `E2E_CACHE_WORKLOAD` | No | auto | `deployment` or `statefulset` for OP-E2E-008b when Valkey shape differs |

`make test-e2e-cmsc` fails fast if `E2E_CLUSTER` is not `1`.

## Per-spec requirements

| ID | Spec | Runs by default? | Extra configuration |
|----|------|------------------|---------------------|
| OP-E2E-001/004 | Pause halts drift | Yes | `cost-onprem-koku-api` Deployment |
| OP-E2E-002 | Resume + drift | Yes | Same; **~6 min** wait after resume |
| OP-E2E-003 | Drift correction | Yes | Same; **~6 min** SSA requeue wait |
| OP-E2E-005 | Koku **upgrade** migrate → rollout | **Skipped** | `E2E_KOKU_UPGRADE_TAG` = newer tag than CMSC; image pullable from cluster; migrate Job must **succeed** |
| OP-E2E-005b | Koku **downgrade** gating | Yes | `E2E_KOKU_DOWNGRADE_TAG` (default `5432d06`); accepts migrate **success or fail-closed**; **~10–20 min** |
| OP-E2E-006 | RBAC migrate → rollout | **Skipped** | `E2E_RBAC_UPGRADE_TAG`; `cost-onprem-rbac-api` Deployment |
| OP-E2E-007 | DB dependency (validation) | Yes | Temporarily sets `database.deploy=false` + unreachable host; needs `cost-onprem-db-credentials` Secret (or `spec.database.secretName`) |
| OP-E2E-008 | Cache dependency (validation) | Yes | Temporarily sets `cache.deploy=false` + unreachable host; creates `{CMSC_NAME}-cache-credentials` if needed (CEL requires `auth.secretName`) |
| OP-E2E-007b | Bundled DB pod loss | **Skipped** | `E2E_BUNDLED_INFRA_PROBE=1`, `database.deploy=true`, `{CMSC_NAME}-database` StatefulSet |
| OP-E2E-008b | Bundled cache pod loss | **Skipped** | `E2E_BUNDLED_INFRA_PROBE=1`, `cache.deploy=true`, Valkey workload (Deployment by default) |
| OP-E2E-009 | Secret rotation | **Skipped** | Blocked on [COST-7694](https://redhat.atlassian.net/browse/COST-7694) |

### Notes on dependency tests (007/008)

The primary path simulates **BYOI validation** by patching CMSC to external mode
with an unreachable hostname. It does **not** require a separate infra namespace.

Scaling bundled Postgres/Valkey to **0 replicas does not flip conditions** — the
operator treats zero replicas as ready. Use 007b/008b (pod delete) for bundled
readiness, or the external-probe path above.

While unreachable, OP-E2E-007/008 also assert top-level conditions:
`Available=False` (`DependencyNotReady`) and `Degraded=True`
(`DependencyUnreachable`). Both are restored in each spec's `defer`.

### Notes on migration tests (005/005b/006)

- Migration is keyed on **`spec.costManagement.api.image.tag`** (Koku) or
  **`spec.rbac.image.tag`** (RBAC), not the operator manager image.
- Tag must **change** from the value on the completed migrate Job annotation.
- **Upgrade (005):** tag must exist in the registry and migrate Job must complete
  within the Job deadline (600s).
- **Downgrade (005b):** schema downgrade may fail (e.g. `768be82` → `5432d06`);
  test asserts fail-closed behavior (`MigrationFailed`, Deployment stays on prior image).
- Tests **restore** the original CMSC image tag in `defer` after each migration spec.

## Ginkgo labels

Each `Describe` block sets labels for `-ginkgo.label-filter`:

| Label | Specs |
|-------|-------|
| `cmsc` | All CMSC lifecycle specs |
| `pause` | OP-E2E-001/002/004 |
| `drift` | OP-E2E-003 |
| `upgrade` | OP-E2E-005, 005b, 006 |
| `dependency` | OP-E2E-007, 008, 007b, 008b |
| `secret-rotation` | OP-E2E-009 (stub) |

Every filtered run still requires `E2E_CLUSTER=1` (and usually `KUBECTL=oc` on
OpenShift).

## Filtering specs

```bash
export E2E_CLUSTER=1 NAMESPACE=cost-onprem CMSC_NAME=cost-onprem KUBECTL=oc

# Full default suite
make test-e2e-cmsc

# Quick pass — skip ~10–20 min downgrade (005b)
go test -tags cluster_e2e ./test/e2e/ -run TestCMSCE2E \
  -ginkgo.skip='OP-E2E-005b' -timeout 30m

# By label
go test -tags cluster_e2e ./test/e2e/ -run TestCMSCE2E \
  -ginkgo.label-filter='pause' -timeout 30m
go test -tags cluster_e2e ./test/e2e/ -run TestCMSCE2E \
  -ginkgo.label-filter='drift' -timeout 30m
go test -tags cluster_e2e ./test/e2e/ -run TestCMSCE2E \
  -ginkgo.label-filter='dependency' -timeout 30m
go test -tags cluster_e2e ./test/e2e/ -run TestCMSCE2E \
  -ginkgo.label-filter='upgrade' -timeout 45m

# Single spec by name
go test -tags cluster_e2e ./test/e2e/ -run TestCMSCE2E \
  -ginkgo.focus='OP-E2E-005b' -timeout 45m
go test -tags cluster_e2e ./test/e2e/ -run TestCMSCE2E \
  -ginkgo.focus='OP-E2E-007' -timeout 15m
```

## Prow integration (COST-7699)

Today `e2e-pytest` in openshift/release runs: OLM install → `hack/ci/e2e.sh`
(stack + pytest). The Go CMSC lifecycle suite is **not wired yet**; add a step
after the stack is Ready (`SchemaUpToDate=True`, `Available=True`).

Suggested presubmit step (after existing `stack` step):

```bash
set -euo pipefail
export E2E_CLUSTER=1
export NAMESPACE=cost-onprem
export CMSC_NAME=cost-onprem
export KUBECTL=oc
export KUBE_CONTEXT="$(kubectl config current-context)"
make test-e2e-cmsc
```

| Topic | Guidance |
|-------|----------|
| **Job order** | OLM install → BYOI/stack (`hack/ci/e2e.sh` with `SKIP_PYTEST=1`) → `make test-e2e-cmsc` → pytest (optional separate step) |
| **Timeout** | Makefile allows `2h`; default suite ~15–25 min with 005b. Presubmit may use `-ginkgo.skip='OP-E2E-005b'` (~10 min saved) |
| **Optional env** | `E2E_KOKU_UPGRADE_TAG` / `E2E_RBAC_UPGRADE_TAG` for rehearse or periodic jobs only |
| **Artifacts** | On failure, tests emit forensics to stdout (CMSC YAML, events, operator logs). Capture step log; consider `oc get cmsc,events -n cost-onprem` in a trap |
| **Not in Kind GHA** | `make test-e2e` (Kind manager smoke) stays separate; do not run CMSC suite there |

Full env/per-spec matrix: sections above. Prow job owner: [COST-7699](https://redhat.atlassian.net/browse/COST-7699).

## Related

- [clusterbot-operator-pytest.md](clusterbot-operator-pytest.md) — deploy path
- [COST-7698 Jira](../jira/COST-7698.md)
- [COST-7699](https://redhat.atlassian.net/browse/COST-7699) — Prow CI gate
