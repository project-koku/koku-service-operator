"""Shared helper functions and configuration for performance tests.

Consolidates utility code that was previously scattered across conftest.py
and test_ingestion.py.
"""

import json
import os
import subprocess
import tempfile
import threading
import time
from dataclasses import dataclass, field
from datetime import datetime, timezone
from pathlib import Path
from typing import Any, Dict, List, Optional

import requests as _requests

from utils import get_pod_by_label, run_oc_command

from .data_classes import (
    PerformanceResult,
    ResourceSnapshot,
    TimingMetric,
)
from .k8s_helpers import calculate_percentiles
from .queue_helpers import get_celery_queue_depths


# ---------------------------------------------------------------------------
# Centralized Performance Test Configuration
# ---------------------------------------------------------------------------

_SOAK_CONDENSED = os.environ.get("SOAK_CONDENSED", "").lower() in ("true", "1")


@dataclass(frozen=True)
class PerfTestConfig:
    """Centralized configuration for performance tests.

    All defaults can be overridden via environment variables.

    When ``SOAK_CONDENSED=true``, soak intervals are compressed so a full
    upload/query/metrics cycle completes in ~15 minutes instead of hours.
    This exercises the same code paths as a real soak run.
    """
    soak_duration_hours: float = field(
        default_factory=lambda: float(os.environ.get(
            "SOAK_DURATION_HOURS", "0.25" if _SOAK_CONDENSED else "1"
        ))
    )
    soak_upload_interval_minutes: int = field(
        default_factory=lambda: int(os.environ.get(
            "SOAK_UPLOAD_INTERVAL_MINUTES", "1" if _SOAK_CONDENSED else "15"
        ))
    )
    soak_query_interval_minutes: int = field(
        default_factory=lambda: int(os.environ.get(
            "SOAK_QUERY_INTERVAL_MINUTES", "1" if _SOAK_CONDENSED else "5"
        ))
    )
    soak_metrics_interval_seconds: int = field(
        default_factory=lambda: int(os.environ.get(
            "SOAK_METRICS_INTERVAL_SECONDS", "15" if _SOAK_CONDENSED else "60"
        ))
    )
    ing_high_freq_duration_minutes: int = field(
        default_factory=lambda: int(os.environ.get("PERF_ING_005_DURATION_MINUTES", "15"))
    )
    ing_high_freq_interval_seconds: int = field(
        default_factory=lambda: int(os.environ.get("PERF_ING_005_INTERVAL_SECONDS", "300"))
    )
    scale_max_sources: int = field(
        default_factory=lambda: int(os.environ.get("PERF_SCALE_002_MAX_SOURCES", "25"))
    )
    scale_batch_size: int = field(
        default_factory=lambda: int(os.environ.get("PERF_SCALE_002_BATCH_SIZE", "5"))
    )
    api_crud_iterations: int = field(
        default_factory=lambda: int(os.environ.get("PERF_API_003_ITERATIONS", "10"))
    )
    timeout_provider_ready: int = field(
        default_factory=lambda: int(os.environ.get("PERF_TIMEOUT_PROVIDER", "300"))
    )
    timeout_summary_tables: int = field(
        default_factory=lambda: int(os.environ.get("PERF_TIMEOUT_SUMMARY", "600"))
    )
    timeout_kruize_experiments: int = field(
        default_factory=lambda: int(os.environ.get("PERF_TIMEOUT_KRUIZE", "300"))
    )
    timeout_kruize_recommendations: int = field(
        default_factory=lambda: int(os.environ.get("PERF_TIMEOUT_RECOMMENDATIONS", "600"))
    )
    memory_growth_daily_percent_max: float = field(
        default_factory=lambda: float(os.environ.get("PERF_MEMORY_GROWTH_MAX", "5.0"))
    )
    # Should be >= the highest ING-003 parametrize value (currently 10) so that
    # test variants are not silently collapsed.
    concurrent_upload_max: int = field(
        default_factory=lambda: int(os.environ.get("PERF_CONCURRENT_UPLOADS_MAX", "10"))
    )


PERF_CONFIG = PerfTestConfig()


# ---------------------------------------------------------------------------
# Timeout Helpers
# ---------------------------------------------------------------------------

def get_profile_timeout_multiplier(profile_name: str) -> float:
    """Return timeout multiplier based on profile size."""
    multipliers = {
        "baseline": 1.0,
        "small": 1.0,
        "medium": 2.0,
        "large": 4.0,
        "xlarge": 8.0,
        "stress_p99": 12.0,
        "stress_max": 20.0,
    }
    return multipliers.get(profile_name, 1.0)


def get_timeout_for_profile(base_timeout: int, profile_name: str) -> int:
    """Calculate timeout adjusted for profile size."""
    return int(base_timeout * get_profile_timeout_multiplier(profile_name))


# ---------------------------------------------------------------------------
# Kruize Credentials
# ---------------------------------------------------------------------------

@dataclass
class KruizeCredentials:
    """Kruize database credentials for ROS tests."""

    user: str
    password: str
    database: str = "costonprem_kruize"

    @classmethod
    def from_secret(cls, namespace: str, secret_name: str) -> Optional["KruizeCredentials"]:
        """Load credentials from Kubernetes secret."""
        from utils import get_secret_value

        user = get_secret_value(namespace, secret_name, "kruize-user")
        password = get_secret_value(namespace, secret_name, "kruize-password")

        if not user or not password:
            return None

        return cls(user=user, password=password)


# ---------------------------------------------------------------------------
# Resource Parsing
# ---------------------------------------------------------------------------

def parse_cpu_millicores(value: str) -> float:
    """Parse CPU value (e.g., '500m', '2') to cores."""
    if not value:
        return 0.0
    if value.endswith("m"):
        return float(value[:-1]) / 1000
    return float(value)


def parse_memory_mib(value: str) -> float:
    """Parse memory value to MiB."""
    if not value:
        return 0.0
    value = str(value)
    if value.endswith("Ki"):
        return float(value[:-2]) / 1024
    elif value.endswith("Mi"):
        return float(value[:-2])
    elif value.endswith("Gi"):
        return float(value[:-2]) * 1024
    elif value.endswith("Ti"):
        return float(value[:-2]) * 1024 * 1024
    return float(value) / (1024 * 1024)  # Assume bytes


# ---------------------------------------------------------------------------
# Cluster Information Helpers
# ---------------------------------------------------------------------------

def get_ocp_version(namespace: str) -> str:
    """Get OpenShift cluster version."""
    result = run_oc_command([
        "get", "clusterversion", "version",
        "-o", "jsonpath={.status.desired.version}"
    ], check=False)
    return result.stdout.strip() if result.returncode == 0 else "unknown"


def get_node_info(namespace: str) -> Dict[str, Any]:
    """Get node count and resource totals."""
    result = run_oc_command(["get", "nodes", "-o", "json"], check=False)

    if result.returncode != 0:
        return {"node_count": 0, "worker_count": 0, "cpu": 0, "memory": 0}

    try:
        nodes = json.loads(result.stdout)
        total_cpu = 0
        total_memory_ki = 0
        worker_count = 0

        for node in nodes.get("items", []):
            labels = node.get("metadata", {}).get("labels", {})
            if "node-role.kubernetes.io/worker" in labels:
                worker_count += 1

            capacity = node.get("status", {}).get("capacity", {})
            cpu = capacity.get("cpu", "0")
            memory = capacity.get("memory", "0Ki")

            if cpu.endswith("m"):
                total_cpu += int(cpu[:-1]) / 1000
            else:
                total_cpu += int(cpu)

            if memory.endswith("Ki"):
                total_memory_ki += int(memory[:-2])
            elif memory.endswith("Mi"):
                total_memory_ki += int(memory[:-2]) * 1024
            elif memory.endswith("Gi"):
                total_memory_ki += int(memory[:-2]) * 1024 * 1024

        return {
            "node_count": len(nodes.get("items", [])),
            "worker_count": worker_count,
            "cpu": int(total_cpu),
            "memory_gib": total_memory_ki / (1024 * 1024),
        }
    except (json.JSONDecodeError, ValueError):
        return {"node_count": 0, "worker_count": 0, "cpu": 0, "memory_gib": 0}


def get_storage_info(namespace: str) -> Dict[str, str]:
    """Detect storage type (ODF, S4, hostpath, etc.)."""
    result = run_oc_command([
        "get", "storagecluster", "-n", "openshift-storage",
        "-o", "jsonpath={.items[0].metadata.name}"
    ], check=False)

    if result.returncode == 0 and result.stdout.strip():
        return {"storage_type": "ODF", "storage_class": "ocs-storagecluster-ceph-rbd"}

    for sc_name, sc_type in [("s4-storage", "S4"), ("hpp-backend", "HPP"), ("longhorn", "Longhorn")]:
        result = run_oc_command(["get", "storageclass", sc_name, "-o", "name"], check=False)
        if result.returncode == 0:
            return {"storage_type": sc_type, "storage_class": sc_name}

    return {"storage_type": "unknown", "storage_class": "unknown"}


def get_s3_backend(namespace: str) -> str:
    """Detect S3 backend type (NooBaa, S4, MinIO, etc.)."""
    result = run_oc_command([
        "get", "noobaa", "-n", "openshift-storage", "-o", "name"
    ], check=False)
    if result.returncode == 0 and result.stdout.strip():
        return "NooBaa"

    result = run_oc_command([
        "get", "storagesystem", "-n", "openshift-storage",
        "-o", "jsonpath={.items[*].metadata.name}"
    ], check=False)
    if result.returncode == 0 and "s4" in result.stdout.lower():
        return "S4"

    result = run_oc_command([
        "get", "deployment", "-l", "app=minio", "-A",
        "-o", "jsonpath={.items[0].metadata.name}"
    ], check=False)
    if result.returncode == 0 and result.stdout.strip():
        return "MinIO"

    result = run_oc_command([
        "get", "configmap", "-n", namespace, "-l", "app.kubernetes.io/component=aws-config",
        "-o", "jsonpath={.items[0].data.config}"
    ], check=False)
    if result.returncode == 0:
        config = result.stdout.lower()
        if "s4" in config:
            return "S4"
        if "noobaa" in config:
            return "NooBaa"
        if "minio" in config:
            return "MinIO"

    return "unknown"


def get_operator_version(namespace: str, cmsc_name: str) -> str:
    """Get the deployed operator version from the controller Deployment image tag."""
    result = run_oc_command([
        "get", "deploy", "-n", namespace,
        "-l", "control-plane=controller-manager",
        "-o", "jsonpath={.items[0].spec.template.spec.containers[0].image}",
    ], check=False)
    if result.returncode == 0 and result.stdout.strip():
        image = result.stdout.strip()
        return image.split(":")[-1] if ":" in image else image
    return "unknown"


def get_chart_version(namespace: str, release_name: str) -> str:
    """Alias for get_operator_version — kept for backward compatibility with test files."""
    return get_operator_version(namespace, release_name)


def get_pod_resource_usage(namespace: str, label_selector: str) -> List[ResourceSnapshot]:
    """Get current resource usage for pods matching selector."""
    snapshots: List[ResourceSnapshot] = []
    timestamp = datetime.now(timezone.utc).isoformat()

    result = run_oc_command([
        "get", "pods", "-n", namespace, "-l", label_selector, "-o", "json"
    ], check=False)

    if result.returncode != 0:
        return snapshots

    try:
        pods = json.loads(result.stdout)
        for pod in pods.get("items", []):
            pod_name = pod.get("metadata", {}).get("name", "")
            for container in pod.get("spec", {}).get("containers", []):
                resources = container.get("resources", {})
                requests = resources.get("requests", {})
                limits = resources.get("limits", {})

                snapshots.append(ResourceSnapshot(
                    timestamp=timestamp,
                    pod_name=f"{pod_name}/{container.get('name', '')}",
                    cpu_usage_cores=0,
                    cpu_request_cores=parse_cpu_millicores(requests.get("cpu", "0")),
                    cpu_limit_cores=parse_cpu_millicores(limits.get("cpu", "0")),
                    memory_usage_mib=0,
                    memory_request_mib=parse_memory_mib(requests.get("memory", "0")),
                    memory_limit_mib=parse_memory_mib(limits.get("memory", "0")),
                ))
    except json.JSONDecodeError:
        pass

    return snapshots


def build_koku_api_url(helm_release: str, namespace: str) -> str:
    """Build the internal Koku API URL for in-cluster requests."""
    return (
        f"http://{helm_release}-koku-api"
        f".{namespace}.svc.cluster.local:8000"
        f"/api/cost-management/v1"
    )


# ---------------------------------------------------------------------------
# Performance Report Persistence
# ---------------------------------------------------------------------------

def save_perf_result(result: PerformanceResult, output_dir: Path) -> Path:
    """Save a performance result to JSON."""
    filename = f"{result.test_name}_{result.profile}_{datetime.now(timezone.utc).strftime('%Y%m%d_%H%M%S')}.json"
    output_path = output_dir / filename

    with open(output_path, "w") as f:
        json.dump(result.to_dict(), f, indent=2)

    return output_path


class PerfResultCollector:
    """Collects and aggregates performance results for a test session."""

    def __init__(self, output_dir: Path):
        self.output_dir = output_dir
        self.results: List[PerformanceResult] = []
        self.session_id = datetime.now(timezone.utc).strftime("%Y%m%d_%H%M%S")

    def add_result(self, result: PerformanceResult):
        """Add a test result to the collection."""
        self.results.append(result)
        save_perf_result(result, self.output_dir)

    def finalize(self):
        """Save aggregated session report."""
        if not self.results:
            return

        session_report = {
            "session_id": self.session_id,
            "timestamp": datetime.now(timezone.utc).isoformat(),
            "total_tests": len(self.results),
            "passed": sum(1 for r in self.results if r.passed),
            "failed": sum(1 for r in self.results if not r.passed),
            "results": [r.to_dict() for r in self.results],
        }

        report_path = self.output_dir / f"session_{self.session_id}.json"
        with open(report_path, "w") as f:
            json.dump(session_report, f, indent=2)


# ---------------------------------------------------------------------------
# PerfTimer — Thread-Safe Timing Utility
# ---------------------------------------------------------------------------

class PerfTimer:
    """Thread-safe timer utility for performance measurements.

    All dict mutations are guarded by a lock so concurrent ThreadPoolExecutor
    workers can safely call start/stop/measure without corrupting state.
    """

    def __init__(self):
        self._timings: Dict[str, TimingMetric] = {}
        self._active: Dict[str, float] = {}
        self._lock = threading.Lock()

    def start(self, name: str, metadata: Optional[Dict[str, Any]] = None):
        """Start timing an operation."""
        with self._lock:
            self._active[name] = time.time()
            self._timings[name] = TimingMetric(
                name=name,
                duration_seconds=0,
                start_time=datetime.now(timezone.utc).isoformat(),
                end_time="",
                metadata=metadata or {},
            )

    def stop(self, name: str) -> float:
        """Stop timing and return duration."""
        with self._lock:
            if name not in self._active:
                return 0.0

            end_time = time.time()
            duration = end_time - self._active[name]

            self._timings[name].duration_seconds = duration
            self._timings[name].end_time = datetime.now(timezone.utc).isoformat()

            del self._active[name]
            return duration

    def measure(self, name: str, metadata: Optional[Dict[str, Any]] = None):
        """Context manager for timing an operation."""
        return _TimerContext(self, name, metadata)

    def get_timings(self) -> List[TimingMetric]:
        """Get all completed timings."""
        with self._lock:
            return list(self._timings.values())

    def get_timing(self, name: str) -> Optional[TimingMetric]:
        """Get a specific timing."""
        with self._lock:
            return self._timings.get(name)


class _TimerContext:
    """Context manager for PerfTimer.measure()."""

    def __init__(self, timer: PerfTimer, name: str, metadata: Optional[Dict[str, Any]]):
        self.timer = timer
        self.name = name
        self.metadata = metadata

    def __enter__(self):
        self.timer.start(self.name, self.metadata)
        return self

    def __exit__(self, exc_type, exc_val, exc_tb):
        self.timer.stop(self.name)
        return False


# ---------------------------------------------------------------------------
# Shared Authentication Helper
# ---------------------------------------------------------------------------

def create_authenticated_session(keycloak_config) -> "requests.Session":
    """Create a requests.Session with a fresh JWT token.

    Delegates to the root conftest version with JSON content type.
    """
    from conftest import create_authenticated_session as _root_create

    return _root_create(keycloak_config, content_type="application/json")


# ---------------------------------------------------------------------------
# Data Generation and Upload
# ---------------------------------------------------------------------------

def generate_and_upload_data(
    cluster_id: str,
    source_name: str,
    start_date: datetime,
    end_date: datetime,
    ingress_url: str,
    jwt_token,
    config=None,
    profile_name: Optional[str] = None,
) -> Dict[str, Any]:
    """Generate NISE data and upload it to ingress.

    Returns timing and metadata about the upload.
    """
    import requests

    from conftest import JWTToken
    from e2e_helpers import NISEConfig, generate_nise_data
    from utils import create_upload_package_from_files

    from .profiles import PROFILES, get_profile_nise_yaml

    with tempfile.TemporaryDirectory() as temp_dir:
        gen_start = time.time()

        if profile_name and profile_name in PROFILES:
            yaml_content = get_profile_nise_yaml(
                profile_name, start_date, end_date, cluster_id, 0
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
                capture_output=True, text=True, timeout=600, cwd=nise_output,
            )

            if result.returncode != 0:
                raise RuntimeError(f"NISE failed: {result.stderr}")

            csv_files = list(Path(nise_output).rglob("*.csv"))
            pod_usage_files = [str(f) for f in csv_files if "pod_usage" in f.name.lower()]
            ros_usage_files = [str(f) for f in csv_files if "ros_usage" in f.name.lower() or "resource_" in f.name.lower()]
            node_label_files = [str(f) for f in csv_files if "node_label" in f.name.lower()]
            namespace_label_files = [str(f) for f in csv_files if "namespace_label" in f.name.lower()]
        else:
            files = generate_nise_data(
                cluster_id, start_date, end_date, temp_dir, config
            )
            pod_usage_files = files.get("pod_usage_files", [])
            ros_usage_files = files.get("ros_usage_files", [])
            node_label_files = files.get("node_label_files", [])
            namespace_label_files = files.get("namespace_label_files", [])

        gen_duration = time.time() - gen_start
        total_files = len(pod_usage_files) + len(ros_usage_files)

        package_start = time.time()
        package_path = create_upload_package_from_files(
            pod_usage_files=pod_usage_files,
            ros_usage_files=ros_usage_files,
            cluster_id=cluster_id,
            start_date=start_date,
            end_date=end_date,
            node_label_files=node_label_files if node_label_files else None,
            namespace_label_files=namespace_label_files if namespace_label_files else None,
        )

        package_size_mb = os.path.getsize(package_path) / (1024 * 1024)
        package_duration = time.time() - package_start

        upload_timeout = max(180, int(package_size_mb / 0.5) + 60)
        upload_start = time.time()
        session = requests.Session()
        session.verify = False

        from e2e_helpers import upload_with_retry

        response = upload_with_retry(
            session,
            f"{ingress_url}/v1/upload",
            package_path,
            jwt_token.authorization_header,
            timeout=upload_timeout,
        )

        upload_duration = time.time() - upload_start

        return {
            "cluster_id": cluster_id,
            "source_name": source_name,
            "csv_file_count": total_files,
            "package_size_mb": round(package_size_mb, 3),
            "generation_seconds": round(gen_duration, 3),
            "packaging_seconds": round(package_duration, 3),
            "upload_seconds": round(upload_duration, 3),
            "upload_status": response.status_code,
            "upload_mb_per_second": round(package_size_mb / upload_duration, 3) if upload_duration > 0 else 0,
            "upload_timeout_seconds": upload_timeout,
        }


# ---------------------------------------------------------------------------
# Source Registration + Tracking
# ---------------------------------------------------------------------------

def register_tracked_source(
    namespace: str,
    ingress_pod: str,
    koku_api_url: str,
    rh_identity_header: str,
    perf_cleanup,
    prefix: str = "perf",
):
    """Register a new source and track it for cleanup.

    Returns (source, cluster_id, source_name).
    """
    from e2e_helpers import generate_cluster_id, register_source

    cluster_id = generate_cluster_id()
    source_name = f"{prefix}-{cluster_id[-8:]}"
    source = register_source(
        namespace,
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
    return source, cluster_id, source_name


# ---------------------------------------------------------------------------
# Pod health helpers (OOMKill detection, restart counts)
# ---------------------------------------------------------------------------


def get_oomkill_events(namespace: str, label: str) -> List[Dict[str, str]]:
    """Check for OOMKilled containers in pods matching *label*."""
    result = run_oc_command(
        ["get", "pods", "-n", namespace,
         "-l", label,
         "-o", "jsonpath={range .items[*]}{.metadata.name}|"
               "{range .status.containerStatuses[*]}"
               "{.lastState.terminated.reason}{end}{'\\n'}{end}"],
        check=False,
    )
    events = []
    if result.returncode == 0:
        for line in result.stdout.strip().splitlines():
            if "OOMKilled" in line:
                pod_name = line.split("|", 1)[0]
                events.append({
                    "pod": pod_name,
                    "reason": "OOMKilled",
                })
    return events


def get_pod_restart_counts(namespace: str, label: str) -> Dict[str, int]:
    """Get total restart counts for pods matching a label.

    Sums restart counts across all containers in each pod so multi-container
    pods (e.g. database with pgbouncer sidecar) are counted correctly.
    """
    result = run_oc_command(
        ["get", "pods", "-n", namespace, "-l", label,
         "-o", "jsonpath={range .items[*]}{.metadata.name} "
               "{range .status.containerStatuses[*]}{.restartCount}{\" \"}{end}{'\\n'}{end}"],
        check=False,
    )
    counts = {}
    if result.returncode == 0:
        for line in result.stdout.strip().splitlines():
            parts = line.split()
            if len(parts) >= 2:
                pod_name = parts[0]
                try:
                    counts[pod_name] = sum(int(c) for c in parts[1:])
                except ValueError:
                    pass
    return counts


# ---------------------------------------------------------------------------
# API Probe Thread (for concurrent load testing)
# ---------------------------------------------------------------------------


@dataclass
class APIProbeSnapshot:
    """Single probe sample: latencies for each endpoint at a point in time."""

    timestamp: float = 0
    report_baseline_s: float = 0
    group_by_s: float = 0
    cost_models_s: float = 0
    errors: int = 0
    queue_depth: int = 0


class APIProbeThread:
    """Background thread that continuously probes API endpoints.

    Fires GET requests against a fixed set of Cost Management API endpoints
    at a configurable interval and records per-sample latencies. Useful for
    measuring API degradation while ingestion or other heavy workloads are
    running concurrently.

    Modeled on ValkeyMonitor from test_valkey_eviction.py — uses
    threading.Event for clean shutdown and daemon=True for safety.

    Usage::

        session = create_authenticated_session(keycloak_config)
        probe = APIProbeThread(session, gateway_url, namespace,
                               keycloak_config=keycloak_config)
        probe.start()
        # ... do heavy work ...
        summary = probe.stop()   # returns percentile stats per endpoint
    """

    def __init__(
        self,
        session: _requests.Session,
        gateway_url: str,
        namespace: str,
        poll_interval: float = 2.0,
        keycloak_config=None,
    ):
        self.session = session
        self.namespace = namespace
        self.poll_interval = poll_interval
        self.snapshots: List[APIProbeSnapshot] = []
        self._stop = threading.Event()
        self._thread: Optional[threading.Thread] = None
        self._keycloak_config = keycloak_config
        self._token_refreshes = 0

        base = gateway_url.rstrip("/")
        self._urls = {
            "report_baseline": f"{base}/api/cost-management/v1/reports/openshift/costs/",
            "group_by": (
                f"{base}/api/cost-management/v1/reports/openshift/costs/"
                f"?group_by[project]=*&filter[time_scope_value]=-30"
            ),
            "cost_models": f"{base}/api/cost-management/v1/cost-models/",
        }

    def _refresh_token(self) -> bool:
        """Refresh the session's JWT token. Returns True on success."""
        if not self._keycloak_config:
            return False
        try:
            from conftest import obtain_jwt_token
            token = obtain_jwt_token(self._keycloak_config)
            self.session.headers["Authorization"] = f"Bearer {token.access_token}"
            self._token_refreshes += 1
            print(f"[api-probe] Refreshed JWT token (refresh #{self._token_refreshes})")
            return True
        except Exception as e:
            print(f"[api-probe] Token refresh failed: {e}")
            return False

    def _probe_once(self) -> APIProbeSnapshot:
        snap = APIProbeSnapshot(timestamp=time.time())
        errors = 0
        got_401 = False

        for attr, url in [
            ("report_baseline_s", self._urls["report_baseline"]),
            ("group_by_s", self._urls["group_by"]),
            ("cost_models_s", self._urls["cost_models"]),
        ]:
            start = time.time()
            try:
                resp = self.session.get(url, timeout=30)
                latency = time.time() - start
                setattr(snap, attr, latency)
                if resp.status_code != 200:
                    errors += 1
                    if resp.status_code == 401:
                        got_401 = True
            except _requests.RequestException:
                setattr(snap, attr, time.time() - start)
                errors += 1

        snap.errors = errors

        if got_401 and self._refresh_token():
            snap.errors = 0

        depths = get_celery_queue_depths(self.namespace)
        snap.queue_depth = sum(depths.values())

        return snap

    def _poll_loop(self):
        while not self._stop.is_set():
            try:
                snap = self._probe_once()
                self.snapshots.append(snap)

                err_str = ""
                if snap.errors:
                    err_str = f" errors={snap.errors}"
                print(
                    f"[api-probe] report={snap.report_baseline_s:.3f}s "
                    f"group_by={snap.group_by_s:.3f}s "
                    f"cost_models={snap.cost_models_s:.3f}s "
                    f"queue={snap.queue_depth}{err_str}"
                )
            except Exception as e:
                print(f"[api-probe] poll error: {e}")

            self._stop.wait(self.poll_interval)

    def start(self):
        self._thread = threading.Thread(target=self._poll_loop, daemon=True)
        self._thread.start()
        print(f"[api-probe] Started (polling every {self.poll_interval}s)")

    def stop(self) -> Dict[str, Any]:
        self._stop.set()
        if self._thread:
            self._thread.join(timeout=10)

        if not self.snapshots:
            return {"error": "no data collected"}

        report_latencies = [s.report_baseline_s for s in self.snapshots]
        group_by_latencies = [s.group_by_s for s in self.snapshots]
        cost_model_latencies = [s.cost_models_s for s in self.snapshots]
        total_errors = sum(s.errors for s in self.snapshots)
        peak_queue = max(s.queue_depth for s in self.snapshots)

        duration = self.snapshots[-1].timestamp - self.snapshots[0].timestamp

        summary = {
            "duration_seconds": round(duration, 1),
            "probe_count": len(self.snapshots),
            "total_errors": total_errors,
            "peak_queue_depth": peak_queue,
            "report_baseline": calculate_percentiles(report_latencies),
            "group_by_project": calculate_percentiles(group_by_latencies),
            "cost_models_list": calculate_percentiles(cost_model_latencies),
        }

        print(
            f"[api-probe] Stopped. {len(self.snapshots)} samples over "
            f"{duration:.0f}s. Peak queue depth: {peak_queue}. "
            f"Errors: {total_errors}"
        )
        return summary
