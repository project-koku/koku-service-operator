# Performance Test Suite

Performance tests for Cost On-Prem per [FLPATH-4036](https://redhat.atlassian.net/browse/FLPATH-4036).

## Overview

This suite validates:
- **Ingestion throughput** - Data upload and processing capacity
- **Scale limits** - Multi-cluster/source management
- **API latency** - Response times under various loads
- **Resource efficiency** - Memory and CPU utilization

## Test Categories

### Ingestion Throughput (PERF-ING-*)

| Test ID | Description | Metrics |
|---------|-------------|---------|
| PERF-ING-001 | Single source baseline | Processing time, listener CPU |
| PERF-ING-002 | Single source burst (90 days) | Throughput MB/s |
| PERF-ING-003 | Concurrent uploads | Queue depth, error rate |
| PERF-ING-004 | Large file upload (50MB+) | Upload time |
| PERF-ING-005 | High frequency uploads | Queue lag, sustained rate |

### Multi-Cluster Scale (PERF-SCALE-*)

| Test ID | Description | Metrics |
|---------|-------------|---------|
| PERF-SCALE-001 | Source count baseline | Memory usage, API latency |
| PERF-SCALE-002 | Source count ramp | Max sources, breaking point |
| PERF-SCALE-003 | Large source dataset | Query time vs size |
| PERF-SCALE-004 | Concurrent API queries | P50/P95/P99, QPS |
| PERF-SCALE-005 | Historical data depth | Query time vs date range |

### API Latency (PERF-API-*)

| Test ID | Description | Metrics |
|---------|-------------|---------|
| PERF-API-001 | Report API baseline | P50/P95/P99 latency |
| PERF-API-002 | Report API under load | Latency under concurrent users |
| PERF-API-003 | Cost model CRUD | Operations/second |
| PERF-API-004 | Source list pagination | Time per page |
| PERF-API-005 | Complex group-by query | Multi-dimension latency |
| PERF-API-006 | Tag filtering | Filter complexity impact |

## Performance Profiles

Based on production data analysis (Pau Garcia Quiles, April 2026).
See [sizing-guide.md](../../../docs/performance/sizing-guide.md) for resource
recommendations and [profiles.py](profiles.py) for the canonical code definitions.

| Profile | Customers | Clusters | Nodes | CPU Cores | Memory |
|---------|-----------|----------|-------|-----------|--------|
| `small` | 37% | 1 | 15 | 200 | 1.1 TB |
| `medium` | 35% | 2 | 49 | 544 | 2.8 TB |
| `large` | 21% | 7 | 133 | 1,964 | 9.7 TB |
| `xlarge` | 6% | 23 | 346 | 6,954 | 48.5 TB |
| `stress_p99` | P99 | 33 | 1,072 | 57,424 | - |
| `stress_max` | Max | 67 | 4,311 | 793,424 | - |

## Running Tests

```bash
# All performance tests
pytest -m performance tests/suites/performance/

# Specific category
pytest -m "performance and ingestion" tests/suites/performance/
pytest -m "performance and scale" tests/suites/performance/
pytest -m "performance and api_latency" tests/suites/performance/

# With specific profile
PERF_PROFILE=medium pytest -m performance tests/suites/performance/

# Quick baseline only
pytest tests/suites/performance/test_ingestion.py::TestIngestionThroughput::test_perf_ing_001_single_source_baseline
```

## Environment Variables

| Variable | Default | Description |
|----------|---------|-------------|
| `PERF_PROFILE` | `small` | Performance profile to use |
| `PERF_ING_005_DURATION_MINUTES` | `15` | Duration for high-frequency test |
| `PERF_ING_005_INTERVAL_SECONDS` | `300` | Upload interval for HF test |
| `PERF_SCALE_002_MAX_SOURCES` | `25` | Max sources for ramp test |
| `PERF_SCALE_002_BATCH_SIZE` | `5` | Source batch size for ramp |
| `PERF_API_003_ITERATIONS` | `10` | CRUD test iterations |
| `CLUSTER_PLATFORM` | `unknown` | Cluster platform for reporting |

## Output

Performance results are saved to `tests/reports/performance/`:

```
tests/reports/performance/
├── test_name_profile_timestamp.json    # Individual test results
└── session_timestamp.json              # Aggregated session report
```

### JSON Schema

Each test result follows this structure:

```json
{
  "test_id": "test_perf_ing_001-20260415123456",
  "test_name": "test_perf_ing_001_single_source_baseline",
  "profile": "small",
  "chart_version": "0.2.20-rc1",
  "timestamp": "2026-04-15T12:34:56.789Z",
  "cluster_info": {
    "ocp_version": "4.20.0",
    "node_count": 5,
    "worker_node_count": 3,
    "total_cpu_cores": 48,
    "total_memory_gib": 192.0,
    "storage_class": "ocs-storagecluster-ceph-rbd",
    "storage_type": "ODF",
    "platform": "bare-metal"
  },
  "timings": [
    {
      "name": "source_registration",
      "duration_seconds": 5.234,
      "start_time": "2026-04-15T12:34:56.000Z",
      "end_time": "2026-04-15T12:35:01.234Z",
      "metadata": {}
    }
  ],
  "metrics": {
    "profile": "small",
    "upload": {
      "package_size_mb": 2.5,
      "upload_seconds": 1.2,
      "upload_mb_per_second": 2.08
    },
    "listener_cpu_cores": 0.85,
    "processing_completed": true
  },
  "passed": true,
  "error_message": null
}
```

## Integration with CI

Performance tests are marked as `slow` and excluded from regular CI runs:

```bash
# Regular CI (excludes performance)
pytest -m "not slow"

# Performance CI job (dedicated)
pytest -m performance --tb=long
```

## Related Files

- `../../../docs/performance/TEST-MATRIX.md` - **Complete test matrix with all permutations and parameters**
- `conftest.py` - Fixtures for timing, cluster info, report collection
- `profiles.py` - Performance profile definitions and NISE YAML generation
- `test_ingestion.py` - Ingestion throughput tests
- `test_api_latency.py` - API latency tests
- `test_scale.py` - Multi-cluster scale tests
- `../../../docs/performance/FINDINGS.md` - **Issues discovered during testing** (create Jira tickets from this)
- `../../../docs/performance/performance-testing-plan.md` - Full testing plan (FLPATH-4036)

## Tracking Findings

Performance testing exists to find issues. When you discover problems:

1. **Document immediately** in `docs/performance/FINDINGS.md` with:
   - Clear description and evidence (logs, metrics)
   - Root cause analysis
   - Proposed fix or workaround
   
2. **Create Jira tickets** for actionable items

3. **Update status** as fixes are implemented

See `docs/performance/FINDINGS.md` for current issues and their status.

## Jira Stories

- FLPATH-4061: Deploy observability stack
- FLPATH-4062: Implement timing instrumentation
- FLPATH-4063: Define JSON schema
- FLPATH-4064: Create Small profile scenario
- FLPATH-4065: Run v0.2.20 baseline
