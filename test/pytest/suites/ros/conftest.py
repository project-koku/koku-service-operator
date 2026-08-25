"""
ROS (Resource Optimization Service) suite fixtures.
"""

import pytest

from utils import get_pod_by_label, get_secret_value, get_route_url, run_oc_command


@pytest.fixture(autouse=True)
def _skip_ros_suite_when_disabled(require_ros_enabled) -> None:
    """Skip ROS suite tests when spec.ros.enabled is false (Cost-only beta)."""


@pytest.fixture(scope="module")
def kruize_pod(cluster_config) -> str:
    """Get Kruize pod name."""
    pod = get_pod_by_label(cluster_config.namespace, "app.kubernetes.io/component=ros-optimization")
    if not pod:
        pytest.skip("Kruize pod not found")
    return pod


@pytest.fixture(scope="module")
def kruize_credentials(cluster_config) -> dict:
    """Get Kruize database credentials."""
    secret_name = f"{cluster_config.helm_release_name}-db-credentials"
    user = get_secret_value(cluster_config.namespace, secret_name, "kruize-user")
    password = get_secret_value(cluster_config.namespace, secret_name, "kruize-password")
    
    if not user or not password:
        pytest.skip("Kruize database credentials not found")
    
    return {"user": user, "password": password, "database": "costonprem_kruize"}


@pytest.fixture(scope="module")
def ros_api_url(cluster_config) -> str:
    """Get ROS API URL via the centralized gateway."""
    # With centralized gateway, all API traffic goes through cost-onprem-api route
    route_name = f"{cluster_config.helm_release_name}-api"
    url = get_route_url(cluster_config.namespace, route_name)
    if not url:
        pytest.skip("API gateway route not found")

    # Get the route path (e.g., /api)
    result = run_oc_command([
        "get", "route", route_name, "-n", cluster_config.namespace,
        "-o", "jsonpath={.spec.path}"
    ], check=False)
    route_path = result.stdout.strip().rstrip("/")

    return f"{url}{route_path}" if route_path else url
