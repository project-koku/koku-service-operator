"""
Performance test fixtures for Cost On-Prem (FLPATH-4036).

Provides session- and function-scoped fixtures for cluster info, timing,
cleanup, authentication, and NISE data generation.  All non-fixture logic
lives in dedicated modules:

- data_classes.py  — ClusterInfo, ResourceSnapshot, TimingMetric, PerformanceResult
- helpers.py       — PerfTestConfig, PerfTimer, PerfResultCollector, parsing,
                     cluster info helpers, generate_and_upload_data, register_tracked_source
- tracker.py       — PerfCleanupTracker, PerfTestResource
- queue_helpers.py — get_celery_queue_depths, wait_for_queue_drain
- profiles.py      — PROFILES dict, NISE YAML generation
"""

import os
import time
import warnings
from datetime import datetime, timedelta, timezone
from pathlib import Path
from typing import Any, Dict, Optional

import pytest
import requests

from conftest import ClusterConfig, DatabaseConfig, obtain_jwt_token
from utils import get_pod_by_label

# Operator NetworkPolicy does not allow the ingress pod to reach koku-api.
# Exec API curls (source registration, source-type lookup) from the koku-api
# pod instead — same pattern used by suites/cost_management/conftest.py.
_KOKU_API_LABEL = "app.kubernetes.io/component=cost-management-api"
_KOKU_API_CONTAINER = "koku-api"

from .data_classes import ClusterInfo, PerformanceResult
from .helpers import (
    PERF_CONFIG,
    PerfResultCollector,
    PerfTestConfig,
    PerfTimer,
    KruizeCredentials,
    build_koku_api_url,
    create_authenticated_session,
    get_chart_version,
    get_node_info,
    get_ocp_version,
    get_profile_timeout_multiplier,
    get_s3_backend,
    get_storage_info,
    get_timeout_for_profile,
    save_perf_result,
)
from .profiles import ACTIVE_PROFILE, PROFILES
from .tracker import PerfCleanupTracker


# ---------------------------------------------------------------------------
# Re-exports for backward compatibility — test files that previously did
# ``from .conftest import X`` will continue to work while we migrate them
# to import from the canonical module.
# ---------------------------------------------------------------------------
from .data_classes import ResourceSnapshot, TimingMetric  # noqa: F401
from .helpers import (  # noqa: F401
    get_pod_resource_usage,
    parse_cpu_millicores,
    parse_memory_mib,
)
from .queue_helpers import get_celery_queue_depths, wait_for_queue_drain  # noqa: F401
from .tracker import PerfTestResource  # noqa: F401


# ---------------------------------------------------------------------------
# Test ordering — run ROS tests before ingestion tests (PERF-FINDING-013)
# ---------------------------------------------------------------------------
_PERF_FILE_ORDER = [
    "test_api_latency",
    "test_ros",
    "test_ingestion",
    "test_scale",
    "test_soak",
]


def pytest_collection_modifyitems(items: list) -> None:
    """Sort performance test items by the preferred file execution order."""

    def _sort_key(item: pytest.Item) -> tuple:
        fname = Path(item.fspath).stem
        try:
            idx = _PERF_FILE_ORDER.index(fname)
        except ValueError:
            idx = len(_PERF_FILE_ORDER)
        return (idx, item.fspath, item.name)

    items.sort(key=_sort_key)


# ---------------------------------------------------------------------------
# Session-Scoped Fixtures
# ---------------------------------------------------------------------------

@pytest.fixture(scope="session")
def cluster_info(cluster_config: ClusterConfig) -> ClusterInfo:
    """Collect cluster information for performance context."""
    node_info = get_node_info(cluster_config.namespace)
    storage_info = get_storage_info(cluster_config.namespace)

    return ClusterInfo(
        ocp_version=get_ocp_version(cluster_config.namespace),
        node_count=node_info["node_count"],
        worker_node_count=node_info["worker_count"],
        total_cpu_cores=node_info["cpu"],
        total_memory_gib=node_info["memory_gib"],
        storage_class=storage_info["storage_class"],
        storage_type=storage_info["storage_type"],
        s3_backend=get_s3_backend(cluster_config.namespace),
        platform=os.environ.get("CLUSTER_PLATFORM", "unknown"),
    )


@pytest.fixture(scope="session")
def chart_version(cluster_config: ClusterConfig) -> str:
    """Get the deployed operator version (image tag of the controller Deployment)."""
    return get_chart_version(cluster_config.namespace, cluster_config.helm_release_name)


@pytest.fixture(scope="session")
def performance_profile() -> str:
    """Get the performance profile to use (from env var or default).

    Asserts consistency with the module-level ACTIVE_PROFILE constant
    used by parametrize lists at import time.
    """
    profile = os.environ.get("PERF_PROFILE", "baseline")
    assert profile == ACTIVE_PROFILE, (
        "PERF_PROFILE changed between import and fixture creation "
        f"(import={ACTIVE_PROFILE!r}, fixture={profile!r})"
    )
    return profile


@pytest.fixture(scope="session")
def profile_config(performance_profile: str) -> Dict[str, Any]:
    """Get the configuration for the selected performance profile."""
    if performance_profile not in PROFILES:
        pytest.skip(f"Unknown performance profile: {performance_profile}")
    return PROFILES[performance_profile]


@pytest.fixture(scope="session")
def perf_reports_dir() -> Path:
    """Get the directory for performance reports.

    When called from deploy-test-cost-onprem.sh with unified output structure,
    uses PERF_OUTPUT_DIR/TEST_RUN_ID/results/ for S3 upload compatibility.
    Otherwise falls back to tests/reports/performance/ for standalone runs.
    """
    perf_output_dir = os.environ.get("PERF_OUTPUT_DIR")
    test_run_id = os.environ.get("TEST_RUN_ID")

    if perf_output_dir and test_run_id:
        reports_dir = Path(perf_output_dir) / test_run_id / "results"
    else:
        reports_dir = Path(__file__).parent.parent.parent / "reports" / "performance"

    reports_dir.mkdir(parents=True, exist_ok=True)
    return reports_dir


@pytest.fixture(scope="session")
def perf_collector(perf_reports_dir: Path):
    """Collector for aggregating performance results across a session."""
    collector = PerfResultCollector(perf_reports_dir)
    yield collector
    collector.finalize()


@pytest.fixture(scope="session")
def kruize_credentials(cluster_config: ClusterConfig) -> Optional[KruizeCredentials]:
    """Get Kruize database credentials.

    Returns None if credentials are not available (ROS not deployed).
    Tests that require ROS should skip if this returns None.
    """
    secret_name = f"{cluster_config.helm_release_name}-db-credentials"
    return KruizeCredentials.from_secret(cluster_config.namespace, secret_name)


@pytest.fixture(scope="session")
def perf_config() -> PerfTestConfig:
    """Get the centralized performance test configuration."""
    return PERF_CONFIG


@pytest.fixture(scope="session")
def koku_api_url(cluster_config: ClusterConfig) -> str:
    """Internal Koku API URL for in-cluster requests."""
    return build_koku_api_url(
        cluster_config.helm_release_name,
        cluster_config.namespace,
    )


@pytest.fixture(scope="session")
def ingress_pod(cluster_config: ClusterConfig) -> str:
    """Get the ingress pod name, skipping if not found."""
    pod = get_pod_by_label(cluster_config.namespace, "app.kubernetes.io/component=ingress")
    if not pod:
        pytest.skip("Ingress pod not found")
    return pod


@pytest.fixture(scope="session")
def koku_api_pod(cluster_config: ClusterConfig) -> str:
    """Get the koku-api pod name for in-cluster API calls.

    The operator NetworkPolicy blocks the ingress pod from reaching koku-api
    on port 8000, so source registration and source-type lookups must be exec'd
    from the koku-api pod itself.  Use this fixture (with container=_KOKU_API_CONTAINER)
    wherever register_source / get_source_type_id is called.

    See: suites/cost_management/conftest.py for the same pattern.
    """
    pod = get_pod_by_label(cluster_config.namespace, _KOKU_API_LABEL)
    if not pod:
        pytest.skip("koku-api pod not found")
    return pod


@pytest.fixture(scope="session")
def authenticated_session(keycloak_config) -> requests.Session:
    """Get a requests.Session with a fresh JWT token."""
    return create_authenticated_session(keycloak_config)


# ---------------------------------------------------------------------------
# Function-Scoped Fixtures
# ---------------------------------------------------------------------------

@pytest.fixture(scope="function")
def perf_timer():
    """Provide a PerfTimer instance for the current test."""
    return PerfTimer()


@pytest.fixture(scope="function")
def perf_result(
    request,
    cluster_info: ClusterInfo,
    chart_version: str,
    performance_profile: str,
) -> PerformanceResult:
    """Create a PerformanceResult for the current test."""
    return PerformanceResult(
        test_id=f"{request.node.nodeid}-{datetime.now(timezone.utc).strftime('%Y%m%d%H%M%S')}",
        test_name=request.node.name,
        profile=performance_profile,
        operator_version=chart_version,
        timestamp=datetime.now(timezone.utc).isoformat(),
        cluster_info=cluster_info,
    )


@pytest.fixture
def perf_cleanup(cluster_config: ClusterConfig, rh_identity_header: str):
    """Fixture that tracks and cleans up performance test resources.

    Usage::

        def test_my_perf_test(self, perf_cleanup, ...):
            source = register_source(...)
            perf_cleanup.track(source_id=source.source_id, ...)
            # Test runs...
            # Cleanup happens automatically
    """
    tracker = PerfCleanupTracker(
        namespace=cluster_config.namespace,
        helm_release=cluster_config.helm_release_name,
    )

    yield tracker

    # S3 cleanup can be slow under load (NooBaa retry backoff) and must not
    # fail an otherwise-passing run — catch ALL exceptions including
    # pytest-timeout's Failed signal.
    try:
        tracker.cleanup(rh_identity_header)
    except BaseException as exc:
        warnings.warn(
            f"Teardown cleanup failed (non-fatal): {exc}",
            RuntimeWarning,
            stacklevel=2,
        )


# ---------------------------------------------------------------------------
# Tag Enablement
# ---------------------------------------------------------------------------
# Tag enablement functions and fixture are defined in the root conftest.py
# and are available to all test suites.  The `ensure_tags_enabled` fixture
# uses the API (PUT /settings/tags/enable/) to enable tags needed for testing.
#
# Performance tests that need tags (e.g., API-006 tag filtering) should
# include `ensure_tags_enabled` in their fixture dependencies.


# ---------------------------------------------------------------------------
# Tag Test Data Fixture
# ---------------------------------------------------------------------------

@pytest.fixture(scope="function")
def labeled_nise_source(
    cluster_config: ClusterConfig,
    gateway_url: str,
    ingress_url: str,
    rh_identity_header: str,
    jwt_token,
    perf_cleanup,
    ensure_tags_enabled,
    koku_api_url: str,
    ingress_pod: str,
    koku_api_pod: str,
):
    """Create a NISE source with labeled data for tag filtering tests.

    This fixture:
    1. Registers a new source
    2. Generates and uploads NISE data with pod labels
    3. Waits for processing to complete
    4. Ensures tags are enabled
    5. Returns info about available tags
    6. Cleans up the source after test
    """
    import subprocess
    import tempfile

    from e2e_helpers import (
        generate_cluster_id,
        register_source,
        upload_with_retry,
        wait_for_provider,
        wait_for_processing_complete,
        wait_for_summary_tables,
    )
    from utils import create_upload_package_from_files, execute_db_query
    from .profiles import get_profile_nise_yaml

    namespace = cluster_config.namespace

    cluster_id = generate_cluster_id()
    source_name = f"perf-tag-{cluster_id[-8:]}"

    print(f"\n[labeled_nise_source] Creating source {source_name} with labeled data")

    source = register_source(
        namespace, koku_api_pod, koku_api_url,
        rh_identity_header, cluster_id, "org1234567", source_name,
        container=_KOKU_API_CONTAINER,
    )

    perf_cleanup.track(
        source_id=source.source_id,
        cluster_id=cluster_id,
        source_name=source_name,
    )

    # Generate NISE data with labels
    end_date = datetime.now(timezone.utc)
    start_date = end_date - timedelta(days=1)

    with tempfile.TemporaryDirectory() as temp_dir:
        yaml_content = get_profile_nise_yaml("baseline", start_date, end_date, cluster_id, 0)
        yaml_path = os.path.join(temp_dir, "static_report.yml")
        with open(yaml_path, "w") as f:
            f.write(yaml_content)

        nise_output = os.path.join(temp_dir, "nise_output")
        os.makedirs(nise_output, exist_ok=True)

        result = subprocess.run(
            ["nise", "report", "ocp",
             "--static-report-file", yaml_path,
             "--ocp-cluster-id", cluster_id, "-w"],
            capture_output=True, text=True, timeout=300, cwd=nise_output,
        )

        if result.returncode != 0:
            raise RuntimeError(f"NISE failed: {result.stderr}")

        csv_files = list(Path(nise_output).rglob("*.csv"))
        pod_usage_files = [str(f) for f in csv_files if "pod_usage" in f.name.lower()]
        node_label_files = [str(f) for f in csv_files if "node_label" in f.name.lower()]
        namespace_label_files = [str(f) for f in csv_files if "namespace_label" in f.name.lower()]

        print(f"[labeled_nise_source] Generated {len(csv_files)} CSV files, {len(pod_usage_files)} with pod data")

        package_path = create_upload_package_from_files(
            pod_usage_files=pod_usage_files,
            ros_usage_files=[],
            cluster_id=cluster_id,
            start_date=start_date,
            end_date=end_date,
            node_label_files=node_label_files if node_label_files else None,
            namespace_label_files=namespace_label_files if namespace_label_files else None,
        )

        session = requests.Session()
        session.verify = False

        response = upload_with_retry(
            session,
            f"{ingress_url}/v1/upload",
            package_path,
            jwt_token.authorization_header,
            max_retries=3,
        )

        print(f"[labeled_nise_source] Upload status: {response.status_code}")

    # Wait for processing
    db_pod = get_pod_by_label(namespace, "app.kubernetes.io/component=database")
    if not db_pod:
        pytest.skip("Database pod not found — cannot verify tag processing")

    print("[labeled_nise_source] Waiting for provider to be created...")
    wait_for_provider(namespace, db_pod, cluster_id, timeout=120)

    print("[labeled_nise_source] Waiting for manifest processing...")
    _profile = os.environ.get("PERF_PROFILE", "baseline")
    proc_deadline = time.time() + get_timeout_for_profile(300, _profile)
    proc_result = {"complete": False}

    while time.time() < proc_deadline:
        proc_result = wait_for_processing_complete(
            namespace, db_pod, cluster_id,
            poll_interval=10,
            max_wait_seconds=max(15, int(proc_deadline - time.time())),
        )
        if proc_result.get("complete"):
            processed = proc_result.get("num_processed_files", 0)
            if processed > 0:
                break
            print("[labeled_nise_source] Stale manifest (0 files processed) — waiting for fresh manifest...")
            time.sleep(15)
        else:
            break

    print(
        f"[labeled_nise_source] Processing: complete={proc_result.get('complete')}, "
        f"files={proc_result.get('num_processed_files', 0)}, "
        f"elapsed={proc_result.get('elapsed_s', '?')}s"
    )

    # Confirm summary table rows with pod_labels
    print("[labeled_nise_source] Waiting for summary table rows with pod_labels...")
    schema_name = proc_result.get("schema_name")

    if not schema_name:
        schema_name = wait_for_summary_tables(
            namespace, db_pod, cluster_id, timeout=300, interval=10,
        )

    if schema_name:
        label_wait_start = time.time()
        label_wait_max = get_timeout_for_profile(300, _profile)
        found_labels = False

        while time.time() - label_wait_start < label_wait_max:
            count_rows = execute_db_query(
                namespace, db_pod, "costonprem_koku", "koku_user",
                f"""
                SELECT COUNT(*)
                FROM   {schema_name}.reporting_ocpusagelineitem_daily_summary
                WHERE  cluster_id = '{cluster_id}'
                  AND  pod_labels IS NOT NULL
                  AND  pod_labels != '{{}}'::jsonb
                """,
            )
            count = int(count_rows[0][0]) if count_rows and count_rows[0] else 0
            elapsed = round(time.time() - label_wait_start, 1)
            if count > 0:
                print(f"[labeled_nise_source] Found {count} summary rows with pod_labels in {elapsed}s")
                found_labels = True
                break
            if int(elapsed) % 60 < 16:
                any_rows = execute_db_query(
                    namespace, db_pod, "costonprem_koku", "koku_user",
                    f"""
                    SELECT COUNT(*), COUNT(NULLIF(pod_labels::text, '{{}}'))
                    FROM   {schema_name}.reporting_ocpusagelineitem_daily_summary
                    WHERE  cluster_id = '{cluster_id}'
                    """,
                )
                total = int(any_rows[0][0]) if any_rows and any_rows[0] else 0
                with_labels = int(any_rows[0][1]) if any_rows and len(any_rows[0]) > 1 else 0
                print(
                    f"[labeled_nise_source] {elapsed}s — cluster {cluster_id[:8]}: "
                    f"{total} total summary rows, {with_labels} with pod_labels"
                )
            else:
                print(
                    f"[labeled_nise_source] {elapsed}s — no summary rows with pod_labels yet "
                    f"for {cluster_id[:8]}..."
                )
            time.sleep(15)

        if not found_labels:
            diag_rows = execute_db_query(
                namespace, db_pod, "costonprem_koku", "koku_user",
                f"""
                SELECT COUNT(*) AS total,
                       COUNT(NULLIF(pod_labels::text, '{{}}')) AS with_labels
                FROM   {schema_name}.reporting_ocpusagelineitem_daily_summary
                WHERE  cluster_id = '{cluster_id}'
                """,
            )
            total = int(diag_rows[0][0]) if diag_rows and diag_rows[0] else 0
            with_labels = int(diag_rows[0][1]) if diag_rows and len(diag_rows[0]) > 1 else 0
            pytest.fail(
                f"Summary table has no rows with pod_labels for cluster {cluster_id} "
                f"after {label_wait_max}s ({total} total rows, {with_labels} with labels). "
                f"Data uploaded (HTTP 202) but labels not summarized. "
                f"Check celery-worker-summary and listener logs."
            )
    else:
        pytest.fail(
            f"Summary table never populated for cluster {cluster_id}. "
            "Summarization may not have run — check celery-worker-summary logs."
        )
    print(f"[labeled_nise_source] Summary rows confirmed in schema: {schema_name}")

    # Query tags from summary table
    db_tag_rows = execute_db_query(
        namespace, db_pod, "costonprem_koku", "koku_user",
        f"""
        SELECT DISTINCT k
        FROM   {schema_name}.reporting_ocpusagelineitem_daily_summary,
               LATERAL jsonb_object_keys(pod_labels) AS k
        WHERE  cluster_id = '{cluster_id}'
          AND  pod_labels IS NOT NULL
          AND  pod_labels != '{{}}'::jsonb
        ORDER  BY k
        """,
    )
    db_tag_keys = [row[0] for row in db_tag_rows] if db_tag_rows else []
    print(f"[labeled_nise_source] Tags in summary table pod_labels: {db_tag_keys}")

    nise_tag_keys = [
        "tagone", "tagtwo", "tagthree", "tagfour", "tagfive",
        "tagsix", "tagseven", "tageight", "tagnine", "tagten",
    ]
    db_hits = [k for k in nise_tag_keys if k in db_tag_keys]
    db_misses = [k for k in nise_tag_keys if k not in db_tag_keys]
    if db_misses:
        print(
            f"[labeled_nise_source] WARNING: {len(db_misses)} NISE labels missing from DB: "
            f"{db_misses}"
        )

    ns_label_rows = execute_db_query(
        namespace, db_pod, "costonprem_koku", "koku_user",
        f"""
        SELECT DISTINCT k
        FROM   {schema_name}.reporting_ocpusagelineitem_daily_summary,
               LATERAL jsonb_object_keys(namespace_labels) AS k
        WHERE  cluster_id = '{cluster_id}'
          AND  namespace_labels IS NOT NULL
          AND  namespace_labels != '{{}}'::jsonb
        ORDER  BY k
        """,
    )
    ns_tag_keys = [row[0] for row in ns_label_rows] if ns_label_rows else []
    print(f"[labeled_nise_source] Tags in namespace_labels: {ns_tag_keys}")

    node_label_rows = execute_db_query(
        namespace, db_pod, "costonprem_koku", "koku_user",
        f"""
        SELECT DISTINCT k
        FROM   {schema_name}.reporting_ocpnode_label_summary,
               LATERAL jsonb_object_keys(node_labels) AS k
        WHERE  cluster_id = '{cluster_id}'
          AND  node_labels IS NOT NULL
          AND  node_labels != '{{}}'::jsonb
        ORDER  BY k
        """,
    )
    node_tag_keys = [row[0] for row in node_label_rows] if node_label_rows else []
    if not node_tag_keys:
        node_label_rows = execute_db_query(
            namespace, db_pod, "costonprem_koku", "koku_user",
            f"""
            SELECT DISTINCT k
            FROM   {schema_name}.reporting_ocpusagelineitem_daily_summary,
                   LATERAL jsonb_object_keys(node_labels) AS k
            WHERE  cluster_id = '{cluster_id}'
              AND  node_labels IS NOT NULL
              AND  node_labels != '{{}}'::jsonb
            ORDER  BY k
            """,
        )
        node_tag_keys = [row[0] for row in node_label_rows] if node_label_rows else []
    print(f"[labeled_nise_source] Tags in node_labels: {node_tag_keys}")

    all_db_tags = sorted(set(db_tag_keys + ns_tag_keys + node_tag_keys))
    print(f"[labeled_nise_source] Total unique tags in DB: {len(all_db_tags)} — {all_db_tags}")

    # Enable discovered tags via the API
    from conftest import enable_tags_via_api
    auth_header = {"Authorization": f"Bearer {jwt_token.access_token}"}

    all_expected_keys = nise_tag_keys + ["nslabel1", "nslabel2", "nodelabel1", "nodelabel2", "nodelabel3"]
    tags_to_enable = [k for k in all_expected_keys if k in all_db_tags]
    if not tags_to_enable:
        tags_to_enable = all_db_tags[:15]
        print(
            f"[labeled_nise_source] No expected NISE tags in DB — using discovered DB tags: "
            f"{tags_to_enable}"
        )

    max_tag_wait = 120
    poll_interval = 10
    tag_wait_start = time.time()

    print(f"[labeled_nise_source] Enabling {len(tags_to_enable)} tags via API (max {max_tag_wait}s)...")

    while time.time() - tag_wait_start < max_tag_wait:
        enable_result = enable_tags_via_api(gateway_url, auth_header, tags_to_enable)
        enabled = enable_result.get('enabled', [])
        already = enable_result.get('already_enabled', [])
        not_found = enable_result.get('not_found', [])

        elapsed = round(time.time() - tag_wait_start, 1)
        print(
            f"[labeled_nise_source] {elapsed}s — enabled={len(enabled)}, "
            f"already={len(already)}, not_found={len(not_found)}"
        )

        if not not_found:
            print(f"[labeled_nise_source] All tags found in API in {elapsed}s")
            break

        time.sleep(poll_interval)
    else:
        print(f"[labeled_nise_source] Warning: tag enablement timed out after {max_tag_wait}s")

    # Final query for available tags from the API
    tag_session = requests.Session()
    tag_session.verify = False
    tag_session.headers["Authorization"] = f"Bearer {jwt_token.access_token}"

    tags_response = tag_session.get(
        f"{gateway_url}/cost-management/v1/tags/openshift/",
        timeout=30,
    )

    available_tags = []
    if tags_response.status_code == 200:
        tag_data = tags_response.json().get("data", [])
        for entry in tag_data:
            if isinstance(entry, dict) and "key" in entry:
                available_tags.append(entry["key"])
            elif isinstance(entry, str):
                available_tags.append(entry)

    print(f"[labeled_nise_source] Available tags from API: {available_tags}")

    yield {
        "source_id": source.source_id,
        "source_name": source_name,
        "cluster_id": cluster_id,
        "available_tags": available_tags,
        "db_tags": all_db_tags,
        "expected_labels": nise_tag_keys,
    }

    # Cleanup handled by perf_cleanup fixture
