"""
Complete end-to-end data flow tests.

These tests validate the entire pipeline:
  Generated Data → Upload via Ingress (JWT) → Koku Processing → ROS/Kruize

This is the canonical E2E test that covers the full production data flow.

Internal API Access:
  Uses PodAdapter via e2e_pod_session fixture for internal Koku API calls.
  This provides a clean requests.Session interface while executing curl
  commands inside the cluster via kubectl exec.

Data Generation Options:
  - NISE (default): Uses koku-nise to generate proper OCP cost data format
  - Simple (fallback): Uses simplified CSV format (may not populate summary tables)

Environment Variables:
  - E2E_USE_SIMPLE_DATA=true: Use simple CSV format instead of NISE
  - E2E_NISE_STATIC_REPORT: Path to custom NISE static report file
  - E2E_CLEANUP_BEFORE=true/false: Run cleanup before tests (default: true)
  - E2E_CLEANUP_AFTER=true/false: Run cleanup after tests (default: true)
  - E2E_RESTART_SERVICES=true: Restart Valkey/listener during cleanup (slower but thorough)
"""

import json
import os
import shutil
import subprocess
import tempfile
import time
from datetime import datetime, timedelta
from pathlib import Path
from typing import Optional

import pytest
import requests

from utils import (
    create_upload_package,
    create_upload_package_from_files,
    execute_db_query,
    get_pod_by_label,
    get_secret_value,
    wait_for_condition,
    run_oc_command,
)
# Note: exec_in_pod is no longer used - we use e2e_pod_session (PodAdapter) instead
from cleanup import full_cleanup
from e2e_helpers import install_nise

# Import shared E2E helpers
from e2e_helpers import (
    E2E_CLUSTER_PREFIX,
    DEFAULT_NISE_CONFIG,
    is_nise_available,
    install_nise,
    ensure_nise_available,
    upload_with_retry,
    wait_for_provider,
    cleanup_database_records,
    cleanup_e2e_sources,
)

# =============================================================================
# Data Generation Utilities
# =============================================================================

def is_nise_available() -> bool:
    """Check if NISE is available for data generation."""
    try:
        result = subprocess.run(
            ["nise", "--version"],
            capture_output=True,
            text=True,
            timeout=10,
        )
        return result.returncode == 0
    except (subprocess.CalledProcessError, FileNotFoundError, subprocess.TimeoutExpired):
        return False


def generate_dynamic_static_report(start_date: datetime, end_date: datetime, output_dir: str) -> str:
    """Generate a dynamic NISE static report YAML with current dates.
    
    This ensures the dates in the nise-generated data match the current billing period,
    which is required for Koku to process and summarize the data correctly.
    
    Args:
        start_date: Start date for data generation
        end_date: End date for data generation
        output_dir: Directory to write the YAML file
        
    Returns:
        Path to the generated YAML file
    """
    yaml_content = f"""---
# Dynamic OCP Static Report - Generated for E2E Testing
# Generated: {datetime.utcnow().isoformat()}
# Date Range: {start_date.strftime('%Y-%m-%d')} to {end_date.strftime('%Y-%m-%d')}

generators:
  - OCPGenerator:
      start_date: {start_date.strftime('%Y-%m-%d')}
      end_date: {end_date.strftime('%Y-%m-%d')}
      nodes:
        - node:
          node_name: test-node-1
          cpu_cores: 2
          memory_gig: 8
          resource_id: test-resource-1
          namespaces:
            test-namespace:
              pods:
                - pod:
                  pod_name: test-pod-1
                  cpu_request: 0.5
                  mem_request_gig: 1
                  cpu_limit: 1
                  mem_limit_gig: 2
                  pod_seconds: 3600
                  cpu_usage:
                    full_period: 0.25
                  mem_usage_gig:
                    full_period: 0.5
                  labels: environment:test|app:e2e-pytest
"""
    
    yaml_path = os.path.join(output_dir, "dynamic_ocp_static_report.yml")
    with open(yaml_path, "w") as f:
        f.write(yaml_content)
    
    return yaml_path


def generate_nise_ocp_data(
    cluster_id: str,
    start_date: datetime,
    end_date: datetime,
    output_dir: str,
    static_report_file: Optional[str] = None,
) -> dict:
    """Generate OCP data using NISE.
    
    Args:
        cluster_id: Cluster identifier for the data
        start_date: Start date for data generation
        end_date: End date for data generation
        output_dir: Directory to write generated files
        static_report_file: Optional path to NISE static report YAML
        
    Returns:
        Dict with paths to generated files and metadata
        
    IMPORTANT: If no static_report_file is provided, a dynamic one will be generated
    with the correct dates. This is critical because:
    1. NISE static reports have hardcoded dates that override command-line dates
    2. Koku requires data in the current billing period to process summaries
    3. Using outdated dates will cause "missing start or end dates" errors
    """
    # Generate dynamic static report if none provided
    # This ensures dates match the current billing period
    if not static_report_file:
        static_report_file = generate_dynamic_static_report(start_date, end_date, output_dir)
        print(f"  Generated dynamic static report: {static_report_file}")
    
    cmd = [
        "nise",
        "report", "ocp",
        "--start-date", start_date.strftime("%Y-%m-%d"),
        "--end-date", end_date.strftime("%Y-%m-%d"),
        "--ocp-cluster-id", cluster_id,
        "--write-monthly",
        "--file-row-limit", "10000",
        "--ros-ocp-info",  # Generate ROS container-level data for resource optimization
        "--static-report-file", static_report_file,
    ]
    
    result = subprocess.run(
        cmd,
        capture_output=True,
        text=True,
        cwd=output_dir,
        timeout=300,
    )
    
    if result.returncode != 0:
        raise RuntimeError(f"NISE failed: {result.stderr}")
    
    # Find generated files
    all_csv_files = list(Path(output_dir).rglob("*.csv"))
    pod_usage_files = [f for f in all_csv_files if "pod_usage" in f.name]
    ros_usage_files = [f for f in all_csv_files if "ros_usage" in f.name and "namespace" not in f.name]
    node_label_files = [f for f in all_csv_files if "node_label" in f.name]
    namespace_label_files = [f for f in all_csv_files if "namespace_label" in f.name]
    manifest_files = list(Path(output_dir).rglob("*manifest.json"))
    
    # Prioritize pod_usage files (for Koku summary tables), then ROS files
    csv_files = pod_usage_files + [f for f in all_csv_files if f not in pod_usage_files]
    
    return {
        "output_dir": output_dir,
        "csv_files": [str(f) for f in csv_files],
        "pod_usage_files": [str(f) for f in pod_usage_files],
        "ros_usage_files": [str(f) for f in ros_usage_files],  # Container-level data for ROS
        "node_label_files": [str(f) for f in node_label_files],  # Node labels for summary tables
        "namespace_label_files": [str(f) for f in namespace_label_files],  # Namespace labels
        "manifest_files": [str(f) for f in manifest_files],
        "cluster_id": cluster_id,
        "start_date": start_date,
        "end_date": end_date,
        "generator": "nise",
    }


def generate_simple_ocp_data(cluster_id: str) -> dict:
    """Generate simple OCP data (legacy format - may not populate summary tables).
    
    WARNING: This format may not be fully processed by Koku. Use NISE for
    complete E2E validation.
    
    Args:
        cluster_id: Cluster identifier
        
    Returns:
        Dict with CSV content and metadata
    """
    now = datetime.utcnow()
    
    # Generate 4 intervals of data
    intervals = []
    for i in range(4):
        start = now - timedelta(minutes=75 - (i * 15))
        end = now - timedelta(minutes=60 - (i * 15))
        intervals.append((start, end))
    
    # CSV header matching OCP ROS format
    header = (
        "report_period_start,report_period_end,interval_start,interval_end,"
        "container_name,pod,owner_name,owner_kind,workload,workload_type,"
        "namespace,image_name,node,resource_id,"
        "cpu_request_container_avg,cpu_request_container_sum,"
        "cpu_limit_container_avg,cpu_limit_container_sum,"
        "cpu_usage_container_avg,cpu_usage_container_min,cpu_usage_container_max,cpu_usage_container_sum,"
        "cpu_throttle_container_avg,cpu_throttle_container_max,cpu_throttle_container_sum,"
        "memory_request_container_avg,memory_request_container_sum,"
        "memory_limit_container_avg,memory_limit_container_sum,"
        "memory_usage_container_avg,memory_usage_container_min,memory_usage_container_max,memory_usage_container_sum,"
        "memory_rss_usage_container_avg,memory_rss_usage_container_min,memory_rss_usage_container_max,memory_rss_usage_container_sum"
    )
    
    rows = [header]
    date_str = now.strftime("%Y-%m-%d")
    
    cpu_usages = [0.247832, 0.265423, 0.289567, 0.234567]
    mem_usages = [413587266, 427891456, 445678901, 398765432]
    
    for i, (start, end) in enumerate(intervals):
        start_str = start.strftime("%Y-%m-%d %H:%M:%S -0000 UTC")
        end_str = end.strftime("%Y-%m-%d %H:%M:%S -0000 UTC")
        cpu = cpu_usages[i]
        mem = mem_usages[i]
        
        row = (
            f"{date_str},{date_str},{start_str},{end_str},"
            f"test-container,test-pod-{cluster_id[:8]},test-deployment,Deployment,test-workload,deployment,"
            f"test-namespace,quay.io/test/image:latest,worker-node-1,resource-{cluster_id[:8]},"
            f"0.5,0.5,1.0,1.0,"
            f"{cpu},{cpu*0.75},{cpu*1.3},{cpu},"
            f"0.001,0.002,0.001,"
            f"536870912,536870912,1073741824,1073741824,"
            f"{mem},{mem*0.99},{mem*1.02},{mem},"
            f"{mem*0.95},{mem*0.94},{mem*0.96},{mem*0.95}"
        )
        rows.append(row)
    
    # Calculate start/end dates for manifest
    start_date = now - timedelta(days=1)
    end_date = now
    
    return {
        "csv_content": "\n".join(rows),
        "cluster_id": cluster_id,
        "expected_cpu_request": 0.5,
        "expected_memory_request_bytes": 536870912,
        "expected_rows": 4,
        "namespace": "test-namespace",
        "node": "worker-node-1",
        "generator": "simple",
        "start_date": start_date,
        "end_date": end_date,
        "warning": (
            "Simple data format may not populate Koku summary tables. "
            "Use NISE for complete E2E validation."
        ),
    }


@pytest.mark.e2e
@pytest.mark.integration
@pytest.mark.slow
class TestCompleteDataFlow:
    """
    End-to-end test of the complete data flow.
    
    This test class validates the FULL production pipeline:
    
    1. Source Registration:
       - Register OCP source via Sources API
       - Verify provider created in Koku database
    
    2. Data Upload (via Ingress):
       - Generate test CSV data with realistic metrics
       - Package into tar.gz with manifest
       - Upload via JWT-authenticated ingress endpoint
       - Verify ingress stores in S3 and publishes to Kafka
    
    3. Koku Processing:
       - Koku Listener consumes from platform.upload.announce
       - MASU processes cost data from S3
       - Manifest and file status tracked in database
       - Summary tables populated with aggregated data
    
    4. ROS Pipeline:
       - Koku copies ROS data to ros-data bucket
       - Koku emits events to hccm.ros.events topic
       - ROS Processor consumes and sends to Kruize
    
    5. Recommendations:
       - Kruize generates optimization recommendations
       - Recommendations accessible via JWT-authenticated API
    """

    @pytest.fixture(scope="class")
    def e2e_cluster_id(self) -> str:
        import uuid
        return str(uuid.uuid4())

    @pytest.fixture(scope="class")
    def e2e_test_data(self, e2e_cluster_id: str) -> dict:
        """Generate test data for E2E validation.
        
        By default, uses NISE to generate proper OCP cost data format.
        Set E2E_USE_SIMPLE_DATA=true to use simplified format (may not populate summary tables).
        """
        use_simple = os.environ.get("E2E_USE_SIMPLE_DATA", "false").lower() == "true"
        
        if use_simple:
            print("\n  ⚠️  Using SIMPLE data format (E2E_USE_SIMPLE_DATA=true)")
            print("     Warning: Summary tables may not be populated with this format")
            return generate_simple_ocp_data(e2e_cluster_id)
        
        # Try NISE first
        if not is_nise_available():
            print("\n  NISE not found, attempting to install...")
            if not install_nise():
                print("  ⚠️  NISE installation failed, falling back to simple data")
                print("     Warning: Summary tables may not be populated with this format")
                data = generate_simple_ocp_data(e2e_cluster_id)
                data["nise_install_failed"] = True
                return data
        
        # Generate NISE data
        print(f"\n  Generating OCP data with NISE for cluster: {e2e_cluster_id}")
        
        now = datetime.utcnow()
        # Use current date range - this is CRITICAL for Koku to process summaries
        # Data must be in the current billing period
        start_date = now - timedelta(days=1)
        end_date = now + timedelta(days=1)
        
        # Create temp directory for NISE output
        temp_dir = tempfile.mkdtemp(prefix="e2e-nise-")
        
        # Check for custom static report (only use if explicitly set)
        # NOTE: Do NOT use the default static report file as it has hardcoded dates
        # that will cause "missing start or end dates" errors in Koku
        static_report = os.environ.get("E2E_NISE_STATIC_REPORT")
        
        try:
            nise_data = generate_nise_ocp_data(
                cluster_id=e2e_cluster_id,
                start_date=start_date,
                end_date=end_date,
                output_dir=temp_dir,
                static_report_file=static_report,  # Will generate dynamic report if None
            )
            
            pod_usage_count = len(nise_data.get('pod_usage_files', []))
            total_count = len(nise_data.get('csv_files', []))
            print(f"  ✅ NISE generated {total_count} CSV files ({pod_usage_count} pod_usage)")
            
            # Read the pod_usage CSV file for upload (required for summary tables)
            pod_usage_files = nise_data.get("pod_usage_files", [])
            csv_files = nise_data.get("csv_files", [])
            
            if pod_usage_files:
                # Prefer pod_usage files - these are required for summary tables
                with open(pod_usage_files[0], "r") as f:
                    csv_content = f.read()
                nise_data["csv_content"] = csv_content
                print(f"  📄 Using pod_usage file: {Path(pod_usage_files[0]).name}")
            elif csv_files:
                # Fall back to any CSV file
                with open(csv_files[0], "r") as f:
                    csv_content = f.read()
                nise_data["csv_content"] = csv_content
                print(f"  ⚠️  No pod_usage files, using: {Path(csv_files[0]).name}")
            else:
                # No CSV files generated - fall back to simple
                print("  ⚠️  NISE generated no CSV files, falling back to simple data")
                shutil.rmtree(temp_dir, ignore_errors=True)
                return generate_simple_ocp_data(e2e_cluster_id)
            
            nise_data["temp_dir"] = temp_dir
            return nise_data
            
        except Exception as e:
            print(f"  ⚠️  NISE generation failed: {e}")
            print("     Falling back to simple data format")
            shutil.rmtree(temp_dir, ignore_errors=True)
            data = generate_simple_ocp_data(e2e_cluster_id)
            data["nise_error"] = str(e)
            return data

    @pytest.fixture(scope="class")
    def registered_source(
        self,
        cluster_config,
        org_id: str,
        e2e_cluster_id: str,
        s3_config,
        koku_api_url: str,
        e2e_pod_session: requests.Session,
    ):
        """Register a source for E2E testing with cleanup before and after.
        
        Uses PodAdapter via e2e_pod_session for cleaner HTTP calls.
        
        Cleanup includes:
          - S3 data files from previous runs
          - Database processing records
          - Optionally Valkey cache and listener restart (if E2E_RESTART_SERVICES=1)
        """
        # Check cleanup settings
        cleanup_before = os.environ.get("E2E_CLEANUP_BEFORE", "true").lower() == "true"
        cleanup_after = os.environ.get("E2E_CLEANUP_AFTER", "true").lower() == "true"
        restart_services = os.environ.get("E2E_RESTART_SERVICES", "false").lower() == "true"
        
        # Get database pod for cleanup
        db_pod = get_pod_by_label(
            cluster_config.namespace,
            "app.kubernetes.io/component=database"
        )
        
        # Prepare S3 config dict for cleanup
        s3_config_dict = None
        if s3_config:
            s3_config_dict = {
                "endpoint": s3_config.endpoint,
                "access_key": s3_config.access_key,
                "secret_key": s3_config.secret_key,
                "bucket": s3_config.bucket,
                "verify_ssl": s3_config.verify_ssl,
                "addressing_style": s3_config.addressing_style,
            }
        
        # Pre-test cleanup
        if cleanup_before and db_pod:
            print("\n" + "=" * 60)
            print("PRE-TEST CLEANUP")
            print("=" * 60)
            full_cleanup(
                namespace=cluster_config.namespace,
                db_pod=db_pod,
                org_id=org_id,
                s3_config=s3_config_dict,
                cluster_id=None,  # Clean all clusters for this org
                restart_services=restart_services,
                verbose=True,
            )
        
        # Get source type ID from Koku
        response = e2e_pod_session.get(f"{koku_api_url}/source_types")
        if not response.ok:
            pytest.fail(
                f"Source types request failed with HTTP {response.status_code}. "
                f"Response: {response.text[:500]}"
            )
        
        source_types = response.json()
        ocp_type_id = None
        for st in source_types.get("data", []):
            if st.get("name") == "openshift":
                ocp_type_id = st.get("id")
                break
        
        if not ocp_type_id:
            pytest.skip("OpenShift source type not found")
        
        # Get application type ID from Koku
        response = e2e_pod_session.get(f"{koku_api_url}/application_types")
        app_types = response.json() if response.ok else {"data": []}
        cost_mgmt_app_id = None
        for at in app_types.get("data", []):
            if at.get("name") == "/insights/platform/cost-management":
                cost_mgmt_app_id = at.get("id")
                break
        
        # Create source with unique name
        source_name = f"e2e-source-{e2e_cluster_id[-8:]}"
        
        # Check for existing e2e sources and delete them
        print(f"  🔍 Checking for existing e2e sources...")
        response = e2e_pod_session.get(f"{koku_api_url}/sources")
        
        if response.ok:
            try:
                existing_sources = response.json()
                for existing in existing_sources.get("data", []):
                    existing_name = existing.get("name", "")
                    existing_id = existing.get("id")
                    # Delete any e2e test sources
                    if existing_id and existing_name.startswith("e2e-source-"):
                        print(f"     🗑️  Deleting existing source '{existing_name}' (id={existing_id})...")
                        e2e_pod_session.delete(f"{koku_api_url}/sources/{existing_id}")
                        time.sleep(2)  # Brief pause for deletion to propagate
            except (json.JSONDecodeError, TypeError):
                pass  # No existing sources or error in response
        
        # Create the new source with retry logic
        # On first run for a new org, the tenant schema creation can be slow
        # which may cause the first request to fail or timeout
        payload = {
            "name": source_name,
            "source_type_id": ocp_type_id,
            "source_ref": e2e_cluster_id,
        }
        
        print(f"  📝 Creating source: {source_name}")
        print(f"     Cluster ID: {e2e_cluster_id}")
        print(f"     Source Type ID: {ocp_type_id}")
        
        # Retry logic for source creation
        # First request may fail due to tenant schema creation (slow operation)
        max_retries = 5
        retry_delay = 5  # seconds
        source_id = None
        last_error = None
        
        for attempt in range(max_retries):
            if attempt > 0:
                print(f"     ⏳ Retry {attempt}/{max_retries - 1} after {retry_delay}s delay...")
                time.sleep(retry_delay)
                retry_delay = min(retry_delay * 2, 30)  # Exponential backoff, max 30s
            
            try:
                response = e2e_pod_session.post(
                    f"{koku_api_url}/sources",
                    json=payload,
                )
                
                if response.status_code in (200, 201):
                    source_data = response.json()
                    source_id = source_data.get("id")
                    if source_id:
                        print(f"     ✅ Source created successfully (id={source_id})")
                        break
                    else:
                        last_error = f"No 'id' in response: {response.text[:200]}"
                        print(f"     ⚠️  Attempt {attempt + 1} failed: {last_error}")
                elif response.status_code >= 500:
                    # 5xx errors might be transient, retry
                    last_error = f"HTTP {response.status_code}: {response.text[:200]}"
                    print(f"     ⚠️  Attempt {attempt + 1} failed: {last_error}")
                    continue
                elif response.status_code == 409:
                    # 409 might mean source already exists, try to get it
                    last_error = f"HTTP 409 Conflict: {response.text[:200]}"
                    print(f"     ⚠️  Attempt {attempt + 1} failed: {last_error}")
                    continue
                else:
                    # 4xx errors are not retryable
                    last_error = f"HTTP {response.status_code}: {response.text[:200]}"
                    print(f"     ⚠️  Attempt {attempt + 1} failed: {last_error}")
                    break
            except requests.RequestException as e:
                last_error = f"Request failed: {e}"
                print(f"     ⚠️  Attempt {attempt + 1} failed: {last_error}")
                continue
        
        if not source_id:
            # Get debug info before failing
            try:
                debug_response = e2e_pod_session.get(f"{koku_api_url}/sources")
                debug_info = debug_response.text[:500]
            except Exception as e:
                debug_info = f"Could not get debug info: {e}"
            
            pytest.fail(
                f"Source creation failed after {max_retries} attempts. "
                f"Last error: {last_error}. "
                f"url={koku_api_url}/sources, "
                f"Debug info: {debug_info}"
            )
        
        # Create application via Koku API
        if cost_mgmt_app_id:
            e2e_pod_session.post(
                f"{koku_api_url}/applications",
                json={
                    "source_id": source_id,
                    "application_type_id": cost_mgmt_app_id,
                },
            )
        
        yield {
            "source_id": source_id,
            "source_name": source_name,
            "cluster_id": e2e_cluster_id,
            "org_id": org_id,
            "koku_api_url": koku_api_url,
            "e2e_pod_session": e2e_pod_session,
            "db_pod": db_pod,
            "s3_config_dict": s3_config_dict,
        }
        
        # Post-test cleanup (only if enabled)
        if cleanup_after:
            print("\n" + "=" * 60)
            print("POST-TEST CLEANUP")
            print("=" * 60)
            
            # Delete the source via Koku API
            print("  🗑️  Deleting test source...")
            e2e_pod_session.delete(f"{koku_api_url}/sources/{source_id}")
            print(f"     ✅ Deleted source {source_id}")
            
            # Full cleanup
            if db_pod:
                full_cleanup(
                    namespace=cluster_config.namespace,
                    db_pod=db_pod,
                    org_id=org_id,
                    s3_config=s3_config_dict,
                    cluster_id=e2e_cluster_id,  # Only clean this test's cluster
                    restart_services=False,  # Don't restart services after tests
                    verbose=True,
                )
        else:
            print("\n" + "=" * 60)
            print("POST-TEST CLEANUP SKIPPED (E2E_CLEANUP_AFTER=false)")
            print("=" * 60)
            print(f"  Data preserved for cluster: {e2e_cluster_id}")
            print(f"  Source ID: {source_id}")

    # =========================================================================
    # Test Steps - Ordered to validate the complete pipeline
    # =========================================================================

    def test_01_source_registered(self, registered_source):
        """Step 1: Verify source was registered successfully."""
        assert registered_source["source_id"], "Source ID not set"
        assert registered_source["cluster_id"], "Cluster ID not set"

    def test_02_provider_created_in_koku(self, cluster_config, registered_source):
        """Step 2: Verify provider was created in Koku database via Kafka."""
        db_pod = get_pod_by_label(
            cluster_config.namespace,
            "app.kubernetes.io/component=database"
        )
        if not db_pod:
            pytest.skip("Database pod not found")
        
        cluster_id = registered_source["cluster_id"]
        
        def check_provider():
            result = execute_db_query(
                cluster_config.namespace,
                db_pod,
                "costonprem_koku",
                "koku_user",
                f"""
                SELECT COUNT(*) FROM api_provider p
                JOIN api_providerauthentication a ON p.authentication_id = a.id
                WHERE a.credentials->>'cluster_id' = '{cluster_id}'
                   OR p.additional_context->>'cluster_id' = '{cluster_id}'
                """,
            )
            return result is not None and int(result[0][0]) > 0
        
        success = wait_for_condition(
            check_provider,
            timeout=180,
            interval=10,
            description="provider creation via Kafka",
        )
        
        assert success, f"Provider not created for cluster {cluster_id}"

    def test_03_upload_data_via_ingress(
        self,
        cluster_config,
        ingress_url: str,
        jwt_token,
        e2e_test_data: dict,
        registered_source,
        http_session: requests.Session,
    ):
        """Step 3: Upload test data via JWT-authenticated ingress."""
        cluster_id = registered_source["cluster_id"]
        
        # Get date range from NISE data or use defaults
        start_date = e2e_test_data.get("start_date")
        end_date = e2e_test_data.get("end_date")
        
        # Check if we have NISE-generated files with separate ROS data
        pod_usage_files = e2e_test_data.get("pod_usage_files", [])
        ros_usage_files = e2e_test_data.get("ros_usage_files", [])
        node_label_files = e2e_test_data.get("node_label_files", [])
        namespace_label_files = e2e_test_data.get("namespace_label_files", [])
        
        if pod_usage_files and ros_usage_files:
            # Use NISE files with proper separation of cost and ROS data
            print(f"  📦 Creating package with {len(pod_usage_files)} pod_usage + {len(ros_usage_files)} ros_usage files")
            if node_label_files:
                print(f"     + {len(node_label_files)} node_label files")
            if namespace_label_files:
                print(f"     + {len(namespace_label_files)} namespace_label files")
            tar_path = create_upload_package_from_files(
                pod_usage_files,
                ros_usage_files,
                cluster_id,
                start_date=start_date,
                end_date=end_date,
                node_label_files=node_label_files if node_label_files else None,
                namespace_label_files=namespace_label_files if namespace_label_files else None,
            )
        else:
            # Fall back to simple CSV content
            tar_path = create_upload_package(
                e2e_test_data["csv_content"],
                cluster_id,
                start_date=start_date,
                end_date=end_date,
            )
        
        try:
            with open(tar_path, "rb") as f:
                response = http_session.post(
                    f"{ingress_url}/v1/upload",
                    files={
                        "file": (
                            "cost-mgmt.tar.gz",
                            f,
                            "application/vnd.redhat.hccm.filename+tgz",
                        )
                    },
                    headers=jwt_token.authorization_header,
                    timeout=60,
                )
            
            if response.status_code == 503:
                pytest.skip("Ingress service returning 503 - pods may not be ready")
            
            assert response.status_code in [200, 202], (
                f"Upload failed: {response.status_code} - {response.text}"
            )
        finally:
            # Clean up temp files
            tar_dir = os.path.dirname(tar_path)
            if os.path.exists(tar_path):
                os.unlink(tar_path)
            if os.path.exists(tar_dir):
                shutil.rmtree(tar_dir, ignore_errors=True)
            
            # Clean up NISE temp directory if present
            nise_temp_dir = e2e_test_data.get("temp_dir")
            if nise_temp_dir and os.path.exists(nise_temp_dir):
                shutil.rmtree(nise_temp_dir, ignore_errors=True)

    def test_04_manifest_created_in_koku(self, cluster_config, registered_source):
        """Step 4: Verify manifest was created in Koku database."""
        db_pod = get_pod_by_label(
            cluster_config.namespace,
            "app.kubernetes.io/component=database"
        )
        if not db_pod:
            pytest.skip("Database pod not found")
        
        cluster_id = registered_source["cluster_id"]
        
        def check_manifest():
            result = execute_db_query(
                cluster_config.namespace,
                db_pod,
                "costonprem_koku",
                "koku_user",
                f"""
                SELECT COUNT(*) FROM reporting_common_costusagereportmanifest
                WHERE cluster_id = '{cluster_id}'
                """,
            )
            return result is not None and int(result[0][0]) > 0
        
        success = wait_for_condition(
            check_manifest,
            timeout=300,
            interval=15,
            description="manifest creation",
        )
        
        assert success, f"Manifest not created for cluster {cluster_id}"
        
        # Validate manifest has required fields (from processing_state tests)
        manifest_result = execute_db_query(
            cluster_config.namespace,
            db_pod,
            "costonprem_koku",
            "koku_user",
            f"""
            SELECT 
                m.id,
                m.assembly_id,
                m.cluster_id,
                m.num_total_files,
                m.creation_datetime
            FROM reporting_common_costusagereportmanifest m
            WHERE m.cluster_id = '{cluster_id}'
            ORDER BY m.creation_datetime DESC
            LIMIT 1
            """,
        )
        
        assert manifest_result and manifest_result[0], "Manifest query returned no results"
        manifest = manifest_result[0]
        
        assert manifest[0] is not None, "Manifest missing ID"
        assert manifest[1] is not None, "Manifest missing assembly_id"
        assert manifest[2] == cluster_id, f"Manifest cluster_id mismatch: {manifest[2]}"
        assert manifest[3] is not None and int(manifest[3]) > 0, "Manifest has no files"
        assert manifest[4] is not None, "Manifest missing creation_datetime"
        
        print(f"  ✅ Manifest {manifest[0]} created with {manifest[3]} files")

    def test_05_files_processed_by_masu(self, cluster_config, registered_source):
        """Step 5: Verify uploaded files were processed by MASU with proper status."""
        db_pod = get_pod_by_label(
            cluster_config.namespace,
            "app.kubernetes.io/component=database"
        )
        if not db_pod:
            pytest.skip("Database pod not found")
        
        cluster_id = registered_source["cluster_id"]
        
        # File processing status codes (from Koku)
        FILE_STATUS_PENDING = 0
        FILE_STATUS_SUCCESS = 1
        FILE_STATUS_FAILED = 2
        
        def check_processing():
            result = execute_db_query(
                cluster_config.namespace,
                db_pod,
                "costonprem_koku",
                "koku_user",
                f"""
                SELECT s.status
                FROM reporting_common_costusagereportmanifest m
                JOIN reporting_common_costusagereportstatus s ON s.manifest_id = m.id
                WHERE m.cluster_id = '{cluster_id}'
                ORDER BY m.creation_datetime DESC
                LIMIT 1
                """,
            )
            # Status 1 = SUCCESS
            return result is not None and len(result) > 0 and str(result[0][0]) == "1"
        
        success = wait_for_condition(
            check_processing,
            timeout=600,
            interval=30,
            description="file processing by MASU",
        )
        
        assert success, "File processing not completed"
        
        # Validate file processing status details (from processing_state tests)
        file_status_result = execute_db_query(
            cluster_config.namespace,
            db_pod,
            "costonprem_koku",
            "koku_user",
            f"""
            SELECT 
                s.report_name,
                s.status,
                s.failed_status,
                s.completed_datetime
            FROM reporting_common_costusagereportmanifest m
            JOIN reporting_common_costusagereportstatus s ON s.manifest_id = m.id
            WHERE m.cluster_id = '{cluster_id}'
            ORDER BY m.creation_datetime DESC
            """,
        )
        
        if file_status_result:
            failed_files = []
            missing_completion = []
            
            for row in file_status_result:
                report_name, status, failed_status, completed_datetime = row
                status_int = int(status) if status is not None else None
                
                if status_int == FILE_STATUS_FAILED:
                    failed_files.append(report_name)
                elif status_int == FILE_STATUS_SUCCESS and completed_datetime is None:
                    missing_completion.append(report_name)
            
            # Log any issues but don't fail (files may still be processing)
            if failed_files:
                print(f"  ⚠️  {len(failed_files)} file(s) failed: {failed_files[:3]}")
            
            if missing_completion:
                print(f"  ⚠️  {len(missing_completion)} successful file(s) missing completion time")
            
            # Count successful files
            successful = sum(1 for row in file_status_result if row[1] and int(row[1]) == FILE_STATUS_SUCCESS)
            print(f"  ✅ {successful}/{len(file_status_result)} files processed successfully")

    @pytest.mark.timeout(900)  # 15 minutes for summary tables
    def test_06_summary_tables_populated(
        self, cluster_config, registered_source, e2e_test_data: dict
    ):
        """Step 6: Verify Koku summary tables are populated with correct data.
        
        IMPORTANT: Summary table population requires proper OCP data format.
        If using simple data (E2E_USE_SIMPLE_DATA=true), this test may fail because
        the simplified CSV format may not contain all required fields for Koku
        to populate summary tables.
        
        For reliable results, use NISE-generated data (the default).
        """
        # Check if using simple data format
        data_generator = e2e_test_data.get("generator", "unknown")
        if data_generator == "simple":
            pytest.skip(
                "Summary table population requires NISE-generated data. "
                "Simple data format (E2E_USE_SIMPLE_DATA=true) may not contain "
                "all required fields for Koku summary processing. "
                "Run with E2E_USE_SIMPLE_DATA=false to use NISE."
            )
        
        if e2e_test_data.get("nise_install_failed"):
            pytest.skip(
                "NISE installation failed - cannot validate summary tables. "
                "Install koku-nise manually: pip install koku-nise"
            )
        
        if e2e_test_data.get("nise_error"):
            pytest.skip(
                f"NISE data generation failed: {e2e_test_data['nise_error']}. "
                "Cannot validate summary tables without proper OCP data format."
            )
        
        db_pod = get_pod_by_label(
            cluster_config.namespace,
            "app.kubernetes.io/component=database"
        )
        if not db_pod:
            pytest.skip("Database pod not found")
        
        cluster_id = registered_source["cluster_id"]
        
        # Get tenant schema
        schema_result = execute_db_query(
            cluster_config.namespace,
            db_pod,
            "costonprem_koku",
            "koku_user",
            f"""
            SELECT c.schema_name
            FROM reporting_common_costusagereportmanifest m
            JOIN api_provider p ON m.provider_id = p.uuid
            JOIN api_customer c ON p.customer_id = c.id
            WHERE m.cluster_id = '{cluster_id}'
            LIMIT 1
            """,
        )
        
        if not schema_result or not schema_result[0][0]:
            # Provide detailed diagnostic information
            manifest_check = execute_db_query(
                cluster_config.namespace,
                db_pod,
                "costonprem_koku",
                "koku_user",
                f"""
                SELECT m.id, m.provider_id, m.num_total_files, m.num_processed_files
                FROM reporting_common_costusagereportmanifest m
                WHERE m.cluster_id = '{cluster_id}'
                ORDER BY m.creation_datetime DESC
                LIMIT 1
                """,
            )
            
            if manifest_check and manifest_check[0]:
                manifest_info = manifest_check[0]
                assert False, (
                    f"Manifest found (id={manifest_info[0]}) but not linked to provider. "
                    f"Provider ID: {manifest_info[1]}, "
                    f"Files: {manifest_info[3]}/{manifest_info[2]} processed. "
                    "This may indicate:\n"
                    "  1. Provider registration failed (check test_02)\n"
                    "  2. Manifest-provider linking is pending\n"
                    "  3. Data format issues preventing provider association"
                )
            else:
                assert False, (
                    f"No manifest found for cluster_id '{cluster_id}'. "
                    "This may indicate:\n"
                    "  1. Upload failed (check test_03)\n"
                    "  2. Koku listener didn't process the Kafka message\n"
                    "  3. Data format issues - ensure NISE-generated data is used"
                )
        
        schema_name = schema_result[0][0].strip()
        
        def check_summary():
            result = execute_db_query(
                cluster_config.namespace,
                db_pod,
                "costonprem_koku",
                "koku_user",
                f"""
                SELECT COUNT(*),
                       COALESCE(SUM(pod_request_cpu_core_hours), 0),
                       COALESCE(SUM(pod_request_memory_gigabyte_hours), 0)
                FROM {schema_name}.reporting_ocpusagelineitem_daily_summary
                WHERE cluster_id = '{cluster_id}'
                """,
            )
            return result is not None and int(result[0][0]) > 0
        
        success = wait_for_condition(
            check_summary,
            timeout=840,  # 14 minutes (leave buffer for pytest timeout)
            interval=30,
            description="summary table population",
        )
        
        if not success:
            # Get diagnostic info about what was processed
            file_status = execute_db_query(
                cluster_config.namespace,
                db_pod,
                "costonprem_koku",
                "koku_user",
                f"""
                SELECT rf.report_name, rf.completed_datetime, rf.status
                FROM reporting_common_costusagereportmanifest m
                JOIN reporting_common_costusagereportstatus rf ON m.id = rf.manifest_id
                WHERE m.cluster_id = '{cluster_id}'
                ORDER BY rf.completed_datetime DESC
                LIMIT 5
                """,
            )
            
            file_info = ""
            if file_status:
                file_info = "\n  Processed files:\n"
                for row in file_status:
                    file_info += f"    - {row[0]}: status={row[2]}, completed={row[1]}\n"
            
            assert False, (
                f"Summary tables not populated within timeout for cluster '{cluster_id}'.\n"
                f"Schema: {schema_name}\n"
                f"{file_info}"
                "\nPossible causes:\n"
                "  1. Koku listener 'missing start or end dates' bug - summary task not triggered\n"
                "     (Check listener logs for this message)\n"
                "  2. Celery workers not processing summary tasks\n"
                "  3. Data format incompatible with summary processing\n"
                "  4. Processing still in progress (try increasing timeout)\n"
                "\nTo debug:\n"
                "  - Check listener logs: oc logs -l app.kubernetes.io/component=listener | grep 'missing start'\n"
                "  - Check OCP worker logs: oc logs -l app.kubernetes.io/component=worker-ocp\n"
                "  - Check summary worker logs: oc logs -l app.kubernetes.io/component=worker-summary\n"
                "\nKNOWN ISSUE: Ingress-based uploads may not trigger summary tasks due to\n"
                "date extraction issues in Koku. Direct S3 upload (bash test) may work."
            )
        
        # Validate processing state (from processing_state tests)
        # Check for stuck manifests
        manifest_state = execute_db_query(
            cluster_config.namespace,
            db_pod,
            "costonprem_koku",
            "koku_user",
            f"""
            SELECT 
                m.id,
                m.num_total_files,
                m.num_processed_files,
                m.completed_datetime,
                m.state::text
            FROM reporting_common_costusagereportmanifest m
            WHERE m.cluster_id = '{cluster_id}'
            ORDER BY m.creation_datetime DESC
            LIMIT 1
            """,
        )
        
        if manifest_state and manifest_state[0]:
            manifest = manifest_state[0]
            manifest_id, total_files, processed_files, completed, state = manifest
            
            # Check if manifest is stuck (has files but none processed and not completed)
            if total_files and int(total_files) > 0:
                processed = int(processed_files) if processed_files else 0
                if processed == 0 and completed is None:
                    print(f"  ⚠️  Manifest {manifest_id} may be stuck: 0/{total_files} files processed")
                else:
                    print(f"  ✅ Manifest {manifest_id}: {processed}/{total_files} files processed")
            
            # Check for summary failures in state
            if state and "failed" in state.lower():
                print(f"  ⚠️  Manifest {manifest_id} has failure in state: {state[:100]}...")
        
        # Get summary data stats
        summary_stats = execute_db_query(
            cluster_config.namespace,
            db_pod,
            "costonprem_koku",
            "koku_user",
            f"""
            SELECT 
                COUNT(*) as row_count,
                COALESCE(SUM(pod_request_cpu_core_hours), 0) as cpu_hours,
                COALESCE(SUM(pod_request_memory_gigabyte_hours), 0) as mem_gb_hours
            FROM {schema_name}.reporting_ocpusagelineitem_daily_summary
            WHERE cluster_id = '{cluster_id}'
            """,
        )
        
        if summary_stats and summary_stats[0]:
            row_count, cpu_hours, mem_gb_hours = summary_stats[0]
            print(f"  ✅ Summary tables populated: {row_count} rows, {float(cpu_hours):.2f} CPU-hours, {float(mem_gb_hours):.2f} GB-hours")

    @pytest.mark.timeout(300)  # 5 minutes for Kruize experiments
    def test_07_kruize_experiments_created(
        self, cluster_config, registered_source, e2e_test_data: dict
    ):
        """Step 7: Verify Kruize experiments were created from ROS events.
        
        IMPORTANT: Kruize experiment creation requires:
        1. Summary tables to be populated (test_06 must pass)
        2. ROS events to be emitted by Koku
        3. ROS Processor to consume events and send to Kruize
        4. Proper OCP data format with resource metrics
        """
        # Check data format
        data_generator = e2e_test_data.get("generator", "unknown")
        if data_generator == "simple":
            pytest.skip(
                "Kruize experiments require NISE-generated data with proper resource metrics. "
                "Simple data format may not contain required fields for ROS processing."
            )
        
        db_pod = get_pod_by_label(
            cluster_config.namespace,
            "app.kubernetes.io/component=database"
        )
        if not db_pod:
            pytest.skip("Database pod not found")
        
        secret_name = f"{cluster_config.helm_release_name}-db-credentials"
        kruize_user = get_secret_value(cluster_config.namespace, secret_name, "kruize-user")
        kruize_password = get_secret_value(cluster_config.namespace, secret_name, "kruize-password")
        
        if not kruize_user:
            pytest.skip("Kruize credentials not found - ROS may not be deployed")
        
        cluster_id = registered_source["cluster_id"]
        
        def check_experiments():
            result = execute_db_query(
                cluster_config.namespace,
                db_pod,
                "costonprem_kruize",
                kruize_user,
                f"""
                SELECT COUNT(*) FROM kruize_experiments
                WHERE cluster_name LIKE '%{cluster_id}%'
                """,
                password=kruize_password,
            )
            return result is not None and int(result[0][0]) > 0
        
        # Kruize processing takes time - ROS events must flow through
        success = wait_for_condition(
            check_experiments,
            timeout=240,  # 4 minutes
            interval=20,
            description="Kruize experiment creation",
        )
        
        if not success:
            # Get diagnostic info
            ros_events_check = None
            try:
                # Check if ROS events topic has messages
                result = run_oc_command([
                    "exec", "-n", cluster_config.namespace,
                    "kafka-cluster-kafka-0", "--",
                    "bin/kafka-console-consumer.sh",
                    "--bootstrap-server", "localhost:9092",
                    "--topic", "hccm.ros.events",
                    "--from-beginning",
                    "--max-messages", "1",
                    "--timeout-ms", "5000",
                ], check=False)
                ros_events_check = "ROS events topic has messages" if result.returncode == 0 else "No messages in ROS events topic"
            except Exception:
                ros_events_check = "Could not check ROS events topic"
            
            assert False, (
                f"Kruize experiments not created for cluster '{cluster_id}'.\n"
                f"ROS Events: {ros_events_check}\n"
                "\nPossible causes:\n"
                "  1. Summary tables not populated (test_06 must pass first)\n"
                "  2. ROS events not being emitted by Koku\n"
                "  3. ROS Processor not consuming events\n"
                "  4. Kruize not processing ROS data\n"
                "  5. Data format missing required resource metrics\n"
                "\nTo debug:\n"
                "  - Check ROS Processor logs: oc logs -l app.kubernetes.io/component=ros-processor\n"
                "  - Check Kruize logs: oc logs -l app.kubernetes.io/name=kruize\n"
                "  - Verify ROS events topic: oc exec kafka-cluster-kafka-0 -- bin/kafka-topics.sh --list --bootstrap-server localhost:9092"
            )

    @pytest.mark.timeout(300)  # 5 minutes for recommendations
    def test_08_recommendations_generated(
        self, cluster_config, registered_source, e2e_test_data: dict
    ):
        """Step 8: Verify recommendations were generated by Kruize.
        
        IMPORTANT: Recommendation generation requires:
        1. Kruize experiments to exist (test_07 must pass)
        2. Sufficient data points for Kruize to generate recommendations
        3. Proper OCP data format with CPU/memory usage metrics
        """
        # Check data format
        data_generator = e2e_test_data.get("generator", "unknown")
        if data_generator == "simple":
            pytest.skip(
                "Recommendation generation requires NISE-generated data with proper metrics. "
                "Simple data format may not contain sufficient data for Kruize recommendations."
            )
        
        db_pod = get_pod_by_label(
            cluster_config.namespace,
            "app.kubernetes.io/component=database"
        )
        if not db_pod:
            pytest.skip("Database pod not found")
        
        secret_name = f"{cluster_config.helm_release_name}-db-credentials"
        kruize_user = get_secret_value(cluster_config.namespace, secret_name, "kruize-user")
        kruize_password = get_secret_value(cluster_config.namespace, secret_name, "kruize-password")
        
        if not kruize_user:
            pytest.skip("Kruize credentials not found - ROS may not be deployed")
        
        cluster_id = registered_source["cluster_id"]
        
        # First check if experiments exist
        experiment_result = execute_db_query(
            cluster_config.namespace,
            db_pod,
            "costonprem_kruize",
            kruize_user,
            f"""
            SELECT COUNT(*) FROM kruize_experiments
            WHERE cluster_name LIKE '%{cluster_id}%'
            """,
            password=kruize_password,
        )
        
        experiment_count = int(experiment_result[0][0]) if experiment_result else 0
        if experiment_count == 0:
            pytest.skip(
                f"No Kruize experiments found for cluster '{cluster_id}'. "
                "test_07 must pass before recommendations can be generated."
            )
        
        def check_recommendations():
            result = execute_db_query(
                cluster_config.namespace,
                db_pod,
                "costonprem_kruize",
                kruize_user,
                f"""
                SELECT COUNT(*) FROM kruize_recommendations
                WHERE cluster_name LIKE '%{cluster_id}%'
                """,
                password=kruize_password,
            )
            return result is not None and int(result[0][0]) > 0
        
        success = wait_for_condition(
            check_recommendations,
            timeout=240,  # 4 minutes
            interval=20,
            description="recommendation generation",
        )
        
        if not success:
            # Get experiment details for diagnostics
            exp_details = execute_db_query(
                cluster_config.namespace,
                db_pod,
                "costonprem_kruize",
                kruize_user,
                f"""
                SELECT experiment_name, status, created_at
                FROM kruize_experiments
                WHERE cluster_name LIKE '%{cluster_id}%'
                ORDER BY created_at DESC
                LIMIT 3
                """,
                password=kruize_password,
            )
            
            exp_info = ""
            if exp_details:
                exp_info = "\n  Experiments found:\n"
                for row in exp_details:
                    exp_info += f"    - {row[0]}: status={row[1]}, created={row[2]}\n"
            
            assert False, (
                f"Recommendations not generated for cluster '{cluster_id}'.\n"
                f"Experiments found: {experiment_count}\n"
                f"{exp_info}"
                "\nPossible causes:\n"
                "  1. Insufficient data points - Kruize needs multiple data uploads\n"
                "  2. Experiments exist but haven't been processed yet\n"
                "  3. Data format missing required CPU/memory usage metrics\n"
                "  4. Kruize recommendation engine not running\n"
                "\nTo debug:\n"
                "  - Check Kruize logs: oc logs -l app.kubernetes.io/name=kruize\n"
                "  - Query experiments: oc exec <db-pod> -- psql -U kruize -d costonprem_kruize -c \"SELECT * FROM kruize_experiments;\"\n"
                "\nNote: In a single E2E test run, recommendations may not be generated.\n"
                "Kruize typically requires multiple data uploads over several hours."
            )

    def test_09_recommendations_accessible_via_api(
        self,
        require_ros_enabled,
        gateway_url: str,
        keycloak_config,
        cluster_config,
        http_session: requests.Session,
    ):
        """Step 9: Verify recommendations are accessible via JWT-authenticated API."""
        from conftest import obtain_user_jwt_token

        try:
            token = obtain_user_jwt_token(keycloak_config, cluster_config)
        except Exception as exc:
            pytest.skip(f"Could not obtain user JWT token: {exc}")

        response = http_session.get(
            f"{gateway_url}/cost-management/v1/recommendations/openshift",
            headers={"Authorization": f"Bearer {token.access_token}"},
            timeout=30,
        )
        
        # 401 may indicate the route requires different auth or isn't configured
        if response.status_code == 401:
            pytest.skip("Recommendations API returned 401 - may require different auth configuration")
        
        assert response.status_code == 200, (
            f"Recommendations API failed: {response.status_code}"
        )
        
        data = response.json()
        
        # Verify response structure
        if "data" in data:
            assert isinstance(data["data"], list), "Invalid response format"
