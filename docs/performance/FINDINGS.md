# Performance Testing Findings

Issues and sizing requirements discovered during performance testing
(FLPATH-4036, COST-7567). This document is a point-in-time record of what
was found and how it was addressed. Finding statuses are intentionally fixed
at the time of discovery — Jira tickets track ongoing resolution.

---

## Product Issues

### PERF-FINDING-001: Gateway Timeout Too Low for Large File Uploads

**Status**: Fixed (Closed)  
**Severity**: Critical  
**Jira**: [FLPATH-4091](https://redhat.atlassian.net/browse/FLPATH-4091)

**Problem**:
The Envoy gateway and HAProxy route had 30s timeouts for the `/api/ingress/` route. Large file uploads (30+ days of data, ~48MB+) take 25-60+ seconds to transfer, causing HTTP 408/504 timeout errors even though ingress successfully processes the upload.

**Impact**:
- Customers with large clusters cannot upload more than ~2 weeks of data at once
- HTTP 504 errors returned to clients despite successful server-side processing

**Proposed Fix**:
1. HAProxy route timeout: 30s → 180s (`values.yaml`)
2. Envoy route timeout: 30s → 180s, per_try_timeout: 10s → 60s (`configmap-envoy.yaml`)

**Validation**: `ing_002[30-days]` passes with fix (68MB upload at 5.23 MB/s).
Applied automatically by `apply_perf_profile_config()` for medium+ profiles.

---

### PERF-FINDING-006: Kruize Pod Restarts Under Load — CPU Throttling

**Status**: Fixed (Closed)  
**Severity**: Medium  
**Jira**: [FLPATH-4302](https://redhat.atlassian.net/browse/FLPATH-4302)

**Problem**:
Kruize pod restarts during moderate load (50 concurrent experiments) due to CPU throttling at the 1 core limit. Peak memory was only 786 MB (38% of 2Gi limit), ruling out OOM. CPU throttling causes slow liveness probe responses, triggering restarts.

**Proposed Fix**:
```yaml
resources:
  kruize:
    requests:
      cpu: "1000m"    # Was 500m
    limits:
      cpu: "2000m"    # Was 1000m
```

**Validation**: 0 restarts at 2 cores with the same workload.
Applied automatically by `apply_perf_profile_config()` for medium+ profiles.

---

### PERF-FINDING-011: Envoy `request_timeout` Kills Large Uploads

**Status**: Fixed in chart  
**Severity**: High

**Problem**: The Envoy connection manager had `request_timeout: 60s`, which caps total time to receive the full request body from the client. For payloads >~40 MB over WAN, body transfer exceeds 60s and Envoy returns 408 before the request reaches the ingress pod.

**Fix**: `request_timeout: 0s` (disabled; per-route timeouts govern each path). `stream_idle_timeout: 300s` remains as protection against hung connections.

---

### PERF-FINDING-013: ROS Processor FK Errors Block Kafka Queue

**Status**: Mitigated; upstream fix needed  
**Severity**: High  
**Jira**: [FLPATH-4428](https://redhat.atlassian.net/browse/FLPATH-4428) — related to [COST-7722](https://redhat.atlassian.net/browse/COST-7722) (ros-ocp-backend performance optimization)

**Problem**:
The ros-processor (Go) uses `kafkaAutoCommit: true` and does not advance the offset on FK errors. When a source is deleted while ROS events are still in Kafka, those events become permanent poison pills that block all subsequent events on that partition.

**Impact**:
Not a production issue today because sources are long-lived. However, it represents a resilience gap for any source lifecycle operation (bulk source removal, disaster recovery re-registration).

**Upstream fix needed**: Dead-letter handling — after N consecutive FK failures on the same event, commit the offset and log a warning rather than retrying forever. The ros-ocp-backend codebase (`ros-ocp-backend/processor/`) handles Kafka consumption in a single-threaded loop with `kafkaAutoCommit: true`. The consumer calls `commitSync()` only after successful DB writes. When an FK constraint violation occurs (e.g., referencing a deleted source), the offset is never advanced, and the same message is re-consumed indefinitely. A dead-letter topic or configurable retry limit (e.g., `MAX_FK_RETRIES=3`) would allow the consumer to skip poison-pill messages and continue processing the partition.

---

## Sizing Requirements

### PERF-FINDING-002: Listener CPU is the Primary Ingestion Bottleneck

**Status**: Documented — sizing recommendation  
**Severity**: High  
**Related Jira**: [COST-7618](https://redhat.atlassian.net/browse/COST-7618) (Sizing guide), [COST-6993](https://redhat.atlassian.net/browse/COST-6993) (Listener autoscaling)

**Problem**:
The chart default CPU limit (150m request / 300m limit) throttles all ingestion workloads. At 300m, the listener runs at 157% CPU (throttled) during medium-profile ingestion.

**Evidence**:

| Component | Peak CPU | CPU Limit | Utilization |
|-----------|---------|-----------|-------------|
| Listener | 471m | 300m | 157% (throttled) |
| Kruize | 185m | 2000m | 9% |
| Celery Workers | 368m | varies | low |
| Postgres | 327m | - | moderate |

**Impact**: ~40-50% slower ingestion compared to uncapped CPU.

**Recommendation**: Production deployments handling burst ingestion or large file uploads should raise the listener CPU limit to at least 1000m.

---

### PERF-FINDING-003: Pipeline Serialization Limits Concurrent Source Processing

**Status**: Documented — scaling recommendation  
**Severity**: Medium  
**Related Jira**: [COST-7598](https://redhat.atlassian.net/browse/COST-7598) (Pipeline analysis), [COST-6163](https://redhat.atlassian.net/browse/COST-6163) (Worker autoscaling)

**Problem**:
The default single-replica listener/worker configuration cannot drain concurrent source uploads within expected processing windows. Sources queue in Kafka and process serially.

**Recommended Configuration**:

| Concurrent Sources | listener | ocp-worker | summary-worker |
|--------------------|----------|------------|----------------|
| 1–3 | 1 | 1 | 1 |
| 4–10 | 2 | 2 | 2 |
| 10+ | 3 | 3 | 3 |

---

### PERF-FINDING-004: Kruize Experiment Creation Rate — CPU Was the Bottleneck

**Status**: Documented — throughput baseline updated  
**Severity**: Low (informational)  
**Related Jira**: [COST-7722](https://redhat.atlassian.net/browse/COST-7722) (ros-ocp-backend performance optimization)

**Problem** (original assessment):
Kruize created experiments at ~8 per minute at the default 500m/1000m CPU allocation, which was attributed to Hibernate connection pool limits (`c3p0maxsize=5`).

**Updated assessment**:
With the FLPATH-4302 CPU increase (1000m/2000m), Kruize throughput jumped to **31 exp/min** — a 4x improvement. This confirms the bottleneck was **CPU throttling**, not the connection pool. The original 1000m CPU limit caused liveness probe slowdowns that gated experiment creation throughput.

**Evidence**:

| Run | Profile | CPU req/lim | Experiments | Rate/min | Peak Memory | Restarts |
|-----|---------|-------------|-------------|----------|-------------|----------|
| 1781193941 | medium | 500m/1000m | 160/160 | 8.6 | 723 MB | 0 |
| 1782335628 | xlarge | 1000m/2000m | 600/600 | 31.0 | 415 MB | 0 |

**Recommendation**: Keep Kruize at 1 replica. The CPU increase from FLPATH-4302 is the primary lever for throughput — connection pool tuning is secondary.

---

### Resource Sizing — Under-Provisioned Defaults

| Component | Chart Default | Recommended | Evidence |
|-----------|-------------|-------------|----------|
| Kruize CPU | 500m/1000m | 1000m/2000m | FINDING-006: probe failures at 1 core |
| ROS processor memory | 1Gi/1Gi | 2Gi/4Gi | OOMKill during experiment processing |
| Listener memory | 300Mi/600Mi | 1Gi/2Gi (large: 2Gi/4Gi) | OOMKill during archive extraction |
| Ingress max upload | 100MB | 200MB (large: 500MB) | Hard limit; needs explicit change for large files |
| Ingress upload memory | 32MB | 64MB (large: 128MB) | FINDING-021: disk spill adds latency |
| Gateway timeouts | 30s | 180s (large: 600s) | FINDING-001, -020: 504 on medium/large uploads |
| Ingress pod memory | 1Gi/1Gi | 1Gi/2Gi (large: 2Gi/4Gi) | FINDING-022: OOM on large/concurrent uploads |
| OCP worker CPU | 250m/500m | 500m/1000m (large) | FINDING-025: 15% faster processing |
| Summary worker CPU | 250m/500m | 500m/1000m (large) | FINDING-025: 15% faster processing |
| OCP worker memory | 512Mi/1Gi | 1Gi/2Gi (large) | FINDING-025: headroom for large data sets |

---

### PERF-FINDING-020: Envoy Gateway ConfigMap Not Reloaded on Helm Upgrade

**Status**: Mitigated in perf scripts; chart fix needed  
**Severity**: Critical  
**Jira**: [FLPATH-4429](https://redhat.atlassian.net/browse/FLPATH-4429)

**Problem**:
Envoy reads its config file at startup only — it does not watch for changes. When `helm upgrade` modifies the gateway ConfigMap (e.g. increasing `ingressTimeout` from 30s to 600s), the running gateway pod continues using the old values. This rendered all timeout overrides from `apply_perf_profile_config()` ineffective, causing HTTP 504 failures on medium/large profile uploads despite correct values in the ConfigMap.

**Evidence**: Envoy ConfigMap showed 600s timeouts, but uploads failed at ~30s (the old default). All ING-001[medium/large], ING-002, ING-003[10], ING-004 tests failed with instant 504/500 errors.

**Mitigation**: `perf-testing.sh` now explicitly runs `oc rollout restart` on the gateway deployment after helm upgrade.

**Recommended chart fix**: Add a `checksum/envoy-config` pod template annotation to the gateway deployment so any ConfigMap change triggers an automatic rolling restart during `helm upgrade`. This is the [standard Helm pattern](https://helm.sh/docs/howto/charts_tips_and_tricks/#automatically-roll-deployments) for static-config applications like Envoy.

**Why this matters for customers**: Any customer who adjusts gateway timeouts, route prefixes, or TLS settings via `helm upgrade` will silently run on stale Envoy config until the gateway pod is manually restarted. This is a silent misconfiguration that is difficult to diagnose — the ConfigMap shows the correct values, but the running process uses the old ones.

---

### PERF-FINDING-021: Ingress In-Memory Buffer Too Small for Large Uploads

**Status**: Fixed in perf profiles  
**Severity**: Medium

**Problem**:
The `INGRESS_MAXUPLOADMEM` default is 32 MB. For uploads >32 MB, insights-ingress-go spills to disk, adding latency. Combined with tight per-try timeouts, this can push uploads past the retry window.

**Fix**: `apply_perf_profile_config()` now sets `ingress.upload.maxMemory` per profile:
- medium: 64 MB
- large: 128 MB

---

### PERF-FINDING-022: Ingress Pod Memory Insufficient for Large/Concurrent Uploads

**Status**: Mitigated in perf profiles; chart fix needed  
**Severity**: High  
**Jira**: [FLPATH-4430](https://redhat.atlassian.net/browse/FLPATH-4430)

**Problem**:
The ingress pod (`insights-ingress-go`) uses the shared `resources.application` block (1Gi memory limit). Processing large uploads requires multipart parsing, tar extraction, and S3 staging — all memory-intensive. Uploads >100 MB or 10+ concurrent uploads cause HTTP 500 errors from the ingress pod due to memory exhaustion.

**Evidence** (large profile):
- ING-004[100] (101 MB) passed but ING-004[50] failed immediately after — residual memory pressure
- ING-001[large] (~200+ MB package) — HTTP 500 on all attempts
- ING-003[10] (10 concurrent uploads) — all 10 uploads returned HTTP 500

Uploads up to 138 MB succeed individually (ING-002[90-days] = 138.74 MB at 2.44 MB/s).

**Mitigation**: `apply_perf_profile_config()` now overrides `resources.application` per profile:
- medium: 1Gi/2Gi
- large: 2Gi/4Gi

**Recommended chart fix**: Give ingress its own resource block in `values.yaml` (separate from the shared `resources.application`) so it can be sized independently for large file handling without affecting other services.

**Why this matters**: The `resources.application` block is shared across 8+ deployments (ingress, koku-api, koku-worker, koku-listener, koku-clowder, masu, etc.). Raising memory limits for ingress also raises them for every other service using the shared block, wasting cluster resources. Ingress has fundamentally different memory characteristics — it buffers entire upload payloads in memory during multipart parsing and S3 staging, whereas most other services have modest steady-state memory needs. An independent `resources.ingress` block in `values.yaml` would allow customers to size ingress for their expected upload volume without over-provisioning the rest of the stack.

---

### PERF-FINDING-024: Ingress Single-Part S3 Upload Fails for Large Payloads

**Status**: Product limitation; upstream enhancement needed  
**Severity**: Medium  
**Jira**: [FLPATH-4431](https://redhat.atlassian.net/browse/FLPATH-4431)

**Problem**:
`insights-ingress-go` uses the minio-go `PutObject()` API for S3 staging, which performs a single-part upload. Payloads exceeding ~150 MB consistently fail with HTTP 500 against NooBaa/Ceph RGW backends. The error originates in the S3 staging step, not in multipart form parsing or pod memory limits.

**Evidence**:
- ING-002[90-days] (138 MB) passes reliably across runs #40, #41
- ING-001[large] (~200+ MB) fails with HTTP 500 on every attempt (runs #40, #41, #42)
- ING-003[10] (10 concurrent small uploads) also fails — likely S3 connection pool exhaustion under concurrent staging

**What was tried**:
- Pod memory: `resources.application` increased to 2Gi/4Gi — no effect on the 500s (pod uses only ~49 Mi at idle, 0 restarts, 0 OOM events)
- `INGRESS_MAXUPLOADMEM`: Increased from 128 MB to 512 MB — **made things worse**. Go's `ParseMultipartForm(512MB)` pre-allocates heap per request, and the oversized allocation destabilized the pipeline (ING-002[30-days], previously reliable, failed with "manifest not yet visible" after 1500s). Reverted to 128 MB.
- Node headroom: Worker nodes at 53-67% memory requests, no evictions — cluster resources are not the constraint

**Recommended upstream enhancement**: `insights-ingress-go` should use multipart S3 uploads (e.g., minio-go `PutObject` with `PartSize` option or the AWS SDK S3 upload manager) for payloads exceeding a configurable threshold. This is the standard pattern for large object uploads to S3-compatible backends.

**Workaround**: Customers with large clusters generating >150 MB upload packages should split data into multiple smaller uploads (e.g., by time range or namespace).

**Upstream context**: The `insights-ingress-go` service ([project-koku/insights-ingress-go](https://github.com/project-koku/insights-ingress-go)) uses `minio.Client.PutObject()` with default options, which performs a single-part upload for small objects and only switches to multipart for objects exceeding `minPartSize` (default 16 MiB in minio-go). However, the S3 backend (NooBaa/Ceph RGW) rejects single-part PUTs exceeding ~150 MB. Setting an explicit `PartSize` option (e.g., 64 MiB) on the `PutObjectOptions` would force multipart uploads for payloads above that threshold, matching the standard pattern for large object uploads to S3-compatible backends.

---

### PERF-FINDING-025: OCP/Summary Worker CPU Throttling Slows Data Processing

**Status**: Mitigated in perf profiles  
**Severity**: Medium  
**Related Jira**: [COST-7598](https://redhat.atlassian.net/browse/COST-7598) (Pipeline analysis), [COST-7618](https://redhat.atlassian.net/browse/COST-7618) (Sizing guide), [COST-7599](https://redhat.atlassian.net/browse/COST-7599) (Validate tuned configuration)

**Problem**:
The chart default CPU limits for OCP and summary celery workers (250m request / 500m limit) throttle data processing throughput. When the listener ingests data faster than workers can process it, the pipeline backs up.

**Evidence** (large profile, before/after worker CPU boost):
- Worker CPU boosted from 250m/500m to 500m/1000m (request/limit)
- Worker memory boosted from 512Mi/1Gi to 1Gi/2Gi
- Total run time: 112 min → 97.6 min (**15% faster**)
- KPI violations: 1 → 0

**Fix**: `apply_perf_profile_config()` now overrides worker resources per profile:
- medium: 250m/1000m CPU, 512Mi/2Gi memory
- large: 500m/1000m CPU, 1Gi/2Gi memory

**Recommendation**: Production deployments processing large or frequent uploads should increase OCP and summary worker CPU limits to at least 1000m.

---

## Backend Bottleneck Analysis (COST-7605)

### PERF-FINDING-026: Valkey Evictions Do Not Cause Chord Failures

**Status**: Validated at medium and large profiles  
**Severity**: Low (informational)  
**Related Jira**: [COST-7605](https://redhat.atlassian.net/browse/COST-7605) (Backend bottleneck analysis)  
**Test**: PERF-VK-001 (`test_valkey_eviction.py`)

**Question**:
Does Valkey key eviction under memory pressure cause Celery chord failures during ingestion? Chords store intermediate results in Valkey DB1 (`celery-task-meta-*` keys). If in-flight chord members are evicted, the chord callback never fires and processing stalls.

**Method**:
Constrained Valkey `maxmemory` via runtime `CONFIG SET` (no pod restart) and ran single-source ingestion while monitoring evictions, task failures, and chord errors via a background `ValkeyMonitor` thread. Tested at both medium (37 MB, 8 files) and large (68 MB, 13 files) workloads.

**Medium profile** (2 clusters, 49 nodes, 30-day data):

| Variant | maxmemory | Evictions | Throughput | Task Failures | Chord Errors |
|---------|-----------|-----------|------------|---------------|--------------|
| 512Mi-default | 536 MB | 0 | 2.41 MB/s | 0 | 0 |
| 2Mi-tight | 2.0 MB | 0 | 2.40 MB/s | 0 | 0 |
| baseline+10K | ~2.0 MB | **11 (0.1/s)** | 2.40 MB/s | 0 | 0 |

**Large profile** (7 clusters, 133 nodes, 30-day data):

| Variant | maxmemory | Evictions | Throughput | Task Failures | Chord Errors |
|---------|-----------|-----------|------------|---------------|--------------|
| 512Mi-default | 536 MB | 0 | 2.23 MB/s | 0 | 0 |
| 4Mi | 4.0 MB | 0 | 2.23 MB/s | 0 | 0 |
| 2Mi-tight | 2.0 MB | 0 | 2.23 MB/s | 0 | 0 |
| baseline+10K | ~2.0 MB | **9 (0.1/s)** | 2.23 MB/s | 0 | 0 |

**Findings**:
1. **Evictions do not degrade throughput or cause failures** — end-to-end ingestion throughput is identical across all memory levels within each profile.
2. **The LRU policy protects active processing.** Evictions target stale completed-task results before in-flight chord members.
3. **The 512Mi default is dramatically oversized.** Baseline Valkey memory usage is ~1.8MB (process overhead). Even at large profile, a single source's task results consume ~49KB. The default provides >250x headroom.
4. **Large profile uses slightly more Valkey memory than medium** but still well within 2MB. The limiting factor for chord safety is concurrent source count, not data size.

**Implications for sizing**:
- The `allkeys-lru` policy (chart default) is the correct choice. A `noeviction` policy would cause Celery write failures (MISCONF errors) instead.
- Chord failure risk would require enough concurrent chords to fill Valkey memory with in-flight members — likely 50+ simultaneous sources.
- No change to default `valkey.maxMemory: 512MB` recommended — the headroom is cheap insurance.

---

### PERF-FINDING-027: PostgreSQL CPU Has Diminishing Returns Above 4000m

**Status**: Validated at medium and large profiles  
**Severity**: Low (informational)  
**Related Jira**: [COST-7605](https://redhat.atlassian.net/browse/COST-7605) (Backend bottleneck analysis)  
**Test**: PERF-DB-001 (`test_db_resource_sweep.py`)

**Question**:
At what CPU limit does PostgreSQL API query latency stop improving?

**Method**:
Patched the database StatefulSet CPU across three levels (2000m → 4000m → 8000m), ran representative API queries (report baseline and complex group-by), and captured `pg_stat_database` cache hit ratios. Tests ran against ingested medium/large profile data.

**Medium profile** (Koku DB ~73 MB):

| CPU Limit | Report Baseline p95 | Group-by p95 | Cache Hit Ratio |
|-----------|---------------------|--------------|-----------------|
| 2000m | 7.2ms | 3.3ms | 100% |
| 4000m | **4.7ms** | 4.3ms | 100% |
| 8000m | 6.2ms | 4.7ms | 100% |

**Large profile** (same dataset, large profile cluster config):

| CPU Limit | Report Baseline p95 | Group-by p95 | Cache Hit Ratio |
|-----------|---------------------|--------------|-----------------|
| 2000m | 5.6ms | 5.9ms | 100% |
| 4000m | **5.1ms** | **4.4ms** | 100% |
| 8000m | 4.9ms | **3.3ms** | 100% |

**Findings**:
1. **All latencies are sub-10ms** — PostgreSQL is not the API bottleneck at any tested CPU level. The dataset is small enough to fit entirely in shared_buffers (100% cache hit rate).
2. **2000m → 4000m shows a modest improvement** (~30% on report baseline at medium). This aligns with PostgreSQL's ability to parallelize query planning and background tasks.
3. **4000m → 8000m shows no consistent improvement** — some metrics improve slightly, others regress (likely measurement noise at sub-10ms latencies). Diminishing returns are clear.
4. **Cache hit ratio is 100% at all levels.** CPU is not constraining buffer management.

**Implications for sizing**:
- The current sizing guide recommendations (medium: 1000m/4000m, large: 2000m/4000m) are well-positioned. Going beyond 4000m limit provides no measurable benefit for API queries.
- For larger datasets that don't fit in shared_buffers, CPU matters more for sequential scans. This should be re-validated at xlarge with 90-day retention.

---

### PERF-FINDING-028: PostgreSQL Memory Provides No Latency Benefit Above 4Gi for Current Workloads

**Status**: Validated at medium and large profiles  
**Severity**: Low (informational)  
**Related Jira**: [COST-7605](https://redhat.atlassian.net/browse/COST-7605) (Backend bottleneck analysis)  
**Test**: PERF-DB-002 (`test_db_resource_sweep.py`)

**Question**:
Does increasing PostgreSQL memory (and thus `shared_buffers`) improve API query latency?

**Method**:
Patched the database StatefulSet memory across three levels (4Gi → 8Gi → 16Gi), which causes PostgreSQL to auto-tune `shared_buffers` to ~25% of available memory. Ran representative API queries and monitored buffer cache statistics.

**Medium profile** (`shared_buffers` auto-tuned):

| Memory Limit | shared_buffers | Report Baseline p95 | Group-by p95 | Cache Hit Ratio | Blocks Read |
|--------------|----------------|---------------------|--------------|-----------------|-------------|
| 4Gi | 1 GB | 6.7ms | 6.7ms | 100% | 0 |
| 8Gi | 2 GB | 7.0ms | 5.8ms | 100% | 0 |
| 16Gi | 4 GB | 5.9ms | 6.6ms | 100% | 0 |

**Large profile**:

| Memory Limit | shared_buffers | Report Baseline p95 | Group-by p95 | Cache Hit Ratio | Blocks Read |
|--------------|----------------|---------------------|--------------|-----------------|-------------|
| 4Gi | 1 GB | 7.3ms | 5.0ms | 100% | 0 |
| 8Gi | 2 GB | 5.0ms | 7.5ms | 100% | 0 |
| 16Gi | 4 GB | 4.5ms | 6.6ms | 100% | 0 |

**Findings**:
1. **Zero disk reads at all memory levels.** The Koku database (~73 MB) fits entirely in even the smallest shared_buffers (1 GB). No cache misses occur.
2. **No consistent latency improvement** with more memory — the ~2ms variation across levels is measurement noise, not a real signal.
3. **Memory sizing should be driven by dataset size, not query latency.** The benefit of larger `shared_buffers` only materializes when the working set exceeds the buffer pool.

**Implications for sizing**:
- For deployments with ≤90-day retention at medium scale, 4Gi memory limit is sufficient. The current sizing guide (medium: 2Gi/8Gi) provides comfortable headroom.
- For large/xlarge deployments with extended retention, 8–16Gi is appropriate as insurance against working set growth, not because current data demands it.
- Re-validate at xlarge profile with 90-day data retention to find the memory level where cache misses begin.

---

## Kafka Scaling Analysis (COST-7638)

### PERF-FINDING-029: Kafka Has Massive Throughput Headroom — Not the Bottleneck

**Status**: Validated at medium, large, and xlarge profiles
**Severity**: Informational — sizing confirmation
**Category**: Kafka throughput
**Jira**: [COST-7638](https://redhat.atlassian.net/browse/COST-7638)

**Method**: Uploaded 2–30 concurrent sources (baseline NISE data, 7 days) while a
background `KafkaMonitor` polled consumer group lag every 5 seconds. Measured at
medium (4 levels), large (5 levels), and xlarge (7 levels) profiles.

| Concurrency | Peak Consumer Lag | Broker-0 CPU | Processing Wait |
|-------------|-------------------|--------------|-----------------|
| 2 sources | 0 | ~1200m | 1.3s |
| 5 sources | 0 | ~800m | 142s |
| 10 sources | 0 | ~850m | 221s |
| 15 sources | 0 | ~850m | 461–516s |
| 20 sources | 0 | ~520–800m | 487–559s |
| 25 sources | 0 | 1432m | 510s |
| 30 sources | 0 | 825m | 474s |

Consumer lag remained at **zero across all concurrency levels and profiles**.
Kafka consumed and delivered messages faster than the 5-second sampling interval.
Only broker-0 served traffic; brokers 1–2 were idle replicas (~35m CPU).

Disk usage: 1,320–1,428 MB out of 10,240 MB (13%) — flat across all runs.

**Where the bottleneck actually is**: Processing wait time scales linearly with
concurrency. The Celery worker pipeline (ocp/summary workers processing in
PostgreSQL) is the constraint, not Kafka or the listener.

**Implications for sizing**:
- Single-broker Kafka is sufficient through xlarge workloads
- The 3-broker default deployment provides availability, not throughput
- Partition count does not matter until the downstream pipeline can saturate Kafka
- Kafka resource recommendations in the sizing guide are conservative — a single
  broker at 500m/2Gi CPU/memory handles all validated workloads

---

## Processing Pipeline Analysis (COST-7598)

### PERF-FINDING-031: Celery Worker Replica Scaling Shows Diminishing Returns — DB is the Bottleneck

**Status**: Validated at medium, large, and xlarge profiles
**Severity**: Informational — sizing confirmation
**Related Jira**: [COST-7598](https://redhat.atlassian.net/browse/COST-7598) (Pipeline analysis), [COST-6163](https://redhat.atlassian.net/browse/COST-6163) (Worker autoscaling)
**Test**: PERF-CEL-001 (`test_celery_scaling.py`)

**Method**:
Scaled each worker component (OCP, summary, listener) through 1/2/3-4 replicas
independently while holding others constant. Ran 5 (medium), 8 (large), or
10 (xlarge) concurrent source uploads at each level, measuring processing time,
DB CPU, per-pod worker CPU, and `pg_stat_database` cache statistics.

**Medium profile** (5 concurrent sources):

| Component | 1 replica | 2 replicas | 4 replicas | Observation |
|-----------|-----------|------------|------------|-------------|
| **Listener** | 294m CPU | 2m + 293m | 2m + 295m + 291m | Work concentrates on 1 pod; others idle |
| **OCP worker** | 60-130m CPU | 3-55m + 6m | 3-17m across 4 pods | Load distributes but individual CPU drops |
| **Summary worker** | 3-4m CPU | 3-6m + 3m | 3-4m across 4 pods | CPU consistently low — not compute-bound |
| **DB CPU** | 147-254m | 105-294m | 70-267m | Varies but doesn't drop — remains the constant |

Processing times across 3 runs were consistent within ±10%, with no significant
improvement beyond 2 replicas for any component at medium workload.

**Large profile** (8 concurrent sources):

| Component | 1 replica | 2 replicas | 3-4 replicas | Observation |
|-----------|-----------|------------|--------------|-------------|
| **Listener** | 287m CPU | 2m + 298m | 2m + 276m + 2m | Same single-pod concentration |
| **OCP worker** | 17m CPU | 2m + 2m | 4m + 3m + 4m + 42m | Low CPU even at 1 replica |
| **Summary worker** | 3m CPU | 6m + 4m | 4m + 3m + 3m + 4m | Consistently idle |
| **DB CPU** | 104-384m | 156-327m | 136-176m | Higher baseline than medium |

**XLarge profile** (10 concurrent sources):

| Component | 1 replica | 2 replicas | 3-4 replicas | Observation |
|-----------|-----------|------------|--------------|-------------|
| **Listener** | 291m CPU | 2m + 290m | 294m + 2m + 2m | Same single-pod concentration |
| **OCP worker** | 49m CPU | 3m + 3m | 4m + 3m + 47m + 6m | Load shifts between pods |
| **Summary worker** | 4m CPU | 3m + 4m | 3m + 3m + 4m + 4m | Consistently idle |
| **DB CPU** | 132-245m | 35-150m | 54-135m | DB CPU decreases with more workers |

**Findings**:
1. **Listener work concentrates on a single pod.** Even at 3 replicas, one
   listener handles ~99% of active CPU while others sit at 2m. Kafka's consumer
   group assignment directs partitions to one consumer when partition count
   equals or exceeds replica count.
2. **OCP worker scaling distributes load but doesn't reduce total time.** At
   4 replicas, each pod uses less CPU individually, but end-to-end processing
   time is unchanged — the pipeline is serialized by DB writes.
3. **Summary workers are consistently idle** at medium workload. 3-4m CPU at
   any replica count confirms summarization is not the bottleneck.
4. **DB CPU is the constant.** Regardless of worker replica count, DB CPU
   ranges 70-300m during processing. The bottleneck is PostgreSQL write
   throughput, not worker compute.
5. **Zero deadlocks** across all configurations and all runs.

**Implications for sizing**:
- 2 replicas per component is the sweet spot for medium workloads — provides
  availability without wasted resources
- Scaling beyond 2 replicas only helps if Kafka partitions are also increased
  (for listener) or if workload is compute-bound (not the case today)
- DB resource increases (FINDING-027/028) are more impactful than adding workers

---

### PERF-FINDING-032: Sequential Ingestion Batches 19-24% Faster — Warm PostgreSQL Cache Confirmed

**Status**: Validated at medium, large, and xlarge profiles
**Severity**: Low (informational)
**Related Jira**: [COST-7598](https://redhat.atlassian.net/browse/COST-7598) (Pipeline analysis)
**Test**: PERF-CEL-003 (`test_celery_scaling.py`)

**Method**:
Ran two sequential ingestion batches (5 concurrent sources each, unique cluster
IDs and source names) on the same deployment without restarts. Captured
`pg_stat_database` deltas, cache hit ratios, active PostgreSQL connections, and
worker pod ages before and after each batch.

**Medium profile** (3 runs, 5 concurrent sources):

| Metric | Batch 1 | Batch 2 | Delta |
|--------|---------|---------|-------|
| Processing time | 120-172s | 95-172s | **0-24% faster** |
| Blocks hit | 327K-362K | 375K-439K | +7-34% more cache hits |
| Blocks read (disk) | 3-1,288 | 7-667 | Variable |
| Cache hit ratio | 0.9964-1.0 | 0.9982-1.0 | Near-perfect both batches |
| Transactions | 11.3K-11.6K | 12.9K-14.4K | +12-25% more transactions |
| Worker pod ages | Consistent | +~110s older | Expected |

**Large profile** (8 concurrent sources):

| Metric | Batch 1 | Batch 2 | Delta |
|--------|---------|---------|-------|
| Processing time | ~215s | ~163s | **~24% faster** |
| Blocks hit | 568K | 728K | +28% more cache hits |
| Blocks read (disk) | 806 | 64 | **-92% disk reads** |
| Cache hit ratio | 0.9986 | 0.9999 | Near-perfect |
| Transactions | 19.7K | 23.3K | +19% more transactions |
| DB CPU | 191m | 84m | -56% (more efficient) |
| Worker pods | 12 | 12 | Stable |

The large profile shows a stronger warm-cache effect: 92% fewer disk reads and
DB CPU drops 56% in batch 2 as cached pages eliminate I/O overhead.

**XLarge profile** (10 concurrent sources):

| Metric | Batch 1 | Batch 2 | Delta |
|--------|---------|---------|-------|
| Blocks hit | 799K | 867K | +9% |
| Blocks read (disk) | 1,498 | 19 | **-99% disk reads** |
| Cache hit ratio | 0.9981 | 1.0000 | Perfect |
| Transactions | 26.0K | 28.7K | +11% more transactions |
| DB CPU | 265m | 250m | -6% (marginal) |

The warm-cache disk read reduction is most dramatic at xlarge (99%), confirming
the effect scales with workload size.

**Findings**:
1. **The speedup is real but variable** — ranges from 0% to 24% across runs.
   Not a guaranteed improvement; depends on PostgreSQL's internal buffer state.
2. **Cache hit ratio is near-perfect in both batches.** The improvement is not
   from avoiding disk reads (which are already near-zero). Rather, it's from
   PostgreSQL's query planner having warmer statistics and index page caches.
3. **Batch 2 processes more transactions** per second — higher throughput,
   not just fewer cache misses.
4. **Each batch uses unique resources** (cluster IDs, source names, NISE data),
   confirming the speedup is from shared infrastructure warmth (index pages,
   connection pools, planner caches), not row-level cache hits.
5. **Worker pod ages show no restarts** between batches — the warm state
   is entirely from PostgreSQL, not from worker process caching.

**Implications for performance testing**:
- First-batch results on a warm cluster should be treated as representative
  of steady-state performance (cache hit ratio is already 99.6%+)
- The 0-24% variance between sequential batches is within normal noise for
  sub-200s processing times
- A fresh deployment with no prior ingestion may show a more significant
  first-batch penalty, but this was not tested (deferred to future work)

---

### PERF-FINDING-033: OCP Workers Survive at 256Mi Memory — No OOM Through XLarge Workload

**Status**: Validated at medium, large, and xlarge profiles
**Severity**: Low (informational)
**Related Jira**: [COST-7598](https://redhat.atlassian.net/browse/COST-7598) (Pipeline analysis)
**Test**: PERF-CEL-002 (`test_celery_scaling.py`)

**Method**:
Constrained OCP worker memory to 256Mi and 512Mi (below the chart default of
512Mi/1Gi), ran ingestion, and monitored for OOMKill events and pod restarts.

**Medium profile** (5 concurrent sources):

| Memory Limit | Processing Time | OOM Events | Restarts | Verdict |
|--------------|-----------------|------------|----------|---------|
| 256Mi | ~57s | 0 | 0 | Survived |
| 512Mi | ~57s | 0 | 0 | Survived |

**Large profile** (8 concurrent sources):

| Memory Limit | Processing Time | OOM Events | Restarts | Verdict |
|--------------|-----------------|------------|----------|---------|
| 128Mi | — | — | — | **Skipped** (pod failed to start) |
| 256Mi | ~57s | 0 | 0 | Survived |
| 512Mi | ~103s | 0 | 0 | Survived |

**XLarge profile** (10 concurrent sources):

| Memory Limit | Processing Time | OOM Events | Restarts | Verdict |
|--------------|-----------------|------------|----------|---------|
| 128Mi | — | — | — | **Skipped** (pod failed to start) |
| 256Mi | ~88s | 0 | 0 | Survived |
| 512Mi | ~62s | 0 | 0 | Survived |

**Findings**:
1. **OCP workers survived at 256Mi** across all profiles (medium, large,
   xlarge) — no OOMKill events, no restarts.
2. **The OOM floor is between 128Mi and 256Mi.** At both large and xlarge
   profiles, the 128Mi patch caused the pod to fail to start.
3. **Processing time is similar** at 256Mi vs 512Mi — memory is not
   constraining throughput for these workload sizes.

**Implications for sizing**:
- The current chart default (512Mi/1Gi) provides comfortable headroom
- Customers with tight cluster budgets could run at 256Mi/512Mi for small
  deployments, but this is not recommended — larger datasets or concurrent
  processing will push memory higher
- The 128Mi floor at large/xlarge confirms the practical minimum is ~256Mi

---

### PERF-FINDING-034: Multi-Replica Kruize Confirms Single-Replica Recommendation

**Status**: Validated at medium, large, and xlarge profiles
**Severity**: Low (informational)
**Related Jira**: [COST-7598](https://redhat.atlassian.net/browse/COST-7598) (Pipeline analysis)
**Test**: PERF-ROS-001 (`test_celery_scaling.py`)

**Method**:
Scaled Kruize from 1 to 2 replicas and captured per-pod CPU utilization via
`oc adm top pod`, with a metrics-server polling loop to ensure both pods
were reporting before measurement.

**All profiles**:

| Profile | Replicas | Pod Count | CPU per Pod | Observation |
|---------|----------|-----------|-------------|-------------|
| Medium | 1 | 1 | 3m (idle) | Baseline |
| Medium | 2 | 2 | 3m + 6m (1209m startup) | Second pod starts successfully |
| Large | 1 | 1 | 3m (idle) | Baseline |
| Large | 2 | 2 | 3m + 1124m (startup) | Second pod starts successfully |
| XLarge | 1 | 1 | 3m (idle) | Baseline |
| XLarge | 2 | 2 | 3m + 4m | Both pods idle (no startup burst) |

**Findings**:
1. **Second replica starts and becomes ready** — pod scheduling works correctly.
2. **Idle CPU at both replicas** — without driving ROS workload, this test
   validates infrastructure readiness, not throughput impact.
3. **Combined with FINDING-004**: Kruize throughput degrades with multiple
   replicas due to DB contention. This test confirms the second pod can start
   (ruling out scheduling issues) while FINDING-004 provides the throughput
   data showing degradation.

**Recommendation**: Keep Kruize at 1 replica per FINDING-004. The multi-replica
validation confirms the recommendation is not due to a scheduling limitation.

---

## Chart Default Validation (COST-7599)

### PERF-FINDING-035: Chart Defaults (Small Profile) Pass Without Runtime Overrides

**Status**: Validated — small passes; medium partially fails (expected)
**Severity**: Informational — validates COST-7599 goal
**Related Jira**: [COST-7599](https://redhat.atlassian.net/browse/COST-7599) (Validate tuned configuration), [COST-7618](https://redhat.atlassian.net/browse/COST-7618) (Sizing guide)

**Background**:
COST-7599 embedded the small-profile sizing findings into `values.yaml` chart
defaults, so customers get optimal sizing out of the box. The key changes from
the old defaults:

| Setting | Old Default | New Default (Small) |
|---------|-------------|---------------------|
| Listener replicas | 1 | 2 |
| OCP worker replicas | 1 | 2 |
| Summary worker replicas | 1 | 2 |
| Database CPU | 100m/500m | 500m/2000m |
| Database memory | 256Mi/512Mi | 1Gi/4Gi |
| HAProxy timeout | 30s | 180s |
| Envoy ingress timeout | 30s | 180s |
| Envoy per-try timeout | 10s | 60s |
| Listener CPU | 150m/300m | 150m/300m (unchanged — see VTC-001a) |

**Method**:
Deployed with `USE_LOCAL_CHART=true` to apply new defaults, then ran
performance tests with `--skip-profile-config --listener-cpu none` to ensure
zero runtime modifications. This tests exactly what a customer gets from a
fresh `helm install`.

**Small profile** (`api,ingestion` with `--skip-profile-config --listener-cpu none`):

| Suite | Tests | Passed | Duration |
|-------|-------|--------|----------|
| API | 16 | 16 | ~2 min |
| Ingestion | 9 | 9 | ~40 min |
| **Total** | **25** | **25** | **41m 51s** |

Key metrics at chart defaults:
- API p95 latency: 14-20ms (report baseline), 51-1457ms (concurrent users)
- Ingestion: 8.85 MB in 16s, 93 MB in 78s, 68 MB in 31s
- Upload throughput: 7.7-16.9 MB/s
- Processing window: within 6-hour window
- Valkey: 2.7-7.7 MB memory, 21-35 cmds/sec

**Medium profile** (`api,ingestion` with `--skip-profile-config --listener-cpu none`):

| Suite | Tests | Passed | Failed | Duration |
|-------|-------|--------|--------|----------|
| API | 16 | 16 | 0 | ~2 min |
| Ingestion | 12 | 10 | **2** | ~2h 13m |
| **Total** | **28** | **26** | **2** | **2h 15m 45s** |

Failed tests (both listener CPU starvation):
- `ing_002[90-days]`: 90-day burst data never processed — "manifest not yet
  visible" for 1504s until timeout. Listener at 300m cannot keep up with
  ~140MB single-source burst.
- `ing_004[100MB]`: 101.49 MB upload succeeded (93.6 MB/s) but processing
  never started — "manifest not yet visible" for 3302s until timeout.

Tests that passed at medium with chart defaults:
- All 16 API tests (no listener CPU dependency)
- `ing_001[medium]`, `ing_002[30-days]`, `ing_002[60-days]` — smaller data volumes
- `ing_003[2,5,10]` — concurrent small uploads
- `ing_004[50MB]` — 68MB file processed in 31s
- `ing_005` — high-frequency streaming uploads
- `ing_006[medium]` — 2-cluster processing window

**VTC-001a: Listener CPU Characterization**:

The initial hypothesis was that listener CPU was the sole medium-scale
bottleneck. Three follow-up runs tested this — first by raising listener CPU
with chart defaults, then by applying the full medium profile without a listener
CPU boost:

| Run | Listener CPU | Other Resources | Result |
|-----|-------------|-----------------|--------|
| Baseline | 300m (default) | Chart defaults only (`--skip-profile-config`) | 26/28 — 2 failures |
| +500m | 500m | Chart defaults only (`--skip-profile-config`) | 26/28 — same 2 failures |
| +1000m | 1000m | Chart defaults only (`--skip-profile-config`) | 26/28 — same 2 failures |
| **Full medium** | **300m (default)** | **Full medium profile** (`--listener-cpu none`) | **28/28 PASS** |

Raising listener CPU from 300m to 500m and 1000m with all other settings at
chart defaults did **not** fix the failures. Applying the full medium profile
overrides (2x worker replicas, increased worker CPU/memory, 200MB upload limit,
1Gi/2Gi ingress memory) with listener CPU at the chart default 300m fixed
everything.

**Full medium profile key metrics** (listener CPU = 300m):
- `ing_002[90-days]`: PASS — 5.5 min, 138.6 MB at 15.9 MB/s, 47s processing
- `ing_004[100MB]`: PASS — 4.2 min, 101.4 MB at 15.4 MB/s, 31s processing
- All 28 tests passed, 0 KPI violations, 42 min total

**Findings**:
1. **Chart defaults work for small-profile workloads.** All 25 tests pass
   with zero runtime overrides — customers get a working system out of the box.
2. **Chart defaults partially work at medium scale.** 26/28 tests pass with
   chart defaults alone — the failures are from large data volumes that need
   more downstream resources, not more listener CPU.
3. **Listener CPU is NOT the medium-scale bottleneck.** Runs #85 and #86
   proved that raising listener CPU to 500m and 1000m (3× the default) with
   chart-default workers/replicas does not fix the failures. The full medium run proved
   the converse: full medium profile resources with default 300m listener CPU
   passes everything.
4. **The actual bottleneck is the downstream pipeline.** Single-replica workers
   at low CPU/memory, combined with limited upload buffer and ingress memory,
   cannot drain the processing queue fast enough for large burst workloads.
   When workers are scaled (2x replicas, 1000m CPU limit, 2Gi memory) and
   upload limits are raised (200MB), the pipeline keeps up at 300m listener CPU.
5. **The threshold is upload + worker provisioning, not listener CPU.** The
   68MB upload (ing_004[50MB]) processed in 31s at chart defaults. The 101MB
   upload (ing_004[100MB]) stalled at chart defaults but processed in 31s
   with medium profile resources — same listener CPU both times.

**Implications**:
- The chart default listener CPU (150m/300m) is correct for all validated
  workloads through medium profile, provided other resources are appropriately
  sized.
- Medium-profile customers need the replica and resource overrides (workers,
  ingress, upload limits), NOT a listener CPU increase.
- Listener CPU boost (500m+) provides faster ingestion throughput as a
  performance optimization for large/xlarge profiles and bulk operations,
  but is not a correctness requirement at medium scale.
- This corrects the initial reading of FINDING-002: while the listener does
  run at high CPU utilization during bulk ingestion, this throttling slows
  ingestion but does not prevent it when the downstream pipeline has adequate
  resources.

---

## RBAC Authorization Testing (COST-7643)

### PERF-FINDING-037: RBAC Authorization is Not a Bottleneck — Profile-Invariant Performance

**Status**: Validated at medium and large profiles
**Severity**: Informational — sizing confirmation
**Category**: Authorization overhead
**Jira**: [COST-7643](https://redhat.atlassian.net/browse/COST-7643)

**Method**: Six tests (RBAC-001 through RBAC-006) measured insights-rbac
authorization performance: baseline latency isolation, cache effectiveness,
concurrent load at 5 levels (1/5/10/20/50), multi-org scaling (1/5/10 tenants),
replica scaling (1/2/3 replicas), and latency under active ingestion load.

**Results by profile**:

| Test | Metric | Medium | Large |
|------|--------|--------|-------|
| **RBAC-001** | RBAC share of API latency | p50=121%, p95=166% | p50=112%, p95=90% |
| **RBAC-002** | Cache speedup | 0.7x | 0.8x |
| **RBAC-003[c=1]** | Throughput / p50 | 254.8 req/s / 3.7ms | 254.1 req/s / 3.8ms |
| **RBAC-003[c=50]** | Throughput / p50 | 291.4 req/s / 147.8ms | 289.1 req/s / 150.9ms |
| **RBAC-004[10]** | p50 latency | 3.8ms | 3.8ms |
| **RBAC-005** | Replica scaling benefit | None | +3%/-1% (noise) |
| **RBAC-006** | Degradation under ingestion | p50=6.9x, p95=14.4x | p50=5.9x, p95=11.3x |

All tests: 0 errors, PG cache hit ratio 1.0, RBAC pod CPU 1-2m.

**Findings**:
1. **RBAC performance is profile-invariant.** Throughput (~280 req/s) and latency
   characteristics are virtually identical between medium and large profiles.
2. **Replica scaling provides no benefit.** At 2 replicas, p95 improved 3%;
   at 3 replicas, p95 regressed 1%. The bottleneck (if one existed) would be
   the shared PostgreSQL instance, not RBAC compute.
3. **Tenant count has zero impact.** Latency at 1, 5, and 10 tenants is
   indistinguishable (p50: 3.3-3.8ms). RBAC queries are per-principal, not
   per-tenant.
4. **Valkey cache (DB 2) is never populated.** Koku's own 1-hour
   `CACHE_TIMEOUT` absorbs all repeated queries before they reach RBAC.
   The Valkey cache only matters when multiple Koku pods serve the same user.
5. **Degradation under ingestion is from PG contention, not RBAC.** The 6-14x
   latency increase during ingestion reflects shared PostgreSQL I/O load, not
   RBAC-specific overhead. The slightly lower degradation at large profile
   (11.3x vs 14.4x p95) is consistent with more worker replicas spreading
   the PG write load.

**Implications for sizing**:
- RBAC API at 1 replica with default resources (250m/500m CPU, 512Mi/1Gi memory)
  is sufficient through large profile. No scaling or tuning needed.
- The 280 req/s throughput ceiling (2 workers x 2 threads) far exceeds any
  realistic concurrent user count for on-premise deployments.
- SC-5 (replica scaling linearity) **failed** — this is a positive finding,
  confirming that RBAC is already operating within its single-replica capacity.

---

## Stress Testing Findings (COST-7627 + COST-7600)

### PERF-FINDING-036: Stress Ramp Breaking Points Scale with Profile

**Status**: Validated (medium, large, xlarge)
**Severity**: Informational
**Category**: Capacity planning

**Tests**: `PERF-STR-001` (ramp-to-failure), `PERF-STR-002` (backlog recovery)

**Breaking points by profile** (with `listener-cpu=max`):

| Profile | Last Good Step | Breaking Point | Reason |
|---------|---------------|---------------|--------|
| Medium | 50 sources | 75 sources | Step time exceeded MAX_STEP_TIME |
| Large | 75 sources | 100 sources | Step time exceeded MAX_STEP_TIME |
| XLarge | 100 sources | 150 sources | pytest timeout (processing still ongoing, not failure) |

**Key observations**:
- System never crashed or OOMed at any profile — breaking point is always
  "processing too slow", not "component failure".
- API remained responsive (4-10ms p50) throughout all ramp steps.
- Backlog recovery (STR-002) passed cleanly at all profiles — queues drain
  and API latencies return to baseline after overload stops.
- XLarge step 8 (150 sources) was still processing when the 2h pytest timeout
  fired — the system was making progress, just slowly.

**Suite split**: The `stress` suite was split into `stress_ramp` (STR-001) and
`stress_recovery` (STR-002) to allow running them independently. STR-001 is
the heavyweight long-running test; STR-002 is shorter and can reuse STR-001
results or fall back to defaults.

**Cleanup timeout**: Resource cleanup is now capped at `PERF_CLEANUP_TIMEOUT`
(default 900s / 15 min) to prevent pipeline hangs. During xlarge runs with
300+ tracked resources, cleanup was observed to hang indefinitely.

---

## Environment Issues

### PERF-FINDING-010: ODF Default Resources Exhaust Cluster Memory

**Status**: Mitigated  
**Severity**: Critical  
**Category**: Environment (not product)

**Problem**:
ODF 4.20 applies full upstream default resource requests. On a 3-worker cluster, ODF consumed ~61.5Gi (68%) in memory requests, leaving insufficient headroom for cost-onprem + Kafka.

**Remediation**: Patch `StorageCluster` and NooBaa resources to reduced targets. See auto-toolbox `odf_reduced_resources` flag.

---

### PERF-FINDING-019: Ceph OSD False-Full Cascade Under Load

**Status**: Documented  
**Severity**: Critical  
**Category**: Environment (not product)

**Problem**:
During medium-profile tests, Ceph reported `OSD_FULL` despite disks at 25-29% actual usage. This cascaded to all CephFS-backed pods (database, Kafka) via SELinux relabel failures.

**Recovery**: Restart OSD pods to clear false-full state, then restart affected workloads.

**Production Implication**: Clusters running cost-onprem at high utilization with CephFS may experience this failure mode. The false-full condition cascades to all CephFS-backed workloads.

---

## Soak / Long-Running Stability Testing

### PERF-FINDING-038: Transient S3 Upload Drops Fail Soak Iterations (No Retry)

**Status**: Fixed
**Severity**: Medium
**Category**: Test harness robustness (not a product issue)

**Tests**: `test_perf_soak_001_continuous_operation` (soak loop, `scripts/soak-loop.sh`)

**Problem**:
During the 72.6h soak run `soak-20260821-173101` (4h iterations, 18 total),
4 iterations (5, 7, 12, 14) failed with the identical signature:

```
AssertionError: 1 uploads failed
assert 1 == 0
```

The underlying error in every case was a single dropped S3 (NooBaa) upload
connection:

```
Upload N failed: Upload failed after 1 attempts:
('Connection aborted.', RemoteDisconnected('Remote end closed connection without response'))
```

Each affected iteration completed 15 of 16 uploads and all 48 queries
successfully — one transient connection drop was enough to fail the
iteration because the soak upload loop gives up after a single attempt
(no retry/backoff).

**Analysis**:
- Failures were sparse and non-contiguous (iters 5, 7, 12, 14) — consistent
  with intermittent connectivity, not a systematic regression.
- All other soak tests passed in every iteration: memory leak detection
  (002), disk usage (003), queue health (004).
- Cluster health was stable throughout: 0 pods not ready, 2 total pod
  restarts across 72.6h.

**Validation**:
The follow-up 24.2h run `soak-20260824-184917` (3h iterations, 8 total)
passed 8/8 with **0 failed uploads across all 96 uploads** and 0 failed
queries — the failure mode did not reproduce.

**Caveat**: The passing run had a shorter exposure window (24h vs 72.6h),
so the intermittent nature of the failure is not fully ruled out at longer
durations.

**Fix**: Add retry with backoff (e.g., 2–3 attempts) to the
soak upload loop for transient errors (`RemoteDisconnected`,
`Connection aborted`, timeouts) so a single dropped connection does not
fail a multi-hour stability iteration.

---

## Performance Baselines

Validated throughput, processing times, API latencies, and concurrent upload
results are maintained in the [Sizing Guide](sizing-guide.md#performance-baselines-validated).

---

## Jira Tracking

| Finding | Summary | Jira | Status |
|---------|---------|------|--------|
| FINDING-001 | HAProxy + Envoy timeouts 30s → 180s | [FLPATH-4091](https://redhat.atlassian.net/browse/FLPATH-4091) | Fixed (Closed) |
| FINDING-002 | Listener CPU ≥1000m for burst ingestion | [COST-7618](https://redhat.atlassian.net/browse/COST-7618), [COST-6993](https://redhat.atlassian.net/browse/COST-6993) | Documented |
| FINDING-003 | Scale replicas for concurrent source count | [COST-7598](https://redhat.atlassian.net/browse/COST-7598), [COST-6163](https://redhat.atlassian.net/browse/COST-6163) | Documented |
| FINDING-004 | Kruize at 1 replica; CPU is throughput lever | [COST-7722](https://redhat.atlassian.net/browse/COST-7722) | Documented |
| FINDING-006 | Kruize CPU 500m/1000m → 1000m/2000m | [FLPATH-4302](https://redhat.atlassian.net/browse/FLPATH-4302) | Fixed (Closed) |
| FINDING-011 | Envoy `request_timeout` removed | — | Fixed in chart |
| FINDING-013 | ROS processor dead-letter handling | [FLPATH-4428](https://redhat.atlassian.net/browse/FLPATH-4428) | Mitigated |
| FINDING-020 | Gateway ConfigMap checksum annotation | [FLPATH-4429](https://redhat.atlassian.net/browse/FLPATH-4429) | Mitigated |
| FINDING-021 | Ingress upload memory per profile | — | Fixed in perf profiles |
| FINDING-022 | Ingress dedicated resource block | [FLPATH-4430](https://redhat.atlassian.net/browse/FLPATH-4430) | Mitigated |
| FINDING-024 | Ingress multipart S3 upload | [FLPATH-4431](https://redhat.atlassian.net/browse/FLPATH-4431) | Product limitation |
| FINDING-025 | Worker CPU/memory sizing | [COST-7598](https://redhat.atlassian.net/browse/COST-7598), [COST-7618](https://redhat.atlassian.net/browse/COST-7618) | Fixed in perf profiles |
| FINDING-026 | Valkey evictions do not cause chord failures | [COST-7605](https://redhat.atlassian.net/browse/COST-7605) | Validated (medium + large) |
| FINDING-027 | PostgreSQL CPU diminishing returns above 4000m | [COST-7605](https://redhat.atlassian.net/browse/COST-7605) | Validated (medium + large) |
| FINDING-028 | PostgreSQL memory no benefit above 4Gi for current workloads | [COST-7605](https://redhat.atlassian.net/browse/COST-7605) | Validated (medium + large) |
| FINDING-029 | Kafka has massive throughput headroom — not the bottleneck | [COST-7638](https://redhat.atlassian.net/browse/COST-7638) | Validated (medium + large + xlarge) |
| FINDING-030 | Sequential batches ~49% faster — cache warmth suspected | [COST-7638](https://redhat.atlassian.net/browse/COST-7638) | Superseded by FINDING-032 |
| FINDING-031 | Worker replica scaling shows diminishing returns — DB is bottleneck | [COST-7598](https://redhat.atlassian.net/browse/COST-7598) | Validated (medium + large + xlarge) |
| FINDING-032 | Sequential batches 19-24% faster — warm PostgreSQL cache confirmed | [COST-7598](https://redhat.atlassian.net/browse/COST-7598) | Validated (medium + large + xlarge) |
| FINDING-033 | OCP workers survive at 256Mi — OOM floor at 128-256Mi | [COST-7598](https://redhat.atlassian.net/browse/COST-7598) | Validated (medium + large + xlarge) |
| FINDING-034 | Multi-replica Kruize confirms single-replica recommendation | [COST-7598](https://redhat.atlassian.net/browse/COST-7598) | Validated (medium + large + xlarge) |
| FINDING-035 | Chart defaults pass small; medium needs worker/ingress scaling, not listener CPU | [COST-7599](https://redhat.atlassian.net/browse/COST-7599) | Validated (VTC-001a complete) |
| FINDING-036 | Stress ramp breaking points scale with profile; system never crashes | [COST-7627](https://redhat.atlassian.net/browse/COST-7627), [COST-7600](https://redhat.atlassian.net/browse/COST-7600) | Validated (medium + large + xlarge) |
| FINDING-037 | RBAC authorization is not a bottleneck — profile-invariant at ~280 req/s | [COST-7643](https://redhat.atlassian.net/browse/COST-7643) | Validated (medium + large) |

**Parent epic**: [COST-7567](https://redhat.atlassian.net/browse/COST-7567) (CoP Performance Tuning & Hardware Sizing Guidelines)

---

_Last Updated: 2026-08-05_
