"""Synchronize Keycloak realm users into insights-rbac Principals.

Source: cost-onprem-chart/cost-onprem/scripts/sync_keycloak_principals.py
(verbatim copy — kept in sync with the Helm chart prototype).

TODO: refactor pagination helpers, tighten orphan-pruning exception handling,
and consider embedding this logic into the RBAC container image instead of
mounting via ConfigMap.

This script is mounted into the RBAC container via ConfigMap and executed
under ``manage.py shell`` so the Django ORM is pre-initialized.  It reads
configuration from environment variables set by the CronJob spec.

Keycloak Admin REST API reference:
  https://www.keycloak.org/docs-api/latest/rest-api/index.html
"""

import json
import logging
import os
import ssl
import sys
import time
import urllib.error
import urllib.parse
import urllib.request

logging.basicConfig(
    level=logging.INFO,
    format="%(asctime)s %(levelname)s %(message)s",
    datefmt="%Y-%m-%dT%H:%M:%S",
)
log = logging.getLogger("keycloak-sync")

PAGE_SIZE = 100
REQUEST_TIMEOUT = 30
TOKEN_REFRESH_MARGIN = 0.8


class KeycloakClient:
    """Minimal Keycloak Admin REST API client using urllib."""

    def __init__(self, base_url, realm, client_id, client_secret, verify_tls=True):
        self.base_url = base_url.rstrip("/")
        self.realm = realm
        self.client_id = client_id
        self.client_secret = client_secret
        self._access_token = None
        self._token_acquired_at = 0.0
        self._token_expires_in = 0

        if verify_tls:
            self._ssl_ctx = ssl.create_default_context()
        else:
            self._ssl_ctx = ssl.create_default_context()
            self._ssl_ctx.check_hostname = False
            self._ssl_ctx.verify_mode = ssl.CERT_NONE

    def _token_is_fresh(self):
        if not self._access_token or self._token_expires_in <= 0:
            return False
        elapsed = time.monotonic() - self._token_acquired_at
        return elapsed < (self._token_expires_in * TOKEN_REFRESH_MARGIN)

    def authenticate(self):
        """Obtain an access token via client_credentials grant."""
        url = f"{self.base_url}/realms/{self.realm}/protocol/openid-connect/token"
        data = urllib.parse.urlencode({
            "grant_type": "client_credentials",
            "client_id": self.client_id,
            "client_secret": self.client_secret,
        }).encode()

        req = urllib.request.Request(url, data=data, method="POST")
        req.add_header("Content-Type", "application/x-www-form-urlencoded")

        with urllib.request.urlopen(req, timeout=REQUEST_TIMEOUT, context=self._ssl_ctx) as resp:
            body = json.loads(resp.read())

        self._access_token = body["access_token"]
        self._token_acquired_at = time.monotonic()
        self._token_expires_in = int(body.get("expires_in", 300))

    def ensure_authenticated(self):
        """Re-authenticate only if the current token is stale or missing."""
        if not self._token_is_fresh():
            self.authenticate()

    def _get(self, path, params=None):
        """Authenticated GET against the Admin API."""
        url = f"{self.base_url}/admin/realms/{self.realm}{path}"
        if params:
            url += "?" + urllib.parse.urlencode(params)

        req = urllib.request.Request(url, method="GET")
        req.add_header("Authorization", f"Bearer {self._access_token}")
        req.add_header("Accept", "application/json")

        with urllib.request.urlopen(req, timeout=REQUEST_TIMEOUT, context=self._ssl_ctx) as resp:
            return json.loads(resp.read())

    def list_groups(self, search=None):
        """Fetch top-level realm groups, optionally filtered by name prefix."""
        all_groups = []
        offset = 0

        while True:
            self.ensure_authenticated()
            params = {"first": offset, "max": PAGE_SIZE, "briefRepresentation": "false"}
            if search:
                params["search"] = search
            page = self._get("/groups", params)
            all_groups.extend(page)
            if len(page) < PAGE_SIZE:
                break
            offset += PAGE_SIZE

        return all_groups

    def get_group_members(self, group_id):
        """Fetch all members of a specific group."""
        members = []
        offset = 0

        while True:
            self.ensure_authenticated()
            page = self._get(
                f"/groups/{urllib.parse.quote(group_id)}/members",
                {"first": offset, "max": PAGE_SIZE},
            )
            members.extend(page)
            if len(page) < PAGE_SIZE:
                break
            offset += PAGE_SIZE

        return members

    def get_subgroups(self, group_id):
        """Fetch child groups of a parent group via the group representation."""
        self.ensure_authenticated()
        group = self._get(f"/groups/{urllib.parse.quote(group_id)}")
        return group.get("subGroups", [])


def sync(org_id, account_number, kc_users, prune_orphans):
    """Core sync loop: Keycloak users -> RBAC Principals for a single org.

    Org-admin access is granted at request time via JWT is_org_admin and the
    public tenant "Default admin access" group — not via per-tenant RBAC groups.

    Args:
        org_id: Organization identifier for the RBAC tenant.
        account_number: Account number for tenant naming.
        kc_users: Pre-fetched list of Keycloak user dicts for this org.
        prune_orphans: Whether to delete RBAC principals absent from kc_users.
    """
    from api.models import Tenant
    from django.db import transaction
    from management.models import Principal

    t0 = time.monotonic()

    if not Tenant.objects.filter(tenant_name="public").exists():
        log.error("[%s] Public tenant does not exist; RBAC migrations have not completed yet. "
                  "This is expected on first install -- the CronJob will retry.", org_id)
        return False

    tenant, created = Tenant.objects.get_or_create(
        org_id=org_id,
        defaults={"tenant_name": "acct" + account_number, "ready": True},
    )
    if created:
        log.info("[%s] Created tenant", org_id)

    counters = {"created": 0, "updated": 0, "unchanged": 0, "pruned": 0, "skipped_disabled": 0}
    synced_usernames = set()

    with transaction.atomic():
        for kc_user in kc_users:
            username = kc_user.get("username", "")
            if not username or username.startswith("service-account-"):
                continue

            if not kc_user.get("enabled", True):
                counters["skipped_disabled"] += 1
                log.info("[%s] AUDIT action=skip_disabled user=\"%s\" reason=keycloak_disabled", org_id, username)
                continue

            synced_usernames.add(username)
            _, was_created = Principal.objects.get_or_create(
                username=username, tenant=tenant,
                defaults={"type": "user"},
            )

            if was_created:
                counters["created"] += 1
                log.info("[%s] AUDIT action=created user=\"%s\"", org_id, username)
            else:
                counters["unchanged"] += 1

        if prune_orphans:
            try:
                orphans = (
                    Principal.objects
                    .filter(tenant=tenant, type="user", cross_account=False)
                    .exclude(username__in=synced_usernames)
                )
            except Exception:
                orphans = (
                    Principal.objects
                    .filter(tenant=tenant, type="user")
                    .exclude(username__in=synced_usernames)
                )
            orphan_count = orphans.count()
            if orphan_count > 0:
                orphan_names = list(orphans.values_list("username", flat=True)[:50])
                for name in orphan_names:
                    log.info("[%s] AUDIT action=pruned user=\"%s\"", org_id, name)
                orphans.delete()
            counters["pruned"] = orphan_count

    elapsed = time.monotonic() - t0
    log.info(
        "[%s] SYNC COMPLETE: members=%d, synced=%d, created=%d, updated=%d, "
        "unchanged=%d, pruned=%d, skipped_disabled=%d, elapsed=%.1fs",
        org_id, len(kc_users), len(synced_usernames), counters["created"],
        counters["updated"], counters["unchanged"], counters["pruned"],
        counters["skipped_disabled"], elapsed,
    )
    return True


def discover_and_sync(kc, org_group_prefix, org_admin_subgroup, prune_orphans):
    """Discover orgs from Keycloak groups and sync each one to RBAC.

    Keycloak group structure expected:
        {prefix}{orgId}          -- top-level org group with attributes:
            attributes.org_id    -- org identifier
            attributes.account_number -- account number
            org-admin/           -- sub-group (logged for observability; admin access is JWT-based)
    """
    log.info("Discovering organizations from Keycloak groups with prefix '%s'", org_group_prefix)

    try:
        groups = kc.list_groups(search=org_group_prefix)
    except Exception:
        log.exception("Failed to list Keycloak groups")
        return False

    org_groups = [g for g in groups if g.get("name", "").startswith(org_group_prefix)]
    if not org_groups:
        log.error("No Keycloak groups found with prefix '%s'. "
                  "Create groups named '{prefix}{orgId}' with org_id and account_number attributes.",
                  org_group_prefix)
        return False

    log.info("Found %d org group(s): %s", len(org_groups),
             ", ".join(g["name"] for g in org_groups))

    all_ok = True
    for group in org_groups:
        group_id = group["id"]
        group_name = group.get("name", "")
        attrs = group.get("attributes", {})

        org_id_list = attrs.get("org_id", [])
        acct_list = attrs.get("account_number", [])
        org_id = org_id_list[0] if org_id_list else None
        account_number = acct_list[0] if acct_list else None

        if not org_id or not account_number:
            log.error("[%s] Group missing required attributes (org_id=%r, account_number=%r); skipping",
                      group_name, org_id, account_number)
            all_ok = False
            continue

        log.info("[%s] Processing org: org_id=%s, account_number=%s", org_id, org_id, account_number)

        try:
            members = kc.get_group_members(group_id)
        except Exception:
            log.exception("[%s] Failed to fetch group members", org_id)
            all_ok = False
            continue

        admin_usernames = set()
        try:
            subgroups = kc.get_subgroups(group_id)
            admin_sg = next(
                (sg for sg in subgroups if sg.get("name") == org_admin_subgroup),
                None,
            )
            if admin_sg:
                admin_members = kc.get_group_members(admin_sg["id"])
                admin_usernames = {u["username"] for u in admin_members if u.get("username")}
                log.info("[%s] Admin sub-group '%s' members: %d", org_id, org_admin_subgroup, len(admin_usernames))
            else:
                log.warning("[%s] No '%s' sub-group found", org_id, org_admin_subgroup)
        except Exception:
            log.exception("[%s] Failed to fetch admin sub-group members", org_id)

        ok = sync(org_id, account_number, members, prune_orphans)
        if not ok:
            all_ok = False

    from django.core.cache import cache
    cache.clear()
    log.info("RBAC cache cleared")

    return all_ok


def main():
    keycloak_url = os.environ.get("KEYCLOAK_URL", "")
    realm = os.environ.get("KEYCLOAK_REALM", "kubernetes")
    client_id = os.environ.get("KEYCLOAK_CLIENT_ID", "")
    client_secret = os.environ.get("KEYCLOAK_CLIENT_SECRET", "")
    verify_tls = os.environ.get("KEYCLOAK_TLS_VERIFY", "true").lower() not in ("false", "0", "no")
    org_group_prefix = os.environ.get("SYNC_ORG_GROUP_PREFIX", "org-")
    org_admin_subgroup = os.environ.get("SYNC_ORG_ADMIN_SUBGROUP", "org-admin")
    prune_orphans = os.environ.get("SYNC_PRUNE_ORPHANS", "true").lower() not in ("false", "0", "no")

    missing = []
    if not keycloak_url:
        missing.append("KEYCLOAK_URL")
    if not client_id:
        missing.append("KEYCLOAK_CLIENT_ID")
    if not client_secret:
        missing.append("KEYCLOAK_CLIENT_SECRET")
    if missing:
        log.error("Missing required environment variables: %s", ", ".join(missing))
        sys.exit(1)

    log.info("Starting Keycloak-to-RBAC sync: realm=%s, org_group_prefix=%s, tls_verify=%s, prune=%s",
             realm, org_group_prefix, verify_tls, prune_orphans)

    kc = KeycloakClient(keycloak_url, realm, client_id, client_secret, verify_tls)

    try:
        kc.authenticate()
    except Exception:
        log.exception("Failed to authenticate with Keycloak")
        sys.exit(1)

    ok = discover_and_sync(kc, org_group_prefix, org_admin_subgroup, prune_orphans)
    sys.exit(0 if ok else 1)


main()
