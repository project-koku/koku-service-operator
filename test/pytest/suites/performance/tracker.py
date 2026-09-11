"""Performance test resource cleanup and tracking."""

import os
import time
import warnings
from dataclasses import dataclass, field
from typing import Dict, List, Optional

from utils import get_pod_by_label, run_oc_command

from .helpers import build_koku_api_url, parse_cpu_millicores, parse_memory_mib


@dataclass
class PerfTestResource:
    """Tracks a resource created by a performance test for cleanup."""
    source_id: Optional[str] = None
    cluster_id: Optional[str] = None
    source_name: Optional[str] = None


class PerfCleanupTracker:
    """Tracks resources created during performance tests for cleanup.

    Usage in tests::

        def test_something(self, perf_cleanup, ...):
            source = register_source(...)
            perf_cleanup.track(source_id=source.source_id, cluster_id=cluster_id)
            # ... test logic ...
            # Cleanup happens automatically after test
    """

    CLEANUP_TIMEOUT_S = int(os.environ.get("PERF_CLEANUP_TIMEOUT", "900"))

    def __init__(self, namespace: str, helm_release: str):
        self.namespace = namespace
        self.helm_release = helm_release
        self.resources: List[PerfTestResource] = []
        self._cleanup_enabled = os.environ.get("E2E_CLEANUP_AFTER", "true").lower() != "false"

    def track(
        self,
        source_id: Optional[str] = None,
        cluster_id: Optional[str] = None,
        source_name: Optional[str] = None,
    ):
        """Track a resource for cleanup after test completes."""
        self.resources.append(PerfTestResource(
            source_id=source_id,
            cluster_id=cluster_id,
            source_name=source_name,
        ))

    def _wait_for_ros_drain(self):
        """Wait for the ROS processor Kafka consumer lag to reach zero.

        The ros-processor is a Go-based Kafka consumer on ``hccm.ros.events``.
        If we delete sources before it has consumed the corresponding events,
        it hits FK constraint errors that poison the queue.  We must wait for
        the queue to fully drain before deleting any sources.

        The timeout scales with the number of tracked resources — more sources
        means more ROS events to process (~3s per workload via Kruize API).
        We also track whether lag is making progress; if lag stalls completely
        for ``stall_timeout`` seconds we give up (processor may be stuck on
        an unrelated error).
        """
        try:
            from suites.performance.test_ros import get_ros_queue_depth
        except ImportError:
            return

        num_resources = len(self.resources)
        max_timeout = max(120, num_resources * 60)
        stall_timeout = 90

        start = time.time()
        prev_lag = None
        last_progress_time = start
        while time.time() - start < max_timeout:
            lag = get_ros_queue_depth(self.namespace)
            if lag is not None and lag == 0:
                return
            if lag is not None:
                if lag != prev_lag:
                    print(f"  [ros-drain] lag={lag}, waiting…")
                    if prev_lag is not None and lag < prev_lag:
                        last_progress_time = time.time()
                    prev_lag = lag
                elif time.time() - last_progress_time > stall_timeout:
                    print(f"  [ros-drain] lag stalled at {lag} for {stall_timeout}s, giving up")
                    return
            time.sleep(5)
        print(f"  [ros-drain] drained to lag={prev_lag} (timeout {max_timeout}s)")

    def _get_s3_config(self) -> Optional[Dict[str, str]]:
        """Get S3 configuration from the cluster."""
        try:
            import base64

            result = run_oc_command([
                "get", "secret", f"{self.helm_release}-s3-credentials",
                "-n", self.namespace,
                "-o", "jsonpath={.data.access-key}"
            ], check=False)
            if result.returncode != 0:
                return None

            access_key = base64.b64decode(result.stdout.strip()).decode('utf-8')

            result = run_oc_command([
                "get", "secret", f"{self.helm_release}-s3-credentials",
                "-n", self.namespace,
                "-o", "jsonpath={.data.secret-key}"
            ], check=False)
            if result.returncode != 0:
                return None

            secret_key = base64.b64decode(result.stdout.strip()).decode('utf-8')

            result = run_oc_command([
                "get", "configmap", f"{self.helm_release}-aws-config",
                "-n", self.namespace,
                "-o", "jsonpath={.data.endpoint}"
            ], check=False)

            if result.returncode == 0 and result.stdout.strip():
                endpoint = result.stdout.strip()
            else:
                s4_result = run_oc_command([
                    "get", "svc", "s4", "-n", "s4",
                    "-o", "jsonpath={.spec.clusterIP}"
                ], check=False)
                if s4_result.returncode == 0 and s4_result.stdout.strip():
                    endpoint = f"http://{s4_result.stdout.strip()}:7480"
                else:
                    endpoint = "https://s3.openshift-storage.svc:443"

            return {
                "endpoint": endpoint,
                "access_key": access_key,
                "secret_key": secret_key,
                "bucket": "koku-bucket",
                "verify_ssl": False,
            }
        except Exception as e:
            print(f"  [s3-cleanup] Could not get S3 config: {e}")
            return None

    def _cleanup_s3_data(self, failures: List[str], deadline: float):
        """Clean up S3 data for all tracked clusters."""
        from cleanup import cleanup_s3_data

        s3_config = self._get_s3_config()
        if not s3_config:
            print("  [s3-cleanup] Skipped - could not get S3 credentials")
            return

        cluster_ids = {r.cluster_id for r in self.resources if r.cluster_id}
        if not cluster_ids:
            return

        org_id = os.environ.get("ORG_ID", "6089719")

        total_deleted = 0
        skipped = 0
        for cluster_id in cluster_ids:
            if time.time() >= deadline:
                skipped = len(cluster_ids) - total_deleted - skipped
                msg = f"S3 cleanup timeout — {skipped} cluster(s) skipped"
                print(f"  [s3-cleanup] {msg}")
                failures.append(msg)
                break
            try:
                result = cleanup_s3_data(
                    endpoint=s3_config["endpoint"],
                    access_key=s3_config["access_key"],
                    secret_key=s3_config["secret_key"],
                    bucket=s3_config["bucket"],
                    org_id=org_id,
                    cluster_id=cluster_id,
                    verify_ssl=s3_config["verify_ssl"],
                )
                files_deleted = result.get("files_deleted", 0)
                total_deleted += files_deleted
                if files_deleted > 0:
                    print(f"  Cleaned {files_deleted} S3 files for cluster {cluster_id}")
                if result.get("error"):
                    print(f"  Warning: S3 cleanup error for {cluster_id}: {result['error']}")
            except Exception as e:
                msg = f"Error cleaning S3 for cluster {cluster_id}: {e}"
                print(f"  {msg}")
                failures.append(msg)

        if total_deleted > 0:
            print(f"  [s3-cleanup] Total: {total_deleted} files deleted")

    def cleanup(self, rh_identity_header: str):
        """Clean up all tracked resources.

        The entire cleanup is capped at CLEANUP_TIMEOUT_S (default 900s /
        15 min, configurable via PERF_CLEANUP_TIMEOUT env var).  If the
        timeout fires, remaining resources are skipped and a warning is
        emitted.  This prevents hung cleanup from blocking the pipeline
        (see xlarge stress run #97 where cleanup hung for 30+ minutes).

        Emits a RuntimeWarning if any cleanup operations fail, so test
        output surfaces dirty state without failing the test itself.
        """
        from e2e_helpers import cleanup_database_records, delete_source

        if not self._cleanup_enabled:
            print(f"\n[PERF CLEANUP] Skipped (E2E_CLEANUP_AFTER=false)")
            print(f"  Tracked resources: {len(self.resources)}")
            for r in self.resources:
                print(f"    - source_id={r.source_id}, cluster_id={r.cluster_id}")
            return

        if not self.resources:
            return

        deadline = time.time() + self.CLEANUP_TIMEOUT_S
        total = len(self.resources)
        print(f"\n[PERF CLEANUP] Cleaning {total} tracked resources "
              f"(timeout: {self.CLEANUP_TIMEOUT_S}s)...")

        # Wait for the ROS processor to consume any pending Kafka events
        # before deleting sources (PERF-FINDING-013).
        self._wait_for_ros_drain()

        if time.time() >= deadline:
            msg = (f"Cleanup timeout ({self.CLEANUP_TIMEOUT_S}s) exhausted during "
                   f"ros-drain — skipping {total} resource deletion(s)")
            print(f"[PERF CLEANUP] {msg}")
            self.resources.clear()
            warnings.warn(msg, RuntimeWarning, stacklevel=2)
            return

        ingress_pod = get_pod_by_label(self.namespace, "app.kubernetes.io/component=ingress")
        db_pod = get_pod_by_label(self.namespace, "app.kubernetes.io/component=database")

        koku_api_url = build_koku_api_url(self.helm_release, self.namespace)

        failures: List[str] = []
        cleaned = 0

        for resource in self.resources:
            if time.time() >= deadline:
                remaining = total - cleaned
                msg = (f"Cleanup timeout ({self.CLEANUP_TIMEOUT_S}s) reached after "
                       f"{cleaned}/{total} resources — {remaining} skipped")
                print(f"\n[PERF CLEANUP] {msg}")
                failures.append(msg)
                break

            if resource.source_id and ingress_pod:
                try:
                    if delete_source(
                        self.namespace,
                        ingress_pod,
                        koku_api_url,
                        rh_identity_header,
                        resource.source_id,
                        container="ingress",
                    ):
                        print(f"  Deleted source {resource.source_id}")
                    else:
                        msg = f"Could not delete source {resource.source_id}"
                        print(f"  Warning: {msg}")
                        failures.append(msg)
                except Exception as e:
                    msg = f"Error deleting source {resource.source_id}: {e}"
                    print(f"  {msg}")
                    failures.append(msg)

            if resource.cluster_id and db_pod:
                try:
                    if cleanup_database_records(self.namespace, db_pod, resource.cluster_id):
                        print(f"  Cleaned DB records for cluster {resource.cluster_id}")
                    else:
                        msg = f"Could not clean DB for cluster {resource.cluster_id}"
                        print(f"  Warning: {msg}")
                        failures.append(msg)
                except Exception as e:
                    msg = f"Error cleaning DB for cluster {resource.cluster_id}: {e}"
                    print(f"  {msg}")
                    failures.append(msg)

            cleaned += 1

        if time.time() < deadline:
            self._cleanup_s3_data(failures, deadline)

        self.resources.clear()

        if failures:
            print(f"[PERF CLEANUP] Completed with {len(failures)} issue(s) ({cleaned}/{total} cleaned)")
            warnings.warn(
                f"Performance test cleanup had {len(failures)} issue(s): "
                + "; ".join(failures[:3]),
                RuntimeWarning,
                stacklevel=2,
            )
        else:
            print(f"[PERF CLEANUP] Complete ({cleaned}/{total})")
