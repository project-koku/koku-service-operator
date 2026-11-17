# RBAC Go backend (rbac-mini) — beta

`spec.rbac.backend: go` runs the experimental **rbac-mini** binary that is
**built into the operator image**, not a separately published Quay image.

Default (empty / omit `backend`, or `django`) still uses insights-rbac from
`spec.rbac.image` and is unchanged.

## How the image is chosen

| Backend | Workload image |
|---------|----------------|
| `go` | Always `OperatorImage` (`--operator-image` / OLM CSV). **`spec.rbac.image` is ignored.** |
| django / empty | `spec.rbac.image` (insights-rbac) |

If `--operator-image` is empty the operator fails closed at startup (same as
wait-for init containers). Go-backend RBAC API, migrate Job, and Keycloak sync
CronJob all reuse that operator image and invoke `/usr/local/bin/rbac-mini`.

## Bumping the Mini commit

The operator Dockerfile pins Mini with build args (defaults in `Dockerfile`):

```dockerfile
ARG RBAC_MINI_REPO=https://github.com/project-koku/rbac-mini
ARG RBAC_MINI_REF=<git-sha>
```

The Mini commit must build under the operator’s Red Hat go-toolset **1.26.x**
with `GOTOOLCHAIN=local` (so `go.mod` must be `go 1.26.x`, not 1.27+) and must
include native FIPS (`cmd/rbac-go/fips.go`, `--fips-status`).

To adopt a new Mini:

1. Pick a known-good Mini commit (CI green on Mini; includes native FIPS).
   Prefer merging Mini FIPS first, then bump this pin.
2. Change `ARG RBAC_MINI_REF=...` in the operator `Dockerfile`.
3. Rebuild the operator image (`make docker-build`) and run `make verify-fips`
   (checks `/manager`, `/wait-for`, and `/usr/local/bin/rbac-mini`).
4. Ship the new operator release — customers get the new Mini only when they
   upgrade the operator.

Override the clone source for a fork with
`--build-arg RBAC_MINI_REPO=https://github.com/<org>/rbac-mini`
and `--build-arg RBAC_MINI_REF=<sha>`.

### Local Mini override (optional)

Default builds `git clone` the pinned `RBAC_MINI_REF`. To build from a local
checkout instead (unpushed Mini changes):

```bash
make docker-build IMG=... RBAC_MINI_LOCAL_DIR=../rbac-mini
```

`make crc-operator-image` also auto-stages `../rbac-mini` when that `go.mod`
exists; pass `RBAC_MINI_LOCAL_DIR=` (empty) to force the Dockerfile git pin.

## Keycloak sync

Enabling `spec.rbac.keycloakSync` still requires a client secret
(`clientSecretRef`) for both backends. The go path runs
`/usr/local/bin/rbac-mini sync` with the same `KEYCLOAK_*` / `SYNC_*` env as
the Django script.

## Sample snippet (optional)

Django remains the documented default. For lab experiments only:

```yaml
# apiVersion: service.costmanagement.openshift.io/v1alpha1
# kind: CostManagementServiceConfig
# metadata:
#   name: cost-management
# spec:
#   rbac:
#     backend: go   # beta — uses operator image; ignore image below for go
#     # image: ...  # still required for django; unused when backend is go
```

## FIPS

`/usr/local/bin/rbac-mini` is built and verified with the same native Go FIPS
contract as `/manager` and `/wait-for`. See [fips.md](fips.md).
