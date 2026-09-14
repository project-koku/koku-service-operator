# OpenShift CI job details

This page is the per-job walkthrough. For the high-level map, triggering, and GitHub Actions split, see [openshift-ci.md](openshift-ci.md). For log redaction, see [openshift-ci-redactions.md](openshift-ci-redactions.md).

Config (source of truth):

[`openshift/release` … `project-koku-koku-service-operator-main.yaml`](https://github.com/openshift/release/blob/master/ci-operator/config/project-koku/koku-service-operator/project-koku-koku-service-operator-main.yaml)

## Shared cluster-claim settings

`e2e-olm`, `e2e-pytest`, and `e2e-iqe` all use:

```yaml
cluster_claim:
  architecture: amd64
  cloud: aws
  owner: openshift-ci
  product: ocp
  version: "4.20"
steps:
  workflow: generic-claim
```

[`generic-claim`](https://github.com/openshift/release/blob/master/ci-operator/step-registry/generic-claim/generic-claim-workflow.yaml):

| Phase | What runs |
|-------|-----------|
| **pre** | [`ipi-install-rbac`](https://github.com/openshift/release/blob/master/ci-operator/step-registry/ipi/install/rbac/ipi-install-rbac-ref.yaml) (give the CI kubeconfig rights on the claimed cluster), [`openshift-configure-cincinnati`](https://github.com/openshift/release/tree/master/ci-operator/step-registry/openshift/configure/cincinnati) |
| **test** | The steps listed on the job |
| **post** | [`gather`](https://github.com/openshift/release/blob/master/ci-operator/step-registry/gather/gather-chain.yaml): must-gather, extra, audit logs |

The claimed cluster is **leased**, not installed by the job. `cluster_claim.timeout` is **1h** for `e2e-olm` and **2h** for `e2e-pytest` / `e2e-iqe` (the Hive lease). If the lease expires, the cluster is reclaimed and the job is aborted — buffered (sanitized) logs are lost. IQE’s poll timeout (`IQE_TIMEOUT`, default 5400s) is set **under** that 2h so a hung IQE run can still write JUnit before abort, not because install + stack + smoke + IQE + gather all fit in 5400s. Typical IQE smoke is ~17 minutes. Worst-case stack wait (`CMSC_READY_TIMEOUT` default 45m) plus a maxed IQE poll already fills the lease. The script warns if `IQE_TIMEOUT` exceeds 7200s (the full claim, not leftover after other steps). The `install` step sets `grace_period: 15m` so its dump trap can run after a failed wait. See [`scripts/run-iqe-tests.sh`](../../scripts/run-iqe-tests.sh).

Namespaces used:

| Job | Operator / CR namespace | Install mode | Infra namespaces (from BYOI) |
|-----|-------------------------|--------------|------------------------------|
| `e2e-olm` | `koku-service-operator-system` | AllNamespaces (`operator-sdk run bundle`) | none (install only) |
| `e2e-pytest` / `e2e-iqe` | `cost-onprem` | OwnNamespace (OperatorGroup `spec: {}`) | `cost-onprem-infra`, `kafka`, `keycloak` |

OwnNamespace is required for the stack jobs: the operator watches only the namespace it is installed in. See [ownnamespace.md](../development/ownnamespace.md).

The committed BYOI sample uses namespace `cost-byoi` and CR name `cost-management` ([`hack/deploy-byoi.sh`](../../hack/deploy-byoi.sh) defaults). The stack step rewrites those to the Prow names above before apply.

Each e2e job section below starts with **Answers** (the question a green run answers) and **Does not prove**. Snapshot counts for a first-time reader: [What a green run looks like](#what-a-green-run-looks-like).

---

## `build`

Always-run container test (no cluster):

```bash
GOFLAGS=-mod=mod go build -o /dev/null ./cmd/main.go
```

This is a compile check only. Unit tests, lint, and Kind e2e stay on GitHub Actions ([`.github/workflows/ci.yml`](../../.github/workflows/ci.yml)).

---

## `images` and the OLM bundle

`images` builds every image listed in the config (operator, e2e runner, catalog). The catalog image depends on the bundle.

`ci-bundle-koku-service-operator-bundle` is the explicit bundle target. It **skips** when the PR only touches docs / markdown / `OWNERS` / `LICENSE` / `PROJECT` / `.gitignore` (`skip_if_only_changed`).

CSV image substitution in the config:

```yaml
substitutions:
- pullspec: quay.io/project-koku/koku-service-operator:v0.0.1
  with: pipeline:koku-service-operator
```

The claimed cluster therefore runs **this PR’s operator image**, not the Quay `v0.0.1` tag. If you bump the CSV version, you must update that `pullspec` and the catalog Dockerfile’s `koku-service-operator.v0.0.1` channel entry in the same `openshift/release` file.

---

## `e2e-olm` — OLM install smoke

**Answers:** Can this PR’s OLM bundle install on a real OCP cluster? Catalog/bundle via `operator-sdk run bundle`, CSV `Succeeded`, controller Available.

**Does not prove:** OwnNamespace (the production install mode used by the stack jobs), a CMSC, product tests, or a healthy Cost stack.

Timeout: 1h **lease**. Image: `operator-sdk` (OpenShift origin **4.19** tag from the config `base_images`; claimed cluster is OCP 4.20).

| Step | Action |
|------|--------|
| `install` | Create `koku-service-operator-system`. Run `operator-sdk run bundle ${OO_BUNDLE} --install-mode AllNamespaces --security-context-config restricted --timeout 10m`. Wait for CSV `koku-service-operator.v0.0.1` `Succeeded` and Deployment `koku-service-operator-controller-manager` Available. Write `junit_olm-install.xml`. |

This is the automated counterpart of [olm-bundle-testing.md](../development/olm-bundle-testing.md) (`make bundle-run`), using the **pipeline** bundle instead of a Quay tag. AllNamespaces is install-smoke only; it does **not** prove the production OwnNamespace model used by `e2e-pytest` / `e2e-iqe`.

It does **not** source `koso-sanitize` and has **no** dump trap. Failures are the raw build log only. Output is install status, not application logs or customer-shaped reports.

---

## `e2e-pytest` — OLM + stack + pytest

**Answers:** Did this PR’s operator deploy a correct, authenticated, healthy Cost stack in OwnNamespace, and does this repo’s pytest suite pass against it?

That includes OLM CatalogSource + Subscription, BYOI + CMSC to `status.phase=Ready`, then pytest covering auth/JWT, infra (DB/S3/Kafka), Sources API, Koku processing, and this repo’s OCP e2e (NISE → ingress → masu). ROS follows the BYOI sample (`spec.ros.enabled: false`): ROS tests are **collected then skipped**, not deselected.

**Does not prove:** Cost Management Metrics Operator (CMMO) uploads, Playwright UI, Helm chart tests, IQE, or ROS/Kruize. CMMO is a [separate product](../install/cmmo.md); [`hack/ci/e2e.sh`](../../hack/ci/e2e.sh) never installs it.

Timeout: 2h **lease**. Test container: `koku-service-operator-e2e` (has `oc` via `cli: latest`, Python 3.11, pytest, koku-nise).

```mermaid
sequenceDiagram
  participant Prow
  participant Sanitize as koso-sanitize
  participant OLM as install step
  participant Stack as hack/ci/e2e.sh
  participant Smoke as run-pytest.sh -m smoke
  participant Full as run-pytest.sh
  participant Hive as Claimed OCP 4.20

  Prow->>Sanitize: install helper + self-test into SHARED_DIR
  Prow->>OLM: CatalogSource + Subscription in cost-onprem
  OLM->>Hive: wait CatalogSource READY, CSV Succeeded, controller Available
  Prow->>Stack: SKIP_PYTEST=1
  Stack->>Hive: BYOI (Kafka, Postgres/Valkey/MinIO, RHBK)
  Stack->>Hive: apply CMSC, wait status.phase=Ready
  Prow->>Smoke: --no-venv --no-ui --ignore=suites/ui -m smoke
  Prow->>Full: --no-venv --no-ui --ignore=suites/ui
```

### Step `project-koku-koso-sanitize`

Registry: [`ci-operator/step-registry/project-koku/koso-sanitize/`](https://github.com/openshift/release/tree/master/ci-operator/step-registry/project-koku/koso-sanitize).

Copies the sanitizer into `${SHARED_DIR}` and runs a self-test. Later steps `source "${SHARED_DIR}/koso-sanitize.sh"`. Details: [openshift-ci-redactions.md](openshift-ci-redactions.md).

### Step `install`

Applies Namespace, OperatorGroup (`spec: {}` → OwnNamespace), CatalogSource (`image: ${CATALOG_IMG}`, `sourceType: grpc`, restricted security context), and Subscription (`channel: beta`, `installPlanApproval: Automatic`) into **`cost-onprem`**.

Waits:

1. CatalogSource `status.connectionState.lastObservedState=READY` (10m)
2. Subscription `status.installedCSV` non-empty (up to ~10m)
3. That CSV `status.phase=Succeeded` (up to ~10m; fails immediately on `Failed`)
4. Deployment `koku-service-operator-controller-manager` Available (5m)

On exit (success or fail) a `dump` trap writes sanitized `csv-status.txt`, `operator.log`, `catalog.log`, `olm-resources.yaml`, `csv.yaml`, `cmsc-status.txt`, `events.txt` into this step’s `${ARTIFACT_DIR}`. `cmsc-status.txt` is empty unless a CMSC already exists (it usually does not; look at the **stack** step for CR conditions). `grace_period: 15m` gives the trap time to run after a failed wait.

Dynamic `oc` calls use `_run_sanitized` / `_capture_sanitized` so kube API hostnames in errors never hit the Prow log.

### Step `stack`

Runs [`hack/ci/e2e.sh`](../../hack/ci/e2e.sh) with:

```bash
export KUBE_CONTEXT="$(kubectl config current-context)"
export NAMESPACE=cost-onprem
export CR_NAME=cost-onprem
export INFRA_NAMESPACE=cost-onprem-infra
export OPERATOR_NAMESPACE=cost-onprem
export SKIP_PYTEST=1
```

`SKIP_PYTEST=1` is required: pytest lives in later steps so a pytest failure does not skip artifact dump from the stack, and smoke/full can publish separate JUnit files.

What `e2e.sh` does (operator **must already be installed**):

| Phase | Script / fixture | Result |
|-------|------------------|--------|
| Pin kubeconfig | isolated flattened kubeconfig for `$KUBE_CONTEXT` | CI pod cannot drift to another cluster |
| BYOI | [`hack/deploy-byoi.sh`](../../hack/deploy-byoi.sh) → [`scripts/deploy-rhbk.sh`](../../scripts/deploy-rhbk.sh) | Kafka (AMQ Streams), Postgres/Valkey/MinIO in `cost-onprem-infra`, RHBK in `keycloak`, OAuth secret mirror. No sibling `cost-onprem-chart` clone. |
| Secret aliases | copies `byoi-*` secrets to `{CR_NAME}-*` names | Pytest fixtures expect `{cr.name}-db-credentials` etc. |
| CMSC | [`config/samples/byoi/app/costmanagementserviceconfig.yaml`](../../config/samples/byoi/app/costmanagementserviceconfig.yaml) | awk rewrites `cost-byoi` / `cost-management` / `cost-byoi-infra` to Prow NS/CR/infra, plus domain and Kafka bootstrap. `ros.enabled` as in the sample (beta: typically false). |
| Keycloak issuer | [`hack/ci/inject_cmsc_issuer.py`](../../hack/ci/inject_cmsc_issuer.py) + merge-patch | `issuerURL` = `https://<keycloak route>` (tokens); JWKS stays on in-cluster `url`; `tls.insecureSkipVerify: true` because claimed-cluster ingress certs are not in the oauth2-proxy trust store |
| Ready | `oc wait cmsc/cost-onprem --for=jsonpath='{.status.phase}'=Ready` | Default `CMSC_READY_TIMEOUT=45m` |

`KEYCLOAK_ADMIN_VIA` defaults to `port-forward` in this script because the Prow pod cannot reach Hive apps Routes for the Keycloak Admin API.

No-cluster regression tests for issuer injection: [`hack/ci/e2e_test.sh`](../../hack/ci/e2e_test.sh) (run via `make test-hack` on GitHub Actions).

Do **not** call Helm / [`scripts/install-cmsc.sh`](../../scripts/install-cmsc.sh) on this path. The operator reconciles the CMSC.

### Step `smoke`

```bash
./scripts/run-pytest.sh --no-venv --no-ui --ignore=suites/ui -m smoke
```

`--no-venv`: Python deps are already in `koku-service-operator-e2e`. `--no-ui` / `--ignore=suites/ui`: no Playwright in this image. Marker `smoke` is the cheap gate (see [`test/pytest/pytest.ini`](../../test/pytest/pytest.ini)).

The runner writes `test/pytest/reports/junit.xml` (and `report.html` when pytest-html is present). Prow copies those into `${ARTIFACT_DIR}` as:

- `${ARTIFACT_DIR}/junit_smoke.xml`
- `${ARTIFACT_DIR}/pytest_report.html`

### Step `pytest`

Same runner **without** `-m smoke` (still `--no-venv --no-ui --ignore=suites/ui`). Default mark expression still excludes `ui`, `performance`, and `helm`.

Same workspace paths as smoke (`test/pytest/reports/`). Prow copies them as:

- `${ARTIFACT_DIR}/junit_e2e.xml`
- `${ARTIFACT_DIR}/pytest_report.html`

Env for both pytest steps:

```bash
NAMESPACE=cost-onprem
CR_NAME=cost-onprem
HELM_RELEASE_NAME=cost-onprem   # resource prefix (app.kubernetes.io/instance), not a Helm release
KUBE_CONTEXT=<claimed cluster>
```

Reproduce locally / on Cluster Bot: [clusterbot-operator-pytest.md](../development/clusterbot-operator-pytest.md). The Prow difference is OLM catalog install instead of `hack/deploy-incluster.sh`.

---

## `e2e-iqe` — OLM + stack + IQE smoke

**Answers:** After the same OwnNamespace stack as `e2e-pytest`, does Cost Management *work* for the QE smoke slice? The IQE `cost_management` plugin (`ENV_FOR_DYNACONF=cost_onprem`) creates OCP sources and cost models, generates and ingests data via nise, and validates reports/queries through the koku API.

**Does not prove:** CMMO metrics-operator upload (CMMO is not deployed), ROS/Kruize, Playwright UI, or the rest of the IQE image catalog (~11k tests across all plugins).

Same `sanitize` → `install` → `stack` → `smoke` as `e2e-pytest`. The last step is IQE instead of full pytest.

Pytest already covers some ingest/processing; IQE is the QE plugin’s broader API/cost-model smoke, not a second copy of pytest.

### Step `iqe`

```bash
export IQE_TEST_RUN=True
./scripts/run-iqe-tests.sh --profile smoke
```

#### Prow step (before the script)

The IQE image `quay.io/cloudservices/iqe-tests:cost-management` is **private**. The step mounts `insights-qe-secrets` from the `test-credentials` namespace at `/tmp/secrets/ci` and creates docker-registry secret `iqe-pull-secret` in `cost-onprem`. If those creds are missing, the log warns and the pod typically `ImagePullBackOff`.

Prow’s `export IQE_TEST_RUN=True` is on the CI pod. The env koku honors for masu test-only routes (`/enabled_tags/`) is already on the BYOI sample (`spec.costManagement.masu.env.IQE_TEST_RUN`).

#### Pre-pod harness

[`scripts/run-iqe-tests.sh`](../../scripts/run-iqe-tests.sh) does a long harness before `kubectl apply` of `iqe-cost-tests`. Tenant schema and RBAC bootstrap **hard-fail** here, so there is usually **no** `junit_iqe.xml` yet — do not treat a missing or empty JUnit as “IQE found 0 tests”.

| Step | Why | Fail vs warn |
|------|------|--------------|
| Resolve cluster config | S3 creds/endpoint, Kafka bootstrap, Koku API route, Keycloak host, `org_id` / `account_number` (Keycloak admin API when reachable) | Hard-fail if the Koku API route is missing |
| Ensure public Keycloak client `cost-management-iqe` | IQE password-grant (`iqe_jwt` `OIDCAuth.from_basic()`) cannot send `client_secret` | Warn if create fails; IQE auth may fail later |
| Copy operator client creds into Secret `iqe-keycloak-credentials` | Avoids putting the secret in the pod spec YAML | Warn if the operator client secret is missing |
| Build ConfigMap `iqe-ca-bundle` (ingress + service CA) | nise HTTPS uploads verify TLS against `/tmp/router-ca.crt` (it ignores `REQUESTS_CA_BUNDLE`); the pod copies the bundle there | Warn if CAs cannot be extracted; the pod then fail-fasts if the file cannot be staged |
| Seed exchange rates via `kubectl exec` to masu **localhost:8000** | Operator Masu NetworkPolicy denies off-pod callers on :8000; currency-filtered tests need rates | **Warn** if this fails |
| Apply additive NetworkPolicy `{release}-masu-iqe` | Lets the IQE pod reach masu:8000 for `/enabled_tags/` (gated by `IQE_TEST_RUN`) without changing the operator policy | **Hard-fail** (`kubectl apply` under `set -e`) |
| Pre-provision koku tenant schema `org{org_id}` | IQE session fixtures GET first (data-retention); without the schema that view 503s the whole run | **Hard-fail** |
| Grant RBAC **Cost Administrator** + **Sources administrator**; `bootstrap_tenants --org-id` | IQE authenticates as the operator SA (`is_org_admin=false`); without roles, source-create/ingest fail. V2 bootstrap avoids `/access/` 400s | **Hard-fail** |
| Create pod `iqe-cost-tests` | `ENV_FOR_DYNACONF=cost_onprem`, `--force-default-user cost_onprem_user`, marker `cost_ocp_on_prem`, `-k` from the smoke profile | Image pull / Ready wait (5m) |

`--profile smoke` ([`scripts/lib/iqe-filters.sh`](../../scripts/lib/iqe-filters.sh)):

- Positive `-k`: `test_api_ocp_source or test_api_cost_model_ocp`
- Skip infra / slow / delta / flaky groups
- On the order of **~71 selected** parameterized cases / ~17 minutes (order of magnitude; see [What a green run looks like](#what-a-green-run-looks-like))

The script writes `tests/reports/iqe_junit.xml` (note `tests/`, not `test/pytest/`). Prow copies that into `${ARTIFACT_DIR}` as `${ARTIFACT_DIR}/junit_iqe.xml`. It also copies `tests/reports/report.html` → `iqe_report.html` **if that file exists**; the runner normally writes JUnit and `iqe_output.log`, not HTML.

`IQE_TIMEOUT` defaults to 5400s so a hung IQE poll can still publish JUnit before the 2h **cluster claim** aborts the job. Typical smoke is ~17 minutes. Do not raise it toward 7200s — that is the whole-job lease, not IQE’s remaining budget. The script warns if `IQE_TIMEOUT` exceeds 7200s.

Other profiles (`extended`, `stable`, `full`) are for lab / future periodics, not this presubmit.

---

## What a green run looks like

Counts are a **dated snapshot** (around Sep 2026), not a pass/fail gate. They drift when tests are added. Skip *reasons* are more durable than the numbers. QE can correct them.

| Surface | Normal | Notes |
|---------|--------|--------|
| `e2e-olm` | 1 synthetic JUnit pass; CSV `Succeeded`; controller Available | No CMSC |
| Stack | `cmsc/cost-onprem` `phase=Ready`; `Available=True`; ROS off | Conditions, not Phase, are the API |
| Pytest smoke | Marker `smoke` and `not ui and not performance and not helm`; 0 failed | Cheap gate before full pytest / IQE |
| Pytest full | On the order of **~176 passed, ~77 skipped, 0 failed** | `e2e-pytest` pytest step only |
| IQE smoke | On the order of **~71 selected**, 0 failed; `ENV_FOR_DYNACONF=cost_onprem` | Selected from the IQE image catalog (~11k tests, all plugins). Filter is `test_api_ocp_source or test_api_cost_model_ocp` plus skip groups |

**Pytest skip vs deselect** (first-time readers often mix these up):

- `--no-ui` **deselects** `ui`, `performance`, and `helm` ([`scripts/lib/pytest_markexpr.sh`](../../scripts/lib/pytest_markexpr.sh)). Those tests do **not** appear in the skipped count.
- The ~77 skipped are **runtime skips**. Expected buckets on this Prow path:
  - **ROS disabled** — `suites/ros/` autouse + `require_ros_enabled` on e2e/Kruize checks (`ROS is disabled (spec.ros.enabled=false)`).
  - **Cloud source types** — on-prem koku has OpenShift; amazon/azure/google are SaaS-seeded ([`test_sources_api.py`](../../test/pytest/suites/sources/test_sources_api.py) `cloud source types not seeded…`).
  - **No-data / not-yet-ingested** — tagging and similar tests skip when the DB has no tags yet (same pattern as [clusterbot-operator-pytest.md](../development/clusterbot-operator-pytest.md)).
- Other environment skips (missing pods, timeouts) are suspicious, not “normal 77”.

---

## Artifacts cheat sheet

Prow uploads **per step**. Open the failing step’s folder in the job Artifacts view — install’s `cmsc-status.txt` is not the stack dump.

| File | Job / step | Meaning |
|------|------------|---------|
| `junit_olm-install.xml` | `e2e-olm` / install | Single synthetic testcase for CSV + controller |
| `junit_smoke.xml` | pytest + IQE jobs / smoke | Pytest `-m smoke` |
| `junit_e2e.xml` | `e2e-pytest` / pytest | Full non-UI pytest (~176 passed / ~77 skipped is a typical green snapshot; see [What a green run looks like](#what-a-green-run-looks-like)) |
| `junit_iqe.xml` | `e2e-iqe` / iqe | IQE smoke (~71 selected; `ENV_FOR_DYNACONF=cost_onprem`) |
| `pytest_report.html` | smoke / pytest | HTML copied from `test/pytest/reports/report.html` when pytest-html is present |
| `iqe_report.html` | iqe | HTML copied from `tests/reports/report.html` **if present** (usually absent) |
| `csv-status.txt`, `operator.log`, `catalog.log`, `olm-resources.yaml`, `csv.yaml`, `events.txt` | install dump trap | OLM failure diagnosis |
| `cmsc-status.txt` | install dump trap | CMSC conditions **if** the CR already exists (usually empty) |
| `operator.log`, `cmsc-cost-onprem.json`, `cmsc-conditions.txt`, `events-cost-onprem.txt`, `events-operator.txt`, `app-resources.txt` | stack (`e2e.sh` dump) | BYOI + CMSC Ready diagnosis |
| gather / must-gather | `generic-claim` post | Cluster-level diagnostics |

Prow also collects JUnit from `${ARTIFACT_DIR}/junit*.xml` for the job’s test pane. Snapshot pass/skip counts: [What a green run looks like](#what-a-green-run-looks-like).

---

## What these jobs do **not** run

| Suite | Where it lives | Prow today |
|-------|----------------|------------|
| UI Playwright | [`test/pytest/suites/ui/`](../../test/pytest/suites/ui/) | Excluded (`--no-ui`) |
| Performance | [`test/pytest/suites/performance/`](../../test/pytest/suites/performance/) | Excluded (`--no-ui` markexpr) |
| Helm chart tests | [`test/pytest/suites/helm/`](../../test/pytest/suites/helm/) | Excluded (operator path) |
| CMMO | [`docs/install/cmmo.md`](../install/cmmo.md) / [`scripts/setup-cost-mgmt-tls.sh`](../../scripts/setup-cost-mgmt-tls.sh) | Not installed; stack never calls the TLS/CMMO lab script |
| Go CMSC lifecycle | [`test/e2e/cmsc_*.go`](../../test/e2e/) / [cmsc-e2e.md](../development/cmsc-e2e.md) | Not wired ([COST-7699](https://redhat.atlassian.net/browse/COST-7699)) |
| Kind `make test-e2e` | [`test/e2e/e2e_test.go`](../../test/e2e/e2e_test.go) | GitHub Actions only |
| IQE extended/stable/full | [`scripts/lib/iqe-filters.sh`](../../scripts/lib/iqe-filters.sh) | Not scheduled |

---

## Debugging a failed Prow e2e

1. Open the job from the PR check or [Prow](https://prow.ci.openshift.org/?repo=project-koku%2Fkoku-service-operator).
2. Identify the failing **step** in the spyglass / build log (`install`, `stack`, `smoke`, `pytest`, `iqe`). Artifacts are per step.
3. If sanitizer self-test failed, the job never reached install — see [openshift-ci-redactions.md](openshift-ci-redactions.md).
4. If **`e2e-olm`** failed: there is no dump trap. Use the build log. Typical issues: bundle image, install-mode, SCC. AllNamespaces does not prove OwnNamespace.
5. If **install** failed: that step’s `csv-status.txt`, `catalog.log`, `operator.log`. Typical issues: catalog image not serving, CSV install-mode / SCC, substituted operator image not pullable.
6. If **stack** failed before Ready: that step’s `e2e.sh` dump (`cmsc-conditions.txt`, `cmsc-cost-onprem.json`, `operator.log`) — not install’s `cmsc-status.txt`. Typical issues: Kafka/RHBK timeouts, missing `issuerURL` (Envoy 401s later), S3/MinIO preflight.
7. If **pytest** failed: `junit_smoke.xml` / `junit_e2e.xml` and HTML if present. Reproduce with [clusterbot-operator-pytest.md](../development/clusterbot-operator-pytest.md) using the same `NAMESPACE` / `CR_NAME` / `--no-ui` flags.
8. If **iqe** failed: `junit_iqe.xml` first. `ImagePullBackOff` → `insights-qe-secrets` / `iqe-pull-secret`. Little or no JUnit → the [pre-pod harness](#pre-pod-harness) failed (tenant schema and RBAC bootstrap hard-fail; image pull never started tests). Do not treat that as “IQE found 0 tests”.
9. Remember: hosts, URLs, tokens, and RFC1918 addresses in logs are **redacted**. You will see `[internal-host redacted]` / `[URL redacted]` rather than the Hive API URL.

---

## Mapping Prow steps → this repo

| Prow step | This repo |
|-----------|-----------|
| Catalog / CSV wait | (inline in `openshift/release` config; no script) |
| `stack` | [`hack/ci/e2e.sh`](../../hack/ci/e2e.sh) → [`hack/deploy-byoi.sh`](../../hack/deploy-byoi.sh) → [`scripts/deploy-rhbk.sh`](../../scripts/deploy-rhbk.sh) → [`hack/ci/inject_cmsc_issuer.py`](../../hack/ci/inject_cmsc_issuer.py) |
| CMSC sample | [`config/samples/byoi/app/costmanagementserviceconfig.yaml`](../../config/samples/byoi/app/costmanagementserviceconfig.yaml) |
| `smoke` / `pytest` | [`scripts/run-pytest.sh`](../../scripts/run-pytest.sh) → [`test/pytest/`](../../test/pytest/) (`test/pytest/reports/` in the workspace) |
| `iqe` | [`scripts/run-iqe-tests.sh`](../../scripts/run-iqe-tests.sh) → [`scripts/lib/iqe-filters.sh`](../../scripts/lib/iqe-filters.sh) (`tests/reports/iqe_junit.xml` in the workspace). Pull secret: inline in `openshift/release` (`insights-qe-secrets`) |
| Local OLM analogue | [olm-bundle-testing.md](../development/olm-bundle-testing.md) |
| Local pytest analogue | [clusterbot-operator-pytest.md](../development/clusterbot-operator-pytest.md) |
