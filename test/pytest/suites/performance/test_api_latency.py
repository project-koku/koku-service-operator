"""
API Latency Performance Tests (PERF-API-*).

Measures API response times under various conditions per FLPATH-4036.

Test IDs:
- PERF-API-001: Report API baseline
- PERF-API-002: Report API under load
- PERF-API-003: Cost model CRUD
- PERF-API-004: Source list pagination
- PERF-API-005: Complex group-by query
- PERF-API-006: Tag filtering
"""

import json
import os
import time
from concurrent.futures import ThreadPoolExecutor, as_completed
from datetime import datetime, timedelta, timezone
from typing import Any, Dict, List, Optional, Tuple

import pytest
import requests

from conftest import ClusterConfig, JWTToken, obtain_jwt_token

from .data_classes import PerformanceResult, TimingMetric
from .helpers import (
    PerfResultCollector,
    PerfTimer,
    create_authenticated_session,
    get_timeout_for_profile,
)
from .k8s_helpers import calculate_percentiles
from .profiles import ACTIVE_PROFILE as _ACTIVE_PROFILE


# =============================================================================
# Profile-Aware Thresholds
# =============================================================================

# API-005 P95 latency thresholds by profile.
# Larger profiles have more data, causing longer query times for complex group-by.
_API_005_P95_THRESHOLDS = {
    "baseline": 10.0,   # 10s for minimal data
    "small":    22.0,   # 22s for 15 nodes, 10 namespaces (observed P95: ~19.5s)
    "medium":   25.0,   # 25s for larger datasets
    "large":    30.0,   # 30s for production-scale data
}
API_005_P95_THRESHOLD = _API_005_P95_THRESHOLDS.get(_ACTIVE_PROFILE, 10.0)

# API-006 tag filtering: the labeled_nise_source fixture must create a source,
# upload data, wait for Koku to process it AND summarize pod_labels, then poll
# the tags API.  On larger profiles the celery workers are under heavier load,
# so the summarization step takes much longer.
_API_006_BASE_TIMEOUT = 300  # baseline: 5 min is sufficient
API_006_TIMEOUT = get_timeout_for_profile(_API_006_BASE_TIMEOUT, _ACTIVE_PROFILE)


# =============================================================================
# Helper Functions
# =============================================================================

def measure_request_latency(
    session: requests.Session,
    url: str,
    method: str = "GET",
    json_data: Optional[Dict] = None,
    params: Optional[Dict] = None,
) -> Tuple[float, int, Optional[Dict]]:
    """Measure latency of a single request.
    
    Returns:
        Tuple of (latency_seconds, status_code, response_json or None)
    """
    start = time.time()
    
    try:
        if method == "GET":
            response = session.get(url, params=params, timeout=60)
        elif method == "POST":
            response = session.post(url, json=json_data, timeout=60)
        elif method == "PUT":
            response = session.put(url, json=json_data, timeout=60)
        elif method == "DELETE":
            response = session.delete(url, timeout=60)
        else:
            raise ValueError(f"Unsupported method: {method}")
        
        latency = time.time() - start
        
        try:
            json_response = response.json() if response.content else None
        except json.JSONDecodeError:
            json_response = None
        
        return latency, response.status_code, json_response
    
    except requests.exceptions.RequestException as e:
        return time.time() - start, 0, {"error": str(e)}




def run_latency_test(
    session: requests.Session,
    url: str,
    iterations: int = 10,
    method: str = "GET",
    json_data: Optional[Dict] = None,
    params: Optional[Dict] = None,
    warmup_iterations: int = 2,
) -> Dict[str, Any]:
    """Run multiple iterations and return latency statistics."""
    
    # Warmup
    for _ in range(warmup_iterations):
        measure_request_latency(session, url, method, json_data, params)
    
    # Actual measurements
    latencies = []
    status_codes = []
    errors = []
    
    for i in range(iterations):
        latency, status, response = measure_request_latency(
            session, url, method, json_data, params
        )
        latencies.append(latency)
        status_codes.append(status)
        
        if status not in (200, 201, 204):
            errors.append({
                "iteration": i,
                "status": status,
                "response": response,
            })
    
    success_rate = sum(1 for s in status_codes if s in (200, 201, 204)) / iterations
    
    return {
        "url": url,
        "method": method,
        "iterations": iterations,
        "latencies": calculate_percentiles(latencies),
        "success_rate": round(success_rate, 4),
        "errors": errors[:5],  # Limit to first 5 errors
    }


# =============================================================================
# Test Classes
# =============================================================================

@pytest.mark.performance
@pytest.mark.api_latency
class TestAPILatency:
    """API latency performance tests."""
    
    @pytest.fixture(autouse=True)
    def setup(
        self,
        cluster_config: ClusterConfig,
        gateway_url: str,
        keycloak_config,
    ):
        """Setup for API latency tests."""
        self.namespace = cluster_config.namespace
        self.gateway_url = gateway_url
        self._keycloak_config = keycloak_config
        
        # API base URL via gateway
        self.api_base = f"{gateway_url}/cost-management/v1"
    
    def _get_authenticated_session(self) -> requests.Session:
        """Get a session with fresh JWT token."""
        return create_authenticated_session(self._keycloak_config)
    
    @pytest.mark.parametrize("iterations", [10, 50])
    def test_perf_api_001_report_baseline(
        self,
        iterations: int,
        perf_timer: PerfTimer,
        perf_result: PerformanceResult,
        perf_collector: PerfResultCollector,
    ):
        """PERF-API-001: Report API baseline - Single report query, no load.
        
        Measures baseline latency for cost report queries.
        """
        session = self._get_authenticated_session()
        
        # Test different report endpoints
        endpoints = [
            "/reports/openshift/costs/",
            "/reports/openshift/memory/",
            "/reports/openshift/compute/",
        ]
        
        results = {}
        
        with perf_timer.measure("report_api_baseline"):
            for endpoint in endpoints:
                url = f"{self.api_base}{endpoint}"
                result = run_latency_test(
                    session,
                    url,
                    iterations=iterations,
                    params={
                        "filter[time_scope_units]": "month",
                        "filter[time_scope_value]": "-1",
                        "filter[resolution]": "daily",
                    },
                )
                results[endpoint] = result
        
        # Aggregate metrics
        all_p95 = [r["latencies"]["p95"] for r in results.values()]
        all_success = [r["success_rate"] for r in results.values()]
        
        perf_result.metrics = {
            "iterations": iterations,
            "endpoints_tested": len(endpoints),
            "results": results,
            "aggregate_p95": round(max(all_p95), 4),
            "aggregate_success_rate": round(min(all_success), 4),
        }
        perf_result.timings = perf_timer.get_timings()
        perf_result.passed = all(r["success_rate"] >= 0.95 for r in results.values())
        
        perf_collector.add_result(perf_result)
        
        for endpoint, result in results.items():
            assert result["success_rate"] >= 0.95, (
                f"Success rate for {endpoint}: {result['success_rate']:.0%} below 95%"
            )
            assert result["latencies"]["p95"] < 5.0, (
                f"P95 latency for {endpoint} exceeds 5s: {result['latencies']['p95']}s"
            )
    
    @pytest.mark.parametrize("concurrent_users", [5, 10, 20])
    def test_perf_api_002_report_under_load(
        self,
        concurrent_users: int,
        perf_timer: PerfTimer,
        perf_result: PerformanceResult,
        perf_collector: PerfResultCollector,
    ):
        """PERF-API-002: Report API under load - N concurrent report queries.
        
        Tests API behavior under concurrent request load.
        """
        requests_per_user = 10
        
        def make_requests(user_id: int) -> List[Tuple[float, int]]:
            session = self._get_authenticated_session()
            results = []
            
            for i in range(requests_per_user):
                latency, status, _ = measure_request_latency(
                    session,
                    f"{self.api_base}/reports/openshift/costs/",
                    params={
                        "filter[time_scope_units]": "month",
                        "filter[time_scope_value]": "-1",
                    },
                )
                results.append((latency, status))
            
            return results
        
        all_latencies = []
        all_statuses = []
        
        with perf_timer.measure("concurrent_load_test"):
            with ThreadPoolExecutor(max_workers=concurrent_users) as executor:
                futures = {executor.submit(make_requests, i): i for i in range(concurrent_users)}
                
                for future in as_completed(futures):
                    results = future.result()
                    for latency, status in results:
                        all_latencies.append(latency)
                        all_statuses.append(status)
        
        latency_stats = calculate_percentiles(all_latencies)
        success_count = sum(1 for s in all_statuses if s == 200)
        total_requests = concurrent_users * requests_per_user
        
        perf_result.metrics = {
            "concurrent_users": concurrent_users,
            "requests_per_user": requests_per_user,
            "total_requests": total_requests,
            "latencies": latency_stats,
            "success_count": success_count,
            "success_rate": round(success_count / total_requests, 4),
            "requests_per_second": round(
                total_requests / perf_timer.get_timing("concurrent_load_test").duration_seconds,
                2,
            ),
        }
        perf_result.timings = perf_timer.get_timings()
        perf_result.passed = (success_count / total_requests) >= 0.95
        
        perf_collector.add_result(perf_result)
        
        # Assert success rate >= 95%
        assert success_count / total_requests >= 0.95, (
            f"Success rate {success_count}/{total_requests} below 95%"
        )
    
    def test_perf_api_003_cost_model_crud(
        self,
        perf_timer: PerfTimer,
        perf_result: PerformanceResult,
        perf_collector: PerfResultCollector,
    ):
        """PERF-API-003: Cost model CRUD - Create/read/update/delete cycle.
        
        Measures cost model operations throughput.
        """
        session = self._get_authenticated_session()
        iterations = int(os.environ.get("PERF_API_003_ITERATIONS", "10"))
        
        create_latencies = []
        read_latencies = []
        update_latencies = []
        delete_latencies = []
        errors = []
        
        with perf_timer.measure("cost_model_crud"):
            for i in range(iterations):
                cost_model_name = f"perf-test-cm-{i:03d}-{int(time.time())}"
                
                # CREATE
                create_data = {
                    "name": cost_model_name,
                    "description": f"Performance test cost model {i}",
                    "source_type": "OCP",
                    "rates": [
                        {
                            "metric": {"name": "cpu_core_usage_per_hour"},
                            "tiered_rates": [{"value": 0.01, "unit": "USD"}],
                        }
                    ],
                }
                
                latency, status, response = measure_request_latency(
                    session,
                    f"{self.api_base}/cost-models/",
                    method="POST",
                    json_data=create_data,
                )
                create_latencies.append(latency)
                
                if status not in (200, 201) or not response or "uuid" not in response:
                    errors.append({"operation": "create", "iteration": i, "status": status})
                    continue
                
                cost_model_uuid = response["uuid"]
                
                # READ
                latency, status, _ = measure_request_latency(
                    session,
                    f"{self.api_base}/cost-models/{cost_model_uuid}/",
                )
                read_latencies.append(latency)
                
                # UPDATE
                update_data = {
                    "name": cost_model_name,
                    "description": f"Updated performance test {i}",
                    "source_type": "OCP",
                    "rates": [
                        {
                            "metric": {"name": "cpu_core_usage_per_hour"},
                            "tiered_rates": [{"value": 0.02, "unit": "USD"}],
                        }
                    ],
                }
                
                latency, status, _ = measure_request_latency(
                    session,
                    f"{self.api_base}/cost-models/{cost_model_uuid}/",
                    method="PUT",
                    json_data=update_data,
                )
                update_latencies.append(latency)
                
                # DELETE
                latency, status, _ = measure_request_latency(
                    session,
                    f"{self.api_base}/cost-models/{cost_model_uuid}/",
                    method="DELETE",
                )
                delete_latencies.append(latency)
        
        perf_result.metrics = {
            "iterations": iterations,
            "create_latencies": calculate_percentiles(create_latencies),
            "read_latencies": calculate_percentiles(read_latencies),
            "update_latencies": calculate_percentiles(update_latencies),
            "delete_latencies": calculate_percentiles(delete_latencies),
            "errors": errors,
            "operations_per_second": round(
                (iterations * 4) / perf_timer.get_timing("cost_model_crud").duration_seconds,
                2,
            ),
        }
        perf_result.timings = perf_timer.get_timings()
        perf_result.passed = len(errors) == 0
        
        perf_collector.add_result(perf_result)
        
        assert len(errors) == 0, f"CRUD operations had errors: {errors}"
    
    @pytest.mark.parametrize("page_size", [10, 50, 100])
    def test_perf_api_004_source_pagination(
        self,
        page_size: int,
        perf_timer: PerfTimer,
        perf_result: PerformanceResult,
        perf_collector: PerfResultCollector,
    ):
        """PERF-API-004: Source list pagination - List sources with pagination.
        
        Tests pagination performance with varying page sizes.
        """
        session = self._get_authenticated_session()
        iterations = 20
        
        with perf_timer.measure("source_pagination"):
            result = run_latency_test(
                session,
                f"{self.api_base}/sources/",
                iterations=iterations,
                params={
                    "limit": page_size,
                    "offset": 0,
                },
            )
        
        perf_result.metrics = {
            "page_size": page_size,
            "iterations": iterations,
            "latencies": result["latencies"],
            "success_rate": result["success_rate"],
        }
        perf_result.timings = perf_timer.get_timings()
        perf_result.passed = (
            result["success_rate"] >= 0.95 and
            result["latencies"]["p95"] < 3.0
        )
        
        perf_collector.add_result(perf_result)
        
        assert result["success_rate"] >= 0.95, (
            f"Success rate {result['success_rate']:.0%} below 95% threshold"
        )
        assert result["latencies"]["p95"] < 3.0, (
            f"P95 latency for page_size={page_size} exceeds 3s"
        )
    
    @pytest.mark.parametrize("group_by_dims", [
        ["project"],
        ["project", "node"],
        ["project", "cluster"],
    ], ids=["1-dim", "2-dim-node", "2-dim-cluster"])
    def test_perf_api_005_complex_group_by(
        self,
        group_by_dims: List[str],
        perf_timer: PerfTimer,
        perf_result: PerformanceResult,
        perf_collector: PerfResultCollector,
    ):
        """PERF-API-005: Complex group-by query - Multi-dimension grouping.
        
        Tests query performance with increasing grouping complexity.
        The Koku API enforces a maximum of 2 group_by dimensions.
        """
        session = self._get_authenticated_session()
        iterations = 10
        
        # Build group_by params
        params = {
            "filter[time_scope_units]": "day",
            "filter[time_scope_value]": "-30",
            "filter[resolution]": "daily",
        }
        for dim in group_by_dims:
            params[f"group_by[{dim}]"] = "*"
        
        with perf_timer.measure("complex_group_by"):
            result = run_latency_test(
                session,
                f"{self.api_base}/reports/openshift/costs/",
                iterations=iterations,
                params=params,
            )
        
        perf_result.metrics = {
            "group_by_dimensions": group_by_dims,
            "dimension_count": len(group_by_dims),
            "iterations": iterations,
            "latencies": result["latencies"],
            "success_rate": result["success_rate"],
        }
        perf_result.timings = perf_timer.get_timings()
        perf_result.passed = (
            result["success_rate"] >= 0.90 and
            result["latencies"]["p95"] < API_005_P95_THRESHOLD
        )
        
        perf_collector.add_result(perf_result)
        
        assert result["success_rate"] >= 0.90, (
            f"Success rate {result['success_rate']:.0%} below 90% threshold"
        )
        assert result["latencies"]["p95"] < API_005_P95_THRESHOLD, (
            f"P95 latency for {len(group_by_dims)}-dim group_by exceeds {API_005_P95_THRESHOLD}s ({_ACTIVE_PROFILE} profile)"
        )
    
    @pytest.mark.timeout(API_006_TIMEOUT)
    @pytest.mark.parametrize("tag_count", [1, 5, 10])
    def test_perf_api_006_tag_filtering(
        self,
        tag_count: int,
        perf_timer: PerfTimer,
        perf_result: PerformanceResult,
        perf_collector: PerfResultCollector,
        labeled_nise_source,
    ):
        """PERF-API-006: Tag filtering - Filter by N tags.
        
        Tests query performance with increasing tag filter complexity.
        Uses the labeled_nise_source fixture which creates its own NISE data
        with pod labels, uploads it, and ensures tags are enabled.
        
        The fixture provides tags from NISE profile labels:
        - environment, app, tier (from pod labels)
        - kubernetes_io_os (from node labels)
        - openshift_io_cluster_monitoring, cost_management (from namespace labels)
        
        If insufficient tags: fails, indicating a fixture/NISE issue.
        """
        session = self._get_authenticated_session()
        iterations = 10
        
        available_tags = labeled_nise_source["available_tags"]
        expected_labels = labeled_nise_source["expected_labels"]
        db_tags = labeled_nise_source.get("db_tags", [])
        
        print(f"\n[API-006] Source: {labeled_nise_source['source_name']}")
        print(f"[API-006] Tags in DB summary table: {db_tags}")
        print(f"[API-006] Tags from API: {available_tags}")
        print(f"[API-006] Expected NISE labels: {expected_labels}")
        
        if len(available_tags) < tag_count:
            missing_from_api = [l for l in expected_labels if l not in available_tags]
            missing_from_db = [l for l in expected_labels if l not in db_tags]
            
            if missing_from_db:
                pytest.fail(
                    f"NISE labels never reached summary table. "
                    f"Missing from DB: {missing_from_db}. "
                    f"DB has: {db_tags}. "
                    f"Root cause is in NISE CSV generation or Koku processing, "
                    f"not tag enablement."
                )
            elif missing_from_api:
                pytest.fail(
                    f"Tags exist in DB but not in API. "
                    f"In DB: {[l for l in expected_labels if l in db_tags]}. "
                    f"Missing from API: {missing_from_api}. "
                    f"Tag indexing or enablement issue — data is present but "
                    f"not yet discoverable via /tags/ endpoint."
                )
            else:
                pytest.skip(
                    f"Environment has only {len(available_tags)} tags, need {tag_count}. "
                    f"Expected NISE labels ({expected_labels}) are present — infrastructure OK."
                )
        
        selected_tags = available_tags[:tag_count]
        
        params = {
            "filter[time_scope_units]": "day",
            "filter[time_scope_value]": "-10",
        }
        for tag_key in selected_tags:
            params[f"filter[tag:{tag_key}]"] = "*"
        
        with perf_timer.measure("tag_filtering"):
            result = run_latency_test(
                session,
                f"{self.api_base}/reports/openshift/costs/",
                iterations=iterations,
                params=params,
            )
        
        perf_result.metrics = {
            "tag_count": tag_count,
            "tag_keys_used": selected_tags,
            "iterations": iterations,
            "latencies": result["latencies"],
            "success_rate": result["success_rate"],
        }
        perf_result.timings = perf_timer.get_timings()
        perf_result.passed = (
            result["success_rate"] >= 0.95 and
            result["latencies"]["p95"] < 5.0
        )
        
        perf_collector.add_result(perf_result)
        
        assert result["success_rate"] >= 0.95, (
            f"Success rate {result['success_rate']:.0%} below 95% threshold"
        )
        assert result["latencies"]["p95"] < 5.0, (
            f"P95 latency for {tag_count} tags exceeds 5s"
        )


@pytest.mark.performance
@pytest.mark.api_latency
class TestAPIHealthCheck:
    """Quick API health and latency checks."""
    
    @pytest.fixture(autouse=True)
    def setup(self, gateway_url: str, keycloak_config):
        self.gateway_url = gateway_url
        self._keycloak_config = keycloak_config
        self.api_base = f"{gateway_url}/cost-management/v1"
    
    def test_api_status_latency(
        self,
        perf_timer: PerfTimer,
        perf_result: PerformanceResult,
        perf_collector: PerfResultCollector,
    ):
        """Quick test of API status endpoint latency."""
        token = obtain_jwt_token(self._keycloak_config)
        session = requests.Session()
        session.headers.update(token.authorization_header)
        session.verify = False
        
        with perf_timer.measure("status_check"):
            result = run_latency_test(
                session,
                f"{self.api_base}/status/",
                iterations=20,
            )
        
        perf_result.metrics = result
        perf_result.timings = perf_timer.get_timings()
        perf_result.passed = result["latencies"]["p95"] < 1.0
        
        perf_collector.add_result(perf_result)
        
        # Status should be very fast
        assert result["latencies"]["p95"] < 1.0, "Status endpoint P95 > 1s"
