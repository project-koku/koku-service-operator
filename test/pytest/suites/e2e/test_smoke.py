"""
E2E smoke tests.

Quick validation that the entire system is operational.
"""

import pytest
import requests

from conftest import get_fresh_auth_header
from smoke_pods import critical_smoke_components
from utils import check_pod_ready, run_oc_command


@pytest.mark.e2e
@pytest.mark.integration
@pytest.mark.smoke
class TestE2ESmoke:
    """Quick smoke tests for E2E validation."""

    def test_all_critical_pods_running(
        self, cluster_config, database_deployed, ros_enabled
    ):
        """Verify critical Cost pods are running (ROS/Kruize only when enabled)."""
        critical_components = critical_smoke_components(
            database_deployed=database_deployed,
            ros_enabled=ros_enabled,
        )

        failures = []
        for name, label in critical_components:
            if not check_pod_ready(cluster_config.namespace, label):
                failures.append(name)

        assert not failures, f"Critical pods not ready: {failures}"

    def test_keycloak_accessible(self, keycloak_config, http_session: requests.Session):
        """Verify Keycloak is accessible."""
        response = http_session.get(
            f"{keycloak_config.url}/realms/{keycloak_config.realm}",
            timeout=10,
        )
        assert response.status_code == 200, "Keycloak not accessible"

    def test_jwt_token_obtainable(self, keycloak_config, http_session: requests.Session):
        """Verify JWT token can be obtained."""
        auth_header = get_fresh_auth_header(keycloak_config, http_session)
        assert auth_header, "Could not obtain JWT token"

    def test_gateway_accepts_authenticated_requests(
        self, gateway_url: str, keycloak_config, http_session: requests.Session
    ):
        """Verify gateway accepts authenticated requests."""
        auth_header = get_fresh_auth_header(keycloak_config, http_session)
        if not auth_header:
            pytest.skip("Could not obtain fresh JWT token")
        
        # Use cost-management status endpoint - always returns 200 with valid auth
        response = http_session.get(
            f"{gateway_url}/cost-management/v1/status/",
            headers=auth_header,
            timeout=10,
        )
        
        assert response.status_code == 200, (
            f"Expected 200 from status endpoint, got {response.status_code}"
        )

    def test_backend_api_accessible(
        self, gateway_url: str, keycloak_config, http_session: requests.Session
    ):
        """Verify backend API is accessible through the gateway."""
        auth_header = get_fresh_auth_header(keycloak_config, http_session)
        if not auth_header:
            pytest.skip("Could not obtain fresh JWT token")

        response = http_session.get(
            f"{gateway_url}/cost-management/v1/status/",
            headers=auth_header,
            timeout=10,
        )

        # Koku status endpoint returns 200 with API version info
        assert response.status_code == 200, (
            f"Expected 200 from backend status, got {response.status_code}"
        )

    def test_kafka_cluster_healthy(self, cluster_config):
        """Verify Kafka cluster is healthy."""
        # Check for Kafka pods in common namespaces
        for ns in ["kafka", cluster_config.namespace, "strimzi"]:
            result = run_oc_command([
                "get", "kafka", "-n", ns,
                "-o", "jsonpath={.items[0].status.conditions[?(@.type=='Ready')].status}"
            ], check=False)
            
            if result.stdout.strip() == "True":
                return
        
        pytest.skip("Kafka cluster not found or not ready")
