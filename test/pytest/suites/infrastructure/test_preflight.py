"""
Preflight infrastructure tests.

Tests to verify infrastructure components are healthy before running other tests.
"""

import pytest

from utils import (
    run_oc_command,
    check_pod_exists,
    check_pod_ready,
    exec_in_pod,
)


@pytest.mark.infrastructure
@pytest.mark.component
@pytest.mark.smoke
class TestPodHealth:
    """Tests for pod health status."""

    def test_database_pod_exists(self, cluster_config, database_deployed):
        """Verify the operator-deployed database pod exists (bundled deployments only)."""
        if not database_deployed:
            pytest.skip(
                "No operator-deployed database pod in this namespace (BYOI: the "
                "database is external; its health is covered by TestDatabaseConnectivity)"
            )
        assert check_pod_exists(
            cluster_config.namespace,
            "app.kubernetes.io/component=database"
        ), "Database pod not found"

    def test_database_pod_ready(self, cluster_config, database_deployed):
        """Verify the operator-deployed database pod is ready (bundled deployments only)."""
        if not database_deployed:
            pytest.skip(
                "No operator-deployed database pod in this namespace (BYOI: the "
                "database is external; its health is covered by TestDatabaseConnectivity)"
            )
        assert check_pod_ready(
            cluster_config.namespace,
            "app.kubernetes.io/component=database"
        ), "Database pod is not ready"

    def test_ingress_pod_exists(self, cluster_config):
        """Verify ingress pod exists."""
        assert check_pod_exists(
            cluster_config.namespace,
            "app.kubernetes.io/component=ingress"
        ), "Ingress pod not found"

    def test_masu_pod_exists(self, cluster_config):
        """Verify MASU pod exists."""
        assert check_pod_exists(
            cluster_config.namespace,
            "app.kubernetes.io/component=cost-processor"
        ), "MASU pod not found"

    def test_listener_pod_exists(self, cluster_config):
        """Verify Koku listener pod exists."""
        assert check_pod_exists(
            cluster_config.namespace,
            "app.kubernetes.io/component=listener"
        ), "Listener pod not found"


@pytest.mark.infrastructure
@pytest.mark.component
class TestDatabaseConnectivity:
    """Tests for database connectivity."""

    @pytest.mark.smoke
    def test_database_accepts_connections(self, cluster_config, database_config):
        """Verify database accepts connections.

        Marked smoke so a real readiness probe (pg_isready) runs in every
        deployment mode. In BYOI the operator-created database pod tests skip,
        so this connectivity check is what actually asserts DB readiness there.
        """
        result = exec_in_pod(
            database_config.namespace,
            database_config.pod_name,
            ["pg_isready", "-U", database_config.user, "-d", database_config.database],
        )
        
        assert result is not None, "Database not accepting connections"
        assert "accepting connections" in result.lower(), f"Unexpected response: {result}"

    def test_koku_database_exists(self, cluster_config, database_config):
        """Verify Koku database exists."""
        # Use the database name from the fixture (auto-detected from deployment)
        db_name = database_config.database
        cmd = [
            "psql", "-U", database_config.user, "-d", "postgres",
            "-t", "-c", f"SELECT 1 FROM pg_database WHERE datname='{db_name}'"
        ]
        
        if database_config.password:
            cmd = ["env", f"PGPASSWORD={database_config.password}"] + cmd
        
        result = exec_in_pod(database_config.namespace, database_config.pod_name, cmd)
        
        assert result is not None and "1" in result, f"{db_name} database not found"

    def test_kruize_database_exists(
        self, require_ros_enabled, cluster_config, kruize_database_config
    ):
        """Verify Kruize database exists."""
        db_name = kruize_database_config.database
        cmd = [
            "psql", "-U", kruize_database_config.user, "-d", "postgres",
            "-t", "-c", f"SELECT 1 FROM pg_database WHERE datname='{db_name}'"
        ]
        
        if kruize_database_config.password:
            cmd = ["env", f"PGPASSWORD={kruize_database_config.password}"] + cmd
        
        result = exec_in_pod(kruize_database_config.namespace, kruize_database_config.pod_name, cmd)
        
        assert result is not None and "1" in result, f"{db_name} database not found"


@pytest.mark.infrastructure
@pytest.mark.component
class TestS3Connectivity:
    """Tests for S3/Object storage connectivity."""

    def test_s3_config_available(self, s3_config):
        """Verify S3 configuration is available."""
        if s3_config is None:
            pytest.skip("S3 configuration not available")
        
        assert s3_config.endpoint, "S3 endpoint not configured"
        assert s3_config.access_key, "S3 access key not configured"
        assert s3_config.secret_key, "S3 secret key not configured"

    def test_s3_bucket_accessible(self, cluster_config, s3_config):
        """Verify S3 bucket is accessible from within the cluster."""
        if s3_config is None:
            pytest.skip("S3 configuration not available")
        
        # Find a pod to test from (use masu or listener)
        pod_name = None
        for label in ["app.kubernetes.io/component=cost-processor", "app.kubernetes.io/component=listener"]:
            result = run_oc_command([
                "get", "pods", "-n", cluster_config.namespace,
                "-l", label,
                "-o", "jsonpath={.items[0].metadata.name}"
            ], check=False)
            if result.stdout.strip():
                pod_name = result.stdout.strip()
                break
        
        if not pod_name:
            pytest.skip("No suitable pod found to test S3 connectivity")
        
        # Check if S3_ENDPOINT env var is set in the pod
        result = exec_in_pod(
            cluster_config.namespace,
            pod_name,
            ["env"],
        )
        
        assert result is not None, "Could not get pod environment"
        assert "S3_ENDPOINT" in result or "AWS_" in result, (
            "S3 configuration not found in pod environment"
        )
