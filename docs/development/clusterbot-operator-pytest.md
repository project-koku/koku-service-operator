# Cluster Bot: operator deploy + pytest

Runbook for reproducing the **operator** path on a Cluster Bot / MCE lab cluster
and running the ported `cost-onprem-chart` pytest suite against it.

This path continues [COST-7697](https://redhat.atlassian.net/browse/COST-7697)
(operator pytest parity, **Closed** — original port in PR #56). Active triage and
cluster-bot reproduction are tracked in
[COST-8121](https://redhat.atlassian.net/browse/COST-8121). Deploys via
`CostManagementServiceConfig` — **not** the Helm chart.

| Related docs | When to use |
|--------------|-------------|
| [clusterbot.md](clusterbot.md) | Quick Redpanda smoke (`hack/clusterbot-smoke.sh`) — no AMQ Streams pytest infra |
| [allnamespaces.md](allnamespaces.md) | AllNamespaces watch vs suggested install NS |
| [pre-prod-install.md](pre-prod-install.md) | Full BYOI + UI OAuth mirror |
| [crc-testing.md](crc-testing.md) | Laptop `make run` against CRC |

## Goal

| Step | Outcome |
|------|---------|
| Infra | RHBK + AMQ Streams Kafka + S4 (S3 stand-in) on the cluster |
| Operator | Manager running **in-cluster** (not laptop `make run`) |
| CMSC | Sample CR reconciled to day-one success |
| Tests | `./scripts/run-pytest.sh` (~25–60 min without UI; ~1h with UI) |

Step 1 deploys external infra only. The default CMSC sample still **bundles
PostgreSQL and Valkey** in `cost-onprem` (lab/CI — not production BYOI).

**Day-one success (enough for most pytest):**

- `SchemaUpToDate=True`, `Available=True` (`KokuAvailable`)
- Core Deployments healthy

**Expected to stay False without extra UI work:**

- `UIReady=False` (`OAuthClientSecretMissing`)
- `Phase=Progressing` (until `UIReady=True`)

## Critical rules (read first)

### 1. AllNamespaces — suggested install NS `cost-onprem`

The operator watches **every** namespace. Recommended lab layout is still one
NS for operator + CR + pytest:

```text
operator NS  ==  CR NS  ==  NAMESPACE for pytest  ==  cost-onprem
```

BYOI (Kafka, RHBK, S4) may live in other namespaces. `make deploy` scaffolds
into `cost-onprem`.

See [allnamespaces.md](allnamespaces.md).

### 2. Use `hack/deploy-incluster.sh`, not `make deploy` from a laptop

| Path | Cluster Bot pytest? |
|------|---------------------|
| `IMG=quay.io/... ./hack/deploy-incluster.sh cost-onprem` | **Yes** — Quay image, lab webhook TLS, AllNamespaces |
| `make deploy` / `install-cmsc.sh` operator step | **No** — integrated registry push from laptop, cert-manager |
| `make run` / `go run ./cmd/main.go` | **No** — `*.svc.cluster.local` not reachable from laptop |

`deploy-incluster.sh` calls `hack/deploy-dev.sh` (CRDs + RBAC), creates a
self-signed webhook serving-cert Secret, and rolls out the manager. It does
**not** install Validating/MutatingWebhookConfiguration — CR apply is not
admission-gated on this lab path (OLM/cert-manager covers production).

### 3. Pytest `NAMESPACE` must match the CR namespace

```bash
export NAMESPACE=cost-onprem
# Prefix for managed resource names (app.kubernetes.io/instance = CR name). Not a Helm release.
export HELM_RELEASE_NAME=cost-onprem
export KEYCLOAK_NAMESPACE=keycloak
```

## Cluster choice

| Item | Recommendation |
|------|----------------|
| **OCP version** | **4.18+** minimum; **4.20+** matches JIRA verification targets |
| **Bot type** | **MCE** for pytest (~1h) — longer TTL than CI Hypershift (~2h) |
| **Workers** | **≥2 workers** strongly recommended |
| **Storage** | Any default `StorageClass` (e.g. `gp3-csi` on AWS) |
| **S3** | Clusters without ODF/NooBaa need **`--deploy-s4`** |

Example cluster (Aug 2026 lab run):

- MCE AWS, OCP 4.20
- 3 masters + 3 workers, all `Ready`
- Domain: `apps.<cluster-id>.crt-mce-aws.devcluster.openshift.com`

## Prerequisites

### On the cluster

```bash
oc login --token=... --server=https://api....:6443
oc whoami                    # kube:admin
oc get nodes                 # all Ready
DOMAIN=$(oc get ingresses.config cluster -o jsonpath='{.spec.domain}')
echo "$DOMAIN"
```

### On your laptop

| Tool | Notes |
|------|--------|
| `oc`, `kubectl` | OpenShift 4.x CLI |
| `helm` v3+ | AMQ Streams / RHBK scripts |
| `yq`, `jq` | Deploy scripts |
| `openssl` | Used by `deploy-incluster.sh` for lab webhook certs |
| **bash 4+** | macOS `/bin/bash` is 3.2 — use Homebrew bash for `deploy-test-cost-onprem.sh` |
| Container engine | Only when **building** a new operator image |

### Mac vs Fedora / Linux

| Topic | macOS | Fedora / Linux |
|-------|-------|----------------|
| **bash** | `/opt/homebrew/bin/bash ./scripts/...` | `./scripts/...` |
| **Image arch** | `docker buildx build --platform linux/amd64 ...` | Usually native amd64 |
| **Registry push** | Integrated registry Route often fails (TLS) — **use Quay** | Same — prefer Quay |
| **pytest SSL** | Homebrew sets `REQUESTS_CA_BUNDLE` — see [Pytest on macOS](#pytest-on-macos) |

## Operator image (one-time or per branch)

Build and push **linux/amd64** (required on Apple Silicon):

```bash
export IMG=quay.io/<your-quay-user>/koku-service-operator:clusterbot-test

docker buildx build --platform linux/amd64 -t "$IMG" --push .
```

Keep `IMG` for all clusters until operator code changes.

## Step 1 — Infra (RHBK + Kafka + S4)

Creates `cost-onprem`, `keycloak`, `kafka`, `s4-test`. Skips operator, CMSC, and pytest.

**Mac:**

```bash
/opt/homebrew/bin/bash ./scripts/deploy-test-cost-onprem.sh \
  --namespace cost-onprem \
  --deploy-s4 \
  --skip-helm \
  --skip-chart-tests \
  --skip-tls \
  --verbose
```

**Fedora / Linux:**

```bash
./scripts/deploy-test-cost-onprem.sh \
  --namespace cost-onprem \
  --deploy-s4 \
  --skip-helm \
  --skip-chart-tests \
  --skip-tls \
  --verbose
```

**Verify (~30–45 min):**

```bash
oc get pods -n keycloak
oc get pods -n kafka
oc get pods -n s4-test
oc get kafka -n kafka
```

### Known S4 credential gap (script bug — fix in PR #127)

`deploy-s4-test.sh` creates Secret **`s4-credentials`**, but
`deploy-test-cost-onprem.sh` expects **`cost-onprem-storage-credentials`** in
`cost-onprem`. If the copy step logs a warning, sync manually before CMSC:

```bash
ACCESS_KEY=$(kubectl get secret s4-credentials -n s4-test -o jsonpath='{.data.access-key}' | base64 -d)
SECRET_KEY=$(kubectl get secret s4-credentials -n s4-test -o jsonpath='{.data.secret-key}' | base64 -d)
kubectl create secret generic cost-onprem-storage-credentials \
  --namespace=cost-onprem \
  --from-literal=access-key="${ACCESS_KEY}" \
  --from-literal=secret-key="${SECRET_KEY}" \
  --dry-run=client -o yaml | kubectl apply -f -
```

After CMSC reconcile, create buckets if preflight fails (`HeadBucket 403` /
`SignatureDoesNotMatch`). Re-run pytest — `s3_bucket_preflight` exercises bucket
access — or create them explicitly against the S4 endpoint:

```bash
# Redeploy regenerates s4-credentials only — it does not update cost-onprem-storage-credentials.
./scripts/deploy-s4-test.sh s4-test cleanup && ./scripts/deploy-s4-test.sh s4-test deploy

# Re-sync credentials into the operator namespace before CMSC reconcile / pytest:
ACCESS_KEY=$(kubectl get secret s4-credentials -n s4-test -o jsonpath='{.data.access-key}' | base64 -d)
SECRET_KEY=$(kubectl get secret s4-credentials -n s4-test -o jsonpath='{.data.secret-key}' | base64 -d)
kubectl create secret generic cost-onprem-storage-credentials \
  --namespace=cost-onprem \
  --from-literal=access-key="${ACCESS_KEY}" \
  --from-literal=secret-key="${SECRET_KEY}" \
  --dry-run=client -o yaml | kubectl apply -f -
```

Required buckets: `koku-bucket`, `ros-data`, `insights-upload-perma`
(`deploy-s4-test.sh` does not create them).

## Step 2 — Operator in-cluster

```bash
export IMG=quay.io/<your-quay-user>/koku-service-operator:clusterbot-test
./hack/deploy-incluster.sh cost-onprem
```

```bash
oc -n cost-onprem rollout status deploy/koku-service-operator --timeout=180s
oc -n cost-onprem logs deploy/koku-service-operator --tail=30
```

## Step 3 — CMSC + cluster-bot patches

Apply the default sample into **`cost-onprem`** (same NS as Step 2):

```bash
DOMAIN=$(oc get ingresses.config cluster -o jsonpath='{.spec.domain}')
KEYCLOAK_HOST="$(oc get route keycloak -n keycloak -o jsonpath='{.spec.host}')"
test -n "$KEYCLOAK_HOST"
KEYCLOAK_URL="https://${KEYCLOAK_HOST}"

oc apply -f config/samples/service.costmanagement_v1alpha1_costmanagementserviceconfig.yaml

oc patch cmsc cost-onprem -n cost-onprem --type merge -p "{
  \"spec\": {
    \"global\": {\"clusterDomain\": \"${DOMAIN}\"},
    \"objectStorage\": {
      \"endpoint\": \"s4.s4-test.svc.cluster.local\",
      \"port\": 7480,
      \"useSSL\": false,
      \"secretName\": \"cost-onprem-storage-credentials\",
      \"s3\": {\"region\": \"us-east-1\"}
    },
    \"auth\": {
      \"keycloak\": {
        \"url\": \"http://keycloak-service.keycloak.svc:8080\",
        \"issuerURL\": \"${KEYCLOAK_URL}\"
      }
    }
  }
}"
```

`hack/deploy-incluster.sh` → `deploy-dev.sh` already grants `anyuid` to the
default ServiceAccount in the operator namespace.

Why these patches (sample defaults are ODF/CRC, not cluster-bot S4):

| Field | Sample default | Cluster-bot value |
|-------|----------------|-------------------|
| `objectStorage.endpoint` | ODF `s3.openshift-storage.svc` | S4 in `s4-test` |
| `objectStorage.s3.region` | unset | `us-east-1` (SigV4 with S4) |
| `auth.keycloak.url` | `https://keycloak...:443` | RHBK in-cluster HTTP `:8080` |
| `auth.keycloak.issuerURL` | commented | public Route host (tokens use this iss) |

Watch reconcile (~10–20 min):

```bash
oc -n cost-onprem get cmsc cost-onprem -o jsonpath='{range .status.conditions[*]}{.type}={.status} ({.reason}){"\n"}{end}'
oc -n cost-onprem get pods
```

Example good end state:

```text
SchemaUpToDate=True (MigrationComplete)
Available=True (KokuAvailable)
GatewayReady=True (GatewayReady)
IngressReady=True (IngressReady)
StorageReady=True (StorageReady)
UIReady=False (OAuthClientSecretMissing)
```

## Step 4 — Pytest

```bash
export NAMESPACE=cost-onprem
export HELM_RELEASE_NAME=cost-onprem
export KEYCLOAK_NAMESPACE=keycloak

# Without UI (recommended first pass on Mac)
./scripts/run-pytest.sh --no-ui -v

# Full suite including UI (see Playwright section)
./scripts/run-pytest.sh -v
```

Reports: `test/pytest/reports/junit.xml`, `test/pytest/reports/report.html`.

### Baseline numbers (reference only)

| Run | Result | Notes |
|-----|--------|-------|
| Elkana PR #56 (reference) | 151 passed, 62 failed, 17 skipped, 74 errors | Aggregate from PR; not apples-to-apples with below |
| Cluster-bot Aug 2026 (`--no-ui`) | 209 passed, 5 failed, 30 errors, 9 skipped | 253 tests; after S3 + Mac SSL fixes; MCE AWS OCP 4.20 |

Compare runs using the **same** `run-pytest.sh` flags and pytest markers.

### Pytest on macOS

Homebrew Python often exports `REQUESTS_CA_BUNDLE`, which makes `requests`
ignore `session.verify=False` unless `session.trust_env=False`.

**Workaround (until fixed in repo):**

```bash
unset REQUESTS_CA_BUNDLE SSL_CERT_FILE
./scripts/run-pytest.sh --no-ui -v
```

A code fix (`trust_env=False` in session fixtures + unset in `run-pytest.sh`)
is tracked for a follow-up PR.

### UI tests (Playwright)

`--no-ui` excludes `ui`, `performance`, and `helm` markers.

To run UI tests:

```bash
cd test/pytest && python3 -m venv .venv && source .venv/bin/activate
pip install -r requirements.txt
playwright install chromium
python -c "from playwright.sync_api import sync_playwright; p = sync_playwright().start(); b = p.chromium.launch(); b.close(); p.stop(); print('OK')"
```

Also requires `UIReady=True` — mirror OAuth secret:

```bash
NAMESPACE=cost-onprem CR_NAME=cost-onprem ./config/samples/byoi/mirror-ui-oauth-secret.sh
```

On macOS, `run-pytest.sh` may fail during `playwright install` (temp dir /
browser cache). Install browsers manually (above) or run UI tests on Fedora/CI.

## Known test gaps (Aug 2026 triage)

Failures after a healthy stack are concentrated in a few buckets — not hundreds
of unrelated bugs:

| Bucket | Symptom | Likely cause |
|--------|---------|--------------|
| **interpod suite** | `fixture 'rh_identity_header' not found` | Missing fixture in `suites/interpod/conftest.py` (PR bug) |
| **cost_management / rbac E2E setup** | `Could not get OpenShift source type ID` | `register_source()` curls from ingress pod; `complete_flow` uses test-runner pod |
| **E2E pipeline** | manifest/masu timeout after upload | Cluster-bot processing slow or backlog |
| **infra migration log** | missing string `Migrations completed successfully` | Brittle log assertion vs current koku wording |
| **tagging skips** | no tags in DB | Expected without full ingestion |
| **ROS recommendations API** | 404 on `/recommendations/openshift` | Expected when `spec.ros.enabled: false` (beta default). Pytest skips `suites/ros/` and E2E `test_09` when ROS is off — not product failures |

**ROS API coverage:** Beta cluster-bot runs use `ros.enabled: false` on the CMSC
sample. Recommendations API tests require `spec.ros.enabled: true` plus ROS/Kruize
images and reconciliation — out of G0 beta scope.

## Optional — UI login (`Phase=Ready`)

See [pre-prod-install.md](pre-prod-install.md) and `mirror-ui-oauth-secret.sh`.

## Problems we hit (and fixes)

### `rhbk_args[@]: unbound variable` (Mac only)

macOS `/bin/bash` 3.2 + `set -u`. Use Homebrew bash for deploy scripts.

### Integrated registry push from laptop

`x509: certificate signed by unknown authority` on `docker login` to cluster
registry Route. **Fix:** Quay + `deploy-incluster.sh`.

### `exec format error` on operator pod

Apple Silicon built `arm64` image. **Fix:** `docker buildx build --platform linux/amd64`.

### Split operator NS vs CR NS

AllNamespaces reconciles a CMSC outside the operator pod NS. The recommended
lab is still one NS: `deploy-incluster.sh cost-onprem`.

### S3 preflight: 301 pytest errors

All tests error on `s3_bucket_preflight` (`HeadBucket 403` / `SignatureDoesNotMatch`).
**Fix:** S4 redeploy + credential sync + buckets (see Step 1).

### Mass `SSLCertVerificationError` on Mac pytest

**Fix:** unset `REQUESTS_CA_BUNDLE` or apply `trust_env=False` fix.

### Cluster died mid-run (CI Hypershift)

TTL ~2h, single worker overload. **Fix:** MCE, ≥2 workers, run infra first.

## Tear down

```bash
oc delete cmsc -n cost-onprem --all --ignore-not-found
oc delete ns cost-onprem s4-test kafka keycloak --ignore-not-found
```

## Quick reference — scripts

| Script | Cluster-bot pytest path? |
|--------|--------------------------|
| `deploy-test-cost-onprem.sh --deploy-s4 --skip-helm ...` | **Yes** — infra (RHBK, Kafka, S4) |
| `hack/deploy-incluster.sh` | **Yes** — operator (preferred) |
| `hack/deploy-byoi.sh` | Optional — full BYOI deps (see pre-prod) |
| `hack/clusterbot-smoke.sh` | **Alternate** — Redpanda smoke, not this path |
| `install-cmsc.sh` / `make deploy` | **Avoid** on cluster-bot from laptop |
| `make run` | **No** — BYOI hostnames not reachable from laptop |
| `scripts/run-pytest.sh` | **Yes** — test orchestration |

## Related JIRA

- [COST-8121](https://redhat.atlassian.net/browse/COST-8121) — reproduce and triage pytest on cluster-bot (active)
- [COST-7697](https://redhat.atlassian.net/browse/COST-7697) — adapt pytest suite for operator (**Closed**)
- PR #56 — original pytest port (merged to `main`)
