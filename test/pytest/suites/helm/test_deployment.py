"""
Helm deployment health tests.

These tests verify a deployed Helm release is healthy.
"""

import pytest

from utils import run_helm_command, run_oc_command, check_pod_ready


@pytest.mark.helm
@pytest.mark.component
class TestHelmRelease:
    """Tests for Helm release status."""

    @pytest.mark.smoke
    def test_release_exists(self, cluster_config):
        """Verify the Helm release exists."""
        result = run_helm_command([
            "status", cluster_config.helm_release_name,
            "-n", cluster_config.namespace,
        ], check=False)
        
        assert result.returncode == 0, (
            f"Helm release '{cluster_config.helm_release_name}' not found "
            f"in namespace '{cluster_config.namespace}'"
        )

    def test_release_deployed_status(self, cluster_config):
        """Verify the Helm release is in 'deployed' status."""
        result = run_helm_command([
            "status", cluster_config.helm_release_name,
            "-n", cluster_config.namespace,
            "-o", "json",
        ], check=False)
        
        if result.returncode != 0:
            pytest.skip("Helm release not found")
        
        import json
        status = json.loads(result.stdout)
        assert status.get("info", {}).get("status") == "deployed", (
            f"Release status is not 'deployed': {status.get('info', {}).get('status')}"
        )


@pytest.mark.helm
@pytest.mark.component
class TestDeploymentHealth:
    """Tests for deployment health after Helm install."""

    @pytest.mark.smoke
    def test_database_pod_ready(self, cluster_config, database_deployed):
        """Verify database pod is ready (bundled deployments only)."""
        if not database_deployed:
            pytest.skip(
                "No operator-deployed database pod in this namespace (BYOI: the "
                "database is external; its health is covered by TestDatabaseConnectivity)"
            )
        assert check_pod_ready(
            cluster_config.namespace,
            "app.kubernetes.io/component=database"
        ), "Database pod is not ready"

    @pytest.mark.smoke
    def test_ingress_pod_ready(self, cluster_config):
        """Verify ingress pod is ready."""
        assert check_pod_ready(
            cluster_config.namespace,
            "app.kubernetes.io/component=ingress"
        ), "Ingress pod is not ready"

    def test_kruize_pod_ready(self, cluster_config):
        """Verify Kruize pod is ready."""
        assert check_pod_ready(
            cluster_config.namespace,
            "app.kubernetes.io/component=ros-optimization"
        ), "Kruize pod is not ready"

    def test_ros_api_pod_ready(self, cluster_config):
        """Verify ROS API pod is ready."""
        assert check_pod_ready(
            cluster_config.namespace,
            "app.kubernetes.io/component=ros-api"
        ), "ROS API pod is not ready"

    def test_ros_processor_pod_ready(self, cluster_config):
        """Verify ROS Processor pod is ready."""
        assert check_pod_ready(
            cluster_config.namespace,
            "app.kubernetes.io/component=ros-processor"
        ), "ROS Processor pod is not ready"

    def test_koku_api_pod_ready(self, cluster_config):
        """Verify Koku API pod is ready (provides cost management and sources endpoints)."""
        assert check_pod_ready(
            cluster_config.namespace,
            "app.kubernetes.io/component=cost-management-api"
        ), "Koku API pod is not ready"

    def test_ui_pod_ready(self, cluster_config):
        """Verify UI pod is ready.
        
        FLPATH-3855: Verify UI Component Deployment
        """
        assert check_pod_ready(
            cluster_config.namespace,
            "app.kubernetes.io/component=ui"
        ), "UI pod is not ready"


@pytest.mark.helm
@pytest.mark.component
class TestServices:
    """Tests for Kubernetes services."""

    def test_services_exist(self, cluster_config, database_deployed):
        """Verify expected services exist."""
        result = run_oc_command([
            "get", "services", "-n", cluster_config.namespace,
            "-o", "jsonpath={.items[*].metadata.name}"
        ], check=False)
        
        services = result.stdout.split()
        
        expected_services = [
            f"{cluster_config.helm_release_name}-ingress",
        ]
        if database_deployed:
            expected_services.append(
                f"{cluster_config.helm_release_name}-database"
            )
        
        for svc in expected_services:
            assert svc in services, f"Service '{svc}' not found"


@pytest.mark.helm
@pytest.mark.component
class TestRoutes:
    """Tests for OpenShift routes."""

    def test_api_gateway_route_exists(self, cluster_config):
        """Verify API gateway route exists."""
        result = run_oc_command([
            "get", "route",
            f"{cluster_config.helm_release_name}-api",
            "-n", cluster_config.namespace,
        ], check=False)

        assert result.returncode == 0, "API gateway route not found"

    def test_ui_route_exists(self, cluster_config):
        """Verify UI route exists."""
        result = run_oc_command([
            "get", "route",
            f"{cluster_config.helm_release_name}-ui",
            "-n", cluster_config.namespace,
        ], check=False)

        assert result.returncode == 0, "UI route not found"

    def test_routes_have_hosts(self, cluster_config):
        """Verify routes have assigned hosts."""
        result = run_oc_command([
            "get", "routes", "-n", cluster_config.namespace,
            "-o", "jsonpath={.items[*].spec.host}"
        ], check=False)
        
        hosts = result.stdout.split()
        assert len(hosts) > 0, "No routes have assigned hosts"
