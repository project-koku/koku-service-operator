# IQE Plugin Migration Plan — Performance Tests

**Status:** Proposal for team review — no action taken  
**Related:** COST-8147 (operator performance test port)  
**Date:** 2026-09-10

---

## Background

The operator performance test suite lives in
`test/pytest/suites/performance/` (61 tests, 21 modules).  It was ported
from `cost-onprem-chart` as part of COST-8147 and is runnable today via
`hack/deploy-test-operator.sh --perf-only` or `scripts/run-pytest.sh`.

This document captures a plan and open questions for the team to evaluate
whether these tests should be moved into an IQE plugin, and if so, which
shape makes sense.

There are some questions that are more generic around where performance tests should be ran. 
The inclusion of soak tests in any broader solution is also a key point to consider; 
we could separate it out to reduce friction, as it's a test that we would run infrequently.

---

## What IQE would give us

| Capability | Current (operator repo) | IQE equivalent |
|---|---|---|
| Config management | `NAMESPACE`, `PERF_PROFILE` env vars | Dynaconf under `cost_management.on_prem.*` |
| ReportPortal integration | Manual DataRouter Jenkins step | Built-in via `iqe-report-portal-plugin` |
| OpenShift cluster client | `run_oc_command()` + `exec_in_pod()` | IQE `cluster` fixture (Python k8s client) |
| Auth / identity headers | Custom `create_rh_identity_header()` | IQE `application` user / auth |
| CI job wiring | Custom Jenkins stage in `insights_onprem.groovy` | Standard IQE Jenkins job |
| Result artifact upload | Custom S3 + `perf-runs/*.json` | ReportPortal artifacts |
| Package distribution | Git submodule / clone | `pip install iqe-cost-management-performance-plugin` |

---

## What doesn't map cleanly to IQE

1. **Multi-hour soak tests.**  
   IQE sessions are designed for minute-scale runs with ReportPortal streaming.
   SOAK-001 runs 1–24 hours.  IQE's `--timeout` and xdist don't handle this
   well without custom session management.

2. **Heavy custom infrastructure.**  
   `PerfCleanupTracker`, `PerfResultCollector`, `PerfTimer`, `soak-loop.sh`,
   Prometheus metrics collection — all self-contained plumbing that would
   either be refactored into IQE fixtures or carried in wholesale.

3. **Operator-only execution context.**  
   All tests require in-cluster pod exec (`koku_api_pod`, `ingress_pod`,
   `exec_in_pod`).  The existing `iqe-cost-management-plugin` is primarily
   SaaS-targeted; the on-prem path is secondary.

4. **Run-level profile/suite parameterization.**  
   `PERF_PROFILE=medium --perf-suite stress` selects a whole workload
   dimension before any test runs.  IQE's `parametrize` operates per-test.

5. **No existing IQE performance plugin precedent.**  
   `iqe-core` contains `iqe/utils/nexus_performance.py` — a utility that
   measures HTTP latency to Nexus PyPI indexes using the same `@dataclass`
   + percentile patterns.  That is the closest analogue in the ecosystem.
   No dedicated performance-testing IQE plugin exists; Option B would be the
   first.  The team should factor in that there is no internal template to
   copy and conventions will need to be established from scratch.

---

## Option A: Add `tests/operator/performance/` to the existing plugin

### Structure

```
iqe_cost_management/
  fixtures/
    performance/           ← data_classes, helpers, tracker, profiles, queue_helpers
  tests/
    operator/
      performance/         ← all 21 test modules, relocated
  conf/
    cost_management.default.yaml  ← add on_prem.perf_profile, on_prem.namespace, etc.
```

### Key adaptations

| Our fixture | IQE equivalent |
|---|---|
| `ClusterConfig` | Thin wrapper around IQE `cluster` fixture + dynaconf keys |
| `rh_identity_header` | Derived from `application.user.auth` |
| `koku_api_url` | `application.cost_management.config.on_prem.api_url` |
| Markers | `@pytest.mark.cost_management_performance`, `cost_management_performance_soak` |

### Pros
- Single package to maintain
- Shared fixtures with existing operator tests (`tests/operator/`)
- IQE CI already wired — no new job needed
- `statsmodels` is already a dependency of the existing plugin

### Cons
- Soak / stress test duration doesn't fit IQE's execution model
- Main plugin becomes on-prem-heavy alongside its SaaS tests
- `tests/operator/performance/` further crowds an already broad `operator/` namespace

---

## Option B: New `iqe-cost-management-performance-plugin`

### Structure

```
iqe-cost-management-performance-plugin/
  iqe_cost_management_performance/
    __init__.py            ← ApplicationCostManagementPerformance plugin class
    fixtures/              ← data_classes, helpers, tracker, profiles, queue_helpers
    tests/
      test_api_latency.py
      test_ingestion.py
      test_soak.py
      ...21 modules total
    conf/
      cost_management_performance.default.yaml
```

### Dependencies (`pyproject.toml`)

```toml
[project]
name = "iqe-cost-management-performance-plugin"
dependencies = [
    "iqe-cost-management-plugin",   # shared cluster/app/auth fixtures
    "statsmodels",                  # percentile analysis (already in parent plugin)
    "kubernetes",
]
```

### Pros
- Clean separation: perf tests evolve on their own cadence
- On-prem operator deployment scope is isolated from SaaS plugin
- Soak duration handled with its own timeout configuration
- No bloat in the main plugin

### Cons
- Another package to create, version, and maintain
- New Jenkins job needed
- Adds coordination overhead for the team

---

## Comparison

| Question | Option A | Option B |
|---|---|---|
| Single package to maintain? | ✅ | ❌ |
| Clean separation of concerns? | ❌ | ✅ |
| Soak test duration fit? | ❌ Poor | ⚠️ Manageable with dedicated config |
| Reuse existing CI wiring? | ✅ Immediate | ⚠️ New job needed |
| On-prem-only isolation? | ❌ Mixed with SaaS plugin | ✅ |
| Team adoption friction? | Low | Medium |

---

## Where changes would need to be made (Option B)

This is the concrete list of things that need to exist or be touched to
ship a standalone `iqe-cost-management-performance-plugin`.

### 1. New repository

Create `iqe-cost-management-performance-plugin/` (likely under
`gitlab.cee.redhat.com/insights-qe/` alongside the existing plugin).

Files to create from scratch:

| File | What goes there |
|---|---|
| `pyproject.toml` | Package metadata, entry points, deps (`iqe-cost-management-plugin`, `statsmodels`, `kubernetes`) |
| `iqe_cost_management_performance/__init__.py` | `ApplicationCostManagementPerformance` plugin class |
| `iqe_cost_management_performance/conftest.py` | Top-level IQE fixtures (`cluster_config`, `koku_api_pod`, `rh_identity_header`, `koku_api_url`) |
| `iqe_cost_management_performance/conf/cost_management_performance.default.yaml` | Dynaconf config: namespace, profile, suite, soak duration, Prometheus, S3 |
| `iqe_cost_management_performance/fixtures/` | Move `data_classes.py`, `helpers.py`, `queue_helpers.py`, `profiles.py`, `tracker.py` from operator repo |
| `iqe_cost_management_performance/tests/` | Move all 21 test modules + `conftest.py` from `test/pytest/suites/performance/` |
| `.gitlab-ci.yml` | Basic lint + unit CI (mirror existing plugin's CI config) |
| `README.md` | Installation, configuration, usage |

### 2. Changes to `iqe-cost-management-plugin`

No structural changes required.  The performance plugin depends on it as a
library.  The only optional addition is a `perf` extras group in the
existing plugin's `pyproject.toml` that pulls in the new package, to make
the relationship discoverable:

```toml
# iqe-cost-management-plugin/pyproject.toml
[project.optional-dependencies]
perf = ["iqe-cost-management-performance-plugin"]
```

### 3. Changes to `iqe-core`

None required.  The plugin uses IQE core via the existing
`iqe-cost-management-plugin` dependency chain.  The `nexus_performance.py`
utility is not a template to copy — it is unrelated tooling.

### 4. Jenkins / CI

| Job / file | Change |
|---|---|
| `insights_onprem.groovy` | Replace the current `hack/deploy-test-operator.sh --perf-only` stage with an IQE job invocation (`iqe-cost-management-performance-plugin`, marker `cost_management_performance`) |
| IQE Jenkins job template | New job entry in the `insights-qe` Jenkins folder pointing at the new plugin and the on-prem cluster config |
| `1.7_plugins_under_test.txt` (workspace) | Add `iqe-cost-management-performance-plugin` to the list |

### 5. Operator repo clean-up (post-migration)

Once tests are live in the plugin:

- Remove `test/pytest/suites/performance/` (or keep as a dev/smoke alias)
- Remove `--perf-only`, `--perf-suite`, `--perf-profile` flags from
  `hack/deploy-test-operator.sh` and `hack/lib/deploy-test-operator.bash`
- Update `docs/performance/README.md` to point at the new plugin repo

---

## Open questions for team review

1. **Option A or B?**  
   The soak test duration mismatch is the strongest argument for Option B.
   If soak tests are always run as a separate job anyway, Option A may be
   sufficient.

2. **exec_in_pod vs. Python k8s client?**  
   All tests call the Koku internal API by exec'ing `curl` inside the
   koku-api pod (workaround for the operator NetworkPolicy).  IQE's
   `cluster` fixture uses the Python `kubernetes` client directly — which
   has the same NetworkPolicy constraint.  Should we keep `exec_in_pod` as-is
   or switch to the k8s client?

3. **S3 result upload or ReportPortal?**  
   Current path writes `perf-runs/*.json` and optionally uploads to S3.
   Moving to IQE means results go to ReportPortal via DataRouter.  Are the
   two artifact formats compatible, or does the team need both?

4. **Soak tests in ReportPortal?**  
   Multi-hour tests require a long-lived IQE session.  Is the IQE
   infrastructure (agent timeouts, DataRouter) ready for 24-hour runs?

5. **Jenkins job ownership?**  
   The current pipeline is in `flight-path-auto-tests/jenkins/`.  If tests
   move to IQE, which team owns the Jenkins job and which job template is
   used?

---

## Current state (no migration)

The suite is fully functional today via the operator repo:

```bash
# Full perf run via deploy-test-operator.sh
hack/deploy-test-operator.sh \
    --perf-only \
    --perf-profile medium \
    --perf-suite all

# Or directly via run-pytest.sh
NAMESPACE=cost-onprem scripts/run-pytest.sh --perf-ingestion --perf-api
```

Jenkins integration is in place via `insights_onprem.groovy` (updated as
part of COST-8147).  Migration to IQE is a follow-on effort — the team
should evaluate after the initial operator perf baseline is established.
