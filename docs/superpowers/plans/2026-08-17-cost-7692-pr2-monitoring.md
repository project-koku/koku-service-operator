# COST-7692 PR2 Implementation Plan — App /metrics scrape

> Spec: `onprem-notes/onprem-monitoring/2026-08-16-beta-monitoring-plan.md`. Builds on PR1.

**Goal:** Wire working Prometheus scrape for beta managed workloads behind the same `spec.monitoring.enabled` switch, and fix disable-path orphan cleanup.

## Done

- [x] Koku API + Masu named `metrics` Service/container ports
- [x] Ingress NetworkPolicy monitoring scrape allow
- [x] `monitoringFrom()` includes user-workload monitoring namespace
- [x] App ServiceMonitor applied for API / Masu / Ingress (no ROS/listener)
- [x] `CostManagementAPIDown` `up`-based alert
- [x] Disable deletes PrometheusRule + App ServiceMonitor
- [x] Unit tests (`go test ./internal/resources/ ./internal/controller/`)
