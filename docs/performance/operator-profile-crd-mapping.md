# Operator CRD ↔ Performance Profile Mapping

> **Status**: Active reference (COST-8147 / COST-8095). Describes how the
> validated Helm sizing profiles map to fields in the
> `CostManagementServiceConfig` CRD that this operator manages.

## Context

Performance testing on the chart (FLPATH-4036) validated four sizing tiers
against production data distributions (Pau Garcia Quiles, April 2026).
Those findings are encoded here as operator CR fields so they remain
actionable after the Helm chart is retired.

| Source | What it is |
|--------|------------|
| `cost-onprem-chart/docs/performance/` | Validated findings, sizing guide, test matrix |
| `cost-onprem-chart/cost-onprem/values-*.yaml` | Helm overlays — the numeric source of truth |
| This document | Mapping from those overlays to `CostManagementServiceConfig` spec paths |

Key constraints from performance findings that affect operator design:

- **`spec.kruize.replicas` must stay 1** — scaling Kruize degrades throughput (PERF-FINDING-004)
- **Kruize CPU limit ≥ 2000m** — required for liveness under load
- **`spec.ingress.maxUploadSize`** — needs raising for large/xlarge profiles (profile-supplied: 100 MB at `small`, no CRD default)
- **HAProxy/Envoy timeout** — large/xlarge require ≥ 600s (PERF-FINDING-001)

---

## `spec.profile` in the current CRD

`spec.profile` is a validated sizing tier: `small` (default), `medium`,
`large`, or `xlarge`, matching the chart's four tiers. It supplies defaults for
resources, replicas, ingress upload limits, Envoy ingress timeouts, and the
HAProxy route timeout. Any explicit CR field overrides the profile value, and
`status.profile` reports the tier whose sized workloads were applied for the current generation. Replica fields and
`spec.ingress.maxUploadSize` have no CRD defaults; the profile supplies them.
Authoritative behaviour and tables:
[profile-sizing-maps.md](../design/profile-sizing-maps.md).

The per-field mapping and per-tier YAML below remain as the reference for how
the Helm overlays map to CR paths.

---

## Field mapping: Helm overlay → `CostManagementServiceConfig` spec

### Replica counts

| Helm overlay path | CR path | Notes |
|-------------------|---------|-------|
| `costManagement.listener.replicas` | `spec.costManagement.listener.replicas` | |
| `costManagement.celery.workers.ocp.replicas` | `spec.costManagement.celery.workers.ocp.replicas` | |
| `costManagement.celery.workers.summary.replicas` | `spec.costManagement.celery.workers.summary.replicas` | |
| `ros.processor.replicas` | `spec.ros.processor.replicas` | |
| *(always 1)* | `spec.kruize.replicas` | Optional `*int32`, capped at 1 by CRD validation (PERF-FINDING-004) |

### Resource requests / limits

| Helm overlay path | CR path |
|-------------------|---------|
| `resources.database.*` | `spec.database.resources` |
| `costManagement.listener.resources.*` | `spec.costManagement.listener.resources` |
| `costManagement.celery.workers.ocp.resources.*` | `spec.costManagement.celery.workers.ocp.resources` |
| `costManagement.celery.workers.summary.resources.*` | `spec.costManagement.celery.workers.summary.resources` |
| `resources.rosProcessor.*` | `spec.ros.processor.resources` |
| `resources.kruize.*` | `spec.kruize.resources` |
| `resources.application.*` | `spec.ingress.resources` (PERF-FINDING-022: ingress OOM on large uploads) |

### Ingress upload limits

| Helm overlay path | CR path | Notes |
|-------------------|---------|-------|
| `ingress.upload.maxUploadSize` | `spec.ingress.maxUploadSize` | int64, bytes |
| `ingress.upload.maxMemory` | `spec.ingress.maxMemory` | Cap at 128Mi until multipart upload lands (PERF-FINDING-024) |

### Timeouts

| Helm overlay path | CR path | Notes |
|-------------------|---------|-------|
| `gatewayRoute.annotations["haproxy.router.openshift.io/timeout"]` | `spec.gatewayRoute.annotations["haproxy.router.openshift.io/timeout"]` | e.g. `"180s"` or `"600s"` |
| `jwtAuth.envoy.ingressTimeout` | `spec.auth.envoy.ingressTimeout` | Envoy route timeout |
| `jwtAuth.envoy.ingressPerTryTimeout` | `spec.auth.envoy.ingressPerTryTimeout` | Envoy per-try timeout |

> The profile supplies the HAProxy route timeout (180s at small/medium, 600s at
> large/xlarge). An explicit
> `spec.gatewayRoute.annotations["haproxy.router.openshift.io/timeout"]`
> overrides it.

---

## Per-profile numbers

The operator applies the sizing tiers via `spec.profile`; the authoritative
per-workload values are in
[profile-sizing-maps.md §5](../design/profile-sizing-maps.md). They are not
duplicated here.

To override one component, set the field explicitly. An explicit `resources`
block replaces the profile's block **whole**, so restate both CPU and memory.
Example override on top of `profile: large`: keep large's listener memory
(2Gi/4Gi) and raise listener CPU from the profile's 150m/300m to 500m/1000m for
faster bulk ingestion. Because the explicit block replaces the profile's block
whole, memory must be restated:

```yaml
spec:
  profile: large
  costManagement:
    listener:
      resources:
        requests: { cpu: 500m, memory: 2Gi }
        limits:   { cpu: 1000m, memory: 4Gi }
```

---

## Sizing abstraction

Implemented by COST-8095: `spec.profile` accepts `small | medium | large | xlarge`
and the operator resolves the effective values in the workload builders at render time (see
[profile-sizing-maps.md](../design/profile-sizing-maps.md)). Rules:

1. A profile supplies defaults for any **unset** nested field.
2. Explicit component values in the CR always win: explicit scalar fields (for
   example `replicas`) override individually, while an explicit `resources`
   block replaces the profile's resources block whole (no per-key merge).
3. If `spec.profile` is unset, `small` applies (COST-7599 baseline).
4. The tier whose sized workloads were applied is reported in `status.profile`
   (set once they are applied, not when every readiness gate passes).

The perf harness (`scripts/lib/perf-testing.sh`) now patches `spec.profile`
rather than using `oc scale`. Envoy's HTTP connection manager matches the chart
(no `request_timeout`, `idle_timeout` 300s, `max_connection_duration` 600s).

### CEL validation rules worth encoding

| Rule | Source |
|------|--------|
| `spec.kruize.replicas` ≤ 1 | PERF-FINDING-004 |
| `spec.kruize.resources.limits.cpu` ≥ 2000m when ROS enabled | Liveness under load |
| `spec.ingress.maxUploadSize` ≤ 524288000 unless large/xlarge profile | Pipeline stability |
| Large/xlarge → HAProxy timeout annotation ≥ 600s | PERF-FINDING-001 |
| Listener CPU boost optional through medium (300m sufficient) | PERF-FINDING-035 / VTC-001a |

---

## CRD gaps

The previously missing fields now exist: `spec.auth.envoy.ingressTimeout`,
`spec.auth.envoy.ingressPerTryTimeout`, and `spec.ingress.maxMemory`. The
remaining CEL-table suggestions above (Kruize CPU >= 2000m when overridden,
upload-size caps, timeout floors) are deliberately not encoded: explicit
overrides are the customer's call and the profile already sets those values
correctly (see [design §4.5](../design/profile-sizing-maps.md)).

---

## Related

- [sizing-guide.md](./sizing-guide.md) — validated numbers per profile
- [CHART-FINDINGS.md](./CHART-FINDINGS.md) — performance issues and root causes
- [TEST-MATRIX.md](./TEST-MATRIX.md) — complete test matrix
- `scripts/lib/perf-testing.sh` — `apply_perf_profile_config()` (patches `spec.profile`)
- `api/v1alpha1/costmanagementserviceconfig_types.go` — authoritative CRD source
