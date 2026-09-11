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
- **`spec.ingress.maxUploadSize`** — needs raising for large/xlarge profiles (default 100 MB)
- **HAProxy/Envoy timeout** — large/xlarge require ≥ 600s (PERF-FINDING-001)

---

## `spec.profile` in the current CRD

The operator currently defines two profile values (`standard`, `ha`) that
control UI replica counts and CPU defaults. These are **not** the same as the
chart's four sizing tiers (small / medium / large / xlarge). The chart tiers
drive Celery worker counts, ingress limits, and timeouts — fields the operator
exposes individually in the spec.

Until a richer `spec.profile` abstraction is added (see [Sizing abstraction](#sizing-abstraction)
below), callers set component fields directly in the CR.

---

## Field mapping: Helm overlay → `CostManagementServiceConfig` spec

### Replica counts

| Helm overlay path | CR path | Notes |
|-------------------|---------|-------|
| `costManagement.listener.replicas` | `spec.costManagement.listener.replicas` | |
| `costManagement.celery.workers.ocp.replicas` | `spec.costManagement.celery.workers.ocp.replicas` | |
| `costManagement.celery.workers.summary.replicas` | `spec.costManagement.celery.workers.summary.replicas` | |
| `ros.processor.replicas` | `spec.ros.processor.replicas` | |
| *(always 1)* | `spec.kruize.replicas` | Hardcoded — see PERF-FINDING-004 |

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
| `ingress.upload.maxMemory` | *(field not yet in CRD)* | Add `spec.ingress.maxMemory`; cap at 128Mi until multipart upload lands (PERF-FINDING-024) |

### Timeouts

| Helm overlay path | CR path | Notes |
|-------------------|---------|-------|
| `gatewayRoute.annotations["haproxy.router.openshift.io/timeout"]` | `spec.gatewayRoute.annotations["haproxy.router.openshift.io/timeout"]` | e.g. `"180s"` or `"600s"` |
| `jwtAuth.envoy.ingressTimeout` | *(not yet in CRD)* | Envoy route timeout — add to `spec.auth.envoy` |
| `jwtAuth.envoy.ingressPerTryTimeout` | *(not yet in CRD)* | Envoy per-try timeout — add to `spec.auth.envoy` |

> The operator injects `haproxy.router.openshift.io/timeout=180s` by default
> unless `spec.gatewayRoute.annotations` overrides it (see `GatewayRouteConfig`).
> Large/xlarge profiles need `"600s"`.

---

## Per-profile numbers (from validated Helm overlays)

These are the numeric values to set on the CR for each sizing tier.
`baseline`/`small` match chart defaults and require no overrides.

### `medium` profile

```yaml
spec:
  costManagement:
    listener:
      replicas: 2
      resources:
        requests: { memory: 1Gi }
        limits:   { memory: 2Gi }
    celery:
      workers:
        ocp:
          replicas: 2
          resources:
            requests: { cpu: 250m, memory: 512Mi }
            limits:   { cpu: 1000m, memory: 2Gi }
        summary:
          replicas: 2
          resources:
            requests: { cpu: 250m }
            limits:   { cpu: 1000m }
  ros:
    processor:
      replicas: 2
      resources:
        requests: { memory: 2Gi }
        limits:   { memory: 4Gi }
  kruize:
    replicas: 1
    resources:
      limits: { cpu: 2000m }
  ingress:
    maxUploadSize: 209715200   # 200 MB
    resources:
      requests: { memory: 1Gi }
      limits:   { memory: 2Gi }
  gatewayRoute:
    annotations:
      haproxy.router.openshift.io/timeout: "180s"
```

### `large` profile

```yaml
spec:
  costManagement:
    listener:
      replicas: 3
      resources:
        requests: { memory: 2Gi }
        limits:   { memory: 4Gi }
    celery:
      workers:
        ocp:
          replicas: 3
          resources:
            requests: { cpu: 500m, memory: 1Gi }
            limits:   { cpu: 1000m, memory: 2Gi }
        summary:
          replicas: 3
          resources:
            requests: { cpu: 500m }
            limits:   { cpu: 1000m }
  ros:
    processor:
      replicas: 3
      resources:
        requests: { memory: 2Gi }
        limits:   { memory: 4Gi }
  kruize:
    replicas: 1
    resources:
      limits: { cpu: 2000m }
  ingress:
    maxUploadSize: 524288000   # 500 MB
    resources:
      requests: { memory: 2Gi }
      limits:   { memory: 4Gi }
  gatewayRoute:
    annotations:
      haproxy.router.openshift.io/timeout: "600s"
```

### `xlarge` profile

Same replica and resource config as `large`, with raised worker CPU for
tag-based cost model processing:

```yaml
spec:
  costManagement:
    celery:
      workers:
        ocp:
          replicas: 3
          resources:
            requests: { cpu: 1000m, memory: 2Gi }
            limits:   { cpu: 2000m, memory: 4Gi }
        summary:
          replicas: 3
          resources:
            requests: { cpu: 1000m }
            limits:   { cpu: 2000m }
  # All other fields identical to large profile above
```

---

## Sizing abstraction

The chart's `apply_perf_profile_config()` in `scripts/lib/perf-testing.sh`
handles replica scaling at test time (Phase 2 — `oc scale`). Resource limits
are the operator's responsibility via the CMSC spec.

Longer term, `spec.profile` could be extended with `small | medium | large | xlarge`
values alongside the existing `standard | ha` tiers, using a mutating webhook
to expand sparse CRs into the per-component field values above. Suggested rules:

1. If `spec.profile` matches a sizing tier, apply its defaults to any **unset** nested fields.
2. Explicit component values in the CR always win (sparse override).
3. If `spec.profile` is unset, use `standard` / `small` defaults (COST-7599 baseline).
4. Reflect the effective profile in `status.appliedProfile`.

### CEL validation rules worth encoding

| Rule | Source |
|------|--------|
| `spec.kruize.replicas` ≤ 1 | PERF-FINDING-004 |
| `spec.kruize.resources.limits.cpu` ≥ 2000m when ROS enabled | Liveness under load |
| `spec.ingress.maxUploadSize` ≤ 524288000 unless large/xlarge profile | Pipeline stability |
| Large/xlarge → HAProxy timeout annotation ≥ 600s | PERF-FINDING-001 |
| Listener CPU boost optional through medium (300m sufficient) | PERF-FINDING-035 / VTC-001a |

---

## CRD gaps to close for full large/xlarge support

1. **Envoy ingress timeout** (`spec.auth.envoy.ingressTimeout`) — Envoy reads
   its timeout config at startup; operator must restart the gateway pod on change.
2. **Envoy per-try timeout** (`spec.auth.envoy.ingressPerTryTimeout`).
3. **Ingress max in-memory buffer** (`spec.ingress.maxMemory`) — cap at 128 Mi
   until multipart upload lands (PERF-FINDING-024).

---

## Related

- [sizing-guide.md](./sizing-guide.md) — validated numbers per profile
- [FINDINGS.md](./FINDINGS.md) — performance issues and root causes
- [TEST-MATRIX.md](./TEST-MATRIX.md) — complete test matrix
- `scripts/lib/perf-testing.sh` — `apply_perf_profile_config()` (Phase 2 oc scale)
- `api/v1alpha1/costmanagementserviceconfig_types.go` — authoritative CRD source
