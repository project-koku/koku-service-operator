# Observability Stack for Performance Testing

**FLPATH-4061** | [Performance Testing Plan](./performance-testing-plan.md)

This document describes the metrics collection infrastructure for Cost On-Prem performance testing: Prometheus-based metrics scraping, JSON export, and S3 publishing for historical analysis.

## Overview

The observability stack enables:

1. **Metrics collection** via Prometheus (OpenShift user workload monitoring)
2. **Database metrics** via postgres_exporter
3. **Cache metrics** via valkey-exporter (Redis-compatible)
4. **Snapshot export** to JSON during test runs
5. **S3 publishing** for historical storage and comparison

All data is exported to JSON and self-contained HTML reports. The collected data can be used with any visualization tool.

## Quick Start

### Integrated Approach (Recommended)

Use `deploy-test-cost-onprem.sh` for full integration of deployment, metrics collection, and performance testing:

```bash
# Deploy with observability, run performance tests, and upload results to S3
./scripts/deploy-test-cost-onprem.sh \
    --deploy-observability \
    --run-perf \
    --perf-profile baseline \
    --collect-metrics \
    --upload-metrics

# With explicit S3 configuration (see "S3 Configuration" below for how to
# set S3_ENDPOINT/S3_BUCKET)
S3_ENDPOINT="${S3_ENDPOINT}" \
S3_BUCKET="${S3_BUCKET}" \
./scripts/deploy-test-cost-onprem.sh \
    --deploy-observability \
    --skip-helm \
    --run-perf \
    --collect-metrics \
    --upload-metrics
```

### Standalone Metrics Collection

For collecting metrics independently of performance tests:

```bash
# 1. Deploy metrics collection infrastructure
./scripts/deploy-observability.sh

# 2. Run a performance test while collecting metrics every 30 seconds
TEST_RUN_ID=baseline-v0.2.20 ./scripts/observability/collect-metrics.sh --continuous 30

# 3. Upload results to S3-compatible storage
S3_BUCKET=perf-metrics S3_ENDPOINT=https://s3.example.com \
  ./scripts/observability/collect-metrics.sh --upload
```

## Components

### 1. User Workload Monitoring (Prometheus)

OpenShift's built-in user workload monitoring provides Prometheus with:

- 15-second scrape interval
- 30-day retention (configurable via `RETENTION_DAYS`)
- Persistent storage (20Gi for user workload, 50Gi for platform)

**Verify it's enabled:**

```bash
oc get pods -n openshift-user-workload-monitoring
```

### 2. postgres_exporter

Exports PostgreSQL metrics including:

| Metric | Description |
|--------|-------------|
| `pg_stat_activity_count` | Active connections by state |
| `pg_stat_database_*` | Database-level statistics |
| `pg_stat_statements_*` | Query-level statistics (requires extension) |
| `pg_locks_count` | Lock counts by mode |
| `pg_database_size_bytes` | Database sizes |

**Configuration:**

```bash
# Set credentials before deployment
export POSTGRES_PASSWORD="${POSTGRES_PASSWORD:?set POSTGRES_PASSWORD in your environment}"
export POSTGRES_HOST=postgresql.cost-onprem.svc.cluster.local
```

### 3. valkey-exporter

Exports Valkey/Redis metrics including:

| Metric | Description |
|--------|-------------|
| `redis_memory_used_bytes` | Current memory usage |
| `redis_connected_clients` | Connected client count |
| `redis_evicted_keys_total` | Evicted keys (memory pressure) |
| `redis_keyspace_hits_total` | Cache hits |
| `redis_keyspace_misses_total` | Cache misses |
| `redis_commands_processed_total` | Commands per second |

### 4. Metrics Collection Script

The `collect-metrics.sh` script queries Prometheus and exports metrics to JSON:

```bash
# Single snapshot
./scripts/observability/collect-metrics.sh

# Continuous collection during test (every 30s)
TEST_RUN_ID=my-test ./scripts/observability/collect-metrics.sh --continuous 30

# Collect time range after test completes
./scripts/observability/collect-metrics.sh --range \
  --start 2026-05-19T10:00:00Z \
  --end 2026-05-19T11:00:00Z

# Upload to S3
S3_BUCKET=perf-metrics ./scripts/observability/collect-metrics.sh --upload
```

**Output structure:**

When using the integrated approach via `deploy-test-cost-onprem.sh`, all outputs are organized under a unified directory:

```
perf-runs/{test_run_id}/
├── metrics/                          # Prometheus metrics snapshots
│   ├── snapshot_20260519_100000.json
│   ├── snapshot_20260519_100030.json
│   └── summary.json
├── results/                          # Performance test result JSONs
│   ├── test_api_latency_baseline_20260519_100000.json
│   ├── test_ingestion_baseline_20260519_100100.json
│   └── session_20260519_100000.json
├── reports/                          # CI-compatible reports
│   ├── junit.xml
│   └── report.html
└── metadata.json                     # Test run context and cluster info
```

**Standalone mode** (when running `collect-metrics.sh` directly):

```
metrics-snapshots/{test_run_id}/
├── snapshot_*.json
└── summary.json
```

## Metrics Collected

The following metrics are scraped by Prometheus and exported to JSON snapshots:

| Category | Key Metrics |
|----------|-------------|
| **Ingress** | Upload rate, latency, payload sizes, Kafka consumer lag, throughput |
| **Processing** | Celery queue depths, task throughput/duration/failure rate, worker CPU/memory |
| **Database** | Connection states, query latency (P50/P95/P99), cache hit rate, row ops, locks |
| **ROS** | Recommendations generated/failed, CSV download latency, Kruize API latency |
| **Infrastructure** | Node CPU/memory, disk/network I/O, Valkey memory/operations/hit rate |

## Enabling pg_stat_statements

For detailed query-level metrics, enable the `pg_stat_statements` extension:

```sql
-- Connect to PostgreSQL
CREATE EXTENSION IF NOT EXISTS pg_stat_statements;

-- Verify
SELECT * FROM pg_stat_statements LIMIT 1;
```

Add to `postgresql.conf`:

```
shared_preload_libraries = 'pg_stat_statements'
pg_stat_statements.track = all
```

## Environment Variables

### deploy-observability.sh

| Variable | Default | Description |
|----------|---------|-------------|
| `LOG_LEVEL` | `WARN` | Output verbosity (ERROR/WARN/INFO/DEBUG) |
| `NAMESPACE` | `cost-onprem` | Target namespace for cost-onprem |
| `SKIP_UWM` | `false` | Skip user workload monitoring setup |
| `SKIP_EXPORTERS` | `false` | Skip postgres/valkey exporter deployment |
| `SKIP_GRAFANA` | `true` | Skip Grafana deployment (optional) |
| `RETENTION_DAYS` | `30` | Prometheus metrics retention |
| `POSTGRES_HOST` | auto-detect | PostgreSQL hostname |
| `POSTGRES_PORT` | `5432` | PostgreSQL port |
| `POSTGRES_USER` | `postgres` | PostgreSQL user for exporter |
| `POSTGRES_PASSWORD` | - | PostgreSQL password (required for exporter) |
| `VALKEY_HOST` | auto-detect | Valkey hostname |
| `VALKEY_PORT` | `6379` | Valkey port |

### collect-metrics.sh

| Variable | Default | Description |
|----------|---------|-------------|
| `NAMESPACE` | `cost-onprem` | Target namespace |
| `PROMETHEUS_URL` | auto-detect | Prometheus/Thanos URL |
| `OUTPUT_DIR` | `./metrics-snapshots` | Local output directory |
| `TEST_RUN_ID` | auto-generated | Unique identifier (`{chart_version}-{perf_profile}-{epoch}`) |
| `PERF_PROFILE` | `baseline` | Performance profile name |
| `METRICS_FLAT_OUTPUT` | `false` | Write directly to OUTPUT_DIR without subdirectory |
| `S3_BUCKET` | - | S3 bucket for uploads |
| `S3_PREFIX` | `cost-onprem-performance/` | S3 key prefix |
| `S3_ENDPOINT` | - | S3 endpoint URL (for MinIO, Ceph, etc.) |
| `S3_NO_VERIFY_SSL` | `true` | Skip TLS verification (MinIO uses untrusted certs) |
| `S3_NO_SIGN_REQUEST` | `true` | Anonymous access (bucket is public, no credentials needed) |

### deploy-test-cost-onprem.sh (Observability Options)

| Variable | Default | Description |
|----------|---------|-------------|
| `PERF_OUTPUT_DIR` | `./perf-runs` | Base directory for unified output |
| `TEST_RUN_ID` | auto-generated | Test run identifier |
| `DEPLOY_OBSERVABILITY` | `false` | Deploy exporters (`--deploy-observability`) |
| `COLLECT_METRICS` | `false` | Collect metrics during tests (`--collect-metrics`) |
| `UPLOAD_METRICS` | `false` | Upload to S3 after tests (`--upload-metrics`) |
| `METRICS_INTERVAL` | `30` | Metrics collection interval in seconds |

## S3 Configuration

For S3-compatible storage (MinIO, Ceph RGW, etc.):

```bash
# Full S3 structure (when using deploy-test-cost-onprem.sh with --upload-metrics):
# s3://{bucket}/cost-onprem-performance/{test_run_id}/
#   ├── metrics/
#   │   ├── snapshot_*.json
#   │   └── summary.json
#   ├── results/
#   │   ├── test_*.json
#   │   └── session_*.json
#   ├── reports/
#   │   ├── junit.xml
#   │   └── report.html
#   └── metadata.json

# This project's perf results are stored on an internal (Red Hat VPN-only),
# S3-compatible endpoint. It's not published in this repo since the repo
# itself is public — get the endpoint/bucket from your team, then set:
export S3_ENDPOINT="<your-teams-s3-endpoint>"
export S3_BUCKET="<your-teams-perf-bucket>"
./scripts/deploy-test-cost-onprem.sh --run-perf --collect-metrics --upload-metrics

# The defaults (S3_NO_VERIFY_SSL=true, S3_NO_SIGN_REQUEST=true) assume a
# setup like the one above: TLS verification skipped (untrusted cert) and
# requests unsigned (public/anonymous bucket, no credentials required).
# Override them below if your S3 backend differs.

# Note: S3_BUCKET is intentionally not defaulted to prevent accidental uploads
# during local development. All outputs are saved locally; S3 upload only
# occurs when S3_BUCKET is explicitly set.

# To target a different S3 backend that requires TLS verification and/or
# signed requests, override the defaults:
S3_NO_VERIFY_SSL=false S3_NO_SIGN_REQUEST=false \
  AWS_ACCESS_KEY_ID="${AWS_ACCESS_KEY_ID}" AWS_SECRET_ACCESS_KEY="${AWS_SECRET_ACCESS_KEY}" \
  S3_BUCKET=my-bucket ./scripts/deploy-test-cost-onprem.sh --run-perf --upload-metrics
```

**TEST_RUN_ID format:** `{chart_version}-{perf_profile}-{epoch_time}`

Example: `0-2-20-baseline-1716130800` for chart version 0.2.20, baseline profile.

**Required tools:** Python 3 with `boto3` (auto-discovered from the test venv).

## Metrics Export Format

Each snapshot is a JSON file with this structure:

```json
{
  "timestamp": "2026-05-19T10:30:00Z",
  "test_run_id": "baseline-v0.2.20",
  "namespace": "cost-onprem",
  "metrics": {
    "api_latency_p95": 0.125,
    "celery_queue_depth_total": 3,
    "pg_connections_active": 15,
    "valkey_memory_used_bytes": 52428800,
    ...
  }
}
```

The summary file aggregates metrics metadata:

```json
{
  "test_run_id": "baseline-v0.2.20",
  "namespace": "cost-onprem",
  "start_time": "2026-05-19T10:00:00Z",
  "end_time": "2026-05-19T11:00:00Z",
  "snapshot_count": 120,
  "files": ["snapshot_20260519_100000.json", ...]
}
```

### metadata.json (Test Run Context)

The `metadata.json` file at the test run root provides overall context:

```json
{
  "test_run_id": "0-2-20-baseline-1716130800",
  "chart_version": "cost-onprem-0.2.20",
  "perf_profile": "baseline",
  "namespace": "cost-onprem",
  "created_at": "2026-05-19T10:00:00Z",
  "cluster_info": {
    "ocp_version": "4.14.0",
    "node_count": 6,
    "storage_type": "ODF",
    "platform": "AWS"
  },
  "file_counts": {
    "metrics": 120,
    "results": 15,
    "reports": 2
  }
}
```

## Verifying Metrics Collection

### Check ServiceMonitors

```bash
# List all ServiceMonitors in the namespace
oc get servicemonitors -n cost-onprem

# Expected output includes:
# - cost-onprem-ros-api
# - cost-onprem-ros-processor
# - cost-onprem-ros-recommendation-poller
# - cost-onprem-kruize
# - koku-api
# - cost-onprem-gateway
# - postgres-exporter
# - valkey-exporter
```

### Check Prometheus Targets

In the OpenShift Console:

1. Navigate to **Observe** → **Targets**
2. Filter by namespace `cost-onprem`
3. Verify all targets show "Up" status

### Query Metrics Directly

```bash
# Port-forward to Thanos Querier
oc port-forward -n openshift-monitoring svc/thanos-querier 9091:9091

# Query via curl
curl -s "http://localhost:9091/api/v1/query?query=up{namespace='cost-onprem'}" | jq
```

## Troubleshooting

### ServiceMonitor Not Scraping

1. Verify the ServiceMonitor selector matches the Service labels:

   ```bash
   oc get svc -n cost-onprem --show-labels
   oc get servicemonitor -n cost-onprem -o yaml | grep -A5 selector
   ```

2. Check if user workload monitoring is enabled:

   ```bash
   oc get pods -n openshift-user-workload-monitoring
   ```

### postgres_exporter Connection Failed

1. Verify the secret exists:

   ```bash
   oc get secret postgres-exporter-secret -n cost-onprem
   ```

2. Check exporter logs:

   ```bash
   oc logs -n cost-onprem deploy/postgres-exporter
   ```

3. Test connection manually:

   ```bash
   oc run pg-test --rm -it --image=postgres:15 --restart=Never -- \
     psql "postgresql://postgres:password@postgresql:5432/postgres"
   ```

## Metrics Reference

### Key PromQL Queries

```promql
# API latency P95
histogram_quantile(0.95, sum(rate(http_request_duration_seconds_bucket{namespace="cost-onprem", job=~".*koku.*"}[5m])) by (le))

# Celery queue depth
sum(celery_queue_length{namespace="cost-onprem"})

# PostgreSQL cache hit rate
pg_stat_database_blks_hit{datname="koku"} / (pg_stat_database_blks_hit{datname="koku"} + pg_stat_database_blks_read{datname="koku"}) * 100

# Valkey memory usage
redis_memory_used_bytes{namespace="cost-onprem"}

# Listener CPU usage
rate(container_cpu_usage_seconds_total{namespace="cost-onprem", pod=~".*listener.*"}[5m])
```

## Related Documentation

- [Performance Testing Plan](./performance-testing-plan.md) - Full testing strategy
- [TEST-MATRIX.md](./TEST-MATRIX.md) - Test permutations and parameters
- [FINDINGS.md](./FINDINGS.md) - Issues discovered during testing
- [Resource Requirements](../operations/resource-requirements.md) - Sizing guidance
