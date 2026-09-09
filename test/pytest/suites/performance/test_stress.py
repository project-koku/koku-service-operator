"""
Stress & Spike/Backlog Recovery Tests (COST-7627 + COST-7600).

Ramp concurrent source count at medium profile until a component fails,
then verify the system recovers gracefully after the load stops.

Test IDs:
- PERF-STR-001: Ramp-to-failure (single test, internal loop over source counts)
- PERF-STR-002: Backlog recovery (sustained overload then drain)

Usage:
    # Both tests (recommended)
    ./scripts/deploy-test-cost-onprem.sh --perf-only --perf-profile medium --perf-suite stress

    # Ramp only (long-running, heavyweight)
    ./scripts/deploy-test-cost-onprem.sh --perf-only --perf-profile medium --perf-suite stress_ramp

    # Recovery only (shorter, uses STR-001 results or defaults)
    ./scripts/deploy-test-cost-onprem.sh --perf-only --perf-profile medium --perf-suite stress_recovery

    # Direct pytest
    PERF_PROFILE=medium pytest -m "performance and stress" tests/suites/performance/
    PERF_PROFILE=medium pytest -m "performance and stress_ramp" tests/suites/performance/
    PERF_PROFILE=medium pytest -m "performance and stress_recovery" tests/suites/performance/

Environment Variables:
    STRESS_RAMP_STEPS: Comma-separated source counts (default: 5,10,20,30,50,75,100)
    STRESS_STEP_TIMEOUT_BASE: Base timeout per step in seconds (default: 120)
    STRESS_STEP_TIMEOUT_PER_SOURCE: Additional seconds per source (default: 60)
    STRESS_MAX_STEP_TIME: Absolute max seconds before declaring step failed (default: 1800)
    STRESS_RECOVERY_SOURCE_COUNT: Sources for recovery test; 0 = auto from STR-001 (default: 0)
    STRESS_RECOVERY_DURATION_S: How long to sustain overload in STR-002 (default: 300)
"""

import os
import time
from concurrent.futures import ThreadPoolExecutor, as_completed
from dataclasses import dataclass, field
from datetime import datetime, timedelta, timezone
from typing import Any, Dict, List, Optional, Tuple

import pytest

from conftest import ClusterConfig, DatabaseConfig, JWTToken, obtain_jwt_token
from e2e_helpers import (
    cleanup_database_records,
    ensure_nise_available,
    generate_cluster_id,
    register_source,
    wait_for_processing_complete,
)
from utils import run_oc_command

from .data_classes import PerformanceResult
from .helpers import (
    APIProbeThread,
    PerfResultCollector,
    PerfTimer,
    create_authenticated_session,
    generate_and_upload_data,
    get_oomkill_events,
    get_pod_restart_counts,
)
from .k8s_helpers import capture_pg_stats, diff_pg_stats
from .profiles import ACTIVE_PROFILE as _ACTIVE_PROFILE
from .queue_helpers import get_celery_queue_depths, wait_for_queue_drain
from .tracker import PerfCleanupTracker


# =============================================================================
# Configuration
# =============================================================================

RAMP_STEPS = [
    int(x) for x in os.environ.get("STRESS_RAMP_STEPS", "5,10,20,30,50,75,100").split(",")
]

STEP_TIMEOUT_BASE = int(os.environ.get("STRESS_STEP_TIMEOUT_BASE", "120"))
STEP_TIMEOUT_PER_SOURCE = int(os.environ.get("STRESS_STEP_TIMEOUT_PER_SOURCE", "60"))
MAX_STEP_TIME = int(os.environ.get("STRESS_MAX_STEP_TIME", "1800"))

RECOVERY_SOURCE_COUNT = int(os.environ.get("STRESS_RECOVERY_SOURCE_COUNT", "0"))
RECOVERY_DURATION_S = int(os.environ.get("STRESS_RECOVERY_DURATION_S", "300"))

COMPONENT_LABELS = [
    "app.kubernetes.io/component=listener",
    "app.kubernetes.io/component=cost-worker",
    "app.kubernetes.io/component=cost-processor",
    "app.kubernetes.io/component=database",
    "app.kubernetes.io/component=ros-processor",
    "app.kubernetes.io/component=ros-optimization",
    "app.kubernetes.io/component=cache",
    "app.kubernetes.io/component=ingress",
]

# Module-level state shared between STR-001 and STR-002.
# Relies on pytest running methods in definition order within a class
# (str_001 before str_002). STR-002 has a fallback chain if this is empty.
_ramp_result: Dict[str, Any] = {}


# =============================================================================
# Helpers
# =============================================================================

@dataclass
class StepResult:
    """Metrics captured during a single ramp step."""
    source_count: int = 0
    upload_successes: int = 0
    upload_errors: int = 0
    processed_count: int = 0
    step_time_s: float = 0
    upload_time_s: float = 0
    processing_time_s: float = 0
    queue_drain_s: float = 0
    oomkill_events: List[Dict[str, str]] = field(default_factory=list)
    restart_delta: Dict[str, int] = field(default_factory=dict)
    total_new_restarts: int = 0
    pg_stats: Dict[str, Any] = field(default_factory=dict)
    queue_depths_peak: Dict[str, int] = field(default_factory=dict)
    stop_reason: Optional[str] = None


def _collect_restart_baseline(namespace: str) -> Dict[str, int]:
    """Snapshot restart counts across all components."""
    baseline: Dict[str, int] = {}
    for label in COMPONENT_LABELS:
        baseline.update(get_pod_restart_counts(namespace, label))
    return baseline


def _compute_restart_delta(
    before: Dict[str, int], after: Dict[str, int]
) -> Tuple[Dict[str, int], int]:
    """Compute per-pod restart deltas and total new restarts."""
    delta = {}
    total = 0
    for pod, count in after.items():
        prev = before.get(pod, 0)
        if count > prev:
            delta[pod] = count - prev
            total += count - prev
    return delta, total


def _collect_oomkill_events(namespace: str) -> List[Dict[str, str]]:
    """Check for OOMKills across all monitored components."""
    events = []
    for label in COMPONENT_LABELS:
        events.extend(get_oomkill_events(namespace, label))
    return events


def _check_stop_conditions(
    step: StepResult,
    queue_stall_detected: bool,
) -> Optional[str]:
    """Evaluate stop conditions. Returns a reason string or None."""
    if step.oomkill_events:
        pods = ", ".join(e["pod"] for e in step.oomkill_events)
        return f"OOMKill detected: {pods}"

    if step.total_new_restarts > 3:
        return f"Excessive restarts: {step.total_new_restarts} new restarts ({step.restart_delta})"

    if step.step_time_s > MAX_STEP_TIME:
        return f"Step took {step.step_time_s:.0f}s (max {MAX_STEP_TIME}s)"

    if queue_stall_detected:
        return "Queue stall: depth strictly increasing over last 5 samples"

    error_rate = step.upload_errors / max(step.source_count, 1)
    if error_rate > 0.05:
        return f"Upload error rate {error_rate:.0%} exceeds 5% threshold"

    return None


# =============================================================================
# Test Class
# =============================================================================

@pytest.mark.performance
@pytest.mark.stress
@pytest.mark.slow
class TestStress:
    """Stress and spike/backlog recovery tests (COST-7627 + COST-7600)."""

    @pytest.fixture(autouse=True)
    def setup(self, cluster_config: ClusterConfig, keycloak_config):
        self.namespace = cluster_config.namespace
        self.helm_release = cluster_config.helm_release_name

        if not ensure_nise_available():
            pytest.skip("NISE (koku-nise) not available")

        self._keycloak_config = keycloak_config

    def _get_fresh_token(self) -> JWTToken:
        return obtain_jwt_token(self._keycloak_config)

    # -----------------------------------------------------------------
    # STR-001: Ramp-to-failure
    # -----------------------------------------------------------------

    @pytest.mark.stress_ramp
    @pytest.mark.timeout(14400)
    def test_perf_str_001_ramp_to_failure(
        self,
        cluster_config: ClusterConfig,
        ingress_url: str,
        database_config: DatabaseConfig,
        koku_api_url: str,
        perf_timer: PerfTimer,
        perf_result: PerformanceResult,
        perf_collector: PerfResultCollector,
        rh_identity_header: str,
        perf_cleanup: PerfCleanupTracker,
        ingress_pod: str,
        keycloak_config,
        gateway_url: str,
    ):
        """PERF-STR-001: Ramp concurrent sources until the system breaks.

        Iterates through increasing source counts, uploading concurrently at
        each step. Captures comprehensive metrics and stops when a failure
        condition is detected.
        """
        global _ramp_result

        print(f"\n{'='*72}")
        print(f"PERF-STR-001: Ramp-to-failure (steps: {RAMP_STEPS})")
        print(f"Profile: {_ACTIVE_PROFILE}")
        print(f"{'='*72}\n")

        # Start background API probe for the entire ramp
        session = create_authenticated_session(keycloak_config)
        api_probe = APIProbeThread(session, gateway_url, self.namespace,
                                   poll_interval=5.0, keycloak_config=keycloak_config)
        api_probe.start()

        step_results: List[StepResult] = []
        breaking_point: Optional[int] = None
        last_good_count = 0

        try:
            for step_idx, source_count in enumerate(RAMP_STEPS):
                print(f"\n{'─'*60}")
                print(f"Step {step_idx + 1}/{len(RAMP_STEPS)}: {source_count} concurrent sources")
                print(f"{'─'*60}")

                step = StepResult(source_count=source_count)
                step_start = time.time()

                # Snapshot state before step
                restart_baseline = _collect_restart_baseline(self.namespace)
                pg_before = capture_pg_stats(
                    self.namespace,
                    database_config.pod_name,
                    database_config.database,
                    database_config.user,
                )

                # Register sources
                sources = []
                for i in range(source_count):
                    cluster_id = generate_cluster_id()
                    source_name = f"perf-str-001-s{step_idx}-{i:03d}-{cluster_id[-6:]}"

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
                    sources.append({
                        "cluster_id": cluster_id,
                        "source_name": source_name,
                        "source": source,
                    })

                # Upload concurrently
                end_date = datetime.now(timezone.utc)
                start_date = end_date - timedelta(days=7)
                upload_errors = []
                upload_successes = []

                def _upload_one(source_info, s_date, e_date):
                    try:
                        jwt_token = self._get_fresh_token()
                        return generate_and_upload_data(
                            source_info["cluster_id"],
                            source_info["source_name"],
                            s_date, e_date,
                            ingress_url, jwt_token,
                            profile_name="baseline",
                        )
                    except Exception as e:
                        return {"error": str(e), "cluster_id": source_info["cluster_id"]}

                upload_start = time.time()
                with ThreadPoolExecutor(max_workers=min(source_count, 20)) as executor:
                    futures = {
                        executor.submit(_upload_one, s, start_date, end_date): s
                        for s in sources
                    }
                    for future in as_completed(futures):
                        result = future.result()
                        if "error" in result:
                            upload_errors.append(result)
                        else:
                            upload_successes.append(result)

                step.upload_time_s = time.time() - upload_start
                step.upload_successes = len(upload_successes)
                step.upload_errors = len(upload_errors)

                print(f"  Uploads: {step.upload_successes} OK, {step.upload_errors} failed "
                      f"({step.upload_time_s:.0f}s)")

                # Wait for processing with scaled timeout
                timeout = min(
                    STEP_TIMEOUT_BASE + source_count * STEP_TIMEOUT_PER_SOURCE,
                    MAX_STEP_TIME,
                )
                deadline = time.time() + timeout
                processed = 0

                # Monitor queue depths during processing for stall detection
                queue_samples: List[int] = []
                processing_start = time.time()

                for source_info in sources:
                    remaining = max(15, int(deadline - time.time()))
                    proc = wait_for_processing_complete(
                        self.namespace,
                        database_config.pod_name,
                        source_info["cluster_id"],
                        max_wait_seconds=remaining,
                    )
                    if proc["complete"]:
                        processed += 1

                    depths = get_celery_queue_depths(self.namespace)
                    queue_samples.append(sum(depths.values()))

                step.processing_time_s = time.time() - processing_start
                step.processed_count = processed

                # Drain queues
                drain = wait_for_queue_drain(
                    self.namespace,
                    max_wait_seconds=600,
                    label=f"STR-001[{source_count}]",
                )
                step.queue_drain_s = drain.get("elapsed_s", 0)

                # Collect post-step metrics
                pg_after = capture_pg_stats(
                    self.namespace,
                    database_config.pod_name,
                    database_config.database,
                    database_config.user,
                )
                step.pg_stats = diff_pg_stats(pg_before, pg_after)

                restarts_after = _collect_restart_baseline(self.namespace)
                step.restart_delta, step.total_new_restarts = _compute_restart_delta(
                    restart_baseline, restarts_after,
                )

                step.oomkill_events = _collect_oomkill_events(self.namespace)
                step.step_time_s = time.time() - step_start

                # Detect queue stall: depth strictly increasing over 5+ samples
                queue_stall = False
                if len(queue_samples) >= 5:
                    tail = queue_samples[-5:]
                    if all(tail[i] < tail[i + 1] for i in range(len(tail) - 1)):
                        queue_stall = True

                # Check stop conditions
                step.stop_reason = _check_stop_conditions(step, queue_stall)

                print(f"  Processed: {processed}/{source_count} "
                      f"({step.processing_time_s:.0f}s processing, "
                      f"{step.queue_drain_s:.0f}s drain)")
                print(f"  PG stats: {step.pg_stats.get('xact_commit_delta', '?')} commits, "
                      f"cache hit {step.pg_stats.get('cache_hit_ratio', '?')}")
                if step.total_new_restarts > 0:
                    print(f"  Restarts: {step.restart_delta}")
                if step.oomkill_events:
                    print(f"  OOMKills: {step.oomkill_events}")

                step_results.append(step)

                if step.stop_reason:
                    print(f"\n  *** STOP: {step.stop_reason} ***")
                    breaking_point = source_count
                    break
                else:
                    last_good_count = source_count
                    print(f"  ✓ Step passed")

        finally:
            api_summary = api_probe.stop()

        # Store results for STR-002
        _ramp_result = {
            "breaking_point": breaking_point,
            "last_good_count": last_good_count,
            "steps": [
                {
                    "source_count": s.source_count,
                    "upload_successes": s.upload_successes,
                    "upload_errors": s.upload_errors,
                    "processed_count": s.processed_count,
                    "step_time_s": round(s.step_time_s, 1),
                    "upload_time_s": round(s.upload_time_s, 1),
                    "processing_time_s": round(s.processing_time_s, 1),
                    "queue_drain_s": round(s.queue_drain_s, 1),
                    "total_new_restarts": s.total_new_restarts,
                    "oomkill_count": len(s.oomkill_events),
                    "pg_commits": s.pg_stats.get("xact_commit_delta", 0),
                    "pg_cache_hit": s.pg_stats.get("cache_hit_ratio", 0),
                    "stop_reason": s.stop_reason,
                }
                for s in step_results
            ],
            "api_probe": api_summary,
        }

        # Summary
        print(f"\n{'='*72}")
        print("STR-001 SUMMARY")
        print(f"{'='*72}")
        print(f"Steps completed: {len(step_results)}/{len(RAMP_STEPS)}")
        print(f"Last good count: {last_good_count}")
        print(f"Breaking point:  {breaking_point or 'none (system survived all steps)'}")
        if breaking_point:
            final = step_results[-1]
            print(f"Failure reason:  {final.stop_reason}")
        print(f"\nAPI probe: {api_summary.get('probe_count', 0)} samples, "
              f"peak queue {api_summary.get('peak_queue_depth', '?')}, "
              f"errors {api_summary.get('total_errors', '?')}")
        for step in step_results:
            status = "FAIL" if step.stop_reason else "PASS"
            print(f"  [{status}] {step.source_count:3d} sources: "
                  f"{step.processed_count}/{step.source_count} processed, "
                  f"{step.step_time_s:.0f}s total, "
                  f"{step.total_new_restarts} restarts")

        # Persist results
        perf_result.test_id = "PERF-STR-001"
        perf_result.metrics = _ramp_result
        perf_result.passed = breaking_point is None or last_good_count > 0
        perf_collector.add_result(perf_result)

        # The test "passes" as long as we collected data. The breaking point
        # is a finding, not a failure — the whole point is to find it.
        assert last_good_count > 0 or breaking_point is None, (
            f"System failed at the first step ({RAMP_STEPS[0]} sources)"
        )

    # -----------------------------------------------------------------
    # STR-002: Backlog recovery
    # -----------------------------------------------------------------

    @pytest.mark.stress_recovery
    @pytest.mark.timeout(3600)
    def test_perf_str_002_backlog_recovery(
        self,
        cluster_config: ClusterConfig,
        ingress_url: str,
        database_config: DatabaseConfig,
        koku_api_url: str,
        perf_timer: PerfTimer,
        perf_result: PerformanceResult,
        perf_collector: PerfResultCollector,
        rh_identity_header: str,
        perf_cleanup: PerfCleanupTracker,
        ingress_pod: str,
        keycloak_config,
        gateway_url: str,
    ):
        """PERF-STR-002: Verify system recovers after sustained overload.

        Uploads at a rate near the breaking point for RECOVERY_DURATION_S
        seconds, then stops and monitors the system until all queues drain
        and API latencies return to baseline.
        """
        global _ramp_result

        # Determine load level
        if RECOVERY_SOURCE_COUNT > 0:
            load_count = RECOVERY_SOURCE_COUNT
            source_label = f"configured ({RECOVERY_SOURCE_COUNT})"
        elif _ramp_result.get("breaking_point"):
            load_count = _ramp_result["breaking_point"]
            source_label = f"breaking point from STR-001 ({load_count})"
        elif _ramp_result.get("last_good_count"):
            load_count = _ramp_result["last_good_count"]
            source_label = f"last good from STR-001 ({load_count})"
        else:
            load_count = 20
            source_label = "default (STR-001 not run or no data)"

        print(f"\n{'='*72}")
        print(f"PERF-STR-002: Backlog recovery")
        print(f"Load level: {load_count} sources ({source_label})")
        print(f"Overload duration: {RECOVERY_DURATION_S}s")
        print(f"{'='*72}\n")

        # Capture quiescent API baseline
        session = create_authenticated_session(keycloak_config)
        api_probe_baseline = APIProbeThread(session, gateway_url, self.namespace,
                                            poll_interval=2.0, keycloak_config=keycloak_config)
        api_probe_baseline.start()
        time.sleep(30)
        baseline_summary = api_probe_baseline.stop()

        baseline_p95 = baseline_summary.get("report_baseline", {}).get("p95", 999)
        print(f"Quiescent API baseline p95: {baseline_p95:.3f}s")

        # Sustained overload: keep uploading for RECOVERY_DURATION_S
        restart_baseline = _collect_restart_baseline(self.namespace)
        overload_start = time.time()
        batch_num = 0
        all_sources: List[Dict[str, Any]] = []

        # Start API probe during overload phase (informational only)
        session2 = create_authenticated_session(keycloak_config)
        api_probe_overload = APIProbeThread(session2, gateway_url, self.namespace,
                                            poll_interval=5.0, keycloak_config=keycloak_config)
        api_probe_overload.start()

        def _upload_batch(sources_batch, batch_start, batch_end):
            """Upload a batch of sources concurrently."""
            def _upload_one(source_info):
                try:
                    jwt_token = self._get_fresh_token()
                    return generate_and_upload_data(
                        source_info["cluster_id"],
                        source_info["source_name"],
                        batch_start, batch_end,
                        ingress_url, jwt_token,
                        profile_name="baseline",
                    )
                except Exception as e:
                    return {"error": str(e), "cluster_id": source_info["cluster_id"]}

            if not sources_batch:
                return
            with ThreadPoolExecutor(max_workers=min(len(sources_batch), 20)) as executor:
                futures = {executor.submit(_upload_one, s): s for s in sources_batch}
                for future in as_completed(futures):
                    future.result()

        try:
            while time.time() - overload_start < RECOVERY_DURATION_S:
                batch_num += 1
                print(f"\n  Overload batch {batch_num}: uploading {load_count} sources...")

                reg_start = time.time()
                sources = []
                for i in range(load_count):
                    cluster_id = generate_cluster_id()
                    source_name = f"perf-str-002-b{batch_num}-{i:03d}-{cluster_id[-6:]}"

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
                    sources.append({
                        "cluster_id": cluster_id,
                        "source_name": source_name,
                        "source": source,
                    })
                reg_time = time.time() - reg_start

                end_date = datetime.now(timezone.utc)
                start_date = end_date - timedelta(days=7)
                upload_start = time.time()
                _upload_batch(sources, start_date, end_date)
                upload_time = time.time() - upload_start

                all_sources.extend(sources)

                depths = get_celery_queue_depths(self.namespace)
                total_queued = sum(depths.values())
                elapsed = time.time() - overload_start
                print(f"  Batch {batch_num}: register {reg_time:.0f}s, upload {upload_time:.0f}s. "
                      f"Queue depth: {total_queued}. "
                      f"Elapsed: {elapsed:.0f}/{RECOVERY_DURATION_S}s")

            overload_api = api_probe_overload.stop()
            print(f"\n  Overload phase complete ({batch_num} batches, "
                  f"{len(all_sources)} total sources)")
            print(f"  Overload API p95: {overload_api.get('report_baseline', {}).get('p95', '?')}s")

            # Recovery phase: stop uploading, monitor until healthy
            print(f"\n{'─'*60}")
            print("Recovery phase: waiting for queue drain + API stabilization")
            print(f"{'─'*60}")

            recovery_start = time.time()

            # Wait for all queues to drain (generous timeout)
            drain_result = wait_for_queue_drain(
                self.namespace,
                max_wait_seconds=1800,
                poll_interval=15,
                label="STR-002-recovery",
            )

            recovery_time = time.time() - recovery_start

            # Check pod health
            restarts_after = _collect_restart_baseline(self.namespace)
            restart_delta, total_new = _compute_restart_delta(restart_baseline, restarts_after)
            oomkills = _collect_oomkill_events(self.namespace)

            # Check for CrashLoopBackOff
            clb_result = run_oc_command(
                ["get", "pods", "-n", self.namespace,
                 "-l", f"app.kubernetes.io/instance={self.helm_release}",
                 "--field-selector=status.phase!=Running,status.phase!=Succeeded",
                 "--no-headers"],
                check=False,
            )
            unhealthy_pods = [
                line.split()[0]
                for line in (clb_result.stdout or "").strip().splitlines()
                if line.strip()
            ]

            # Post-recovery API probe: measures latency *after* queues
            # have drained, so the comparison against the quiescent baseline
            # actually proves the API returned to normal.
            session3 = create_authenticated_session(keycloak_config)
            api_probe_post = APIProbeThread(session3, gateway_url, self.namespace,
                                            poll_interval=2.0, keycloak_config=keycloak_config)
            api_probe_post.start()
            time.sleep(30)
            post_recovery_api = api_probe_post.stop()

        except Exception:
            api_probe_overload.stop()
            if "api_probe_post" in locals():
                api_probe_post.stop()
            raise

        # Evaluate recovery
        recovered = drain_result.get("drained", False)
        recovery_p95 = post_recovery_api.get("report_baseline", {}).get("p95", 999)
        latency_ratio = recovery_p95 / baseline_p95 if baseline_p95 > 0 else 999

        print(f"\n{'='*72}")
        print("STR-002 SUMMARY")
        print(f"{'='*72}")
        print(f"Overload: {batch_num} batches, {len(all_sources)} sources")
        print(f"Queue drained: {recovered} ({drain_result.get('elapsed_s', '?')}s)")
        print(f"Recovery time: {recovery_time:.0f}s")
        print(f"API p95 — baseline: {baseline_p95:.3f}s, post-recovery: {recovery_p95:.3f}s "
              f"(ratio: {latency_ratio:.1f}x)")
        print(f"New restarts: {total_new} {restart_delta if restart_delta else ''}")
        print(f"OOMKills: {len(oomkills)}")
        print(f"Unhealthy pods: {unhealthy_pods or 'none'}")

        healthy = (
            recovered
            and not unhealthy_pods
            and not oomkills
            and latency_ratio < 3.0
        )
        print(f"\nVerdict: {'RECOVERED' if healthy else 'NOT RECOVERED'}")

        perf_result.test_id = "PERF-STR-002"
        perf_result.metrics = {
            "load_count": load_count,
            "batches": batch_num,
            "total_sources": len(all_sources),
            "recovery_time_s": round(recovery_time, 1),
            "queue_drained": recovered,
            "queue_drain_s": drain_result.get("elapsed_s", 0),
            "baseline_p95": round(baseline_p95, 4),
            "recovery_p95": round(recovery_p95, 4),
            "overload_p95": round(overload_api.get("report_baseline", {}).get("p95", 0), 4),
            "latency_ratio": round(latency_ratio, 2),
            "new_restarts": total_new,
            "oomkill_count": len(oomkills),
            "unhealthy_pods": unhealthy_pods,
            "api_probe_overload": overload_api,
            "api_probe_post_recovery": post_recovery_api,
        }
        perf_result.passed = healthy
        perf_collector.add_result(perf_result)

        assert recovered, (
            f"Queues did not drain within 30 minutes. "
            f"Final depths: {drain_result.get('final_depths', {})}"
        )
        assert not unhealthy_pods, (
            f"Pods not recovered: {unhealthy_pods}"
        )
        assert latency_ratio < 3.0, (
            f"API latency did not return to near-baseline: "
            f"{recovery_p95:.3f}s vs {baseline_p95:.3f}s ({latency_ratio:.1f}x)"
        )
