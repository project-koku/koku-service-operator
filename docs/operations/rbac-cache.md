# RBAC cache flush

After RBAC permission changes (role assignments, group membership, policy
updates), permission checks may stay stale until caches expire.

| Cache | Where | Default TTL | Effect |
|-------|-------|-------------|--------|
| insights-rbac Django cache | RBAC API pod | varies | Stale RBAC query results |
| Koku RBAC response cache | Valkey/Redis | 300s (`RBAC_CACHE_TIMEOUT`) | Stale `/access/` responses |

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

1. **Django cache** — `kubectl exec` into the `rbac-api` pod and run
   `cache.clear()` via `manage.py shell`.
2. **Valkey cache** — `valkey-cli FLUSHALL` in the bundled cache pod
   (`app.kubernetes.io/component=cache`, with fallback to `valkey` for older
   installs).

## External cache (BYOI / production)

When `spec.cache.deploy: false`, there is no Valkey pod in the namespace. The
script clears the Django cache and prints instructions to flush the external
Redis/Valkey endpoint yourself:

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
  "from django.core.cache import cache; cache.clear(); print('RBAC cache cleared')"

VALKEY_POD=$(oc get pod -l app.kubernetes.io/component=cache -n <namespace> \
  -o jsonpath='{.items[0].metadata.name}')

oc exec -n <namespace> "$VALKEY_POD" -- valkey-cli FLUSHALL
```

## Related

- [COST-7592](https://redhat.atlassian.net/browse/COST-7592) — dedicated flush script
- Chart equivalent: `cost-onprem-chart` `docs/operations/rbac-setup.md`
