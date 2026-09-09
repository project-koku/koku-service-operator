"""
Multi-Cluster Scale Performance Tests (PERF-SCALE-*).

Tests system limits with multiple sources and large datasets per FLPATH-4036.

Test IDs:
- PERF-SCALE-001: Source count baseline
- PERF-SCALE-002: Source count ramp
- PERF-SCALE-003: Large source dataset
- PERF-SCALE-004: Concurrent API queries
- PERF-SCALE-005: Historical data depth
"""

import json
import os
import time
import uuid
from concurrent.futures import ThreadPoolExecutor, as_completed
from datetime import datetime, timedelta, timezone
from typing import Any, Dict, List, Optional

import pytest
import requests

from conftest import ClusterConfig
from e2e_helpers import (
    cleanup_database_records,
    delete_source,
    ensure_nise_available,
    generate_cluster_id,
    register_source,
    wait_for_summary_tables,
)
from utils import exec_in_pod, execute_db_query, get_pod_by_label, run_oc_command

from .data_classes import PerformanceResult
from .helpers import (
    PerfResultCollector,
    PerfTimer,
    create_authenticated_session,
    parse_memory_mib,
)
from .tracker import PerfCleanupTracker
from .profiles import ACTIVE_PROFILE as _ACTIVE_PROFILE, PROFILES, get_profile_metrics
from .test_api_latency import calculate_percentiles, measure_request_latency


# =============================================================================
# Profile-filtered parametrize lists
# Mirrors the pattern used in test_ingestion.py — all gated by PERF_PROFILE so
# baseline runs stay fast and heavier scale variants only appear at profile
# levels where they produce meaningful signal.
# =============================================================================

_SCALE_001_SKIP_REASON = (
    "SCALE-001[10] not run for baseline — registering 10 sources adds 2+ min "
    "with no additional signal beyond the [5] variant."
)
_SCALE_001_COUNTS: dict = {
    "baseline": [5],
    "small":    [5, 10],
    "medium":   [5, 10],
    "large":    [5, 10],
}
SCALE_001_COUNTS = _SCALE_001_COUNTS.get(_ACTIVE_PROFILE, _SCALE_001_COUNTS["large"])

# SCALE-002 runs for all profiles including baseline.  The test registers up to 25
# sources in batches and measures API latency after each batch — no data processing
# or NISE generation involved.  Observed runtime is ~5 min, making it practical for
# baseline.  It validates a core behavior (source registration + API responsiveness
# under incremental load) that is meaningful at every profile level.
_SCALE_002_SKIP_REASON = ""  # unused — skipif removed below

_SCALE_003_SKIP_REASON = (
    "SCALE-003 (large namespace count) requires substantial ingested data "
    "to be meaningful and is not applicable to baseline."
)

_SCALE_004_CONCURRENCY: dict = {
    "baseline": [5],
    "small":    [5, 10],
    "medium":   [5, 10, 20],
    "large":    [5, 10, 20],
}
SCALE_004_CONCURRENCY = _SCALE_004_CONCURRENCY.get(_ACTIVE_PROFILE, _SCALE_004_CONCURRENCY["large"])

_SCALE_005_DAYS: dict = {
    "baseline": [10],
    "small":    [10, 30],
    "medium":   [10, 30],
    "large":    [10, 30],
}
SCALE_005_DAYS = _SCALE_005_DAYS.get(_ACTIVE_PROFILE, _SCALE_005_DAYS["large"])


# =============================================================================
# Test Classes
# =============================================================================

@pytest.mark.performance
@pytest.mark.scale
@pytest.mark.slow
class TestMultiClusterScale:
    """Multi-cluster scale performance tests."""
    
    @pytest.fixture(autouse=True)
    def setup(
        self,
        cluster_config: ClusterConfig,
        keycloak_config,
        gateway_url: str,
    ):
        """Setup for scale tests."""
        self.namespace = cluster_config.namespace
        self.helm_release = cluster_config.helm_release_name
        self._keycloak_config = keycloak_config
        self.gateway_url = gateway_url
        self.api_base = f"{gateway_url}/cost-management/v1"
    
    def _get_authenticated_session(self) -> requests.Session:
        """Get session with fresh token."""
        return create_authenticated_session(self._keycloak_config)
    
    def _get_source_count(self, session: requests.Session) -> int:
        """Get current number of sources."""
        response = session.get(
            f"{self.api_base}/sources/",
            params={"limit": 1},
            timeout=30,
        )
        if response.ok:
            data = response.json()
            return data.get("meta", {}).get("count", 0)
        return 0
    
    def _get_memory_usage(self) -> Dict[str, float]:
        """Get memory usage for key pods."""
        memory_usage = {}
        
        components = [
            ("koku-api", "app.kubernetes.io/component=cost-management-api"),
            ("listener", "app.kubernetes.io/component=listener"),
            ("database", "app.kubernetes.io/component=database"),
        ]
        
        for name, label in components:
            result = run_oc_command([
                "adm", "top", "pod", "-n", self.namespace,
                "-l", label, "--no-headers"
            ], check=False)
            
            if result.returncode == 0 and result.stdout.strip():
                try:
                    parts = result.stdout.strip().split()
                    if len(parts) >= 3:
                        memory_usage[name] = parse_memory_mib(parts[2])
                except (ValueError, IndexError):
                    pass
        
        return memory_usage
    
    @pytest.mark.parametrize("source_count", SCALE_001_COUNTS)
    def test_perf_scale_001_source_count_baseline(
        self,
        source_count: int,
        cluster_config: ClusterConfig,
        database_config,
        rh_identity_header: str,
        koku_api_url: str,
        perf_timer: PerfTimer,
        perf_result: PerformanceResult,
        perf_collector: PerfResultCollector,
        perf_cleanup,
    ):
        """PERF-SCALE-001: Source count baseline - N sources, steady state.
        
        Tests system behavior with a fixed number of sources.
        
        Metrics:
        - Memory usage per component
        - API latency with N sources
        """
        session = self._get_authenticated_session()
        initial_sources = self._get_source_count(session)
        
        ingress_pod = get_pod_by_label(self.namespace, "app.kubernetes.io/component=ingress")
        if not ingress_pod:
            pytest.skip("Ingress pod not found")
        
        # Register sources
        sources_created = []
        
        with perf_timer.measure("source_registration"):
            for i in range(source_count):
                cluster_id = generate_cluster_id()
                source_name = f"perf-scale-001-{i:03d}-{cluster_id[-8:]}"
                
                try:
                    source = register_source(
                        self.namespace,
                        ingress_pod,
                        koku_api_url,
                        rh_identity_header,
                        cluster_id,
                        "org1234567",
                        source_name,
                    )
                    sources_created.append(source)
                    # Track for cleanup
                    perf_cleanup.track(source_id=source.source_id, cluster_id=cluster_id, source_name=source_name)
                except Exception as e:
                    break
        
        # Measure memory after source creation
        memory_after_sources = self._get_memory_usage()
        
        # Test API latency with N sources
        api_latencies = []
        with perf_timer.measure("api_latency_test"):
            for _ in range(20):
                latency, status, _ = measure_request_latency(
                    session,
                    f"{self.api_base}/sources/",
                    params={"limit": 100},
                )
                if status == 200:
                    api_latencies.append(latency)
        
        final_sources = self._get_source_count(session)
        
        perf_result.metrics = {
            "target_source_count": source_count,
            "sources_created": len(sources_created),
            "initial_source_count": initial_sources,
            "final_source_count": final_sources,
            "memory_usage_mib": memory_after_sources,
            "api_latencies": calculate_percentiles(api_latencies),
        }
        perf_result.timings = perf_timer.get_timings()
        perf_result.passed = len(sources_created) == source_count
        
        perf_collector.add_result(perf_result)
        
        assert len(sources_created) >= source_count * 0.9, (
            f"Only created {len(sources_created)}/{source_count} sources"
        )

    @pytest.mark.timeout(900)  # 15 minutes for source ramp test
    def test_perf_scale_002_source_ramp(
        self,
        cluster_config: ClusterConfig,
        database_config,
        rh_identity_header: str,
        koku_api_url: str,
        perf_timer: PerfTimer,
        perf_result: PerformanceResult,
        perf_collector: PerfResultCollector,
        perf_cleanup,
    ):
        """PERF-SCALE-002: Source count ramp - Add sources until degradation.
        
        Incrementally adds sources and monitors for performance degradation.
        
        Metrics:
        - Max sources before degradation
        - Memory growth per source
        - API latency degradation curve
        """
        max_sources = int(os.environ.get("PERF_SCALE_002_MAX_SOURCES", "25"))
        batch_size = int(os.environ.get("PERF_SCALE_002_BATCH_SIZE", "5"))
        latency_threshold_seconds = 5.0
        
        session = self._get_authenticated_session()
        initial_sources = self._get_source_count(session)
        initial_memory = self._get_memory_usage()
        
        ingress_pod = get_pod_by_label(self.namespace, "app.kubernetes.io/component=ingress")
        if not ingress_pod:
            pytest.skip("Ingress pod not found")
        
        checkpoints = []
        sources_created = 0
        breaking_point = None
        
        with perf_timer.measure("source_ramp"):
            while sources_created < max_sources:
                # Add batch of sources
                batch_created = 0
                for i in range(batch_size):
                    cluster_id = generate_cluster_id()
                    source_name = f"perf-scale-002-{sources_created + i:03d}-{cluster_id[-8:]}"
                    
                    try:
                        source = register_source(
                            self.namespace,
                            ingress_pod,
                            koku_api_url,
                            rh_identity_header,
                            cluster_id,
                            "org1234567",
                            source_name,
                        )
                        batch_created += 1
                        # Track for cleanup
                        perf_cleanup.track(source_id=source.source_id, cluster_id=cluster_id, source_name=source_name)
                    except Exception as e:
                        break
                
                sources_created += batch_created
                
                # Measure state after batch
                current_memory = self._get_memory_usage()
                
                # Test API latency
                latencies = []
                for _ in range(10):
                    latency, status, _ = measure_request_latency(
                        session,
                        f"{self.api_base}/sources/",
                        params={"limit": 100},
                    )
                    if status == 200:
                        latencies.append(latency)
                
                p95_latency = calculate_percentiles(latencies)["p95"] if latencies else 999
                
                checkpoint = {
                    "source_count": initial_sources + sources_created,
                    "memory_mib": current_memory,
                    "api_p95_latency": p95_latency,
                }
                checkpoints.append(checkpoint)
                
                # Check for degradation
                if p95_latency > latency_threshold_seconds:
                    breaking_point = checkpoint
                    break
                
                if batch_created < batch_size:
                    break
        
        perf_result.metrics = {
            "max_sources_attempted": max_sources,
            "sources_added": sources_created,
            "initial_sources": initial_sources,
            "final_sources": initial_sources + sources_created,
            "initial_memory_mib": initial_memory,
            "breaking_point": breaking_point,
            "checkpoints": checkpoints,
            "degradation_detected": breaking_point is not None,
        }
        perf_result.timings = perf_timer.get_timings()
        # Require at least half the target sources to be created, not just one batch.
        # With batch_size=5 and max_sources=25, 1 batch is only 20% of the target.
        min_required = max(batch_size, max_sources // 2)
        perf_result.passed = sources_created >= min_required
        
        perf_collector.add_result(perf_result)
    
    @pytest.mark.skipif(
        _ACTIVE_PROFILE == "baseline",
        reason=_SCALE_003_SKIP_REASON,
    )
    def test_perf_scale_003_large_namespace_count(
        self,
        cluster_config: ClusterConfig,
        database_config,
        rh_identity_header: str,
        perf_timer: PerfTimer,
        perf_result: PerformanceResult,
        perf_collector: PerfResultCollector,
    ):
        """PERF-SCALE-003: Large source dataset - 1 source with many namespaces.
        
        Tests with a single source containing large amounts of data.
        Uses 'large' profile which has 30 namespaces × 10 pods = 300 pods per cluster.
        
        Metrics:
        - Query time vs namespace count
        - Memory pressure
        """
        if not ensure_nise_available():
            pytest.skip("NISE not available")
        
        profile_name = "large"
        profile = PROFILES[profile_name]
        
        session = self._get_authenticated_session()
        
        # Note: This test assumes data has already been loaded via PERF-ING tests
        # It measures query performance against existing large datasets
        
        # Test queries with namespace filtering
        query_results = []
        
        with perf_timer.measure("namespace_queries"):
            # Query all namespaces
            latency, status, response = measure_request_latency(
                session,
                f"{self.api_base}/reports/openshift/costs/",
                params={
                    "filter[time_scope_units]": "month",
                    "filter[time_scope_value]": "-1",
                    "group_by[project]": "*",
                },
            )
            query_results.append({
                "query": "all_namespaces",
                "latency": latency,
                "status": status,
                "namespace_count": len(response.get("data", [])) if response else 0,
            })
            
            # Query with limit
            for limit in [10, 50, 100]:
                latency, status, response = measure_request_latency(
                    session,
                    f"{self.api_base}/reports/openshift/costs/",
                    params={
                        "filter[time_scope_units]": "month",
                        "filter[time_scope_value]": "-1",
                        "group_by[project]": "*",
                        "limit": limit,
                    },
                )
                query_results.append({
                    "query": f"limit_{limit}",
                    "latency": latency,
                    "status": status,
                })
        
        memory_usage = self._get_memory_usage()
        
        perf_result.metrics = {
            "profile": profile_name,
            "expected_namespaces": profile["namespaces_per_cluster"],
            "expected_pods": profile["namespaces_per_cluster"] * profile["pods_per_namespace"],
            "query_results": query_results,
            "memory_usage_mib": memory_usage,
        }
        perf_result.timings = perf_timer.get_timings()
        perf_result.passed = all(r["status"] == 200 for r in query_results)
        
        perf_collector.add_result(perf_result)
    
    @pytest.mark.parametrize("concurrent_queries", SCALE_004_CONCURRENCY)
    def test_perf_scale_004_concurrent_queries(
        self,
        concurrent_queries: int,
        perf_timer: PerfTimer,
        perf_result: PerformanceResult,
        perf_collector: PerfResultCollector,
    ):
        """PERF-SCALE-004: Concurrent API queries - N parallel report requests.
        
        Tests API scaling under concurrent query load.
        
        Metrics:
        - P50/P95/P99 latency
        - Throughput (queries/second)
        """
        queries_per_worker = 10
        
        def run_queries(worker_id: int) -> List[Dict[str, Any]]:
            session = self._get_authenticated_session()
            results = []
            
            # Mix of query types
            query_types = [
                {
                    "name": "costs_daily",
                    "endpoint": "/reports/openshift/costs/",
                    "params": {
                        "filter[time_scope_units]": "month",
                        "filter[time_scope_value]": "-1",
                        "filter[resolution]": "daily",
                    },
                },
                {
                    "name": "compute_grouped",
                    "endpoint": "/reports/openshift/compute/",
                    "params": {
                        "filter[time_scope_units]": "month",
                        "filter[time_scope_value]": "-1",
                        "group_by[project]": "*",
                    },
                },
                {
                    "name": "sources_list",
                    "endpoint": "/sources/",
                    "params": {"limit": 100},
                },
            ]
            
            for i in range(queries_per_worker):
                query = query_types[i % len(query_types)]
                latency, status, _ = measure_request_latency(
                    session,
                    f"{self.api_base}{query['endpoint']}",
                    params=query["params"],
                )
                results.append({
                    "worker": worker_id,
                    "query": query["name"],
                    "latency": latency,
                    "status": status,
                })
            
            return results
        
        all_results = []
        
        with perf_timer.measure("concurrent_queries"):
            with ThreadPoolExecutor(max_workers=concurrent_queries) as executor:
                futures = {executor.submit(run_queries, i): i for i in range(concurrent_queries)}
                
                for future in as_completed(futures):
                    all_results.extend(future.result())
        
        total_duration = perf_timer.get_timing("concurrent_queries").duration_seconds
        all_latencies = [r["latency"] for r in all_results]
        success_count = sum(1 for r in all_results if r["status"] == 200)
        
        perf_result.metrics = {
            "concurrent_workers": concurrent_queries,
            "queries_per_worker": queries_per_worker,
            "total_queries": len(all_results),
            "latencies": calculate_percentiles(all_latencies),
            "success_count": success_count,
            "success_rate": round(success_count / len(all_results), 4),
            "queries_per_second": round(len(all_results) / total_duration, 2),
        }
        perf_result.timings = perf_timer.get_timings()
        perf_result.passed = (success_count / len(all_results)) >= 0.95
        
        perf_collector.add_result(perf_result)
        
        assert (success_count / len(all_results)) >= 0.90, "Success rate below 90%"
    
    @pytest.mark.parametrize("date_range_days", SCALE_005_DAYS)
    def test_perf_scale_005_historical_depth(
        self,
        date_range_days: int,
        perf_timer: PerfTimer,
        perf_result: PerformanceResult,
        perf_collector: PerfResultCollector,
    ):
        """PERF-SCALE-005: Historical data depth - Query time vs date range.
        
        Tests query performance across different historical ranges.
        
        Metrics:
        - Query time vs date range
        - Data volume returned
        """
        session = self._get_authenticated_session()
        iterations = 10
        
        query_results = []
        
        with perf_timer.measure(f"historical_{date_range_days}d"):
            for _ in range(iterations):
                latency, status, response = measure_request_latency(
                    session,
                    f"{self.api_base}/reports/openshift/costs/",
                    params={
                        "filter[time_scope_units]": "day",
                        "filter[time_scope_value]": f"-{date_range_days}",
                        "filter[resolution]": "daily",
                    },
                )
                
                data_points = len(response.get("data", [])) if response else 0
                query_results.append({
                    "latency": latency,
                    "status": status,
                    "data_points": data_points,
                })
        
        latencies = [r["latency"] for r in query_results if r["status"] == 200]
        
        perf_result.metrics = {
            "date_range_days": date_range_days,
            "iterations": iterations,
            "latencies": calculate_percentiles(latencies),
            "avg_data_points": sum(r["data_points"] for r in query_results) / len(query_results),
            "success_rate": sum(1 for r in query_results if r["status"] == 200) / len(query_results),
        }
        perf_result.timings = perf_timer.get_timings()
        perf_result.passed = len(latencies) >= iterations * 0.9
        
        perf_collector.add_result(perf_result)
        
        # Longer ranges should still complete in reasonable time
        max_expected_latency = 2.0 + (date_range_days / 30)  # Scale with date range
        if latencies:
            p95 = calculate_percentiles(latencies)["p95"]
            assert p95 < max_expected_latency, (
                f"P95 latency {p95}s exceeds {max_expected_latency}s for {date_range_days}d range"
            )
