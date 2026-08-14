# Data Retention Semantics: `dataRetentionMonths` / `RETAIN_NUM_MONTHS`

**Date:** 2026-08-14
**Status:** Active design note
**Affects:** Operator CRD, `KokuCommonEnv`, Koku Global Settings API/UI

---

## Summary

The `RETAIN_NUM_MONTHS` environment variable controls how many months of cost
data Koku retains before the MASU vacuum process purges older records.  Its
interaction with the Koku application is **not** a simple "env var sets the
value" relationship.  Koku implements a three-tier priority system with a
special "env override" detection mechanism that changes behavior depending on
whether the env var's value equals or differs from the application's built-in
code default.

This document captures the exact semantics so that changes to the operator's
CRD default, Go-level fallback, or env-var emission logic are made with full
understanding of the downstream effects.

---

## The three-tier priority system

Koku's `get_data_retention_months()` function
(`koku/api/settings/utils.py:297-325`) resolves the effective retention period
for each tenant using this priority chain:

```
1. Env var RETAIN_NUM_MONTHS  (only when value != code default)
2. Tenant DB row              (TenantSettings.data_retention_months)
3. Config.MASU_RETAIN_NUM_MONTHS  (= settings.RETAIN_NUM_MONTHS, which
                                    defaults to DEFAULT_RETAIN_NUM_MONTHS = 4)
```

**Critical detail:** tier 1 only fires when the env var is set to a value
that **differs** from `DEFAULT_RETAIN_NUM_MONTHS` (4).  When the env var
equals the code default, tier 1 is skipped entirely and the system falls
through to the DB row or the config default.

### Code reference (utils.py)

```python
def get_data_retention_months(schema_name: str) -> "int | None":
    env_val = os.environ.get("RETAIN_NUM_MONTHS")
    if env_val is not None:
        try:
            parsed = int(env_val)
            if parsed != DEFAULT_RETAIN_NUM_MONTHS:   # <-- key check
                return parsed                          # env wins, skip DB
        except (ValueError, TypeError):
            LOG.error(...)
            return None          # treat parse failure as "skip purge"

    # Fall through: check tenant DB row, then config default
    try:
        with schema_context(schema_name):
            row = TenantSettings.objects.first()
            if row:
                return row.data_retention_months
    except Exception:
        return None
    return Config.MASU_RETAIN_NUM_MONTHS    # == 4
```

---

## The env-override lock mechanism

The Global Settings API view (`koku/api/settings/views.py:114-173`) exposes
a `GET` endpoint that returns `env_override: true/false` and a `PUT` endpoint
that allows tenant admins to change retention.

### Lock detection (`_is_env_retention_locked`)

```python
@staticmethod
def _is_env_retention_locked():
    """Return True when RETAIN_NUM_MONTHS is set to a non-default value."""
    env_val = os.environ.get("RETAIN_NUM_MONTHS")
    if env_val is None:
        return False
    try:
        return int(env_val) != DEFAULT_RETAIN_NUM_MONTHS
    except (ValueError, TypeError):
        return True   # unparseable => locked (safe fallback)
```

### Lock effects

| Condition | `env_override` | GET returns | PUT behavior |
|-----------|---------------|-------------|--------------|
| Env var **not set** | `false` | DB row or config default (4) | Allowed (200/204) |
| Env var **== code default** (4) | `false` | DB row or config default (4) | Allowed (200/204) |
| Env var **!= code default** (e.g. 3, 24) | `true` | Env var value | **Blocked (403)** |
| Env var **unparseable** (e.g. "abc") | `true` | 503 (None from helper) | **Blocked (403)** |

When `env_override` is `true`:
- The UI disables the retention control (greys it out)
- `PUT` returns `403 Forbidden` with the message: "Data retention is controlled
  by the RETAIN_NUM_MONTHS environment variable and cannot be modified via
  the API."
- The effective value is the env var, ignoring any tenant DB row

When `env_override` is `false`:
- The UI enables the retention control
- `PUT` is allowed; the value is stored in the `TenantSettings` DB row
- The effective value comes from the DB row (if present) or the config
  default (4)

### Test evidence

These claims are verified by the test suite at
`koku/api/settings/test/test_global_settings_views.py`:

- `test_get_env_override_false_when_env_equals_default` (line 106):
  env="4", DB has 6 => `env_override=false`, returns 6
- `test_get_env_override_true_when_env_differs_from_default` (line 98):
  env="24" => `env_override=true`, returns 24
- `test_put_allowed_when_env_equals_default` (line 198):
  env="4" => PUT returns 204 (allowed)
- `test_put_blocked_when_env_differs_from_default` (line 184):
  env="24" => PUT returns 403 (blocked)

---

## Defaults across the stack

| Source | Value | Effect |
|--------|-------|--------|
| **Koku `settings.py`** (`DEFAULT_RETAIN_NUM_MONTHS`) | `4` | The code default; the "magic number" that the lock detection compares against |
| **Koku `docker-compose.yml`** | `${RETAIN_NUM_MONTHS-4}` | Matches code default (transparent) |
| **Helm chart `values.yaml`** | `"3"` | Differs from code default => **locks UI, forces 3 months** |
| **JIRA COST-7678 spec** | `3` | Copied from Helm chart |
| **Operator CRD** (`+kubebuilder:default:=4`) | `4` | Matches code default => **transparent, UI unlocked** |
| **Operator Go fallback** | `4` | Matches CRD default (for zero-value guard) |

---

## Behavioral analysis: what happens with each operator default

### Operator default = 4 (current)

The operator always emits `RETAIN_NUM_MONTHS=4` via `KokuCommonEnv`.  Since
`4 == DEFAULT_RETAIN_NUM_MONTHS`, this is **invisible** to Koku:

- `_is_env_retention_locked()` returns `false`
- `get_data_retention_months()` falls through to DB row or config default
- Tenant admins **can** configure retention via the Global Settings UI/API
- Out of the box, retention is 4 months (the code default)
- If a tenant admin sets retention to 12 via the UI, that sticks

**Consequence:** The CRD field `dataRetentionMonths` with default 4 is
effectively a no-op.  It only has an effect when the admin explicitly changes
it in the CR to a value other than 4.

### Operator default = 3 (matching Helm chart / JIRA)

If the CRD default were changed to 3, the operator would emit
`RETAIN_NUM_MONTHS=3`.  Since `3 != DEFAULT_RETAIN_NUM_MONTHS`:

- `_is_env_retention_locked()` returns `true`
- `get_data_retention_months()` returns 3 immediately (DB row ignored)
- Tenant admins **cannot** configure retention via the UI/API (403 on PUT)
- Retention is forced to 3 months for all tenants
- Changing `dataRetentionMonths` in the CR to any value other than 4
  continues to lock the UI at that value

**Consequence:** The Helm chart's choice of 3 was a deliberate policy
decision to lock retention at 3 months and remove tenant admin control.

### Operator does not set the env var (hypothetical)

If the operator only emitted `RETAIN_NUM_MONTHS` when the CR value differs
from the code default:

- `os.environ.get("RETAIN_NUM_MONTHS")` returns `None`
- Lock returns `false`; effective value comes from DB row or config default
- Tenant admins have full control via UI/API
- Setting `dataRetentionMonths: 12` in the CR would emit the env var and
  lock the UI at 12

**Consequence:** This would give tenant admins the most flexibility but
requires conditional env-var emission logic in the operator.

---

## Design considerations for the operator

### Should the operator match the Helm chart (default 3)?

Arguments for:
- Behavioral parity with the Helm-based deployment
- On-prem deployments may want a shorter retention to limit storage usage
- The JIRA spec explicitly says 3

Arguments against:
- Locks the UI for all tenants out of the box, removing admin self-service
- Differs from the application's own default (4), creating confusion
- If an admin sets `dataRetentionMonths: 4` to "unlock" the UI, the
  semantics are counterintuitive ("set to 4 to enable admin control")

### Should the operator keep default 4 (current)?

Arguments for:
- Matches the application's code default (transparent behavior)
- Preserves tenant admin self-service via UI/API
- Out-of-the-box experience matches what Koku does without any env override

Arguments against:
- Different behavior from the Helm deployment (retention is 4, not 3)
- The CRD field is effectively a no-op at its default value

### Should the operator conditionally emit the env var?

The operator could omit `RETAIN_NUM_MONTHS` from the env entirely when the
CR value equals the code default (4), and only set it when the admin
explicitly overrides:

```go
if cfg.Spec.CostManagement.DataRetentionMonths != 0 &&
   cfg.Spec.CostManagement.DataRetentionMonths != 4 {
    env = append(env, EnvVal("RETAIN_NUM_MONTHS",
        fmt.Sprintf("%d", cfg.Spec.CostManagement.DataRetentionMonths)))
}
```

This gives the cleanest semantics:
- Default: env var absent, UI unlocked, DB/config default applies
- Admin sets `dataRetentionMonths: 12`: env var present, UI locked at 12
- Admin sets `dataRetentionMonths: 4`: same as default (env var omitted)

But it couples the operator to Koku's internal `DEFAULT_RETAIN_NUM_MONTHS`
constant, creating a cross-component dependency that could break silently if
Koku changes its default.

---

## Current implementation

As of this PR, the operator:

1. Declares `DataRetentionMonths int32` with CRD default `4`, range `[1, 60]`
2. Always emits `RETAIN_NUM_MONTHS` in `KokuCommonEnv`
3. Has a Go-level zero-value fallback to `4` (matching the CRD default)

With these settings, the env var is transparent to Koku when at its default.
Tenant admins retain full control via the UI/API.  The env var only takes
effect (and locks the UI) when an admin explicitly sets `dataRetentionMonths`
to a value other than 4 in the CR.

---

## File references

| File | What |
|------|------|
| `koku/koku/settings.py:379-380` | `DEFAULT_RETAIN_NUM_MONTHS = 4` and `RETAIN_NUM_MONTHS` setting |
| `koku/api/settings/views.py:119-128` | `_is_env_retention_locked()` lock detection |
| `koku/api/settings/views.py:130-173` | GET/PUT endpoints with lock enforcement |
| `koku/api/settings/utils.py:297-325` | `get_data_retention_months()` three-tier resolution |
| `koku/api/settings/test/test_global_settings_views.py` | Test coverage for all lock/unlock scenarios |
| `koku/api/settings/test/test_global_settings_utils.py` | Test coverage for three-tier resolution |
| `api/v1alpha1/costmanagementserviceconfig_types.go:433-437` | CRD field declaration |
| `internal/resources/env.go:29-31,66` | Go-level fallback and env-var emission |
| `cost-onprem-chart/cost-onprem/values.yaml` | Helm chart default (3) |
