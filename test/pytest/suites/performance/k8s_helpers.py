"""
Kubernetes resource patching, PostgreSQL stats, and statistics helpers
for performance tests.

Shared by test_valkey_eviction.py, test_db_resource_sweep.py,
test_celery_scaling.py, and test_api_latency.py.
"""

import json
import statistics
import subprocess
import time
from dataclasses import dataclass
from typing import Any, Dict, List, Optional

from utils import execute_db_query, run_oc_command


# =============================================================================
# Percentile Calculation
# =============================================================================


def calculate_percentiles(
    latencies: List[float], errors: int = 0
) -> Dict[str, Any]:
    """Calculate p50/p95/p99 latency statistics.

    Works with any list of numeric measurements (seconds, milliseconds, etc.).
    """
    if not latencies:
        return {"p50": 0, "p95": 0, "p99": 0, "min": 0, "max": 0, "avg": 0, "count": 0, "errors": errors}

    n = len(latencies)
    if n < 2:
        v = round(latencies[0], 4)
        return {"p50": v, "p95": v, "p99": v, "min": v, "max": v, "avg": v, "count": n, "errors": errors}

    q = statistics.quantiles(latencies, n=100, method="inclusive")
    return {
        "p50": round(q[49], 4),
        "p95": round(q[94], 4),
        "p99": round(q[98], 4),
        "min": round(min(latencies), 4),
        "max": round(max(latencies), 4),
        "avg": round(statistics.mean(latencies), 4),
        "count": n,
        "errors": errors,
    }


# =============================================================================
# K8s Resource Patching
# =============================================================================


def get_resource_spec(
    namespace: str, kind: str, name: str
) -> dict:
    """Read the current container resource spec from a Deployment or StatefulSet."""
    result = run_oc_command(
        ["get", kind, name, "-n", namespace,
         "-o", "jsonpath={.spec.template.spec.containers[0].resources}"],
        check=False,
    )
    if result.returncode != 0:
        return {}
    try:
        return json.loads(result.stdout.strip())
    except (json.JSONDecodeError, ValueError):
        return {}


def patch_resource_spec(
    namespace: str, kind: str, name: str, resources: dict,
    rollout_timeout: int = 180,
) -> bool:
    """Patch a Deployment or StatefulSet's container resources and wait for rollout.

    For StatefulSets, also deletes the pod to force recreation since
    StatefulSet updates don't automatically roll pods on resource changes.
    """
    result = run_oc_command(
        ["patch", kind, name, "-n", namespace,
         "--type=json",
         "-p", json.dumps([{
             "op": "replace",
             "path": "/spec/template/spec/containers/0/resources",
             "value": resources,
         }])],
        check=False,
    )
    if result.returncode != 0:
        print(f"[k8s-patch] Failed to patch {kind}/{name}: {result.stderr.strip()[:200]}")
        return False

    print(f"[k8s-patch] Patched {kind}/{name}")

    # StatefulSets need a pod delete to pick up resource changes
    if kind.lower() in ("statefulset", "sts"):
        _delete_first_pod(namespace, name)

    try:
        rollout = run_oc_command(
            ["rollout", "status", f"{kind}/{name}",
             "-n", namespace, f"--timeout={rollout_timeout}s"],
            check=False,
            timeout=rollout_timeout + 30,
        )
    except subprocess.TimeoutExpired:
        print(f"[k8s-patch] Rollout timed out for {kind}/{name}")
        return False

    if rollout.returncode != 0:
        print(f"[k8s-patch] Rollout failed: {rollout.stderr.strip()[:200]}")
        return False

    print(f"[k8s-patch] {kind}/{name} ready")
    return True


def restore_resource_spec(
    namespace: str, kind: str, name: str, resources: dict,
    default: Optional[dict] = None,
) -> bool:
    """Restore a Deployment or StatefulSet to previously-captured resources."""
    if not resources:
        resources = default or {
            "requests": {"cpu": "100m", "memory": "256Mi"},
            "limits": {"cpu": "500m", "memory": "512Mi"},
        }
    success = patch_resource_spec(namespace, kind, name, resources)
    label = f"{kind}/{name}"
    if success:
        print(f"[k8s-restore] {label} resources restored")
    else:
        print(f"[k8s-restore] WARNING: failed to restore {label}")
    return success


def merge_resources(original: dict, overrides: dict) -> dict:
    """Merge override resources into original, preserving unspecified fields.

    Example: merge_resources(
        {"requests": {"cpu": "100m", "memory": "256Mi"}, "limits": {"cpu": "500m", "memory": "512Mi"}},
        {"limits": {"cpu": "4000m"}}
    ) -> {"requests": {"cpu": "100m", "memory": "256Mi"}, "limits": {"cpu": "4000m", "memory": "512Mi"}}
    """
    merged = {
        "requests": dict(original.get("requests", {})),
        "limits": dict(original.get("limits", {})),
    }
    for section in ("requests", "limits"):
        if section in overrides:
            merged[section].update(overrides[section])
    return merged


def _delete_first_pod(namespace: str, owner_name: str) -> None:
    """Delete the first pod owned by a StatefulSet to force recreation.

    Uses the StatefulSet naming convention (<owner_name>-0) to identify the pod.
    """
    pod_name = f"{owner_name}-0"
    print(f"[k8s-patch] Deleting pod {pod_name} to apply new resources...")
    run_oc_command(
        ["delete", "pod", pod_name, "-n", namespace, "--wait=false"],
        check=False,
    )


# =============================================================================
# Deployment Scaling
# =============================================================================


def scale_deployment(namespace: str, name: str, replicas: int) -> bool:
    """Scale a Deployment and wait for rollout."""
    run_oc_command(
        ["scale", "deployment", name, "-n", namespace,
         f"--replicas={replicas}"],
        check=False,
    )
    result = run_oc_command(
        ["rollout", "status", "deployment", name,
         "-n", namespace, "--timeout=180s"],
        check=False,
        timeout=210,
    )
    ok = result.returncode == 0
    status = "ready" if ok else "FAILED"
    print(f"[scale] {name} → {replicas} replicas: {status}")
    return ok


def get_deployment_replicas(namespace: str, name: str) -> int:
    result = run_oc_command(
        ["get", "deployment", name, "-n", namespace,
         "-o", "jsonpath={.spec.replicas}"],
        check=False,
    )
    try:
        return int(result.stdout.strip())
    except (ValueError, AttributeError):
        return 1


# =============================================================================
# PostgreSQL Stats Collection
# =============================================================================


def _safe_int(s: str) -> int:
    try:
        return int(s.strip())
    except (ValueError, AttributeError):
        return 0


@dataclass
class PgStatSnapshot:
    """Point-in-time snapshot of PostgreSQL performance counters."""

    timestamp: float = 0
    # pg_stat_bgwriter
    buffers_checkpoint: int = 0
    buffers_clean: int = 0
    buffers_backend: int = 0
    # pg_stat_database (for the koku database)
    blks_hit: int = 0
    blks_read: int = 0
    xact_commit: int = 0
    xact_rollback: int = 0
    tup_returned: int = 0
    tup_fetched: int = 0
    deadlocks: int = 0
    # derived
    cache_hit_ratio: float = 0.0


def capture_pg_stats(
    namespace: str, db_pod: str, db_name: str, db_user: str
) -> PgStatSnapshot:
    """Capture a snapshot of PostgreSQL performance statistics."""
    snap = PgStatSnapshot(timestamp=time.time())

    bgwriter_query = (
        "SELECT buffers_checkpoint, buffers_clean, buffers_backend "
        "FROM pg_stat_bgwriter"
    )
    rows = execute_db_query(namespace, db_pod, db_name, db_user, bgwriter_query)
    if rows and rows[0]:
        row = rows[0]
        if len(row) >= 3:
            snap.buffers_checkpoint = _safe_int(row[0])
            snap.buffers_clean = _safe_int(row[1])
            snap.buffers_backend = _safe_int(row[2])

    db_query = (
        f"SELECT blks_hit, blks_read, xact_commit, xact_rollback, "
        f"tup_returned, tup_fetched, deadlocks "
        f"FROM pg_stat_database WHERE datname = '{db_name}'"
    )
    rows = execute_db_query(namespace, db_pod, db_name, db_user, db_query)
    if rows and rows[0]:
        row = rows[0]
        if len(row) >= 7:
            snap.blks_hit = _safe_int(row[0])
            snap.blks_read = _safe_int(row[1])
            snap.xact_commit = _safe_int(row[2])
            snap.xact_rollback = _safe_int(row[3])
            snap.tup_returned = _safe_int(row[4])
            snap.tup_fetched = _safe_int(row[5])
            snap.deadlocks = _safe_int(row[6])

    total_blocks = snap.blks_hit + snap.blks_read
    if total_blocks > 0:
        snap.cache_hit_ratio = round(snap.blks_hit / total_blocks, 4)

    return snap


def diff_pg_stats(before: PgStatSnapshot, after: PgStatSnapshot) -> Dict[str, Any]:
    """Compute the delta between two pg_stat snapshots."""
    delta_hit = after.blks_hit - before.blks_hit
    delta_read = after.blks_read - before.blks_read
    total = delta_hit + delta_read

    return {
        "duration_s": round(after.timestamp - before.timestamp, 1),
        "blks_hit_delta": delta_hit,
        "blks_read_delta": delta_read,
        "cache_hit_ratio": round(delta_hit / total, 4) if total > 0 else 1.0,
        "xact_commit_delta": after.xact_commit - before.xact_commit,
        "xact_rollback_delta": after.xact_rollback - before.xact_rollback,
        "tup_returned_delta": after.tup_returned - before.tup_returned,
        "tup_fetched_delta": after.tup_fetched - before.tup_fetched,
        "deadlocks_delta": after.deadlocks - before.deadlocks,
        "buffers_backend_delta": after.buffers_backend - before.buffers_backend,
    }


def get_db_cpu_utilization(namespace: str, db_pod: str) -> Optional[float]:
    """Read current CPU usage of the database pod in millicores."""
    result = run_oc_command(
        ["adm", "top", "pod", db_pod, "-n", namespace, "--no-headers"],
        check=False,
    )
    if result.returncode != 0:
        return None
    parts = result.stdout.split()
    if len(parts) >= 2:
        cpu_str = parts[1]
        if cpu_str.endswith("m"):
            return int(cpu_str[:-1])
        try:
            return int(cpu_str) * 1000
        except ValueError:
            pass
    return None


def get_db_shared_buffers(
    namespace: str, db_pod: str, db_name: str, db_user: str
) -> str:
    """Read the current shared_buffers setting."""
    rows = execute_db_query(
        namespace, db_pod, db_name, db_user, "SHOW shared_buffers"
    )
    if rows and rows[0]:
        return str(rows[0][0]).strip() if isinstance(rows[0], tuple) else str(rows[0]).strip()
    return "unknown"
