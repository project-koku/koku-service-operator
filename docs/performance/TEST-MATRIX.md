# Performance Test Matrix

This document provides a high-level view of all performance test permutations.

## Quick Reference

| Suite | Tests | Parametrizations | Total Permutations |
|-------|-------|------------------|-------------------|
| [**Ingestion**](#ingestion-tests-perf-ing) | 6 | profiles, concurrency, burst days, file sizes | 14 |
| [**API Latency**](#api-latency-tests-perf-api) | 6 | iterations, users, pages, groups, tags | 16 |
| [**Scale**](#scale-tests-perf-scale) | 5 | sources, queries, date ranges | 9 |
| [**ROS**](#roskruize-tests-perf-ros) | 4 | Kruize/recommendation performance | 4 |
| [**Soak**](#soakstability-tests-perf-soak) | 4 | Long-running stability (memory, disk, queues) | 4 |
| [**Listener CPU Sizing**](#listener-cpu-sizing-scenarios) | cross-cutting | listener CPU × load profile | 20 |
| **Total** | **25+** | | **67+** |

---

## Performance Profiles

All tests can be run with different data profiles via `PERF_PROFILE` environment variable.
Profile definitions (cluster counts, node counts, CPU cores) and sizing recommendations
are maintained in [sizing-guide.md](sizing-guide.md#quick-reference) — the canonical
reference for profile data.  Key profiles: `baseline` (smoke), `small`/`medium`/`large`
(customer tiers covering 93% of deployments), `xlarge` (top 6%).

### Cluster Infrastructure per Profile

Worker node VM resources used for each profile. These are the `WORKER_CPU` and
`WORKER_MEMORY` settings for the auto-toolbox deployment. Master CPU can be lowered
to 8 vCPU (actual usage <2 cores) to free headroom for workers.

| Profile | Workers | WORKER_CPU | WORKER_MEMORY | Replicas (proc/listener/ocp/summary) | Notes |
|---------|---------|------------|---------------|--------------------------------------|-------|
| `baseline` | 3 | 12 | 48000 | 1/1/1/1 | Listener CPU boosted to node max |
| `small` | 3 | 12 | 48000 | 1/2/2/2 | Listener + workers scaled for drain |
| `medium` | 3 | 12 | 64000 | 2/2/2/2 | Memory bump needed for ROS/listener |
| `large` | 3 | 16 | 64000 | 3/3/3/3 | CPU bump needed for extra pods |
| `xlarge` | 3 | 18 | 64000 | 3/3/3/3 | **Validated** — 42/42 pass, 123 min |

Hypervisor constraint: 80 threads total. With 3 masters at 10 vCPU (30 total),
max `WORKER_CPU` is 16 (48 + 30 = 78/80). Trimming masters to 8 vCPU allows
`WORKER_CPU=18` (54 + 24 = 78/80).

---

## Ingestion Tests (PERF-ING-*)

Tests data upload and processing capacity.

| Test ID | Description | Parameters | Values |
|---------|-------------|------------|--------|
| **ING-001** | Single source baseline | `profile_name` | `baseline`, `small` |
| **ING-002** | Single source burst | `burst_days` | `30`, `60`, `90` |
| **ING-003** | Concurrent uploads | `concurrent_sources` | `2`, `5`, `10` |
| **ING-004** | Large file upload (50MB+) | `target_size_mb` | `50`, `100` |
| **ING-005** | High frequency uploads | - | (configurable via env) |
| **ING-006** | Processing window validation | `profile_name` | Runs for small/medium/large profiles (SC-4 SLA validation) |

### Ingestion Test Matrix

| Test | baseline | small | medium | large | xlarge | Notes |
|------|----------|-------|--------|-------|--------|-------|
| ING-001 | ✓ | ✓ | - | - | - | Quick validation |
| ING-002 | - | 30d/60d/90d | - | - | - | Burst scenarios |
| ING-003 | - | 2/5/10 concurrent | - | - | - | Concurrency limits |
| ING-004 | - | - | - | 50MB | 100MB | Large file uploads |
| ING-005 | - | ✓ | - | - | - | Sustained load |
| ING-006 | - | ✓ | ✓ | ✓ | - | SC-4 window validation |

### Environment Variables

| Variable | Default | Description |
|----------|---------|-------------|
| `PERF_ING_005_DURATION_MINUTES` | 15 | High-frequency test duration |
| `PERF_ING_005_INTERVAL_SECONDS` | 300 | Upload interval |
| `PERF_INGESTION_BURST_DAYS` | 30 | Default burst duration |
| `PERF_ING_006_UPLOADS` | 2 (baseline profile) / 4 (small+) | Daily uploads to simulate |

---

## API Latency Tests (PERF-API-*)

Tests API response times under various conditions.

| Test ID | Description | Parameters | Values |
|---------|-------------|------------|--------|
| **API-001** | Report baseline | `iterations` | `10`, `50` |
| **API-002** | Report under load | `concurrent_users` | `5`, `10`, `20` |
| **API-003** | Cost model CRUD | `iterations` | `10` (via `PERF_API_003_ITERATIONS`) |
| **API-004** | Source pagination | `page_size` | `10`, `50`, `100` |
| **API-005** | Complex group-by | `group_by_dims` | `[project]`, `[project,node]`, `[project,cluster]` (API max: 2 dims) |
| **API-006** | Tag filtering | `tag_count` | `1`, `5`, `10` |

### API Test Matrix

| Test | 10 | 50 | 100 | Notes |
|------|-----|-----|------|-------|
| API-001 iterations | ✓ | ✓ | - | Request count |
| API-004 page_size | ✓ | ✓ | ✓ | Pagination |

| Test | 5 | 10 | 20 | Notes |
|------|---|-----|-----|-------|
| API-002 users | ✓ | ✓ | ✓ | Concurrent load |

| Test | 1 dim | 2 dims (node) | 2 dims (cluster) | Notes |
|------|-------|---------------|-------------------|-------|
| API-005 group-by | ✓ | ✓ | ✓ | API enforces max 2 group_by dims |

| Test | 1 tag | 5 tags | 10 tags | Notes |
|------|-------|--------|---------|-------|
| API-006 filters | ✓ | ✓ | ✓ | All tag counts pass (profile-aware timeout) |

### Environment Variables

| Variable | Default | Description |
|----------|---------|-------------|
| `PERF_API_003_ITERATIONS` | 10 | CRUD test iterations |

---

## Scale Tests (PERF-SCALE-*)

Tests multi-cluster and source management at scale.

| Test ID | Description | Parameters | Values |
|---------|-------------|------------|--------|
| **SCALE-001** | Source count baseline | `source_count` | `5`, `10` |
| **SCALE-002** | Source ramp | - | (configurable max/batch) |
| **SCALE-003** | Large namespace count | - | - |
| **SCALE-004** | Concurrent queries | `concurrent_queries` | `5`, `10`, `20` |
| **SCALE-005** | Historical depth | `date_range_days` | `10`, `30` |

### Scale Test Matrix

| Test | 5 | 10 | 20 | 30 | Notes |
|------|---|-----|-----|-----|-------|
| SCALE-001 sources | ✓ | ✓ | - | - | Baseline |
| SCALE-004 queries | ✓ | ✓ | ✓ | - | Concurrent |
| SCALE-005 days | ✓ | - | - | ✓ | History |

### Environment Variables

| Variable | Default | Description |
|----------|---------|-------------|
| `PERF_SCALE_002_MAX_SOURCES` | 25 | Max sources for ramp |
| `PERF_SCALE_002_BATCH_SIZE` | 5 | Source batch size |

---

## ROS/Kruize Tests (PERF-ROS-*)

Tests Resource Optimization Service performance.

| Test ID | Description | Prerequisites |
|---------|-------------|---------------|
| **ROS-001** | Recommendation baseline | Data with ROS metrics |
| **ROS-002** | Multi-workload scale | Multiple namespaces |
| **ROS-003** | Recommendation refresh | Existing experiments |
| **ROS-004** | Kruize memory pressure | Extended run |

### ROS Test Requirements

- Kruize must be deployed and healthy
- Data must include resource metrics (CPU/memory requests/limits)
- Kafka must be configured for ROS events

---

## Soak/Stability Tests (PERF-SOAK-*)

Tests long-running stability and resource trends.

| Test ID | Description | Default Duration |
|---------|-------------|------------------|
| **SOAK-001** | Continuous operation | 1 hour |
| **SOAK-002** | Memory leak detection | 1 hour |
| **SOAK-003** | Disk usage monitoring | 1 hour |
| **SOAK-004** | Queue health monitoring | 1 hour |

### Soak Test Environment Variables

| Variable | Default | Description |
|----------|---------|-------------|
| `SOAK_TESTS` | `false` | **Required** - Set to `true` to enable soak tests |
| `SOAK_DURATION_HOURS` | 1 | Test duration |
| `SOAK_UPLOAD_INTERVAL_MINUTES` | 15 | Upload frequency |
| `SOAK_QUERY_INTERVAL_MINUTES` | 5 | Query frequency |
| `SOAK_METRICS_INTERVAL_SECONDS` | 60 | Metrics collection |

**Note:** Soak tests are **opt-in** and independent of `PERF_PROFILE`. They require explicit `SOAK_TESTS=true` because they run for hours and need pre-validated data setup.

### Soak Duration Recommendations

| Scenario | Duration | Use Case |
|----------|----------|----------|
| Quick validation | 0.02h (~72s) | CI/development |
| Standard | 1h | Nightly |
| Extended | 4h | Weekly |
| Full soak | 24h | Release qualification |

---

## Quick Reference: Flags → Actions

Example command (see `docs/performance/OBSERVABILITY.md` for how to set
`S3_BUCKET`/`S3_ENDPOINT`):
```bash
S3_BUCKET="<your-teams-perf-bucket>" \
S3_PREFIX="cost-onprem-performance/" \
S3_ENDPOINT="<your-teams-s3-endpoint>" \
./scripts/deploy-test-cost-onprem.sh \
  --skip-deploy --perf-only --perf-profile small --collect-metrics --upload-metrics
```

| Flag / Env Var | Effect |
|----------------|--------|
| `--skip-deploy` | Skip Helm install, use existing deployment |
| `--perf-only` | Run only performance tests (skip E2E/IQE) |
| `--perf-profile small` | Use `small` profile: ING-002, ING-004, ING-005, ING-006, extended SCALE/ROS tests |
| `--collect-metrics` | Capture Prometheus snapshots during tests |
| `--upload-metrics` | Sync results to S3 after tests complete |
| `S3_BUCKET` | Target bucket for uploads |
| `S3_ENDPOINT` | Custom S3 endpoint (MinIO, ODF, etc.) |
| `SOAK_TESTS=true` | Enable soak tests (opt-in, adds ~4 hours) |

| Profile | Tests Run | Skipped | Duration |
|---------|-----------|---------|----------|
| `baseline` | ING-001, API-*, SCALE-001[5], ROS-001/003 | ING-002/004/005/006, SOAK-* | ~30-50 min |
| `small` | Above + ING-002/004/005/006, extended SCALE/ROS | SOAK-* | ~2 hours |
| `small` + `SOAK_TESTS=true` | Above + SOAK-001/002/003/004 | — | ~6 hours |

---

## Test Execution Patterns

### Quick Validation (~5 min)
```bash
# Infrastructure check + health
pytest suites/performance/test_api_latency.py::TestAPIHealthCheck -v

# Baseline ingestion only
pytest -k "ing_001 and baseline" -v
```

### Baseline profile (~30-50 min)
```bash
# Full suite including ING-006[window_validation] (2 uploads, trimmed dataset)
PERF_PROFILE=baseline pytest -m performance -v
```

### Small profile (~2-3 hours)
```bash
# Includes ING-006[small] (~2 hrs alone) + full suite
PERF_PROFILE=small pytest -m performance --timeout=14400 -v
```

### Full Performance (~4-8 hours)
```bash
PERF_PROFILE=medium pytest -m performance --timeout=28800 -v
```

### Profile-Specific
```bash
# Small profile (excludes soak tests by default)
PERF_PROFILE=small pytest -m performance -v

# Medium profile (longer timeouts)
PERF_PROFILE=medium pytest -m performance --timeout=14400 -v

# Large profile (stress testing)
PERF_PROFILE=large pytest -m performance --timeout=28800 -v
```

### Soak Tests (opt-in, separate from profiles)
```bash
# Run soak tests with 1-hour duration (default)
SOAK_TESTS=true pytest -m soak -v

# Run soak tests with custom duration
SOAK_TESTS=true SOAK_DURATION_HOURS=4 pytest -m soak --timeout=18000 -v

# Combine with performance tests (not recommended - very long runtime)
PERF_PROFILE=small SOAK_TESTS=true pytest -m performance --timeout=28800 -v
```

---

## Timeout Multipliers

Timeouts automatically scale based on profile:

| Profile | Multiplier | 300s base → |
|---------|------------|-------------|
| baseline | 1.0x | 300s |
| small | 1.0x | 300s |
| medium | 2.0x | 600s |
| large | 4.0x | 1200s |
| xlarge | 8.0x | 2400s |
| stress_p99 | 12.0x | 3600s |
| stress_max | 20.0x | 6000s |

---

## Data Setup Scenarios

For tests requiring pre-existing data, use `setup-test-data.sh`:

| Scenario | Clusters | Nodes | Days | ROS | Upload Time | Processing |
|----------|----------|-------|------|-----|-------------|------------|
| `minimal` | 1 | 1 | 1 | No | <30s | <2min |
| `baseline` | 1 | 2 | 7 | Yes | <2min | <10min |
| `perf-small` | 1 | 15 | 30 | Yes | <5min | <30min |
| `perf-medium` | 2 | 49 | 30 | Yes | <15min | <60min |
| `perf-large` | 7 | 133 | 30 | Yes | <45min | <3hr |
| `ros` | 1 | 3 | 7 | Yes | <2min | <15min |

```bash
# Setup for E2E tests
./scripts/setup-test-data.sh --scenario baseline

# Setup for performance tests
./scripts/setup-test-data.sh --scenario perf-small
```

---

## Listener CPU Sizing Scenarios

The listener is the ingestion bottleneck — it receives Kafka messages, downloads
payloads from S3, and hands them off to Celery workers. Its CPU limit directly
determines upload throughput and backpressure behaviour under concurrent load.

### Why This Matters

The on-prem default is **150m request / 300m limit** (aligned with SaaS Clowder).
In production environments with burst ingestion or large file uploads, this may be
undersized. These scenarios answer: *"what CPU do I need to meet my SLA?"*

### Listener CPU Configurations

| Label | CPU Request | CPU Limit | Notes |
|-------|------------|-----------|-------|
| `constrained` | 150m | 300m | Chart default (SaaS-aligned) |
| `moderate` | 250m | 500m | Modest uplift |
| `recommended` | 500m | 1000m | Suggested for production |
| `uncapped` | 500m | node max | Benchmark ceiling (`--listener-cpu max`) |

### Listener CPU × Load Profile Matrix

Primary focus: ingestion tests (ING-001, ING-003, ING-004, ING-006) since those
are directly CPU-bound on the listener. API and ROS tests are largely unaffected.

| CPU Config | baseline (~30-50 min) | small (~2-3 hr) | medium (~4-6 hr) | large | Primary Metric |
|------------|-----------------------|-----------------|-----------------|-------|----------------|
| `constrained` (300m) | ING-001..006[wv] ✓ | ING-001,003,005,006[wv+small] ✓ | ING-006[+medium] ✓ | ING-004 ✓ | Throughput ceiling |
| `moderate` (500m) | - | ING-001,003,005,006 ✓ | ING-006 ✓ | ING-004 ✓ | Improvement delta |
| `recommended` (1000m) | - | ING-001,003,005,006 ✓ | ING-006 ✓ | ING-004 ✓ | Target SLA check |
| `uncapped` (max) | - | ING-001,003,005,006 ✓ | ING-006 ✓ | ING-004 ✓ | Theoretical max |

### Key Questions Per Scenario

| Scenario | Question |
|----------|----------|
| `constrained` × `small` | Does the default config meet the 6-hour processing window? |
| `constrained` × `large` | At what file size does 300m CPU become a bottleneck? |
| `moderate/recommended` × `small` | What is the throughput improvement per 250m of CPU? |
| `uncapped` × `medium` | What is the maximum achievable throughput without CPU constraints? |
| `constrained` × `concurrent=10` | Does CPU throttling cause backpressure / message lag under concurrency? |
| `recommended` × `concurrent=10` | Does 1000m CPU fully resolve concurrency backpressure? |

### Execution

Use `--listener-cpu` combined with `--perf-profile` and `-k` to run specific
cells of the matrix:

```bash
# Row: constrained × small (establish baseline)
./scripts/deploy-test-cost-onprem.sh \
  --skip-deploy --perf-only --perf-profile small \
  --collect-metrics

# Row: recommended × small (compare against baseline)
./scripts/deploy-test-cost-onprem.sh \
  --skip-deploy --perf-only --perf-profile small \
  --listener-cpu 1000m \
  --collect-metrics

# Row: uncapped × medium (find ceiling)
./scripts/deploy-test-cost-onprem.sh \
  --skip-deploy --perf-only --perf-profile medium \
  --listener-cpu max \
  --collect-metrics

# Focused: concurrency test only, two CPU configs
PERF_PROFILE=small LISTENER_CPU_LIMIT=300m \
  pytest tests/suites/performance/test_ingestion.py -k "ing_003" -v

PERF_PROFILE=small LISTENER_CPU_LIMIT=1000m \
  pytest tests/suites/performance/test_ingestion.py -k "ing_003" -v
```

### Metrics to Capture Per Run

| Metric | Source | Target |
|--------|--------|--------|
| Upload throughput (MB/s) | Test output | > 10 MB/s at `recommended` |
| Processing time (s) per MB | Test output | < 30s/MB at `recommended` |
| Listener CPU utilization % | Prometheus `container_cpu_usage_seconds_total` | < 80% sustained |
| Kafka consumer lag | `kafka_consumergroup_lag` | < 100 messages |
| 6-hour window compliance | ING-006 pass/fail | All profiles pass at `recommended` |

### Suggested Priority Order

1. `constrained` × `small` — establish the current baseline (run this first)
2. `recommended` × `small` — validate the proposed production default
3. `constrained` × `medium` — find where default config breaks
4. `uncapped` × `medium` — find the ceiling
5. `recommended` × `large` (ING-004 only) — large file upload with recommended CPU

---

## Related Documentation

- [Performance Testing Plan](./performance-testing-plan.md) - Full FLPATH-4036 plan
- [Test Data Setup Guide](../development/test-data-setup.md) - Data generation
- [Sizing Guide](./sizing-guide.md) - Resource recommendations
- [FINDINGS.md](./FINDINGS.md) - Issues discovered during testing
