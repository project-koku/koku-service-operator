"""Keycloak Admin API helpers for RBAC-related tests.

Creates ephemeral realm users that match RBAC principal names so JWTs
obtained via password grant exercise the same Envoy → Koku/ROS path as real
clients. Intended for CI / lab clusters only (see deploy-rhbk.sh).
"""

from __future__ import annotations

import logging
from typing import Optional

from utils import external_http_session, get_secret_value

logger = logging.getLogger(__name__)

# Prow pytest runs outside the cluster; Keycloak is an apps Route.
_http = external_http_session(verify=False)


def fetch_keycloak_master_admin_token(
    keycloak_base_url: str,
    keycloak_namespace: str,
) -> Optional[str]:
    """Return a bearer token for the Keycloak master realm admin-cli user."""
    admin_password = get_secret_value(
        keycloak_namespace, "keycloak-initial-admin", "password"
    )
    admin_username = (
        get_secret_value(keycloak_namespace, "keycloak-initial-admin", "username")
        or "admin"
    )
    if not admin_password:
        logger.warning("Keycloak initial-admin password secret not found")
        return None

    base = keycloak_base_url.rstrip("/")
    resp = _http.post(
        f"{base}/realms/master/protocol/openid-connect/token",
        data={
            "client_id": "admin-cli",
            "grant_type": "password",
            "username": admin_username,
            "password": admin_password,
        },
        headers={"Content-Type": "application/x-www-form-urlencoded"},
        verify=False,
        timeout=30,
    )
    if resp.status_code != 200:
        logger.warning(
            "Master realm token failed: %s %s",
            resp.status_code,
            resp.text[:200],
        )
        return None
    return resp.json().get("access_token")


def _realm_user_id(
    base: str,
    realm: str,
    admin_token: str,
    username: str,
) -> Optional[str]:
    r = _http.get(
        f"{base}/admin/realms/{realm}/users",
        params={"username": username, "exact": "true"},
        headers={"Authorization": f"Bearer {admin_token}"},
        verify=False,
        timeout=30,
    )
    if r.status_code != 200:
        return None
    users = r.json()
    return users[0]["id"] if users else None


def ensure_realm_user_with_password(
    *,
    keycloak_base_url: str,
    realm: str,
    admin_token: str,
    username: str,
    password: str,
    org_id: str,
    account_number: str,
    email: str,
) -> None:
    """Create or update a realm user with org_id / account_number attributes."""
    base = keycloak_base_url.rstrip("/")
    headers = {
        "Authorization": f"Bearer {admin_token}",
        "Content-Type": "application/json",
    }

    user_id = _realm_user_id(base, realm, admin_token, username)
    # firstName/lastName are required by RHBK's default user profile.
    # Without them Keycloak interrupts OIDC with VERIFY_PROFILE
    # ("Account is not fully set up") and UI tests never reach /oauth2/callback.
    body = {
        "username": username,
        "enabled": True,
        "email": email,
        "emailVerified": True,
        "firstName": username.capitalize(),
        "lastName": "User",
        "requiredActions": [],
        "attributes": {
            "org_id": [org_id],
            "account_number": [account_number],
        },
        "credentials": [
            {"type": "password", "value": password, "temporary": False},
        ],
    }

    if user_id:
        put_body = {k: v for k, v in body.items() if k != "credentials"}
        r = _http.put(
            f"{base}/admin/realms/{realm}/users/{user_id}",
            json=put_body,
            headers=headers,
            verify=False,
            timeout=30,
        )
        if r.status_code not in (204, 200):
            raise RuntimeError(
                f"Keycloak PUT user {username} failed: {r.status_code} {r.text[:300]}"
            )
    else:
        r = _http.post(
            f"{base}/admin/realms/{realm}/users",
            json=body,
            headers=headers,
            verify=False,
            timeout=30,
        )
        if r.status_code not in (201, 204) and r.status_code != 409:
            raise RuntimeError(
                f"Keycloak POST user {username} failed: {r.status_code} {r.text[:300]}"
            )

    if not user_id:
        user_id = _realm_user_id(base, realm, admin_token, username)
    if not user_id:
        raise RuntimeError(f"Could not resolve Keycloak user id for {username}")

    pr = _http.put(
        f"{base}/admin/realms/{realm}/users/{user_id}/reset-password",
        json={"type": "password", "value": password, "temporary": False},
        headers=headers,
        verify=False,
        timeout=30,
    )
    if pr.status_code not in (204, 200):
        raise RuntimeError(
            f"Keycloak reset-password for {username} failed: "
            f"{pr.status_code} {pr.text[:300]}"
        )


def ensure_rbac_gateway_persona_users(
    keycloak_base_url: str,
    keycloak_namespace: str,
    realm: str,
    org_id: str,
    account_number: str,
    *,
    password: str,
    extra_users: Optional[list[str]] = None,
) -> None:
    """Provision Keycloak users whose usernames match RBAC E2E principals."""
    token = fetch_keycloak_master_admin_token(keycloak_base_url, keycloak_namespace)
    if not token:
        raise RuntimeError("Could not obtain Keycloak master admin token")

    users = ["alice", "bob", "carol", "nobody-unassigned"]
    if extra_users:
        users = list(dict.fromkeys(users + list(extra_users)))

    for uname in users:
        ensure_realm_user_with_password(
            keycloak_base_url=keycloak_base_url,
            realm=realm,
            admin_token=token,
            username=uname,
            password=password,
            org_id=org_id,
            account_number=account_number,
            email=f"{uname}@rbac-gateway.test",
        )


def _get_or_create_realm_role(
    base: str,
    realm: str,
    admin_token: str,
    role_name: str,
) -> dict:
    """Return Keycloak realm role representation, creating a simple realm role if missing."""
    headers = {
        "Authorization": f"Bearer {admin_token}",
        "Content-Type": "application/json",
    }
    r = _http.get(
        f"{base}/admin/realms/{realm}/roles/{role_name}",
        headers={"Authorization": f"Bearer {admin_token}"},
        verify=False,
        timeout=30,
    )
    if r.status_code == 200:
        return r.json()
    if r.status_code != 404:
        raise RuntimeError(
            f"Keycloak GET realm role {role_name!r} failed: {r.status_code} {r.text[:300]}"
        )
    cr = _http.post(
        f"{base}/admin/realms/{realm}/roles",
        json={"name": role_name, "description": "Realm role for cost-onprem chart tests"},
        headers=headers,
        verify=False,
        timeout=30,
    )
    if cr.status_code not in (201, 204):
        raise RuntimeError(
            f"Keycloak create realm role {role_name!r} failed: {cr.status_code} {cr.text[:300]}"
        )
    r2 = _http.get(
        f"{base}/admin/realms/{realm}/roles/{role_name}",
        headers={"Authorization": f"Bearer {admin_token}"},
        verify=False,
        timeout=30,
    )
    if r2.status_code != 200:
        raise RuntimeError(
            f"Keycloak re-fetch realm role {role_name!r} failed: {r2.status_code} {r2.text[:300]}"
        )
    return r2.json()


def _list_user_realm_roles(
    base: str,
    realm: str,
    admin_token: str,
    user_id: str,
) -> list[dict]:
    r = _http.get(
        f"{base}/admin/realms/{realm}/users/{user_id}/role-mappings/realm",
        headers={"Authorization": f"Bearer {admin_token}"},
        verify=False,
        timeout=30,
    )
    if r.status_code != 200:
        raise RuntimeError(
            f"Keycloak list realm roles for user failed: {r.status_code} {r.text[:300]}"
        )
    return r.json()


def _org_group_name(org_id: str) -> str:
    """Match deploy-rhbk.sh ``ORG_GROUP_PREFIX`` default (``org-``)."""
    return f"org-{org_id}"


def _find_group_id_by_name(
    base: str,
    realm: str,
    admin_token: str,
    name: str,
) -> Optional[str]:
    r = _http.get(
        f"{base}/admin/realms/{realm}/groups",
        params={"search": name, "exact": "true"},
        headers={"Authorization": f"Bearer {admin_token}"},
        verify=False,
        timeout=30,
    )
    if r.status_code != 200:
        return None
    for group in r.json() or []:
        if group.get("name") == name:
            return group.get("id")
    return None


def _ensure_org_group(
    base: str,
    realm: str,
    admin_token: str,
    org_id: str,
    account_number: str,
) -> str:
    """Return the id of ``org-{org_id}``, creating it with org attributes if missing.

    RHBK's default user profile often strips unmanaged user attributes, so JWT
    ``org_id`` / ``account_number`` come from this group's attributes — the same
    path ``deploy-rhbk.sh`` uses for Helm lab users.
    """
    name = _org_group_name(org_id)
    group_id = _find_group_id_by_name(base, realm, admin_token, name)
    if group_id:
        return group_id

    headers = {
        "Authorization": f"Bearer {admin_token}",
        "Content-Type": "application/json",
    }
    r = _http.post(
        f"{base}/admin/realms/{realm}/groups",
        json={
            "name": name,
            "attributes": {
                "org_id": [org_id],
                "account_number": [account_number],
            },
        },
        headers=headers,
        verify=False,
        timeout=30,
    )
    if r.status_code not in (201, 204, 409):
        raise RuntimeError(
            f"Keycloak POST group {name!r} failed: {r.status_code} {r.text[:300]}"
        )
    group_id = _find_group_id_by_name(base, realm, admin_token, name)
    if not group_id:
        raise RuntimeError(f"Could not resolve Keycloak group id for {name!r}")
    return group_id


def _add_user_to_group(
    base: str,
    realm: str,
    admin_token: str,
    user_id: str,
    group_id: str,
) -> None:
    r = _http.put(
        f"{base}/admin/realms/{realm}/users/{user_id}/groups/{group_id}",
        headers={"Authorization": f"Bearer {admin_token}"},
        verify=False,
        timeout=30,
    )
    if r.status_code not in (204, 200):
        raise RuntimeError(
            f"Keycloak PUT user {user_id} into group {group_id} failed: "
            f"{r.status_code} {r.text[:300]}"
        )


def _set_user_has_realm_role(
    base: str,
    realm: str,
    admin_token: str,
    user_id: str,
    role_rep: dict,
    want: bool,
) -> None:
    """Add or remove a single realm role on a user."""
    headers = {
        "Authorization": f"Bearer {admin_token}",
        "Content-Type": "application/json",
    }
    body = [{"id": role_rep["id"], "name": role_rep["name"]}]
    if want:
        r = _http.post(
            f"{base}/admin/realms/{realm}/users/{user_id}/role-mappings/realm",
            json=body,
            headers=headers,
            verify=False,
            timeout=30,
        )
        if r.status_code not in (204, 200):
            raise RuntimeError(
                f"Keycloak add realm role {role_rep['name']!r}: {r.status_code} {r.text[:300]}"
            )
        return
    current = _list_user_realm_roles(base, realm, admin_token, user_id)
    names = {x.get("name") for x in current}
    if role_rep["name"] not in names:
        return
    r = _http.delete(
        f"{base}/admin/realms/{realm}/users/{user_id}/role-mappings/realm",
        json=body,
        headers=headers,
        verify=False,
        timeout=30,
    )
    if r.status_code not in (204, 200):
        raise RuntimeError(
            f"Keycloak remove realm role {role_rep['name']!r}: {r.status_code} {r.text[:300]}"
        )


def _find_keycloak_ui_client_internal_id(
    base: str,
    realm: str,
    admin_token: str,
    ui_client_id: str,
) -> tuple[Optional[str], str]:
    """Return (internal UUID, diagnostic). internal UUID is None on failure."""
    r = _http.get(
        f"{base}/admin/realms/{realm}/clients",
        params={"clientId": ui_client_id, "max": 1},
        headers={"Authorization": f"Bearer {admin_token}"},
        verify=False,
        timeout=30,
    )
    if r.status_code != 200:
        return None, (
            f"GET /admin/realms/{realm}/clients?clientId={ui_client_id!r} "
            f"→ HTTP {r.status_code}: {r.text[:400]!r}"
        )
    clients = r.json()
    if not clients:
        return None, (
            f"GET clients returned 200 but empty list for clientId={ui_client_id!r} "
            f"(realm={realm!r})"
        )
    cid = clients[0].get("id")
    if not cid:
        snippet = repr(clients[0])[:400]
        return None, f"client record missing 'id': {snippet}"
    return cid, "ok"


def _find_roles_client_scope_id(
    base: str,
    realm: str,
    admin_token: str,
) -> tuple[Optional[str], str]:
    """Return (scope internal id for name ``roles``, diagnostic)."""
    r = _http.get(
        f"{base}/admin/realms/{realm}/client-scopes",
        headers={"Authorization": f"Bearer {admin_token}"},
        verify=False,
        timeout=30,
    )
    if r.status_code != 200:
        return None, (
            f"GET /admin/realms/{realm}/client-scopes "
            f"→ HTTP {r.status_code}: {r.text[:400]!r}"
        )
    rows = r.json()
    names = sorted({row.get("name") for row in rows if row.get("name")})
    for row in rows:
        if row.get("name") == "roles":
            sid = row.get("id")
            if sid:
                return sid, "ok"
            return None, "client-scope named 'roles' exists but has no 'id' field"
    hints = sorted(n for n in names if "role" in n.lower())[:30]
    return None, (
        f"no client-scope named exactly 'roles' (realm has {len(names)} scopes: {names!r}); "
        f"names containing 'role' (case-insensitive, max 30): {hints!r}"
    )


def ensure_cost_management_ui_roles_default_client_scope(
    keycloak_base_url: str,
    realm: str,
    admin_token: str,
    ui_client_id: str = "cost-management-ui",
) -> bool:
    """Attach the realm ``roles`` *client scope* as a default scope on the UI client.

    Password-grant tokens then include ``realm_access.roles`` without passing a
    literal ``roles`` value in the OAuth ``scope`` query string (Keycloak often
    rejects that with ``invalid_scope``).

    Logs a distinct ``[Keycloak UI default scope 'roles']`` prefix on each outcome
    so CI/lab logs show which Admin API step failed.
    """
    log_pfx = "[Keycloak UI default scope 'roles']"
    base = keycloak_base_url.rstrip("/")

    internal_id, diag = _find_keycloak_ui_client_internal_id(
        base, realm, admin_token, ui_client_id,
    )
    if not internal_id:
        logger.warning("%s resolve UI client %r → %s", log_pfx, ui_client_id, diag)
        return False

    roles_sid, diag = _find_roles_client_scope_id(base, realm, admin_token)
    if not roles_sid:
        logger.warning("%s resolve client-scope 'roles' → %s", log_pfx, diag)
        return False

    headers = {"Authorization": f"Bearer {admin_token}"}
    r = _http.get(
        f"{base}/admin/realms/{realm}/clients/{internal_id}/default-client-scopes",
        headers=headers,
        verify=False,
        timeout=30,
    )
    if r.status_code != 200:
        logger.warning(
            "%s GET default-client-scopes for client_uuid=%s → HTTP %s: %s",
            log_pfx,
            internal_id,
            r.status_code,
            repr(r.text[:400]) if r.text else "",
        )
        return False
    current = r.json()
    if any(s.get("id") == roles_sid for s in current):
        linked = [s.get("name") for s in current if s.get("id") == roles_sid]
        logger.info(
            "%s already linked: clientId=%r client_uuid=%s scope=%r",
            log_pfx,
            ui_client_id,
            internal_id,
            linked[0] if linked else roles_sid,
        )
        return True

    pr = _http.put(
        f"{base}/admin/realms/{realm}/clients/{internal_id}/default-client-scopes/{roles_sid}",
        headers=headers,
        verify=False,
        timeout=30,
    )
    if pr.status_code not in (200, 204):
        logger.warning(
            "%s PUT …/clients/%s/default-client-scopes/%s → HTTP %s: %s",
            log_pfx,
            internal_id,
            roles_sid,
            pr.status_code,
            repr(pr.text[:400]) if pr.text else "",
        )
        return False
    logger.info(
        "%s linked scope 'roles' to clientId=%r client_uuid=%s",
        log_pfx,
        ui_client_id,
        internal_id,
    )
    return True


def ensure_password_grant_lab_users_with_org_admin(
    *,
    keycloak_base_url: str,
    keycloak_namespace: str,
    realm: str,
    org_id: str,
    account_number: str,
    admin_username: str = "admin",
    admin_password: str = "admin",
    viewer_username: str = "viewer",
    viewer_password: str = "viewer",
) -> None:
    """Ensure admin/viewer exist for UI password grant and org-admin realm role is correct.

    Lab clusters sometimes lose Keycloak realm role mappings after restores or
    partial installs. Tests expect ``admin`` to carry the ``org-admin`` realm role
    and ``viewer`` to authenticate with password ``viewer`` without that role.

    ``viewer`` is also added to the top-level ``org-{org_id}`` group (not the
    ``org-admin`` subgroup). Operator ``deploy-rhbk.sh`` only provisions admin
    in that group; without membership the UI client's JWT lacks ``org_id`` and
    Envoy returns 401 on ``/user-access/`` instead of a permission 403.
    """
    token = fetch_keycloak_master_admin_token(keycloak_base_url, keycloak_namespace)
    if not token:
        raise RuntimeError("Could not obtain Keycloak master admin token")

    # Ensures JWT realm_access.roles for org-admin tests (see module docstring).
    ensure_cost_management_ui_roles_default_client_scope(
        keycloak_base_url, realm, token,
    )

    base = keycloak_base_url.rstrip("/")
    admin_uid = _realm_user_id(base, realm, token, admin_username)
    if admin_uid:
        ur = _http.get(
            f"{base}/admin/realms/{realm}/users/{admin_uid}",
            headers={"Authorization": f"Bearer {token}"},
            verify=False,
            timeout=30,
        )
        if ur.status_code == 200:
            attrs = ur.json().get("attributes") or {}
            oid = (attrs.get("org_id") or [None])[0]
            acct = (attrs.get("account_number") or [None])[0]
            if oid:
                org_id = oid
            if acct:
                account_number = acct

    ensure_realm_user_with_password(
        keycloak_base_url=keycloak_base_url,
        realm=realm,
        admin_token=token,
        username=admin_username,
        password=admin_password,
        org_id=org_id,
        account_number=account_number,
        email=f"{admin_username}@cost-onprem-chart.test",
    )
    ensure_realm_user_with_password(
        keycloak_base_url=keycloak_base_url,
        realm=realm,
        admin_token=token,
        username=viewer_username,
        password=viewer_password,
        org_id=org_id,
        account_number=account_number,
        email=f"{viewer_username}@cost-onprem-chart.test",
    )

    admin_uid = _realm_user_id(base, realm, token, admin_username)
    viewer_uid = _realm_user_id(base, realm, token, viewer_username)
    if not admin_uid or not viewer_uid:
        raise RuntimeError("Could not resolve Keycloak user ids after provisioning")

    org_admin_role = _get_or_create_realm_role(base, realm, token, "org-admin")
    _set_user_has_realm_role(base, realm, token, admin_uid, org_admin_role, want=True)
    _set_user_has_realm_role(base, realm, token, viewer_uid, org_admin_role, want=False)

    org_group_id = _ensure_org_group(base, realm, token, org_id, account_number)
    _add_user_to_group(base, realm, token, viewer_uid, org_group_id)
