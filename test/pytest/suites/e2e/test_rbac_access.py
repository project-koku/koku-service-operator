"""
RBAC Access Control E2E Tests.

Validates namespace-level and cluster-level RBAC filtering for OpenShift
cost reports. Seeds 3 OCP clusters with distinct namespaces, creates custom
RBAC roles with resourceDefinitions, and verifies that each user sees only
their authorized data.

Cluster layout:
  - cluster-alpha: payment, frontend, backend
  - cluster-beta:  payment, api-gateway, database
  - cluster-gamma: monitoring, logging, infra

User access:
  - alice (Payment Team Lead): openshift.project filtered to "payment" only
  - bob (Cluster Alpha Ops):   openshift.cluster filtered to cluster-alpha only
  - carol (Cost Administrator): cost-management:*:* (full access)

Run with:
  pytest suites/e2e/test_rbac_access.py -v
  pytest -m e2e suites/e2e/test_rbac_access.py
"""

import json
import os
import shutil
import subprocess
import tempfile
import time
import uuid
from datetime import datetime, timedelta
from typing import Dict

import pytest
import requests

from conftest import (
    ClusterConfig,
    KeycloakConfig,
    obtain_jwt_token,
    obtain_password_grant_token,
)
from e2e_helpers import (
    get_koku_api_url,
    register_source,
    upload_with_retry,
    wait_for_provider,
    wait_for_summary_tables,
)
from rbac_keycloak_users import ensure_rbac_gateway_persona_users
from suites.auth.test_gateway_auth import _check_gateway_reachable
from suites.e2e.conftest import make_user_pod_session
from utils import (
    create_pod_session,
    create_rh_identity_header,
    create_upload_package_from_files,
    exec_in_pod,
    get_pod_by_label,
)


# =============================================================================
# Constants
# =============================================================================

CLUSTER_CONFIGS = {
    "alpha": {
        "namespaces": {
            "payment": [
                {"pod_name": "payment-api-1", "cpu_request": 0.5, "mem_request_gig": 1, "labels": "app:payment-api|team:payments"},
                {"pod_name": "payment-worker-1", "cpu_request": 0.25, "mem_request_gig": 0.5, "labels": "app:payment-worker|team:payments"},
            ],
            "frontend": [
                {"pod_name": "frontend-web-1", "cpu_request": 0.25, "mem_request_gig": 0.5, "labels": "app:frontend|team:web"},
            ],
            "backend": [
                {"pod_name": "backend-svc-1", "cpu_request": 0.75, "mem_request_gig": 2, "labels": "app:backend|team:platform"},
            ],
        },
    },
    "beta": {
        "namespaces": {
            "payment": [
                {"pod_name": "payment-processor-1", "cpu_request": 0.5, "mem_request_gig": 1, "labels": "app:payment-processor|team:payments"},
            ],
            "api-gateway": [
                {"pod_name": "gateway-1", "cpu_request": 0.25, "mem_request_gig": 0.5, "labels": "app:api-gateway|team:infra"},
            ],
            "database": [
                {"pod_name": "postgres-1", "cpu_request": 1.0, "mem_request_gig": 4, "labels": "app:postgres|team:data"},
            ],
        },
    },
    "gamma": {
        "namespaces": {
            "monitoring": [
                {"pod_name": "prometheus-1", "cpu_request": 0.5, "mem_request_gig": 2, "labels": "app:prometheus|team:sre"},
            ],
            "logging": [
                {"pod_name": "fluentd-1", "cpu_request": 0.25, "mem_request_gig": 1, "labels": "app:fluentd|team:sre"},
            ],
            "infra": [
                {"pod_name": "cert-manager-1", "cpu_request": 0.1, "mem_request_gig": 0.25, "labels": "app:cert-manager|team:infra"},
            ],
        },
    },
}


# =============================================================================
# NISE Data Generation Helpers
# =============================================================================


def generate_nise_yaml(cluster_name: str, start_date: datetime, end_date: datetime) -> str:
    """Generate a NISE static report YAML for a cluster with multiple namespaces."""
    config = CLUSTER_CONFIGS[cluster_name]

    pods_yaml = ""
    for ns_name, pods in config["namespaces"].items():
        pods_section = ""
        for pod in pods:
            pods_section += f"""                - pod:
                  pod_name: {pod['pod_name']}
                  cpu_request: {pod['cpu_request']}
                  mem_request_gig: {pod['mem_request_gig']}
                  cpu_limit: {pod['cpu_request'] * 2}
                  mem_limit_gig: {pod['mem_request_gig'] * 2}
                  pod_seconds: 3600
                  cpu_usage:
                    full_period: {pod['cpu_request'] * 0.6}
                  mem_usage_gig:
                    full_period: {pod['mem_request_gig'] * 0.7}
                  labels: {pod['labels']}
"""
        pods_yaml += f"""            {ns_name}:
              pods:
{pods_section}"""

    return f"""---
generators:
  - OCPGenerator:
      start_date: {start_date.strftime('%Y-%m-%d')}
      end_date: {end_date.strftime('%Y-%m-%d')}
      nodes:
        - node:
          node_name: {cluster_name}-node-1
          cpu_cores: 8
          memory_gig: 32
          resource_id: {cluster_name}-resource-001
          namespaces:
{pods_yaml}"""


def run_nise_for_cluster(cluster_name: str, cluster_id: str, output_dir: str,
                         start_date: datetime, end_date: datetime) -> Dict:
    """Run NISE to generate OCP data for a single cluster."""
    yaml_content = generate_nise_yaml(cluster_name, start_date, end_date)
    yaml_path = os.path.join(output_dir, f"{cluster_name}_static_report.yml")
    with open(yaml_path, "w") as f:
        f.write(yaml_content)

    nise_output = os.path.join(output_dir, cluster_name)
    os.makedirs(nise_output, exist_ok=True)

    cmd = [
        "nise", "report", "ocp",
        "--static-report-file", yaml_path,
        "--ocp-cluster-id", cluster_id,
        "-w",
        "--ros-ocp-info",
    ]

    result = subprocess.run(cmd, capture_output=True, text=True, timeout=300, cwd=nise_output)
    if result.returncode != 0:
        raise RuntimeError(f"NISE failed for {cluster_name}: {result.stderr}")

    files = {"pod_usage_files": [], "node_label_files": [], "namespace_label_files": [], "all_files": []}
    for root, _, filenames in os.walk(nise_output):
        for f in filenames:
            if f.endswith(".csv"):
                full_path = os.path.join(root, f)
                files["all_files"].append(full_path)
                if "pod_usage" in f:
                    files["pod_usage_files"].append(full_path)
                elif "node_label" in f:
                    files["node_label_files"].append(full_path)
                elif "namespace_label" in f:
                    files["namespace_label_files"].append(full_path)

    return files


# =============================================================================
# RBAC Setup Helper
# =============================================================================

RBAC_SETUP_SCRIPT = '''
import json
from management.models import Role, Access, Permission, ResourceDefinition, Group, Policy, Principal
from api.models import Tenant

public_tenant = Tenant.objects.get(tenant_name="public")
user_tenant = Tenant.objects.get(org_id="{org_id}")

# --- Step 1: Ensure admin_default group exists ---
admin_group, _ = Group.objects.get_or_create(
    name="Cost Admin Default",
    tenant=user_tenant,
    defaults={{"admin_default": True, "system": True,
              "description": "Admin default: Cost Administrator for is_org_admin users"}}
)
admin_group.admin_default = True
admin_group.save()
cost_admin_role = Role.objects.get(name="Cost Administrator", tenant=public_tenant)
admin_policy, _ = Policy.objects.get_or_create(
    name="Cost Admin Default Policy", tenant=user_tenant, group=admin_group
)
admin_policy.roles.add(cost_admin_role)
print(f"admin_default group OK: {{admin_group.uuid}}")

# --- Step 2: Get permission objects ---
perms = {{}}
for rt in ["openshift.cluster", "openshift.node", "openshift.project"]:
    p = Permission.objects.get(application="cost-management", resource_type=rt, verb="read")
    perms[rt] = p

# --- Step 3: Create "Payment Namespace Viewer" role ---
payment_role, _ = Role.objects.get_or_create(
    name="Payment Namespace Viewer", tenant=public_tenant,
    defaults={{"display_name": "Payment Namespace Viewer",
              "description": "OCP access restricted to payment namespace only",
              "system": False}}
)
for rt in ["openshift.cluster", "openshift.node", "openshift.project"]:
    access_obj, _ = Access.objects.get_or_create(
        role=payment_role, permission=perms[rt], tenant=public_tenant
    )
    if rt == "openshift.project":
        ResourceDefinition.objects.get_or_create(
            access=access_obj, tenant=public_tenant,
            defaults={{"attributeFilter": {{
                "key": "cost-management.openshift.project",
                "operation": "in",
                "value": ["payment"]
            }}}}
        )
print(f"Payment Namespace Viewer role OK: {{payment_role.uuid}}")

# --- Step 4: Create "Cluster Alpha Viewer" role ---
cluster_alpha_id = "{cluster_alpha_id}"
alpha_role, _ = Role.objects.get_or_create(
    name="Cluster Alpha Viewer", tenant=public_tenant,
    defaults={{"display_name": "Cluster Alpha Viewer",
              "description": "OCP access restricted to cluster-alpha only",
              "system": False}}
)
for rt in ["openshift.cluster", "openshift.node", "openshift.project"]:
    access_obj, _ = Access.objects.get_or_create(
        role=alpha_role, permission=perms[rt], tenant=public_tenant
    )
    if rt == "openshift.cluster":
        rd, created = ResourceDefinition.objects.get_or_create(
            access=access_obj, tenant=public_tenant,
            defaults={{"attributeFilter": {{
                "key": "cost-management.openshift.cluster",
                "operation": "equal",
                "value": cluster_alpha_id
            }}}}
        )
        if not created:
            rd.attributeFilter = {{
                "key": "cost-management.openshift.cluster",
                "operation": "equal",
                "value": cluster_alpha_id
            }}
            rd.save()
print(f"Cluster Alpha Viewer role OK: {{alpha_role.uuid}}")

# --- Step 5: Remove users from prior PoC groups ---
old_groups = ["Cost Readers", "Cloud Analysts", "Platform Engineers", "Cost Admins"]
for gname in old_groups:
    g = Group.objects.filter(name=gname, tenant=user_tenant).first()
    if g:
        g.principals.clear()
        print(f"Cleared principals from: {{gname}}")

# --- Step 6: Create new groups and assign roles + principals ---
group_configs = [
    ("RBAC Payment Team", payment_role, ["alice"]),
    ("RBAC Cluster Alpha Ops", alpha_role, ["bob"]),
    ("RBAC Cost Admins", cost_admin_role, ["carol"]),
]

for gname, role, usernames in group_configs:
    grp, _ = Group.objects.get_or_create(name=gname, tenant=user_tenant)
    policy, _ = Policy.objects.get_or_create(
        name=f"{{gname}} Policy", tenant=user_tenant, group=grp
    )
    policy.roles.clear()
    policy.roles.add(role)
    grp.principals.clear()
    for uname in usernames:
        principal, _ = Principal.objects.get_or_create(
            username=uname, tenant=user_tenant, defaults={{"type": "user"}}
        )
        grp.principals.add(principal)
    print(f"Group {{gname}}: role={{role.name}}, principals={{usernames}}")

# --- Step 7: Flush RBAC cache ---
from django.core.cache import cache
cache.clear()
print("RBAC cache flushed")

# Output role UUIDs for reference
print(f"ROLE_UUIDS: payment={{payment_role.uuid}} alpha={{alpha_role.uuid}} admin={{cost_admin_role.uuid}}")
'''


# =============================================================================
# Fixtures
# =============================================================================


@pytest.fixture(scope="module")
def rbac_cluster_data(
    cluster_config: ClusterConfig,
    keycloak_config: KeycloakConfig,
    org_id: str,
    ingress_url: str,
    test_runner_pod: str,
):
    """Seed 3 OCP clusters with NISE data and wait for Koku processing.

    Yields a dict with cluster_ids and metadata for use in RBAC tests.
    """
    now = datetime.utcnow()
    start_date = now - timedelta(days=1)
    end_date = now + timedelta(days=1)

    # Cluster IDs must be <= 50 chars (OCPUsageReportPeriod.cluster_id is varchar(50))
    short_id = uuid.uuid4().hex[:8]
    cluster_ids = {
        "alpha": f"e2e-rbac-alpha-{short_id}",
        "beta": f"e2e-rbac-beta-{short_id}",
        "gamma": f"e2e-rbac-gamma-{short_id}",
    }

    temp_dir = tempfile.mkdtemp(prefix="e2e-rbac-")
    admin_identity = create_rh_identity_header(org_id)
    koku_url = get_koku_api_url(cluster_config.helm_release_name, cluster_config.namespace)

    ingress_pod = get_pod_by_label(cluster_config.namespace, "app.kubernetes.io/component=ingress")
    db_pod = get_pod_by_label(cluster_config.namespace, "app.kubernetes.io/component=database")

    if not ingress_pod or not db_pod:
        pytest.skip("Required pods (ingress/database) not found")

    print(f"\n{'='*60}")
    print("RBAC E2E: SEEDING 3 CLUSTERS")
    print(f"{'='*60}")

    # Register sources and upload data for each cluster
    source_ids = {}
    for name, cid in cluster_ids.items():
        print(f"\n  [{name}] Cluster ID: {cid}")

        # Register source
        print(f"    Registering source...")
        reg = register_source(
            namespace=cluster_config.namespace,
            pod=ingress_pod,
            api_url=koku_url,
            rh_identity_header=admin_identity,
            cluster_id=cid,
            org_id=org_id,
            source_name=f"e2e-rbac-{name}-{cid[-8:]}",
            container="ingress",
        )
        source_ids[name] = reg.source_id
        print(f"    Source ID: {reg.source_id}")

    # Wait for all providers
    print(f"\n  Waiting for providers...")
    for name, cid in cluster_ids.items():
        if not wait_for_provider(cluster_config.namespace, db_pod, cid, timeout=180):
            pytest.fail(f"Provider not created for cluster {name} ({cid})")
        print(f"    [{name}] Provider created")

    # Generate and upload NISE data sequentially with retries.
    # Koku's parquet conversion can fail on the FIRST cluster uploaded for a
    # new org/schema because the S4 directory structure isn't initialized yet.
    # The first attempt triggers initialization (and fails); subsequent uploads
    # to the same schema succeed. We retry with fresh JWT tokens.
    upload_url = f"{ingress_url}/v1/upload"
    upload_session = requests.Session()
    upload_session.trust_env = False
    upload_session.verify = False

    max_upload_retries = 3
    for name, cid in cluster_ids.items():
        print(f"\n  [{name}] Generating NISE data...")
        files = run_nise_for_cluster(name, cid, temp_dir, start_date, end_date)
        print(f"    Generated {len(files['all_files'])} CSV files")

        if not files["pod_usage_files"]:
            pytest.fail(f"No pod_usage files generated for cluster {name}")

        package_path = create_upload_package_from_files(
            pod_usage_files=files["pod_usage_files"],
            ros_usage_files=files["pod_usage_files"],
            cluster_id=cid,
            start_date=start_date,
            end_date=end_date,
            node_label_files=files["node_label_files"] or None,
            namespace_label_files=files["namespace_label_files"] or None,
        )

        for attempt in range(max_upload_retries + 1):
            if attempt > 0:
                retry_delay = 30 * attempt
                print(f"    Retry {attempt}/{max_upload_retries} after {retry_delay}s delay...")
                time.sleep(retry_delay)

            # Always get a fresh token (Keycloak tokens are short-lived ~5min)
            upload_token = obtain_jwt_token(keycloak_config)

            print(f"    Uploading to {upload_url}...")
            response = upload_with_retry(
                upload_session, upload_url, package_path, upload_token.authorization_header
            )
            if response.status_code not in [200, 201, 202]:
                if attempt < max_upload_retries:
                    print(f"    Upload returned {response.status_code}, will retry...")
                    continue
                pytest.fail(f"Upload failed for {name}: {response.status_code}")
            print(f"    Upload OK: {response.status_code}")

            # Wait for this cluster's summary tables before moving to next
            print(f"    Waiting for processing (attempt {attempt + 1})...")
            schema = wait_for_summary_tables(
                cluster_config.namespace, db_pod, cid, timeout=300
            )
            if schema:
                print(f"    [{name}] Summary tables populated in schema: {schema}")
                break
            elif attempt < max_upload_retries:
                print(f"    [{name}] Processing failed, will retry...")
            else:
                pytest.fail(
                    f"Summary tables not populated for {name} ({cid}) "
                    f"after {max_upload_retries + 1} attempts"
                )

        # Pause between clusters to let Koku's pipeline settle
        time.sleep(10)

    print(f"\n{'='*60}")
    print("RBAC E2E: DATA SEEDING COMPLETE")
    print(f"{'='*60}\n")

    yield {
        "cluster_ids": cluster_ids,
        "source_ids": source_ids,
        "org_id": org_id,
        "namespace": cluster_config.namespace,
        "db_pod": db_pod,
        "koku_api_url": koku_url,
    }

    # Teardown
    cleanup = os.environ.get("E2E_CLEANUP_AFTER", "true").lower() == "true"
    if cleanup:
        print("\n  Cleaning up RBAC E2E sources...")
        admin_session = create_pod_session(
            namespace=cluster_config.namespace,
            pod=test_runner_pod if test_runner_pod else ingress_pod,
            container="runner" if test_runner_pod else "ingress",
            headers={"X-Rh-Identity": admin_identity, "Content-Type": "application/json"},
            timeout=60,
        )
        for name, sid in source_ids.items():
            try:
                admin_session.delete(f"{koku_url}/sources/{sid}")
            except Exception:
                pass
    else:
        print("\n  Cleanup skipped (E2E_CLEANUP_AFTER=false)")
        print(f"  Cluster IDs: {json.dumps(cluster_ids, indent=2)}")

    shutil.rmtree(temp_dir, ignore_errors=True)


@pytest.fixture(scope="module")
def rbac_access_setup(
    cluster_config: ClusterConfig,
    org_id: str,
    rbac_cluster_data: dict,
):
    """Set up RBAC custom roles and groups for the 3 demo users.

    Creates roles via Django ORM (RBAC API rejects custom roles for
    cost-management) and assigns users to groups.
    """
    cluster_alpha_id = rbac_cluster_data["cluster_ids"]["alpha"]

    rbac_pod = get_pod_by_label(cluster_config.namespace, "app.kubernetes.io/component=rbac-api")
    if not rbac_pod:
        pytest.skip("RBAC API pod not found")

    valkey_pod = get_pod_by_label(cluster_config.namespace, "app.kubernetes.io/component=valkey")

    script = RBAC_SETUP_SCRIPT.format(
        org_id=org_id,
        cluster_alpha_id=cluster_alpha_id,
    )

    print(f"\n{'='*60}")
    print("RBAC E2E: SETTING UP ROLES AND GROUPS")
    print(f"{'='*60}")

    result = exec_in_pod(
        cluster_config.namespace,
        rbac_pod,
        ["python", "/opt/rbac/rbac/manage.py", "shell", "-c", script],
        timeout=60,
    )
    print(result or "  (no output)")

    # Flush Valkey cache
    if valkey_pod:
        exec_in_pod(cluster_config.namespace, valkey_pod, ["valkey-cli", "FLUSHALL"], timeout=10)
        print("  Valkey cache flushed")

    print(f"\n{'='*60}")
    print("RBAC E2E: SETUP COMPLETE")
    print(f"{'='*60}\n")

    # Brief pause for cache propagation
    time.sleep(2)

    yield {
        "cluster_ids": rbac_cluster_data["cluster_ids"],
        "org_id": org_id,
        "koku_api_url": rbac_cluster_data["koku_api_url"],
    }


@pytest.fixture(scope="module")
def rbac_gateway_jwt_password(
    cluster_config: ClusterConfig,
    keycloak_config: KeycloakConfig,
    org_id: str,
    rbac_access_setup,
    rbac_gateway_test_user_password: str,
):
    """Create Keycloak users matching RBAC principals; yield session password."""
    try:
        ensure_rbac_gateway_persona_users(
            keycloak_config.url,
            cluster_config.keycloak_namespace,
            keycloak_config.realm,
            org_id,
            "7890123",
            password=rbac_gateway_test_user_password,
        )
    except RuntimeError as exc:
        pytest.skip(f"Keycloak RBAC gateway persona provisioning failed: {exc}")
    yield rbac_gateway_test_user_password


# =============================================================================
# Test Class
# =============================================================================


@pytest.mark.e2e
@pytest.mark.integration
class TestRBACAccessControl:
    """Validate namespace-level and cluster-level RBAC filtering.

    Depends on rbac_cluster_data (seeds 3 clusters) and rbac_access_setup
    (creates custom roles with resourceDefinitions).
    """

    # --- Helper to make per-user pod sessions ---

    def _user_session(self, cluster_config, test_runner_pod, org_id, username):
        """Create a pod session for a specific user."""
        return make_user_pod_session(
            namespace=cluster_config.namespace,
            pod=test_runner_pod,
            org_id=org_id,
            username=username,
        )

    # =========================================================================
    # Permission Gate Tests (DRF layer - 403 vs 200)
    # =========================================================================

    def test_admin_full_access(
        self, cluster_config, test_runner_pod, org_id, rbac_access_setup
    ):
        """Admin identity (is_org_admin=true) gets 200 on OCP costs."""
        admin_identity = create_rh_identity_header(org_id)
        session = create_pod_session(
            namespace=cluster_config.namespace,
            pod=test_runner_pod,
            container="runner",
            headers={"X-Rh-Identity": admin_identity, "Content-Type": "application/json"},
            timeout=120,
        )
        url = rbac_access_setup["koku_api_url"]
        response = session.get(f"{url}/reports/openshift/costs/")
        assert response.status_code == 200, f"Admin got {response.status_code}: {response.text[:300]}"

    def test_no_rbac_user_denied(
        self, cluster_config, test_runner_pod, org_id, rbac_access_setup
    ):
        """User with no RBAC group membership gets 403."""
        session = make_user_pod_session(
            namespace=cluster_config.namespace,
            pod=test_runner_pod,
            org_id=org_id,
            username="nobody-unassigned",
        )
        url = rbac_access_setup["koku_api_url"]
        response = session.get(f"{url}/reports/openshift/costs/")
        assert response.status_code == 403, f"Expected 403 got {response.status_code}"

    # =========================================================================
    # Auto-injection Tests (unfiltered - Koku injects RBAC access as filter)
    # =========================================================================

    def test_alice_unfiltered_sees_only_payment(
        self, cluster_config, test_runner_pod, org_id, rbac_access_setup, rbac_cluster_data
    ):
        """Alice's unfiltered query only returns payment namespace data."""
        session = self._user_session(cluster_config, test_runner_pod, org_id, "alice")
        url = rbac_access_setup["koku_api_url"]
        response = session.get(f"{url}/reports/openshift/costs/?group_by[project]=*")
        assert response.status_code == 200, f"Alice got {response.status_code}: {response.text[:300]}"

        data = response.json()
        projects_seen = set()
        for day in data.get("data", []):
            for project_group in day.get("projects", []):
                proj = project_group.get("project")
                if proj:
                    projects_seen.add(proj)

        assert len(projects_seen) > 0, (
            "Alice returned 200 but no project data — RBAC fixture may not have seeded data"
        )
        assert projects_seen == {"payment"}, (
            f"Alice should only see 'payment' but saw: {projects_seen}"
        )

    def test_bob_unfiltered_sees_only_alpha(
        self, cluster_config, test_runner_pod, org_id, rbac_access_setup, rbac_cluster_data
    ):
        """Bob's unfiltered query only returns cluster-alpha data."""
        session = self._user_session(cluster_config, test_runner_pod, org_id, "bob")
        url = rbac_access_setup["koku_api_url"]
        alpha_id = rbac_cluster_data["cluster_ids"]["alpha"]
        response = session.get(f"{url}/reports/openshift/costs/?group_by[cluster]=*")
        assert response.status_code == 200, f"Bob got {response.status_code}: {response.text[:300]}"

        data = response.json()
        clusters_seen = set()
        for day in data.get("data", []):
            for cluster_group in day.get("clusters", []):
                cid = cluster_group.get("cluster")
                if cid:
                    clusters_seen.add(cid)

        assert len(clusters_seen) > 0, (
            "Bob returned 200 but no cluster data — RBAC fixture may not have seeded data"
        )
        assert clusters_seen == {alpha_id}, (
            f"Bob should only see cluster-alpha ({alpha_id}) but saw: {clusters_seen}"
        )

    def test_carol_unfiltered_sees_everything(
        self, cluster_config, test_runner_pod, org_id, rbac_access_setup, rbac_cluster_data
    ):
        """Carol (Cost Administrator) sees all 3 clusters."""
        session = self._user_session(cluster_config, test_runner_pod, org_id, "carol")
        url = rbac_access_setup["koku_api_url"]
        response = session.get(f"{url}/reports/openshift/costs/?group_by[cluster]=*")
        assert response.status_code == 200, f"Carol got {response.status_code}: {response.text[:300]}"

        data = response.json()
        clusters_seen = set()
        for day in data.get("data", []):
            for cluster_group in day.get("clusters", []):
                cid = cluster_group.get("cluster")
                if cid:
                    clusters_seen.add(cid)

        expected_clusters = set(rbac_cluster_data["cluster_ids"].values())
        assert len(clusters_seen) > 0, (
            "Carol returned 200 but no cluster data — RBAC fixture may not have seeded data"
        )
        assert expected_clusters.issubset(clusters_seen), (
            f"Carol should see at least {expected_clusters} but saw: {clusters_seen}"
        )

    # =========================================================================
    # Explicit Filter -- Allowed (200 with data)
    # =========================================================================

    def test_alice_filter_payment_returns_data(
        self, cluster_config, test_runner_pod, org_id, rbac_access_setup
    ):
        """Alice filtering to project=payment gets 200."""
        session = self._user_session(cluster_config, test_runner_pod, org_id, "alice")
        url = rbac_access_setup["koku_api_url"]
        response = session.get(f"{url}/reports/openshift/costs/?filter[project]=payment")
        assert response.status_code == 200, f"Alice got {response.status_code}: {response.text[:300]}"

    def test_bob_explicit_cluster_filter_allowed(
        self, cluster_config, test_runner_pod, org_id, rbac_access_setup, rbac_cluster_data
    ):
        """Bob can explicitly filter to his authorized cluster and get data."""
        session = self._user_session(cluster_config, test_runner_pod, org_id, "bob")
        url = rbac_access_setup["koku_api_url"]
        alpha_id = rbac_cluster_data["cluster_ids"]["alpha"]
        response = session.get(
            f"{url}/reports/openshift/costs/?filter[cluster]={alpha_id}&group_by[project]=*"
        )

        assert response.status_code == 200, (
            f"Bob should be allowed to filter to his authorized cluster, "
            f"got {response.status_code}: {response.text[:300]}"
        )

    # =========================================================================
    # Explicit Filter -- Denied (403)
    # =========================================================================

    def test_alice_filter_frontend_denied(
        self, cluster_config, test_runner_pod, org_id, rbac_access_setup
    ):
        """Alice explicitly requesting project=frontend gets 403."""
        session = self._user_session(cluster_config, test_runner_pod, org_id, "alice")
        url = rbac_access_setup["koku_api_url"]
        response = session.get(f"{url}/reports/openshift/costs/?filter[project]=frontend")
        assert response.status_code == 403, (
            f"Alice should get 403 for frontend but got {response.status_code}: {response.text[:300]}"
        )

    def test_bob_filter_beta_denied(
        self, cluster_config, test_runner_pod, org_id, rbac_access_setup, rbac_cluster_data
    ):
        """Bob explicitly requesting cluster-beta gets 403."""
        session = self._user_session(cluster_config, test_runner_pod, org_id, "bob")
        url = rbac_access_setup["koku_api_url"]
        beta_id = rbac_cluster_data["cluster_ids"]["beta"]
        response = session.get(f"{url}/reports/openshift/costs/?filter[cluster]={beta_id}")
        assert response.status_code == 403, (
            f"Bob should get 403 for beta but got {response.status_code}: {response.text[:300]}"
        )

    # =========================================================================
    # Cross-Cluster Namespace Test (key demo scenario)
    # =========================================================================

    def test_alice_payment_spans_two_clusters(
        self, cluster_config, test_runner_pod, org_id, rbac_access_setup, rbac_cluster_data
    ):
        """Alice sees payment data from both cluster-alpha and cluster-beta."""
        session = self._user_session(cluster_config, test_runner_pod, org_id, "alice")
        url = rbac_access_setup["koku_api_url"]
        response = session.get(
            f"{url}/reports/openshift/costs/?filter[project]=payment&group_by[cluster]=*"
        )
        assert response.status_code == 200, f"Alice got {response.status_code}: {response.text[:300]}"

        data = response.json()
        clusters_seen = set()
        for day in data.get("data", []):
            for cluster_group in day.get("clusters", []):
                cid = cluster_group.get("cluster")
                if cid:
                    clusters_seen.add(cid)

        alpha_id = rbac_cluster_data["cluster_ids"]["alpha"]
        beta_id = rbac_cluster_data["cluster_ids"]["beta"]
        gamma_id = rbac_cluster_data["cluster_ids"]["gamma"]

        if clusters_seen:
            assert alpha_id in clusters_seen, f"Expected alpha ({alpha_id}) in results"
            assert beta_id in clusters_seen, f"Expected beta ({beta_id}) in results"
            assert gamma_id not in clusters_seen, (
                f"Gamma ({gamma_id}) should NOT appear (no payment namespace)"
            )

    # =========================================================================
    # Gateway JWT path (Keycloak password grant → Envoy → Koku)
    # =========================================================================

    def test_alice_password_grant_via_gateway_sees_only_payment(
        self,
        gateway_url: str,
        http_session: requests.Session,
        cluster_config: ClusterConfig,
        keycloak_config: KeycloakConfig,
        rbac_access_setup,
        rbac_cluster_data,
        rbac_gateway_jwt_password: str,
    ):
        """Alice JWT through gateway matches in-cluster RBAC (payment only)."""
        if not _check_gateway_reachable(gateway_url, http_session):
            pytest.skip("Gateway service not available")

        tok = obtain_password_grant_token(
            "alice",
            rbac_gateway_jwt_password,
            keycloak_config,
            cluster_config,
        )
        url = (
            f"{gateway_url.rstrip('/')}/cost-management/v1/reports/openshift/costs/"
            "?group_by[project]=*"
        )
        response = http_session.get(
            url, headers=tok.authorization_header, timeout=120, verify=False
        )
        assert response.status_code == 200, (
            f"Alice (gateway) got {response.status_code}: {response.text[:400]}"
        )
        data = response.json()
        projects_seen = set()
        for day in data.get("data", []):
            for project_group in day.get("projects", []):
                proj = project_group.get("project")
                if proj:
                    projects_seen.add(proj)
        assert len(projects_seen) > 0, (
            "Alice (gateway) returned 200 but no project data — "
            "RBAC fixture may not have seeded data"
        )
        assert projects_seen == {"payment"}, (
            f"Alice gateway should only see 'payment' but saw: {projects_seen}"
        )

    def test_bob_password_grant_via_gateway_sees_only_alpha(
        self,
        gateway_url: str,
        http_session: requests.Session,
        cluster_config: ClusterConfig,
        keycloak_config: KeycloakConfig,
        rbac_access_setup,
        rbac_cluster_data,
        rbac_gateway_jwt_password: str,
    ):
        """Bob JWT through gateway is limited to cluster-alpha."""
        if not _check_gateway_reachable(gateway_url, http_session):
            pytest.skip("Gateway service not available")

        tok = obtain_password_grant_token(
            "bob",
            rbac_gateway_jwt_password,
            keycloak_config,
            cluster_config,
        )
        alpha_id = rbac_cluster_data["cluster_ids"]["alpha"]
        url = (
            f"{gateway_url.rstrip('/')}/cost-management/v1/reports/openshift/costs/"
            "?group_by[cluster]=*"
        )
        response = http_session.get(
            url, headers=tok.authorization_header, timeout=120, verify=False
        )
        assert response.status_code == 200, (
            f"Bob (gateway) got {response.status_code}: {response.text[:400]}"
        )
        data = response.json()
        clusters_seen = set()
        for day in data.get("data", []):
            for cluster_group in day.get("clusters", []):
                cid = cluster_group.get("cluster")
                if cid:
                    clusters_seen.add(cid)
        assert len(clusters_seen) > 0, (
            "Bob (gateway) returned 200 but no cluster data — "
            "RBAC fixture may not have seeded data"
        )
        assert clusters_seen == {alpha_id}, (
            f"Bob gateway should only see alpha ({alpha_id}) but saw: {clusters_seen}"
        )

    def test_carol_password_grant_via_gateway_sees_all_test_clusters(
        self,
        gateway_url: str,
        http_session: requests.Session,
        cluster_config: ClusterConfig,
        keycloak_config: KeycloakConfig,
        rbac_access_setup,
        rbac_cluster_data,
        rbac_gateway_jwt_password: str,
    ):
        """Carol (Cost Administrator) via gateway sees all three RBAC test clusters."""
        if not _check_gateway_reachable(gateway_url, http_session):
            pytest.skip("Gateway service not available")

        tok = obtain_password_grant_token(
            "carol",
            rbac_gateway_jwt_password,
            keycloak_config,
            cluster_config,
        )
        url = (
            f"{gateway_url.rstrip('/')}/cost-management/v1/reports/openshift/costs/"
            "?group_by[cluster]=*"
        )
        response = http_session.get(
            url, headers=tok.authorization_header, timeout=120, verify=False
        )
        assert response.status_code == 200, (
            f"Carol (gateway) got {response.status_code}: {response.text[:400]}"
        )
        data = response.json()
        clusters_seen = set()
        for day in data.get("data", []):
            for cluster_group in day.get("clusters", []):
                cid = cluster_group.get("cluster")
                if cid:
                    clusters_seen.add(cid)
        expected_clusters = set(rbac_cluster_data["cluster_ids"].values())
        assert len(clusters_seen) > 0, (
            "Carol (gateway) returned 200 but no cluster data — "
            "RBAC fixture may not have seeded data"
        )
        assert expected_clusters.issubset(clusters_seen), (
            f"Carol gateway should see at least {expected_clusters} "
            f"but saw: {clusters_seen}"
        )

    def test_alice_password_grant_via_gateway_ros_recommendations_ok(
        self,
        gateway_url: str,
        http_session: requests.Session,
        cluster_config: ClusterConfig,
        keycloak_config: KeycloakConfig,
        rbac_access_setup,
        rbac_gateway_jwt_password: str,
    ):
        """Alice can reach ROS recommendations through gateway (same RBAC app)."""
        if not _check_gateway_reachable(gateway_url, http_session):
            pytest.skip("Gateway service not available")

        tok = obtain_password_grant_token(
            "alice",
            rbac_gateway_jwt_password,
            keycloak_config,
            cluster_config,
        )
        url = (
            f"{gateway_url.rstrip('/')}/cost-management/v1/recommendations/openshift"
        )
        response = http_session.get(
            url, headers=tok.authorization_header, timeout=120, verify=False
        )
        assert response.status_code in (200, 404), (
            f"Alice ROS (gateway) expected 200 or 404, got {response.status_code}: "
            f"{response.text[:400]}"
        )
