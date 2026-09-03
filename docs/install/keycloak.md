# Configure external Keycloak (RHBK)

The Cost Management service operator **never deploys Keycloak**. You supply
Red Hat build of Keycloak (RHBK) or a compatible OIDC provider. Set
`spec.auth.keycloak.url` on the `CostManagementServiceConfig` so Envoy can
fetch JWKS. The operator does not auto-detect Keycloak.

This page matches what the gateway Lua filter requires. Realm, clients, and
JWT claims must match the CR.

## Spec fields

| Field | Required | Purpose |
|-------|----------|---------|
| `spec.auth.keycloak.url` | Yes | In-cluster base URL used to fetch JWKS. Prefer a Service URL so Envoy does not depend on the OpenShift router. Example: `http://keycloak-service.keycloak.svc:8080` |
| `spec.auth.keycloak.issuerURL` | No | Token `iss` value. Must use `https://` when set. Set this to the public Route URL when RHBK issues tokens with that `iss` even if clients talk to the in-cluster Service. When empty, issuer is derived from `url` + realm |
| `spec.auth.keycloak.realm` | No | Default `kubernetes` |
| `spec.auth.keycloak.audiences` | No | Default `cost-management-operator`, `cost-management-ui` |
| `spec.auth.keycloak.tls.caCertSecretName` | No | Secret with key `ca.crt` to verify Keycloak TLS (needed when `issuerURL` is a Route) |

`url` is the JWKS fetch URL. `issuerURL` is the token `iss` Envoy validates.
They are often different on OpenShift: HTTP Service for JWKS, HTTPS Route for
`iss`.

Admission rejects an empty or whitespace-only `url`. A failed JWKS probe sets
`AuthenticationReady=False`. That does not by itself block `GatewayReady`.

## Clients

Create these in the realm named by `spec.auth.keycloak.realm` (default
`kubernetes`).

### UI (confidential)

oauth2-proxy on the UI Deployment uses a confidential client. Store the
credentials in a Secret in the **CR namespace**. Default name:
`{cr-name}-ui-oauth-client` (override with `spec.ui.oauthClientSecretRef.name`).

| Key | Required |
|-----|----------|
| `client-id` | Yes |
| `client-secret` | Yes |

Redirect URI the client must allow:

`https://{cr-name}-ui-{namespace}.{apps-domain}/oauth2/callback`

Until this Secret exists with both keys, `UIReady` stays False
(`OAuthClientSecretMissing`). Core APIs can still become `Available`.

### CMMO (`client_credentials`)

Reporting clusters that upload with Cost Management Metrics Operator need a
confidential client with the **client credentials** grant. See
[cmmo.md](cmmo.md).

### Other API clients

The gateway accepts any client whose access-token `aud` intersects
`spec.auth.keycloak.audiences`. You do not need a dedicated operator-owned
client beyond the audiences you configure.

## Audiences

Default audiences:

- `cost-management-operator`
- `cost-management-ui`

Add an audience mapper (or a dedicated client scope) on each client so
**access tokens** include those `aud` values. Envoy rejects tokens whose
`aud` does not match `spec.auth.keycloak.audiences`.

## Protocol mappers (required claims)

Envoy Lua hard-401s if either claim is missing. There are **no** fallback
claim names (`organization_id`, `tenant_id`, `account_id`, and similar are
ignored).

| Claim | Required | Format |
|-------|----------|--------|
| `org_id` | Yes | String matching `^[a-zA-Z0-9._-]+$`, max 128 characters |
| `account_number` | Yes | Same as `org_id` |

Use protocol mappers (or user attributes copied into the access token) so
every access token carries **exactly** these names.

### Optional claims and roles

| Claim / role | If missing |
|--------------|------------|
| `preferred_username` | Lua uses `sub`, then `"user"` |
| `email` | Lua synthesizes `{username}@example.com` |
| Realm role `org-admin` | `is_org_admin` in `X-Rh-Identity` is `false` |

## Authorization

Koku runs with `ENHANCED_ORG_ADMIN=False`. Authorization goes through
insights-rbac. Do not disable that path or treat `org-admin` as a bypass for
RBAC.

Keycloak-to-RBAC principal sync (`spec.rbac.keycloakSync`) is a separate
CronJob. It is not required for the JWT gateway.

## Upgrade note

`spec.auth.keycloak.namespace` is removed. If a live CR still sets that field,
delete it before upgrading the CRD.
