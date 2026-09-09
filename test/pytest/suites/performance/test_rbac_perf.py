"""
RBAC Authorization Performance Tests (COST-7643).

Isolate and measure the insights-rbac contribution to API latency. Every
Koku API call delegates a permission check to the RBAC service; if RBAC is
slow, all API latencies degrade. These tests quantify the overhead and
identify whether RBAC is a bottleneck.

Test IDs:
- PERF-RBAC-001: Baseline RBAC latency isolation
- PERF-RBAC-002: Cache effectiveness (cold vs warm)
- PERF-RBAC-003: Concurrent authorization load (parametrized)
- PERF-RBAC-004: Multi-org scaling (parametrized, conditional)
- PERF-RBAC-005: Replica scaling (1→2→3 replicas under load)
- PERF-RBAC-006: RBAC latency under ingestion load

Usage:
    # All RBAC perf tests
    ./scripts/deploy-test-cost-onprem.sh --perf-only --perf-profile medium --perf-suite rbac

    # Direct pytest
    PERF_PROFILE=medium pytest -m "performance and rbac_perf" tests/suites/performance/
"""

import re
import time
import threading
from concurrent.futures import ThreadPoolExecutor
from datetime import datetime, timedelta, timezone
from typing import Any, Dict, List

import pytest
import requests as _requests

from conftest import ClusterConfig, DatabaseConfig, obtain_jwt_token
from e2e_helpers import generate_cluster_id, register_source
from utils import (
    exec_in_pod,
    exec_in_pod_raw,
    execute_db_query,
    get_pod_by_label,
    run_oc_command,
)

from .data_classes import PerformanceResult
from .helpers import (
    PerfResultCollector,
    PerfTimer,
    create_authenticated_session,
    generate_and_upload_data,
)
from .k8s_helpers import (
    calculate_percentiles,
    capture_pg_stats,
    diff_pg_stats,
    get_deployment_replicas,
    scale_deployment,
)


# =============================================================================
# Helpers
# =============================================================================


def _rbac_access_url(gateway_url: str) -> str:
    """RBAC permission check endpoint via the Envoy gateway.

    Tests run outside the cluster (Jenkins hypervisor), so ClusterIP
    service DNS is not resolvable. The gateway routes ``/api/rbac/``
    to the RBAC backend, giving us an externally reachable path that
    still isolates RBAC latency from the Koku API path.
    """
    return f"{gateway_url.rstrip('/')}/api/rbac/v1/access/?application=cost-management"


RBAC_VALKEY_DB = 2


def _flush_rbac_cache(namespace: str) -> int:
    """Flush all keys from Valkey DB used by RBAC (Django cache DB 2).

    RBAC's Django cache uses ``redis://…/2`` with its own key format
    (version-prefixed, e.g. ``:1:key``), so we flush the entire DB
    rather than pattern-matching on a prefix.
    """
    pod = get_pod_by_label(namespace, "app.kubernetes.io/component=cache")
    if not pod:
        return -1

    count_result = exec_in_pod(
        namespace, pod,
        ["valkey-cli", "-n", str(RBAC_VALKEY_DB), "DBSIZE"],
        timeout=15,
    )
    before_count = 0
    if count_result:
        try:
            before_count = int(count_result.strip())
        except ValueError:
            pass

    result = exec_in_pod(
        namespace, pod,
        ["valkey-cli", "-n", str(RBAC_VALKEY_DB), "FLUSHDB"],
        timeout=30,
    )
    if result is None:
        return -1
    return before_count


def _count_rbac_cache_keys(namespace: str) -> int:
    """Count keys in Valkey DB used by RBAC (DB 2)."""
    pod = get_pod_by_label(namespace, "app.kubernetes.io/component=cache")
    if not pod:
        return -1

    result = exec_in_pod(
        namespace, pod,
        ["valkey-cli", "-n", str(RBAC_VALKEY_DB), "DBSIZE"],
        timeout=15,
    )
    if result is None:
        return -1
    try:
        return int(result.strip())
    except ValueError:
        return -1


def _measure_latency(
    session: _requests.Session,
    url: str,
    n: int = 100,
    timeout: float = 30.0,
) -> Dict[str, Any]:
    """Make N sequential GET requests and return success latencies + error count.

    Error latencies (timeouts, connection failures, 5xx) are excluded from the
    returned list so they don't skew percentile calculations.
    """
    latencies: List[float] = []
    errors = 0
    for _ in range(n):
        start = time.time()
        try:
            resp = session.get(url, timeout=timeout)
            elapsed = time.time() - start
            if resp.status_code >= 500:
                errors += 1
                print(f"  [latency] {url} returned {resp.status_code}")
            else:
                latencies.append(elapsed)
        except _requests.RequestException as e:
            errors += 1
            print(f"  [latency] {url} error: {e}")
    return {"latencies": latencies, "errors": errors, "total": n}


def _measure_latency_concurrent(
    session_factory,
    url: str,
    concurrency: int,
    duration_s: float = 60.0,
    timeout: float = 30.0,
) -> Dict[str, Any]:
    """Run concurrent GET requests for duration_s seconds.

    session_factory: callable that returns a new requests.Session (one per thread).
    Returns percentile stats + throughput.
    """
    stop_event = threading.Event()
    all_latencies: List[float] = []
    error_count = 0
    lock = threading.Lock()

    def _worker():
        nonlocal error_count
        sess = session_factory()
        local_latencies = []
        local_errors = 0
        while not stop_event.is_set():
            start = time.time()
            try:
                resp = sess.get(url, timeout=timeout)
                elapsed = time.time() - start
                if resp.status_code >= 500:
                    local_errors += 1
                else:
                    local_latencies.append(elapsed)
            except _requests.RequestException:
                local_errors += 1
        with lock:
            all_latencies.extend(local_latencies)
            error_count += local_errors

    threads = []
    for _ in range(concurrency):
        t = threading.Thread(target=_worker, daemon=True)
        t.start()
        threads.append(t)

    time.sleep(duration_s)
    stop_event.set()
    # Allow up to the HTTP timeout for in-flight requests to complete
    for t in threads:
        t.join(timeout=timeout + 5)

    total = len(all_latencies) + error_count
    stats = calculate_percentiles(all_latencies, errors=error_count)
    stats["total_requests"] = total
    stats["successful_requests"] = len(all_latencies)
    stats["requests_per_second"] = round(total / max(duration_s, 0.1), 1)
    stats["concurrency"] = concurrency
    return stats


# =============================================================================
# Multi-org tenant provisioning
# =============================================================================

_PERF_ORG_PREFIX = "rbac-perf-org"
_PERF_ACCT_PREFIX = "rbac-perf-acct"


def _count_rbac_tenants(namespace: str, db_pod: str) -> int:
    """Return the current tenant count in the RBAC database."""
    rows = execute_db_query(
        namespace, db_pod, "costonprem_rbac", "postgres",
        "SELECT count(*) FROM api_tenant;",
    )
    if rows and rows[0]:
        try:
            return int(rows[0][0])
        except (ValueError, IndexError):
            pass
    return 0


def _provision_rbac_tenants(
    namespace: str,
    target_count: int,
    db_pod: str,
) -> List[str]:
    """Provision ephemeral RBAC tenants via the Django ORM.

    Creates ``api_tenant`` rows directly through the RBAC management shell.
    This is the most reliable approach — the gateway trigger and
    ``bootstrap_tenants`` command do not consistently insert ``api_tenant``
    rows for freshly created org_ids.

    Returns a list of org_id strings for cleanup.
    """
    current = _count_rbac_tenants(namespace, db_pod)
    needed = target_count - current
    if needed <= 0:
        return []

    rbac_pod = get_pod_by_label(namespace, "app.kubernetes.io/component=rbac-api")
    if not rbac_pod:
        pytest.skip("RBAC API pod not found — cannot provision tenants")

    provisioned_orgs: List[str] = []
    print(f"  Provisioning {needed} tenant(s) via RBAC ORM (current={current}, target={target_count})...")

    # Batch-create all tenants in one exec call
    org_ids = [f"{_PERF_ORG_PREFIX}-{current + i + 1:04d}" for i in range(needed)]
    orm_script = "from api.models import Tenant\n"
    for org_id in org_ids:
        orm_script += (
            f"t, c = Tenant.objects.get_or_create("
            f"org_id={org_id!r}, "
            f"defaults={{'tenant_name': {org_id!r}, 'ready': True}})\n"
            f"print(f'  {{\"created\" if c else \"exists\"}}: org_id={{t.org_id!r}} id={{t.id}}')\n"
        )
    orm_script += f"print(f'Total tenants: {{Tenant.objects.count()}}')\n"

    try:
        result = exec_in_pod_raw(
            namespace, rbac_pod,
            ["python", "/opt/rbac/rbac/manage.py", "shell", "-c", orm_script],
            timeout=60,
        )
        if result.stdout:
            for line in result.stdout.strip().splitlines():
                if not line.startswith("GLITCHTIP") and "imported automatically" not in line:
                    print(f"    {line}")
                    # Track which orgs were actually created
                    m = re.search(r"created: org_id='([^']+)'", line)
                    if m:
                        provisioned_orgs.append(m.group(1))
        if result.returncode != 0 and result.stderr:
            print(f"  WARN: ORM stderr: {result.stderr.strip()[:200]}")
    except Exception as e:
        print(f"  WARN: Tenant ORM provisioning failed: {e}")
        return []

    new_count = _count_rbac_tenants(namespace, db_pod)
    print(f"  Tenant count after provisioning: {new_count}")
    return provisioned_orgs


def _cleanup_rbac_tenants(
    namespace: str,
    provisioned_orgs: List[str],
    db_pod: str,
) -> None:
    """Remove ephemeral tenants created by _provision_rbac_tenants."""
    if not provisioned_orgs:
        return

    rbac_pod = get_pod_by_label(namespace, "app.kubernetes.io/component=rbac-api")
    if not rbac_pod:
        print("  WARN: RBAC API pod not found — cannot clean up tenants")
        return

    quoted = ", ".join(f"'{o}'" for o in provisioned_orgs)
    orm_script = (
        "from api.models import Tenant\n"
        f"deleted, _ = Tenant.objects.filter(org_id__in=[{quoted}]).delete()\n"
        f"print(f'Deleted {{deleted}} tenant(s), remaining: {{Tenant.objects.count()}}')\n"
    )
    try:
        result = exec_in_pod_raw(
            namespace, rbac_pod,
            ["python", "/opt/rbac/rbac/manage.py", "shell", "-c", orm_script],
            timeout=30,
        )
        if result.stdout:
            for line in result.stdout.strip().splitlines():
                if not line.startswith("GLITCHTIP") and "imported automatically" not in line:
                    print(f"    {line}")
    except Exception as e:
        print(f"  WARN: Tenant cleanup failed: {e}")


# =============================================================================
# Test Class
# =============================================================================


@pytest.mark.performance
@pytest.mark.rbac_perf
class TestRBACPerf:
    """RBAC authorization performance tests (COST-7643)."""

    @pytest.fixture(autouse=True)
    def setup(self, cluster_config: ClusterConfig, keycloak_config):
        self.namespace = cluster_config.namespace
        self.helm_release = cluster_config.helm_release_name
        self._keycloak_config = keycloak_config
        self._cluster_config = cluster_config

    def _create_session(self) -> _requests.Session:
        return create_authenticated_session(self._keycloak_config)

    # -----------------------------------------------------------------
    # RBAC-001: Baseline latency isolation
    # -----------------------------------------------------------------

    @pytest.mark.timeout(300)
    def test_perf_rbac_001_baseline_isolation(
        self,
        cluster_config: ClusterConfig,
        database_config: DatabaseConfig,
        gateway_url: str,
        perf_timer: PerfTimer,
        perf_result: PerformanceResult,
        perf_collector: PerfResultCollector,
        keycloak_config,
    ):
        """PERF-RBAC-001: Measure RBAC's contribution to API latency.

        Compares direct RBAC access-check latency against end-to-end Koku
        API latency to determine what percentage of total latency comes
        from RBAC permission checks.
        """
        print(f"\n{'='*72}")
        print("PERF-RBAC-001: Baseline RBAC Latency Isolation")
        print(f"{'='*72}\n")

        session = self._create_session()
        rbac_access = _rbac_access_url(gateway_url)
        koku_reports = f"{gateway_url.rstrip('/')}/api/cost-management/v1/reports/openshift/costs/"

        # Warm up: a few requests to prime caches
        for _ in range(5):
            session.get(rbac_access, timeout=30)
            session.get(koku_reports, timeout=30)

        # Capture PG stats during measurement
        pg_before = capture_pg_stats(
            self.namespace,
            database_config.pod_name,
            database_config.database,
            database_config.user,
        )

        # Measure direct RBAC latency (100 sequential calls)
        print("Measuring direct RBAC latency (100 calls)...")
        rbac_result = _measure_latency(session, rbac_access, n=100)
        rbac_stats = calculate_percentiles(rbac_result["latencies"])
        print(f"  RBAC direct: p50={rbac_stats['p50']*1000:.1f}ms "
              f"p95={rbac_stats['p95']*1000:.1f}ms "
              f"p99={rbac_stats['p99']*1000:.1f}ms "
              f"(errors={rbac_result['errors']})")

        # Measure end-to-end Koku API latency (100 sequential calls)
        print("Measuring end-to-end Koku API latency (100 calls)...")
        koku_result = _measure_latency(session, koku_reports, n=100)
        koku_stats = calculate_percentiles(koku_result["latencies"])
        print(f"  Koku API:    p50={koku_stats['p50']*1000:.1f}ms "
              f"p95={koku_stats['p95']*1000:.1f}ms "
              f"p99={koku_stats['p99']*1000:.1f}ms")

        pg_after = capture_pg_stats(
            self.namespace,
            database_config.pod_name,
            database_config.database,
            database_config.user,
        )
        pg_delta = diff_pg_stats(pg_before, pg_after)

        # Calculate RBAC's share
        rbac_pct_p50 = (rbac_stats["p50"] / koku_stats["p50"] * 100) if koku_stats["p50"] > 0 else 0
        rbac_pct_p95 = (rbac_stats["p95"] / koku_stats["p95"] * 100) if koku_stats["p95"] > 0 else 0

        print(f"\n{'='*72}")
        print("RBAC-001 SUMMARY")
        print(f"{'='*72}")
        print(f"RBAC share of API latency: p50={rbac_pct_p50:.0f}%, p95={rbac_pct_p95:.0f}%")
        print(f"PG commits during measurement: {pg_delta.get('xact_commit_delta', '?')}")
        print(f"PG cache hit ratio: {pg_delta.get('cache_hit_ratio', '?')}")

        perf_result.test_id = "PERF-RBAC-001"
        perf_result.metrics = {
            "rbac_direct": rbac_stats,
            "koku_api": koku_stats,
            "rbac_share_pct_p50": round(rbac_pct_p50, 1),
            "rbac_share_pct_p95": round(rbac_pct_p95, 1),
            "pg_stats": pg_delta,
        }
        perf_result.passed = True
        perf_collector.add_result(perf_result)

    # -----------------------------------------------------------------
    # RBAC-002: Cache effectiveness
    # -----------------------------------------------------------------

    @pytest.mark.timeout(300)
    def test_perf_rbac_002_cache_effectiveness(
        self,
        cluster_config: ClusterConfig,
        gateway_url: str,
        perf_timer: PerfTimer,
        perf_result: PerformanceResult,
        perf_collector: PerfResultCollector,
        keycloak_config,
    ):
        """PERF-RBAC-002: Determine whether the RBAC Valkey cache provides measurable latency reduction.

        Flushes RBAC keys from Valkey DB 2, measures cold (miss) latency,
        then measures warm (hit) latency. A speedup >1x indicates the
        cache is effective; ~1x or below indicates Koku's own upstream
        cache (CACHE_TIMEOUT) absorbs repeated queries before they
        reach Valkey.
        """
        print(f"\n{'='*72}")
        print("PERF-RBAC-002: Cache Effectiveness")
        print(f"{'='*72}\n")

        session = self._create_session()
        rbac_access = _rbac_access_url(gateway_url)

        # Flush RBAC cache (Valkey DB 2)
        before_count = _flush_rbac_cache(self.namespace)
        print(f"Flushed Valkey DB {RBAC_VALKEY_DB} ({before_count} keys)")

        if before_count < 0:
            pytest.skip("Could not flush Valkey RBAC cache")

        # Cold (cache miss) measurement
        print("Measuring cold latency (cache miss, 50 calls)...")
        cold_result = _measure_latency(session, rbac_access, n=50)
        cold_stats = calculate_percentiles(cold_result["latencies"])
        print(f"  Cold: p50={cold_stats['p50']*1000:.1f}ms "
              f"p95={cold_stats['p95']*1000:.1f}ms")

        cache_keys_after_cold = _count_rbac_cache_keys(self.namespace)
        print(f"  Cache keys after cold run: {cache_keys_after_cold}")

        # Warm (cache hit) measurement — cache should now be populated
        print("Measuring warm latency (cache hit, 50 calls)...")
        warm_result = _measure_latency(session, rbac_access, n=50)
        warm_stats = calculate_percentiles(warm_result["latencies"])
        print(f"  Warm: p50={warm_stats['p50']*1000:.1f}ms "
              f"p95={warm_stats['p95']*1000:.1f}ms")

        # Calculate speedup
        speedup_p50 = cold_stats["p50"] / warm_stats["p50"] if warm_stats["p50"] > 0 else 0
        speedup_p95 = cold_stats["p95"] / warm_stats["p95"] if warm_stats["p95"] > 0 else 0
        delta_ms = (cold_stats["p50"] - warm_stats["p50"]) * 1000

        print(f"\n{'='*72}")
        print("RBAC-002 SUMMARY")
        print(f"{'='*72}")
        print(f"Cache speedup: p50={speedup_p50:.1f}×, p95={speedup_p95:.1f}×")
        print(f"Absolute delta (p50): {delta_ms:.1f}ms")
        print(f"Cache keys populated: {cache_keys_after_cold}")

        perf_result.test_id = "PERF-RBAC-002"
        perf_result.metrics = {
            "cold": cold_stats,
            "warm": warm_stats,
            "speedup_p50": round(speedup_p50, 2),
            "speedup_p95": round(speedup_p95, 2),
            "delta_ms_p50": round(delta_ms, 1),
            "cache_keys": cache_keys_after_cold,
        }
        perf_result.passed = True
        perf_collector.add_result(perf_result)

    # -----------------------------------------------------------------
    # RBAC-003: Concurrent load
    # -----------------------------------------------------------------

    @pytest.mark.timeout(600)
    @pytest.mark.parametrize("concurrency", [1, 5, 10, 20, 50])
    def test_perf_rbac_003_concurrent_auth(
        self,
        concurrency: int,
        cluster_config: ClusterConfig,
        database_config: DatabaseConfig,
        gateway_url: str,
        perf_timer: PerfTimer,
        perf_result: PerformanceResult,
        perf_collector: PerfResultCollector,
        keycloak_config,
    ):
        """PERF-RBAC-003: Measure RBAC throughput at varying concurrency.

        Runs N threads for 60 seconds, each hitting the RBAC access
        endpoint. Identifies the concurrency level where latency
        degrades >2× baseline (concurrency=1).
        """
        print(f"\n{'='*72}")
        print(f"PERF-RBAC-003: Concurrent Auth Load (concurrency={concurrency})")
        print(f"{'='*72}\n")

        rbac_access = _rbac_access_url(gateway_url)

        pg_before = capture_pg_stats(
            self.namespace,
            database_config.pod_name,
            database_config.database,
            database_config.user,
        )

        stats = _measure_latency_concurrent(
            session_factory=self._create_session,
            url=rbac_access,
            concurrency=concurrency,
            duration_s=60.0,
        )

        pg_after = capture_pg_stats(
            self.namespace,
            database_config.pod_name,
            database_config.database,
            database_config.user,
        )
        pg_delta = diff_pg_stats(pg_before, pg_after)

        # Get RBAC pod CPU/memory usage
        rbac_pod = get_pod_by_label(self.namespace, "app.kubernetes.io/component=rbac-api")
        pod_metrics = {}
        if rbac_pod:
            result = run_oc_command(
                ["adm", "top", "pod", rbac_pod, "-n", self.namespace, "--no-headers"],
                check=False,
            )
            if result.returncode == 0 and result.stdout.strip():
                parts = result.stdout.strip().split()
                if len(parts) >= 3:
                    pod_metrics = {"cpu": parts[1], "memory": parts[2]}

        print(f"\n{'='*72}")
        print(f"RBAC-003 SUMMARY (concurrency={concurrency})")
        print(f"{'='*72}")
        print(f"Requests: {stats['total_requests']} in 60s "
              f"({stats['requests_per_second']} req/s)")
        print(f"Latency: p50={stats['p50']*1000:.1f}ms "
              f"p95={stats['p95']*1000:.1f}ms "
              f"p99={stats['p99']*1000:.1f}ms")
        print(f"Errors: {stats.get('errors', 0)}")
        if pod_metrics:
            print(f"RBAC pod: CPU={pod_metrics.get('cpu', '?')}, "
                  f"Memory={pod_metrics.get('memory', '?')}")
        print(f"PG commits: {pg_delta.get('xact_commit_delta', '?')}, "
              f"cache hit: {pg_delta.get('cache_hit_ratio', '?')}")

        error_rate = stats.get("errors", 0) / max(stats["total_requests"], 1)
        perf_result.test_id = f"PERF-RBAC-003[{concurrency}]"
        perf_result.metrics = {
            "concurrency": concurrency,
            "latency": stats,
            "rbac_pod": pod_metrics,
            "pg_stats": pg_delta,
            "error_rate": round(error_rate, 4),
        }
        perf_result.passed = error_rate < 0.05
        perf_collector.add_result(perf_result)

        assert error_rate < 0.05, (
            f"RBAC-003[{concurrency}]: error rate {error_rate:.1%} exceeds 5% "
            f"({stats.get('errors', 0)}/{stats['total_requests']} requests)"
        )

    # -----------------------------------------------------------------
    # RBAC-004: Multi-org scaling (conditional)
    # -----------------------------------------------------------------

    @pytest.mark.timeout(600)
    @pytest.mark.parametrize("org_count", [1, 5, 10])
    def test_perf_rbac_004_multi_org_scaling(
        self,
        org_count: int,
        cluster_config: ClusterConfig,
        gateway_url: str,
        perf_timer: PerfTimer,
        perf_result: PerformanceResult,
        perf_collector: PerfResultCollector,
        keycloak_config,
    ):
        """PERF-RBAC-004: Measure RBAC latency with varying org count.

        Provisions ephemeral tenants (via Keycloak + gateway trigger +
        bootstrap_tenants) up to the target count, measures access-check
        latency, then cleans up. If RBAC queries are properly scoped
        per-tenant, latency should be flat regardless of org count.
        """
        print(f"\n{'='*72}")
        print(f"PERF-RBAC-004: Multi-Org Scaling Check (target={org_count})")
        print(f"{'='*72}\n")

        db_pod = get_pod_by_label(self.namespace, "app.kubernetes.io/component=database")
        if not db_pod:
            pytest.skip("Database pod not found")

        current_tenants = _count_rbac_tenants(self.namespace, db_pod)
        print(f"Current tenant count in RBAC DB: {current_tenants}")

        provisioned_orgs: List[str] = []
        try:
            if current_tenants < org_count:
                provisioned_orgs = _provision_rbac_tenants(
                    self.namespace, org_count, db_pod,
                )
                current_tenants = _count_rbac_tenants(self.namespace, db_pod)
                if current_tenants < org_count:
                    pytest.skip(
                        f"Provisioning reached {current_tenants} tenants, "
                        f"still short of target {org_count}"
                    )

            session = self._create_session()
            rbac_access = _rbac_access_url(gateway_url)

            print(f"Measuring RBAC latency with {current_tenants} tenants (100 calls)...")
            lat_result = _measure_latency(session, rbac_access, n=100)
            stats = calculate_percentiles(lat_result["latencies"])

            table_sizes = {}
            for table in ["api_tenant", "api_principal", "management_group",
                          "management_policy", "management_role"]:
                size_result = execute_db_query(
                    self.namespace, db_pod,
                    "costonprem_rbac", "postgres",
                    f"SELECT count(*) FROM {table};",
                )
                if size_result and size_result[0]:
                    try:
                        table_sizes[table] = int(size_result[0][0])
                    except (ValueError, IndexError):
                        pass

            print(f"\n{'='*72}")
            print(f"RBAC-004 SUMMARY (tenants={current_tenants}, target={org_count})")
            print(f"{'='*72}")
            print(f"Latency: p50={stats['p50']*1000:.1f}ms "
                  f"p95={stats['p95']*1000:.1f}ms "
                  f"p99={stats['p99']*1000:.1f}ms")
            print(f"RBAC table sizes: {table_sizes}")
            if provisioned_orgs:
                print(f"Provisioned {len(provisioned_orgs)} ephemeral tenant(s)")

            perf_result.test_id = f"PERF-RBAC-004[{org_count}]"
            perf_result.metrics = {
                "org_count_target": org_count,
                "actual_tenants": current_tenants,
                "provisioned_count": len(provisioned_orgs),
                "latency": stats,
                "table_sizes": table_sizes,
            }
            perf_result.passed = True
            perf_collector.add_result(perf_result)
        finally:
            if provisioned_orgs:
                print(f"\nCleaning up {len(provisioned_orgs)} ephemeral tenant(s)...")
                _cleanup_rbac_tenants(
                    self.namespace, provisioned_orgs, db_pod,
                )
                final_count = _count_rbac_tenants(self.namespace, db_pod)
                print(f"Tenant count after cleanup: {final_count}")

    # -----------------------------------------------------------------
    # RBAC-005: Replica scaling
    # -----------------------------------------------------------------

    @pytest.mark.timeout(900)
    def test_perf_rbac_005_replica_scaling(
        self,
        cluster_config: ClusterConfig,
        gateway_url: str,
        perf_timer: PerfTimer,
        perf_result: PerformanceResult,
        perf_collector: PerfResultCollector,
        keycloak_config,
    ):
        """PERF-RBAC-005: Determine if scaling RBAC replicas improves throughput.

        Runs concurrent load (concurrency=20) at 1, 2, and 3 RBAC API replicas.
        Compares p95 latencies and throughput to determine scaling linearity.
        Restores original replica count on completion.
        """
        print(f"\n{'='*72}")
        print("PERF-RBAC-005: RBAC Replica Scaling")
        print(f"{'='*72}\n")

        rbac_deploy = f"{self.helm_release}-rbac-api"
        original_replicas = get_deployment_replicas(self.namespace, rbac_deploy)
        print(f"Original RBAC replica count: {original_replicas}")

        rbac_access = _rbac_access_url(gateway_url)
        concurrency = 20
        duration = 60.0
        results_by_replicas = {}

        try:
            for replica_count in [1, 2, 3]:
                print(f"\n--- Scaling RBAC to {replica_count} replica(s) ---")
                if not scale_deployment(self.namespace, rbac_deploy, replica_count):
                    print(f"  WARN: Failed to scale to {replica_count}, skipping")
                    continue

                result = run_oc_command(
                    ["rollout", "status", f"deployment/{rbac_deploy}",
                     "-n", self.namespace, "--timeout=60s"],
                    check=False, timeout=90,
                )
                if result.returncode != 0:
                    print(f"  WARN: Rollout not ready after 60s, proceeding anyway")

                print(f"  Running concurrent load (concurrency={concurrency}, "
                      f"duration={duration}s)...")
                stats = _measure_latency_concurrent(
                    session_factory=self._create_session,
                    url=rbac_access,
                    concurrency=concurrency,
                    duration_s=duration,
                )

                pod_metrics = []
                result = run_oc_command(
                    ["adm", "top", "pod",
                     "-l", "app.kubernetes.io/component=rbac-api",
                     "-n", self.namespace, "--no-headers"],
                    check=False,
                )
                if result.returncode == 0 and result.stdout.strip():
                    for line in result.stdout.strip().splitlines():
                        parts = line.split()
                        if len(parts) >= 3:
                            pod_metrics.append({
                                "pod": parts[0],
                                "cpu": parts[1],
                                "memory": parts[2],
                            })

                results_by_replicas[replica_count] = {
                    "latency": stats,
                    "pod_metrics": pod_metrics,
                }

                print(f"  {replica_count} replica(s): "
                      f"{stats['total_requests']} reqs, "
                      f"{stats['requests_per_second']} req/s, "
                      f"p50={stats['p50']*1000:.1f}ms, "
                      f"p95={stats['p95']*1000:.1f}ms, "
                      f"errors={stats.get('errors', 0)}")
        finally:
            print(f"\nRestoring RBAC to {original_replicas} replica(s)...")
            scale_deployment(self.namespace, rbac_deploy, original_replicas)

        print(f"\n{'='*72}")
        print("RBAC-005 SUMMARY: Replica Scaling")
        print(f"{'='*72}")

        baseline_p95 = None
        for rc, data in sorted(results_by_replicas.items()):
            lat = data["latency"]
            p95 = lat["p95"] * 1000
            improvement = ""
            if baseline_p95 is not None and baseline_p95 > 0:
                pct = (1 - p95 / baseline_p95) * 100
                improvement = f" ({pct:+.0f}% vs 1-replica)"
            else:
                baseline_p95 = p95
            print(f"  {rc} replica(s): p95={p95:.1f}ms, "
                  f"{lat['requests_per_second']} req/s{improvement}")

        perf_result.test_id = "PERF-RBAC-005"
        perf_result.metrics = {
            "concurrency": concurrency,
            "results_by_replicas": results_by_replicas,
        }
        perf_result.passed = True
        perf_collector.add_result(perf_result)

    # -----------------------------------------------------------------
    # RBAC-006: Under ingestion load
    # -----------------------------------------------------------------

    @pytest.mark.timeout(600)
    def test_perf_rbac_006_under_ingestion(
        self,
        cluster_config: ClusterConfig,
        database_config: DatabaseConfig,
        gateway_url: str,
        ingress_url: str,
        koku_api_url: str,
        rh_identity_header: str,
        ingress_pod: str,
        perf_timer: PerfTimer,
        perf_result: PerformanceResult,
        perf_collector: PerfResultCollector,
        perf_cleanup,
        keycloak_config,
    ):
        """PERF-RBAC-006: Measure RBAC latency while ingestion is active.

        Captures quiescent RBAC baseline, then uploads 5 sources concurrently
        and immediately re-measures RBAC latency. Compares to quantify whether
        shared PostgreSQL write load from ingestion degrades RBAC read perf.
        """
        print(f"\n{'='*72}")
        print("PERF-RBAC-006: RBAC Latency Under Ingestion Load")
        print(f"{'='*72}\n")

        rbac_access = _rbac_access_url(gateway_url)
        session = self._create_session()

        # --- Phase 1: quiescent baseline ---
        print("Phase 1: Measuring quiescent RBAC baseline (100 calls)...")
        baseline_result = _measure_latency(session, rbac_access, n=100)
        baseline = calculate_percentiles(baseline_result["latencies"])
        print(f"  Baseline: p50={baseline['p50']*1000:.1f}ms "
              f"p95={baseline['p95']*1000:.1f}ms")

        # --- Phase 2: register sources and start ingestion ---
        print("\nPhase 2: Starting ingestion (5 sources)...")
        jwt = obtain_jwt_token(self._keycloak_config)

        source_count = 5
        sources = []
        for i in range(source_count):
            try:
                cluster_id = generate_cluster_id()
                source_name = f"rbac-ing-{i}-{cluster_id[-6:]}"
                source = register_source(
                    self.namespace,
                    ingress_pod,
                    koku_api_url,
                    rh_identity_header,
                    cluster_id,
                    "org1234567",
                    source_name,
                )
                perf_cleanup.track(
                    source_id=source.source_id,
                    cluster_id=cluster_id,
                    source_name=source_name,
                )
                sources.append((source, cluster_id, source_name))
            except Exception as e:
                print(f"  WARN: Failed to register source {i}: {e}")

        if not sources:
            pytest.skip("Could not register any sources for ingestion")

        print(f"  Registered {len(sources)} sources, uploading concurrently...")

        now = datetime.now(timezone.utc)
        end_dt = now.replace(hour=0, minute=0, second=0, microsecond=0)
        start_dt = end_dt - timedelta(days=2)

        def _upload_one(idx, _source, _cluster_id, _source_name):
            try:
                return generate_and_upload_data(
                    _cluster_id, _source_name,
                    start_dt, end_dt,
                    ingress_url, jwt,
                )
            except Exception as e:
                print(f"  Upload {idx} failed: {e}")
                return None

        with ThreadPoolExecutor(max_workers=source_count) as pool:
            futures = [
                pool.submit(_upload_one, i, s[0], s[1], s[2])
                for i, s in enumerate(sources)
            ]

        upload_ok = sum(1 for f in futures if f.result() is not None)
        print(f"  {upload_ok}/{len(sources)} uploads completed")

        # --- Phase 3: measure RBAC under load ---
        print("\nPhase 3: Measuring RBAC latency during processing "
              f"(concurrency=10, 60s)...")

        pg_before = capture_pg_stats(
            self.namespace,
            database_config.pod_name,
            database_config.database,
            database_config.user,
        )

        under_load = _measure_latency_concurrent(
            session_factory=self._create_session,
            url=rbac_access,
            concurrency=10,
            duration_s=60.0,
        )

        pg_after = capture_pg_stats(
            self.namespace,
            database_config.pod_name,
            database_config.database,
            database_config.user,
        )
        pg_delta = diff_pg_stats(pg_before, pg_after)

        # --- Summary ---
        degradation_p50 = (under_load["p50"] / baseline["p50"]
                           if baseline["p50"] > 0 else 0)
        degradation_p95 = (under_load["p95"] / baseline["p95"]
                           if baseline["p95"] > 0 else 0)

        print(f"\n{'='*72}")
        print("RBAC-006 SUMMARY: Under Ingestion Load")
        print(f"{'='*72}")
        print(f"Quiescent: p50={baseline['p50']*1000:.1f}ms "
              f"p95={baseline['p95']*1000:.1f}ms")
        print(f"Under load: p50={under_load['p50']*1000:.1f}ms "
              f"p95={under_load['p95']*1000:.1f}ms "
              f"({under_load['requests_per_second']} req/s)")
        print(f"Degradation: p50={degradation_p50:.1f}×, "
              f"p95={degradation_p95:.1f}×")
        print(f"Errors: {under_load.get('errors', 0)}")
        print(f"PG commits delta: {pg_delta.get('xact_commit_delta', '?')}, "
              f"cache hit: {pg_delta.get('cache_hit_ratio', '?')}")

        error_rate = under_load.get("errors", 0) / max(under_load["total_requests"], 1)
        # Under ingestion load, up to 50x degradation is acceptable — we're
        # testing that RBAC still responds, not that it's fast.
        max_degradation = 50.0
        passed = error_rate < 0.05 and degradation_p95 < max_degradation

        perf_result.test_id = "PERF-RBAC-006"
        perf_result.metrics = {
            "baseline": baseline,
            "under_load": under_load,
            "degradation_p50": round(degradation_p50, 2),
            "degradation_p95": round(degradation_p95, 2),
            "sources_uploaded": upload_ok,
            "pg_stats": pg_delta,
            "error_rate": round(error_rate, 4),
        }
        perf_result.passed = passed
        perf_collector.add_result(perf_result)

        assert error_rate < 0.05, (
            f"RBAC-006: error rate {error_rate:.1%} exceeds 5% under ingestion load"
        )
        assert degradation_p95 < max_degradation, (
            f"RBAC-006: p95 degradation {degradation_p95:.1f}x exceeds "
            f"{max_degradation}x ceiling (baseline={baseline['p95']*1000:.1f}ms, "
            f"under_load={under_load['p95']*1000:.1f}ms)"
        )
