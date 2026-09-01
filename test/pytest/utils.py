"""
Utility functions for cost-onprem-chart tests.

These are helper functions that can be imported by test modules across all suites.
"""

import base64
import http.client
import io
import json
import re
import socket
import subprocess
import tarfile
import tempfile
import uuid
from datetime import datetime, timezone
from pathlib import Path
from typing import Any, Optional
from urllib.parse import urlparse

import requests
from requests.adapters import HTTPAdapter
from requests.structures import CaseInsensitiveDict
from urllib3.response import HTTPResponse
from urllib3.util.retry import Retry


def route_http_retry() -> Retry:
    """Retry NXDOMAIN/connect errors to claimed-cluster ``*.apps.*`` Routes.

    Prow pytest runs outside the cluster. urllib3 does not retry
    ``NameResolutionError`` unless ``connect`` retries are set.
    """
    retry_kw = {
        "total": 6,
        "connect": 6,
        "read": 3,
        "backoff_factor": 0.5,
        "status_forcelist": (502, 503, 504),
        "raise_on_status": False,
    }
    try:
        return Retry(allowed_methods=False, **retry_kw)
    except TypeError:
        return Retry(method_whitelist=False, **retry_kw)


def mount_route_dns_retries(session: requests.Session) -> requests.Session:
    """Mount connect retries on *session* for http and https."""
    adapter = HTTPAdapter(max_retries=route_http_retry())
    session.mount("https://", adapter)
    session.mount("http://", adapter)
    return session


def install_route_dns_retries() -> None:
    """Apply Route DNS retries to every ``requests.Session``, including ``requests.get``.

    Patching individual fixtures is not enough: token helpers and many tests
    call ``requests.post`` / ``requests.get``, which construct a throwaway
    Session with ``max_retries=0``.
    """
    if getattr(install_route_dns_retries, "_installed", False):
        return
    orig_init = requests.Session.__init__

    def _init(self, *args, **kwargs):
        orig_init(self, *args, **kwargs)
        mount_route_dns_retries(self)

    requests.Session.__init__ = _init  # type: ignore[method-assign]
    install_route_dns_retries._installed = True  # type: ignore[attr-defined]


def external_http_session(*, verify: bool = False) -> requests.Session:
    """Session for Route/API calls from the Prow pod (outside the cluster)."""
    session = requests.Session()
    session.trust_env = False
    session.verify = verify
    return mount_route_dns_retries(session)


class _FakeSocket:
    """Minimal socket-like object for http.client.HTTPResponse.
    
    Used by PodAdapter to parse raw HTTP response text from curl output.
    http.client.HTTPResponse expects a socket with a makefile() method.
    """
    
    def __init__(self, data: bytes) -> None:
        self._file = io.BytesIO(data)
    
    def makefile(self, mode: str) -> io.BytesIO:
        return self._file


# =============================================================================
# Kubernetes/OpenShift Commands
# =============================================================================


def run_oc_command(
    args: list[str],
    check: bool = True,
    timeout: int = 60,
) -> subprocess.CompletedProcess:
    """Run an oc command and return the result.
    
    Args:
        args: Command arguments (without 'oc' prefix)
        check: Raise exception on non-zero exit code
        timeout: Command timeout in seconds
    
    Returns:
        CompletedProcess with stdout, stderr, returncode
    """
    cmd = ["oc"] + args
    return subprocess.run(
        cmd,
        capture_output=True,
        text=True,
        check=check,
        timeout=timeout,
    )


def get_route_url(namespace: str, route_name: str) -> Optional[str]:
    """Get the URL for an OpenShift route."""
    try:
        result = run_oc_command(
            ["get", "route", route_name, "-n", namespace, "-o", "jsonpath={.spec.host}"]
        )
        host = result.stdout.strip()
        if not host:
            return None

        # Check if TLS is enabled
        tls_result = run_oc_command(
            [
                "get",
                "route",
                route_name,
                "-n",
                namespace,
                "-o",
                "jsonpath={.spec.tls.termination}",
            ],
            check=False,
        )
        tls = tls_result.stdout.strip()

        scheme = "https" if tls else "http"
        return f"{scheme}://{host}"
    except subprocess.CalledProcessError:
        return None


def get_secret_value(namespace: str, secret_name: str, key: str) -> Optional[str]:
    """Get a decoded value from a Kubernetes secret."""
    try:
        result = run_oc_command(
            [
                "get",
                "secret",
                secret_name,
                "-n",
                namespace,
                "-o",
                f"jsonpath={{.data.{key}}}",
            ]
        )
        encoded = result.stdout.strip()
        if not encoded:
            return None
        return base64.b64decode(encoded).decode("utf-8")
    except (subprocess.CalledProcessError, ValueError):
        return None


def get_pod_by_label(namespace: str, label: str) -> Optional[str]:
    """Get the first pod name matching a label selector."""
    try:
        result = run_oc_command([
            "get", "pods", "-n", namespace,
            "-l", label,
            "-o", "jsonpath={.items[0].metadata.name}"
        ], check=False)
        pod_name = result.stdout.strip()
        return pod_name if pod_name else None
    except subprocess.CalledProcessError:
        return None


def exec_in_pod(
    namespace: str,
    pod_name: str,
    command: list[str],
    container: Optional[str] = None,
    timeout: int = 60,
) -> Optional[str]:
    """Execute a command in a pod and return stdout."""
    try:
        args = ["exec", "-n", namespace, pod_name]
        if container:
            args.extend(["-c", container])
        args.append("--")
        args.extend(command)
        
        result = run_oc_command(args, check=False, timeout=timeout)
        return result.stdout if result.returncode == 0 else None
    except (subprocess.CalledProcessError, subprocess.TimeoutExpired):
        return None


def exec_in_pod_raw(
    namespace: str,
    pod_name: str,
    command: list[str],
    container: Optional[str] = None,
    timeout: int = 60,
) -> subprocess.CompletedProcess:
    """Execute a command in a pod and return the full CompletedProcess.
    
    Unlike exec_in_pod(), this returns the full result including stderr
    and returncode, useful for the PodAdapter.
    """
    args = ["exec", "-n", namespace, pod_name]
    if container:
        args.extend(["-c", container])
    args.append("--")
    args.extend(command)
    
    return run_oc_command(args, check=False, timeout=timeout)


# =============================================================================
# Pod HTTP Adapter - Route requests.Session through kubectl exec curl
# =============================================================================


class PodAdapter(HTTPAdapter):
    """HTTP adapter that routes requests through curl inside a Kubernetes pod.
    
    This adapter allows using the standard requests.Session API for making
    HTTP calls that execute inside a pod, useful for testing internal
    cluster services that aren't exposed externally.
    
    Usage:
        session = requests.Session()
        adapter = PodAdapter(namespace="cost-onprem", pod="test-runner", container="runner")
        session.mount("http://", adapter)
        session.mount("https://", adapter)
        
        # Now use standard requests API
        response = session.get("http://koku-api:8000/api/v1/status/")
        assert response.status_code == 200
        data = response.json()
    """
    
    def __init__(
        self,
        namespace: str,
        pod: str,
        container: Optional[str] = None,
        timeout: int = 60,
        **kwargs,
    ):
        """Initialize the PodAdapter.
        
        Args:
            namespace: Kubernetes namespace where the pod is running
            pod: Name of the pod to execute curl in
            container: Container name (if pod has multiple containers)
            timeout: Timeout for curl commands in seconds
        """
        self.namespace = namespace
        self.pod = pod
        self.container = container
        self.timeout = timeout
        super().__init__(**kwargs)
    
    def send(
        self,
        request: requests.PreparedRequest,
        stream: bool = False,
        timeout: Any = None,
        verify: bool = True,
        cert: Any = None,
        proxies: Any = None,
    ) -> requests.Response:
        """Send a PreparedRequest by executing curl inside the pod.
        
        This method builds a curl command from the PreparedRequest,
        executes it inside the pod, and parses the raw HTTP response
        into a requests.Response object.
        """
        # Build curl command with -i to include headers in output
        cmd = ["curl", "-i", "-s", "-S"]
        
        # Add method
        cmd.extend(["-X", request.method])
        
        # Add headers
        if request.headers:
            for key, value in request.headers.items():
                # Skip host header as curl sets it automatically
                if key.lower() != "host":
                    cmd.extend(["-H", f"{key}: {value}"])
        
        # Add request body to curl command
        # The requests library may provide body as bytes or str. Since we're
        # passing this through kubectl exec -> curl, we need it as a string.
        # Writing to a temp file inside the pod would require additional exec
        # calls, so we pass the body inline via --data-raw instead.
        if request.body:
            if isinstance(request.body, bytes):
                body_str = request.body.decode("utf-8", errors="replace")
            else:
                body_str = request.body
            cmd.extend(["--data-raw", body_str])
        
        # Add URL
        cmd.append(request.url)
        
        # Add timeout
        effective_timeout = timeout if timeout else self.timeout
        if isinstance(effective_timeout, tuple):
            effective_timeout = effective_timeout[1]  # Use read timeout
        cmd.extend(["--max-time", str(effective_timeout)])
        
        # For self-signed certs, add -k flag
        if not verify:
            cmd.append("-k")
        
        # Execute curl in pod
        result = exec_in_pod_raw(
            self.namespace,
            self.pod,
            cmd,
            container=self.container,
            timeout=effective_timeout + 10,  # Add buffer for kubectl overhead
        )
        
        # Parse the raw HTTP response
        return self._parse_curl_response(result, request)
    
    def _parse_curl_response(
        self,
        result: subprocess.CompletedProcess,
        request: requests.PreparedRequest,
    ) -> requests.Response:
        """Parse curl -i output into a requests.Response object.
        
        Uses http.client to parse the raw HTTP response and HTTPAdapter.build_response()
        to construct the Response properly.
        """
        # Handle curl errors
        if result.returncode != 0:
            raise requests.exceptions.ConnectionError(
                f"curl failed (exit {result.returncode}): {result.stderr or 'Connection failed'}"
            )
        
        raw_output = result.stdout
        if not raw_output:
            raise requests.exceptions.ConnectionError("curl returned empty response")
        
        # Normalize line endings to \r\n for HTTP parsing
        # HTTP spec requires \r\n, but curl output may have \n depending on server/platform
        # Use regex to normalize only \n that aren't already part of \r\n
        raw_output = re.sub(r'(?<!\r)\n', '\r\n', raw_output)
        
        # Handle HTTP/1.1 100 Continue - skip to the real response
        while raw_output.startswith("HTTP/") and " 100 " in raw_output.split("\r\n")[0]:
            # Find the end of the 100 Continue response and skip it
            parts = raw_output.split("\r\n\r\n", 1)
            if len(parts) > 1:
                raw_output = parts[1]
            else:
                break
        
        try:
            # Parse using http.client.HTTPResponse with a fake socket
            raw_bytes = raw_output.encode("utf-8")
            sock = _FakeSocket(raw_bytes)
            http_response = http.client.HTTPResponse(sock)
            http_response.begin()
            
            # Read the body
            body = http_response.read()
            
            # Build urllib3 HTTPResponse for build_response()
            # Setting preload_content=True ensures body is read into response
            urllib3_response = HTTPResponse(
                body=io.BytesIO(body),
                headers=dict(http_response.getheaders()),
                status=http_response.status,
                reason=http_response.reason,
                preload_content=True,
            )
            
            # Use HTTPAdapter's build_response to construct Response properly
            response = self.build_response(request, urllib3_response)
            
            # build_response with preload_content=True should set _content,
            # but ensure it's set for consistency
            if not response._content_consumed:
                response._content = body
            
            return response
            
        except Exception as e:
            raise requests.exceptions.ConnectionError(
                f"Failed to parse HTTP response: {e}"
            ) from e


def create_pod_session(
    namespace: str,
    pod: str,
    container: Optional[str] = None,
    headers: Optional[dict] = None,
    timeout: int = 60,
) -> requests.Session:
    """Create a requests.Session that routes through a pod.
    
    This is a convenience function to create a pre-configured session
    with the PodAdapter mounted for both http:// and https:// URLs.
    
    Args:
        namespace: Kubernetes namespace where the pod is running
        pod: Name of the pod to execute curl in
        container: Container name (if pod has multiple containers)
        headers: Default headers to include in all requests
        timeout: Default timeout for requests
    
    Returns:
        A requests.Session configured to route through the pod
    
    Example:
        session = create_pod_session("cost-onprem", "test-runner", container="runner")
        session.headers["X-Rh-Identity"] = identity_header
        
        response = session.get("http://koku-api:8000/api/v1/status/")
        assert response.ok
        data = response.json()
    """
    session = requests.Session()
    adapter = PodAdapter(namespace, pod, container=container, timeout=timeout)
    session.mount("http://", adapter)
    session.mount("https://", adapter)
    
    if headers:
        session.headers.update(headers)
    
    return session


# =============================================================================
# Database Utilities
# =============================================================================


def execute_db_query(
    namespace: str,
    pod_name: str,
    database: str,
    user: str,
    query: str,
    password: Optional[str] = None,
) -> Optional[list[tuple]]:
    """Execute a SQL query via kubectl exec and return results."""
    try:
        env_prefix = []
        if password:
            env_prefix = ["env", f"PGPASSWORD={password}"]
        
        cmd = env_prefix + [
            "psql", "-U", user, "-d", database,
            "-t", "-A", "-F", "|",
            "-c", query
        ]
        
        result = exec_in_pod(namespace, pod_name, cmd, timeout=120)
        if not result:
            return None
        
        # Parse pipe-delimited output
        rows = []
        for line in result.strip().split("\n"):
            if line:
                rows.append(tuple(line.split("|")))
        return rows
    except Exception:
        return None


# =============================================================================
# Authentication Utilities
# =============================================================================


def create_rh_identity_header(org_id: str, account_number: str = None) -> str:
    """Create X-Rh-Identity header value for Koku authentication.

    The X-Rh-Identity header is a base64-encoded JSON structure required by
    Koku middleware for tenant identification and authorization.
    
    NOTE: The "username" field in the identity JSON is NOT a Keycloak credential.
    It's a placeholder value in the identity claim structure used for internal
    API testing. It doesn't need to match any real user.

    Args:
        org_id: Organization ID for the tenant
        account_number: Account number (defaults to org_id if not provided)

    Returns:
        Base64-encoded identity JSON string
    """
    if account_number is None:
        account_number = org_id

    identity_json = {
        "org_id": org_id,
        "identity": {
            "org_id": org_id,
            "account_number": account_number,
            "type": "User",
            "user": {
                "username": "admin",
                "email": "admin@test.com",
                "is_org_admin": True,
            },
        },
        "entitlements": {
            "cost_management": {
                "is_entitled": True,
            },
        },
    }

    return base64.b64encode(json.dumps(identity_json).encode()).decode()


_UNSET = object()


def create_identity_header_custom(
    org_id: str,
    is_org_admin: bool = True,
    username: str = "admin",
    email: object = _UNSET,
    entitlements: Optional[dict] = None,
    account_number: Optional[str] = None,
) -> str:
    """Create X-Rh-Identity header with customizable fields for error testing.

    This function allows creating identity headers with various configurations
    to test authentication error scenarios.
    
    NOTE: The "username" field in the identity JSON is NOT a Keycloak credential.
    It's a placeholder value in the identity claim structure used for internal
    API testing. It doesn't need to match any real user.

    Args:
        org_id: Organization ID for the tenant
        is_org_admin: Whether the user is an org admin (default: True)
        username: Username for the identity (must match RBAC principal for per-user tests)
        email: User email (default: {username}@example.com). Pass None to omit.
        entitlements: Custom entitlements dict (default: cost_management is_entitled=True)
        account_number: Account number (defaults to org_id if not provided)

    Returns:
        Base64-encoded identity JSON string
    """
    if account_number is None:
        account_number = org_id

    if email is _UNSET:
        email = f"{username}@example.com"

    if entitlements is None:
        entitlements = {
            "cost_management": {
                "is_entitled": True,
            },
        }

    user_dict: dict[str, Any] = {
        "username": username,
        "is_org_admin": is_org_admin,
    }
    if email is not None:
        user_dict["email"] = email

    identity_json = {
        "org_id": org_id,
        "identity": {
            "org_id": org_id,
            "account_number": account_number,
            "type": "User",
            "user": user_dict,
        },
        "entitlements": entitlements,
    }

    return base64.b64encode(json.dumps(identity_json).encode()).decode()


def check_service_exists(namespace: str, service_name: str) -> bool:
    """Check if Kubernetes service exists using oc get.

    Args:
        namespace: Kubernetes namespace
        service_name: Name of the service

    Returns:
        True if service exists, False otherwise
    """
    try:
        result = run_oc_command([
            "get", "service", service_name, "-n", namespace,
            "-o", "jsonpath={.metadata.name}"
        ], check=False)
        return result.returncode == 0 and result.stdout.strip() == service_name
    except subprocess.CalledProcessError:
        return False


def check_deployment_exists(namespace: str, deployment_name: str) -> bool:
    """Check if Kubernetes deployment exists using oc get.

    Args:
        namespace: Kubernetes namespace
        deployment_name: Name of the deployment

    Returns:
        True if deployment exists, False otherwise
    """
    try:
        result = run_oc_command([
            "get", "deployment", deployment_name, "-n", namespace,
            "-o", "jsonpath={.metadata.name}"
        ], check=False)
        return result.returncode == 0 and result.stdout.strip() == deployment_name
    except subprocess.CalledProcessError:
        return False


# =============================================================================
# Upload Package Creation
# =============================================================================


def create_upload_package(
    csv_data: str,
    cluster_id: str,
    start_date: Optional[datetime] = None,
    end_date: Optional[datetime] = None,
) -> str:
    """Create a tar.gz upload package with CSV and manifest.
    
    Args:
        csv_data: CSV content as string
        cluster_id: Unique cluster identifier
        start_date: Start date for the report period (required for summary processing)
        end_date: End date for the report period (required for summary processing)
    
    Returns:
        Path to the created tar.gz file
        
    IMPORTANT: The manifest.json MUST include 'start' and 'end' fields for Koku
    to trigger summary processing. Without these fields, Koku will log:
    "missing start or end dates - cannot summarize ocp reports"
    """
    from datetime import timedelta
    
    temp_dir = tempfile.mkdtemp()
    csv_file = Path(temp_dir) / "openshift_usage_report.csv"
    manifest_file = Path(temp_dir) / "manifest.json"
    tar_file = Path(temp_dir) / "cost-mgmt.tar.gz"

    # Write CSV
    csv_file.write_text(csv_data)

    # Calculate date range if not provided
    now = datetime.now(timezone.utc)
    if start_date is None:
        # Default to yesterday
        start_date = now - timedelta(days=1)
    if end_date is None:
        # Default to today
        end_date = now
    
    # Ensure dates are timezone-aware
    if start_date.tzinfo is None:
        start_date = start_date.replace(tzinfo=timezone.utc)
    if end_date.tzinfo is None:
        end_date = end_date.replace(tzinfo=timezone.utc)

    # Write manifest - MUST include start and end for summary processing
    manifest = {
        "uuid": str(uuid.uuid4()),
        "cluster_id": cluster_id,
        "cluster_alias": f"e2e-source-{cluster_id[-8:]}",
        "date": now.isoformat(),
        "files": ["openshift_usage_report.csv"],
        "resource_optimization_files": ["openshift_usage_report.csv"],
        "certified": True,
        "operator_version": "1.0.0",
        "daily_reports": False,
        # CRITICAL: These fields are required for Koku to trigger summary processing
        "start": start_date.isoformat(),
        "end": end_date.isoformat(),
    }
    manifest_file.write_text(json.dumps(manifest, indent=2))

    # Create tar.gz
    with tarfile.open(tar_file, "w:gz") as tar:
        tar.add(csv_file, arcname="openshift_usage_report.csv")
        tar.add(manifest_file, arcname="manifest.json")

    return str(tar_file)


def create_upload_package_from_files(
    pod_usage_files: list[str],
    ros_usage_files: list[str],
    cluster_id: str,
    start_date: Optional[datetime] = None,
    end_date: Optional[datetime] = None,
    node_label_files: Optional[list[str]] = None,
    namespace_label_files: Optional[list[str]] = None,
) -> str:
    """Create a tar.gz upload package from NISE-generated files.
    
    This function creates a proper upload package with separate file lists
    for cost management (pod_usage) and resource optimization (ros_usage).
    
    Args:
        pod_usage_files: List of paths to pod_usage CSV files (for Koku cost management)
        ros_usage_files: List of paths to ros_usage CSV files (for ROS processor)
        cluster_id: Unique cluster identifier
        start_date: Start date for the report period
        end_date: End date for the report period
        node_label_files: Optional list of paths to node label CSV files
        namespace_label_files: Optional list of paths to namespace label CSV files
    
    Returns:
        Path to the created tar.gz file
    """
    from datetime import timedelta
    import os
    
    temp_dir = tempfile.mkdtemp()
    manifest_file = Path(temp_dir) / "manifest.json"
    tar_file = Path(temp_dir) / "cost-mgmt.tar.gz"

    # Calculate date range if not provided
    now = datetime.now(timezone.utc)
    if start_date is None:
        start_date = now - timedelta(days=1)
    if end_date is None:
        end_date = now
    
    # Ensure dates are timezone-aware
    if start_date.tzinfo is None:
        start_date = start_date.replace(tzinfo=timezone.utc)
    if end_date.tzinfo is None:
        end_date = end_date.replace(tzinfo=timezone.utc)

    # Get just the filenames for the manifest
    pod_filenames = [os.path.basename(f) for f in pod_usage_files]
    ros_filenames = [os.path.basename(f) for f in ros_usage_files]
    node_label_filenames = [os.path.basename(f) for f in (node_label_files or [])]
    namespace_label_filenames = [os.path.basename(f) for f in (namespace_label_files or [])]
    
    # Combine all files for the manifest's "files" array
    # Koku processes all files listed here, including label files
    all_data_files = pod_filenames + node_label_filenames + namespace_label_filenames

    # Write manifest with separate file lists
    manifest = {
        "uuid": str(uuid.uuid4()),
        "cluster_id": cluster_id,
        "cluster_alias": f"e2e-source-{cluster_id[-8:]}",
        "date": now.isoformat(),
        "files": all_data_files,  # All data files for Koku cost management (pod, node labels, namespace labels)
        "resource_optimization_files": ros_filenames,  # Container-level data for ROS
        "certified": True,
        "operator_version": "1.0.0",
        "daily_reports": False,
        "start": start_date.isoformat(),
        "end": end_date.isoformat(),
    }
    manifest_file.write_text(json.dumps(manifest, indent=2))

    # Create tar.gz with all files
    with tarfile.open(tar_file, "w:gz") as tar:
        # Add pod usage files
        for filepath in pod_usage_files:
            tar.add(filepath, arcname=os.path.basename(filepath))
        # Add ROS usage files
        for filepath in ros_usage_files:
            tar.add(filepath, arcname=os.path.basename(filepath))
        # Add node label files if provided
        if node_label_files:
            for filepath in node_label_files:
                tar.add(filepath, arcname=os.path.basename(filepath))
        # Add namespace label files if provided
        if namespace_label_files:
            for filepath in namespace_label_files:
                tar.add(filepath, arcname=os.path.basename(filepath))
        # Add manifest
        tar.add(manifest_file, arcname="manifest.json")

    return str(tar_file)


# =============================================================================
# Helm Utilities
# =============================================================================


def run_helm_command(
    args: list[str],
    check: bool = True,
    timeout: int = 120,
) -> subprocess.CompletedProcess:
    """Run a helm command and return the result."""
    cmd = ["helm"] + args
    return subprocess.run(
        cmd,
        capture_output=True,
        text=True,
        check=check,
        timeout=timeout,
    )


def helm_lint(chart_path: str) -> tuple[bool, str]:
    """Run helm lint on a chart.
    
    Returns:
        Tuple of (success, output)
    """
    try:
        result = run_helm_command(["lint", chart_path], check=False)
        success = result.returncode == 0
        output = result.stdout + result.stderr
        return success, output
    except subprocess.TimeoutExpired:
        return False, "Helm lint timed out"


def helm_template(
    chart_path: str,
    release_name: str = "test-release",
    values_file: Optional[str] = None,
    set_values: Optional[dict] = None,
) -> tuple[bool, str]:
    """Run helm template on a chart.
    
    Returns:
        Tuple of (success, rendered_yaml)
    """
    try:
        args = ["template", release_name, chart_path]
        if values_file:
            args.extend(["-f", values_file])
        if set_values:
            for key, value in set_values.items():
                args.extend(["--set", f"{key}={value}"])
        
        result = run_helm_command(args, check=False)
        success = result.returncode == 0
        return success, result.stdout if success else result.stderr
    except subprocess.TimeoutExpired:
        return False, "Helm template timed out"


# =============================================================================
# Validation Helpers
# =============================================================================


def check_pod_exists(namespace: str, label: str) -> bool:
    """Check if a pod with the given label exists."""
    return get_pod_by_label(namespace, label) is not None


def check_pod_ready(namespace: str, label: str) -> bool:
    """Check if any pod with the given label is ready.

    Uses field-selector to only consider Running pods, avoiding false negatives
    when a Pending pod (e.g. from a stale ReplicaSet) sorts before a Running one.
    """
    try:
        result = run_oc_command([
            "get", "pods", "-n", namespace,
            "-l", label,
            "--field-selector=status.phase=Running",
            "-o", "jsonpath={.items[0].status.conditions[?(@.type=='Ready')].status}"
        ], check=False)
        return result.stdout.strip() == "True"
    except subprocess.CalledProcessError:
        return False


def wait_for_condition(
    check_func,
    timeout: int = 300,
    interval: int = 10,
    description: str = "condition",
) -> bool:
    """Wait for a condition to become true.

    Args:
        check_func: Callable that returns True when condition is met
        timeout: Maximum wait time in seconds
        interval: Check interval in seconds
        description: Description for logging

    Returns:
        True if condition was met, False if timeout
    """
    import time

    start_time = time.time()
    while time.time() - start_time < timeout:
        if check_func():
            return True
        time.sleep(interval)
    return False


# =============================================================================
# Log Validation Utilities
# =============================================================================


class LogSearchResult:
    """Result of a log search operation.
    
    Attributes:
        found: Whether the pattern was found
        matches: List of matching log lines
        line_count: Total number of lines searched
        pattern: The pattern that was searched for
    """
    
    def __init__(
        self,
        found: bool,
        matches: list[str],
        line_count: int,
        pattern: str,
    ):
        self.found = found
        self.matches = matches
        self.line_count = line_count
        self.pattern = pattern
    
    def __bool__(self) -> bool:
        return self.found
    
    def __repr__(self) -> str:
        return f"LogSearchResult(found={self.found}, matches={len(self.matches)}, pattern={self.pattern!r})"


def get_pod_logs(
    namespace: str,
    pod_name: Optional[str] = None,
    label: Optional[str] = None,
    container: Optional[str] = None,
    since: Optional[str] = None,
    tail: Optional[int] = None,
    previous: bool = False,
) -> Optional[str]:
    """Get logs from a pod.
    
    Args:
        namespace: Kubernetes namespace
        pod_name: Name of the pod (mutually exclusive with label)
        label: Label selector to find pod (mutually exclusive with pod_name)
        container: Container name (required if pod has multiple containers)
        since: Only return logs newer than a relative duration (e.g., "5m", "1h")
        tail: Number of lines from the end of logs to show
        previous: Get logs from previous container instance
    
    Returns:
        Log content as string, or None if failed
    
    Example:
        # Get last 100 lines from koku-api pod
        logs = get_pod_logs("cost-onprem", label="app.kubernetes.io/component=cost-management-api", tail=100)
        
        # Get logs from specific container in last 5 minutes
        logs = get_pod_logs("cost-onprem", pod_name="koku-migrate-xyz", container="migrate", since="5m")
    """
    # Resolve pod name from label if needed
    if pod_name is None and label is not None:
        pod_name = get_pod_by_label(namespace, label)
        if pod_name is None:
            return None
    elif pod_name is None:
        raise ValueError("Either pod_name or label must be provided")
    
    args = ["logs", "-n", namespace, pod_name]
    
    if container:
        args.extend(["-c", container])
    if since:
        args.extend(["--since", since])
    if tail:
        args.extend(["--tail", str(tail)])
    if previous:
        args.append("--previous")
    
    try:
        result = run_oc_command(args, check=False, timeout=120)
        if result.returncode == 0:
            return result.stdout
        return None
    except subprocess.TimeoutExpired:
        return None


def get_job_logs(
    namespace: str,
    job_name: str,
    container: Optional[str] = None,
    since: Optional[str] = None,
    tail: Optional[int] = None,
) -> Optional[str]:
    """Get logs from a Kubernetes Job's pod.
    
    This is a convenience function that finds the pod created by a Job
    and retrieves its logs.
    
    Args:
        namespace: Kubernetes namespace
        job_name: Name of the Job
        container: Container name (if job pod has multiple containers)
        since: Only return logs newer than a relative duration
        tail: Number of lines from the end of logs to show
    
    Returns:
        Log content as string, or None if failed
    
    Example:
        # Get migration job logs
        logs = get_job_logs("cost-onprem", "cost-onprem-koku-migrate", container="migrate")
    """
    # Find pod created by the job
    label = f"job-name={job_name}"
    return get_pod_logs(
        namespace,
        label=label,
        container=container,
        since=since,
        tail=tail,
    )


def search_logs(
    logs: str,
    pattern: str,
    case_sensitive: bool = True,
    regex: bool = False,
) -> LogSearchResult:
    """Search for a pattern in log content.
    
    Args:
        logs: Log content to search
        pattern: Pattern to search for (string or regex)
        case_sensitive: Whether search is case-sensitive
        regex: Whether pattern is a regular expression
    
    Returns:
        LogSearchResult with match information
    
    Example:
        logs = get_pod_logs("cost-onprem", label="app.kubernetes.io/component=cost-management-api")
        result = search_logs(logs, "Skipping hive database creation")
        assert result.found, "Expected log message not found"
    """
    if not logs:
        return LogSearchResult(found=False, matches=[], line_count=0, pattern=pattern)
    
    lines = logs.split("\n")
    matches = []
    
    if regex:
        flags = 0 if case_sensitive else re.IGNORECASE
        compiled = re.compile(pattern, flags)
        for line in lines:
            if compiled.search(line):
                matches.append(line)
    else:
        search_pattern = pattern if case_sensitive else pattern.lower()
        for line in lines:
            search_line = line if case_sensitive else line.lower()
            if search_pattern in search_line:
                matches.append(line)
    
    return LogSearchResult(
        found=len(matches) > 0,
        matches=matches,
        line_count=len(lines),
        pattern=pattern,
    )


def assert_log_contains(
    logs: str,
    pattern: str,
    message: Optional[str] = None,
    case_sensitive: bool = True,
    regex: bool = False,
) -> LogSearchResult:
    """Assert that logs contain a pattern.
    
    Args:
        logs: Log content to search
        pattern: Pattern that must be present
        message: Custom assertion message
        case_sensitive: Whether search is case-sensitive
        regex: Whether pattern is a regular expression
    
    Returns:
        LogSearchResult for further inspection
    
    Raises:
        AssertionError: If pattern is not found
    
    Example:
        logs = get_job_logs("cost-onprem", "cost-onprem-koku-migrate", container="migrate")
        assert_log_contains(logs, "Migrations completed successfully")
    """
    result = search_logs(logs, pattern, case_sensitive=case_sensitive, regex=regex)
    
    if not result.found:
        error_msg = message or f"Expected pattern not found in logs: {pattern!r}"
        raise AssertionError(error_msg)
    
    return result


def assert_log_not_contains(
    logs: str,
    pattern: str,
    message: Optional[str] = None,
    case_sensitive: bool = True,
    regex: bool = False,
) -> LogSearchResult:
    """Assert that logs do NOT contain a pattern.
    
    Args:
        logs: Log content to search
        pattern: Pattern that must NOT be present
        message: Custom assertion message
        case_sensitive: Whether search is case-sensitive
        regex: Whether pattern is a regular expression
    
    Returns:
        LogSearchResult for further inspection
    
    Raises:
        AssertionError: If pattern is found
    
    Example:
        logs = get_job_logs("cost-onprem", "cost-onprem-koku-migrate", container="migrate")
        assert_log_not_contains(logs, "Migration failed")
    """
    result = search_logs(logs, pattern, case_sensitive=case_sensitive, regex=regex)
    
    if result.found:
        error_msg = message or f"Unexpected pattern found in logs: {pattern!r}"
        if result.matches:
            error_msg += f"\nMatching lines:\n" + "\n".join(f"  {m}" for m in result.matches[:5])
            if len(result.matches) > 5:
                error_msg += f"\n  ... and {len(result.matches) - 5} more"
        raise AssertionError(error_msg)
    
    return result


def validate_logs(
    logs: str,
    must_contain: Optional[list[str]] = None,
    must_not_contain: Optional[list[str]] = None,
    case_sensitive: bool = True,
) -> dict[str, LogSearchResult]:
    """Validate logs against multiple patterns.
    
    This is a convenience function for validating multiple conditions at once.
    
    Args:
        logs: Log content to validate
        must_contain: List of patterns that must be present
        must_not_contain: List of patterns that must NOT be present
        case_sensitive: Whether searches are case-sensitive
    
    Returns:
        Dict mapping pattern to LogSearchResult
    
    Raises:
        AssertionError: If any validation fails (with all failures listed)
    
    Example:
        logs = get_job_logs("cost-onprem", "cost-onprem-koku-migrate", container="migrate")
        validate_logs(
            logs,
            must_contain=["Migrations completed successfully", "Migration lock"],
            must_not_contain=["Migration failed", "ERROR", "Traceback"],
        )
    """
    results = {}
    failures = []
    
    for pattern in (must_contain or []):
        result = search_logs(logs, pattern, case_sensitive=case_sensitive)
        results[pattern] = result
        if not result.found:
            failures.append(f"Missing required pattern: {pattern!r}")
    
    for pattern in (must_not_contain or []):
        result = search_logs(logs, pattern, case_sensitive=case_sensitive)
        results[pattern] = result
        if result.found:
            failures.append(f"Found forbidden pattern: {pattern!r}")
            if result.matches:
                failures.append(f"  First match: {result.matches[0][:200]}")
    
    if failures:
        raise AssertionError("Log validation failed:\n" + "\n".join(failures))
    
    return results


def get_deployment_logs(
    namespace: str,
    deployment_name: str,
    container: Optional[str] = None,
    since: Optional[str] = None,
    tail: Optional[int] = None,
) -> Optional[str]:
    """Get logs from pods in a Deployment.
    
    Args:
        namespace: Kubernetes namespace
        deployment_name: Name of the Deployment
        container: Container name (if pods have multiple containers)
        since: Only return logs newer than a relative duration
        tail: Number of lines from the end of logs to show
    
    Returns:
        Log content as string, or None if failed
    
    Example:
        logs = get_deployment_logs("cost-onprem", "cost-onprem-koku-api", tail=500)
    """
    # Get the deployment's selector labels as JSON
    try:
        result = run_oc_command([
            "get", "deployment", deployment_name, "-n", namespace,
            "-o", "json"
        ], check=False)
        
        if result.returncode != 0 or not result.stdout.strip():
            return None
        
        # Parse the full deployment JSON and extract matchLabels
        deployment = json.loads(result.stdout)
        labels = deployment.get("spec", {}).get("selector", {}).get("matchLabels", {})
        
        if not labels:
            return None
        
        label_selector = ",".join(f"{k}={v}" for k, v in labels.items())
        
        return get_pod_logs(
            namespace,
            label=label_selector,
            container=container,
            since=since,
            tail=tail,
        )
    except (subprocess.CalledProcessError, json.JSONDecodeError):
        return None


def wait_for_deployment_replicas(
    namespace: str,
    label_selector: str,
    expected_replicas: int,
    timeout: int = 60,
    interval: int = 2,
) -> bool:
    """Wait for a deployment to reach the expected replica count.

    Args:
        namespace: Kubernetes namespace
        label_selector: Label selector (e.g., "app.kubernetes.io/component=rbac-api")
        expected_replicas: Expected number of ready replicas
        timeout: Maximum wait time in seconds
        interval: Check interval in seconds

    Returns:
        True if replicas reached expected count, False if timeout

    Example:
        # Wait for scale-down to 0
        wait_for_deployment_replicas(ns, "app=rbac", 0, timeout=30)

        # Wait for scale-up to 1
        wait_for_deployment_replicas(ns, "app=rbac", 1, timeout=60)
    """
    import time

    start_time = time.time()
    while time.time() - start_time < timeout:
        try:
            # Check ready replicas via deployment status
            result = run_oc_command(
                [
                    "get", "deployment",
                    "-n", namespace,
                    "-l", label_selector,
                    "-o", "jsonpath={.items[0].status.readyReplicas}",
                ],
                check=False,
            )

            ready_str = result.stdout.strip()
            # If no readyReplicas field (deployment scaled to 0), treat as 0
            ready_count = int(ready_str) if ready_str else 0

            if ready_count == expected_replicas:
                return True

        except (subprocess.CalledProcessError, ValueError):
            # Transient errors or missing deployment - keep polling
            pass

        time.sleep(interval)

    return False
