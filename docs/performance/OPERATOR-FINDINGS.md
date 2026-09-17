# Operator Performance Testing Findings

Issues and sizing requirements discovered during **koku-service-operator**
performance testing (COST-8148). All findings here are specific to the
operator deployment path; chart-path findings live in
[CHART-FINDINGS.md](./CHART-FINDINGS.md).

Finding IDs continue the chart series (PERF-FINDING-NNN) in a single
namespace to avoid cross-reference confusion.

This document is a point-in-time record of what was found and how it was
addressed. Finding statuses are intentionally fixed at the time of discovery
— Jira tickets track ongoing resolution.

---

## Operator Deployment Gaps

### PERF-FINDING-039: `apply_perf_profile_config` Phase 1 Is a Stub — Resource Overrides Not Applied on Operator Path

**Status**: Open — CMSC patch automation needed
**Severity**: Medium
**Related Jira**: [COST-8154](https://redhat.atlassian.net/browse/COST-8154) (Translate chart sizing guide to operator CMSC configuration)
**Test run**: `main-small-api+rbac+ingestion-1789657257`

**Problem**:
`apply_perf_profile_config()` in `scripts/lib/perf-testing.sh` has two
phases:

- **Phase 1** — CPU/memory limits on workers, upload sizes, gateway
  timeouts. On the chart path this runs `helm upgrade --set ...`. On the
  operator path this is **explicitly stubbed** with a log message:
  _"Phase 1 skipped: resource/timeout configuration is owned by the CMSC CR
  — patch spec before running."_
- **Phase 2** — `oc scale deployment` for listener, ocp-worker, and
  summary-worker replicas. This runs on both paths.

The result is that perf test runs on the operator path execute with whatever
the CMSC and namespace LimitRange happen to provide, which for a default
community install is `resources: {}` on all celery workers. The namespace
LimitRange then applies a ceiling (typically 512Mi memory), which is lower
than the chart small-profile default of `512Mi request / 1Gi limit`.

**Evidence** (small profile, `main-small-api+rbac+ingestion-1789657257`):

| Test | Budget | Actual | Result | Notes |
|------|--------|--------|--------|-------|
| ING-003[2] concurrent | 3m00s | 5m19s | **FAIL** | 1/2 sources processed |
| ING-003[5] concurrent | 4m30s | 7m58s | PASS | 5/5 — warm pipeline |
| ING-003[10] concurrent | 7m00s | 19m33s | **FAIL** | 2/10 sources processed |
| ING-001[small] | — | 1m52s | PASS | single source, no contention |
| ING-002[30-days] | — | 3m06s | PASS | single source |
| ING-002[60-days] | — | 4m29s | PASS | single source |
| ING-004[50mb] | — | 3m49s | PASS | large file, single source |
| ING-005 high-freq | — | 16m12s | PASS | sequential uploads |
| ING-006 processing window | — | 1m13s | PASS | |

Single-source tests pass cleanly. Failures are confined to `ING-003`
concurrent uploads where multiple sources compete for the same
`celery-worker-ocp` slots. The processing timeout budget (`120 + n×30s`)
was calibrated on chart runs where Phase 1 had applied `500m CPU limit /
1Gi memory limit`; the operator default effective limit is lower, causing
CPU throttling under concurrent load.

The `celery-worker-ocp` pods were also manually patched to `limits.memory:
1Gi` (up from the LimitRange default of 512Mi) to resolve OOMKills
(see PERF-FINDING-040). Even with that patch in place, ING-003 concurrent
failures persisted — indicating the bottleneck is CPU throttling at `500m
limit`, not memory.

**What the chart small profile applies (Phase 1)**:

```
ocp_worker_cpu_req="250m"  ocp_worker_cpu_lim="500m"
ocp_worker_mem_req="512Mi" ocp_worker_mem_lim="1Gi"
```

**What the operator actually runs** (CMSC default, `resources: {}`):
- CPU: namespace LimitRange default (unset → `500m` limit via LimitRange)
- Memory: namespace LimitRange default (`512Mi` limit — no headroom buffer)
- No explicit requests set → over-commit allowed → pods scheduled tightly

**Fix required**:
1. The CMSC sample and/or the deploy script must apply explicit resource
   values matching the chart small-profile baseline before running perf
   tests.
2. `apply_perf_profile_config` Phase 1 should be implemented to patch the
   CMSC with profile-appropriate resource values, or at minimum emit a
   blocking error if the CMSC `resources` fields are empty and `--perf-only`
   is used.

**Suggested CMSC patch for small profile pre-test**:
```bash
oc patch cmsc cost-onprem -n cost-onprem --type=merge -p '{
  "spec": {
    "costManagement": {
      "celery": {
        "workers": {
          "ocp":     {"resources": {"requests": {"cpu":"250m","memory":"512Mi"}, "limits": {"cpu":"500m","memory":"1Gi"}}},
          "summary": {"resources": {"requests": {"cpu":"250m","memory":"512Mi"}, "limits": {"cpu":"500m","memory":"1Gi"}}},
          "default": {"resources": {"requests": {"cpu":"100m","memory":"256Mi"}, "limits": {"cpu":"500m","memory":"512Mi"}}}
        }
      }
    }
  }
}'
```

---

### PERF-FINDING-040: `celery-worker-ocp` OOMKilled at Default Namespace LimitRange (512Mi)

**Status**: Mitigated by cluster patch; permanent fix needed in CMSC default
**Severity**: High
**Related Jira**: [COST-8154](https://redhat.atlassian.net/browse/COST-8154)

**Problem**:
The operator's `CeleryWorkerDeployment()` builder passes `spec.Resources`
directly to the pod template with no default injection. A default community
install leaves `resources: {}` on all celery workers. The namespace
LimitRange applies a 512Mi memory ceiling with no explicit request, allowing
over-commit. The `celery-worker-ocp` pod consistently OOMKilled after ~16
seconds of task processing.

**Evidence**:
- 210 restarts over 18 hours in session preceding `1789657257` test run
- Exit Code 137 (OOMKill) on every crash
- `reporting_ocpusagelineitem_daily_summary.pod_labels` remained NULL/`{}`
  because the OCP worker never completed the summary task
- `pod_labels` column populated in raw `openshift_pod_usage_line_items_daily`
  (2,816 rows with labels) — raw ingest worked; summary step did not

**Chart default** (from `values.yaml` / PERF-FINDING-033):
`512Mi request / 1Gi limit` — workers survived at 256Mi; comfortable at 512Mi/1Gi.

**Operator default**: `resources: {}` → LimitRange caps at 512Mi with no
request → effective limit is 512Mi with no headroom buffer over the request.

**Mitigation applied**: Manual `oc patch` raised `limits.memory` to `1Gi`
on the live deployment. The operator will revert this on next reconcile.

**Permanent fix**: The operator must inject sensible default resources for
`celery-worker-ocp` when `spec.costManagement.celery.workers.ocp.resources`
is not set. Minimum recommended defaults:
```
requests: {cpu: 100m, memory: 256Mi}
limits:   {cpu: 500m, memory: 1Gi}
```

---

## Jira Tracking

| Finding | Summary | Jira | Status |
|---------|---------|------|--------|
| FINDING-039 | Phase 1 profile application stub — ING-003 concurrent failures | [COST-8154](https://redhat.atlassian.net/browse/COST-8154) | Open |
| FINDING-040 | celery-worker-ocp OOMKill at LimitRange default 512Mi | [COST-8154](https://redhat.atlassian.net/browse/COST-8154) | Mitigated (cluster patch); fix needed in operator default |

**Parent epic**: [COST-8148](https://redhat.atlassian.net/browse/COST-8148) (Operator performance testing)

---

_Last Updated: 2026-09-17_
