# COST-7692 PR1 Implementation Plan — Operator-centric monitoring

> **For agentic workers:** Execute task-by-task. Steps use checkbox syntax. Spec: `onprem-notes/onprem-monitoring/2026-08-16-beta-monitoring-plan.md`.

**Goal:** Ship beta-useful PrometheusRules + lifecycle Events behind `spec.monitoring.enabled`, without claiming app `/metrics` scrape works.

**Architecture:** `reconcileMonitoring` applies only `PrometheusRules` (stop App/Kruize ServiceMonitors). Alerts use CMSC conditions, migration Jobs, and kube-state pod/deployment metrics for operator-managed workloads. Events cover phase and dependency transitions. App scrape deferred to PR2.

**Tech Stack:** Go, controller-runtime, unstructured `monitoring.coreos.com/v1` PrometheusRule, fake client unit tests.

## Global Constraints

- ROS/Kruize out of beta scrape/alert ownership (Job regex may still match ROS migrate if present).
- No BYOI infra scrape (Postgres/Valkey/Kafka/S3/Keycloak).
- No business metrics / Celery backlog.
- No configurable scrape interval/labels (PR2).
- Silent skip when Prometheus Operator CRDs absent (`IsNoMatchError`).
- Prefer stopping App ServiceMonitor apply (recommendation 1 from plan).

---

### Task 1: PrometheusRules — operator-centric beta set

**Files:**
- Modify: `internal/resources/monitoring.go` (`PrometheusRules`)
- Create: `internal/resources/monitoring_test.go`

**Produces:** `PrometheusRules(cfg)` with beta alert set; no scrape-`up` APIDown rule.

- [ ] **Step 1: Write tests asserting rule alert names and that APIDown is absent**

Assert present: `CostManagementMigrationFailed`, `CostManagementMigrationStalled`, `CostManagementDegraded`, `CostManagementDependencyDown`, `CostManagementPodRestarting`, `CostManagementNotAvailable` (or keep `CostManagementNotProgressing` if already used — pick one and stick to it).

Assert absent: `CostManagementAPIDown`, `CostManagementCeleryBacklog`.

- [ ] **Step 2: Implement rules**

| Alert | Expr intent | `for` |
|-------|-------------|-------|
| MigrationFailed | `kube_job_status_failed` on `{instance}-(koku\|ros\|rbac)-migrate` | 1m |
| MigrationStalled | CMSC `SchemaUpToDate` status false | 10m |
| Degraded | CMSC `Degraded` true | 5m |
| DependencyDown | CMSC `DatabaseReady` or `CacheReady` false | 5m |
| PodRestarting | `increase(kube_pod_container_status_restarts_total{namespace, pod=~"{instance}-.*"}[15m]) > 3` | 15m |
| NotAvailable | CMSC `Available` false | 30m |

Remove scrape-based `CostManagementAPIDown`. Remove or stop shipping `CostManagementSchemaOutOfDate` if replaced by MigrationStalled (avoid duplicate).

- [ ] **Step 3: Run** `go test ./internal/resources/ -run Monitoring -count=1`

---

### Task 2: reconcileMonitoring applies rules only

**Files:**
- Modify: `internal/controller/costmanagementserviceconfig_controller.go` (`reconcileMonitoring`)
- Modify: `internal/controller/monitoring_test.go`

**Produces:** Enabled path applies `PrometheusRules` only; best-effort delete of legacy App ServiceMonitor; keep `KruizeServiceMonitor` builder for `ros_cleanup.go`.

- [ ] **Step 1: Update reconcileMonitoring**

When enabled:
1. Best-effort `Delete` App ServiceMonitor (`resources.AppServiceMonitor(cfg)`), ignore NotFound / NoMatch.
2. Apply `resources.PrometheusRules(cfg)` with existing NoMatch skip / real-error surface.

Do **not** apply `AppServiceMonitor` or `KruizeServiceMonitor` here.

- [ ] **Step 2: Extend monitoring tests** — enabled path patches only PrometheusRule (interceptor counts kinds); delete attempted for App SM.

- [ ] **Step 3: Run** `go test ./internal/controller/ -run Monitoring -count=1`

---

### Task 3: Lifecycle Events

**Files:**
- Modify: `internal/controller/costmanagementserviceconfig_controller.go`
- Modify: `internal/controller/validation.go`
- Modify: `internal/controller/event_transition_test.go` (or new `events_monitoring_test.go`)

**Produces:** `PhaseChanged`, `MigrationsComplete`, `DependencyFailed` on real transitions. Skip `SecretRotated` / `DriftCorrected` (follow-ups).

- [ ] **Step 1: PhaseChanged** — after reconcile success/error, if `priorPhase != finalPhase`, emit `PhaseChanged` with message including old→new. Keep existing `Ready` event behavior.

- [ ] **Step 2: MigrationsComplete** — rename event reason from `MigrationComplete` → `MigrationsComplete` (ticket name). Keep condition reason as-is if needed for stability.

- [ ] **Step 3: DependencyFailed** — when validation sets `DatabaseReady` or `CacheReady` to False, emit Warning `DependencyFailed` only on False transition (check prior condition status).

- [ ] **Step 4: Tests** for PhaseChanged / MigrationsComplete / DependencyFailed transition guards.

- [ ] **Step 5: Run** `go test ./internal/controller/ -count=1`

---

### Task 4: Docs

**Files:**
- Modify: `onprem-notes/onprem-monitoring/2026-08-16-beta-monitoring-plan.md` (or sibling status note)
- Optional comment-ready blurb for COST-7692

- [ ] Document PR1 shipped vs PR2 deferred; note App SM no longer applied.

---

### Out of PR1

- Operator HTTPS metrics port alignment / custom `costmanagement_*` series
- App ServiceMonitor scrape wiring
- Celery backlog, DriftCorrected, SecretRotated
