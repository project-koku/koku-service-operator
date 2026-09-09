"""
PostgreSQL Resource Sweep Tests (COST-7605 DB-1, DB-2, DB-3).

Tests whether PostgreSQL CPU and memory allocations affect API query latency
and ingestion processing time. Patches the database StatefulSet with different
resource configurations, runs representative API queries, and captures
pg_stat metrics to identify diminishing-returns thresholds.

DB-3 measures API latency degradation while Celery workers are actively
processing an ingestion workload (concurrent read/write contention).

Test IDs:
- PERF-DB-001: CPU sweep (1000m → 2000m → 4000m)
- PERF-DB-002: Memory sweep (4Gi → 8Gi → 16Gi)
- PERF-DB-003: API latency under ingestion load
"""

import os
import time
from dataclasses import field
from datetime import datetime, timedelta, timezone
from typing import Any, Dict, List, Optional, Tuple

import pytest
import requests

from conftest import (
    ClusterConfig,
    DatabaseConfig,
    JWTToken,
    KeycloakConfig,
    create_authenticated_session,
    obtain_jwt_token,
)
from e2e_helpers import (
    generate_cluster_id,
    register_source,
    wait_for_processing_complete,
)
from utils import get_pod_by_label, run_oc_command

from .data_classes import PerformanceResult
from .helpers import (
    APIProbeThread,
    PerfResultCollector,
    PerfTimer,
    generate_and_upload_data,
)
from .k8s_helpers import (
    PgStatSnapshot,
    calculate_percentiles,
    capture_pg_stats,
    diff_pg_stats,
    get_db_cpu_utilization,
    get_db_shared_buffers,
    get_resource_spec,
    merge_resources,
    patch_resource_spec,
    restore_resource_spec,
)
from .profiles import ACTIVE_PROFILE as _ACTIVE_PROFILE, PROFILES
from .tracker import PerfCleanupTracker


# =============================================================================
# API Latency Measurement (lightweight, inline)
# =============================================================================


def measure_api_latencies(
    gateway_url: str,
    keycloak_config: KeycloakConfig,
    iterations: int = 10,
) -> Dict[str, Any]:
    """Run a standard set of API queries and return latency stats.

    Uses the same endpoints as the existing API latency tests but
    with fewer iterations for faster sweep runs.
    """
    session = create_authenticated_session(keycloak_config)
    base = gateway_url.rstrip("/")
    results = {}

    # API-001 equivalent: report baseline
    report_url = f"{base}/api/cost-management/v1/reports/openshift/costs/"
    report_latencies = _measure_endpoint(session, report_url, iterations)
    results["report_baseline"] = report_latencies

    # API-003 equivalent: group_by query (most CPU-sensitive)
    group_by_url = (
        f"{base}/api/cost-management/v1/reports/openshift/costs/"
        f"?group_by[project]=*&filter[time_scope_value]=-30"
    )
    group_by_latencies = _measure_endpoint(session, group_by_url, iterations)
    results["group_by_project"] = group_by_latencies

    # Cost models list
    cost_models_url = f"{base}/api/cost-management/v1/cost-models/"
    cost_model_latencies = _measure_endpoint(session, cost_models_url, iterations)
    results["cost_models_list"] = cost_model_latencies

    session.close()
    return results


def _measure_endpoint(
    session: requests.Session, url: str, iterations: int
) -> Dict[str, Any]:
    """Measure latency for a single endpoint over N iterations."""
    # Warmup
    for _ in range(2):
        try:
            session.get(url, timeout=60)
        except requests.RequestException:
            pass

    latencies = []
    errors = 0
    for _ in range(iterations):
        start = time.time()
        try:
            resp = session.get(url, timeout=60)
            latency = time.time() - start
            latencies.append(latency)
            if resp.status_code != 200:
                errors += 1
        except requests.RequestException:
            latencies.append(time.time() - start)
            errors += 1

    return calculate_percentiles(latencies, errors)






# =============================================================================
# Test Parametrization
# =============================================================================

# CPU sweep: request/limit pairs
_DB_CPU_LEVELS: dict = {
    "baseline": [
        pytest.param(
            {"requests": {"cpu": "100m"}, "limits": {"cpu": "500m"}},
            id="500m-default",
        ),
    ],
    "small": [
        pytest.param(
            {"requests": {"cpu": "100m"}, "limits": {"cpu": "500m"}},
            id="500m-default",
        ),
        pytest.param(
            {"requests": {"cpu": "500m"}, "limits": {"cpu": "2000m"}},
            id="2000m-medium",
        ),
    ],
    "medium": [
        pytest.param(
            {"requests": {"cpu": "500m"}, "limits": {"cpu": "2000m"}},
            id="2000m-medium",
        ),
        pytest.param(
            {"requests": {"cpu": "1000m"}, "limits": {"cpu": "4000m"}},
            id="4000m-large",
        ),
        pytest.param(
            {"requests": {"cpu": "2000m"}, "limits": {"cpu": "8000m"}},
            id="8000m-xlarge",
        ),
    ],
    "large": [
        pytest.param(
            {"requests": {"cpu": "500m"}, "limits": {"cpu": "2000m"}},
            id="2000m-medium",
        ),
        pytest.param(
            {"requests": {"cpu": "1000m"}, "limits": {"cpu": "4000m"}},
            id="4000m-large",
        ),
        pytest.param(
            {"requests": {"cpu": "2000m"}, "limits": {"cpu": "8000m"}},
            id="8000m-xlarge",
        ),
    ],
}

# Memory sweep: request/limit pairs
_DB_MEM_LEVELS: dict = {
    "baseline": [
        pytest.param(
            {"requests": {"memory": "256Mi"}, "limits": {"memory": "512Mi"}},
            id="512Mi-default",
        ),
    ],
    "small": [
        pytest.param(
            {"requests": {"memory": "256Mi"}, "limits": {"memory": "512Mi"}},
            id="512Mi-default",
        ),
        pytest.param(
            {"requests": {"memory": "2Gi"}, "limits": {"memory": "4Gi"}},
            id="4Gi-small",
        ),
    ],
    "medium": [
        pytest.param(
            {"requests": {"memory": "2Gi"}, "limits": {"memory": "4Gi"}},
            id="4Gi-medium",
        ),
        pytest.param(
            {"requests": {"memory": "4Gi"}, "limits": {"memory": "8Gi"}},
            id="8Gi-large",
        ),
        pytest.param(
            {"requests": {"memory": "8Gi"}, "limits": {"memory": "16Gi"}},
            id="16Gi-xlarge",
        ),
    ],
    "large": [
        pytest.param(
            {"requests": {"memory": "2Gi"}, "limits": {"memory": "4Gi"}},
            id="4Gi-medium",
        ),
        pytest.param(
            {"requests": {"memory": "4Gi"}, "limits": {"memory": "8Gi"}},
            id="8Gi-large",
        ),
        pytest.param(
            {"requests": {"memory": "8Gi"}, "limits": {"memory": "16Gi"}},
            id="16Gi-xlarge",
        ),
    ],
}


def _get_levels(table: dict) -> list:
    profile = _ACTIVE_PROFILE if _ACTIVE_PROFILE in table else "medium"
    return table.get(profile, table["medium"])


# =============================================================================
# Tests
# =============================================================================


@pytest.mark.performance
@pytest.mark.db_sweep
class TestPostgresCPUSweep:
    """PERF-DB-001: PostgreSQL CPU sweep.

    Patches the database StatefulSet with different CPU allocations,
    runs API queries, and captures pg_stat metrics to find the
    diminishing-returns threshold.
    """

    @pytest.fixture(autouse=True)
    def setup(self, cluster_config: ClusterConfig, keycloak_config: KeycloakConfig):
        self.namespace = cluster_config.namespace
        self.helm_release = cluster_config.helm_release_name
        self._keycloak_config = keycloak_config
        self._sts_name = f"{self.helm_release}-database"

    @pytest.mark.parametrize("cpu_resources", _get_levels(_DB_CPU_LEVELS))
    def test_perf_db_001_cpu_sweep(
        self,
        cpu_resources: dict,
        gateway_url: str,
        database_config: DatabaseConfig,
        perf_timer: PerfTimer,
        perf_result: PerformanceResult,
        perf_collector: PerfResultCollector,
    ):
        """Measure API latency and pg_stat metrics at a given CPU allocation."""
        cpu_limit = cpu_resources["limits"]["cpu"]
        profile_name = _ACTIVE_PROFILE if _ACTIVE_PROFILE in PROFILES else "medium"

        print(f"\n{'='*70}")
        print(f"PERF-DB-001: Database CPU = {cpu_limit} (profile: {profile_name})")
        print(f"{'='*70}")

        original_resources = get_resource_spec(self.namespace, "statefulset", self._sts_name)
        print(f"Original DB resources: {original_resources}")

        # Merge: keep existing memory, override CPU only
        target_resources = merge_resources(original_resources, cpu_resources)
        is_modified = target_resources != original_resources

        try:
            if is_modified:
                with perf_timer.measure("db_resource_patch"):
                    success = patch_resource_spec(
                        self.namespace, "statefulset", self._sts_name, target_resources
                    )
                    if not success:
                        pytest.skip(f"Could not patch database to CPU {cpu_limit}")

            # Capture pre-test pg_stat
            pg_before = capture_pg_stats(
                self.namespace,
                database_config.pod_name,
                database_config.database,
                database_config.user,
            )
            shared_buffers = get_db_shared_buffers(
                self.namespace,
                database_config.pod_name,
                database_config.database,
                database_config.user,
            )
            print(f"shared_buffers = {shared_buffers}")

            # Sample DB CPU before queries
            cpu_before = get_db_cpu_utilization(self.namespace, database_config.pod_name)

            # Run API latency measurements
            with perf_timer.measure("api_latency_measurement"):
                api_results = measure_api_latencies(
                    gateway_url, self._keycloak_config, iterations=20
                )

            # Sample DB CPU during/after queries
            cpu_after = get_db_cpu_utilization(self.namespace, database_config.pod_name)

            # Capture post-test pg_stat
            pg_after = capture_pg_stats(
                self.namespace,
                database_config.pod_name,
                database_config.database,
                database_config.user,
            )

            pg_delta = diff_pg_stats(pg_before, pg_after)

        finally:
            if is_modified:
                print(f"\nRestoring DB resources to original...")
                restore_resource_spec(
                    self.namespace, "statefulset", self._sts_name, original_resources
                )

        # Build result
        perf_result.test_id = f"PERF-DB-001-{cpu_limit}"
        perf_result.metrics = {
            "cpu_limit": cpu_limit,
            "cpu_request": cpu_resources["requests"]["cpu"],
            "profile": profile_name,
            "shared_buffers": shared_buffers,
            "api_latencies": api_results,
            "pg_stat_delta": pg_delta,
            "db_cpu_millicores": {
                "before_queries": cpu_before,
                "after_queries": cpu_after,
            },
        }
        perf_result.timings = perf_timer.get_timings()
        perf_result.passed = True
        perf_collector.add_result(perf_result)

        # Print summary
        print(f"\n{'='*70}")
        print(f"PERF-DB-001 SUMMARY — CPU {cpu_limit}")
        print(f"  Cache hit ratio:     {pg_delta['cache_hit_ratio']:.4f}")
        print(f"  Transactions:        {pg_delta['xact_commit_delta']} commit, "
              f"{pg_delta['xact_rollback_delta']} rollback")
        print(f"  Report baseline p95: {api_results['report_baseline']['p95']:.4f}s")
        print(f"  Group-by p95:        {api_results['group_by_project']['p95']:.4f}s")
        print(f"  DB CPU:              {cpu_before}m → {cpu_after}m")
        print(f"{'='*70}")


@pytest.mark.performance
@pytest.mark.db_sweep
class TestPostgresMemorySweep:
    """PERF-DB-002: PostgreSQL memory sweep.

    Patches the database StatefulSet with different memory allocations,
    runs API queries, and measures buffer cache hit ratio to find
    the point of diminishing returns.
    """

    @pytest.fixture(autouse=True)
    def setup(self, cluster_config: ClusterConfig, keycloak_config: KeycloakConfig):
        self.namespace = cluster_config.namespace
        self.helm_release = cluster_config.helm_release_name
        self._keycloak_config = keycloak_config
        self._sts_name = f"{self.helm_release}-database"

    @pytest.mark.parametrize("mem_resources", _get_levels(_DB_MEM_LEVELS))
    def test_perf_db_002_memory_sweep(
        self,
        mem_resources: dict,
        gateway_url: str,
        database_config: DatabaseConfig,
        perf_timer: PerfTimer,
        perf_result: PerformanceResult,
        perf_collector: PerfResultCollector,
    ):
        """Measure API latency and buffer cache hit ratio at a given memory allocation."""
        mem_limit = mem_resources["limits"]["memory"]
        profile_name = _ACTIVE_PROFILE if _ACTIVE_PROFILE in PROFILES else "medium"

        print(f"\n{'='*70}")
        print(f"PERF-DB-002: Database memory = {mem_limit} (profile: {profile_name})")
        print(f"{'='*70}")

        original_resources = get_resource_spec(self.namespace, "statefulset", self._sts_name)
        print(f"Original DB resources: {original_resources}")

        # Merge: keep existing CPU, override memory only
        target_resources = merge_resources(original_resources, mem_resources)
        is_modified = target_resources != original_resources

        try:
            if is_modified:
                with perf_timer.measure("db_resource_patch"):
                    success = patch_resource_spec(
                        self.namespace, "statefulset", self._sts_name, target_resources
                    )
                    if not success:
                        pytest.skip(f"Could not patch database to memory {mem_limit}")

            # Refresh database_config pod name (new pod after restart)
            db_pod = _find_db_pod(self.namespace)
            if not db_pod:
                pytest.skip("Database pod not found after patch")

            shared_buffers = get_db_shared_buffers(
                self.namespace, db_pod,
                database_config.database, database_config.user,
            )
            print(f"shared_buffers = {shared_buffers}")

            pg_before = capture_pg_stats(
                self.namespace, db_pod,
                database_config.database, database_config.user,
            )

            with perf_timer.measure("api_latency_measurement"):
                api_results = measure_api_latencies(
                    gateway_url, self._keycloak_config, iterations=20
                )

            pg_after = capture_pg_stats(
                self.namespace, db_pod,
                database_config.database, database_config.user,
            )
            pg_delta = diff_pg_stats(pg_before, pg_after)

        finally:
            if is_modified:
                print(f"\nRestoring DB resources to original...")
                restore_resource_spec(
                    self.namespace, "statefulset", self._sts_name, original_resources
                )

        perf_result.test_id = f"PERF-DB-002-{mem_limit}"
        perf_result.metrics = {
            "memory_limit": mem_limit,
            "memory_request": mem_resources["requests"]["memory"],
            "profile": profile_name,
            "shared_buffers": shared_buffers,
            "api_latencies": api_results,
            "pg_stat_delta": pg_delta,
        }
        perf_result.timings = perf_timer.get_timings()
        perf_result.passed = True
        perf_collector.add_result(perf_result)

        print(f"\n{'='*70}")
        print(f"PERF-DB-002 SUMMARY — Memory {mem_limit}")
        print(f"  Cache hit ratio:     {pg_delta['cache_hit_ratio']:.4f}")
        print(f"  Blocks hit/read:     {pg_delta['blks_hit_delta']}/{pg_delta['blks_read_delta']}")
        print(f"  shared_buffers:      {shared_buffers}")
        print(f"  Report baseline p95: {api_results['report_baseline']['p95']:.4f}s")
        print(f"  Group-by p95:        {api_results['group_by_project']['p95']:.4f}s")
        print(f"{'='*70}")



@pytest.mark.performance
@pytest.mark.db_sweep
class TestAPIUnderLoad:
    """PERF-DB-003: API latency under ingestion load.

    Measures API response time degradation while Celery workers are actively
    processing an ingestion workload. Compares against a quiescent baseline
    to quantify the impact of concurrent read/write contention on PostgreSQL.
    """

    @pytest.fixture(autouse=True)
    def setup(self, cluster_config: ClusterConfig, keycloak_config: KeycloakConfig):
        self.namespace = cluster_config.namespace
        self.helm_release = cluster_config.helm_release_name
        self._keycloak_config = keycloak_config

    @pytest.mark.timeout(1800)
    def test_perf_db_003_api_under_ingestion_load(
        self,
        gateway_url: str,
        ingress_url: str,
        database_config: DatabaseConfig,
        koku_api_url: str,
        perf_timer: PerfTimer,
        perf_result: PerformanceResult,
        perf_collector: PerfResultCollector,
        rh_identity_header: str,
        perf_cleanup: PerfCleanupTracker,
        ingress_pod: str,
        jwt_token: JWTToken,
    ):
        """Measure API latency with and without active ingestion processing."""
        profile_name = _ACTIVE_PROFILE if _ACTIVE_PROFILE in PROFILES else "medium"

        print(f"\n{'='*70}")
        print(f"PERF-DB-003: API Under Ingestion Load (profile: {profile_name})")
        print(f"{'='*70}")

        # ------------------------------------------------------------------
        # Phase 1: Quiescent baseline
        # ------------------------------------------------------------------
        print("\n--- Phase 1: Quiescent baseline ---")
        with perf_timer.measure("quiescent_baseline"):
            baseline = measure_api_latencies(
                gateway_url, self._keycloak_config, iterations=20
            )

        print(
            f"Baseline — report p95={baseline['report_baseline']['p95']:.4f}s  "
            f"group_by p95={baseline['group_by_project']['p95']:.4f}s  "
            f"cost_models p95={baseline['cost_models_list']['p95']:.4f}s"
        )

        # ------------------------------------------------------------------
        # Phase 2: Register source + trigger ingestion
        # ------------------------------------------------------------------
        print("\n--- Phase 2: Trigger ingestion ---")

        cluster_id = generate_cluster_id()
        source_name = f"perf-db-003-{cluster_id[-8:]}"

        with perf_timer.measure("source_registration"):
            source = register_source(
                self.namespace, ingress_pod, koku_api_url,
                rh_identity_header, cluster_id, "org1234567", source_name,
            )

        perf_cleanup.track(
            source_id=source.source_id,
            cluster_id=cluster_id,
            source_name=source_name,
        )

        end_date = datetime.now(timezone.utc)
        start_date = end_date - timedelta(days=PROFILES[profile_name]["data_days"])

        with perf_timer.measure("data_generation_and_upload"):
            upload_result = generate_and_upload_data(
                cluster_id,
                source_name,
                start_date,
                end_date,
                ingress_url,
                jwt_token,
                profile_name=profile_name,
            )

        package_mb = upload_result.get("package_size_mb", 0)
        upload_s = upload_result.get("upload_seconds", 0)
        print(
            f"Uploaded {package_mb:.1f} MB in {upload_s:.1f}s "
            f"({package_mb / upload_s:.2f} MB/s)"
            if upload_s > 0 else f"Uploaded {package_mb:.1f} MB"
        )

        # ------------------------------------------------------------------
        # Phase 3: Probe API continuously during processing
        # ------------------------------------------------------------------
        print("\n--- Phase 3: API probing during ingestion ---")

        # Capture pg_stat before processing window
        pg_before = capture_pg_stats(
            self.namespace,
            database_config.pod_name,
            database_config.database,
            database_config.user,
        )

        session = create_authenticated_session(self._keycloak_config)
        probe = APIProbeThread(
            session, gateway_url, self.namespace, poll_interval=2.0,
            keycloak_config=self._keycloak_config,
        )
        probe.start()

        try:
            with perf_timer.measure("processing_wait"):
                processing_result = wait_for_processing_complete(
                    self.namespace,
                    database_config.pod_name,
                    cluster_id,
                    poll_interval=10,
                    max_wait_seconds=1500,
                )
        finally:
            under_load = probe.stop()
            session.close()

        # Capture pg_stat after processing window
        pg_after = capture_pg_stats(
            self.namespace,
            database_config.pod_name,
            database_config.database,
            database_config.user,
        )
        pg_delta = diff_pg_stats(pg_before, pg_after)

        processing_complete = processing_result.get("complete", False)
        processing_elapsed = processing_result.get("elapsed_s", 0)

        # ------------------------------------------------------------------
        # Phase 4: Compute degradation ratios
        # ------------------------------------------------------------------
        degradation = {}
        for endpoint in ("report_baseline", "group_by_project", "cost_models_list"):
            base_stats = baseline.get(endpoint, {})
            load_stats = under_load.get(endpoint, {})
            ratios = {}
            for pct in ("p50", "p95", "p99"):
                base_val = base_stats.get(pct, 0)
                load_val = load_stats.get(pct, 0)
                if base_val > 0:
                    ratios[pct] = round(load_val / base_val, 2)
                else:
                    ratios[pct] = 0
            degradation[endpoint] = ratios

        # ------------------------------------------------------------------
        # Build result
        # ------------------------------------------------------------------
        perf_result.test_id = "PERF-DB-003"
        perf_result.metrics = {
            "profile": profile_name,
            "baseline": baseline,
            "under_load": under_load,
            "degradation_ratios": degradation,
            "upload": {
                "package_size_mb": package_mb,
                "upload_seconds": upload_s,
            },
            "processing": {
                "complete": processing_complete,
                "elapsed_s": processing_elapsed,
            },
            "pg_stat_delta": pg_delta,
            "probe_count": under_load.get("probe_count", 0),
            "peak_queue_depth": under_load.get("peak_queue_depth", 0),
        }
        perf_result.timings = perf_timer.get_timings()
        perf_result.passed = True
        perf_collector.add_result(perf_result)

        # ------------------------------------------------------------------
        # Summary
        # ------------------------------------------------------------------
        print(f"\n{'='*70}")
        print(f"PERF-DB-003 SUMMARY — API Under Ingestion Load ({profile_name})")
        print(f"{'='*70}")
        print(f"  Processing:          {'complete' if processing_complete else 'INCOMPLETE'} "
              f"({processing_elapsed:.0f}s)")
        print(f"  Probes collected:    {under_load.get('probe_count', 0)}")
        print(f"  Peak queue depth:    {under_load.get('peak_queue_depth', 0)}")
        print(f"  Probe errors:        {under_load.get('total_errors', 0)}")
        print(f"  pg_stat commits:     {pg_delta['xact_commit_delta']} "
              f"(rollbacks: {pg_delta['xact_rollback_delta']})")
        print(f"  Cache hit ratio:     {pg_delta['cache_hit_ratio']:.4f}")

        for endpoint in ("report_baseline", "group_by_project", "cost_models_list"):
            b = baseline.get(endpoint, {})
            u = under_load.get(endpoint, {})
            d = degradation.get(endpoint, {})
            print(f"\n  {endpoint}:")
            print(f"    Baseline p95:      {b.get('p95', 0):.4f}s")
            print(f"    Under-load p95:    {u.get('p95', 0):.4f}s")
            print(f"    Degradation:       {d.get('p95', 0):.1f}x")

        print(f"{'='*70}")


# =============================================================================
# Helpers
# =============================================================================


def _find_db_pod(namespace: str) -> Optional[str]:
    """Find the current running database pod name."""
    return get_pod_by_label(namespace, "app.kubernetes.io/component=database")
