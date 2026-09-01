# RBAC cache flush

After RBAC permission changes (role assignments, group membership, policy
updates), permission checks may stay stale until caches expire.

| Cache | Where | Redis DB | Default TTL | Effect |
|-------|-------|----------|-------------|--------|
| insights-rbac **AccessCache** | Valkey/Redis | **DB 1** (`rbac::policy::...`) | 600s | Stale authorization policy data |
| insights-rbac Django `CACHES` | Valkey/Redis | **DB 2** | varies | Stale ORM/query cache |
| Koku RBAC response cache | Valkey/Redis | shared instance | 300s (`RBAC_CACHE_TIMEOUT`) | Stale `/access/` responses |

For most operations the TTL is enough. Flush manually only when you need
**immediate** effect (revocation, demo, troubleshooting).

## One-command flush (recommended)

From the repository root, with `oc` or `kubectl` logged into the cluster:

```bash
./scripts/flush-rbac-cache.sh cost-onprem
```

Operator-managed stacks often set `app.kubernetes.io/instance` to the CMSC name.
When multiple CMSCs share a namespace, pass `--instance`:

```bash
./scripts/flush-rbac-cache.sh cost-onprem --instance cost-onprem
```

Options:

```bash
./scripts/flush-rbac-cache.sh cost-onprem --dry-run
./scripts/flush-rbac-cache.sh cost-onprem --django-only
./scripts/flush-rbac-cache.sh cost-onprem --valkey-only
```

## What the script does

1. **Django-side flush** — `kubectl exec` into the `rbac-api` pod and run:
   - `cache.clear()` — Django `CACHES` (Redis DB 2)
   - `purge_cache_for_all_tenants()` from `management.seeds` — AccessCache (Redis DB 1)

   This matches pytest/bootstrap for `cache.clear()` and adds the AccessCache
   purge that matters when Valkey is not reachable from the namespace.

2. **Valkey flush** — `valkey-cli FLUSHALL` in the bundled cache pod
   (`app.kubernetes.io/component=cache`, with fallback to `valkey` for older
   installs). `FLUSHALL` clears **all** Redis databases on that instance,
   including AccessCache (DB 1) and Koku RBAC responses.

### When `--django-only` is not enough

`--django-only` runs the Django-side flush above but does **not** call Koku's
cached `/access/` responses (300s TTL) unless you also flush Valkey. On bundled
dev/CI installs, use the full script (default) so `FLUSHALL` covers DB 1 and
Koku.

On BYOI (`spec.cache.deploy: false`), Valkey often lives in another namespace
(for example `cost-byoi-infra`). The script exits 0 after the Django flush and
prints manual instructions — you must flush the external endpoint for immediate
Koku effect.

## External cache (BYOI / production)

When `spec.cache.deploy: false`, there is no Valkey pod in the application
namespace. The script:

1. Clears Django cache + AccessCache via the rbac-api pod
2. Prints instructions to flush the external Redis/Valkey endpoint

```bash
# Read password from the cache Secret — do not pass it via -a (exposes in shell history/ps)
export REDISCLI_AUTH="$(
  oc get secret <secret> -n <namespace> \
    -o jsonpath='{.data.redis-password}' | base64 -d
)"
valkey-cli -h <host> -p <port> FLUSHALL
unset REDISCLI_AUTH
```

Use credentials from `spec.cache.auth.secretName` (key `redis-password`). Add
TLS flags when `spec.cache.tls.enabled` is true.

If you cannot run `FLUSHALL` on a shared production Valkey, wait for TTL expiry
(AccessCache 600s, Koku RBAC 300s) or coordinate a maintenance window.

## Exit codes

| Outcome | Exit code |
|---------|-----------|
| Both steps succeeded | 0 |
| Django flush failed | 1 |
| Bundled Valkey pod found but `FLUSHALL` failed | 1 |
| BYOI — no bundled Valkey pod (manual flush required) | 0 (warning printed) |
| `--valkey-only` with no bundled pod | 0 (manual instructions printed) |

## Warning: FLUSHALL

`FLUSHALL` removes **all** keys in the Valkey database, including Celery task
metadata. This is acceptable for the bundled dev/CI cache (dedicated to the
Cost stack). For shared production Valkey, prefer waiting for TTL expiry or
coordinate a maintenance window.

## Manual fallback

If the script is unavailable:

```bash
RBAC_POD=$(oc get pod -l app.kubernetes.io/component=rbac-api -n <namespace> \
  -o jsonpath='{.items[0].metadata.name}')

oc exec -n <namespace> "$RBAC_POD" -- \
  python manage.py shell -c \
  "from django.core.cache import cache; from management.seeds import purge_cache_for_all_tenants; cache.clear(); purge_cache_for_all_tenants(); print('RBAC and AccessCache cleared')"

VALKEY_POD=$(oc get pod -l app.kubernetes.io/component=cache -n <namespace> \
  -o jsonpath='{.items[0].metadata.name}')

oc exec -n <namespace> "$VALKEY_POD" -- valkey-cli FLUSHALL
```

## Related

- [COST-7592](https://redhat.atlassian.net/browse/COST-7592) — dedicated flush script
- Chart equivalent: `cost-onprem-chart` `docs/operations/rbac-setup.md`
