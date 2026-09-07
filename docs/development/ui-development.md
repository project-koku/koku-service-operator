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

### Gate B — data presence (`has_data` / `current_month_data`)
`apps/koku-ui-hccm/src/routes/utils/providers.ts`. While `has_data: false` the UI
renders `<NoData />`. Cost breakdowns, cluster cards and historical selectors
stay hidden until Celery/masu processes at least one payload and flips
`has_data: true`.

### Gate C — user access / RBAC (`user-access`)
`GET /api/cost-management/v1/user-access/`. Controls Cost Models, Tag Management,
etc. Without an org-admin role, Settings and Cost Model tabs are locked.

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
`--org-id ID` (default `1234567`), `--no-venv`. `oc` must be logged in to the
target cluster. masu processes the upload asynchronously off Kafka; data appears
in the UI a few minutes later. Each run adds another source.

### Option 2: the operator E2E suite

`./scripts/run-pytest.sh --e2e` also seeds as a side effect (`test_01` registers
a source, `test_03` uploads NISE data); run it with cleanup disabled to keep the
data:

```bash
E2E_CLEANUP_BEFORE=false E2E_CLEANUP_AFTER=false \
NAMESPACE=cost-byoi HELM_RELEASE_NAME=cost-management KEYCLOAK_NAMESPACE=keycloak \
  ./scripts/run-pytest.sh --e2e --no-ui
```

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
export API_TOKEN=$(curl -sk -X POST \
  https://keycloak-keycloak.apps-crc.testing/realms/kubernetes/protocol/openid-connect/token \
  -d grant_type=client_credentials -d client_id=$CLIENT_ID -d client_secret=$CLIENT_SECRET \
  | jq -r '.access_token')
export API_PROXY_URL=https://cost-management-gateway-cost-byoi.apps-crc.testing/api/cost-management/v1

npm run start:onprem
```

Open http://localhost:9001. Saves in `apps/koku-ui-hccm/src/` or
`apps/koku-ui-onprem/src/` hot-reload in the browser.

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
3. Rebuild. Once the upstream PR merges and the submodule is bumped past it,
   drop the shim and its two config lines.
