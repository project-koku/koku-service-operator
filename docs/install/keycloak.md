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

### Optional claims and roles

| Claim / role | If missing |
|--------------|------------|
| `preferred_username` | Lua uses `sub`, then `"user"` |
| `email` | Lua synthesizes `{username}@example.com` |
| Realm role `org-admin` | `is_org_admin` in `X-Rh-Identity` is `false` |

To grant org-admin: **Realm roles** → create `org-admin` if needed → assign
it to the user (UI) or the service-account user (CMMO) under **Role
mapping**.

## Authorization

Koku runs with `ENHANCED_ORG_ADMIN=False`. Authorization goes through
insights-rbac. Do not disable that path or treat `org-admin` as a bypass for
RBAC.

Keycloak-to-RBAC principal sync (`spec.rbac.keycloakSync`) is a separate
CronJob. It is not required for the JWT gateway.

## Upgrade note

`spec.auth.keycloak.namespace` is removed. If a live CR still sets that field,
delete it before upgrading the CRD.

Older operator versions silently defaulted `spec.auth.keycloak.url` to
`https://keycloak.keycloak.svc.cluster.local` when the field was empty. That
default is gone. If a live CR has no `spec.auth.keycloak.url`, the gateway
will break after upgrade until you set the field. Patch
`spec.auth.keycloak.url` before or during the CRD upgrade, same as the
namespace field.
