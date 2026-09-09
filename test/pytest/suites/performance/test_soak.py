"""
Soak/Stability Performance Tests (PERF-SOAK-*).

Long-running tests to validate system stability under sustained load per FLPATH-4036.

These tests are designed to run over extended periods (hours to days) and detect:
- Memory leaks
- Resource exhaustion
- Queue starvation
- Performance degradation over time

Test IDs:
- PERF-SOAK-001: Continuous operation stability
- PERF-SOAK-002: Memory leak detection
- PERF-SOAK-003: Disk usage monitoring
- PERF-SOAK-004: Queue health monitoring

Usage:
    # Run 1-hour soak test (default)
    SOAK_TESTS=true pytest -m "performance and soak"

    # Condensed mode (~15 min) — same code paths, compressed intervals
    SOAK_TESTS=true SOAK_CONDENSED=true pytest -m "performance and soak"

    # Run 7-day soak test
    SOAK_TESTS=true SOAK_DURATION_HOURS=168 pytest -m "performance and soak"

Environment Variables:
    SOAK_TESTS: Set to "true" to enable soak tests (opt-in)
    SOAK_CONDENSED: Set to "true" for compressed intervals (~15 min cycle)
    SOAK_DURATION_HOURS: Test duration in hours (default: 1, or 0.25 if condensed)
    SOAK_UPLOAD_INTERVAL_MINUTES: Interval between uploads (default: 15, or 1 if condensed)
    SOAK_QUERY_INTERVAL_MINUTES: Interval between API queries (default: 5, or 1 if condensed)
    SOAK_METRICS_INTERVAL_SECONDS: Metrics collection interval (default: 60, or 15 if condensed)
"""

import json
import os
import tempfile
import threading
import time
import uuid
from collections import deque
from dataclasses import dataclass, field
from datetime import datetime, timedelta, timezone
from pathlib import Path
from typing import Any, Callable, Dict, List, Optional, Tuple

import pytest
import requests

from conftest import ClusterConfig, JWTToken
from e2e_helpers import (
    cleanup_database_records,
    delete_source,
    ensure_nise_available,
    generate_cluster_id,
    generate_nise_data,
    register_source,
    upload_with_retry,
    wait_for_provider,
    wait_for_summary_tables,
)
from utils import (
    create_upload_package_from_files,
    exec_in_pod,
    execute_db_query,
    get_pod_by_label,
    get_secret_value,
    run_oc_command,
)

from .data_classes import PerformanceResult
from .helpers import PerfResultCollector, PerfTestConfig, save_perf_result
from .profiles import ACTIVE_PROFILE as _ACTIVE_PROFILE
from .tracker import PerfCleanupTracker

_SOAK_ENABLED = os.environ.get("SOAK_TESTS", "").lower() in ("true", "1", "yes")
_SOAK_CONDENSED = os.environ.get("SOAK_CONDENSED", "").lower() in ("true", "1")
_SOAK_DURATION_S = int(float(os.environ.get(
    "SOAK_DURATION_HOURS", "0.25" if _SOAK_CONDENSED else "1",
)) * 3600)

# Module-level state shared between SOAK-001 and SOAK-002/003/004.
# SOAK-001 populates this after its run; subsequent tests analyze the
# same collected samples rather than re-collecting independently.
_soak_shared: Dict[str, Any] = {}

# Number of consecutive iterations a pod must exceed the leak threshold
# before SOAK-002 actually fails. soak-loop.sh runs each iteration as an
# independent process, so a single hour's snapshot can't tell a one-time
# step-change (e.g. JVM heap growth during warm-up, which plateaus) apart
# from a genuine sustained leak. Requiring persistence across iterations
# filters out one-off blips while still catching real leaks (just one
# iteration later). See docs/performance/performance-testing-plan.md.
_SOAK_LEAK_CONSECUTIVE_ITERATIONS = int(
    os.environ.get("SOAK_LEAK_CONSECUTIVE_ITERATIONS", "2")
)


def _soak_leak_state_path() -> Optional[Path]:
    """Stable file location shared across soak-loop.sh iterations.

    PERF_OUTPUT_DIR is set per-iteration to ``<run_dir>/iteration-N``; its
    parent is the stable run directory shared by every iteration of a
    soak-loop.sh run. Returns None when not running under the loop (e.g. a
    single long continuous soak run), where cross-iteration persistence
    doesn't apply and detection falls back to immediate (single-window)
    evaluation.
    """
    perf_output_dir = os.environ.get("PERF_OUTPUT_DIR")
    if not perf_output_dir:
        return None
    return Path(perf_output_dir).parent / ".soak_leak_state.json"


def _load_soak_leak_state(path: Optional[Path]) -> Dict[str, int]:
    if not path or not path.exists():
        return {}
    try:
        return json.loads(path.read_text())
    except (json.JSONDecodeError, OSError):
        return {}


def _save_soak_leak_state(path: Optional[Path], state: Dict[str, int]) -> None:
    if not path:
        return
    try:
        path.write_text(json.dumps(state))
    except OSError:
        pass


# =============================================================================
# Configuration
# =============================================================================

# Soak configuration is provided by PerfTestConfig via the perf_config fixture.
# Helper properties for convenience:
def _soak_duration_seconds(perf_config) -> float:
    return perf_config.soak_duration_hours * 3600

def _soak_upload_interval_seconds(perf_config) -> float:
    return perf_config.soak_upload_interval_minutes * 60

def _soak_query_interval_seconds(perf_config) -> float:
    return perf_config.soak_query_interval_minutes * 60


@dataclass
class MetricSample:
    """A single metrics sample."""
    timestamp: str
    elapsed_seconds: float
    memory_mb: Dict[str, float]  # pod_name -> memory
    cpu_cores: Dict[str, float]  # pod_name -> cpu
    disk_usage_gb: Dict[str, float]  # component -> disk
    queue_depths: Dict[str, int]  # queue_name -> depth
    error_count: int = 0
    

@dataclass
class SoakTestState:
    """Tracks state during a soak test run.
    
    Thread-safe for concurrent access from background workers.
    """
    
    start_time: float = field(default_factory=time.time)
    # Use deque for thread-safe append operations
    _samples: deque = field(default_factory=lambda: deque(maxlen=10000))
    _errors: deque = field(default_factory=lambda: deque(maxlen=1000))
    _lock: threading.Lock = field(default_factory=threading.Lock)
    uploads_completed: int = 0
    uploads_failed: int = 0
    queries_completed: int = 0
    queries_failed: int = 0
    stop_event: threading.Event = field(default_factory=threading.Event)
    
    @property
    def elapsed_seconds(self) -> float:
        return time.time() - self.start_time
    
    @property
    def samples(self) -> List[MetricSample]:
        """Return samples as list (thread-safe copy)."""
        return list(self._samples)
    
    @property
    def errors(self) -> List[str]:
        """Return errors as list (thread-safe copy)."""
        return list(self._errors)
    
    def add_sample(self, sample: MetricSample):
        """Thread-safe sample addition."""
        self._samples.append(sample)
    
    def add_error(self, error: str):
        """Thread-safe error addition."""
        self._errors.append(f"[{datetime.now(timezone.utc).isoformat()}] {error}")
    
    def increment_uploads(self, success: bool = True):
        """Thread-safe upload counter increment."""
        with self._lock:
            if success:
                self.uploads_completed += 1
            else:
                self.uploads_failed += 1
    
    def increment_queries(self, success: bool = True):
        """Thread-safe query counter increment."""
        with self._lock:
            if success:
                self.queries_completed += 1
            else:
                self.queries_failed += 1


# =============================================================================
# Metrics Collection
# =============================================================================

def collect_pod_resources(namespace: str) -> Tuple[Dict[str, float], Dict[str, float]]:
    """Collect CPU and memory usage for all Cost On-Prem pods.
    
    Returns:
        Tuple of (memory_dict, cpu_dict) with pod_name -> value mappings
    """
    from .helpers import parse_cpu_millicores, parse_memory_mib

    memory = {}
    cpu = {}
    
    result = run_oc_command([
        "adm", "top", "pod", "-n", namespace, "--no-headers"
    ], check=False)
    
    if result.returncode != 0:
        return memory, cpu
    
    for line in result.stdout.strip().split("\n"):
        if not line.strip():
            continue
        
        parts = line.split()
        if len(parts) >= 3:
            pod_name = parts[0]
            try:
                cpu[pod_name] = parse_cpu_millicores(parts[1])
            except (ValueError, IndexError):
                pass
            try:
                memory[pod_name] = parse_memory_mib(parts[2])
            except (ValueError, IndexError):
                pass
    
    return memory, cpu


def collect_disk_usage(namespace: str) -> Dict[str, float]:
    """Collect disk usage for persistent volumes.
    
    Returns:
        Dict with component -> disk_usage_gb mappings
    """
    disk = {}
    
    # PostgreSQL disk usage
    db_pod = get_pod_by_label(namespace, "app.kubernetes.io/component=database")
    if db_pod:
        result = run_oc_command([
            "exec", "-n", namespace, db_pod, "--",
            "df", "-BG", "/var/lib/pgsql/data"
        ], check=False)
        
        if result.returncode == 0:
            lines = result.stdout.strip().split("\n")
            if len(lines) > 1:
                parts = lines[1].split()
                if len(parts) >= 3:
                    used = parts[2].replace("G", "")
                    if used.isdigit():
                        disk["postgresql"] = float(used)
    
    # Kafka disk usage — use the same broker lookup as collect_queue_depths
    from .kafka_helpers import get_kafka_broker_pod, get_kafka_namespace

    kafka_namespace = get_kafka_namespace()
    kafka_pod = get_kafka_broker_pod(kafka_namespace)
    if kafka_pod:
        result = run_oc_command([
            "exec", "-n", kafka_namespace, kafka_pod, "--",
            "df", "-BG", "/var/lib/kafka/data-0"
        ], check=False)

        if result.returncode == 0:
            lines = result.stdout.strip().split("\n")
            if len(lines) > 1:
                parts = lines[1].split()
                if len(parts) >= 3:
                    used = parts[2].replace("G", "")
                    if used.isdigit():
                        disk["kafka"] = float(used)

    # ODF/NooBaa PVC usage — ODF environments don't have a MinIO pod.
    # Query PVC capacity/usage for the noobaa-default-backing-store PVC instead.
    # Falls back to MinIO pod lookup for non-ODF environments.
    noobaa_pod = get_pod_by_label("openshift-storage", "app=noobaa-core")
    if noobaa_pod:
        result = run_oc_command([
            "exec", "-n", "openshift-storage", noobaa_pod, "--",
            "df", "-BG", "/data"
        ], check=False)

        if result.returncode == 0:
            lines = result.stdout.strip().split("\n")
            if len(lines) > 1:
                parts = lines[1].split()
                if len(parts) >= 3:
                    used = parts[2].replace("G", "")
                    if used.isdigit():
                        disk["object_storage"] = float(used)
    else:
        minio_pod = get_pod_by_label(namespace, "app.kubernetes.io/name=minio")
        if minio_pod:
            result = run_oc_command([
                "exec", "-n", namespace, minio_pod, "--",
                "df", "-BG", "/data"
            ], check=False)

            if result.returncode == 0:
                lines = result.stdout.strip().split("\n")
                if len(lines) > 1:
                    parts = lines[1].split()
                    if len(parts) >= 3:
                        used = parts[2].replace("G", "")
                        if used.isdigit():
                            disk["minio"] = float(used)

    return disk


def collect_queue_depths(namespace: str) -> Dict[str, int]:
    """Collect queue depths from Kafka and Valkey.
    
    Returns:
        Dict with queue_name -> depth mappings
    """
    from .queue_helpers import get_celery_queue_depths

    queues: Dict[str, int] = {}

    # Valkey/Celery queues via shared helper
    celery_depths = get_celery_queue_depths(namespace)
    for q, depth in celery_depths.items():
        queues[f"celery/{q}"] = depth

    # Kafka consumer lag (soak-specific — not covered by get_celery_queue_depths)
    from .kafka_helpers import get_kafka_broker_pod, get_kafka_namespace

    kafka_namespace = get_kafka_namespace()
    kafka_pod = get_kafka_broker_pod(kafka_namespace)

    if kafka_pod:
        result = run_oc_command([
            "exec", "-n", kafka_namespace, kafka_pod, "--",
            "bin/kafka-consumer-groups.sh",
            "--bootstrap-server", "localhost:9092",
            "--all-groups",
            "--describe"
        ], check=False)

        if result.returncode == 0:
            for line in result.stdout.strip().split("\n"):
                if "koku" in line.lower() or "ros" in line.lower():
                    parts = line.split()
                    if len(parts) >= 6:
                        group = parts[0]
                        topic = parts[1]
                        lag = parts[5]
                        if lag.isdigit():
                            queues[f"kafka/{group}/{topic}"] = int(lag)

    return queues


def collect_metrics(namespace: str, start_time: float) -> MetricSample:
    """Collect all metrics for a single sample."""
    memory, cpu = collect_pod_resources(namespace)
    disk = collect_disk_usage(namespace)
    queues = collect_queue_depths(namespace)
    
    return MetricSample(
        timestamp=datetime.now(timezone.utc).isoformat(),
        elapsed_seconds=time.time() - start_time,
        memory_mb=memory,
        cpu_cores=cpu,
        disk_usage_gb=disk,
        queue_depths=queues,
    )


# =============================================================================
# Background Workers
# =============================================================================

def metrics_collector_worker(
    namespace: str,
    state: SoakTestState,
    interval_seconds: float,
):
    """Background worker that collects metrics at regular intervals."""
    while not state.stop_event.is_set():
        try:
            sample = collect_metrics(namespace, state.start_time)
            state.add_sample(sample)
        except Exception as e:
            state.add_error(f"Metrics collection failed: {e}")
        
        state.stop_event.wait(interval_seconds)


def _refresh_token(keycloak_cfg) -> str:
    """Obtain a fresh JWT token string from Keycloak."""
    from conftest import obtain_jwt_token
    token = obtain_jwt_token(keycloak_cfg)
    return token.access_token


def upload_worker(
    namespace: str,
    gateway_url: str,
    upload_url: str,
    jwt_token: str,
    cluster_id: str,
    state: SoakTestState,
    interval_seconds: float,
    db_pod: Optional[str] = None,
    keycloak_config=None,
):
    """Background worker that performs periodic uploads with processing verification.

    After every 3rd successful upload, verifies that data has been ingested by
    checking summary tables. This catches silent data drops without adding
    excessive overhead on every upload cycle.

    Token is refreshed automatically on 401 responses (Keycloak tokens expire
    after ~5 minutes).
    """
    ensure_nise_available()

    current_token = jwt_token
    upload_count = 0
    uploads_since_verify = 0
    VERIFY_EVERY_N = 3

    session = requests.Session()
    session.verify = False

    while not state.stop_event.is_set():
        try:
            end_date = datetime.now(timezone.utc)
            start_date = end_date - timedelta(days=1)

            with tempfile.TemporaryDirectory() as temp_dir:
                nise_result = generate_nise_data(
                    cluster_id=cluster_id,
                    start_date=start_date,
                    end_date=end_date,
                    output_dir=temp_dir,
                )

                if nise_result:
                    pod_usage_files = nise_result.get("pod_usage_files", [])
                    ros_usage_files = nise_result.get("ros_usage_files", [])

                    if pod_usage_files:
                        tar_path = create_upload_package_from_files(
                            pod_usage_files,
                            ros_usage_files if ros_usage_files else [],
                            cluster_id,
                            start_date=start_date,
                            end_date=end_date,
                        )

                        auth_header = {"Authorization": f"Bearer {current_token}"}
                        try:
                            response = upload_with_retry(
                                session, upload_url, tar_path, auth_header,
                                max_retries=2, timeout=120,
                            )
                            if response.status_code == 401 and keycloak_config:
                                current_token = _refresh_token(keycloak_config)
                                print("[upload_worker] Refreshed JWT token (401)")
                                auth_header = {"Authorization": f"Bearer {current_token}"}
                                # Keycloak tokens expire after ~5 min while uploads happen
                                # every 15 min, so nearly every upload takes this path. A single
                                # attempt here (previously max_retries=1) had zero margin for
                                # a transient RemoteDisconnected and permanently failed the
                                # upload, tripping SOAK-001's zero-tolerance assertion on long
                                # runs. Give it the same retry budget as the primary attempt.
                                response = upload_with_retry(
                                    session, upload_url, tar_path, auth_header,
                                    max_retries=2, timeout=120,
                                )

                            if response.status_code in (200, 201, 202):
                                state.increment_uploads(success=True)
                                upload_count += 1
                                uploads_since_verify += 1
                            else:
                                state.increment_uploads(success=False)
                                state.add_error(
                                    f"Upload {upload_count + 1} returned {response.status_code}"
                                )
                        except RuntimeError as ue:
                            state.increment_uploads(success=False)
                            state.add_error(f"Upload {upload_count + 1} failed: {ue}")

                        if db_pod and uploads_since_verify >= VERIFY_EVERY_N:
                            uploads_since_verify = 0
                            try:
                                schema = wait_for_summary_tables(
                                    namespace, db_pod, cluster_id,
                                    timeout=120, interval=30,
                                )
                                if not schema:
                                    state.add_error(
                                        f"Processing verification failed after upload {upload_count}: "
                                        "summary tables not populated"
                                    )
                            except Exception as ve:
                                state.add_error(f"Processing verification error: {ve}")
                else:
                    state.increment_uploads(success=False)
                    state.add_error(f"NISE generation failed for upload {upload_count + 1}")
        except Exception as e:
            state.increment_uploads(success=False)
            state.add_error(f"Upload worker error: {e}")

        state.stop_event.wait(interval_seconds)


def query_worker(
    gateway_url: str,
    jwt_token: str,
    state: SoakTestState,
    interval_seconds: float,
    keycloak_config=None,
):
    """Background worker that performs periodic API queries.

    Token is refreshed automatically on 401 Unauthorized responses.
    """
    endpoints = [
        "/cost-management/v1/sources/",
        "/cost-management/v1/reports/openshift/costs/?filter[time_scope_units]=month&filter[time_scope_value]=-1",
        "/cost-management/v1/reports/openshift/memory/?filter[time_scope_units]=month&filter[time_scope_value]=-1",
        "/cost-management/v1/recommendations/openshift",
    ]

    current_token = jwt_token
    query_idx = 0
    while not state.stop_event.is_set():
        try:
            endpoint = endpoints[query_idx % len(endpoints)]
            url = f"{gateway_url}{endpoint}"

            response = requests.get(
                url,
                headers={"Authorization": f"Bearer {current_token}"},
                timeout=30,
                verify=False,
            )

            if response.status_code == 401 and keycloak_config:
                try:
                    current_token = _refresh_token(keycloak_config)
                    print(f"[query_worker] Refreshed JWT token (401 on {endpoint})")
                    response = requests.get(
                        url,
                        headers={"Authorization": f"Bearer {current_token}"},
                        timeout=30,
                        verify=False,
                    )
                except Exception as refresh_err:
                    # Keep the stale token and retry next iteration rather than
                    # crashing the worker, but surface the failure — silently
                    # swallowing this would otherwise hide a permanent 401 loop
                    # (e.g. Keycloak down/unreachable) for the rest of the run.
                    print(f"[query_worker] Token refresh failed: {refresh_err}")
                    state.add_error(f"Token refresh failed: {refresh_err}")

            if response.status_code in [200, 404]:
                state.increment_queries(success=True)
            else:
                state.increment_queries(success=False)
                state.add_error(f"Query failed: {endpoint} returned {response.status_code}")

            query_idx += 1
        except Exception as e:
            state.increment_queries(success=False)
            state.add_error(f"Query worker error: {e}")

        state.stop_event.wait(interval_seconds)


# =============================================================================
# Analysis Functions
# =============================================================================

def analyze_memory_trend(samples: List[MetricSample]) -> Dict[str, Any]:
    """Analyze memory usage trend over time.
    
    Returns:
        Dict with per-pod memory analysis including growth rate.
    """
    if len(samples) < 2:
        return {"error": "Insufficient samples"}
    
    analysis = {}
    
    # Get all pod names that appear in samples
    all_pods = set()
    for sample in samples:
        all_pods.update(sample.memory_mb.keys())
    
    for pod in all_pods:
        # Get time series for this pod
        times = []
        values = []
        for sample in samples:
            if pod in sample.memory_mb:
                times.append(sample.elapsed_seconds)
                values.append(sample.memory_mb[pod])
        
        if len(values) < 2:
            continue
        
        # Calculate statistics
        initial = values[0]
        final = values[-1]
        peak = max(values)
        avg = sum(values) / len(values)
        
        # Calculate growth rate (MB per hour)
        duration_hours = (times[-1] - times[0]) / 3600
        if duration_hours > 0:
            growth_rate = (final - initial) / duration_hours
            growth_pct = ((final - initial) / initial * 100) if initial > 0 else 0
        else:
            growth_rate = 0
            growth_pct = 0
        
        analysis[pod] = {
            "initial_mb": initial,
            "final_mb": final,
            "peak_mb": peak,
            "avg_mb": avg,
            "growth_rate_mb_per_hour": growth_rate,
            "growth_pct": growth_pct,
            "sample_count": len(values),
        }
    
    return analysis


def analyze_disk_trend(samples: List[MetricSample]) -> Dict[str, Any]:
    """Analyze disk usage trend over time."""
    if len(samples) < 2:
        return {"error": "Insufficient samples"}
    
    analysis = {}
    
    all_components = set()
    for sample in samples:
        all_components.update(sample.disk_usage_gb.keys())
    
    for component in all_components:
        times = []
        values = []
        for sample in samples:
            if component in sample.disk_usage_gb:
                times.append(sample.elapsed_seconds)
                values.append(sample.disk_usage_gb[component])
        
        if len(values) < 2:
            continue
        
        initial = values[0]
        final = values[-1]
        peak = max(values)
        
        duration_hours = (times[-1] - times[0]) / 3600
        growth_rate = (final - initial) / duration_hours if duration_hours > 0 else 0
        
        analysis[component] = {
            "initial_gb": initial,
            "final_gb": final,
            "peak_gb": peak,
            "growth_rate_gb_per_hour": growth_rate,
            "sample_count": len(values),
        }
    
    return analysis


def analyze_queue_health(samples: List[MetricSample]) -> Dict[str, Any]:
    """Analyze queue health over time."""
    if len(samples) < 2:
        return {"error": "Insufficient samples"}
    
    analysis = {}
    
    all_queues = set()
    for sample in samples:
        all_queues.update(sample.queue_depths.keys())
    
    for queue in all_queues:
        values = [s.queue_depths.get(queue, 0) for s in samples]
        
        peak = max(values)
        avg = sum(values) / len(values)
        
        # Count how often queue was non-empty
        non_empty_count = sum(1 for v in values if v > 0)
        non_empty_pct = non_empty_count / len(values) * 100
        
        analysis[queue] = {
            "peak_depth": peak,
            "avg_depth": avg,
            "non_empty_pct": non_empty_pct,
            "sample_count": len(values),
        }
    
    return analysis


def _get_restart_counts(namespace: str) -> Dict[str, int]:
    """Return {pod_name: restart_count} for all pods in the namespace."""
    result = run_oc_command([
        "get", "pods", "-n", namespace,
        "-o", "jsonpath={range .items[*]}{.metadata.name}:{.status.containerStatuses[0].restartCount}{\"\\n\"}{end}"
    ], check=False)
    counts: Dict[str, int] = {}
    if result.returncode == 0:
        for line in result.stdout.strip().split("\n"):
            if ":" in line:
                pod, count = line.split(":", 1)
                if count.isdigit():
                    counts[pod] = int(count)
    return counts


def _get_soak_samples_or_standalone(
    namespace: str, perf_config: PerfTestConfig, test_id: str
) -> List["MetricSample"]:
    """Return SOAK-001's shared samples, or fall back to a short standalone
    collection when no test in this process already populated ``_soak_shared``.

    A missing SOAK-001 fallback almost always means either a deliberate
    standalone invocation (fine — proceed with a short smoke check) or tests
    ran out of order within the same process, e.g. under pytest-randomly or
    pytest-xdist (not fine — silently degrading to a 2-minute sample would
    mask that and produce a hollow pass/fail signal for a run that's supposed
    to be a real multi-hour/multi-day soak). Distinguish the two by duration:
    a non-condensed run with a substantial configured duration is expected to
    have real SOAK-001 data, so treat its absence as an ordering error.
    """
    if "samples" in _soak_shared and len(_soak_shared["samples"]) >= 2:
        samples = _soak_shared["samples"]
        print(f"\n=== {test_id}: Analyzing {len(samples)} samples from SOAK-001 ===")
        return samples

    if not _SOAK_CONDENSED and _SOAK_DURATION_S > 300:
        pytest.skip(
            f"{test_id}: no SOAK-001 data available for a non-condensed "
            f"{_SOAK_DURATION_S}s soak run. This usually means tests ran out "
            "of order within this process (e.g. pytest-randomly/pytest-xdist) "
            "rather than a deliberate standalone run — run the full "
            "TestSoakStability class in its normal order, or set "
            "SOAK_CONDENSED=true to opt into standalone smoke-check mode."
        )

    print(
        f"\n=== {test_id}: No SOAK-001 data — collecting standalone (2 min, "
        "smoke check only; too short for a meaningful trend signal) ==="
    )
    interval = perf_config.soak_metrics_interval_seconds
    samples = []
    start_time = time.time()
    standalone_duration = 120
    while time.time() - start_time < standalone_duration:
        samples.append(collect_metrics(namespace, start_time))
        time.sleep(interval)
    print(f"  Collected {len(samples)} standalone samples")
    return samples


# =============================================================================
# Test Classes
# =============================================================================

@pytest.mark.performance
@pytest.mark.soak
class TestSoakStability:
    """Soak/stability tests (PERF-SOAK-*)."""

    # Soak configuration comes from the session-scoped perf_config fixture
    # provided by conftest.py (PerfTestConfig dataclass).

    @pytest.fixture(scope="class")
    def upload_url(self, gateway_url: str) -> str:
        """Get upload URL via the session-scoped gateway_url.

        gateway_url from conftest already includes /api (e.g. https://host/api).
        """
        return f"{gateway_url}/ingress/v1/upload"

    # ingress_pod and koku_api_url are provided by session-scoped fixtures
    # in conftest.py

    @pytest.mark.skipif(
        not _SOAK_ENABLED,
        reason="SOAK-001 requires SOAK_TESTS=true — multi-hour stability tests are opt-in.",
    )
    @pytest.mark.timeout(_SOAK_DURATION_S * 2 + 600)
    def test_perf_soak_001_continuous_operation(
        self,
        cluster_config,
        perf_cleanup,
        perf_result,
        perf_collector,
        perf_config: PerfTestConfig,
        gateway_url,
        upload_url,
        ingress_pod,
        koku_api_url,
        jwt_token: JWTToken,
        keycloak_config,
        rh_identity_header: str,
    ):
        """PERF-SOAK-001: Continuous operation stability test.
        
        Runs for the configured duration with:
        - Periodic data uploads
        - Periodic API queries
        - Continuous metrics collection
        
        Validates:
        - No OOM kills
        - Sustained throughput
        - System responsiveness
        """
        cluster_id = generate_cluster_id()
        source_name = f"perf-soak-001-{uuid.uuid4().hex[:8]}"
        
        # Register source
        source = register_source(
            cluster_config.namespace,
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
        
        # Initialize state
        state = SoakTestState()

        # Capture baseline restart counts so we only flag restarts that happen
        # during the soak window (not pre-existing ones from prior runs).
        baseline_restarts = _get_restart_counts(cluster_config.namespace)

        # Get DB pod for processing verification
        db_pod = get_pod_by_label(cluster_config.namespace, "app.kubernetes.io/component=database")
        
        # Start background workers
        threads = []
        
        # Metrics collector
        metrics_thread = threading.Thread(
            target=metrics_collector_worker,
            args=(cluster_config.namespace, state, perf_config.soak_metrics_interval_seconds),
        )
        metrics_thread.start()
        threads.append(metrics_thread)
        
        # Upload worker
        upload_thread = threading.Thread(
            target=upload_worker,
            args=(
                cluster_config.namespace,
                gateway_url,
                upload_url,
                jwt_token.access_token,
                cluster_id,
                state,
                _soak_upload_interval_seconds(perf_config),
                db_pod,
                keycloak_config,
            ),
        )
        upload_thread.start()
        threads.append(upload_thread)

        # Query worker
        query_thread = threading.Thread(
            target=query_worker,
            args=(
                gateway_url,
                jwt_token.access_token,
                state,
                _soak_query_interval_seconds(perf_config),
                keycloak_config,
            ),
        )
        query_thread.start()
        threads.append(query_thread)
        
        mode_label = "CONDENSED" if _SOAK_CONDENSED else "STANDARD"
        print(f"\n=== PERF-SOAK-001: Starting {perf_config.soak_duration_hours}h soak test ({mode_label}) ===")
        print(f"  Upload interval: {perf_config.soak_upload_interval_minutes} min")
        print(f"  Query interval: {perf_config.soak_query_interval_minutes} min")
        print(f"  Metrics interval: {perf_config.soak_metrics_interval_seconds} sec")
        
        try:
            start = time.time()
            soak_duration_s = _soak_duration_seconds(perf_config)
            report_interval = 60 if _SOAK_CONDENSED else 300
            last_report = 0.0
            while time.time() - start < soak_duration_s:
                elapsed = time.time() - start
                remaining = soak_duration_s - elapsed

                if elapsed - last_report >= report_interval:
                    last_report = elapsed
                    print(f"  Progress: {elapsed/3600:.1f}h elapsed, {remaining/3600:.1f}h remaining")
                    print(f"    Uploads: {state.uploads_completed} ok, {state.uploads_failed} failed")
                    print(f"    Queries: {state.queries_completed} ok, {state.queries_failed} failed")
                    print(f"    Samples: {len(state.samples)}")

                time.sleep(min(30, report_interval))
        finally:
            # Stop workers
            state.stop_event.set()
            for t in threads:
                t.join(timeout=30)
        
        # Share collected samples with SOAK-002/003/004
        _soak_shared["samples"] = state.samples
        _soak_shared["state"] = state

        # Analyze results
        memory_analysis = analyze_memory_trend(state.samples)
        disk_analysis = analyze_disk_trend(state.samples)
        queue_analysis = analyze_queue_health(state.samples)

        # Check for restarts that occurred *during* the soak (delta from baseline)
        current_restarts = _get_restart_counts(cluster_config.namespace)
        restarts = {}
        for pod, count in current_restarts.items():
            baseline = baseline_restarts.get(pod, 0)
            delta = count - baseline
            if delta > 0:
                restarts[pod] = delta
        
        # Build result
        perf_result.metrics = {
            "duration_hours": perf_config.soak_duration_hours,
            "duration_actual_seconds": state.elapsed_seconds,
            "uploads_completed": state.uploads_completed,
            "uploads_failed": state.uploads_failed,
            "queries_completed": state.queries_completed,
            "queries_failed": state.queries_failed,
            "sample_count": len(state.samples),
            "pod_restarts": restarts,
            "memory_analysis": memory_analysis,
            "disk_analysis": disk_analysis,
            "queue_analysis": queue_analysis,
            "errors": state.errors[-20:],  # Last 20 errors
        }
        
        perf_result.passed = (
            state.uploads_failed == 0 and
            state.queries_failed / max(state.queries_completed, 1) < 0.05 and
            len(restarts) == 0
        )
        
        perf_collector.add_result(perf_result)
        
        print(f"\n=== PERF-SOAK-001 Results ===")
        print(f"  Duration: {state.elapsed_seconds/3600:.2f} hours")
        print(f"  Uploads: {state.uploads_completed} ok, {state.uploads_failed} failed")
        print(f"  Queries: {state.queries_completed} ok, {state.queries_failed} failed")
        print(f"  Pod restarts: {restarts if restarts else 'None'}")
        print(f"  Errors: {len(state.errors)}")
        
        assert len(restarts) == 0, f"Pod restarts detected (possible OOM): {restarts}"
        assert state.uploads_failed == 0, f"{state.uploads_failed} uploads failed"

    @pytest.mark.skipif(
        not _SOAK_ENABLED,
        reason="SOAK-002 requires SOAK_TESTS=true — multi-hour stability tests are opt-in.",
    )
    @pytest.mark.timeout(600)
    def test_perf_soak_002_memory_leak_detection(
        self,
        cluster_config,
        perf_result,
        perf_collector,
        perf_config: PerfTestConfig,
    ):
        """PERF-SOAK-002: Memory leak detection.

        Analyzes SOAK-001's collected samples for memory growth patterns.
        If SOAK-001 hasn't run, does a short standalone collection (2 min).

        Success criteria: < 5% memory growth per day (extrapolated)
        """
        samples = _get_soak_samples_or_standalone(
            cluster_config.namespace, perf_config, "PERF-SOAK-002"
        )

        actual_duration_s = samples[-1].elapsed_seconds if samples else 0

        # Analyze memory trend
        memory_analysis = analyze_memory_trend(samples)

        leak_detected = False

        # For short runs (< 1h), extrapolation to daily growth is unreliable.
        # Normal JVM/Python GC and cache warm-up cause memory fluctuations that
        # extrapolate to hundreds of percent daily growth when measured over
        # 15 minutes. Require substantial absolute growth to flag a leak.
        #
        # This floor also applies to normal (>= 1h) windows: a 1-4MB swing on
        # a low-memory pod (e.g. 36MB -> 37MB) extrapolates to 60%+/day when
        # projected linearly from a single hour, but it's pure measurement
        # noise, not a leak. See COST-7634 soak validation findings.
        duration_hours = actual_duration_s / 3600 if actual_duration_s > 0 else 0
        daily_threshold = 5.0 if duration_hours >= 1.0 else 50.0
        min_absolute_growth_mb = 50.0

        candidates = []
        for pod, stats in memory_analysis.items():
            if isinstance(stats, dict) and "growth_pct" in stats:
                if duration_hours > 0:
                    daily_growth_pct = stats["growth_pct"] * (24 / duration_hours)
                    absolute_growth = stats["final_mb"] - stats["initial_mb"]
                    if daily_growth_pct > daily_threshold and absolute_growth > min_absolute_growth_mb:
                        candidates.append({
                            "pod": pod,
                            "daily_growth_pct": daily_growth_pct,
                            "initial_mb": stats["initial_mb"],
                            "final_mb": stats["final_mb"],
                            "absolute_growth_mb": absolute_growth,
                        })

        # Require growth to persist across consecutive iterations before
        # failing. A single hour's step-change (e.g. one-time JVM heap growth
        # during warm-up, which then plateaus) looks identical to the start
        # of a leak when viewed through one 1-hour window in isolation.
        state_path = _soak_leak_state_path()
        leak_state = _load_soak_leak_state(state_path)
        candidate_pods = {c["pod"] for c in candidates}
        for pod in list(leak_state.keys()):
            if pod not in candidate_pods:
                leak_state[pod] = 0
        for pod in candidate_pods:
            leak_state[pod] = leak_state.get(pod, 0) + 1
        _save_soak_leak_state(state_path, leak_state)

        if state_path is None:
            # No loop context (standalone/continuous run) — nothing to wait
            # for next iteration, so evaluate immediately.
            leak_pods = candidates
        else:
            leak_pods = [
                c for c in candidates
                if leak_state.get(c["pod"], 0) >= _SOAK_LEAK_CONSECUTIVE_ITERATIONS
            ]

        leak_detected = bool(leak_pods)

        perf_result.metrics = {
            "duration_seconds": actual_duration_s,
            "sample_count": len(samples),
            "memory_analysis": memory_analysis,
            "leak_detected": leak_detected,
            "leak_pods": leak_pods,
            "leak_candidates": candidates,
        }

        perf_result.passed = not leak_detected
        perf_collector.add_result(perf_result)

        print(f"\n=== PERF-SOAK-002 Results ===")
        print(f"  Duration: {actual_duration_s/60:.0f} minutes")
        print(f"  Samples: {len(samples)}")
        print(f"  Leak detected: {leak_detected}")
        if candidates:
            for c in candidates:
                confirmed = leak_state.get(c["pod"], 0) >= _SOAK_LEAK_CONSECUTIVE_ITERATIONS
                marker = "CONFIRMED" if confirmed else f"watch ({leak_state.get(c['pod'], 0)}/{_SOAK_LEAK_CONSECUTIVE_ITERATIONS})"
                print(f"    - {c['pod']}: {c['daily_growth_pct']:.1f}% daily growth [{marker}]")

        assert not leak_detected, f"Memory leak detected in pods: {[p['pod'] for p in leak_pods]}"

    @pytest.mark.skipif(
        not _SOAK_ENABLED,
        reason="SOAK-003 requires SOAK_TESTS=true — multi-hour stability tests are opt-in.",
    )
    @pytest.mark.timeout(600)
    def test_perf_soak_003_disk_usage_monitoring(
        self,
        cluster_config,
        perf_result,
        perf_collector,
        perf_config: PerfTestConfig,
    ):
        """PERF-SOAK-003: Disk usage monitoring.

        Analyzes SOAK-001's collected samples for disk growth patterns.
        If SOAK-001 hasn't run, does a short standalone collection (2 min).

        Success criteria: No disk exhaustion warnings (< 50 GB projected 7-day growth)
        """
        samples = _get_soak_samples_or_standalone(
            cluster_config.namespace, perf_config, "PERF-SOAK-003"
        )

        actual_duration_s = samples[-1].elapsed_seconds if samples else 0

        # Analyze disk trend
        disk_analysis = analyze_disk_trend(samples)

        warnings = []
        for component, stats in disk_analysis.items():
            if isinstance(stats, dict) and "growth_rate_gb_per_hour" in stats:
                projected_growth = stats["growth_rate_gb_per_hour"] * 24 * 7
                if projected_growth > 50:
                    warnings.append({
                        "component": component,
                        "current_gb": stats["final_gb"],
                        "projected_7day_growth_gb": projected_growth,
                    })

        perf_result.metrics = {
            "duration_seconds": actual_duration_s,
            "sample_count": len(samples),
            "disk_analysis": disk_analysis,
            "warnings": warnings,
        }

        perf_result.passed = len(warnings) == 0
        perf_collector.add_result(perf_result)

        print(f"\n=== PERF-SOAK-003 Results ===")
        print(f"  Duration: {actual_duration_s/60:.0f} minutes")
        for component, stats in disk_analysis.items():
            if isinstance(stats, dict):
                print(f"  {component}:")
                print(f"    Current: {stats.get('final_gb', 'N/A')} GB")
                print(f"    Growth rate: {stats.get('growth_rate_gb_per_hour', 0):.2f} GB/hour")
        
        if warnings:
            print(f"  Warnings: {len(warnings)}")
            for w in warnings:
                print(f"    - {w['component']}: {w['projected_7day_growth_gb']:.1f} GB projected growth in 7 days")

    @pytest.mark.skipif(
        not _SOAK_ENABLED,
        reason="SOAK-004 requires SOAK_TESTS=true — multi-hour stability tests are opt-in.",
    )
    @pytest.mark.timeout(600)
    def test_perf_soak_004_queue_health_monitoring(
        self,
        cluster_config,
        perf_result,
        perf_collector,
        perf_config: PerfTestConfig,
    ):
        """PERF-SOAK-004: Queue health monitoring.

        Analyzes SOAK-001's collected samples for queue starvation or backlog.
        If SOAK-001 hasn't run, does a short standalone collection (2 min).

        Success criteria: No sustained queue growth indicating processing backup
        """
        samples = _get_soak_samples_or_standalone(
            cluster_config.namespace, perf_config, "PERF-SOAK-004"
        )

        actual_duration_s = samples[-1].elapsed_seconds if samples else 0

        # Analyze queue health
        queue_analysis = analyze_queue_health(samples)

        concerns = []
        for queue, stats in queue_analysis.items():
            if isinstance(stats, dict):
                if stats.get("avg_depth", 0) > 100:
                    concerns.append({
                        "queue": queue,
                        "issue": "high_avg_depth",
                        "avg_depth": stats["avg_depth"],
                        "peak_depth": stats["peak_depth"],
                    })
                if stats.get("non_empty_pct", 0) > 90:
                    concerns.append({
                        "queue": queue,
                        "issue": "sustained_backlog",
                        "non_empty_pct": stats["non_empty_pct"],
                    })

        perf_result.metrics = {
            "duration_seconds": actual_duration_s,
            "sample_count": len(samples),
            "queue_analysis": queue_analysis,
            "concerns": concerns,
        }

        perf_result.passed = len(concerns) == 0
        perf_collector.add_result(perf_result)

        print(f"\n=== PERF-SOAK-004 Results ===")
        print(f"  Duration: {actual_duration_s/60:.0f} minutes")
        for queue, stats in queue_analysis.items():
            if isinstance(stats, dict):
                print(f"  {queue}:")
                print(f"    Peak: {stats.get('peak_depth', 0)}, Avg: {stats.get('avg_depth', 0):.1f}")
                print(f"    Non-empty: {stats.get('non_empty_pct', 0):.0f}%")
        
        if concerns:
            print(f"  Concerns: {len(concerns)}")
            for c in concerns:
                print(f"    - {c['queue']}: {c['issue']}")
