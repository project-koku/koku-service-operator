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
2. `manage.py seeds --skip-notifications` (built-in insights-rbac seeds only)
3. **Cost-management/sources seed** — generated from the embedded rbac-config
   snapshots (`rbac_seed.CostManagementSeedPython()`)
4. On-prem-only steps: `admin_default` groups, `bootstrap_tenants`, and
   `platform_default` cleanup (not in rbac-config)

Steps 3–4 run inside the **RBAC image** pod; the operator only renders the Job
manifest.

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
4. If the generated migration script changed, bump `rbacSeedRevision` in
   `internal/resources/migration.go` (e.g. `cmseed1` → `cmseed2`) so existing
   clusters re-run the RBAC migration Job.

## Beta vs GA

**Beta (COST-7593):** CI drift guard + embedded snapshots (this document).

**GA (future):** replace inline Python with native insights-rbac seeding
(`manage.py seeds` with bundled rbac-config JSON or a dedicated management
command). See `docs/review-follow-ups.md` §4.

## Related

- [COST-7593](https://redhat.atlassian.net/browse/COST-7593)
- [COST-7685 gap analysis](../gap_analysis/COST-7685.md) — migration Job lifecycle
- [RBAC cache operations](rbac-cache.md)
