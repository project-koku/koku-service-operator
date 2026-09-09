"""
Kafka Throughput and Scaling Performance Tests (COST-7638).

Tests Kafka (AMQ Streams) throughput ceiling, consumer lag recovery,
and partition scaling behavior under concurrent ingestion load.

Test IDs:
- PERF-KAF-001: Single-partition throughput ceiling
- PERF-KAF-002: Partition scaling validation
"""

import time
from concurrent.futures import ThreadPoolExecutor, as_completed
from datetime import datetime, timedelta, timezone
from typing import Any, Dict, List

import pytest

from conftest import ClusterConfig, JWTToken, obtain_jwt_token
from e2e_helpers import (
    wait_for_processing_complete,
    ensure_nise_available,
    generate_cluster_id,
    register_source,
)
from utils import run_oc_command

from .data_classes import PerformanceResult
from .helpers import (
    PerfResultCollector,
    PerfTimer,
    generate_and_upload_data,
)
from .kafka_helpers import (
    KafkaMonitor,
    get_broker_disk_usage,
    get_broker_resource_usage,
    get_kafka_broker_pod,
    get_kafka_namespace,
    get_topic_partition_count,
    get_total_consumer_lag,
)
from .queue_helpers import wait_for_queue_drain
from .tracker import PerfCleanupTracker
from .profiles import ACTIVE_PROFILE as _ACTIVE_PROFILE


# ---------------------------------------------------------------------------
# Profile-gated parametrize lists
# ---------------------------------------------------------------------------

_KAF_001_CONCURRENCY: dict = {
    "baseline": [
        pytest.param(2, id="2-sources"),
    ],
    "medium": [
        pytest.param(2, id="2-sources"),
        pytest.param(5, id="5-sources"),
        pytest.param(10, id="10-sources"),
        pytest.param(15, id="15-sources"),
    ],
    "large": [
        pytest.param(2, id="2-sources"),
        pytest.param(5, id="5-sources"),
        pytest.param(10, id="10-sources"),
        pytest.param(15, id="15-sources"),
        pytest.param(20, id="20-sources"),
    ],
    "xlarge": [
        pytest.param(2, id="2-sources"),
        pytest.param(5, id="5-sources"),
        pytest.param(10, id="10-sources"),
        pytest.param(15, id="15-sources"),
        pytest.param(20, id="20-sources"),
        pytest.param(25, id="25-sources"),
        pytest.param(30, id="30-sources"),
    ],
}
KAF_001_CONCURRENCY = _KAF_001_CONCURRENCY.get(
    _ACTIVE_PROFILE, _KAF_001_CONCURRENCY.get("medium", [])
)


# ---------------------------------------------------------------------------
# Test class
# ---------------------------------------------------------------------------

@pytest.mark.performance
@pytest.mark.kafka_throughput
@pytest.mark.slow
class TestKafkaThroughputCeiling:
    """PERF-KAF-001: Single-partition throughput ceiling.

    Determines the maximum concurrent upload rate the current Kafka setup
    can sustain before consumer lag grows unboundedly. Runs ING-003-style
    concurrent uploads at increasing concurrency while monitoring Kafka
    consumer lag recovery.
    """

    @pytest.fixture(autouse=True)
    def setup(self, cluster_config: ClusterConfig, keycloak_config):
        self.namespace = cluster_config.namespace
        self.helm_release = cluster_config.helm_release_name
        if not ensure_nise_available():
            pytest.skip("NISE (koku-nise) not available")
        self._keycloak_config = keycloak_config

        self.kafka_namespace = get_kafka_namespace()
        self.broker_pod = get_kafka_broker_pod(self.kafka_namespace)
        if not self.broker_pod:
            pytest.skip(
                f"No Kafka broker pod found in namespace '{self.kafka_namespace}'"
            )

    def _get_fresh_token(self) -> JWTToken:
        return obtain_jwt_token(self._keycloak_config)

    @pytest.mark.timeout(2400)
    @pytest.mark.parametrize("concurrent_sources", KAF_001_CONCURRENCY)
    def test_perf_kaf_001_throughput_ceiling(
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
        perf_cleanup: PerfCleanupTracker,
        ingress_pod: str,
    ):
        """Concurrent uploads with Kafka consumer lag monitoring.

        For each concurrency level, uploads N sources simultaneously while a
        KafkaMonitor records consumer lag snapshots.  After uploads finish,
        continues monitoring until lag recovers to 0 or the budget expires.
        """
        # ---- Capture pre-test Kafka state ----
        pre_partitions = get_topic_partition_count(
            self.kafka_namespace, self.broker_pod,
            "platform.upload.announce",
        )
        pre_disk = get_broker_disk_usage(
            self.kafka_namespace, self.broker_pod,
        )
        pre_lag = get_total_consumer_lag(
            self.kafka_namespace, self.broker_pod,
        )

        # ---- Register sources ----
        sources: List[Dict[str, Any]] = []
        with perf_timer.measure("source_registration"):
            for i in range(concurrent_sources):
                cluster_id = generate_cluster_id()
                source_name = f"perf-kaf-001-{i:02d}-{cluster_id[-8:]}"
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

        end_date = datetime.now(timezone.utc)
        start_date = end_date - timedelta(days=7)

        # ---- Start Kafka monitor ----
        monitor = KafkaMonitor(
            self.kafka_namespace, self.broker_pod, poll_interval=5.0,
        )
        monitor.start()

        # ---- Concurrent uploads ----
        upload_results: List[Dict[str, Any]] = []
        errors: List[Dict[str, Any]] = []

        def _upload(source_info: Dict[str, Any]) -> Dict[str, Any]:
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
            with ThreadPoolExecutor(max_workers=concurrent_sources) as pool:
                futures = {pool.submit(_upload, s): s for s in sources}
                for future in as_completed(futures):
                    result = future.result()
                    if "error" in result:
                        errors.append(result)
                    else:
                        upload_results.append(result)

        # ---- Wait for processing ----
        total_budget = 120 + concurrent_sources * 30
        if _ACTIVE_PROFILE in ("medium", "large", "xlarge"):
            total_budget = int(total_budget * 1.5)
        deadline = time.time() + total_budget
        processed_count = 0

        with perf_timer.measure("processing_wait"):
            for source_info in sources:
                remaining = max(15, int(deadline - time.time()))
                proc = wait_for_processing_complete(
                    self.namespace,
                    database_config.pod_name,
                    source_info["cluster_id"],
                    max_wait_seconds=remaining,
                )
                if proc["complete"]:
                    processed_count += 1

        # ---- Wait for queue drain + lag recovery ----
        with perf_timer.measure("queue_drain"):
            drain_result = wait_for_queue_drain(
                self.namespace,
                max_wait_seconds=600,
                label=f"KAF-001[{concurrent_sources}]",
            )

        with perf_timer.measure("lag_recovery"):
            max_lag_wait = 300
            lag_start = time.time()
            lag_recovered = False
            while time.time() - lag_start < max_lag_wait:
                lag = get_total_consumer_lag(
                    self.kafka_namespace, self.broker_pod,
                )
                total = sum(lag.values())
                if total == 0:
                    lag_recovered = True
                    break
                time.sleep(10)

        # ---- Stop monitor ----
        snapshots = monitor.stop()

        # ---- Post-test Kafka state ----
        post_disk = get_broker_disk_usage(
            self.kafka_namespace, self.broker_pod,
        )
        post_lag = get_total_consumer_lag(
            self.kafka_namespace, self.broker_pod,
        )

        # Capture listener CPU to identify consumer-side bottleneck
        listener_cpu = None
        listener_result = run_oc_command(
            ["adm", "top", "pod", "-n", self.namespace,
             "-l", "app.kubernetes.io/component=listener", "--no-headers"],
            check=False,
        )
        if listener_result.returncode == 0 and listener_result.stdout.strip():
            parts = listener_result.stdout.strip().split()
            if len(parts) >= 2:
                listener_cpu = parts[1]

        # ---- Record results ----
        perf_result.test_id = f"PERF-KAF-001-{concurrent_sources}src"
        perf_result.metrics = {
            "concurrent_sources": concurrent_sources,
            "successful_uploads": len(upload_results),
            "failed_uploads": len(errors),
            "processed_count": processed_count,
            "total_upload_mb": sum(
                r.get("package_size_mb", 0) for r in upload_results
            ),
            "kafka": {
                "platform_topic_partitions": pre_partitions,
                "peak_consumer_lag": monitor.peak_lag(),
                "lag_recovery_seconds": monitor.lag_recovery_time(),
                "lag_recovered": lag_recovered,
                "snapshots_collected": len(snapshots),
                "pre_lag": pre_lag,
                "post_lag": post_lag,
                "pre_disk": pre_disk,
                "post_disk": post_disk,
                "broker_resources": (
                    get_broker_resource_usage(self.kafka_namespace)
                ),
            },
            "listener_cpu_at_completion": listener_cpu,
            "queue_drain": drain_result,
            "errors": errors,
        }
        perf_result.timings = perf_timer.get_timings()
        perf_result.passed = (
            processed_count == concurrent_sources and len(errors) == 0
        )
        perf_collector.add_result(perf_result)

        # ---- Print summary ----
        print(f"\n{'='*70}")
        print(f"  PERF-KAF-001 | {concurrent_sources} concurrent sources")
        print(f"{'='*70}")
        print(f"  Uploads:      {len(upload_results)} OK / {len(errors)} failed")
        print(f"  Processed:    {processed_count}/{concurrent_sources}")
        print(f"  Upload MB:    {perf_result.metrics['total_upload_mb']:.1f}")
        print(f"  Partitions:   {pre_partitions}")
        print(f"  Peak lag:     {monitor.peak_lag()} messages")
        recovery = monitor.lag_recovery_time()
        if recovery is not None:
            print(f"  Lag recovery: {recovery:.1f}s")
        elif monitor.peak_lag() == 0:
            print(f"  Lag recovery: n/a (no lag observed)")
        else:
            print(f"  Lag recovery: did not recover")
        print(f"  Lag final:    {post_lag}")
        print(f"  Listener CPU: {listener_cpu or 'n/a'}")
        timings = perf_timer.get_timings()
        for t in timings:
            print(f"  {t.name}: {t.duration_seconds:.1f}s")
        print(f"{'='*70}\n")

        assert len(errors) == 0, f"Upload errors: {errors}"
        assert processed_count == concurrent_sources, (
            f"Only {processed_count}/{concurrent_sources} processed"
        )


@pytest.mark.performance
@pytest.mark.kafka_throughput
@pytest.mark.slow
class TestKafkaPartitionScaling:
    """PERF-KAF-002: Partition scaling validation.

    Tests whether increasing Kafka partitions + listener replicas
    provides throughput improvement. Modifies topic partitions and
    listener replica count, then runs concurrent uploads to measure
    the delta.

    **Side effects**: Kafka does not allow decreasing partition counts.
    The ``finally`` block restores listener replicas, but any partition
    increase is permanent for the life of the cluster.  Subsequent runs
    on the same cluster will use the already-scaled partition count as
    their baseline.  The test records ``original_partitions`` in its
    metrics so the starting state is always visible in results.
    """

    @pytest.fixture(autouse=True)
    def setup(self, cluster_config: ClusterConfig, keycloak_config):
        self.namespace = cluster_config.namespace
        self.helm_release = cluster_config.helm_release_name
        if not ensure_nise_available():
            pytest.skip("NISE (koku-nise) not available")
        self._keycloak_config = keycloak_config

        self.kafka_namespace = get_kafka_namespace()
        self.broker_pod = get_kafka_broker_pod(self.kafka_namespace)
        if not self.broker_pod:
            pytest.skip(
                f"No Kafka broker pod found in namespace '{self.kafka_namespace}'"
            )

        if _ACTIVE_PROFILE == "baseline":
            pytest.skip(
                "KAF-002 partition scaling is not meaningful at baseline profile"
            )

    def _get_fresh_token(self) -> JWTToken:
        return obtain_jwt_token(self._keycloak_config)

    def _set_topic_partitions(self, topic: str, partitions: int) -> bool:
        """Increase partition count (cannot decrease in Kafka)."""
        result = run_oc_command(
            ["exec", "-n", self.kafka_namespace, self.broker_pod, "--",
             "bin/kafka-topics.sh",
             "--bootstrap-server", "localhost:9092",
             "--alter", "--topic", topic,
             "--partitions", str(partitions)],
            check=False, timeout=30,
        )
        if result.returncode != 0:
            print(f"[KAF-002] Failed to set {topic} to {partitions} partitions: "
                  f"{result.stderr}")
            return False
        print(f"[KAF-002] Set {topic} to {partitions} partitions")
        return True

    def _scale_listener(self, replicas: int) -> None:
        """Scale the listener deployment."""
        from .k8s_helpers import scale_deployment
        scale_deployment(
            self.namespace, f"{self.helm_release}-koku-listener", replicas,
        )

    def _get_listener_replicas(self) -> int:
        from .k8s_helpers import get_deployment_replicas
        return get_deployment_replicas(
            self.namespace, f"{self.helm_release}-koku-listener",
        )

    def _run_upload_batch(
        self,
        concurrent_sources: int,
        ingress_url: str,
        ingress_pod: str,
        koku_api_url: str,
        rh_identity_header: str,
        database_config,
        perf_cleanup: PerfCleanupTracker,
        label: str,
    ) -> Dict[str, Any]:
        """Run a batch of concurrent uploads and return throughput metrics."""
        sources: List[Dict[str, Any]] = []
        for i in range(concurrent_sources):
            cluster_id = generate_cluster_id()
            source_name = f"perf-kaf-002-{label}-{i:02d}-{cluster_id[-8:]}"
            source = register_source(
                self.namespace, ingress_pod,
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
        start_date = end_date - timedelta(days=7)

        monitor = KafkaMonitor(
            self.kafka_namespace, self.broker_pod, poll_interval=5.0,
        )
        monitor.start()

        upload_results = []
        errors = []
        upload_start = time.time()

        def _upload(info: Dict[str, Any]) -> Dict[str, Any]:
            try:
                token = self._get_fresh_token()
                return generate_and_upload_data(
                    info["cluster_id"], info["source_name"],
                    start_date, end_date, ingress_url, token,
                    profile_name="baseline",
                )
            except Exception as e:
                return {"error": str(e), "cluster_id": info["cluster_id"]}

        with ThreadPoolExecutor(max_workers=concurrent_sources) as pool:
            futures = {pool.submit(_upload, s): s for s in sources}
            for future in as_completed(futures):
                result = future.result()
                if "error" in result:
                    errors.append(result)
                else:
                    upload_results.append(result)

        upload_elapsed = time.time() - upload_start

        total_budget = 120 + concurrent_sources * 30
        if _ACTIVE_PROFILE in ("medium", "large", "xlarge"):
            total_budget = int(total_budget * 1.5)
        deadline = time.time() + total_budget
        processed = 0
        proc_start = time.time()
        for s in sources:
            remaining = max(15, int(deadline - time.time()))
            proc = wait_for_processing_complete(
                self.namespace, database_config.pod_name,
                s["cluster_id"], max_wait_seconds=remaining,
            )
            if proc["complete"]:
                processed += 1
        proc_elapsed = time.time() - proc_start

        wait_for_queue_drain(
            self.namespace, max_wait_seconds=600, label=f"KAF-002[{label}]",
        )

        # Poll until consumer lag reaches zero (or budget expires)
        lag_start = time.time()
        while time.time() - lag_start < 120:
            lag = get_total_consumer_lag(self.kafka_namespace, self.broker_pod)
            if sum(lag.values()) == 0:
                break
            time.sleep(10)

        snapshots = monitor.stop()
        total_mb = sum(r.get("package_size_mb", 0) for r in upload_results)

        return {
            "label": label,
            "concurrent_sources": concurrent_sources,
            "uploads_ok": len(upload_results),
            "uploads_failed": len(errors),
            "processed": processed,
            "total_mb": round(total_mb, 2),
            "upload_elapsed_s": round(upload_elapsed, 1),
            "processing_elapsed_s": round(proc_elapsed, 1),
            "throughput_mb_per_sec": round(total_mb / upload_elapsed, 3) if upload_elapsed > 0 else 0,
            "peak_lag": monitor.peak_lag(),
            "lag_recovery_s": monitor.lag_recovery_time(),
            "snapshots": len(snapshots),
        }

    @pytest.mark.timeout(3600)
    def test_perf_kaf_002_partition_scaling(
        self,
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
        """Scale partitions and listener replicas, measuring throughput delta.

        Runs:
          1. Baseline (current partition count and listener replica count)
          2. At least 3 partitions, 3 listener replicas
          3. Restore original listener replica count (partitions cannot be decreased)
        """
        topic = "platform.upload.announce"
        concurrent = 10
        original_partitions = get_topic_partition_count(
            self.kafka_namespace, self.broker_pod, topic,
        )
        original_replicas = self._get_listener_replicas()

        results: List[Dict[str, Any]] = []

        try:
            # --- Run 1: baseline ---
            with perf_timer.measure("run_baseline"):
                baseline = self._run_upload_batch(
                    concurrent, ingress_url, ingress_pod,
                    koku_api_url, rh_identity_header,
                    database_config, perf_cleanup, "baseline",
                )
                results.append(baseline)

            # --- Run 2: scaled partitions + listeners ---
            target_partitions = max(original_partitions, 3)
            if target_partitions > original_partitions:
                self._set_topic_partitions(topic, target_partitions)
                time.sleep(10)
            self._scale_listener(3)
            time.sleep(15)

            with perf_timer.measure("run_scaled"):
                scaled = self._run_upload_batch(
                    concurrent, ingress_url, ingress_pod,
                    koku_api_url, rh_identity_header,
                    database_config, perf_cleanup, "3part-3rep",
                )
                results.append(scaled)

        finally:
            self._scale_listener(original_replicas)

        perf_result.test_id = "PERF-KAF-002"
        perf_result.metrics = {
            "original_partitions": original_partitions,
            "original_listener_replicas": original_replicas,
            "runs": results,
        }
        perf_result.timings = perf_timer.get_timings()
        perf_result.passed = all(r["processed"] == concurrent for r in results)
        perf_collector.add_result(perf_result)

        # ---- Print summary ----
        print(f"\n{'='*70}")
        print(f"  PERF-KAF-002 | Partition Scaling Validation")
        print(f"{'='*70}")
        for r in results:
            print(f"  [{r['label']}]")
            print(f"    Uploads:       {r['uploads_ok']} OK / {r['uploads_failed']} failed")
            print(f"    Processed:     {r['processed']}/{r['concurrent_sources']}")
            print(f"    Throughput:    {r['throughput_mb_per_sec']} MB/s")
            print(f"    Peak lag:      {r['peak_lag']} messages")
            recovery = r['lag_recovery_s']
            if recovery is not None:
                print(f"    Lag recovery:  {recovery:.1f}s")
            elif r['peak_lag'] == 0:
                print(f"    Lag recovery:  n/a (no lag observed)")
            else:
                print(f"    Lag recovery:  did not recover")
        if len(results) >= 2:
            delta = results[1]["throughput_mb_per_sec"] - results[0]["throughput_mb_per_sec"]
            print(f"\n  Throughput delta: {delta:+.3f} MB/s "
                  f"({results[0]['label']} → {results[1]['label']})")
        print(f"{'='*70}\n")

        assert all(r["processed"] == concurrent for r in results), (
            "Not all sources processed in one or more runs"
        )
