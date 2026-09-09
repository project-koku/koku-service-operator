# Cost On-Prem Sizing Guide

Sizing recommendations for Cost On-Prem deployments based on performance testing
results (FLPATH-4036, COST-7567). Profile definitions are derived from production
data analysis by Pau Garcia Quiles (April 2026) and validated through automated
performance runs on a 3-worker OCP 4.20 cluster (54 CPU / 183 Gi).

## Changelog

### COST-7599: Chart Defaults Updated to Small Profile (2026-07-23)

The following `values.yaml` defaults were changed based on performance testing
findings. A fresh `helm install` now produces a deployment sized for the small
profile without any overrides.

| Setting | Previous Default | New Default | Evidence |
|---------|-----------------|-------------|----------|
| Listener replicas | 1 | 2 | FINDING-003: concurrent source processing |
| OCP worker replicas | 1 | 2 | FINDING-031: worker scaling analysis |
| Summary worker replicas | 1 | 2 | FINDING-031: worker scaling analysis |
| Database CPU (req/lim) | 100m / 500m | 500m / 2000m | FINDING-027: CPU sweep at medium/large |
| Database memory (req/lim) | 256Mi / 512Mi | 1Gi / 4Gi | FINDING-028: memory sweep at medium/large |
| HAProxy route timeout | 30s | 180s | FINDING-001: large upload timeouts |
| Envoy ingress timeout | 30s | 180s | FINDING-001: large upload timeouts |
| Envoy per-try timeout | 10s | 60s | FINDING-001: large upload timeouts |
| Listener CPU (req/lim) | 150m / 300m | 150m / 300m (unchanged) | FINDING-035/VTC-001a: not the bottleneck |

**Migration jobs** were also hardened with robust PostgreSQL readiness checks
to prevent `BackoffLimitExceeded` failures during upgrades that change database
resources.

**Key insight (FINDING-035)**: Listener CPU at the chart default 300m is
sufficient for all workloads through medium profile. The medium-scale bottleneck
is the downstream pipeline (worker replicas, worker CPU/memory, ingress memory,
upload limits), not the listener. See [FINDINGS.md](./FINDINGS.md#perf-finding-035)
for the full VTC-001a characterization.

### COST-7618: Profile Values Overlays (2026-07-27)

Published declarative Helm overlays for each sizing profile under
[`cost-onprem/`](../../cost-onprem/). These are the deploy-time
equivalent of `apply_perf_profile_config()`, plus database sizing. Soft
recommendation for mapping overlays to a future operator CRD:
[operator-profile-crd-mapping.md](./operator-profile-crd-mapping.md).

---

## Quick Reference

| Profile | Clusters | Nodes | CPU Cores | Memory | % of Customers |
|---------|----------|-------|-----------|--------|----------------|
| Small | 1 | 15 | ~200 | ~1.1 TB | 37% |
| Medium | 2 | ~49 | ~544 | ~2.8 TB | 35% |
| Large | 7 | ~133 | ~1,964 | ~9.7 TB | 21% |
| XLarge | 23 | ~346 | ~6,954 | ~48.5 TB | 6% |
| Stress (P99) | 33 | ~1,072 | ~57,424 | ~137 TB | <1% |

**Validation status**: Small, medium, large, and xlarge profiles have been validated
with clean (0-failure) automated runs. Stress profiles have not yet been executed.

---

## Component Resource Recommendations

**Chart defaults = Small profile** (as of COST-7599). A fresh install with no
`values.yaml` overrides provides the small-profile resource allocations listed
below. Medium, large, and xlarge profiles require explicit overlays — see
[Helm Values Overlays](#helm-values-overlays). Performance test runs can apply
the same settings dynamically via `apply_perf_profile_config()`.

Kruize memory and Database resources should be set in `values.yaml` at
deployment time. All values have been validated through successful end-to-end
test suites at each profile level.

### Kruize (ROS Optimization Engine)

| Profile | Replicas | CPU Request | CPU Limit | Memory Request | Memory Limit |
|---------|----------|-------------|-----------|----------------|--------------|
| Small | 1 | 500m | 2000m | chart default | chart default |
| Medium | 1 | 500m | 2000m | chart default | chart default |
| Large | 1 | 500m | 2000m | 2Gi (rec.) | 4Gi (rec.) |
| XLarge | 1 | 1000m | 2000m | 2Gi (rec.) | 4Gi (rec.) |

Kruize memory is not dynamically adjusted by `apply_perf_profile_config()`.
For large/xlarge workloads, 2Gi/4Gi is recommended based on observed heap
pressure during experiment creation.

**Key finding (PERF-FINDING-004, PERF-FINDING-006)**: Kruize throughput is
CPU-bound, not connection-pool-bound. At the chart default of 500m/1000m,
experiment creation runs at ~8/min. At 1000m/2000m (xlarge), it reaches
**31 exp/min** — a 4x improvement. The CPU limit should always be at least
2000m to prevent liveness probe failures under load.

Always run Kruize as a **single replica** — scaling replicas increases DB
contention and degrades throughput.

### Koku Listener (Ingestion)

| Profile | Replicas | CPU Request | CPU Limit | Memory Request | Memory Limit |
|---------|----------|-------------|-----------|----------------|--------------|
| Small | 2 | 150m | 300m | 300Mi | 600Mi |
| Medium | 2 | 150m | 300m | 1Gi | 2Gi |
| Large | 3 | 150m | 300m | 2Gi | 4Gi |
| XLarge | 3 | 150m | 300m | 2Gi | 4Gi |

**Key finding (PERF-FINDING-002, PERF-FINDING-035 VTC-001a)**: The listener
runs at high CPU utilization during bulk ingestion (157% throttled at 300m).
However, VTC-001a characterization proved that **listener CPU is not the
medium-scale bottleneck** — the downstream pipeline (worker replicas, worker
CPU/memory, ingress memory, upload limits) is what determines whether bulk
workloads succeed or stall. All 28 medium-profile tests passed at the chart
default 300m listener CPU when medium profile resources were applied.

**Listener CPU sizing guidance**:
- **All workloads through medium profile**: Chart default 300m is sufficient
  when other resources are properly provisioned (see medium profile overrides)
- **Performance optimization for large/xlarge**: Raise to 1000m+ for faster
  ingestion throughput during bulk operations
- **Perf testing**: Use `--listener-cpu max` to remove CPU as a variable

Listener CPU is managed separately via the `--listener-cpu` flag and is not
part of `apply_perf_profile_config()`.

### Celery Workers

#### OCP Worker (Cost Processing)

| Profile | Replicas | CPU Request | CPU Limit | Memory Request | Memory Limit |
|---------|----------|-------------|-----------|----------------|--------------|
| Small | 2 | 250m | 500m | 512Mi | 1Gi |
| Medium | 2 | 250m | 1000m | 512Mi | 2Gi |
| Large | 3 | 500m | 1000m | 1Gi | 2Gi |
| XLarge | 3 | 1000m | 2000m | 2Gi | 4Gi |

#### Summary Worker

| Profile | Replicas | CPU Request | CPU Limit | Memory Request | Memory Limit |
|---------|----------|-------------|-----------|----------------|--------------|
| Small | 2 | 250m | 500m | chart default | chart default |
| Medium | 2 | 250m | 1000m | chart default | chart default |
| Large | 3 | 500m | 1000m | chart default | chart default |
| XLarge | 3 | 1000m | 2000m | chart default | chart default |

Summary worker memory is not dynamically adjusted — the chart default is
sufficient for all validated profiles.

**Key finding (PERF-FINDING-025)**: Boosting worker CPU from 250m/500m to
500m/1000m reduced end-to-end processing time by 15% on the large profile
(112 min → 97.6 min). XLarge uses 1000m/2000m to handle tag-based cost model
processing.

**Replica scaling (PERF-FINDING-031)**: Scaling beyond 2 replicas shows
diminishing returns at medium workloads. PostgreSQL write throughput is the
bottleneck, not worker compute. Listener work concentrates on a single pod
due to Kafka partition assignment. OCP workers distribute load across replicas
but total processing time is unchanged. Summary workers are consistently idle
at medium scale.

**OOM threshold (PERF-FINDING-033)**: OCP workers survived at 256Mi memory
with no OOMKill events at medium workload. The chart default (512Mi/1Gi) provides
comfortable headroom. Larger workloads may require more — large profile
validation pending.

### ROS Processor

| Profile | Replicas | Memory Request | Memory Limit |
|---------|----------|----------------|--------------|
| Small | 1 | 1Gi | 1Gi |
| Medium | 2 | 2Gi | 4Gi |
| Large | 3 | 2Gi | 4Gi |
| XLarge | 3 | 2Gi | 4Gi |

### Ingress (Upload Service)

| Profile | Max Upload Size | Max Upload Memory | App Memory Req/Lim |
|---------|-----------------|-------------------|--------------------|
| Small | 100 MB (default) | 32 MB (default) | 1Gi / 1Gi |
| Medium | 200 MB | 64 MB | 1Gi / 2Gi |
| Large | 500 MB | 128 MB | 2Gi / 4Gi |
| XLarge | 500 MB | 128 MB | 2Gi / 4Gi |

**Key finding (PERF-FINDING-022)**: The ingress pod shares the
`resources.application` memory block with other services. Large/concurrent
uploads cause OOM at the 1Gi default. Raising to 2Gi/4Gi for large profiles
resolves this, but a dedicated `resources.ingress` block would be cleaner.

**Key finding (PERF-FINDING-024)**: `insights-ingress-go` uses single-part
S3 uploads which fail for payloads >150 MB. This is an upstream code limitation,
not a resource constraint.

### Database (PostgreSQL)

| Profile | CPU Request | CPU Limit | Memory Request | Memory Limit | Storage |
|---------|-------------|-----------|----------------|--------------|---------|
| Small | 500m | 2000m | 1Gi | 4Gi | 10Gi |
| Medium | 1000m | 4000m | 2Gi | 8Gi | 50Gi |
| Large | 2000m | 4000m | 4Gi | 16Gi | 100Gi |
| XLarge | 4000m | 8000m | 8Gi | 32Gi | 200Gi |

Database resources are not dynamically adjusted by `apply_perf_profile_config()` —
they should be set via `values.yaml` at deployment time.

### PostgreSQL Tuning Guide

The chart deploys a single PostgreSQL StatefulSet. PostgreSQL auto-tunes
`shared_buffers` to approximately 25% of the container memory limit — no
manual tuning is required. The recommendations above are validated by CPU
and memory sweeps at medium and large profiles (COST-7605).

#### CPU Sizing

| CPU Limit | Report Baseline p95 | Complex Group-by p95 | Cache Hit Ratio |
|-----------|---------------------|----------------------|-----------------|
| 2000m | 5.6–7.2ms | 3.3–5.9ms | 100% |
| 4000m | 4.7–5.1ms | 4.3–4.4ms | 100% |
| 8000m | 4.9–6.2ms | 3.3–4.7ms | 100% |

(Ranges span medium and large profile results — PERF-FINDING-027)

**Guidance**:
- All latencies are **sub-10ms** at every tested CPU level. PostgreSQL is not
  the API bottleneck for typical workloads.
- **2000m → 4000m** provides a modest improvement (~30% on report queries).
  This comes from PostgreSQL's ability to parallelize query planning and
  background tasks (autovacuum, checkpointing).
- **4000m → 8000m** shows no consistent improvement — the variation is
  measurement noise at sub-10ms latencies. Diminishing returns are clear.
- The chart default (500m/2000m for small, 1000m/4000m for medium) is
  well-positioned. Going beyond 4000m limit provides no measurable API
  benefit for current workload sizes.
- For deployments with **extended retention (90-day+)** or **concurrent
  heavy queries**, re-evaluate at 4000m–8000m as the working set grows.

#### Memory Sizing

| Memory Limit | shared_buffers (auto) | Report Baseline p95 | Blocks Read (disk) | Cache Hit Ratio |
|--------------|-----------------------|---------------------|--------------------|-----------------|
| 4Gi | ~1 GB | 5.9–7.3ms | 0 | 100% |
| 8Gi | ~2 GB | 5.0–7.0ms | 0 | 100% |
| 16Gi | ~4 GB | 4.5–5.9ms | 0 | 100% |

(Ranges span medium and large profile results — PERF-FINDING-028)

**Guidance**:
- **Zero disk reads** at all memory levels. The Koku database fits entirely
  in `shared_buffers` at 1 GB (the smallest tested level). No cache misses
  occur with current workloads.
- **No consistent latency improvement** with more memory — the ~2ms variation
  is measurement noise, not a real signal.
- **Size memory for the dataset, not for query speed.** The benefit of larger
  `shared_buffers` only materializes when the working set exceeds the buffer
  pool.
- The chart default (1Gi/4Gi for small) provides comfortable headroom.
  Medium (2Gi/8Gi) and large (4Gi/16Gi) are sized for dataset growth, not
  because current data demands it.
- **Rule of thumb**: Ensure `shared_buffers` (≈25% of memory limit) exceeds
  the total database size. Check with:

```sql
SELECT pg_size_pretty(pg_database_size('koku'));
```

#### When to Increase Database Resources

| Scenario | CPU Recommendation | Memory Recommendation |
|----------|--------------------|-----------------------|
| ≤2 clusters, 30-day retention | 500m/2000m (default) | 1Gi/4Gi (default) |
| 2-7 clusters, 30-day retention | 1000m/4000m | 2Gi/8Gi |
| 7+ clusters or 90-day retention | 2000m/4000m | 4Gi/16Gi |
| Heavy concurrent API queries | 4000m/8000m | 8Gi/32Gi |
| Extended retention (90-day+) at scale | 4000m/8000m | 8Gi/32Gi+ |

For extended retention deployments where the database exceeds shared_buffers,
monitor cache hit ratio:

```sql
SELECT
  round(blks_hit::numeric / nullif(blks_hit + blks_read, 0) * 100, 2) AS cache_hit_pct,
  blks_hit, blks_read
FROM pg_stat_database
WHERE datname = 'koku';
```

If `cache_hit_pct` drops below 99%, increase memory to bring `shared_buffers`
above the working set. If query latency increases but cache hit is high,
increase CPU.

---

## Gateway Configuration

### Timeout Settings

The chart default gateway timeout is 180s (updated from 30s in COST-7599).
Large/xlarge profiles should increase to 600s for bulk uploads.

| Profile | HAProxy Timeout | Envoy Route Timeout | Envoy Per-Try Timeout |
|---------|-----------------|---------------------|----------------------|
| Small | 180s (default) | 180s (default) | 60s (default) |
| Medium | 180s (default) | 180s (default) | 180s |
| Large | 600s | 600s | 300s |
| XLarge | 600s | 600s | 300s |

**Configuration** (see large/xlarge overlays for full context):

```yaml
jwtAuth:
  envoy:
    ingressTimeout: "600s"
    ingressPerTryTimeout: "300s"

gatewayRoute:
  annotations:
    haproxy.router.openshift.io/timeout: "600s"
```

**Key finding (PERF-FINDING-001, PERF-FINDING-020)**: After `helm upgrade`,
the Envoy gateway pod must be restarted for timeout changes to take effect.
Envoy reads config at startup only. A `checksum/envoy-config` pod annotation
is recommended (see FINDING-020).

---

## Performance Baselines (Validated)

### Upload Throughput

| File Size | Profile | Upload Throughput | Processing Time | Total Time |
|-----------|---------|-------------------|-----------------|------------|
| ~13 KB | baseline | — | <1s | <1s |
| ~48 MB | medium | 1-2 MB/s | 5-10 min | 6-11 min |
| ~67 MB | large | 1.1-1.9 MB/s | 7-20 min | 8-22 min |
| ~70 MB | xlarge | 14.5 MB/s | 31s | ~3 min |
| ~102 MB | xlarge | 14.2 MB/s | 31s | ~3 min |

The xlarge throughput improvement (14+ MB/s vs 1-3 MB/s) is from the higher
worker CPU allocation (1000m/2000m).

### API Response Times (P95)

| Endpoint | Small | Medium | Large/XLarge | Notes |
|----------|-------|--------|--------------|-------|
| GET /sources/ | < 500ms | < 500ms | < 1s | List all sources |
| GET /reports/openshift/costs/ | < 2s | < 3s | < 5s | Monthly aggregation |
| GET /reports/openshift/costs/ (group_by) | < 5s | < 10s | < 25s | Complex queries |
| GET /recommendations/openshift | < 2s | < 2s | < 3s | ROS recommendations |

### Kruize Experiment Creation

| Profile | CPU Req/Lim | Workloads | Rate (exp/min) | Peak Memory | Restarts |
|---------|-------------|-----------|----------------|-------------|----------|
| Medium | 500m/1000m | 160 | 8.6 | 723 MB | 0 |
| XLarge | 1000m/2000m | 600 | 31.0 | 415 MB | 0 |

### Concurrent Upload Scaling

| Concurrent Sources | Replicas (listener/ocp/summary) | Profile | Result |
|--------------------|--------------------------------|---------|--------|
| 2 | 1/1/1 | baseline | PASS |
| 5 | 2/2/2 | medium | PASS |
| 10 | 2/2/2 | medium | PASS |
| 10 | 3/3/3 | xlarge | PASS |

---

## Storage Requirements

### PostgreSQL Storage

| Profile | Initial Size | 30-day Retention | 90-day Retention |
|---------|--------------|------------------|------------------|
| Small | 5 GB | 10-15 GB | 30-45 GB |
| Medium | 10 GB | 25-40 GB | 75-120 GB |
| Large | 25 GB | 75-125 GB | 225-375 GB |
| XLarge | 50 GB | 150-250 GB | 450-750 GB |

### S3/MinIO Storage

| Component | Small | Medium | Large | XLarge |
|-----------|-------|--------|-------|--------|
| koku-bucket | 10 GB | 50 GB | 100 GB | 250 GB |
| insights-upload-perma | 5 GB | 10 GB | 20 GB | 50 GB |
| ros-data | 2 GB | 5 GB | 10 GB | 25 GB |

### Kafka Storage

| Profile | Retention Period | Recommended Storage |
|---------|------------------|---------------------|
| Small | 7 days | 10 GB |
| Medium | 7 days | 10 GB |
| Large | 7 days | 10 GB |
| XLarge | 7 days | 10 GB |

10 Gi per broker is sufficient for all validated profiles (through xlarge).
The default 100 Gi is sized for sustained multi-day production ingestion.
For perf testing clusters, 10 Gi saves 270 Gi of ODF PV capacity.

### Kafka Broker Resources (COST-7638)

| Tier | Brokers | CPU Req/Limit | Memory Req/Limit | Disk | Platform Partitions | Announce Partitions |
|------|---------|---------------|-------------------|------|---------------------|---------------------|
| Small | 1 | 250m/1 | 1Gi/2Gi | 20Gi | 1 | 1 |
| Medium | 1 | 500m/2 | 2Gi/4Gi | 50Gi | 3 | 3 |
| Large | 3 | 1/2 | 2Gi/4Gi | 100Gi | 6 | 6 |
| XLarge | 3 | 2/4 | 4Gi/8Gi | 200Gi | 12 | 12 |

> **Note**: Single-broker tiers (Small/Medium) are validated — PERF-KAF-001
> confirmed zero consumer lag through 30 concurrent sources on a single broker
> (FINDING-029). The multi-broker recommendations for Large/XLarge provide
> availability, not throughput, and require a cluster with multiple Kafka replicas
> to validate HA behavior.

---

## Helm Values Overlays

Declarative profile overlays live in
`cost-onprem/` alongside `values.yaml`. They use the same chart keys as
`apply_perf_profile_config()` and add database sizing (which the perf script
does not change at runtime).

| Profile | Overlay | Notes |
|---------|---------|-------|
| Small | [values-small.yaml](../../cost-onprem/values-small.yaml) | ≡ chart defaults (COST-7599); optional `-f` |
| Medium | [values-medium.yaml](../../cost-onprem/values-medium.yaml) | Required for medium workloads |
| Large | [values-large.yaml](../../cost-onprem/values-large.yaml) | Restart gateway after upgrade (FINDING-020) |
| XLarge | [values-xlarge.yaml](../../cost-onprem/values-xlarge.yaml) | Same replicas as large; higher worker CPU/memory |

```bash
# Medium example (manual)
helm upgrade --install cost-onprem ./cost-onprem \
  -n cost-onprem \
  -f openshift-values.yaml \
  -f cost-onprem/values-medium.yaml \
  --wait

# Via deploy script
./scripts/deploy-test-cost-onprem.sh --namespace cost-onprem --sizing-profile medium
```

**Small validated** with `--skip-profile-config --listener-cpu none` (pure
chart defaults, no runtime overrides). Medium through xlarge validated via
profile-config performance runs.

For a soft recommendation on how these overlays map to a future Cost
Management OpenShift Operator CRD, see
[operator-profile-crd-mapping.md](./operator-profile-crd-mapping.md).

---

## Known Limitations

- **Single-part S3 upload ceiling** (PERF-FINDING-024): Uploads >150 MB fail
  against NooBaa/Ceph. Upstream `insights-ingress-go` enhancement needed.
- **Gateway ConfigMap not hot-reloaded** (PERF-FINDING-020): `helm upgrade`
  timeout changes require gateway pod restart.
- **Ingress shares `resources.application`** (PERF-FINDING-022): Cannot size
  ingress memory independently from other services.
- **Listener CPU is a throughput lever, not a correctness requirement**
  (VTC-001a, FINDING-035): The chart default 300m listener CPU is sufficient
  for all workloads through medium profile when other resources are properly
  provisioned. Raising listener CPU improves bulk ingestion speed at large/xlarge
  but is not required for the pipeline to complete.
- **RBAC authorization is not a sizing concern** (FINDING-037): insights-rbac
  at 1 replica with default resources handles ~280 req/s with sub-5ms p50 latency.
  Performance is profile-invariant (identical at medium and large). No replica
  scaling or resource tuning needed.
- **Stress profiles (P99, max) not yet validated**: Profiles beyond xlarge
  have not been tested.

---

## Related

- [Performance Testing Plan](./performance-testing-plan.md) — test methodology and profiles
- [FINDINGS.md](./FINDINGS.md) — detailed product findings and evidence
- [TEST-MATRIX.md](./TEST-MATRIX.md) — test coverage matrix
- [OBSERVABILITY.md](./OBSERVABILITY.md) — metrics collection infrastructure
- [Profile overlays](../../cost-onprem/) — `values-{small,medium,large,xlarge}.yaml`
- [Operator CRD mapping](./operator-profile-crd-mapping.md) — soft recommendation for a future operator

---

_Based on FLPATH-4036 / COST-7567 performance testing. Last updated: 2026-08-05._
