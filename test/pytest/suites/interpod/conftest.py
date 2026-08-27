"""
Fixtures for interpod (pod-to-pod) cluster tests.

These fixtures provide helpers for executing commands inside the cluster
via the test-runner pod, testing internal service networking.

The test-runner pod is a dedicated UBI9 container created at session start
that provides a consistent environment for all interpod tests.
"""

import json
import pytest
import requests
from dataclasses import dataclass
from typing import Callable, Optional

from conftest import ClusterConfig
from utils import run_oc_command, create_rh_identity_header, create_pod_session


@dataclass
class CurlResult:
    """Result from internal curl command.
    
    DEPRECATED: Use pod_session fixture instead for a standard requests API.
    This class is kept for backward compatibility with existing tests.
    """
    stdout: str
    stderr: str
    returncode: int
    
    @property
    def ok(self) -> bool:
        """Check if the curl command succeeded."""
        return self.returncode == 0
    
    def json(self) -> dict:
        """Parse stdout as JSON."""
        return json.loads(self.stdout)


@pytest.fixture
def internal_curl(
    test_runner_pod: str,
    cluster_config: ClusterConfig,
) -> Callable:
    """Helper to execute curl commands from the test-runner pod.
    
    DEPRECATED: Use pod_session fixture instead for a standard requests API.
    
    This fixture provides a callable that executes curl commands inside
    the cluster, useful for testing internal service endpoints.
    
    Usage:
        def test_something(internal_curl, internal_api_url):
            result = internal_curl(f"{internal_api_url}/api/v1/status/")
            assert result.ok
            data = result.json()
            assert data["status"] == "ok"
    
    Args:
        url: The URL to request
        method: HTTP method (GET, POST, etc.)
        headers: Optional dict of headers to include
        data: Optional request body (string)
        
    Returns:
        CurlResult with stdout, stderr, and returncode
    """
    def _curl(
        url: str,
        method: str = "GET",
        headers: Optional[dict] = None,
        data: Optional[str] = None,
    ) -> CurlResult:
        cmd = ["curl", "-s", "-X", method]
        
        # Add headers
        for key, value in (headers or {}).items():
            cmd.extend(["-H", f"{key}: {value}"])
        
        # Add data
        if data:
            cmd.extend(["-d", data])
        
        cmd.append(url)
        
        # Build full exec command - use runner container in test-runner pod
        args = ["exec", "-n", cluster_config.namespace, test_runner_pod, "-c", "runner", "--"]
        args.extend(cmd)
        
        result = run_oc_command(args, check=False, timeout=60)
        
        return CurlResult(
            stdout=result.stdout,
            stderr=result.stderr,
            returncode=result.returncode,
        )
    
    return _curl


@pytest.fixture
def internal_identity_header(cluster_config: ClusterConfig, org_id: str) -> str:
    """Pre-built X-Rh-Identity header for internal API calls.
    
    Internal services expect the X-Rh-Identity header that would normally
    be injected by the gateway. This fixture provides a valid header for
    direct service-to-service testing.
    """
    return create_rh_identity_header(
        org_id=org_id,
        account_number="1234567",
    )


@pytest.fixture
def rh_identity_header(org_id: str) -> str:
    """Admin X-Rh-Identity header for pod_session (aligned with e2e/sources suites)."""
    return create_rh_identity_header(org_id)


@pytest.fixture
def pod_session(
    test_runner_pod: str,
    cluster_config: ClusterConfig,
    rh_identity_header: str,
) -> requests.Session:
    """Pre-configured requests.Session that routes through the test-runner pod.
    
    This fixture provides a standard requests.Session API for making HTTP
    calls that execute inside the cluster via kubectl exec curl. It includes
    the X-Rh-Identity header required for internal service authentication.
    
    Usage:
        def test_something(pod_session, internal_api_url):
            response = pod_session.get(f"{internal_api_url}/api/v1/status/")
            assert response.ok
            data = response.json()
            assert data["status"] == "ok"
    
    The session supports all standard requests methods:
    - response = pod_session.get(url)
    - response = pod_session.post(url, json=data)
    - response = pod_session.put(url, json=data)
    - response = pod_session.delete(url)
    
    And all standard response attributes:
    - response.ok (True if status_code < 400)
    - response.status_code
    - response.json()
    - response.text
    - response.headers
    - response.raise_for_status()
    """
    session = create_pod_session(
        namespace=cluster_config.namespace,
        pod=test_runner_pod,
        container="runner",
        headers={
            "X-Rh-Identity": rh_identity_header,
            "Content-Type": "application/json",
        },
        timeout=60,
    )
    return session


@pytest.fixture
def pod_session_no_auth(
    test_runner_pod: str,
    cluster_config: ClusterConfig,
) -> requests.Session:
    """Pre-configured requests.Session without authentication headers.
    
    Use this fixture when testing endpoints that don't require authentication
    or when you want to explicitly test authentication failures.
    """
    session = create_pod_session(
        namespace=cluster_config.namespace,
        pod=test_runner_pod,
        container="runner",
        timeout=60,
    )
    return session
