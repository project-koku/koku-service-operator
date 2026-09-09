"""
Valkey Eviction Correlation Tests (COST-7605 DB-3).

Tests whether Valkey key evictions cause Celery chord failures during
ingestion workloads.  Constrains Valkey memory to force evictions under
load, then correlates eviction rate with task failure rate and chord
completion success.

Test IDs:
- PERF-VK-001: Eviction correlation under constrained memory
"""

import json
import os
import threading
import time
from dataclasses import asdict, dataclass, field
from datetime import datetime, timedelta, timezone
from typing import Any, Dict, List, Optional

import pytest

from conftest import ClusterConfig, JWTToken, obtain_jwt_token
from e2e_helpers import (
    wait_for_processing_complete,
    cleanup_database_records,
    ensure_nise_available,
    generate_cluster_id,
    register_source,
)
from utils import get_pod_by_label, run_oc_command

from .data_classes import PerformanceResult
from .helpers import (
    PerfResultCollector,
    PerfTimer,
    generate_and_upload_data,
    save_perf_result,
)
from .queue_helpers import get_celery_queue_depths
from .tracker import PerfCleanupTracker
from .profiles import ACTIVE_PROFILE as _ACTIVE_PROFILE, PROFILES


# =============================================================================
# Data Classes
# =============================================================================

@dataclass
class ValkeySnapshot:
    """Point-in-time Valkey metrics."""
    timestamp: float
    evicted_keys: int = 0
    used_memory_bytes: int = 0
    maxmemory_bytes: int = 0
    connected_clients: int = 0
    keyspace_hits: int = 0
    keyspace_misses: int = 0
    db0_keys: int = 0
    celery_result_count: int = 0

    @property
    def memory_pct(self) -> float:
        if self.maxmemory_bytes == 0:
            return 0.0
        return (self.used_memory_bytes / self.maxmemory_bytes) * 100

    def to_dict(self) -> Dict[str, Any]:
        d = asdict(self)
        d["memory_pct"] = round(self.memory_pct, 1)
        return d


@dataclass
class CeleryTaskSnapshot:
    """Point-in-time Celery task state from Valkey result backend."""
    timestamp: float
    total_results: int = 0
    success_count: int = 0
    failure_count: int = 0
    pending_count: int = 0
    queue_depths: Dict[str, int] = field(default_factory=dict)

    @property
    def failure_rate(self) -> float:
        if self.total_results == 0:
            return 0.0
        return self.failure_count / self.total_results

    def to_dict(self) -> Dict[str, Any]:
        d = asdict(self)
        d["failure_rate"] = round(self.failure_rate, 4)
        return d


# =============================================================================
# Valkey Monitor
# =============================================================================

class ValkeyMonitor:
    """Background thread that polls Valkey metrics during a test run."""

    def __init__(self, namespace: str, poll_interval: float = 5.0):
        self.namespace = namespace
        self.poll_interval = poll_interval
        self.valkey_snapshots: List[ValkeySnapshot] = []
        self.celery_snapshots: List[CeleryTaskSnapshot] = []
        self._stop = threading.Event()
        self._thread: Optional[threading.Thread] = None

    def _get_valkey_pod(self) -> Optional[str]:
        return get_pod_by_label(self.namespace, "app.kubernetes.io/component=cache")

    def _query_valkey_info(self, pod: str) -> Optional[ValkeySnapshot]:
        result = run_oc_command(
            ["exec", "-n", self.namespace, pod, "--",
             "valkey-cli", "INFO"],
            check=False,
        )
        if result.returncode != 0:
            return None

        snap = ValkeySnapshot(timestamp=time.time())
        for line in result.stdout.splitlines():
            line = line.strip()
            if line.startswith("evicted_keys:"):
                snap.evicted_keys = int(line.split(":")[1])
            elif line.startswith("used_memory:") and not line.startswith("used_memory_"):
                snap.used_memory_bytes = int(line.split(":")[1])
            elif line.startswith("maxmemory:"):
                snap.maxmemory_bytes = int(line.split(":")[1])
            elif line.startswith("connected_clients:"):
                snap.connected_clients = int(line.split(":")[1])
            elif line.startswith("keyspace_hits:"):
                snap.keyspace_hits = int(line.split(":")[1])
            elif line.startswith("keyspace_misses:"):
                snap.keyspace_misses = int(line.split(":")[1])
            elif line.startswith("db0:"):
                try:
                    parts = dict(kv.split("=") for kv in line.split(":")[1].split(","))
                    snap.db0_keys = int(parts.get("keys", 0))
                except (ValueError, KeyError):
                    pass
            elif line.startswith("db1:"):
                try:
                    parts = dict(kv.split("=") for kv in line.split(":")[1].split(","))
                    snap.celery_result_count = int(parts.get("keys", 0))
                except (ValueError, KeyError):
                    pass
        return snap

    def _query_celery_task_states(self, pod: str) -> CeleryTaskSnapshot:
        """Count Celery task results by state in Valkey DB1.

        Uses DBSIZE for total count and a single oc exec to sample
        task statuses, avoiding per-key round-trips.
        """
        snap = CeleryTaskSnapshot(timestamp=time.time())

        result = run_oc_command(
            ["exec", "-n", self.namespace, pod, "--",
             "valkey-cli", "-n", "1", "DBSIZE"],
            check=False,
        )
        if result.returncode == 0:
            try:
                snap.total_results = int(result.stdout.strip())
            except ValueError:
                pass

        # Sample task results using Lua EVAL (single round-trip)
        lua_sample = r"""
local result = redis.call("SCAN", "0", "MATCH", "celery-task-meta-*", "COUNT", 30)
local keys = result[2]
local counts = {SUCCESS=0, FAILURE=0, PENDING=0}
for i = 1, math.min(#keys, 20) do
    local val = redis.call("GET", keys[i])
    if val then
        local ok, data = pcall(cjson.decode, val)
        if ok and data and data.status then
            if counts[data.status] ~= nil then
                counts[data.status] = counts[data.status] + 1
            end
        end
    end
end
return cjson.encode(counts)
"""
        result = run_oc_command(
            ["exec", "-n", self.namespace, pod, "--",
             "valkey-cli", "-n", "1", "--no-auth-warning",
             "EVAL", lua_sample, "0"],
            check=False,
            timeout=15,
        )
        if result.returncode == 0 and result.stdout.strip():
            try:
                counts = json.loads(result.stdout.strip())
                snap.success_count = counts.get("SUCCESS", 0)
                snap.failure_count = counts.get("FAILURE", 0)
                snap.pending_count = counts.get("PENDING", 0)
            except (json.JSONDecodeError, ValueError):
                pass

        snap.queue_depths = get_celery_queue_depths(self.namespace)
        return snap

    def _poll_loop(self):
        pod = self._get_valkey_pod()
        if not pod:
            print("[valkey-monitor] WARNING: Could not find Valkey pod")
            return

        while not self._stop.is_set():
            try:
                valkey_snap = self._query_valkey_info(pod)
                if valkey_snap:
                    self.valkey_snapshots.append(valkey_snap)

                celery_snap = self._query_celery_task_states(pod)
                self.celery_snapshots.append(celery_snap)

                evicted = valkey_snap.evicted_keys if valkey_snap else "?"
                mem_pct = f"{valkey_snap.memory_pct:.0f}%" if valkey_snap else "?"
                celery_result_count = valkey_snap.celery_result_count if valkey_snap else "?"
                failures = celery_snap.failure_count
                queue_total = sum(celery_snap.queue_depths.values())

                print(
                    f"[valkey-monitor] evicted={evicted} mem={mem_pct} "
                    f"result_keys={celery_result_count} task_failures={failures} "
                    f"queue_depth={queue_total}"
                )
            except Exception as e:
                print(f"[valkey-monitor] poll error: {e}")

            self._stop.wait(self.poll_interval)

    def start(self):
        self._thread = threading.Thread(target=self._poll_loop, daemon=True)
        self._thread.start()
        print(f"[valkey-monitor] Started (polling every {self.poll_interval}s)")

    def stop(self) -> dict:
        self._stop.set()
        if self._thread:
            self._thread.join(timeout=10)

        if not self.valkey_snapshots:
            return {"error": "no data collected"}

        first = self.valkey_snapshots[0]
        last = self.valkey_snapshots[-1]
        duration = last.timestamp - first.timestamp

        total_evictions = last.evicted_keys - first.evicted_keys
        eviction_rate = total_evictions / duration if duration > 0 else 0

        peak_memory_pct = max(s.memory_pct for s in self.valkey_snapshots)
        peak_celery_result_count = max(s.celery_result_count for s in self.valkey_snapshots)

        total_failures_sampled = sum(s.failure_count for s in self.celery_snapshots)
        total_success_sampled = sum(s.success_count for s in self.celery_snapshots)

        summary = {
            "duration_seconds": round(duration, 1),
            "total_evictions": total_evictions,
            "eviction_rate_per_sec": round(eviction_rate, 2),
            "peak_memory_pct": round(peak_memory_pct, 1),
            "peak_result_keys": peak_celery_result_count,
            "samples_collected": len(self.valkey_snapshots),
            "sampled_task_failures": total_failures_sampled,
            "sampled_task_successes": total_success_sampled,
            "valkey_snapshots": [s.to_dict() for s in self.valkey_snapshots],
            "celery_snapshots": [s.to_dict() for s in self.celery_snapshots],
        }

        print(
            f"[valkey-monitor] Stopped. {len(self.valkey_snapshots)} samples over "
            f"{duration:.0f}s. Evictions: {total_evictions} "
            f"({eviction_rate:.1f}/s). Peak mem: {peak_memory_pct:.0f}%. "
            f"Task failures sampled: {total_failures_sampled}"
        )
        return summary


# =============================================================================
# Valkey Resource Helpers
# =============================================================================

def get_valkey_config(namespace: str) -> dict:
    """Read current Valkey CONFIG settings."""
    pod = get_pod_by_label(namespace, "app.kubernetes.io/component=cache")
    if not pod:
        return {}

    config = {}
    for key in ["maxmemory", "maxmemory-policy"]:
        result = run_oc_command(
            ["exec", "-n", namespace, pod, "--",
             "valkey-cli", "CONFIG", "GET", key],
            check=False,
        )
        if result.returncode == 0:
            lines = result.stdout.strip().splitlines()
            if len(lines) >= 2:
                config[key] = lines[1].strip()
    return config


def set_valkey_maxmemory(namespace: str, maxmemory: str) -> bool:
    """Set Valkey maxmemory via CONFIG SET (takes effect immediately)."""
    pod = get_pod_by_label(namespace, "app.kubernetes.io/component=cache")
    if not pod:
        return False

    result = run_oc_command(
        ["exec", "-n", namespace, pod, "--",
         "valkey-cli", "CONFIG", "SET", "maxmemory", maxmemory],
        check=False,
    )
    if result.returncode == 0 and "OK" in result.stdout:
        print(f"[valkey-config] Set maxmemory={maxmemory}")
        return True

    print(f"[valkey-config] Failed to set maxmemory: {result.stderr}")
    return False


def count_celery_task_failures(namespace: str) -> dict:
    """Count Celery task results by status in Valkey DB1.

    Uses a Lua EVAL script inside a single oc exec to scan all
    celery-task-meta-* keys and tally statuses without per-key
    round-trips.
    """
    pod = get_pod_by_label(namespace, "app.kubernetes.io/component=cache")
    if not pod:
        return {"error": "valkey pod not found"}

    # Lua script that scans all task-meta keys and counts by status.
    # cjson is built into Valkey/Redis.
    lua_script = r"""
local cursor = "0"
local counts = {}
local total = 0
repeat
    local result = redis.call("SCAN", cursor, "MATCH", "celery-task-meta-*", "COUNT", 100)
    cursor = result[1]
    local keys = result[2]
    for _, key in ipairs(keys) do
        local val = redis.call("GET", key)
        if val then
            local ok, data = pcall(cjson.decode, val)
            if ok and data and data.status then
                counts[data.status] = (counts[data.status] or 0) + 1
            else
                counts["OTHER"] = (counts["OTHER"] or 0) + 1
            end
            total = total + 1
        end
    end
until cursor == "0"
counts["total_scanned"] = total
return cjson.encode(counts)
"""

    result = run_oc_command(
        ["exec", "-n", namespace, pod, "--",
         "valkey-cli", "-n", "1", "--no-auth-warning",
         "EVAL", lua_script, "0"],
        check=False,
        timeout=60,
    )
    if result.returncode == 0 and result.stdout.strip():
        try:
            return json.loads(result.stdout.strip())
        except (json.JSONDecodeError, ValueError):
            pass

    # Fallback: just get DBSIZE
    db_result = run_oc_command(
        ["exec", "-n", namespace, pod, "--",
         "valkey-cli", "-n", "1", "DBSIZE"],
        check=False,
    )
    total = 0
    if db_result.returncode == 0:
        try:
            total = int(db_result.stdout.strip())
        except ValueError:
            pass
    return {"total_keys": total, "fallback": True}


def get_chord_failure_logs(namespace: str, release: str, since_seconds: int = 600) -> List[str]:
    """Extract chord-related error messages from Celery worker logs."""
    chord_errors = []
    workers = ["celery-worker-ocp", "celery-worker-summary", "celery-worker-priority"]

    for worker in workers:
        result = run_oc_command(
            ["logs", f"deploy/{release}-{worker}",
             "-n", namespace,
             f"--since={since_seconds}s",
             "--tail=500"],
            check=False,
        )
        if result.returncode != 0:
            continue

        for line in result.stdout.splitlines():
            lower = line.lower()
            if any(kw in lower for kw in [
                "chord", "chorderror", "groupresult",
                "key was evicted", "result lost",
                "task raised unexpected",
                "celery.exceptions",
            ]):
                chord_errors.append(f"[{worker}] {line.strip()}")

    return chord_errors


# =============================================================================
# Tests
# =============================================================================

# Memory levels to test: from chart default down to constrained
_VK_001_MEMORY_LEVELS: dict = {
    "baseline": [pytest.param("512Mi", id="512Mi-default")],
    "small":    [pytest.param("512Mi", id="512Mi-default"),
                 pytest.param("2Mi",   id="2Mi-tight-maxmem")],
    "medium":   [pytest.param("512Mi", id="512Mi-default"),
                 pytest.param("2Mi",   id="2Mi-tight-maxmem"),
                 pytest.param("baseline+10K", id="baseline-plus-10K")],
    "large":    [pytest.param("512Mi", id="512Mi-default"),
                 pytest.param("4Mi",   id="4Mi-constrained-maxmem"),
                 pytest.param("2Mi",   id="2Mi-tight-maxmem"),
                 pytest.param("baseline+10K", id="baseline-plus-10K")],
}
VK_001_PARAMS = _VK_001_MEMORY_LEVELS.get(
    _ACTIVE_PROFILE, _VK_001_MEMORY_LEVELS["medium"]
)


@pytest.mark.performance
@pytest.mark.valkey_eviction
@pytest.mark.slow
class TestValkeyEvictionCorrelation:
    """COST-7605 DB-3: Valkey eviction → Celery chord failure correlation."""

    @pytest.fixture(autouse=True)
    def setup(self, cluster_config: ClusterConfig, keycloak_config):
        self.namespace = cluster_config.namespace
        self.helm_release = cluster_config.helm_release_name
        self._keycloak_config = keycloak_config

        if not ensure_nise_available():
            pytest.skip("NISE (koku-nise) not available")

    def _get_fresh_token(self) -> JWTToken:
        return obtain_jwt_token(self._keycloak_config)

    @pytest.mark.timeout(2400)
    @pytest.mark.parametrize("valkey_memory", VK_001_PARAMS)
    def test_perf_vk_001_eviction_correlation(
        self,
        valkey_memory: str,
        cluster_config: ClusterConfig,
        ingress_url: str,
        database_config,
        koku_api_url: str,
        perf_timer: PerfTimer,
        perf_result: PerformanceResult,
        perf_collector: PerfResultCollector,
        rh_identity_header: str,
        perf_cleanup: PerfCleanupTracker,
        ingress_pod: str,
    ):
        """PERF-VK-001: Eviction correlation under constrained Valkey memory.

        For each memory level:
        1. Constrain Valkey to the target memory
        2. Run a medium-profile ingestion (generates significant Celery chord work)
        3. Monitor evictions and task failures throughout
        4. Check whether processing completed successfully
        5. Restore original Valkey memory
        """
        profile_name = _ACTIVE_PROFILE if _ACTIVE_PROFILE in PROFILES else "medium"
        is_constrained = valkey_memory != "512Mi"

        print(f"\n{'='*70}")
        print(f"PERF-VK-001: Valkey memory = {valkey_memory} (profile: {profile_name})")
        print(f"{'='*70}")

        # Record original CONFIG for restoration (no K8s patching needed)
        original_config = get_valkey_config(self.namespace)
        print(f"Original Valkey config: {original_config}")

        monitor = None
        monitor_summary = {"error": "monitor never started"}
        processing_complete = False
        processing_elapsed = 0
        pre_task_counts = {}
        post_task_counts = {}
        chord_errors = []
        target_maxmemory = 0
        upload_result = {}

        try:
            # Constrain via CONFIG SET maxmemory only (no pod restart)
            if is_constrained:
                with perf_timer.measure("valkey_memory_constraint"):
                    target_maxmemory = _resolve_maxmemory(
                        valkey_memory, self.namespace
                    )
                    print(f"Setting maxmemory to {target_maxmemory} bytes "
                          f"({target_maxmemory / (1024*1024):.1f} MB)")
                    set_valkey_maxmemory(self.namespace, str(target_maxmemory))

            # Record baseline eviction count
            monitor = ValkeyMonitor(self.namespace, poll_interval=10.0)
            baseline_snap = monitor._query_valkey_info(monitor._get_valkey_pod())
            baseline_evictions = baseline_snap.evicted_keys if baseline_snap else 0
            print(f"Baseline evictions before test: {baseline_evictions}")

            # Start continuous monitoring
            monitor.start()

            # Pre-test: count existing task results
            pre_task_counts = count_celery_task_failures(self.namespace)
            print(f"Pre-test task results: {pre_task_counts}")

            # Register source and generate data
            cluster_id = generate_cluster_id()
            source_name = f"perf-vk-001-{valkey_memory.lower()}-{cluster_id[-8:]}"
            db_pod = database_config.pod_name if database_config else None
            cleanup_database_records(self.namespace, db_pod, cluster_id)

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
            jwt_token = self._get_fresh_token()

            with perf_timer.measure("data_generation_and_upload"):
                upload_result = generate_and_upload_data(
                    cluster_id, source_name,
                    start_date, end_date,
                    ingress_url, jwt_token,
                    profile_name=profile_name,
                )

            with perf_timer.measure("processing_wait"):
                proc = wait_for_processing_complete(
                    self.namespace, db_pod, cluster_id,
                    max_wait_seconds=1800,
                )

            processing_complete = proc.get("complete", False)
            processing_elapsed = proc.get("elapsed_s", 0)

        finally:
            # ALWAYS stop monitor
            if monitor:
                monitor_summary = monitor.stop()

            # ALWAYS restore Valkey maxmemory to original
            if is_constrained:
                original_maxmem = original_config.get("maxmemory", "536870912")
                print(f"\nRestoring Valkey maxmemory to {original_maxmem}...")
                set_valkey_maxmemory(self.namespace, original_maxmem)

        # Post-test: count task results
        post_task_counts = count_celery_task_failures(self.namespace)
        print(f"Post-test task results: {post_task_counts}")

        # Collect chord error logs
        chord_errors = get_chord_failure_logs(
            self.namespace, self.helm_release, since_seconds=2400
        )
        if chord_errors:
            print(f"\nChord-related errors ({len(chord_errors)}):")
            for err in chord_errors[:20]:
                print(f"  {err}")

        # Calculate delta
        new_failures = (
            post_task_counts.get("FAILURE", 0) - pre_task_counts.get("FAILURE", 0)
        )

        # Compute ingestion throughput
        throughput_mb_s = 0
        if processing_elapsed > 0 and upload_result.get("package_size_mb"):
            throughput_mb_s = upload_result["package_size_mb"] / processing_elapsed

        # Build result
        perf_result.test_id = f"PERF-VK-001-{valkey_memory}"
        perf_result.metrics = {
            "valkey_memory_limit": valkey_memory,
            "target_maxmemory_bytes": target_maxmemory,
            "profile": profile_name,
            "is_constrained": is_constrained,
            "processing_complete": processing_complete,
            "processing_elapsed_s": processing_elapsed,
            "ingestion": {
                "upload": upload_result,
                "throughput_mb_per_sec": round(throughput_mb_s, 4),
            },
            "evictions": {
                "total": monitor_summary.get("total_evictions", 0),
                "rate_per_sec": monitor_summary.get("eviction_rate_per_sec", 0),
                "baseline": baseline_evictions,
            },
            "memory": {
                "peak_pct": monitor_summary.get("peak_memory_pct", 0),
                "peak_result_keys": monitor_summary.get("peak_result_keys", 0),
            },
            "task_failures": {
                "new_failures": new_failures,
                "pre_test": pre_task_counts,
                "post_test": post_task_counts,
                "sampled_during_test": monitor_summary.get("sampled_task_failures", 0),
            },
            "chord_errors": {
                "count": len(chord_errors),
                "samples": chord_errors[:10],
            },
            "monitor_samples": monitor_summary.get("samples_collected", 0),
            "monitor_duration_s": monitor_summary.get("duration_seconds", 0),
        }
        perf_result.timings = perf_timer.get_timings()
        perf_result.passed = processing_complete
        perf_collector.add_result(perf_result)

        # Print summary
        evictions = monitor_summary.get("total_evictions", 0)
        eviction_rate = monitor_summary.get("eviction_rate_per_sec", 0)
        peak_mem = monitor_summary.get("peak_memory_pct", 0)
        pkg_mb = upload_result.get("package_size_mb", 0)
        upload_s = upload_result.get("upload_seconds", 0)
        print(f"\n{'='*70}")
        print(f"PERF-VK-001 SUMMARY — Valkey {valkey_memory}")
        print(f"  Processing complete: {processing_complete}")
        print(f"  Processing time:     {processing_elapsed:.0f}s")
        print(f"  Package size:        {pkg_mb:.2f} MB ({upload_result.get('csv_file_count', '?')} files)")
        print(f"  Upload time:         {upload_s:.1f}s ({upload_result.get('upload_mb_per_second', 0):.2f} MB/s)")
        print(f"  Ingestion throughput:{throughput_mb_s:.4f} MB/s (end-to-end)")
        print(f"  Total evictions:     {evictions} ({eviction_rate:.1f}/s)")
        print(f"  Peak memory usage:   {peak_mem:.0f}%")
        print(f"  New task failures:   {new_failures}")
        print(f"  Chord errors:        {len(chord_errors)}")
        print(f"{'='*70}\n")

        if not is_constrained:
            assert processing_complete, (
                "Processing failed at default Valkey memory — baseline failure"
            )
        else:
            if not processing_complete:
                print(
                    f"FINDING: Processing FAILED at {valkey_memory} with "
                    f"{evictions} evictions ({eviction_rate:.1f}/s). "
                    f"This is the eviction threshold for chord failures."
                )


def _resolve_maxmemory(spec: str, namespace: str) -> int:
    """Resolve a memory spec to a maxmemory value in bytes.

    Supports:
    - K8s notation: "2Mi", "4Mi", "512Mi"
    - Relative notation: "baseline+10K" (current used_memory + 10KB)
    """
    if spec.startswith("baseline+"):
        suffix = spec.split("+", 1)[1]
        overhead = 0
        if suffix.endswith("K"):
            overhead = int(suffix[:-1]) * 1024
        elif suffix.endswith("M"):
            overhead = int(suffix[:-1]) * 1024 * 1024
        else:
            overhead = int(suffix)

        # Read current used_memory from Valkey
        pod = get_pod_by_label(namespace, "app.kubernetes.io/component=cache")
        if pod:
            result = run_oc_command(
                ["exec", "-n", namespace, pod, "--",
                 "valkey-cli", "INFO", "memory"],
                check=False,
            )
            if result.returncode == 0:
                for line in result.stdout.splitlines():
                    if line.startswith("used_memory:"):
                        baseline = int(line.split(":")[1].strip())
                        return baseline + overhead
        return 2 * 1024 * 1024 + overhead

    return _parse_memory_to_bytes(spec)


def _parse_memory_to_bytes(mem_str: str) -> int:
    """Convert K8s memory notation to bytes (e.g. '256Mi' → 268435456)."""
    mem_str = mem_str.strip()
    if mem_str.endswith("Gi"):
        return int(float(mem_str[:-2]) * 1024 * 1024 * 1024)
    elif mem_str.endswith("Mi"):
        return int(float(mem_str[:-2]) * 1024 * 1024)
    elif mem_str.endswith("Ki"):
        return int(float(mem_str[:-2]) * 1024)
    return int(mem_str)

