# Helm Chart vs Operator Comparison Report

Updated: 2026-08-25 (remaining gaps only; closed items from 2026-08-21 dropped)

Systematic comparison of `cost-onprem-chart/cost-onprem/` (Helm chart) against
`koku-service-operator` (operator). Both deploy the same Cost Management
on-premise stack. This document tracks **open** deviations only.

---

## 1. Remaining resource gaps

| Component | Helm Chart | Operator | Status |
|-----------|-----------|----------|--------|
| ROS Processor Service | yes (metrics 9000) | **MISSING** | **gap** |
| ROS Recommendation Poller Service | yes (metrics 9000) | **MISSING** | **gap** |
| ROS processor-metrics NetworkPolicy | yes | **MISSING** | **gap** |
| ROS poller-metrics NetworkPolicy | yes | **MISSING** | **gap** |
| ROS API ServiceMonitor | yes | **MISSING** | **gap** |
| ROS Processor/Poller ServiceMonitors | yes | **MISSING** | **gap** (need Services first) |
| Kruize ServiceMonitor | yes (`http` / `/q/metrics`) | builder only, **not applied** | **gap** (COST-8054) |
| RBAC ServiceMonitor | yes (`http` / `/metrics`) | **MISSING** | **gap** |

ROS API already has a Service with a named `metrics` port and a NetworkPolicy
that allows gateway (8000) plus OpenShift monitoring (9000). Processor and
poller Deployments expose container port `metrics` but have no Service or
scrape NetworkPolicy.

---

## 2. Missing Resources (Gaps)

### 2.1 ROS Processor and Recommendation Poller Services

The chart creates Service objects for both with a metrics port (9000),
enabling Prometheus scraping. The operator creates Deployments but no
Services. Owned by [COST-8054](jira/COST-8054.md) (ROS, out of Cost beta).

### 2.2 Metrics-scraping NetworkPolicies

The chart's `ros/networkpolicies.yaml` has dedicated metrics NPs for ROS API,
processor, and poller. ROS API monitoring ingress is now on
`ROSAPINetworkPolicy`. Still missing:

- **processor-metrics** — no NetworkPolicy for ROS Processor
- **poller-metrics** — no NetworkPolicy for ROS Recommendation Poller

Prometheus cannot scrape those pods in a default-deny environment. Also
COST-8054.

### 2.3 ServiceMonitor gaps

Chart ServiceMonitors still unmatched:

| Target | Chart | Operator |
|--------|-------|----------|
| `rbac-api` | port `http`, `/metrics` | none. `RBACAPIService` is `http` only; `RBACAPINetworkPolicy` has no monitoring peer |
| `ros-api` | port `metrics`, `/metrics` | not in `AppServiceMonitor` (Cost-core SM is API / Masu / Ingress) |
| `ros-processor` / `ros-recommendation-poller` | port `metrics`, `/metrics` | none (no Services) |
| `kruize` / `ros-optimization` | port `http`, `/q/metrics` | builder matches chart; not applied until COST-8054. `KruizeNetworkPolicy` has no monitoring peer (chart same), so apply-only will not scrape under default-deny |

Cost-core scrape that **does** work: Koku API, Masu, Ingress (`AppServiceMonitor`
port `metrics`), Gateway (Envoy admin `/stats/prometheus`), Celery workers,
operator. Those are not listed as gaps.

`MonitoringConfig` is still only `Enabled *bool`; scrape interval is hardcoded
`30s` (chart has `monitoring.scrapeInterval`).

---

## 3. Env var differences

### 3.1 RBAC `ROLE_CREATE_ALLOW_LIST`

The chart exposes `rbac.roleCreateAllowList` (default `""`). The operator
neither exposes nor sets this.

Empty default matches SaaS: REST custom-role create is blocked unless a
caller sets `ROLE_CREATE_ALLOW_LIST=cost-management`. Unset on the operator
is equivalent to the chart default. Only needed if product wants REST-created
custom roles.

---

## 4. Remaining fixes (priority order)

1. **Add RBAC ServiceMonitor + monitoring peer** on `RBACAPINetworkPolicy` (Cost-core leftover; chart scrapes `http` `/metrics`)
2. **Add ROS Processor + Poller Services** (prerequisite for scrape; COST-8054)
3. **Add processor-metrics and poller-metrics NetworkPolicies** (COST-8054)
4. **Apply ROS API / processor / poller / Kruize ServiceMonitors** when ROS is on (COST-8054; Kruize builder already uses `http` `/q/metrics`). Applying the Kruize SM still will not scrape in a default-deny namespace: `KruizeNetworkPolicy` has no monitoring peer (the chart has the same gap).
5. **Expose `roleCreateAllowList`** in the RBAC CR section if REST custom roles are required
