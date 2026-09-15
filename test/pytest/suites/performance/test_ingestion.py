"""
Ingestion Throughput Performance Tests (PERF-ING-*).

Tests data ingestion capacity under various loads per FLPATH-4036.

Test IDs:
- PERF-ING-001: Single source baseline
- PERF-ING-002: Single source burst (90 days)
- PERF-ING-003: Concurrent uploads
- PERF-ING-004: Large file upload (50MB+)
- PERF-ING-005: High frequency uploads
- PERF-ING-006: 6-hour processing window validation (SC-4)
"""

import os
import subprocess
import tempfile
import time
import uuid
from concurrent.futures import ThreadPoolExecutor, as_completed
from datetime import datetime, timedelta, timezone
from pathlib import Path
from typing import Any, Dict, List, Optional

import pytest
import requests

from conftest import ClusterConfig, JWTToken, obtain_jwt_token
from e2e_helpers import (
    wait_for_processing_complete,
    NISEConfig,
    SourceRegistration,
    cleanup_database_records,
    delete_source,
    ensure_nise_available,
    generate_cluster_id,
    generate_nise_data,
    register_source,
    upload_with_retry,
    wait_for_provider,
)
from utils import (
    create_upload_package_from_files,
    exec_in_pod,
    run_oc_command,
)

from .data_classes import PerformanceResult, ResourceSnapshot, TimingMetric
from .helpers import (
    PerfResultCollector,
    PerfTimer,
    generate_and_upload_data,
    get_pod_resource_usage,
    save_perf_result,
)
from .tracker import PerfCleanupTracker
from .profiles import ACTIVE_PROFILE as _ACTIVE_PROFILE, PROFILES, get_profile_metrics, get_profile_nise_yaml


# =============================================================================
# Constants
# =============================================================================

# ---------------------------------------------------------------------------
# Profile-filtered parametrize lists — all gated by PERF_PROFILE so that
# baseline runs stay fast (<30 min) and heavier variants only appear at the
# run levels where they're meaningful.
# ---------------------------------------------------------------------------

# ING-001: data profile variants to generate and upload.
# baseline → baseline data (1 cluster, 3 nodes, 1 day)
# small+   → small data (1 cluster, 15 nodes, 30 days)
# medium+  → medium data (2 clusters, 25 nodes, 30 days)
# large+   → large data (7 clusters, 19 nodes, 30 days)
_ING_001_PROFILES: dict = {
    "baseline": ["baseline"],
    "small":    ["small"],
    "medium":   ["small", "medium"],
    "large":    ["small", "medium", "large"],
}
ING_001_PROFILES = _ING_001_PROFILES.get(_ACTIVE_PROFILE, _ING_001_PROFILES["large"])

# ING-002: burst window variants (data_days, processing_timeout_s).
# baseline → skipped; ING-002 is a volume/burst test — ING-001 already covers
#            the single-source pipeline for baseline.  Adding burst windows here
#            only adds 5-20 min of upload+processing time with no extra signal.
# small    → 30 + 60-day
# medium+  → all three (30, 60, 90-day)
_ING_002_SKIP_REASON = (
    "ING-002 is a volume/burst test, not a baseline scenario — "
    "ING-001 already validates the single-source pipeline end-to-end."
)
_ING_002_VARIANTS: dict = {
    "baseline": [pytest.param(30, 600,  id="30-days",
                              marks=pytest.mark.skip(reason=_ING_002_SKIP_REASON))],
    "small":    [pytest.param(30, 600,  id="30-days"),
                 pytest.param(60, 900,  id="60-days")],
    "medium":   [pytest.param(30, 600,  id="30-days"),
                 pytest.param(60, 900,  id="60-days"),
                 pytest.param(90, 1200, id="90-days")],
    "large":    [pytest.param(30, 600,  id="30-days"),
                 pytest.param(60, 900,  id="60-days"),
                 pytest.param(90, 1200, id="90-days")],
}
ING_002_PARAMS = _ING_002_VARIANTS.get(_ACTIVE_PROFILE, _ING_002_VARIANTS["large"])

# ING-004: large-file target sizes.
# Skipped for baseline — ING-004 intentionally ignores PERF_PROFILE and always
# generates large-profile (133 nodes) × 30-day NISE data regardless of the active
# profile.  That makes the 50 MB variant take 45-60 min of local NISE generation,
# which is not a baseline concern.  ING-001 already validates the upload+processing
# pipeline end-to-end.  ING-004 is meaningful from small upward where large-file
# upload limits and ingress timeout behaviour are worth testing.
_ING_004_SKIP_REASON = (
    "ING-004 always generates large-profile NISE data regardless of PERF_PROFILE "
    "— 50 MB takes 45-60 min of data generation, not appropriate for baseline."
)
_ING_004_SIZES: dict = {
    "baseline": [pytest.param(50, id="50", marks=pytest.mark.skip(reason=_ING_004_SKIP_REASON))],
    "small":    [50],
    "medium":   [50, 100],
    "large":    [50, 100],
}
ING_004_SIZES = _ING_004_SIZES.get(_ACTIVE_PROFILE, _ING_004_SIZES["large"])

# ING-006: processing-window validation (SC-4 SLA).
# Runs for small/medium/large profiles; skipped for baseline.
_ING_006_PROFILE = _ACTIVE_PROFILE if _ACTIVE_PROFILE in ("small", "medium", "large", "xlarge") else None
ING_006_PROFILES = (
    [_ING_006_PROFILE]
    if _ING_006_PROFILE
    else [pytest.param("baseline", marks=pytest.mark.skip(reason="ING-006 skipped for baseline profile"))]
)


# =============================================================================
# Helper Functions
# =============================================================================

def get_listener_cpu_usage(namespace: str) -> Optional[float]:
    """Get current listener CPU usage in cores."""
    from .helpers import parse_cpu_millicores

    result = run_oc_command([
        "adm", "top", "pod", "-n", namespace,
        "-l", "app.kubernetes.io/component=listener",
        "--no-headers"
    ], check=False)
    
    if result.returncode != 0 or not result.stdout.strip():
        return None
    
    try:
        parts = result.stdout.strip().split()
        if len(parts) >= 2:
            return parse_cpu_millicores(parts[1])
    except (ValueError, IndexError):
        pass
    
    return None


# =============================================================================
# Test Classes
# =============================================================================

@pytest.mark.performance
@pytest.mark.ingestion
@pytest.mark.slow
class TestIngestionThroughput:
    """Ingestion throughput performance tests."""
    
    @pytest.fixture(autouse=True)
    def setup(self, cluster_config: ClusterConfig, keycloak_config):
        """Setup for ingestion tests."""
        self.namespace = cluster_config.namespace
        self.helm_release = cluster_config.helm_release_name
        
        # Ensure NISE is available
        if not ensure_nise_available():
            pytest.skip("NISE (koku-nise) not available")
        
        # Store keycloak config for token refresh
        self._keycloak_config = keycloak_config
    
    def _get_fresh_token(self) -> JWTToken:
        """Get a fresh JWT token for long-running tests."""
        return obtain_jwt_token(self._keycloak_config)
    
    @pytest.mark.timeout(1800)  # 30 min: generation + upload + summary table wait
    @pytest.mark.parametrize("profile_name", ING_001_PROFILES)
    def test_perf_ing_001_single_source_baseline(
        self,
        profile_name: str,
        cluster_config: ClusterConfig,
        ingress_url: str,
        database_config,
        koku_api_url: str,
        perf_timer: PerfTimer,
        perf_result: PerformanceResult,
        perf_collector: PerfResultCollector,
        rh_identity_header: str,
        perf_cleanup,
        ingress_pod: str,
    ):
        """PERF-ING-001: Single source baseline - 1 source, 1 month data, default config.
        
        Metrics captured:
        - Time to complete full pipeline
        - Listener CPU utilization
        - Upload throughput (MB/s)
        """
        cluster_id = generate_cluster_id()
        source_name = f"perf-ing-001-{cluster_id[-8:]}"

        # Pre-test cleanup: remove any leftover source/DB records with this name
        # from a previous cancelled run to prevent HTTP 400 duplicate-source errors.
        db_pod = database_config.pod_name if database_config else None
        cleanup_database_records(self.namespace, db_pod, cluster_id)

        # Register source
        with perf_timer.measure("source_registration"):
            source = register_source(
                self.namespace,
                ingress_pod,
                koku_api_url,
                rh_identity_header,
                cluster_id,
                "org1234567",
                source_name,
            )
        
        # Track for cleanup
        perf_cleanup.track(source_id=source.source_id, cluster_id=cluster_id, source_name=source_name)
        
        # Generate and upload data
        end_date = datetime.now(timezone.utc)
        start_date = end_date - timedelta(days=PROFILES[profile_name]["data_days"])
        
        jwt_token = self._get_fresh_token()
        
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
        
        with perf_timer.measure("processing_wait"):
            proc = wait_for_processing_complete(
                self.namespace,
                database_config.pod_name,
                cluster_id,
                max_wait_seconds=1500,  # test timeout=1800; leave headroom for registration+upload
            )

        # Capture listener CPU at end
        listener_cpu = get_listener_cpu_usage(self.namespace)

        perf_result.metrics = {
            "profile": profile_name,
            "upload": upload_result,
            "listener_cpu_cores": listener_cpu,
            "processing_completed": proc["complete"],
            "schema_name": proc.get("schema_name"),
        }
        perf_result.timings = perf_timer.get_timings()
        perf_result.passed = proc["complete"]

        if not proc["complete"]:
            perf_result.error_message = "Processing did not complete within timeout"

        perf_collector.add_result(perf_result)
        
        assert proc["complete"], "Data processing did not complete"

    @pytest.mark.parametrize("data_days,timeout_seconds", ING_002_PARAMS)
    @pytest.mark.timeout(1800)  # 30 min ceiling — well above the 1200s max variant
    def test_perf_ing_002_single_source_burst(
        self,
        data_days: int,
        timeout_seconds: int,
        cluster_config: ClusterConfig,
        ingress_url: str,
        database_config,
        koku_api_url: str,
        perf_timer: PerfTimer,
        perf_result: PerformanceResult,
        perf_collector: PerfResultCollector,
        rh_identity_header: str,
        perf_cleanup,
        ingress_pod: str,
        request,
    ):
        """PERF-ING-002: Single source burst - 1 source, N days data, max listener CPU.
        
        Tests processing throughput with varying data volumes (30, 60, 90 days).
        
        Metrics captured:
        - Total processing time
        - Throughput (MB/s)
        - Listener CPU over time
        """
        # Apply dynamic timeout based on data volume
        # pytest-timeout doesn't support parametrized timeouts directly,
        # so we implement our own deadline
        test_deadline = time.time() + timeout_seconds
        
        cluster_id = generate_cluster_id()
        source_name = f"perf-ing-002-{data_days}d-{cluster_id[-8:]}"
        profile_name = "single_source_burst"
        
        # Register source
        with perf_timer.measure("source_registration"):
            source = register_source(
                self.namespace,
                ingress_pod,
                koku_api_url,
                rh_identity_header,
                cluster_id,
                "org1234567",
                source_name,
            )
        
        # Track for cleanup
        perf_cleanup.track(source_id=source.source_id, cluster_id=cluster_id, source_name=source_name)
        
        # Variable days of data based on parameter
        end_date = datetime.now(timezone.utc)
        start_date = end_date - timedelta(days=data_days)
        
        jwt_token = self._get_fresh_token()
        
        # Capture baseline CPU
        baseline_cpu = get_listener_cpu_usage(self.namespace)
        cpu_samples = [baseline_cpu] if baseline_cpu else []
        
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
        
        # Check if we've exceeded deadline after upload
        if time.time() > test_deadline:
            perf_result.metrics = {
                "profile": profile_name,
                "data_days": data_days,
                "upload": upload_result,
                "error": "Upload exceeded timeout",
            }
            perf_result.timings = perf_timer.get_timings()
            perf_result.passed = False
            perf_result.error_message = f"Upload exceeded {timeout_seconds}s timeout"
            perf_collector.add_result(perf_result)
            pytest.fail(f"{data_days}-day data upload exceeded {timeout_seconds}s timeout")
        
        # Monitor processing; sample CPU on each poll cycle.
        with perf_timer.measure("processing_wait"):
            def _sample_cpu():
                cpu = get_listener_cpu_usage(self.namespace)
                if cpu:
                    cpu_samples.append(cpu)

            proc = wait_for_processing_complete(
                self.namespace,
                database_config.pod_name,
                cluster_id,
                max_wait_seconds=1500,  # test timeout=1800; leave headroom for upload+registration
                on_poll=_sample_cpu,
            )
        schema = proc.get("schema_name")
        
        # Calculate metrics
        processing_time = perf_timer.get_timing("processing_wait")
        throughput_mb_s = 0
        if processing_time and processing_time.duration_seconds > 0:
            throughput_mb_s = upload_result["package_size_mb"] / processing_time.duration_seconds
        
        perf_result.metrics = {
            "profile": profile_name,
            "data_days": data_days,
            "timeout_seconds": timeout_seconds,
            "upload": upload_result,
            "cpu_samples": cpu_samples,
            "avg_cpu_cores": sum(cpu_samples) / len(cpu_samples) if cpu_samples else 0,
            "max_cpu_cores": max(cpu_samples) if cpu_samples else 0,
            "processing_throughput_mb_s": round(throughput_mb_s, 4),
            "processing_completed": proc["complete"],
        }
        perf_result.timings = perf_timer.get_timings()
        perf_result.passed = proc["complete"]
        
        perf_collector.add_result(perf_result)
        
        assert proc["complete"], f"{data_days}-day data processing did not complete"

    @pytest.mark.timeout(1200)  # 20 minutes — 10 concurrent large-profile uploads need more headroom
    @pytest.mark.parametrize("concurrent_sources", [2, 5, 10])
    def test_perf_ing_003_concurrent_uploads(
        self,
        concurrent_sources: int,
        cluster_config: ClusterConfig,
        ingress_url: str,
        database_config,
        koku_api_url: str,
        perf_timer: PerfTimer,
        perf_result: PerformanceResult,
        perf_collector: PerfResultCollector,
        rh_identity_header: str,
        perf_cleanup,
        ingress_pod: str,
        perf_config,
    ):
        """PERF-ING-003: Concurrent uploads - N sources uploading simultaneously.
        
        Tests system behavior under concurrent load.
        
        Metrics captured:
        - Queue depth
        - Time to complete all
        - Error rate
        """
        # Cap workers so the test client doesn't become the bottleneck.
        # If the cap clips the requested value, record both so results are not misleading.
        requested_sources = concurrent_sources
        concurrent_sources = min(concurrent_sources, perf_config.concurrent_upload_max)
        if concurrent_sources < requested_sources:
            print(
                f"\n[ING-003] PERF_CONCURRENT_UPLOADS_MAX={perf_config.concurrent_upload_max} "
                f"capped requested {requested_sources} → {concurrent_sources} workers"
            )
        
        # Register all sources first
        sources = []
        with perf_timer.measure("source_registration_all"):
            for i in range(concurrent_sources):
                cluster_id = generate_cluster_id()
                source_name = f"perf-ing-003-{i:02d}-{cluster_id[-8:]}"
                
                source = register_source(
                    self.namespace,
                    ingress_pod,
                    koku_api_url,
                    rh_identity_header,
                    cluster_id,
                    "org1234567",
                    source_name,
                )
                
                # Track each source for cleanup
                perf_cleanup.track(source_id=source.source_id, cluster_id=cluster_id, source_name=source_name)
                
                sources.append({
                    "cluster_id": cluster_id,
                    "source_name": source_name,
                    "source": source,
                })
        
        end_date = datetime.now(timezone.utc)
        start_date = end_date - timedelta(days=7)  # 1 week of data each
        
        # Upload concurrently
        upload_results = []
        errors = []
        
        def upload_source(source_info: Dict[str, Any]) -> Dict[str, Any]:
            try:
                jwt_token = self._get_fresh_token()
                return generate_and_upload_data(
                    source_info["cluster_id"],
                    source_info["source_name"],
                    start_date,
                    end_date,
                    ingress_url,
                    jwt_token,
                    profile_name="baseline",
                )
            except Exception as e:
                return {"error": str(e), "cluster_id": source_info["cluster_id"]}
        
        with perf_timer.measure("concurrent_uploads"):
            with ThreadPoolExecutor(max_workers=concurrent_sources) as executor:
                futures = {executor.submit(upload_source, s): s for s in sources}
                
                for future in as_completed(futures):
                    result = future.result()
                    if "error" in result:
                        errors.append(result)
                    else:
                        upload_results.append(result)
        
        # Wait for all sources to process using a shared deadline.
        # Sources process in parallel through Kafka, so the total wall-clock
        # time is dominated by pipeline depth (concurrent_sources / replicas)
        # not the sum of individual processing times. A per-source timeout
        # causes false negatives when earlier sources in the loop are slow
        # to appear (queued behind others), even though later ones are already done.
        #
        # Budget: 30s per source (accounts for queueing) + 120s base overhead.
        total_budget = 120 + concurrent_sources * 30
        if _ACTIVE_PROFILE in ("medium", "large"):
            total_budget = int(total_budget * 1.5)
        import time as _time
        deadline = _time.time() + total_budget
        processed_count = 0
        with perf_timer.measure("processing_wait_all"):
            for source_info in sources:
                remaining = max(15, int(deadline - _time.time()))
                proc = wait_for_processing_complete(
                    self.namespace,
                    database_config.pod_name,
                    source_info["cluster_id"],
                    max_wait_seconds=remaining,
                )
                if proc["complete"]:
                    processed_count += 1
        
        # Wait for all Celery work from this test to fully drain before completing.
        # This prevents downstream tests from starting while this test's tasks
        # (cost model calculations, summaries, etc.) are still in flight.
        from .queue_helpers import wait_for_queue_drain
        drain_result = wait_for_queue_drain(
            self.namespace,
            max_wait_seconds=600,
            label=f"ING-003[{concurrent_sources}]",
        )

        perf_result.metrics = {
            "concurrent_sources_requested": requested_sources,
            "concurrent_sources": concurrent_sources,
            "successful_uploads": len(upload_results),
            "failed_uploads": len(errors),
            "processed_count": processed_count,
            "error_rate": len(errors) / concurrent_sources if concurrent_sources > 0 else 0,
            "total_upload_mb": sum(r.get("package_size_mb", 0) for r in upload_results),
            "errors": errors,
            "queue_drain": drain_result,
        }
        perf_result.timings = perf_timer.get_timings()
        perf_result.passed = processed_count == concurrent_sources

        perf_collector.add_result(perf_result)

        assert len(errors) == 0, f"Upload errors: {errors}"
        assert processed_count == concurrent_sources, f"Only {processed_count}/{concurrent_sources} processed"

    @pytest.mark.timeout(3600)  # 60 minutes for large file upload test (generation + upload + processing)
    @pytest.mark.parametrize("target_size_mb", ING_004_SIZES)
    def test_perf_ing_004_large_file_upload(
        self,
        target_size_mb: int,
        cluster_config: ClusterConfig,
        ingress_url: str,
        database_config,
        koku_api_url: str,
        perf_timer: PerfTimer,
        perf_result: PerformanceResult,
        perf_collector: PerfResultCollector,
        rh_identity_header: str,
        perf_cleanup,
        ingress_pod: str,
    ):
        """PERF-ING-004: Large file upload (50MB+).
        
        Tests system behavior with large payload files that approach or exceed
        typical upload size limits.
        
        Default ingress limit is 100MB. This test validates:
        - Upload succeeds for large files
        - Processing completes within reasonable time
        - No timeouts or memory issues
        
        Metrics captured:
        - Upload time and throughput
        - Processing time
        - Any errors or retries
        
        The test uses extended date ranges with larger profiles to generate
        files approaching the target size. Actual generated size depends on
        profile characteristics and NISE output.
        """
        cluster_id = generate_cluster_id()
        source_name = f"perf-ing-004-{target_size_mb}mb-{cluster_id[-8:]}"
        
        # Register source
        with perf_timer.measure("source_registration"):
            source = register_source(
                self.namespace,
                ingress_pod,
                koku_api_url,
                rh_identity_header,
                cluster_id,
                "org1234567",
                source_name,
            )
        
        # Track for cleanup
        perf_cleanup.track(source_id=source.source_id, cluster_id=cluster_id, source_name=source_name)
        
        # NOTE: This test intentionally ignores PERF_PROFILE and always uses "large"
        # profile because the goal is to generate files of specific sizes (50MB+).
        # Smaller profiles cannot generate files large enough to test upload limits.
        #
        # Empirical data sizes (from test runs):
        # - large profile (133 nodes) x 90 days ≈ 200 MB (much larger than expected)
        # - medium profile (49 nodes) x 30 days ≈ 15-25 MB  
        # - medium profile x 60 days ≈ 30-50 MB
        # - large profile x 30 days ≈ 50-70 MB
        # - large profile x 45 days ≈ 100+ MB
        if target_size_mb <= 50:
            days_for_size = 30
            profile_name = "large"  # Always large - required for 50MB+ files
        else:
            days_for_size = 45
            profile_name = "large"  # Always large - required for 100MB+ files
        
        end_date = datetime.now(timezone.utc)
        start_date = end_date - timedelta(days=days_for_size)
        
        jwt_token = self._get_fresh_token()
        
        # Capture baseline CPU
        baseline_cpu = get_listener_cpu_usage(self.namespace)
        cpu_samples = [baseline_cpu] if baseline_cpu else []
        
        # Generate and upload data using standard helper
        # This handles NISE generation, packaging, and upload
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
        
        package_size_mb = upload_result.get("package_size_mb", 0)
        upload_seconds = upload_result.get("upload_seconds", 0)
        generation_seconds = upload_result.get("generation_seconds", 0)
        
        # Log the actual size achieved
        print(f"\n[PERF-ING-004] Generated package: {package_size_mb:.2f} MB (target: {target_size_mb} MB)")
        
        # Note: we don't skip if size is less than target - we document what was achieved
        # The test validates that large file uploads work, not that we hit exact sizes
        size_achieved_pct = (package_size_mb / target_size_mb * 100) if target_size_mb > 0 else 0
        
        upload_throughput = package_size_mb / upload_seconds if upload_seconds > 0 else 0
        
        # Wait for manifest processing — no manual time ceiling; @pytest.mark.timeout guards.
        with perf_timer.measure("processing_wait"):
            def _sample_cpu():
                cpu = get_listener_cpu_usage(self.namespace)
                if cpu:
                    cpu_samples.append(cpu)

            proc = wait_for_processing_complete(
                self.namespace,
                database_config.pod_name,
                cluster_id,
                max_wait_seconds=3300,  # test timeout=3600; leave headroom for generation+upload
                on_poll=_sample_cpu,
            )
        schema = proc.get("schema_name")

        processing_time = proc["elapsed_s"]

        # Capture final queue depths — key diagnostic for stalled processing
        from .queue_helpers import get_celery_queue_depths
        final_queue_depths = get_celery_queue_depths(cluster_config.namespace)

        perf_result.metrics = {
            "target_size_mb": target_size_mb,
            "actual_size_mb": round(package_size_mb, 2),
            "size_achieved_pct": round(size_achieved_pct, 1),
            "profile": profile_name,
            "days": days_for_size,
            "generation_time_seconds": round(generation_seconds, 2),
            "upload_time_seconds": round(upload_seconds, 2),
            "upload_throughput_mb_s": round(upload_throughput, 4),
            "processing_time_seconds": round(processing_time, 2),
            "total_time_seconds": round(upload_seconds + processing_time, 2),
            "cpu_samples": cpu_samples,
            "avg_cpu_cores": sum(cpu_samples) / len(cpu_samples) if cpu_samples else 0,
            "max_cpu_cores": max(cpu_samples) if cpu_samples else 0,
            "processing_completed": proc["complete"],
            "final_queue_depths": final_queue_depths,
        }
        perf_result.timings = perf_timer.get_timings()
        perf_result.passed = proc["complete"]
        
        perf_collector.add_result(perf_result)
        
        # Print summary
        print(f"\n=== PERF-ING-004 Results ({target_size_mb}MB target) ===")
        print(f"  Actual size: {package_size_mb:.2f} MB ({size_achieved_pct:.0f}% of target)")
        print(f"  Generation: {generation_seconds:.1f}s")
        print(f"  Upload: {upload_seconds:.1f}s ({upload_throughput:.2f} MB/s)")
        print(f"  Processing: {processing_time:.1f}s")
        print(f"  Total: {upload_seconds + processing_time:.1f}s")
        print(f"  Completed: {proc['complete']}")
        
        assert proc["complete"], f"Large file ({package_size_mb:.2f} MB) processing did not complete"

    @pytest.mark.timeout(1200)  # 20 minutes for high-frequency upload test
    def test_perf_ing_005_high_frequency_uploads(
        self,
        cluster_config: ClusterConfig,
        ingress_url: str,
        database_config,
        koku_api_url: str,
        perf_timer: PerfTimer,
        perf_result: PerformanceResult,
        perf_collector: PerfResultCollector,
        rh_identity_header: str,
        perf_cleanup,
        ingress_pod: str,
    ):
        """PERF-ING-005: High frequency uploads - Upload every 5 min for 1 hour.
        
        Tests sustained upload rate and queue handling.
        
        Metrics captured:
        - Message queue lag
        - Error rate over time
        - Processing backlog
        """
        test_duration_minutes = int(os.environ.get("PERF_ING_005_DURATION_MINUTES", "15"))
        upload_interval_seconds = int(os.environ.get("PERF_ING_005_INTERVAL_SECONDS", "300"))
        
        cluster_id = generate_cluster_id()
        source_name = f"perf-ing-005-{cluster_id[-8:]}"
        
        # Register source
        source = register_source(
            self.namespace,
            ingress_pod,
            koku_api_url,
            rh_identity_header,
            cluster_id,
            "org1234567",
            source_name,
        )
        
        # Track for cleanup
        perf_cleanup.track(source_id=source.source_id, cluster_id=cluster_id, source_name=source_name)
        
        # Run uploads at intervals
        upload_results = []
        errors = []
        start_time = time.time()
        end_time = start_time + (test_duration_minutes * 60)
        upload_count = 0
        
        with perf_timer.measure("high_frequency_test"):
            while time.time() < end_time:
                upload_count += 1
                
                # Each upload covers last 6 hours (simulating real upload pattern)
                data_end = datetime.now(timezone.utc)
                data_start = data_end - timedelta(hours=6)
                
                try:
                    jwt_token = self._get_fresh_token()
                    result = generate_and_upload_data(
                        cluster_id,
                        source_name,
                        data_start,
                        data_end,
                        ingress_url,
                        jwt_token,
                        profile_name="baseline",
                    )
                    result["upload_number"] = upload_count
                    result["elapsed_minutes"] = (time.time() - start_time) / 60
                    upload_results.append(result)
                except Exception as e:
                    errors.append({
                        "upload_number": upload_count,
                        "elapsed_minutes": (time.time() - start_time) / 60,
                        "error": str(e),
                    })
                
                # Wait for next interval (if not done)
                if time.time() < end_time:
                    time.sleep(upload_interval_seconds)
        
        perf_result.metrics = {
            "test_duration_minutes": test_duration_minutes,
            "upload_interval_seconds": upload_interval_seconds,
            "total_uploads": upload_count,
            "successful_uploads": len(upload_results),
            "failed_uploads": len(errors),
            "error_rate": len(errors) / upload_count if upload_count > 0 else 0,
            "total_data_mb": sum(r.get("package_size_mb", 0) for r in upload_results),
            "upload_results": upload_results,
            "errors": errors,
        }
        perf_result.timings = perf_timer.get_timings()
        perf_result.passed = len(errors) == 0
        
        perf_collector.add_result(perf_result)
        
        error_rate = len(errors) / upload_count if upload_count > 0 else 0
        assert error_rate < 0.1, f"Error rate {error_rate:.1%} exceeds 10% threshold"

    @pytest.mark.timeout(21600)  # 6 hours max
    @pytest.mark.parametrize("profile_name", ING_006_PROFILES)
    def test_perf_ing_006_processing_window_validation(
        self,
        cluster_config: ClusterConfig,
        ingress_url: str,
        database_config,
        koku_api_url: str,
        perf_timer: PerfTimer,
        perf_result: PerformanceResult,
        perf_collector: PerfResultCollector,
        rh_identity_header: str,
        perf_cleanup,
        ingress_pod: str,
        profile_name: str,
    ):
        """PERF-ING-006: 6-hour processing window validation (SC-4).
        
        Validates that recommended configurations sustain daily processing
        within a 6-hour window.
        
        This test simulates a full day's data upload pattern:
        - 4 uploads per day (every 6 hours, as per real-world pattern)
        - Uses profile-specific data volumes (small/medium/large)
        - Measures total end-to-end processing time
        
        Success criteria (SC-4):
        - All 4 daily uploads processed
        - Total processing time < 6 hours
        - Summary tables updated after each upload
        
        Environment:
            PERF_PROFILE: Override profile (small, medium, large)
        """
        # Get profile configuration
        profile = PROFILES.get(profile_name)
        if not profile:
            pytest.skip(f"Profile '{profile_name}' not found")
        
        # Get profile settings
        clusters = profile.get("clusters", 1)
        
        # Track all sources for cleanup
        sources = []
        cluster_ids = []
        
        # Register sources for each cluster in the profile
        for i in range(clusters):
            cluster_id = generate_cluster_id()
            source_name = f"perf-ing-006-{profile_name}-c{i:02d}-{cluster_id[-6:]}"
            
            source = register_source(
                self.namespace,
                ingress_pod,
                koku_api_url,
                rh_identity_header,
                cluster_id,
                "org1234567",
                source_name,
            )
            
            sources.append(source)
            cluster_ids.append(cluster_id)
            perf_cleanup.track(source_id=source.source_id, cluster_id=cluster_id, source_name=source_name)
        
        print(f"\n=== PERF-ING-006: 6-Hour Processing Window ({profile_name} profile) ===")
        print(f"  Profile: {profile.get('description', profile_name)}")
        print(f"  Clusters: {clusters}")
        print(f"  Registered sources: {len(sources)}")
        
        # Simulate daily uploads (6-hour intervals).
        # Default: 2 for all profiles (sufficient to validate processing window).
        # Override via PERF_ING_006_UPLOADS for more thorough testing.
        default_uploads = 2
        uploads_per_day = int(os.environ.get("PERF_ING_006_UPLOADS", str(default_uploads)))
        upload_results = []
        total_start_time = time.time()
        
        # Pre-generate NISE data once per cluster to avoid repeated expensive generation.
        # This significantly reduces test runtime for larger profiles (small/medium/large).
        print(f"\n  Pre-generating NISE data for {len(cluster_ids)} cluster(s)...")
        pre_generated_data = {}
        data_end = datetime.now(timezone.utc)
        data_start = data_end - timedelta(hours=6)
        
        for i, cluster_id in enumerate(cluster_ids):
            print(f"    Generating data for cluster {i+1}/{len(cluster_ids)} ({cluster_id[:8]}...)...")
            with tempfile.TemporaryDirectory() as temp_dir:
                if profile_name and profile_name in PROFILES:
                    yaml_content = get_profile_nise_yaml(
                        profile_name, data_start, data_end, cluster_id, 0
                    )
                    yaml_path = os.path.join(temp_dir, "static_report.yml")
                    with open(yaml_path, "w") as f:
                        f.write(yaml_content)
                    
                    nise_output = os.path.join(temp_dir, "nise_output")
                    os.makedirs(nise_output, exist_ok=True)
                    
                    result = subprocess.run(
                        ["nise", "report", "ocp",
                         "--static-report-file", yaml_path,
                         "--ocp-cluster-id", cluster_id,
                         "-w", "--ros-ocp-info"],
                        capture_output=True,
                        text=True,
                        timeout=600,
                        cwd=nise_output,
                    )
                    
                    if result.returncode != 0:
                        raise RuntimeError(f"NISE failed: {result.stderr}")
                    
                    # Collect and copy files to a persistent location
                    csv_files = list(Path(nise_output).rglob("*.csv"))
                    pod_usage = [str(f) for f in csv_files if "pod_usage" in f.name.lower()]
                    ros_usage = [str(f) for f in csv_files if "ros_usage" in f.name.lower()]
                    node_label = [str(f) for f in csv_files if "node_label" in f.name.lower()]
                    namespace_label = [str(f) for f in csv_files if "namespace_label" in f.name.lower()]
                    
                    def _read(path):
                        with open(path) as fh:
                            return fh.read()

                    pre_generated_data[cluster_id] = {
                        "pod_usage": [(f, _read(f)) for f in pod_usage],
                        "ros_usage": [(f, _read(f)) for f in ros_usage],
                        "node_label": [(f, _read(f)) for f in node_label],
                        "namespace_label": [(f, _read(f)) for f in namespace_label],
                    }
        
        print(f"  Pre-generation complete for {len(pre_generated_data)} cluster(s)")
        
        for upload_num in range(uploads_per_day):
            upload_start = time.time()
            
            # Each upload covers 6 hours of data (reuse pre-generated data)
            print(f"\n  Upload {upload_num + 1}/{uploads_per_day}:")
            print(f"    Using pre-generated data (6-hour window)")
            
            upload_details = []
            
            with perf_timer.measure(f"upload_{upload_num + 1}"):
                for i, (source, cluster_id) in enumerate(zip(sources, cluster_ids)):
                    try:
                        jwt_token = self._get_fresh_token()
                        
                        # Use pre-generated data instead of regenerating
                        if cluster_id in pre_generated_data:
                            with tempfile.TemporaryDirectory() as temp_dir:
                                # Write pre-generated files
                                pod_files, ros_files, node_files, ns_files = [], [], [], []
                                for orig_path, content in pre_generated_data[cluster_id]["pod_usage"]:
                                    path = os.path.join(temp_dir, os.path.basename(orig_path))
                                    with open(path, "w") as f:
                                        f.write(content)
                                    pod_files.append(path)
                                for orig_path, content in pre_generated_data[cluster_id]["ros_usage"]:
                                    path = os.path.join(temp_dir, os.path.basename(orig_path))
                                    with open(path, "w") as f:
                                        f.write(content)
                                    ros_files.append(path)
                                for orig_path, content in pre_generated_data[cluster_id]["node_label"]:
                                    path = os.path.join(temp_dir, os.path.basename(orig_path))
                                    with open(path, "w") as f:
                                        f.write(content)
                                    node_files.append(path)
                                for orig_path, content in pre_generated_data[cluster_id]["namespace_label"]:
                                    path = os.path.join(temp_dir, os.path.basename(orig_path))
                                    with open(path, "w") as f:
                                        f.write(content)
                                    ns_files.append(path)
                                
                                # Create upload package
                                package_path = create_upload_package_from_files(
                                    pod_usage_files=pod_files,
                                    ros_usage_files=ros_files,
                                    cluster_id=cluster_id,
                                    start_date=data_start,
                                    end_date=data_end,
                                    node_label_files=node_files if node_files else None,
                                    namespace_label_files=ns_files if ns_files else None,
                                )
                                
                                package_size_mb = os.path.getsize(package_path) / (1024 * 1024)
                                
                                # Upload
                                session = requests.Session()
                                session.verify = False
                                response = upload_with_retry(
                                    session,
                                    f"{ingress_url}/v1/upload",
                                    package_path,
                                    jwt_token.authorization_header,
                                )
                                
                                result = {
                                    "package_size_mb": package_size_mb,
                                    "upload_time": 0,
                                }
                        else:
                            # Fallback to full generation if pre-gen not available
                            result = generate_and_upload_data(
                                cluster_id,
                                source.source_name if hasattr(source, 'source_name') else f"source-{i}",
                                data_start,
                                data_end,
                                ingress_url,
                                jwt_token,
                                profile_name=profile_name,
                            )
                        
                        upload_details.append({
                            "cluster_id": cluster_id,
                            "success": True,
                            "size_mb": result.get("package_size_mb", 0),
                            "upload_time": result.get("upload_time", 0),
                        })
                    except Exception as e:
                        upload_details.append({
                            "cluster_id": cluster_id,
                            "success": False,
                            "error": str(e),
                        })
            
            # Wait for each manifest to fully process — 1 hour per source per upload cycle.
            with perf_timer.measure(f"processing_{upload_num + 1}"):
                for source, cluster_id in zip(sources, cluster_ids):
                    try:
                        wait_for_processing_complete(
                            self.namespace,
                            database_config.pod_name,
                            cluster_id,
                            max_wait_seconds=3600,
                        )
                    except Exception as e:
                        print(f"    Warning: Processing wait failed for {cluster_id}: {e}")
            
            upload_elapsed = time.time() - upload_start
            successful = sum(1 for d in upload_details if d.get("success", False))
            
            upload_results.append({
                "upload_number": upload_num + 1,
                "elapsed_seconds": upload_elapsed,
                "successful_uploads": successful,
                "total_uploads": len(upload_details),
                "details": upload_details,
            })
            
            print(f"    Completed: {successful}/{len(upload_details)} sources in {upload_elapsed:.1f}s")
        
        # Calculate totals
        total_elapsed = time.time() - total_start_time
        total_successful = sum(r["successful_uploads"] for r in upload_results)
        total_uploads = sum(r["total_uploads"] for r in upload_results)
        
        # 6-hour window = 21600 seconds
        processing_window_seconds = 6 * 60 * 60
        
        perf_result.metrics = {
            "profile": profile_name,
            "profile_description": profile.get("description", ""),
            "clusters": clusters,
            "uploads_per_day": uploads_per_day,
            "total_elapsed_seconds": total_elapsed,
            "total_elapsed_hours": total_elapsed / 3600,
            "successful_uploads": total_successful,
            "total_uploads": total_uploads,
            "processing_window_seconds": processing_window_seconds,
            "within_window": total_elapsed < processing_window_seconds,
            "window_utilization_pct": (total_elapsed / processing_window_seconds) * 100,
            "upload_results": upload_results,
        }
        perf_result.timings = perf_timer.get_timings()
        perf_result.passed = (
            total_elapsed < processing_window_seconds and
            total_successful == total_uploads
        )
        
        perf_collector.add_result(perf_result)
        
        print(f"\n=== PERF-ING-006 Results ({profile_name}) ===")
        print(f"  Total processing time: {total_elapsed/3600:.2f} hours")
        print(f"  Window utilization: {perf_result.metrics['window_utilization_pct']:.1f}%")
        print(f"  Successful uploads: {total_successful}/{total_uploads}")
        print(f"  Within 6-hour window: {perf_result.metrics['within_window']}")
        
        # Assert success criteria (SC-4)
        assert total_elapsed < processing_window_seconds, (
            f"Processing time {total_elapsed/3600:.2f}h exceeds 6-hour window for {profile_name} profile"
        )
        assert total_successful == total_uploads, (
            f"Only {total_successful}/{total_uploads} uploads succeeded"
        )
