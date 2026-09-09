"""
Celery Worker Scaling and OOM Threshold Tests (COST-7598).

Tests per-queue worker replica scaling to find diminishing-returns points,
worker memory OOM thresholds, and multi-replica Kruize behavior.

Test IDs:
- PERF-CEL-001: Per-queue replica sweep (OCP / summary / listener)
- PERF-CEL-002: Worker OOM threshold (memory constraint)
- PERF-ROS-001: Multi-replica Kruize validation
"""

import time
from concurrent.futures import ThreadPoolExecutor, as_completed
from dataclasses import dataclass, field
from datetime import datetime, timedelta, timezone
from typing import Any, Dict, List, Optional

import pytest

from conftest import ClusterConfig, DatabaseConfig, JWTToken, obtain_jwt_token
from e2e_helpers import (
    ensure_nise_available,
    generate_cluster_id,
    register_source,
    wait_for_processing_complete,
)
from utils import execute_db_query, run_oc_command

from .data_classes import PerformanceResult
from .helpers import (
    PerfResultCollector,
    PerfTimer,
    generate_and_upload_data,
    get_oomkill_events,
    get_pod_restart_counts,
)
from .k8s_helpers import (
    capture_pg_stats,
    diff_pg_stats,
    get_db_cpu_utilization,
    get_deployment_replicas,
    get_resource_spec,
    merge_resources,
    patch_resource_spec,
    restore_resource_spec,
    scale_deployment,
)
from .profiles import ACTIVE_PROFILE as _ACTIVE_PROFILE
from .queue_helpers import get_celery_queue_depths, wait_for_queue_drain
from .tracker import PerfCleanupTracker


def get_worker_cpu_utilization(
    namespace: str, label: str
) -> Dict[str, str]:
    """Read CPU usage for all pods matching a label."""
    result = run_oc_command(
        ["adm", "top", "pod", "-n", namespace,
         "-l", label, "--no-headers"],
        check=False, timeout=30,
    )
    if result.returncode != 0:
        return {}
    pods = {}
    for line in result.stdout.splitlines():
        parts = line.split()
        if len(parts) >= 2:
            pods[parts[0]] = parts[1]
    return pods


def _wait_for_pod_metrics(
    namespace: str, label: str, expected_count: int,
    timeout: int = 90, poll_interval: int = 10,
) -> Dict[str, str]:
    """Poll ``oc adm top pod`` until *expected_count* pods report metrics.

    Metrics-server can lag behind pod readiness by 30-60s, so a simple
    sleep is unreliable.  Returns whatever was last observed if the
    timeout expires.
    """
    deadline = time.time() + timeout
    result: Dict[str, str] = {}
    while time.time() < deadline:
        result = get_worker_cpu_utilization(namespace, label)
        if len(result) >= expected_count:
            return result
        remaining = int(deadline - time.time())
        print(f"  [metrics] {len(result)}/{expected_count} pods reporting, "
              f"retrying ({remaining}s left)")
        time.sleep(poll_interval)
    print(f"  [metrics] timeout — got {len(result)}/{expected_count} pods")
    return result




# ---------------------------------------------------------------------------
# Ingestion batch runner (extracted from KAF-002 pattern)
# ---------------------------------------------------------------------------

@dataclass
class IngestionBatchResult:
    """Results from a concurrent upload + processing batch."""
    label: str = ""
    concurrent_sources: int = 0
    uploads_ok: int = 0
    uploads_failed: int = 0
    processed: int = 0
    total_mb: float = 0.0
    upload_elapsed_s: float = 0.0
    processing_elapsed_s: float = 0.0
    queue_drain_elapsed_s: float = 0.0
    errors: List[Dict[str, Any]] = field(default_factory=list)

    @property
    def all_processed(self) -> bool:
        return self.processed == self.concurrent_sources and self.uploads_failed == 0


def run_ingestion_batch(
    *,
    namespace: str,
    concurrent_sources: int,
    ingress_url: str,
    ingress_pod: str,
    koku_api_url: str,
    rh_identity_header: str,
    database_config: DatabaseConfig,
    perf_cleanup: PerfCleanupTracker,
    keycloak_config,
    label: str = "batch",
    profile_name: str = "baseline",
    data_days: int = 7,
) -> IngestionBatchResult:
    """Register sources, upload concurrently, wait for processing, drain queues.

    Reusable building block for any test that needs a controlled ingestion
    workload without Kafka-specific monitoring.
    """
    result = IngestionBatchResult(
        label=label,
        concurrent_sources=concurrent_sources,
    )

    sources: List[Dict[str, Any]] = []
    for i in range(concurrent_sources):
        cluster_id = generate_cluster_id()
        source_name = f"perf-{label}-{i:02d}-{cluster_id[-8:]}"
        source = register_source(
            namespace, ingress_pod,
            koku_api_url, rh_identity_header,
            cluster_id, "org1234567", source_name,
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

    end_date = datetime.now(timezone.utc)
    start_date = end_date - timedelta(days=data_days)

    upload_results = []
    errors = []
    upload_start = time.time()

    def _upload(info: Dict[str, Any]) -> Dict[str, Any]:
        try:
            token = obtain_jwt_token(keycloak_config)
            return generate_and_upload_data(
                info["cluster_id"], info["source_name"],
                start_date, end_date, ingress_url, token,
                profile_name=profile_name,
            )
        except Exception as e:
            return {"error": str(e), "cluster_id": info["cluster_id"]}

    with ThreadPoolExecutor(max_workers=concurrent_sources) as pool:
        futures = {pool.submit(_upload, s): s for s in sources}
        for future in as_completed(futures):
            r = future.result()
            if "error" in r:
                errors.append(r)
            else:
                upload_results.append(r)

    result.upload_elapsed_s = round(time.time() - upload_start, 1)
    result.uploads_ok = len(upload_results)
    result.uploads_failed = len(errors)
    result.errors = errors
    result.total_mb = round(
        sum(r.get("package_size_mb", 0) for r in upload_results), 2
    )

    # Wait for processing
    total_budget = 120 + concurrent_sources * 30
    if _ACTIVE_PROFILE in ("medium", "large", "xlarge"):
        total_budget = int(total_budget * 1.5)
    deadline = time.time() + total_budget
    proc_start = time.time()
    for s in sources:
        remaining = max(15, int(deadline - time.time()))
        proc = wait_for_processing_complete(
            namespace, database_config.pod_name,
            s["cluster_id"], max_wait_seconds=remaining,
        )
        if proc["complete"]:
            result.processed += 1
    result.processing_elapsed_s = round(time.time() - proc_start, 1)

    # Drain queues
    drain = wait_for_queue_drain(
        namespace, max_wait_seconds=600, label=label,
    )
    result.queue_drain_elapsed_s = drain.get("elapsed_s", 0)

    return result


# ---------------------------------------------------------------------------
# Profile-gated parametrize lists
# ---------------------------------------------------------------------------

_CEL_001_EXPERIMENTS = {
    "baseline": [
        pytest.param("ocp", [1, 2], id="ocp-1-2"),
    ],
    "medium": [
        pytest.param("ocp", [1, 2, 4], id="ocp-1-2-4"),
        pytest.param("summary", [1, 2, 4], id="summary-1-2-4"),
        pytest.param("listener", [1, 2, 3], id="listener-1-2-3"),
    ],
    "large": [
        pytest.param("ocp", [1, 2, 4], id="ocp-1-2-4"),
        pytest.param("summary", [1, 2, 4], id="summary-1-2-4"),
        pytest.param("listener", [1, 2, 3], id="listener-1-2-3"),
    ],
    "xlarge": [
        pytest.param("ocp", [1, 2, 4], id="ocp-1-2-4"),
        pytest.param("summary", [1, 2, 4], id="summary-1-2-4"),
        pytest.param("listener", [1, 2, 3], id="listener-1-2-3"),
    ],
}
CEL_001_EXPERIMENTS = _CEL_001_EXPERIMENTS.get(
    _ACTIVE_PROFILE, _CEL_001_EXPERIMENTS.get("medium", [])
)

_CEL_CONCURRENCY = {
    "baseline": 3,
    "medium": 5,
    "large": 8,
    "xlarge": 10,
}

WORKER_COMPONENTS = {
    "ocp": {
        "deployment_suffix": "celery-worker-ocp",
        "label": "cost-onprem.io/worker-queue=ocp",
    },
    "summary": {
        "deployment_suffix": "celery-worker-summary",
        "label": "cost-onprem.io/worker-queue=summary",
    },
    "listener": {
        "deployment_suffix": "koku-listener",
        "label": "app.kubernetes.io/component=listener",
    },
}


# ---------------------------------------------------------------------------
# CEL-1: Per-Queue Replica Sweep
# ---------------------------------------------------------------------------

@pytest.mark.performance
@pytest.mark.celery_scaling
@pytest.mark.slow
class TestCeleryReplicaSweep:
    """PERF-CEL-001: Per-queue replica sweep.

    For each worker type (OCP, summary, listener), scales replicas through
    a series of levels while holding other components constant.  Measures
    end-to-end processing time and DB CPU at each level to identify
    diminishing returns and bottleneck shifts.
    """

    @pytest.fixture(autouse=True)
    def setup(self, cluster_config: ClusterConfig, keycloak_config):
        self.namespace = cluster_config.namespace
        self.helm_release = cluster_config.helm_release_name
        self._keycloak_config = keycloak_config
        if not ensure_nise_available():
            pytest.skip("NISE (koku-nise) not available")

    @pytest.mark.timeout(3600)
    @pytest.mark.parametrize("component,replica_levels", CEL_001_EXPERIMENTS)
    def test_perf_cel_001_replica_sweep(
        self,
        component: str,
        replica_levels: List[int],
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
    ):
        """Scale one worker component through replica levels, measuring processing time."""
        info = WORKER_COMPONENTS[component]
        deploy_name = f"{self.helm_release}-{info['deployment_suffix']}"
        original_replicas = get_deployment_replicas(self.namespace, deploy_name)

        concurrent = _CEL_CONCURRENCY.get(_ACTIVE_PROFILE, 5)
        runs: List[Dict[str, Any]] = []

        try:
            for replicas in replica_levels:
                run_label = f"{component}-{replicas}r"
                print(f"\n[CEL-001] === {component} @ {replicas} replica(s) ===")

                scale_deployment(self.namespace, deploy_name, replicas)
                time.sleep(10)

                # Capture pre-run state
                pg_before = capture_pg_stats(
                    self.namespace, database_config.pod_name,
                    database_config.database, database_config.user,
                )

                with perf_timer.measure(f"run_{run_label}"):
                    batch = run_ingestion_batch(
                        namespace=self.namespace,
                        concurrent_sources=concurrent,
                        ingress_url=ingress_url,
                        ingress_pod=ingress_pod,
                        koku_api_url=koku_api_url,
                        rh_identity_header=rh_identity_header,
                        database_config=database_config,
                        perf_cleanup=perf_cleanup,
                        keycloak_config=self._keycloak_config,
                        label=f"cel-001-{run_label}",
                    )

                # Capture post-run state
                pg_after = capture_pg_stats(
                    self.namespace, database_config.pod_name,
                    database_config.database, database_config.user,
                )
                db_cpu = get_db_cpu_utilization(
                    self.namespace, database_config.pod_name,
                )
                worker_cpu = get_worker_cpu_utilization(
                    self.namespace, info["label"],
                )

                runs.append({
                    "component": component,
                    "replicas": replicas,
                    "processing_s": batch.processing_elapsed_s,
                    "queue_drain_s": batch.queue_drain_elapsed_s,
                    "uploads_ok": batch.uploads_ok,
                    "processed": batch.processed,
                    "db_cpu_millicores": db_cpu,
                    "worker_cpu": worker_cpu,
                    "pg_stat_delta": diff_pg_stats(pg_before, pg_after),
                    "all_processed": batch.all_processed,
                })
        finally:
            scale_deployment(self.namespace, deploy_name, original_replicas)

        perf_result.test_id = f"PERF-CEL-001-{component}"
        perf_result.metrics = {
            "component": component,
            "original_replicas": original_replicas,
            "concurrent_sources": concurrent,
            "runs": runs,
        }
        perf_result.timings = perf_timer.get_timings()
        perf_result.passed = all(r["all_processed"] for r in runs)
        perf_collector.add_result(perf_result)

        # Print summary
        print(f"\n{'='*70}")
        print(f"  PERF-CEL-001 | {component} replica sweep")
        print(f"{'='*70}")
        for r in runs:
            print(f"  [{r['replicas']} replica(s)]")
            print(f"    Processing:  {r['processing_s']:.1f}s")
            print(f"    Queue drain: {r['queue_drain_s']:.1f}s")
            print(f"    DB CPU:      {r['db_cpu_millicores']}m")
            print(f"    Worker CPU:  {r['worker_cpu']}")
            pg = r["pg_stat_delta"]
            print(f"    Cache hit:   {pg.get('cache_hit_ratio', 'n/a')}")
            print(f"    Deadlocks:   {pg.get('deadlocks_delta', 0)}")
        if len(runs) >= 2:
            delta = runs[-1]["processing_s"] - runs[0]["processing_s"]
            pct = (delta / runs[0]["processing_s"] * 100) if runs[0]["processing_s"] > 0 else 0
            print(f"\n  Processing delta ({runs[0]['replicas']}r → {runs[-1]['replicas']}r): "
                  f"{delta:+.1f}s ({pct:+.1f}%)")
        print(f"{'='*70}\n")

        assert all(r["all_processed"] for r in runs), (
            "Not all sources processed in one or more replica configurations"
        )


# ---------------------------------------------------------------------------
# CEL-2: Worker OOM Threshold
# ---------------------------------------------------------------------------

_CEL_002_MEMORY_LEVELS = {
    "baseline": [
        pytest.param("256Mi", id="256Mi"),
    ],
    "medium": [
        pytest.param("256Mi", id="256Mi"),
        pytest.param("512Mi", id="512Mi"),
    ],
    "large": [
        pytest.param("128Mi", id="128Mi"),
        pytest.param("256Mi", id="256Mi"),
        pytest.param("512Mi", id="512Mi"),
    ],
    "xlarge": [
        pytest.param("128Mi", id="128Mi"),
        pytest.param("256Mi", id="256Mi"),
        pytest.param("512Mi", id="512Mi"),
    ],
}
CEL_002_MEMORY_LEVELS = _CEL_002_MEMORY_LEVELS.get(
    _ACTIVE_PROFILE, _CEL_002_MEMORY_LEVELS.get("medium", [])
)


@pytest.mark.performance
@pytest.mark.celery_scaling
@pytest.mark.slow
class TestWorkerOOMThreshold:
    """PERF-CEL-002: Worker OOM threshold.

    Constrains OCP worker memory to find the minimum that avoids OOMKill
    under an ingestion workload.
    """

    @pytest.fixture(autouse=True)
    def setup(self, cluster_config: ClusterConfig, keycloak_config):
        self.namespace = cluster_config.namespace
        self.helm_release = cluster_config.helm_release_name
        self._keycloak_config = keycloak_config
        if not ensure_nise_available():
            pytest.skip("NISE (koku-nise) not available")

    @pytest.mark.timeout(1800)
    @pytest.mark.parametrize("memory_limit", CEL_002_MEMORY_LEVELS)
    def test_perf_cel_002_worker_oom_threshold(
        self,
        memory_limit: str,
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
    ):
        """Constrain OCP worker memory and run ingestion to detect OOMKill."""
        deploy_name = f"{self.helm_release}-celery-worker-ocp"
        original_resources = get_resource_spec(
            self.namespace, "deployment", deploy_name,
        )

        constrained = merge_resources(original_resources, {
            "requests": {"memory": memory_limit},
            "limits": {"memory": memory_limit},
        })

        restarts_before = get_pod_restart_counts(
            self.namespace, "cost-onprem.io/worker-queue=ocp",
        )

        try:
            print(f"\n[CEL-002] Constraining {deploy_name} memory to {memory_limit}")
            patched = patch_resource_spec(
                self.namespace, "deployment", deploy_name, constrained,
            )
            if not patched:
                pytest.skip(f"Failed to patch {deploy_name} to {memory_limit}")

            time.sleep(15)

            with perf_timer.measure(f"ingestion_{memory_limit}"):
                batch = run_ingestion_batch(
                    namespace=self.namespace,
                    concurrent_sources=3,
                    ingress_url=ingress_url,
                    ingress_pod=ingress_pod,
                    koku_api_url=koku_api_url,
                    rh_identity_header=rh_identity_header,
                    database_config=database_config,
                    perf_cleanup=perf_cleanup,
                    keycloak_config=self._keycloak_config,
                    label=f"cel-002-{memory_limit}",
                )

            restarts_after = get_pod_restart_counts(
                self.namespace, "cost-onprem.io/worker-queue=ocp",
            )
            oom_events = get_oomkill_events(
                self.namespace, "cost-onprem.io/worker-queue=ocp",
            )

            new_restarts = {}
            for pod, count in restarts_after.items():
                before = restarts_before.get(pod, 0)
                if count > before:
                    new_restarts[pod] = count - before

        finally:
            restore_resource_spec(
                self.namespace, "deployment", deploy_name, original_resources,
            )

        had_oom = len(oom_events) > 0 or len(new_restarts) > 0

        perf_result.test_id = f"PERF-CEL-002-{memory_limit}"
        perf_result.metrics = {
            "memory_limit": memory_limit,
            "processing_s": batch.processing_elapsed_s,
            "processed": batch.processed,
            "concurrent_sources": 3,
            "oom_events": oom_events,
            "new_restarts": new_restarts,
            "had_oom": had_oom,
            "all_processed": batch.all_processed,
        }
        perf_result.timings = perf_timer.get_timings()
        perf_result.passed = True  # informational — OOM is an expected outcome at low limits
        perf_collector.add_result(perf_result)

        print(f"\n{'='*70}")
        print(f"  PERF-CEL-002 | OCP worker memory = {memory_limit}")
        print(f"{'='*70}")
        print(f"  Processed:    {batch.processed}/3")
        print(f"  Processing:   {batch.processing_elapsed_s:.1f}s")
        print(f"  OOM events:   {len(oom_events)}")
        print(f"  New restarts: {new_restarts}")
        print(f"  Verdict:      {'OOMKilled' if had_oom else 'Survived'}")
        print(f"{'='*70}\n")


# ---------------------------------------------------------------------------
# ROS-1: Multi-Replica Kruize Validation
# ---------------------------------------------------------------------------

@pytest.mark.performance
@pytest.mark.celery_scaling
class TestKruizeMultiReplica:
    """PERF-ROS-001: Multi-replica Kruize validation.

    Scales Kruize to 2 replicas and captures per-pod CPU utilization to
    verify pod scheduling and readiness, confirming FINDING-004's
    single-replica recommendation.

    This test produces comparison data only — it does NOT run the full ROS
    experiment pipeline or drive API traffic.  It validates that the second
    replica starts and reports metrics (via ``oc adm top pod``).
    """

    @pytest.fixture(autouse=True)
    def setup(self, cluster_config: ClusterConfig):
        self.namespace = cluster_config.namespace
        self.helm_release = cluster_config.helm_release_name

    @pytest.mark.timeout(600)
    def test_perf_ros_001_kruize_multi_replica(
        self,
        cluster_config: ClusterConfig,
        perf_timer: PerfTimer,
        perf_result: PerformanceResult,
        perf_collector: PerfResultCollector,
    ):
        """Compare Kruize pod scheduling and CPU at 1 vs 2 replicas."""
        deploy_name = f"{self.helm_release}-kruize"
        original_replicas = get_deployment_replicas(self.namespace, deploy_name)

        results = []

        try:
            for replicas in [1, 2]:
                print(f"\n[ROS-001] Kruize at {replicas} replica(s)")
                scale_deployment(self.namespace, deploy_name, replicas)

                kruize_label = "app.kubernetes.io/component=ros-optimization"
                kruize_cpu = _wait_for_pod_metrics(
                    self.namespace, kruize_label, replicas,
                )

                results.append({
                    "replicas": replicas,
                    "kruize_cpu": kruize_cpu,
                    "pod_count": len(kruize_cpu),
                })
        finally:
            scale_deployment(self.namespace, deploy_name, original_replicas)

        perf_result.test_id = "PERF-ROS-001"
        perf_result.metrics = {
            "original_replicas": original_replicas,
            "results": results,
        }
        perf_result.timings = perf_timer.get_timings()
        perf_result.passed = True
        perf_collector.add_result(perf_result)

        print(f"\n{'='*70}")
        print(f"  PERF-ROS-001 | Kruize multi-replica comparison")
        print(f"{'='*70}")
        for r in results:
            print(f"  [{r['replicas']} replica(s)]")
            print(f"    Pod count: {r['pod_count']}")
            print(f"    CPU:       {r['kruize_cpu']}")
        print(f"{'='*70}\n")


# ---------------------------------------------------------------------------
# CEL-3: Cold vs Warm State Characterization
# ---------------------------------------------------------------------------

def capture_warm_state_indicators(
    namespace: str,
    db_pod: str,
    db_name: str,
    db_user: str,
    helm_release: str,
) -> Dict[str, Any]:
    """Capture observable indicators of system warmth.

    Returns a snapshot of metrics that should differ between a cold
    (freshly started) and warm (post-workload) system state:
    - pg_stat cache hit ratio and cumulative counters
    - Worker pod ages (seconds since creation)
    - Active PostgreSQL connection count
    """
    indicators: Dict[str, Any] = {}

    # PostgreSQL stats
    pg = capture_pg_stats(namespace, db_pod, db_name, db_user)
    indicators["pg_cache_hit_ratio"] = pg.cache_hit_ratio
    indicators["pg_blks_hit"] = pg.blks_hit
    indicators["pg_blks_read"] = pg.blks_read
    indicators["pg_xact_commit"] = pg.xact_commit

    # Active connection count
    rows = execute_db_query(
        namespace, db_pod, db_name, db_user,
        "SELECT count(*) FROM pg_stat_activity WHERE state = 'active'",
    )
    if rows and rows[0]:
        try:
            indicators["pg_active_connections"] = int(rows[0][0])
        except (ValueError, IndexError):
            indicators["pg_active_connections"] = -1

    # Worker pod ages — use the actual chart labels:
    #   Workers: app.kubernetes.io/component=cost-worker, cost-onprem.io/worker-queue=<queue>
    #   Listener: app.kubernetes.io/component=listener
    worker_ages: Dict[str, float] = {}
    label_selectors = [
        f"app.kubernetes.io/instance={helm_release},app.kubernetes.io/component=cost-worker",
        f"app.kubernetes.io/instance={helm_release},app.kubernetes.io/component=listener",
    ]
    for selector in label_selectors:
        result = run_oc_command(
            ["get", "pods", "-n", namespace,
             "-l", selector,
             "-o", "jsonpath={range .items[*]}{.metadata.name} "
                   "{.metadata.creationTimestamp}{'\\n'}{end}"],
            check=False,
        )
        if result.returncode == 0:
            for line in result.stdout.strip().splitlines():
                parts = line.split()
                if len(parts) >= 2:
                    try:
                        created = datetime.fromisoformat(
                            parts[1].replace("Z", "+00:00")
                        )
                        age_s = (datetime.now(timezone.utc) - created).total_seconds()
                        worker_ages[parts[0]] = round(age_s, 0)
                    except (ValueError, TypeError):
                        pass
    indicators["worker_pod_ages"] = worker_ages

    return indicators


@pytest.mark.performance
@pytest.mark.celery_scaling
@pytest.mark.slow
class TestColdWarmCharacterization:
    """PERF-CEL-003: Cold vs warm state characterization.

    Runs two sequential ingestion batches on the same cluster and captures
    warm-state indicators before and after each.  The delta between batch-1
    and batch-2 processing times, combined with the indicator snapshots,
    helps identify which caching layers are responsible for the ~49%
    speedup observed in sequential runs (FINDING-030).

    This test does NOT restart pods between batches — it measures the
    natural warm-up effect.  A separate "post-restart" scenario can be
    added once cold/warm indicators are understood.
    """

    @pytest.fixture(autouse=True)
    def setup(self, cluster_config: ClusterConfig, keycloak_config):
        self.namespace = cluster_config.namespace
        self.helm_release = cluster_config.helm_release_name
        self._keycloak_config = keycloak_config
        if not ensure_nise_available():
            pytest.skip("NISE (koku-nise) not available")

    @pytest.mark.timeout(3600)
    def test_perf_cel_003_cold_warm_characterization(
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
    ):
        """Run two sequential batches, capturing warm-state indicators around each."""
        concurrent = _CEL_CONCURRENCY.get(_ACTIVE_PROFILE, 5)
        batches: List[Dict[str, Any]] = []

        for batch_num in (1, 2):
            label = f"batch-{batch_num}"
            print(f"\n[CEL-003] === {label} ===")

            indicators_before = capture_warm_state_indicators(
                self.namespace, database_config.pod_name,
                database_config.database, database_config.user,
                self.helm_release,
            )
            pg_before = capture_pg_stats(
                self.namespace, database_config.pod_name,
                database_config.database, database_config.user,
            )

            with perf_timer.measure(label):
                batch = run_ingestion_batch(
                    namespace=self.namespace,
                    concurrent_sources=concurrent,
                    ingress_url=ingress_url,
                    ingress_pod=ingress_pod,
                    koku_api_url=koku_api_url,
                    rh_identity_header=rh_identity_header,
                    database_config=database_config,
                    perf_cleanup=perf_cleanup,
                    keycloak_config=self._keycloak_config,
                    label=f"cel-003-{label}",
                )

            indicators_after = capture_warm_state_indicators(
                self.namespace, database_config.pod_name,
                database_config.database, database_config.user,
                self.helm_release,
            )
            pg_after = capture_pg_stats(
                self.namespace, database_config.pod_name,
                database_config.database, database_config.user,
            )
            db_cpu = get_db_cpu_utilization(
                self.namespace, database_config.pod_name,
            )

            batches.append({
                "batch": batch_num,
                "processing_s": batch.processing_elapsed_s,
                "queue_drain_s": batch.queue_drain_elapsed_s,
                "processed": batch.processed,
                "all_processed": batch.all_processed,
                "db_cpu_millicores": db_cpu,
                "pg_stat_delta": diff_pg_stats(pg_before, pg_after),
                "indicators_before": indicators_before,
                "indicators_after": indicators_after,
            })

        perf_result.test_id = "PERF-CEL-003"
        perf_result.metrics = {
            "concurrent_sources": concurrent,
            "batches": batches,
        }
        perf_result.timings = perf_timer.get_timings()
        perf_result.passed = all(b["all_processed"] for b in batches)
        perf_collector.add_result(perf_result)

        # Print summary
        print(f"\n{'='*70}")
        print(f"  PERF-CEL-003 | Cold vs Warm State Characterization")
        print(f"{'='*70}")
        for b in batches:
            pg = b["pg_stat_delta"]
            ind_b = b["indicators_before"]
            print(f"  [Batch {b['batch']}]")
            print(f"    Processing:       {b['processing_s']:.1f}s")
            print(f"    Queue drain:      {b['queue_drain_s']:.1f}s")
            print(f"    DB CPU:           {b['db_cpu_millicores']}m")
            print(f"    Cache hit ratio:  {pg.get('cache_hit_ratio', 'n/a')}")
            print(f"    Blocks hit:       {pg.get('blks_hit_delta', 0):,}")
            print(f"    Blocks read:      {pg.get('blks_read_delta', 0):,}")
            print(f"    Transactions:     {pg.get('xact_commit_delta', 0):,}")
            print(f"    Deadlocks:        {pg.get('deadlocks_delta', 0)}")
            print(f"    Active conns (before): {ind_b.get('pg_active_connections', 'n/a')}")
            ages = ind_b.get("worker_pod_ages", {})
            if ages:
                avg_age = sum(ages.values()) / len(ages)
                print(f"    Worker avg age:   {avg_age:.0f}s ({len(ages)} pods)")

        if len(batches) == 2:
            b1, b2 = batches[0]["processing_s"], batches[1]["processing_s"]
            if b1 > 0:
                speedup = (b1 - b2) / b1 * 100
                print(f"\n  Batch-2 speedup: {speedup:+.1f}% "
                      f"({b1:.1f}s → {b2:.1f}s)")
            hit1 = batches[0]["pg_stat_delta"].get("cache_hit_ratio", 0)
            hit2 = batches[1]["pg_stat_delta"].get("cache_hit_ratio", 0)
            print(f"  Cache hit ratio:  batch-1={hit1:.4f}  batch-2={hit2:.4f}")
        print(f"{'='*70}\n")

        assert all(b["all_processed"] for b in batches), (
            "Not all sources processed in one or both batches"
        )
