# COST-8167 Cost principal-profile service

Cost owns Keycloak interpretation behind a small HTTP profile interface.
insights-rbac BYPASS principals call that interface; the operator deploys it as
a sidecar on the RBAC API pod (same operator image, `/principal-profile`).

## Architecture

```
UI → Envoy → insights-rbac /api/rbac/v1/principals
                 │
                 │ PRINCIPAL_PROFILE_SERVICE_URL=http://127.0.0.1:8099
                 ▼
         principal-profile sidecar (Cost)
                 │
                 ▼
         Keycloak Admin API
         (org-{orgId} members + org-admin realm role)
```

## Contract

```http
GET /v1/orgs/{org_id}/profiles
```

```json
{
  "profiles": {
    "admin": {
      "email": "admin@example.com",
      "first_name": "Org",
      "last_name": "Admin",
      "is_org_admin": true,
      "enabled": true
    }
  }
}
```

## Local stub (no Keycloak)

```bash
make run-principal-profile
export PRINCIPAL_PROFILE_SERVICE_URL=http://127.0.0.1:8099
```

When `KEYCLOAK_*` is unset, the binary serves fixture users (`admin` / `alice` / `bob`).
The process binds `127.0.0.1:8099` by default (loopback only).

## Operator wiring

When `spec.rbac.keycloakSync.enabled` is true, `RBACAPIDeployment` adds:

- Sidecar `principal-profile` using `OperatorImage` + command `/principal-profile`
- Listen address `127.0.0.1:8099` (not the pod IP); probes use `/principal-profile --healthcheck`
- Keycloak Admin env/secret/CA on the **sidecar only**
- `PRINCIPAL_PROFILE_SERVICE_URL=http://127.0.0.1:8099` on the RBAC API container

Keycloak sync creates/preserves Principals for disabled Keycloak users so the
Users table can show them as Inactive via profile enrichment; they are not
treated as prune orphans.

## insights-rbac

Set `PRINCIPAL_PROFILE_SERVICE_URL` (operator does this). No Keycloak Admin client
inside insights-rbac.