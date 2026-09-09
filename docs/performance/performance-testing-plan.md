# Cost On-Prem Performance Testing Plan

**Date**: 2026-04-15 (updated 2026-08-11)  
**Status**: Execution — Small through XLarge profiles validated; soak tests hardened and validated  
**Epic**: [FLPATH-4036](https://redhat.atlassian.net/browse/FLPATH-4036) / [COST-7567](https://redhat.atlassian.net/browse/COST-7567) - CoP - Performance Tuning & Hardware Sizing Guidelines

---

## Ownership & Collaboration

| Area | Owner | Notes |
|------|-------|-------|
| **Performance testing** | Thomas Stetson | Test execution, automation, analysis |
| **Dev guidance & infrastructure** | Moti Asayag | Primary dev contact, epic author |
| **Customer size profiles** | Pau Garcia Quiles | ✅ Delivered - production data analysis |

---

## Success Criteria (from FLPATH-4036)

| ID | Criteria | Description |
|----|----------|-------------|
| SC-1 | Sizing table | Published sizing table mapping cluster profiles to resource requirements |
| SC-2 | Cluster count limits | Documented maximum supported cluster count per deployment size (S/M/L/XL) |
| SC-3 | Bottleneck analysis | Identified top-3 bottlenecks with measured impact and mitigation options |
| SC-4 | Processing window | Validated that recommended configurations sustain daily processing within 6-hour window |
| SC-5 | Soak test | 7-day stability test without OOM, disk exhaustion, or queue starvation |

---

## Executive Summary

This document outlines the performance testing strategy for Cost Management On-Premise deployments. Performance testing will validate:

1. **Ingestion throughput** - How fast can data be uploaded and processed?
2. **Scale limits** - How many clusters/sources can be managed?
3. **Resource efficiency** - Are resource allocations appropriate?
4. **Latency** - API response times under various loads

---

## Architecture Overview (Performance-Critical View)

From FLPATH-4036:

```
Cost Management On-Premise processes OpenShift cluster cost and resource 
optimization data through a multi-stage pipeline:

  Ingestion (insights-ingress-go)
      ↓
  Cost Processing (koku/MASU + Celery workers)
      ↓
  Resource Optimization (ros-ocp-backend + Kruize)
      ↓
  Storage (PostgreSQL + Valkey)
```

Current Helm chart deploys all components with **conservative defaults** (single replicas, low resource limits) without validated sizing guidance.

---

## Known Bottlenecks

### From Existing Documentation

| Component | Role | Constraint | Reference |
|-----------|------|------------|-----------|
| **Koku Listener** | Processes uploaded CSV files | Single-threaded, serial Kafka consumer | `iqe-testing-setup.md` |
| **Celery Workers** | Background summarization tasks | Queue-based, depends on worker count | `resource-requirements.md` |
| **PostgreSQL** | Cost data storage & queries | Query complexity, connection pooling | `resource-requirements.md` |
| **Kafka** | Message queue for uploads | Topic partitions, broker count | `deploy-kafka.sh` |
| **Ingress** | File upload API | Upload size limits (100MB default) | `configuration.md` |

### Measured Performance (from test runs)

```
Listener Throughput:
- Processes files serially via single Kafka consumer
- CPU-bound (parquet conversion, SQL insertion)
- Default CPU limit throttles processing
- Boosting to 4 cores = ~40-50% faster processing

Source Processing Time:
- Small static source (1 month, no GPU): 30-60s
- Dynamic daily source (3 months, 6 CSVs/month): 2-5 min
```

---

## Scenario Definitions (from FLPATH-4036)

### Workload Density

OCP uploads cost data daily (24h reporting window, typically 288 intervals at 5-min granularity per pod).

### Row Count Formula

```
daily_rows = pods × 288 intervals/day × (pod_usage + storage_usage factor ~1.0)
monthly_rows = daily_rows × 30
upload_size = ~43 bytes/CSV row compressed at ~10:1 ratio
```

### Cluster Profiles (from Pau Garcia Quiles - Production Data April 2026)

Based on production metrics snapshot covering 417 active OCP accounts.
Resource sizing per profile is maintained in [sizing-guide.md](sizing-guide.md).

| Profile | % of Customers | Clusters | Nodes | CPU Cores | Memory | PVCs | Cost Models |
|---------|---------------|----------|-------|-----------|--------|------|-------------|
| **Small** | 37% | 1 | 15 | 200 | 1.1 TB | 48 | 1 (CPU dist) |
| **Medium** | 35% | 2 | 49 | 544 | 2.8 TB | 177 | 1 (CPU dist) |
| **Large** | 21% | 7 | 133 | 1,964 | 9.7 TB | 492 | 1-2 (CPU dist) |
| **Extra-Large** | 6% | 23 | 346 | 6,954 | 48.5 TB | 1,255 | 1-3 (CPU dist + tag rates) |

**Key Production Insights**:
- 39% of customers have single cluster, 72% have ≤4 clusters
- Control plane: universally 3 nodes per cluster
- Typical node: 16 cores / 63 GB RAM (median)
- 71% of accounts use cost models; adoption increases with size (100% at XL)
- CPU distribution is dominant cost model feature (58%); tag rates rare (9.4%)

**Stress/Edge-Case Values (P99/Max)**:
| Metric | P99 | Max |
|--------|-----|-----|
| Clusters | 33 | 67 |
| Nodes | 1,072 | 4,311 |
| CPU Cores | 57,424 | 793,424 |
| PVCs | 6,099 | 32,443 |
| Cost Models | 7 | 12 |

### Multi-Cluster Scenarios

Each cluster gets a unique `cluster_id` and `source_id`. Data generation produces separate payloads per cluster.

| Scenario | Clusters | Profile per Cluster | Total Pods | Notes |
|----------|----------|---------------------|------------|-------|
| Single Small | 1 | Small | TBD | Baseline |
| Multi Small | 5 | Small | TBD | Small fleet |
| Single Large | 1 | Large | TBD | Enterprise single |
| Multi Mixed | 10 | Mixed | TBD | Realistic fleet |

---

## Data Generation Design (from FLPATH-4036)

### Tooling: NISE (koku-nise)

NISE is the existing data generation tool used by E2E tests. It generates proper OCP cost CSVs with `manifest.json`.

### QE Approach: Leverage Existing Infrastructure

> **Note**: PR #144 proposes standalone bash scripts (`generate-test-data.sh`, `upload-test-data.sh`). From a QE/test automation perspective, we should leverage the **existing pytest/NISE integration** rather than creating parallel tooling:

| Existing Infrastructure | Location | Use For Performance Tests |
|------------------------|----------|---------------------------|
| `NISEConfig` class | `tests/e2e_helpers.py` | Configure scenario parameters |
| `generate_nise_data()` | `tests/e2e_helpers.py` | Generate test data |
| `upload_with_retry()` | `tests/e2e_helpers.py` | Upload to ingress |
| `register_source()` | `tests/e2e_helpers.py` | Source registration |
| `create_upload_package_from_files()` | `tests/utils.py` | Package payloads |
| NISE templates | `tests/data/nise_templates/` | Scenario definitions |

**Recommended approach**:
1. Create scenario YAML files for S1-S11, M1-M6 profiles
2. Use pytest fixtures to generate and upload data
3. Use `@pytest.mark.performance` marker for performance tests
4. Integrate with existing CI/reporting infrastructure

This ensures:
- Consistent tooling across functional and performance tests
- JWT auth handled by existing fixtures
- JUnit XML output for CI integration
- No duplication of NISE/upload logic

### Static Report Configuration

NISE static report YAML defines workloads. Scenario profiles (S1-S11) are being generated as part of PR #144 under `scripts/perf/scenarios/`.

### Upload Frequency Simulation

| Pattern | Description | Use Case |
|---------|-------------|----------|
| Real-world | Every 6 hours (4x/day) | Baseline validation |
| Accelerated | Every 1 minute | Simulate backlog processing |
| Spike | 24 uploads simultaneously | Simulate 6 days of backlog |

---

## Test Categories

### 1. Ingestion Throughput Tests

**Goal**: Measure data ingestion capacity under various loads.

| ID | Test Case | Description | Metrics |
|----|-----------|-------------|---------|
| PERF-ING-001 | Single source baseline | 1 source, 1 month data, default config | Time to complete, listener CPU% |
| PERF-ING-002 | Single source burst | 1 source, 3 months data (90 days), max listener CPU | Time to complete, throughput MB/s |
| PERF-ING-003 | Concurrent uploads | N sources uploading simultaneously | Queue depth, time to complete all |
| PERF-ING-004 | Large file upload | Single 50MB+ payload | Upload time, processing time |
| PERF-ING-005 | High frequency uploads | Upload every 5 min for 1 hour | Message queue lag, error rate |

**Variables**:
- Listener CPU allocation: 150m (default) vs 1000m vs 4000m (max)
- Data volume: 1 month vs 3 months (90 days - current test boundary)
- Concurrent sources: 1, 5, 10, 20

### 2. Multi-Cluster Scale Tests

**Goal**: Determine limits for number of managed OCP clusters/sources.

| ID | Test Case | Description | Metrics |
|----|-----------|-------------|---------|
| PERF-SCALE-001 | Source count baseline | 5 sources, steady state | Memory usage, API latency |
| PERF-SCALE-002 | Source count ramp | Add sources until degradation | Max sources, breaking point |
| PERF-SCALE-003 | Large source dataset | 1 source, 100+ namespaces | Query time, memory pressure |
| PERF-SCALE-004 | Concurrent API queries | N parallel report requests | P50/P95/P99 latency |
| PERF-SCALE-005 | Historical data depth | 3+ months data, various query ranges | Query time vs date range |

**Variables**:
- Number of sources: 1, 5, 10, 25, 50, 100
- Namespaces per cluster: 10, 50, 100, 500
- Pods per namespace: 10, 50, 100
- Data retention period: 1, 3 months (90 days is current test boundary; retention policy TBD)

### 3. API Latency Tests

**Goal**: Measure API response times under load.

| ID | Test Case | Description | Metrics |
|----|-----------|-------------|---------|
| PERF-API-001 | Report API baseline | Single report query, no load | Response time |
| PERF-API-002 | Report API under load | 10 concurrent report queries | P50/P95/P99 |
| PERF-API-003 | Cost model CRUD | Create/read/update/delete cycle | Operations/sec |
| PERF-API-004 | Source list pagination | List 100+ sources with pagination | Time per page |
| PERF-API-005 | Complex group-by query | Multi-dimension grouping | Query time |
| PERF-API-006 | Tag filtering | Filter by N tags | Query time vs tag count |

### 4. ROS/Kruize Performance Tests

**Goal**: Validate resource optimization recommendation pipeline.

| ID | Test Case | Description | Metrics |
|----|-----------|-------------|---------|
| PERF-ROS-001 | Recommendation baseline | Single workload, 15 min data | Time to recommendation |
| PERF-ROS-002 | Multi-workload scale | 50 workloads concurrently | Memory usage, queue depth |
| PERF-ROS-003 | Recommendation refresh | Update existing recommendations | Refresh time |
| PERF-ROS-004 | Kruize memory pressure | High workload count | Kruize heap usage |

### 5. Soak Testing (SC-5)

**Goal**: Validate 7-day stability.

| ID | Test Case | Description | Success Criteria |
|----|-----------|-------------|------------------|
| PERF-SOAK-001 | 7-day continuous operation | Normal upload pattern, query load | No OOM |
| PERF-SOAK-002 | Memory leak detection | Monitor memory growth over time | < 5% growth/day |
| PERF-SOAK-003 | Disk usage | Monitor PostgreSQL, Kafka storage | No exhaustion |
| PERF-SOAK-004 | Queue health | Monitor Celery/Kafka queue depths | No starvation |

---

## Metrics Collection (from FLPATH-4036)

### Observability Stack Requirements

- **Prometheus** with 15-second scrape interval, 30-day retention
- **Grafana** with pre-built dashboards per component

### Metrics by Layer

| Layer | Metrics |
|-------|---------|
| **Ingress** | Upload count, size, latency, error rate |
| **Kafka** | Consumer lag, message throughput, partition health |
| **Listener** | Files processed/min, CPU utilization, processing time |
| **Celery** | Queue lengths, task completion times, worker utilization |
| **PostgreSQL** | Connections, query time, disk usage, cache hit ratio |
| **Kruize** | Heap usage, recommendation latency, experiment count |
| **API** | Request count, latency histogram (P50/P95/P99), error rate |

### Prometheus Queries

```promql
# Listener CPU usage
rate(container_cpu_usage_seconds_total{pod=~".*listener.*"}[5m])

# Celery queue length
celery_queue_length{queue="ocp"}

# API request latency
histogram_quantile(0.95, rate(http_request_duration_seconds_bucket{job="koku-api"}[5m]))
```

---

## Testing Phases (from FLPATH-4036)

### Phase 1: Baseline Establishment

1. Deploy cost-onprem with default configuration
2. Run single-source ingestion test (PERF-ING-001)
3. Run API latency baseline (PERF-API-001)
4. Capture resource utilization baseline

### Phase 2: Optimization Testing

1. Test listener CPU boost impact (PERF-ING-002)
2. Establish optimal listener CPU setting
3. Document throughput improvements

### Phase 3: Scale Testing

1. Incrementally add sources (PERF-SCALE-001, 002)
2. Test with large datasets (PERF-SCALE-003)
3. Identify breaking points and limits

### Phase 4: Load Testing

1. Concurrent API load (PERF-API-002, 003)
2. Sustained ingestion load (PERF-ING-005)
3. Combined load scenarios

### Phase 5: Soak Testing (SC-5)

1. 7-day continuous operation test
2. Monitor for OOM, disk exhaustion, queue starvation
3. Document steady-state resource usage

---

## Test Infrastructure Requirements

### Cluster Sizing for Performance Tests

| Tier | Workers | CPU/Worker | Memory/Worker | Storage | Use Case |
|------|---------|------------|---------------|---------|----------|
| Small | 3 | 4 cores | 16 Gi | 200 Gi | Baseline tests |
| Medium | 5 | 8 cores | 32 Gi | 500 Gi | Scale tests |
| Large | 7+ | 16 cores | 64 Gi | 1 Ti | Limit testing |

### Storage Requirements

- **ODF recommended** for shared storage (S3, PVCs)
- **Minimum**: 500 Gi for extended scale tests
- **Network**: 10 Gbps between workers for Kafka/DB traffic

### Resource Baseline (from `resource-requirements.md`)

| Configuration | Pods | CPU Request | Memory Request |
|---------------|------|-------------|----------------|
| OCP-only (default) | 27 | ~8.4 cores | ~19.4 Gi |
| High Availability | 30+ | ~12+ cores | ~24+ Gi |

---

## Deliverables

### Documentation (SC-1, SC-2, SC-3)

- [ ] Sizing table (cluster profiles → resource requirements)
- [ ] Max cluster count per deployment size
- [ ] Top-3 bottlenecks with mitigations
- [ ] Tuning recommendations

### Automation

- [ ] Performance test suite (pytest markers: `@pytest.mark.performance`)
- [ ] NISE data generation scripts per scenario
- [ ] Metrics collection automation
- [ ] Grafana dashboards

### Validation (SC-4, SC-5)

- [ ] 6-hour processing window validation
- [ ] 7-day soak test results

---

## Dependencies

### Pending Information

| Item | Status | Notes |
|------|--------|-------|
| **Customer size profiles** | ✅ Complete | See Section 3 - profiles from Pau's production analysis |
| **Data retention policy** | TBD | Separate investigation needed |
| Latency SLOs | TBD | Acceptable response times |
| Rate counts per cost model | Unknown | Not in production data (requires DB query) |

---

## Related Documentation

### In Repository

- `docs/operations/resource-requirements.md` - Resource allocations
- `docs/development/iqe-testing-setup.md` - Test performance analysis
- `.cursor/prompts/analyze-test-run.md` - Test run analysis guide
- `tests/suites/e2e/README.md` - E2E test scenarios
- `tests/e2e_helpers.py` - NISE integration (NISEConfig, generate_nise_data, upload_with_retry)
- `tests/utils.py` - Upload package creation utilities

### External

- [FLPATH-4036](https://redhat.atlassian.net/browse/FLPATH-4036) - Epic with full details
- [PR #144](https://github.com/insights-onprem/cost-onprem-chart/pull/144) - Performance tuning plan and handover doc
- [IQE Cost Management Plugin](https://gitlab.cee.redhat.com/insights-qe/iqe-cost-management-plugin) - SaaS test suite
- [Koku Repository](https://github.com/project-koku/koku) - Backend source

---

## Open Questions

1. **What is the data retention policy for on-prem?**
   - Current test boundary: 90 days
   - Production retention requirements TBD
   - Affects storage sizing and query performance

2. **What are acceptable API latencies?**
   - Need to define P50/P95/P99 targets

3. **Should we test disconnected/air-gapped scenarios?**
   - Different performance characteristics
   - No external S3, local storage only

4. **What monitoring/observability is required in production?**
   - Prometheus/Grafana integration
   - Performance dashboards

---

## Implementation Status

### Completed

- [x] Customer size profiles received from Pau Garcia Quiles
- [x] Performance test marker registered (`@pytest.mark.performance`)
- [x] Profile definitions created (`tests/suites/performance/profiles.py`)
- [x] Test fixtures implemented (`tests/suites/performance/conftest.py`)
- [x] Ingestion throughput tests (PERF-ING-001 through PERF-ING-006)
- [x] API latency tests (PERF-API-001 through PERF-API-006)
- [x] Multi-cluster scale tests (PERF-SCALE-001 through PERF-SCALE-005)
- [x] ROS/Kruize performance tests (PERF-ROS-001 through PERF-ROS-004)
- [x] Soak tests validated (PERF-SOAK-001 through SOAK-004, COST-7634) — condensed mode, JWT refresh, shared data collection
- [x] Observability stack (FLPATH-4061 / COST-7625) — metrics collection, HTML reports, S3 archival
- [x] Profile-aware resource tuning (`apply_perf_profile_config()` in `perf-testing.sh`)
- [x] Jenkins CI integration (`insights_onprem.groovy` with `PERF_PROFILE` and `PERF_SUITE` params)
- [x] Self-contained HTML run reports and JSON summaries
- [x] S3 result archival to shared S3-compatible storage
- [x] Stress ramp-to-failure + backlog recovery (`test_stress.py`, COST-7627 + COST-7600)
- [x] RBAC authorization performance (`test_rbac_perf.py`, COST-7643) — 6 tests validated at medium + large

### Stress Test Environment Variables

| Variable | Default | Description |
|----------|---------|-------------|
| `STRESS_RAMP_STEPS` | `5,10,20,30,50,75,100` | Comma-separated source counts per ramp step |
| `STRESS_MAX_STEP_TIME` | `1800` | Absolute max seconds per step before declaring failure |
| `STRESS_STEP_TIMEOUT_BASE` | `120` | Base timeout per step (seconds) |
| `STRESS_STEP_TIMEOUT_PER_SOURCE` | `60` | Additional seconds per source |
| `STRESS_RECOVERY_SOURCE_COUNT` | `0` | Sources for recovery test; 0 = auto from STR-001 |
| `STRESS_RECOVERY_DURATION_S` | `300` | Overload duration for STR-002 (seconds) |
| `PERF_CLEANUP_TIMEOUT` | `900` | Max seconds for post-test resource cleanup (prevents hangs) |
| `METRICS_MAX_DURATION` | `14400` | Max seconds for metrics collector (auto-set from `JOB_TIMEOUT_HOURS` in Jenkins) |

### Soak Test Environment Variables

| Variable | Default | Description |
|----------|---------|-------------|
| `SOAK_TESTS` | `false` | Set to `true` to enable soak tests (opt-in) |
| `SOAK_CONDENSED` | `false` | Set to `true` for compressed intervals (~15 min cycle) |
| `SOAK_DURATION_HOURS` | `1` (or `0.25` if condensed) | Test duration in hours |
| `SOAK_UPLOAD_INTERVAL_MINUTES` | `15` (or `1` if condensed) | Interval between uploads |
| `SOAK_QUERY_INTERVAL_MINUTES` | `5` (or `1` if condensed) | Interval between API queries |
| `SOAK_METRICS_INTERVAL_SECONDS` | `60` (or `15` if condensed) | Metrics collection interval |
| `SOAK_CHECKPOINT_HOURS` | `6` | How often to publish state snapshot to S3 (0 = disabled) |

### 7-Day Soak: Operational Guide

The 1-hour soak validates test logic; a 7-day run validates production stability
(SC-5). A 7-day pytest process **cannot run from a local machine** — VPN drops,
SSH timeouts, and laptop sleeps will kill it.

#### Industry Patterns (Red Hat / OpenShift)

Research across Red Hat projects shows three patterns for long-running stability
testing. **None use sidecars or in-cluster Jobs for soak tests.**

| Pattern | Used by | How it works |
|---------|---------|--------------|
| **Periodic CI trigger** | Karpenter, K8s upstream, OpenShift TRT | Short test runs on a schedule against a long-lived cluster. Stability measured by trends across runs, not within a single run. |
| **Loop script on stable host** | OLM v1 (`run_e2e.py`) | Python script loops for N days, running the test binary every M seconds from a bastion/hypervisor. Publishes results to Slack/Google Sheets/S3. |
| **In-cluster Job** | Rare for soak; mostly batch workloads | Single Kubernetes Job running pytest. Fragile — crash at day 5 loses state; log rotation limits visibility. |

The **loop script on stable host** pattern is the best fit for us. Our hypervisors
are always on, have `kubeconfig` access, and aren't affected by VPN. The test
logic (analysis, assertions, JWT refresh) is already in the pytest tests. We
just need a thin outer loop and S3 checkpoint persistence.

#### Architecture

```
┌──────────────────────────────────────────────┐
│  Hypervisor (kni@<host>)                     │
│  soak-loop.sh --background (nohup-based)     │
│                                              │
│    for each iteration (every 1h):            │
│      1. run-pytest.sh --perf-soak            │
│      2. publish checkpoint to S3             │
│      3. check for stop signal                │
│    end                                       │
│                                              │
│  Runs for --days N (default 7)               │
│  Survives SSH disconnect (no screen/tmux     │
│  needed) — log: /tmp/soak-loop.log           │
│  Stop: touch /tmp/soak-stop                  │
└──────────────────────────────────────────────┘
         │
         │  S3 checkpoint after each iteration
         ▼
┌──────────────────────────────────────────────┐
│  S3 bucket (internal, VPN-only storage)      │
│                                              │
│  cost-onprem-performance/<run-id>/           │
│    checkpoint-001.json  (hour 1)             │
│    checkpoint-002.json  (hour 2)             │
│    ...                                       │
│    final-results.json   (day 7)              │
│                                              │
│  Pull from any machine on Red Hat VPN        │
└──────────────────────────────────────────────┘
```

Each checkpoint contains the iteration's `SoakTestState` snapshot: upload/query
counts, error list, memory/disk/queue samples, and elapsed time. On completion,
a final summary with aggregate analysis across all iterations is published.

Monitoring is as simple as pulling the latest checkpoint from S3 — no
`kubectl` or SSH into the hypervisor required, though you do need to be on
Red Hat VPN to reach the internal S3 endpoint.

#### Prerequisites

| Requirement | Why |
|-------------|-----|
| **Dedicated cluster** | No other workloads should share the cluster during the soak |
| **Stable deployment** | Deploy with `--sizing-profile medium` (or target profile) first |
| **Hypervisor access** | SSH to the machine hosting the cluster (always-on, unaffected by VPN) |
| **No scheduled jobs** | Disable any cron-based cluster operations that could interfere |

#### Execution

`soak-loop.sh` does not depend on `screen`/`tmux` being installed on the
hypervisor. Use its built-in `--background` flag, which daemonizes itself
via `nohup` + `disown` — the loop survives the SSH session ending, and there
is no need to reattach to anything. (If `screen` or `tmux` happen to be
available, running it inside one works too, but it's not required.)

Use the same `S3_BUCKET`/`S3_ENDPOINT` you already have configured for
publishing results elsewhere (see `docs/performance/OBSERVABILITY.md`):

```bash
export S3_BUCKET="<your-teams-perf-bucket>"
export S3_ENDPOINT="<your-teams-s3-endpoint>"
```

```bash
ssh kni@<hypervisor>
export KUBECONFIG=/home/kni/clusterconfigs/auth/kubeconfig_ocp-edge94
cd /path/to/koku-service-operator

# 7-day soak: 1-hour iterations, checkpoint to S3 after each.
# --background returns immediately once the loop is launched.
# PYTHON=python3.12: the test suite requires 3.10+, but some hosts (e.g.
# ocp-edge94) default `python3` to 3.9 — see note below.
SOAK_TESTS=true SOAK_DURATION_HOURS=1 PYTHON=python3.12 \
  ./scripts/soak-loop.sh --days 7 --background \
  --s3-bucket "${S3_BUCKET}" --s3-endpoint "${S3_ENDPOINT}"

# You can safely exit the SSH session now — the loop keeps running.
```

> **Python version:** the test suite requires Python 3.10+ (some test files
> use PEP 604 `X | None` type hints that raise a `TypeError` at collection
> time on older Python). `run-pytest.sh` checks this and fails fast with the
> available candidates if the default `python3` is too old, but on hosts
> where that's known ahead of time (e.g. `ocp-edge94` defaults to 3.9, with
> `python3.12` available), set `PYTHON=python3.12` up front. `tests/.venv` is
> bound to whichever interpreter created it — delete it before switching
> interpreters on a host that's already run tests with the wrong one.

```bash
# Reconnect later to check on it
ssh kni@<hypervisor>
tail -50 /tmp/soak-loop.log          # recent progress
kill -0 $(cat /tmp/soak-loop.pid)    # exits 0 if still running

# Stop gracefully after the current iteration finishes
touch /tmp/soak-stop

# Force kill (last resort — skips final summary)
kill $(cat /tmp/soak-loop.pid)
```

```bash
# Monitor from any machine on Red Hat VPN via S3 (rooted under the shared
# cost-onprem-performance/ prefix, like every other perf suite). Use
# scripts/s3-upload.py, not raw `aws s3` — this bucket is anonymous/public on
# a non-AWS S3 endpoint with an untrusted cert, and a bare `aws` command
# will behave differently (or fail) depending on your local AWS CLI config.
# s3-upload.py bakes in anonymous signing, TLS bypass, and IPv4-only
# resolution ($S3_BUCKET/$S3_ENDPOINT as set above).
python3 scripts/s3-upload.py ls \
  "s3://${S3_BUCKET}/cost-onprem-performance/<run-id>/" \
  --endpoint-url "${S3_ENDPOINT}"

python3 scripts/s3-upload.py cp \
  "s3://${S3_BUCKET}/cost-onprem-performance/<run-id>/checkpoint-latest.json" - \
  --endpoint-url "${S3_ENDPOINT}"
```

If you'd rather use the `aws` CLI directly, it needs all three flags to behave
correctly against this bucket (and may still be flakier on networks with
broken IPv6 to this endpoint):

```bash
aws s3 ls "s3://${S3_BUCKET}/cost-onprem-performance/<run-id>/" \
  --endpoint-url "${S3_ENDPOINT}" \
  --no-sign-request --no-verify-ssl
```

#### Monitoring Checklist (check daily)

- [ ] **S3 checkpoint** — pull latest, verify upload/query counts incrementing and error count low
- [ ] `kubectl get pods -n cost-onprem` — all pods Running, no CrashLoopBackOff
- [ ] `kubectl top pods -n cost-onprem` — no pod near its memory limit
- [ ] `oc adm top nodes` — no node at >90% memory
- [ ] DB disk: `kubectl exec -n cost-onprem cost-onprem-database-0 -- df -h /var/lib/pgsql/data`

#### What the Test Validates Over 7 Days

| Check | SOAK Test | Threshold |
|-------|-----------|-----------|
| OOM kills | SOAK-001 | 0 pod restarts during window |
| Upload success rate | SOAK-001 | 0 failed uploads |
| API error rate | SOAK-001 | < 5% query failures |
| Memory growth | SOAK-002 | < 5% daily growth per pod |
| Disk growth | SOAK-003 | < 50 GB projected 7-day growth |
| Queue backlog | SOAK-004 | No sustained >100 avg depth or >90% non-empty |

#### Implementation Status

- [x] Test code hardened and validated (1-hour, 4/4 passed)
- [x] Condensed mode for rapid iteration
- [x] JWT token refresh for long-running workers
- [x] Jenkins parameters wired (`SOAK_TESTS`, `SOAK_CONDENSED`, `SOAK_DURATION_HOURS`)
- [x] `soak-loop.sh` wrapper script (outer loop, S3 checkpoints, stop signal)
- [x] S3 checkpoint publishing (JSON checkpoints per iteration + final summary)

### Validated Profiles

| Profile | Run ID | Tests | Result | Duration | Key Metric |
|---------|--------|-------|--------|----------|------------|
| baseline | multiple | 41 | **41 passed, 0 failed** | ~5 min | Smoke test |
| small | multiple | 42 | **42 passed, 0 failed** | ~20 min | Standard workload |
| medium | multiple | 42 | **42 passed, 0 failed** | ~45 min | Listener saturation identified |
| large | 1782412062 | 42 | **42 passed, 0 failed** | ~75 min | 1.9 MB/s upload throughput |
| xlarge | 1782403098 | 42 | **42 passed, 0 failed** | ~123 min | 14+ MB/s upload, 31 exp/min Kruize |

### Test Files

| File | Tests | Description |
|------|-------|-------------|
| `test_ingestion.py` | 6 | Ingestion throughput (ING-001 through ING-006) |
| `test_api_latency.py` | 6 | API latency (API-001 through API-006) |
| `test_scale.py` | 5 | Multi-cluster scale (SCALE-001 through SCALE-005) |
| `test_ros.py` | 4 | ROS/Kruize performance (ROS-001 through ROS-004) |
| `test_soak.py` | 4 | Soak stability (SOAK-001 through SOAK-004, opt-in; condensed mode available) |
| `test_stress.py` | 2 | Stress ramp-to-failure + backlog recovery (STR-001, STR-002) |
| `test_rbac_perf.py` | 6 | RBAC authorization performance (RBAC-001 through RBAC-006) |
| `profiles.py` | — | Profile definitions + NISE YAML generation |
| `tracker.py` | — | Resource cleanup tracker with configurable timeout |
| `conftest.py` | — | Fixtures, cleanup, data generation |
| `verify_infrastructure.py` | — | Pre-run infrastructure validation |

### Running Performance Tests

```bash
# Via the deploy script (recommended — handles profile config, metrics, S3 upload)
./scripts/deploy-test-cost-onprem.sh \
    --skip-deploy --perf-only \
    --perf-profile xlarge --perf-suite all \
    --listener-cpu max --collect-metrics --upload-metrics

# Direct pytest (requires manual profile config)
PERF_PROFILE=medium pytest -m performance tests/suites/performance/

# Specific suite
PERF_PROFILE=xlarge pytest -m "performance and ingestion" tests/suites/performance/

# Stress tests — all, ramp only, or recovery only
./scripts/deploy-test-cost-onprem.sh --perf-only --perf-profile medium --perf-suite stress
./scripts/deploy-test-cost-onprem.sh --perf-only --perf-profile medium --perf-suite stress_ramp
./scripts/deploy-test-cost-onprem.sh --perf-only --perf-profile medium --perf-suite stress_recovery

# Soak tests (opt-in — condensed ~15 min, or 1h/7d standard)
SOAK_TESTS=true SOAK_CONDENSED=true \
  ./scripts/deploy-test-cost-onprem.sh --perf-only --perf-suite soak --listener-cpu none
SOAK_TESTS=true SOAK_DURATION_HOURS=1 \
  ./scripts/deploy-test-cost-onprem.sh --perf-only --perf-suite soak --listener-cpu none

# RBAC authorization tests
./scripts/deploy-test-cost-onprem.sh --perf-only --perf-profile medium --perf-suite rbac

# Via Jenkins
# Job: flightpath-insights-onprem (RUN_PERF_TESTS=true, PERF_PROFILE=xlarge, PERF_SUITE=all)
```

---

## Success Criteria Status

| ID | Criteria | Status | Evidence |
|----|----------|--------|----------|
| SC-1 | Sizing table | **Done** | [sizing-guide.md](./sizing-guide.md) — small through xlarge validated |
| SC-2 | Cluster count limits | **Done** | XLarge (23 clusters) validated; stress ramp tested at medium (75), large (100), xlarge (100+) concurrent sources |
| SC-3 | Bottleneck analysis | **Done** | [FINDINGS.md](./FINDINGS.md) — 14 findings documented with severity and evidence |
| SC-4 | Processing window | **Partial** | XLarge completes in ~2h; need to validate against 6-hour SLA formally |
| SC-5 | Soak test | **Partial** | 1-hour soak validated (4/4 passed, COST-7634). Condensed mode available for rapid iteration. 7-day run pending dedicated cluster time. |

## Next Steps

1. [x] Run medium profile for a clean validated baseline (target: 0 failures) — 28/28 api+ingestion, 4/4 ros passed
2. [x] Execute stress ramp-to-failure across medium, large, xlarge — breaking points identified (75, 100, 100+ sources)
3. [x] Soak tests hardened and validated (COST-7634) — 1-hour run passes, condensed mode for rapid iteration
   - [ ] Execute 7-day soak test — requires dedicated cluster time
4. [x] Publish sizing profile overlays + operator mapping draft (COST-7618) — see `cost-onprem/values-*.yaml` and `operator-profile-crd-mapping.md`
5. [x] File tickets for untracked findings — FLPATH-4428 (013), FLPATH-4429 (020), FLPATH-4430 (022), FLPATH-4431 (024)
6. [x] RBAC authorization performance testing (COST-7643) — 6 tests, validated at medium + large, FINDING-037
