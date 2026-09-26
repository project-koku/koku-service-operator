# Koku UI development against an operator-deployed stack (CRC)

How **Koku UI** works, how it connects to a local **CRC** cluster running
`koku-service-operator`, how its conditional gates work, how to seed synthetic
data, and how to develop and test UI changes.

The UI source lives in the **`project-koku/koku-ui`** repo (`apps/…` paths below
are relative to that checkout); seeding and cluster wiring live in this repo.

For the cluster install itself see
[crc-testing.md](crc-testing.md) / [pre-prod-install.md](pre-prod-install.md).

---

## 1. Cluster & environment state

Local CRC cluster (`apps-crc.testing`), CR namespace `cost-byoi`, CR name
`cost-management`:

| Thing | Value |
|-------|-------|
| Operator | `deployment/koku-service-operator` in `cost-byoi` |
| UI Deployment | `deployment/cost-management-ui` — `oauth-proxy` (`registry.redhat.io/rhceph/oauth2-proxy-rhel9:v7.6.0`) + `app` (Nginx static bundle, arm64 image e.g. `quay.io/martin_povolny/koku-ui-onprem:crc-arm64`) |
| UI Route | https://cost-management-ui-cost-byoi.apps-crc.testing |
| API gateway (Envoy) | https://cost-management-gateway-cost-byoi.apps-crc.testing/api/cost-management/v1/ |
| Keycloak | https://keycloak-keycloak.apps-crc.testing/ (namespace `keycloak`, realm `kubernetes`) |

Resource names follow `<CR name>-<component>`; swap `cost-management` /
`cost-byoi` for your CR name / namespace.

### UI test routes

When validating UI flows manually against the CRC cluster or local dev server:

| Screen | Route path |
|--------|------------|
| Cost Management (OpenShift) | `/openshift` |
| Cost Overview | `/overview` |
| Cost Explorer | `/explorer` |
| Cost Models | `/cost-models` |
| Settings | `/settings` |
| IAM: Roles | `/iam/user-access/roles` |
| IAM: Users | `/iam/user-access/users` |
| IAM: Groups | `/iam/user-access/groups` |
| Keycloak Admin Console | https://keycloak-keycloak.apps-crc.testing/admin/kubernetes/console/ |

### Test accounts (Keycloak `kubernetes` realm)

The local cluster setup includes standard test personas:

- **Org Administrator** (`admin` / `admin`): Has the `org-admin` role in Keycloak. Bypasses Koku and RBAC checks (`is_org_admin: true`); grants full access to Settings, Cost Models, and IAM.
- **Provider Viewer** (`viewer` / `viewer`): Standard non-admin user (`is_org_admin: false`). Member of group **"Cost OpenShift Viewers"** bound to role **"Cost OpenShift Viewer"** with `cost-management:openshift.cluster:*` and no `sources:*:*` permissions. Used to test scoped and non-admin view permissions.
- **Keycloak Master Admin** (`temp-admin`): Credentials stored in `secret/keycloak-initial-admin` in namespace `keycloak` (`oc get secret keycloak-initial-admin -n keycloak -o jsonpath='{.data.password}' | base64 -d`). Used to configure Keycloak realm settings, client scopes, and token lifespans.

---

## 2. Architecture & mental model

A React SPA, not server-rendered HTML:

```
        Browser (React SPA)  ──REST/JSON──▶  cost-management-gateway (Envoy)
                  ▲                                   │
                  └────────────JSON───────────────────▼
                                          Koku Django backend (cost-management-koku-api)
```

1. **SPA** — the browser downloads HTML + JS bundles + CSS once.
2. **Client-side rendering** — JavaScript builds and updates the DOM.
3. **API-driven** — the frontend talks to Koku only via REST/JSON.

---

## 3. Monorepo structure (`apps/` in `koku-ui`)

npm monorepo with federated apps:

| Directory | Role |
|-----------|------|
| `apps/koku-ui-onprem/` | **Host shell** — masthead, nav, production `Containerfile`; loads the others via Webpack Module Federation |
| `apps/koku-ui-hccm/` | **Core Cost Management** — Overview, OpenShift details, Cost Explorer, Cost Models, Settings, charts. **~90% of changes live here.** |
| `apps/koku-ui-ros/` | ROS UI (recommendation cards/tables) |
| `apps/koku-ui-sources/` | Sources UI (integrations / data source config) |
| `apps/rbac-ui-onprem/` | RBAC UI (users & roles) |

---

## 4. Key frontend concepts

- **React components (`.tsx`)** — reusable TypeScript functions returning JSX.
  ```tsx
  export const CostTitle = ({ name }: { name: string }) => (
    <Title headingLevel="h1">{name}</Title>
  );
  ```
- **PatternFly** — Red Hat's React component library (`@patternfly/react-core`,
  `-table`, `-charts`): `Button`, `Table`, `Card`, `Modal`, …
- **State & data fetching** — `useState` / `useEffect` or Redux. Filter/date
  changes call helpers in `apps/koku-ui-hccm/src/api/`, receive JSON, update
  state, re-render.

---

## 5. UI gating: why parts of the UI are hidden on a fresh cluster

### Gate A — provider presence (`hasProviders`)
`GET /api/cost-management/v1/sources/`; `apps/koku-ui-hccm/src/utils/userAccess.ts`.
If `providers.meta.count === 0`, Overview renders `<NoProviders />` and OpenShift
renders an empty prompt. On a fresh cluster `api_provider` has 0 rows and Koku
logs `Tenant does not exist` until a source is registered.

> **Provider viewers & the `/sources/` discovery trap ([project-koku/koku#6288](https://github.com/project-koku/koku/pull/6288))**:
> The UI unconditionally queries `GET /api/cost-management/v1/sources/` to check
> whether sources exist and whether `has_data: true`. Previously, non-admin users
> who only had provider permissions (e.g. `cost-management:openshift.cluster:*`)
> received `403 Forbidden` on `/sources/` because they lacked `sources:*:read`.
> This caused the OpenShift cost views to hang in an empty or "Still processing data"
> state for valid viewers.
> In on-prem, `SAFE_METHODS` (`GET`, `HEAD`, `OPTIONS`) on `/sources/` are relaxed
> so that any user with an org-wide wildcard read (`"*"`) on any provider resource
> type is permitted to list sources, while write actions (`POST`, `PATCH`, `DELETE`)
> remain strictly `403`.

### Gate B — data presence (`has_data` / `current_month_data`)
`apps/koku-ui-hccm/src/routes/utils/providers.ts`. While `has_data: false` the UI
renders `<NoData />`. Cost breakdowns, cluster cards and historical selectors
stay hidden until Celery/masu processes at least one payload and flips
`has_data: true`.

### Gate C — user access / RBAC (`user-access`)
`GET /api/cost-management/v1/user-access/`. Controls Cost Models, Tag Management,
etc. Without an org-admin role, Settings and Cost Model tabs are locked.
For non-admin roles, permissions must be created and assigned in the RBAC UI or
API (`cost-management-rbac-api`). Note that the RBAC API requires
`ROLE_CREATE_ALLOW_LIST="cost-management,sources"` (managed by the operator in
`internal/resources/rbac.go`) to allow creating roles for cost-management.

### Gate D — on-prem mode toggle (`isOnPremEnabled`)
`apps/koku-ui-hccm/src/components/featureToggle.ts`. Hides standalone AWS/Azure/GCP
tabs in on-prem mode.

---

## 6. Ingestion pipeline (what unlocks the UI)

```
Source registration   POST /api/cost-management/v1/sources  → tenant schema + api_provider row
        ▼
NISE data generation  synthetic nodes/pods/CPU/memory/PVCs → .tar.gz
        ▼
Ingress upload        POST /api/ingress/v1/upload → MinIO bucket (koku-bucket)
        ▼
Celery / masu         processes the payload off Kafka → Parquet (Trino) + summaries (PostgreSQL); provider has_data=true
        ▼
UI unlocked           Overview, OpenShift details, breakdowns, Cost Explorer render live data
```

---

## 7. Seed data into CRC

### Option 1 (recommended): `./scripts/seed-test-data.sh`

From this repo's root. Registers an OpenShift source (via a port-forward to the
internal Koku API + an RBAC bootstrap for the org), generates NISE OCP data, and
uploads it through the gateway/ingress. Reuses the helpers in `test/pytest`
(`e2e_helpers.py`, `conftest.py`, `utils.py`) so it stays in sync with the tests,
but it does **not** run pytest.

```bash
NAMESPACE=cost-byoi HELM_RELEASE_NAME=cost-management KEYCLOAK_NAMESPACE=keycloak \
  ./scripts/seed-test-data.sh --days 7 --source-name dev-ui
```

Flags: `--days N` (default 3), `--clusters N`, `--source-name NAME`,
`--org-id ID` (defaults to the `org_id` claim in the client-credentials JWT),
`--no-venv`. `oc` must be logged in to the target cluster. With `--no-venv`,
install `test/pytest/requirements.txt` into the active Python environment first.
masu processes the upload asynchronously off Kafka; data appears in the UI a
few minutes later. Each run adds another source.

### Option 2: the operator E2E suite

`./scripts/run-pytest.sh --e2e` also seeds as a side effect (`test_01` registers
a source, `test_03` uploads NISE data); run it with cleanup disabled to keep the
data:

```bash
E2E_CLEANUP_BEFORE=false E2E_CLEANUP_AFTER=false \
NAMESPACE=cost-byoi HELM_RELEASE_NAME=cost-management KEYCLOAK_NAMESPACE=keycloak \
  ./scripts/run-pytest.sh --e2e --no-ui
```

Record the source ID and cluster ID from the output. When the retained data is
no longer needed, use the supported teardown command documented in
[the pytest data-generation guide](../../test/pytest/README.md#alternative-the-e2e-suite).

Heavier (full venv + suite run), but useful when you also want the E2E
assertions. See [test/pytest/README.md](../../test/pytest/README.md#data-generation).

---

## 8. Development workflows (in the `koku-ui` checkout)

### Workflow A (recommended): local dev server with hot reload

```
Browser http://localhost:9001
   ├─ static assets (HTML/JS)  → local Webpack dev server on your Mac
   └─ /api/cost-management/v1   → proxied to the CRC Envoy gateway
```

```bash
# once
git submodule update --init --recursive
npm ci

# fetch a bearer token from CRC Keycloak (client-credentials)
CLIENT_ID=cost-management-operator
CLIENT_SECRET=$(oc get secret keycloak-client-secret-cost-management-operator \
  -n keycloak -o jsonpath='{.data.CLIENT_SECRET}' | base64 -d)
CLIENT_SECRET_FILE="$(mktemp)"
chmod 600 "$CLIENT_SECRET_FILE"
printf '%s' "$CLIENT_SECRET" > "$CLIENT_SECRET_FILE"
trap 'rm -f "$CLIENT_SECRET_FILE"' EXIT
# Use the OpenShift ingress CA that signs the Keycloak route. If your cluster
# uses a different private CA, set KEYCLOAK_CA_BUNDLE to its PEM file instead.
KEYCLOAK_CA_BUNDLE="${KEYCLOAK_CA_BUNDLE:-${TMPDIR:-/tmp}/openshift-ingress-ca.crt}"
if [[ ! -s "$KEYCLOAK_CA_BUNDLE" ]]; then
  oc get configmap default-ingress-cert -n openshift-config-managed \
    -o jsonpath='{.data.ca-bundle\.crt}' > "$KEYCLOAK_CA_BUNDLE"
fi

export API_TOKEN=$(curl --fail --silent --show-error --cacert "$KEYCLOAK_CA_BUNDLE" \
  -X POST \
  https://keycloak-keycloak.apps-crc.testing/realms/kubernetes/protocol/openid-connect/token \
  --data-urlencode grant_type=client_credentials \
  --data-urlencode "client_id=$CLIENT_ID" \
  --data-urlencode "client_secret@$CLIENT_SECRET_FILE" \
  | jq -r '.access_token')
export API_PROXY_URL=https://cost-management-gateway-cost-byoi.apps-crc.testing/api/cost-management/v1

npm run start:onprem
```

Open http://localhost:9001. Saves in `apps/koku-ui-hccm/src/` or
`apps/koku-ui-onprem/src/` hot-reload in the browser.

#### Testing as a specific user persona (`admin` vs `viewer`)

To test non-admin user views (like `viewer`) without running through the full OAuth login flow in the browser, obtain a password-grant token for the specific user and pass it as `API_TOKEN`:

```bash
CLIENT_SECRET=$(oc get secret keycloak-client-secret-cost-management-ui \
  -n keycloak -o jsonpath='{.data.CLIENT_SECRET}' | base64 -d)

KEYCLOAK_CA_BUNDLE="${KEYCLOAK_CA_BUNDLE:-${TMPDIR:-/tmp}/openshift-ingress-ca.crt}"
if [[ ! -s "$KEYCLOAK_CA_BUNDLE" ]]; then
  oc get configmap default-ingress-cert -n openshift-config-managed \
    -o jsonpath='{.data.ca-bundle\.crt}' > "$KEYCLOAK_CA_BUNDLE"
fi

# Use username=viewer / password=viewer (or admin / admin)
export API_TOKEN=$(curl --fail --silent --show-error --cacert "$KEYCLOAK_CA_BUNDLE" -X POST \
  https://keycloak-keycloak.apps-crc.testing/realms/kubernetes/protocol/openid-connect/token \
  -d "grant_type=password" \
  -d "client_id=cost-management-ui" \
  -d "client_secret=$CLIENT_SECRET" \
  -d "username=viewer" \
  -d "password=viewer" | jq -r '.access_token')

export API_PROXY_URL=https://cost-management-gateway-cost-byoi.apps-crc.testing/api/cost-management/v1
npm run -w @koku-ui/koku-ui-onprem start -- --no-open
```

> **Token lifespan in CRC**: In the CRC `kubernetes` realm, the default
> `accessTokenLifespan` is 300s (5 minutes). When developing locally, you can
> increase this to e.g. 7200s (2 hours) via Keycloak Admin Console (**Realm
> Settings > Tokens > Access Token Lifespan**) to avoid having to re-fetch tokens
> every 5 minutes.

### Workflow B: build the container and deploy to CRC

CRC on Apple Silicon runs an **arm64** node, so build arm64:

```bash
docker build -f apps/koku-ui-onprem/Containerfile \
  -t quay.io/<you>/koku-ui-onprem:crc-arm64 .
docker push quay.io/<you>/koku-ui-onprem:crc-arm64

# point the CR at your image (spec.ui.app.image) or, for a quick swap:
oc set image deployment/cost-management-ui app=quay.io/<you>/koku-ui-onprem:crc-arm64 -n cost-byoi
oc rollout status deployment/cost-management-ui -n cost-byoi
```

> The operator manages `deployment/cost-management-ui` with server-side apply, so
> a manual `oc set image` is reverted on the next reconcile (~5 min). For a
> lasting change set `spec.ui.app.image` on the `CostManagementServiceConfig`.

### Workflow C: fast backend / API hot-patching via ConfigMap subPath

When developing or verifying UI changes that depend on Koku backend or permission
fixes (e.g. `sources_access.py`), rebuilding and pushing full container images
takes minutes (especially with cold package caches). Furthermore, the API
container runs as a non-root user on a read-only root filesystem and lacks `tar`,
so `oc cp` / `kubectl cp` is not supported.

Instead, hot-patch single Python files into the running cluster in seconds using
a ConfigMap and `subPath` volume mount:

```bash
# 1. Create or update a ConfigMap from the local file in the koku repo
kubectl create configmap sources-access-patch \
  --from-file=sources_access.py=/path/to/koku/koku/api/common/permissions/sources_access.py \
  -n cost-byoi --dry-run=client -o yaml | kubectl apply -f -

# 2. Patch deployment/cost-management-koku-api with the subPath mount
kubectl patch deployment cost-management-koku-api -n cost-byoi --type strategic -p '
spec:
  template:
    spec:
      containers:
      - name: koku-api
        volumeMounts:
        - name: sources-access-patch
          mountPath: /opt/koku/koku/api/common/permissions/sources_access.py
          subPath: sources_access.py
      volumes:
      - name: sources-access-patch
        configMap:
          name: sources-access-patch
'

# 3. Wait for rollout
kubectl rollout restart deployment/cost-management-koku-api -n cost-byoi
kubectl rollout status deployment/cost-management-koku-api -n cost-byoi
```

### Workflow D: running Cypress E2E live tests against CRC

To automate live verification of complex UI flows (e.g. role creation wizard,
permission tables, resource scoping) against the running local dev server and
CRC gateway:

```bash
# In project-koku/koku-ui:
npx cypress run --config-file apps/koku-ui-onprem/cypress.config.ts \
  --spec apps/koku-ui-onprem/cypress/e2e/live/test-cost-resources-role-creation.cy.ts
```

---

## 9. Codebase quick reference (`koku-ui` repo)

### Pages (`apps/koku-ui-hccm/src/routes/`)
- OpenShift details — `details/ocpDetails/`
- OpenShift breakdown — `details/ocpBreakdown/`
- Cost Explorer — `explorer/`
- Overview — `overview/`
- Settings (Cost Models, Tags, Currencies) — `settings/`

### API integration (`apps/koku-ui-hccm/src/api/`)
- client & endpoints — `api/` (`reports/`, `costModels.ts`, …)
- query-param handlers — `api/queries/`

### Host shell & navigation (`apps/koku-ui-onprem/src/`)
- layout / masthead — `components/AppLayout.tsx`
- nav sidebar links — `components/NavItem.tsx`
- route switching / microfrontend loading — `components/AppRoutes.tsx`
- container build spec — `Containerfile`

---

## 10. Building the RBAC UI with fixes not yet in `insights-rbac-ui`

**This applies to `rbac-ui-onprem` only.** It is the sole app that vendors an
upstream repo it can't edit: the RBAC screens come from
`RedHatInsights/insights-rbac-ui`, pulled in as the `vendor/insights-rbac-ui`
submodule and consumed via `"insights-rbac-frontend": "file:../../vendor/insights-rbac-ui"`.
`koku-ui-hccm` (cost management), `koku-ui-ros`, `koku-ui-sources` and the
`koku-ui-onprem` host shell are first-party source in this monorepo — patch those
by editing the `.tsx` directly and rebuilding.

`apps/rbac-ui-onprem` has a **module-replacement** hook in `webpack.config.ts`
(`insightsRbacModuleReplacements` + `resolve.alias`) that swaps individual
upstream source files for local shims under `src/shims/insights-rbac/` at build
time — no submodule bump, no upstream edits. This is how on-prem-only fixes and
not-yet-merged upstream PRs are carried.

(Separately, `libs/onprem-cloud-deps` provides on-prem stubs for the Red Hat
Cloud Services frontend deps — `@redhat-cloud-services/frontend-components*`,
`@unleash/proxy-client-react` — aliased in the `koku-ui-onprem` and
`rbac-ui-onprem` webpack configs so the apps run outside the SaaS Chrome shell.
That's shared plumbing, not a per-fix patch layer.)

Current shims (COST-8190 / COST-8202, upstream
[insights-rbac-ui#2411](https://github.com/RedHatInsights/insights-rbac-ui/pull/2411)):

| Shim | Replaces (upstream) | Why |
|------|---------------------|-----|
| `browser.tsx` | `src/shared/entry/browser.tsx` | `handle401Error` calls `window.location.reload()` — a loop on-prem behind oauth2-proxy; redirect to re-auth instead (also see COST-8202: set `spec.ui.oauthProxy.cookieRefresh`) |
| `CostResources.tsx`, `ReviewStep.tsx`, `addRoleSchema.tsx` | `src/v1/features/roles/add-role/{CostResources,ReviewStep,schema}.tsx` | make cost-management resource definitions optional in the add-role wizard |
| `addRolePermissionsSchema.tsx` | `src/v1/features/roles/add-role-permissions/schema.tsx` | same wizard, permissions step |

### Reproducing the build (colleague, Workflow B)

The shims + `webpack.config.ts` change live on a `koku-ui` branch. From a
checkout of that branch:

```bash
git submodule update --init --recursive   # vendor/insights-rbac-ui stays pinned; shims override it

docker buildx build --platform linux/arm64 \
  -f apps/koku-ui-onprem/Containerfile \
  -t quay.io/<you>/koku-ui-onprem:rbac-shims-arm64 --push .

oc -n cost-byoi patch cmsc cost-management --type=merge -p \
  '{"spec":{"ui":{"app":{"image":{"repository":"quay.io/<you>/koku-ui-onprem","tag":"rbac-shims-arm64"}}}}}'
oc -n cost-byoi rollout status deploy/cost-management-ui
```

No Containerfile change is needed: it already does `COPY apps/rbac-ui-onprem …`
(picks up `src/shims/` and `webpack.config.ts`) and `COPY vendor ./vendor`;
`npm run build:onprem` then applies the aliases.

### Adding another upstream file to a shim

1. Copy the fixed file from the upstream PR to
   `apps/rbac-ui-onprem/src/shims/insights-rbac/<Name>.tsx`.
2. Add a `{ match: /…\/path\/to\/file\.tsx$/, replacement: <shim> }` entry to
   `insightsRbacModuleReplacements` **and** the matching `resolve.alias` line in
   `apps/rbac-ui-onprem/webpack.config.ts` (the regex accepts both
   `insights-rbac-frontend` and `insights-rbac-ui` path segments).
3. **i18n Messages fallback rule**: If the upstream PR introduced new message
   descriptors in `Messages.js` (e.g. `messages.selectResourcesOptionalMsg`),
   the pinned `vendor/insights-rbac-ui/src/Messages.js` won't contain them.
   Always provide fallback descriptors in the shim:
   ```tsx
   const selectResourcesOptionalMsg = (messages as any).selectResourcesOptionalMsg || {
     id: 'rbac.selectResourcesOptional',
     defaultMessage: 'Select resources (optional - default all)',
   };
   ```
   Without this, React throws `TypeError: Cannot read properties of undefined (reading 'id')`
   when `intl.formatMessage(...)` executes at runtime.
4. **Local build**: Build `@koku-ui/rbac-ui-onprem` locally to verify:
   ```bash
   npm run build:onprem -w @koku-ui/rbac-ui-onprem
   ```
5. Rebuild. Once the upstream PR merges and the submodule is bumped past it,
   drop the shim and its two config lines.

> **RBAC backend requirement**: The RBAC API backend (`cost-management-rbac-api`)
> requires `ROLE_CREATE_ALLOW_LIST="cost-management,sources"` (managed by the
> operator via `internal/resources/rbac.go`). If this variable is missing or empty,
> Step 2 of the Role Creation wizard will return an empty permissions table (`data: []`)
> and 0 applications.
