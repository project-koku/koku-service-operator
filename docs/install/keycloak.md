# Configure external Keycloak (RHBK)

The Cost Management service operator **never deploys Keycloak**. You supply
Red Hat build of Keycloak (RHBK) or a compatible OIDC provider. Set
`spec.auth.keycloak.url` on the `CostManagementServiceConfig` so Envoy can
fetch JWKS. The operator does not auto-detect Keycloak.

This page is the QE and install recipe for realm, clients, client scopes,
audience mappers, and protocol mappers. It matches what the gateway Lua
filter requires. Realm, clients, and JWT claims must match the CR.

## Spec fields

| Field | Required | Purpose |
|-------|----------|---------|
| `spec.auth.keycloak.url` | Yes | URL reachable by Envoy to fetch JWKS. Prefer an in-cluster Service URL so Envoy does not depend on the OpenShift router. Must start with `http://` or `https://` and include a host. Example: `http://keycloak-service.keycloak.svc:8080` |
| `spec.auth.keycloak.issuerURL` | No | Token `iss` value. Must use `https://` when set. Set this to the public Route URL when RHBK issues tokens with that `iss` even if clients talk to the in-cluster Service. When empty, issuer is derived from `url` + realm |
| `spec.auth.keycloak.realm` | No | Default `kubernetes` |
| `spec.auth.keycloak.audiences` | No | Default `cost-management-operator`, `cost-management-ui` |
| `spec.auth.keycloak.tls.caCertSecretName` | No | Secret with key `ca.crt` to verify Keycloak TLS (needed when `issuerURL` is a Route) |

`url` is the JWKS fetch URL. `issuerURL` is the token `iss` Envoy validates.
They are often different on OpenShift: HTTP Service for JWKS, HTTPS Route for
`iss`.

Admission rejects a missing `spec.auth.keycloak` block, an empty or
whitespace-only `url`, a `url` that does not start with `http://` or
`https://`, and a `url` with no host (for example `http://`). A failed JWKS
probe sets `AuthenticationReady=False`. An empty
`url` that reaches reconcile (tests that bypass admission) sets
`AuthenticationReady=False` with reason `OIDCConfigMissing`. That does not by
itself block `GatewayReady`.

## Realm

Create a realm named **`kubernetes`**, unless you set
`spec.auth.keycloak.realm` to something else. Every client, scope, and mapper
in this recipe belongs in that realm.

In the RHBK Admin Console: **Create realm** → Realm name `kubernetes` →
**Create**.

## Clients

Create these in the realm named by `spec.auth.keycloak.realm` (default
`kubernetes`).

### UI (confidential)

oauth2-proxy on the UI Deployment uses a confidential client.

1. **Clients** → **Create client**.
2. Client type: **OpenID Connect**. Client ID: a stable name such as
   `cost-management-ui` (this value goes in the Secret below).
3. Enable **Client authentication** (confidential). Enable **Standard flow**.
   Disable **Direct access grants** in production; labs may leave it on for
   password-grant tests.
4. Valid redirect URI (exact, one entry):

   `https://{cr-name}-ui-{namespace}.{apps-domain}/oauth2/callback`

   Example for CR `cost-onprem` in namespace `cost-onprem` on
   `apps.cluster.example.com`:

   `https://cost-onprem-ui-cost-onprem.apps.cluster.example.com/oauth2/callback`

5. Copy the client secret from **Credentials**.

Store the credentials in a Secret in the **CR namespace**. Default name:
`{cr-name}-ui-oauth-client` (override with `spec.ui.oauthClientSecretRef.name`).

| Key | Required |
|-----|----------|
| `client-id` | Yes |
| `client-secret` | Yes |

Until this Secret exists with both keys, `UIReady` stays False
(`OAuthClientSecretMissing`). Core APIs can still become `Available`.

Assign the audience client scope (below) and the claim mappers (below) to
this client.

### CMMO (`client_credentials`)

Reporting clusters that upload with Cost Management Metrics Operator need a
second confidential client with **only** the **Client credentials** grant
(Service accounts roles). Standard flow is not required.

Create the client in the same realm. Assign the **same** audience client
scope and **same** `org_id` / `account_number` mappers as the UI client.
Service-account tokens must carry those claims (set them on the
service-account user, or as hardcoded mapper values).

On each reporting cluster, store `client_id` / `client_secret` (underscore
keys) as described in [cmmo.md](cmmo.md).

### Other API clients

The gateway accepts any client whose access-token `aud` intersects
`spec.auth.keycloak.audiences`. You do not need a dedicated operator-owned
client beyond the audiences you configure. Any extra client still needs the
audience scope and claim mappers if it will call the gateway.

## Audience mapper (client scope)

Access-token `aud` must intersect `spec.auth.keycloak.audiences`. Defaults:

- `cost-management-operator`
- `cost-management-ui`

Envoy rejects tokens whose `aud` does not match. Create a **dedicated
client scope** and attach it to every Cost Management client (UI and CMMO)
instead of copying mappers onto each client by hand.

1. **Client scopes** → **Create client scope**.
2. Name: `cost-management-audiences`. Protocol: **OpenID Connect**. Type:
   **Optional** (or Default if every client in the realm should get it).
3. Open the new scope → **Mappers** → **Configure a new mapper** →
   **Audience**.
4. Add one Audience mapper per default audience (or per value in
   `spec.auth.keycloak.audiences` if you overrode the CR):

   | Mapper name | Included Client Audience | Add to access token |
   |-------------|--------------------------|---------------------|
   | `aud-cost-management-operator` | `cost-management-operator` | On |
   | `aud-cost-management-ui` | `cost-management-ui` | On |

   Leave **Add to ID token** off unless you need it. Envoy reads the
   **access token**.

5. On the UI client and the CMMO client: **Client scopes** → **Add client
   scope** → `cost-management-audiences` → **Default**.

Decode a token and confirm `aud` is a string or list that includes at least
one configured audience.

## Protocol mappers (required claims)

Envoy Lua hard-401s if either claim is missing. There are **no** fallback
claim names (`organization_id`, `tenant_id`, `account_id`, and similar are
ignored).

| Claim | Required | Format |
|-------|----------|--------|
| `org_id` | Yes | String matching `^[a-zA-Z0-9._-]+$`, max 128 characters |
| `account_number` | Yes | Same as `org_id` |

Use **User Attribute** protocol mappers (or equivalent Hardcoded claim
mappers on a service account) so every **access token** carries **exactly**
these names.

Add the mappers on both the **UI confidential client** and the **CMMO
client_credentials client**. Putting them on a shared client scope (for
example the `cost-management-audiences` scope above) is fine; both clients
must still receive that scope.

For each claim:

1. **Clients** → client (or the shared scope) → **Client scopes** /
   **Mappers** → **Add mapper** → **By configuration** → **User Attribute**.
2. Settings:

   | Field | `org_id` mapper | `account_number` mapper |
   |-------|-----------------|-------------------------|
   | Name | `org_id` | `account_number` |
   | User Attribute | `org_id` | `account_number` |
   | Token Claim Name | `org_id` | `account_number` |
   | Claim JSON Type | String | String |
   | Add to access token | On | On |
   | Add to ID token | Off | Off |
   | Multivalued | Off | Off |

3. On each human user (UI password / authorization-code flow):
   **Users** → user → **Attributes** → add `org_id` and `account_number`
   with values that match `^[a-zA-Z0-9._-]+$` and are at most 128
   characters.
4. On the CMMO service-account user (or via hardcoded mappers): set the
   same two attributes so client-credentials tokens are not 401'd by Lua.

Do **not** map `organization_id`, `tenant_id`, `account_id`, or `account`.
Lua does not read those names.

### Optional claims

| Claim | If missing |
|-------|------------|
| `preferred_username` | Lua uses `sub`, then `"user"` |
| `email` | Lua synthesizes `{username}@example.com` |

## Org-admin realm role (first administrator)

The operator **does not create human users**. A Keycloak (or IdP) admin
provisions users, sets `org_id` and `account_number` attributes (above), and
assigns the **`org-admin` realm role** to anyone who should administer Cost
Management or delegate access to others.

### What it does

On each request, Envoy Lua reads the access token. If the token includes the
**realm role** `org-admin`, Envoy sets `is_org_admin: true` in the
`X-Rh-Identity` header it forwards to koku and insights-rbac. Without that
role, `is_org_admin` is `false`.

insights-rbac uses `is_org_admin` to apply the **admin_default** role bundle
at runtime. That bundle includes permissions such as:

| Role | Permission |
|------|------------|
| Cost Administrator | `cost-management:*:*` |
| User Access administrator | `rbac:*:*` |
| Sources administrator | `sources:*:*` |

Plus other platform admin roles seeded by the operator's RBAC migration Job.
The migrate Job seeds roles into the database; it does **not** assign them
to a named user. The realm role is what grants them on login.

### Why it is required

Without `org-admin`, a user can authenticate (valid JWT with `org_id` and
`account_number`) but receives **no** admin_default permissions — they cannot
manage Cost data or use the User Access UI to delegate roles. There is no
chicken-and-egg for the **first** administrator: assigning the realm role is
enough; the user does not need a pre-existing RBAC group membership.

RBAC migration alone does not make anyone an admin. Keycloak user creation
plus the `org-admin` realm role is the supported first-admin path.

### How to assign it

1. **Realm roles** → create realm role `org-admin` if it does not exist.
2. **Users** → select the user → **Role mapping** → **Assign role** →
   `org-admin`.
3. Confirm the user has `org_id` and `account_number` attributes (protocol
   mappers above).
4. User logs in through the UI (oauth2-proxy → Keycloak).

Service-account users (CMMO) follow the same steps if they need org-admin
claims; most reporting clusters only need the attribute mappers, not
`org-admin`.

### Not the same as the `org-admin` Keycloak subgroup

If you enable `spec.rbac.keycloakSync`, labs often create a group layout such
as `org-{orgId}/org-admin/`. That **subgroup name is for observability only**
in the sync CronJob — it does **not** grant admin access and is not a
substitute for the realm role.

| Mechanism | Grants admin access? |
|-----------|----------------------|
| **`org-admin` realm role** | **Yes** — JWT → `is_org_admin=true` → admin_default |
| **`org-admin` Keycloak subgroup** | **No** — sync logs membership; admin stays JWT-based |

For gateway authentication, assign the **realm role**.

## UI login troubleshooting

The UI Deployment runs **oauth2-proxy** beside the nginx app container.
oauth2-proxy performs OIDC discovery against `spec.auth.keycloak.issuerURL`
(or the issuer derived from `url` + realm when `issuerURL` is unset).

That is a **different** URL than the JWKS probe the operator uses for
`AuthenticationReady`. Envoy fetches JWKS from `spec.auth.keycloak.url`
(often an in-cluster HTTP Service). The UI talks to the **public issuer**
(typically an HTTPS OpenShift Route). `AuthenticationReady=True` and
`UIReady=True` do **not** guarantee the UI Route serves traffic.

### `UIReady` vs a working UI

`UIReady=True` means the UI OAuth client Secret exists (and the UI Route is
admitted when a cluster domain is known). It does **not** check that UI pods
are Ready. Always confirm the Deployment after conditions look good:

```bash
export NAMESPACE=cost-onprem
export CR_NAME=cost-management-minimal

oc -n "$NAMESPACE" get deploy "${CR_NAME}-ui"
oc -n "$NAMESPACE" get pods -l app.kubernetes.io/component=ui
oc -n "$NAMESPACE" logs deploy/"${CR_NAME}"-ui -c oauth-proxy --tail=50
```

Both containers (`oauth-proxy` and the app) should be `Running` and the
Deployment should show `READY` matching desired replicas.

### TLS when `issuerURL` is a public Route

When RHBK issues tokens with `iss` on a public HTTPS Route, set
`spec.auth.keycloak.issuerURL` to that Route host. oauth2-proxy then verifies
the Route certificate using `spec.auth.keycloak.tls.caCertSecretName` (Secret
key `ca.crt`).

Without a custom CA Secret, the operator mounts the OpenShift **service-CA**
for `--provider-ca-file`. Public Routes use the **ingress/router** CA, not
the service CA. A common symptom is oauth-proxy in CrashLoopBackOff with:

`x509: certificate signed by unknown authority`

**Production fix:** create a Secret with the ingress/router CA (or a bundle
that includes it) and set `spec.auth.keycloak.tls.caCertSecretName`. See
[production.md](production.md).

**Lab only:** `spec.auth.keycloak.tls.insecureSkipVerify: true` skips TLS
verification for oauth2-proxy. Do not use in production.

Example (replace Secret name and issuer host):

```yaml
spec:
  auth:
    keycloak:
      url: "http://keycloak-service.keycloak.svc.cluster.local:8080"
      issuerURL: "https://keycloak-keycloak.apps.cluster.example.com"
      tls:
        caCertSecretName: "keycloak-ingress-ca"
```

### Symptom table

| Symptom | Check |
|---------|-------|
| `UIReady=False`, reason `OAuthClientSecretMissing` | Create `{cr-name}-ui-oauth-client` with `client-id` / `client-secret` |
| `UIReady=True`, Route shows "Application is not available" | `oc logs deploy/{cr}-ui -c oauth-proxy` for TLS or OIDC errors |
| oauth-proxy `x509: unknown authority` on issuer host | Set `tls.caCertSecretName` to ingress CA when `issuerURL` is a Route |
| Login redirect fails after UI loads | Redirect URI in Keycloak must match `https://{cr}-ui-{ns}.{domain}/oauth2/callback` |

## Authorization

Koku runs with `ENHANCED_ORG_ADMIN=False`. Authorization goes through
insights-rbac. The `org-admin` realm role is the supported way to receive
admin_default permissions at login; it is not a bypass of RBAC.

Keycloak-to-RBAC principal sync (`spec.rbac.keycloakSync`) is a separate
CronJob. It copies top-level `org-{orgId}` group members into RBAC
Principals. It is not required for JWT gateway auth and does not replace
assigning the `org-admin` realm role to administrators.

## Upgrade note

`spec.auth.keycloak.namespace` is removed. If a live CR still sets that field,
delete it before upgrading the CRD.

Older operator versions silently defaulted `spec.auth.keycloak.url` to
`https://keycloak.keycloak.svc.cluster.local` when the field was empty. That
default is gone. If a live CR has no `spec.auth.keycloak.url`, the gateway
will break after upgrade until you set the field. Patch
`spec.auth.keycloak.url` before or during the CRD upgrade, same as the
namespace field.
