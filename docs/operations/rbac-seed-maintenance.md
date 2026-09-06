# RBAC permission and role seed maintenance

The operator RBAC migration Job seeds cost-management permissions and roles into
the insights-rbac PostgreSQL database. The permission/role definitions are a
snapshot of [project-kessel/rbac-config](https://github.com/project-kessel/rbac-config)
`configs/prod/`.

## Source of truth

| rbac-config path | Embedded snapshot |
|------------------|-------------------|
| `configs/prod/permissions/cost-management.json` | `internal/resources/rbac_seed/data/cost-management.permissions.json` |
| `configs/prod/roles/cost-management.json` | `internal/resources/rbac_seed/data/cost-management.roles.json` |
| `configs/prod/permissions/sources.json` | `internal/resources/rbac_seed/data/sources.permissions.json` |
| `configs/prod/roles/sources.json` | `internal/resources/rbac_seed/data/sources.roles.json` |

The pinned upstream commit is recorded in
`internal/resources/rbac_seed/rbac_config_ref`.

## What the migration Job does

The `{cr}-rbac-migrate` Job (see `internal/resources/migration.go`) runs, in
order:

1. `manage.py migrate`
2. `manage.py seeds --skip-notifications` — platform permissions/roles/groups
   from the RBAC image **plus** cost-management/sources JSON mounted from
   operator ConfigMaps at insights-rbac's standard seed paths:
   - `{cr}-rbac-seed-permissions` → `management/role/permissions/`
   - `{cr}-rbac-seed-definitions` → `management/role/definitions/`

This matches console.redhat.com: mount rbac-config JSON, then run native
`manage.py seeds`. `seed_group()` wires public **Default admin access** to all
`admin_default=True` roles (Cost Administrator, Sources administrator).

ConfigMaps are applied in `reconcileSharedConfig` before the migration Job is
created.

## Drift check (CI and local)

```bash
make check-rbac-seed
```

This compares embedded JSON files to rbac-config at the pinned ref. CI runs the
same check in the `rbac-seed-sync` workflow job.

To skip the upstream fetch in unit tests (offline):

```bash
RBAC_SEED_SKIP_UPSTREAM=1 go test ./internal/resources/rbac_seed/...
```

## When rbac-config changes

1. Update the four JSON files under `internal/resources/rbac_seed/data/` from
   rbac-config `configs/prod/`.
2. Bump `internal/resources/rbac_seed/rbac_config_ref` to the rbac-config
   commit SHA.
3. Run `make check-rbac-seed` and `go test ./internal/resources/...`.
4. If embedded JSON or mount layout changed, bump `rbacSeedRevision` in
   `internal/resources/migration.go` (e.g. `cmseed2` → `cmseed3`) so existing
   clusters re-run the RBAC migration Job.

## Related

- [COST-7593](https://redhat.atlassian.net/browse/COST-7593)
- [COST-7685 gap analysis](../gap_analysis/COST-7685.md) — migration Job lifecycle
- [RBAC cache operations](rbac-cache.md)
