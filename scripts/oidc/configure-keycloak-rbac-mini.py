#!/usr/bin/env python3
"""Configure Google brokering and least-privilege rbac-mini sync in Keycloak."""

from __future__ import annotations

import argparse
import base64
import getpass
import json
import os
import re
import ssl
import subprocess
import sys
import tempfile
from pathlib import Path
from shutil import which
from urllib.error import HTTPError, URLError
from urllib.parse import quote, urlencode, urlsplit
from urllib.request import Request, urlopen

SYNC_ROLES = ("query-groups", "query-users", "view-realm", "view-users")
CLIENT_SECRET_KEY = "CLIENT_SECRET"


class SetupError(RuntimeError):
    pass


def parse_args() -> argparse.Namespace:
    parser = argparse.ArgumentParser(
        description="Set up the Keycloak Google broker (optional), rbac-mini sync client, and Kubernetes Secret."
    )
    parser.add_argument("--keycloak-url", required=True, help="HTTPS Keycloak origin, optionally with a base path")
    parser.add_argument("--realm", default="kubernetes", help="Cost realm (default: kubernetes)")
    parser.add_argument("--admin-realm", default="master", help="Realm used to authenticate the admin (default: master)")
    parser.add_argument("--admin-user", help="Keycloak administrator username; password is prompted")
    parser.add_argument("--admin-secret-namespace", help="Read the RHBK admin credential Secret from this namespace")
    parser.add_argument("--admin-secret-name", help="Read the RHBK admin credential Secret instead of prompting for a password")
    google = parser.add_mutually_exclusive_group()
    google.add_argument("--google-client-id", help="Google Web OAuth client ID; if set, its secret is prompted and the broker is configured")
    google.add_argument("--google-client-file", help="OAuth client JSON downloaded from Cloud Console; its client ID and secret are read without printing")
    parser.add_argument("--google-alias", default="google", help="Keycloak identity-provider alias (default: google)")
    parser.add_argument("--client-id", default="rbac-keycloak-sync", help="Dedicated sync client ID")
    parser.add_argument("--scope-name", default="kc-admin-api", help="Dedicated client scope name")
    parser.add_argument("--context", help="Explicit kubectl/oc context for the namespace Secret")
    parser.add_argument("--namespace", help="Namespace where the operator reads the sync Secret")
    parser.add_argument("--secret-name", default="keycloak-client-secret-rbac-sync", help="Kubernetes Secret name")
    parser.add_argument("--ca-cert", help="CA bundle used to verify the Keycloak HTTPS endpoint")
    parser.add_argument("--no-cluster-secret", action="store_true", help="Configure Keycloak but do not write a Kubernetes Secret")
    args = parser.parse_args()

    parsed_url = urlsplit(args.keycloak_url)
    if parsed_url.scheme != "https" or not parsed_url.netloc or parsed_url.query or parsed_url.fragment:
        parser.error("--keycloak-url must be an HTTPS URL without a query or fragment")
    args.keycloak_url = args.keycloak_url.rstrip("/")
    if not args.no_cluster_secret and (not args.context or not args.namespace):
        parser.error("--context and --namespace are required unless --no-cluster-secret is set")
    if args.admin_secret_name:
        if not args.context or not args.admin_secret_namespace:
            parser.error("--context and --admin-secret-namespace are required with --admin-secret-name")
        if args.admin_user:
            parser.error("use either --admin-user or --admin-secret-name, not both")
    elif args.admin_secret_namespace:
        parser.error("--admin-secret-namespace requires --admin-secret-name")
    elif not args.admin_user:
        parser.error("provide --admin-user or use --admin-secret-name with --context")
    for name, value in (("realm", args.realm), ("admin-realm", args.admin_realm), ("client-id", args.client_id), ("scope-name", args.scope_name), ("google-alias", args.google_alias)):
        if not re.fullmatch(r"[A-Za-z0-9._-]+", value):
            parser.error(f"--{name} may contain only letters, digits, dot, underscore, and hyphen")
    return args


class KeycloakAdmin:
    def __init__(self, base_url: str, ca_cert: str | None, token: str):
        self.base_url = base_url
        self.context = ssl.create_default_context(cafile=ca_cert) if ca_cert else ssl.create_default_context()
        self.token = token

    @staticmethod
    def login(base_url: str, admin_realm: str, username: str, password: str, ca_cert: str | None) -> str:
        context = ssl.create_default_context(cafile=ca_cert) if ca_cert else ssl.create_default_context()
        url = f"{base_url}/realms/{quote(admin_realm, safe='')}/protocol/openid-connect/token"
        body = urlencode({"grant_type": "password", "client_id": "admin-cli", "username": username, "password": password}).encode()
        request = Request(url, data=body, headers={"Content-Type": "application/x-www-form-urlencoded"})
        try:
            with urlopen(request, context=context, timeout=30) as response:
                payload = json.loads(response.read())
        except HTTPError as error:
            detail = error.read().decode("utf-8", "replace")[:500]
            raise SetupError(f"Keycloak admin login failed (HTTP {error.code}): {detail}") from None
        except URLError as error:
            raise SetupError(f"Could not reach Keycloak admin endpoint: {error.reason}") from None
        token = payload.get("access_token")
        if not token:
            raise SetupError("Keycloak admin login returned no access token")
        return token

    def request(self, method: str, path: str, body: object | None = None, allow_404: bool = False, token: str | None = None) -> object | None:
        url = f"{self.base_url}{path}"
        headers = {"Accept": "application/json", "Authorization": f"Bearer {token or self.token}"}
        data = None
        if body is not None:
            headers["Content-Type"] = "application/json"
            data = json.dumps(body).encode()
        request = Request(url, data=data, headers=headers, method=method)
        try:
            with urlopen(request, context=self.context, timeout=30) as response:
                raw = response.read()
        except HTTPError as error:
            if allow_404 and error.code == 404:
                return None
            detail = error.read().decode("utf-8", "replace")[:700]
            raise SetupError(f"Keycloak Admin API {method} {path} failed (HTTP {error.code}): {detail}") from None
        except URLError as error:
            raise SetupError(f"Keycloak Admin API request failed: {error.reason}") from None
        if not raw:
            return None
        try:
            return json.loads(raw)
        except json.JSONDecodeError:
            raise SetupError(f"Keycloak Admin API {method} {path} returned invalid JSON") from None


def encoded(value: str) -> str:
    return quote(value, safe="")


def get_one(items: object, label: str) -> dict:
    if not isinstance(items, list):
        raise SetupError(f"Keycloak returned an invalid list while looking up {label}")
    if not items:
        raise SetupError(f"Keycloak could not find {label}")
    if len(items) > 1:
        raise SetupError(f"Keycloak returned multiple matches for {label}")
    return items[0]


def ensure_google_broker(kc: KeycloakAdmin, realm: str, alias: str, client_id: str, client_secret: str) -> None:
    path = f"/admin/realms/{encoded(realm)}/identity-provider/instances/{encoded(alias)}"
    existing = kc.request("GET", path, allow_404=True)
    config = dict(existing.get("config", {})) if isinstance(existing, dict) else {}
    config.update({"clientId": client_id, "clientSecret": client_secret, "defaultScope": "openid email profile"})
    if existing is None:
        representation = {
            "alias": alias,
            "displayName": "Google" if alias == "google" else alias,
            "providerId": "google",
            "enabled": True,
            "trustEmail": True,
            "config": config,
        }
        kc.request("POST", f"/admin/realms/{encoded(realm)}/identity-provider/instances", representation)
        print(f"Configured Google identity provider '{alias}' in realm '{realm}'.")
    else:
        if existing.get("providerId") != "google":
            raise SetupError(f"Identity-provider alias '{alias}' exists but is not a Google provider; choose another --google-alias")
        existing["enabled"] = True
        existing["trustEmail"] = True
        existing["config"] = config
        kc.request("PUT", path, existing)
        print(f"Updated Google identity provider '{alias}' in realm '{realm}'.")
    callback = f"{kc.base_url}/realms/{encoded(realm)}/broker/{encoded(alias)}/endpoint"
    print(f"Register this exact Google OAuth redirect URI: {callback}")


def ensure_sync_client(kc: KeycloakAdmin, realm: str, client_id: str) -> dict:
    root = f"/admin/realms/{encoded(realm)}"
    matches = kc.request("GET", f"{root}/clients?{urlencode({'clientId': client_id})}")
    if not isinstance(matches, list):
        raise SetupError("Keycloak returned an invalid clients list")
    if len(matches) > 1:
        raise SetupError(f"More than one Keycloak client uses client ID '{client_id}'")
    desired = {
        "enabled": True,
        "protocol": "openid-connect",
        "publicClient": False,
        "clientAuthenticatorType": "client-secret",
        "serviceAccountsEnabled": True,
        "standardFlowEnabled": False,
        "implicitFlowEnabled": False,
        "directAccessGrantsEnabled": False,
        "fullScopeAllowed": False,
    }
    if matches:
        client = matches[0]
        client.update(desired)
        kc.request("PUT", f"{root}/clients/{encoded(client['id'])}", client)
        print(f"Configured dedicated sync client '{client_id}'.")
        return client
    client = {"clientId": client_id, **desired}
    kc.request("POST", f"{root}/clients", client)
    matches = kc.request("GET", f"{root}/clients?{urlencode({'clientId': client_id})}")
    created = get_one(matches, f"client '{client_id}' after creation")
    print(f"Created dedicated sync client '{client_id}'.")
    return created


def ensure_client_roles(kc: KeycloakAdmin, root: str, client: dict, realm_mgmt: dict, role_names: tuple[str, ...]) -> list[dict]:
    realm_mgmt_id = realm_mgmt["id"]
    role_representations = []
    for name in role_names:
        role = kc.request("GET", f"{root}/clients/{encoded(realm_mgmt_id)}/roles/{encoded(name)}", allow_404=True)
        if not isinstance(role, dict):
            raise SetupError(f"Required realm-management role '{name}' is missing")
        role_representations.append(role)
    service_account = kc.request("GET", f"{root}/clients/{encoded(client['id'])}/service-account-user")
    if not isinstance(service_account, dict) or not service_account.get("id"):
        raise SetupError("Keycloak did not return a service-account user for the sync client")
    user_id = encoded(service_account["id"])
    mapping_path = f"{root}/users/{user_id}/role-mappings/clients/{encoded(realm_mgmt_id)}"
    current = kc.request("GET", mapping_path)
    current_names = {entry.get("name") for entry in current if isinstance(entry, dict)} if isinstance(current, list) else set()
    extras = current_names - set(role_names)
    if extras:
        raise SetupError("The dedicated sync service account has extra realm-management roles: " + ", ".join(sorted(name for name in extras if name)))
    missing = [role for role in role_representations if role["name"] not in current_names]
    if missing:
        kc.request("POST", mapping_path, missing)
    print("Assigned the four read-only realm-management roles to the sync service account.")
    return role_representations


def ensure_scope(kc: KeycloakAdmin, root: str, scope_name: str) -> tuple[dict, dict]:
    scopes = kc.request("GET", f"{root}/client-scopes")
    if not isinstance(scopes, list):
        raise SetupError("Keycloak returned an invalid client-scopes list")
    roles_scope_matches = [scope for scope in scopes if scope.get("name") == "roles"]
    if len(roles_scope_matches) != 1:
        raise SetupError("Keycloak's built-in 'roles' client scope was not found uniquely")
    custom_matches = [scope for scope in scopes if scope.get("name") == scope_name]
    if len(custom_matches) > 1:
        raise SetupError(f"More than one client scope uses name '{scope_name}'")
    if custom_matches:
        custom = custom_matches[0]
        if custom.get("protocol") != "openid-connect":
            raise SetupError(f"Client scope '{scope_name}' is not an OpenID Connect scope")
    else:
        kc.request("POST", f"{root}/client-scopes", {"name": scope_name, "protocol": "openid-connect"})
        scopes = kc.request("GET", f"{root}/client-scopes")
        custom = get_one([scope for scope in scopes if scope.get("name") == scope_name], f"client scope '{scope_name}' after creation")
    return roles_scope_matches[0], custom


def ensure_scope_roles(kc: KeycloakAdmin, root: str, scope: dict, realm_mgmt: dict, required_roles: list[dict]) -> None:
    path = f"{root}/client-scopes/{encoded(scope['id'])}/scope-mappings/clients/{encoded(realm_mgmt['id'])}"
    current = kc.request("GET", path)
    current_roles = current if isinstance(current, list) else []
    allowed = {role["name"] for role in required_roles}
    existing = {role.get("name") for role in current_roles if isinstance(role, dict)}
    extras = existing - allowed
    if extras:
        names = ", ".join(sorted(name for name in extras if name))
        raise SetupError(f"Client scope '{scope['name']}' has extra realm-management roles ({names}); remove them or use a dedicated --scope-name")
    missing = [role for role in required_roles if role["name"] not in existing]
    if missing:
        kc.request("POST", path, missing)


def ensure_audience_mapper(kc: KeycloakAdmin, root: str, scope: dict) -> None:
    path = f"{root}/client-scopes/{encoded(scope['id'])}/protocol-mappers/models"
    mappers = kc.request("GET", path)
    if not isinstance(mappers, list):
        raise SetupError("Keycloak returned an invalid protocol-mappers list")
    named = [mapper for mapper in mappers if mapper.get("name") == "realm-management audience"]
    if len(named) > 1:
        raise SetupError("Client scope contains duplicate 'realm-management audience' mappers")
    desired_config = {"included.client.audience": "realm-management", "access.token.claim": "true", "id.token.claim": "false"}
    if named:
        mapper = named[0]
        if mapper.get("protocolMapper") != "oidc-audience-mapper":
            raise SetupError("Mapper named 'realm-management audience' exists with a different mapper type")
        mapper["config"] = {**mapper.get("config", {}), **desired_config}
        kc.request("PUT", f"{path}/{encoded(mapper['id'])}", mapper)
    else:
        kc.request("POST", path, {
            "name": "realm-management audience",
            "protocol": "openid-connect",
            "protocolMapper": "oidc-audience-mapper",
            "consentRequired": False,
            "config": desired_config,
        })


def attach_default_scope(kc: KeycloakAdmin, root: str, client: dict, scope: dict) -> None:
    kc.request("PUT", f"{root}/clients/{encoded(client['id'])}/default-client-scopes/{encoded(scope['id'])}")


def verify_client_token(kc: KeycloakAdmin, realm: str, client_id: str, client_secret: str, ca_cert: str | None) -> str:
    context = ssl.create_default_context(cafile=ca_cert) if ca_cert else ssl.create_default_context()
    url = f"{kc.base_url}/realms/{encoded(realm)}/protocol/openid-connect/token"
    body = urlencode({"grant_type": "client_credentials", "client_id": client_id, "client_secret": client_secret}).encode()
    request = Request(url, data=body, headers={"Content-Type": "application/x-www-form-urlencoded"})
    try:
        with urlopen(request, context=context, timeout=30) as response:
            token_response = json.loads(response.read())
    except HTTPError as error:
        detail = error.read().decode("utf-8", "replace")[:500]
        raise SetupError(f"Sync-client token request failed (HTTP {error.code}): {detail}") from None
    except URLError as error:
        raise SetupError(f"Could not request a sync-client token: {error.reason}") from None
    token = token_response.get("access_token")
    if not token:
        raise SetupError("Sync-client token response contained no access token")
    try:
        payload_part = token.split(".")[1]
        payload = json.loads(base64.urlsafe_b64decode(payload_part + "=" * (-len(payload_part) % 4)))
    except (IndexError, ValueError, json.JSONDecodeError):
        raise SetupError("Could not inspect the sync-client token claims") from None
    token_roles = set(payload.get("resource_access", {}).get("realm-management", {}).get("roles", []))
    missing = set(SYNC_ROLES) - token_roles
    if missing:
        raise SetupError("Sync-client token is missing expected realm-management roles: " + ", ".join(sorted(missing)))
    audience = payload.get("aud", [])
    if isinstance(audience, str):
        audience = [audience]
    if "realm-management" not in audience:
        raise SetupError("Sync-client token is missing the realm-management audience")
    root = f"/admin/realms/{encoded(realm)}"
    kc.request("GET", f"{root}/groups?first=0&max=1", token=token)
    kc.request("GET", f"{root}/users?first=0&max=1", token=token)
    print("Verified the sync token's roles, audience, and read access to groups and users.")
    return token



def read_admin_secret(context: str, namespace: str, name: str) -> tuple[str, str]:
    command = ["oc", "--context", context, "-n", namespace, "get", "secret", name, "-o", "json"]
    result = subprocess.run(command, text=True, stdout=subprocess.PIPE, stderr=subprocess.PIPE, check=False)
    if result.returncode:
        raise SetupError(f"Could not read Keycloak admin Secret '{name}' in namespace '{namespace}': {result.stderr.strip()}")
    try:
        data = json.loads(result.stdout).get("data", {})
        username = base64.b64decode(data["username"]).decode()
        password = base64.b64decode(data["password"]).decode()
    except (KeyError, ValueError, json.JSONDecodeError):
        raise SetupError("Keycloak admin Secret must contain base64 data keys 'username' and 'password'") from None
    if not username or not password:
        raise SetupError("Keycloak admin Secret contains an empty username or password")
    return username, password


def read_google_client_file(path: str) -> tuple[str, str]:
    try:
        data = json.loads(Path(path).read_text(encoding="utf-8"))
        client = data.get("web") or data.get("installed") or data
        client_id = client["client_id"]
        client_secret = client["client_secret"]
    except (OSError, KeyError, TypeError, json.JSONDecodeError):
        raise SetupError("Google OAuth client file must contain client_id and client_secret in its web or installed object") from None
    if not client_id or not client_secret:
        raise SetupError("Google OAuth client file contains an empty client ID or secret")
    return client_id, client_secret


def preflight_oc(context: str, namespace: str) -> None:
    def run(args: list[str]) -> str:
        result = subprocess.run(args, text=True, stdout=subprocess.PIPE, stderr=subprocess.PIPE, check=False)
        if result.returncode:
            message = result.stderr.strip() or result.stdout.strip()
            raise SetupError(f"OpenShift preflight failed for '{' '.join(args[1:])}': {message}")
        return result.stdout.strip()

    run(["oc", "--context", context, "get", "namespace", namespace])
    for verb in ("create", "patch"):
        answer = run(["oc", "--context", context, "-n", namespace, "auth", "can-i", verb, "secrets"])
        if answer.lower() != "yes":
            raise SetupError(f"The selected OpenShift identity cannot {verb} Secrets in namespace '{namespace}'")


def write_cluster_secret(context: str, namespace: str, name: str, client_secret: str) -> None:
    with tempfile.TemporaryDirectory(prefix="keycloak-rbac-mini-") as temporary_directory:
        secret_file = Path(temporary_directory) / "client-secret"
        secret_file.write_text(client_secret, encoding="utf-8")
        secret_file.chmod(0o600)
        create_command = [
            "oc", "--context", context, "-n", namespace, "create", "secret", "generic", name,
            f"--from-file={CLIENT_SECRET_KEY}={secret_file}", "--dry-run=client", "-o", "yaml",
        ]
        generated = subprocess.run(create_command, text=True, stdout=subprocess.PIPE, stderr=subprocess.PIPE, check=False)
        if generated.returncode:
            raise SetupError("Could not generate the Kubernetes Secret manifest: " + (generated.stderr.strip() or "oc failed"))
        apply_command = ["oc", "--context", context, "-n", namespace, "apply", "-f", "-"]
        applied = subprocess.run(apply_command, input=generated.stdout, text=True, stdout=subprocess.PIPE, stderr=subprocess.PIPE, check=False)
        if applied.returncode:
            raise SetupError("Could not apply the Kubernetes Secret: " + (applied.stderr.strip() or "oc failed"))
    print(f"Applied Secret '{name}' in namespace '{namespace}' with key '{CLIENT_SECRET_KEY}'.")


def main() -> int:
    args = parse_args()
    if not args.no_cluster_secret or args.admin_secret_name:
        if not which("oc"):
            raise SetupError("oc is required for the requested cluster operation")
    if not args.no_cluster_secret:
        preflight_oc(args.context, args.namespace)
    if args.admin_secret_name:
        answer = subprocess.run(
            ["oc", "--context", args.context, "-n", args.admin_secret_namespace, "auth", "can-i", "get", "secrets"],
            text=True, stdout=subprocess.PIPE, stderr=subprocess.PIPE, check=False,
        )
        if answer.returncode or answer.stdout.strip().lower() != "yes":
            raise SetupError(f"The selected OpenShift identity cannot read Secrets in namespace '{args.admin_secret_namespace}'")

    if args.admin_secret_name:
        admin_user, admin_password = read_admin_secret(args.context, args.admin_secret_namespace, args.admin_secret_name)
    else:
        admin_user = args.admin_user
        admin_password = getpass.getpass("Keycloak admin password: ")
        if not admin_password:
            raise SetupError("Keycloak admin password cannot be empty")
    google_secret = None
    if args.google_client_file:
        args.google_client_id, google_secret = read_google_client_file(args.google_client_file)
    elif args.google_client_id:
        google_secret = getpass.getpass("Google OAuth client secret: ")
        if not google_secret:
            raise SetupError("Google OAuth client secret cannot be empty")

    token = KeycloakAdmin.login(args.keycloak_url, args.admin_realm, admin_user, admin_password, args.ca_cert)
    admin_password = ""
    admin_user = ""
    kc = KeycloakAdmin(args.keycloak_url, args.ca_cert, token)
    realm = encoded(args.realm)
    kc.request("GET", f"/admin/realms/{realm}")
    if google_secret is not None:
        ensure_google_broker(kc, args.realm, args.google_alias, args.google_client_id, google_secret)
        google_secret = ""

    root = f"/admin/realms/{realm}"
    client = ensure_sync_client(kc, args.realm, args.client_id)
    realm_mgmt = get_one(kc.request("GET", f"{root}/clients?{urlencode({'clientId': 'realm-management'})}"), "realm-management client")
    roles = ensure_client_roles(kc, root, client, realm_mgmt, SYNC_ROLES)
    roles_scope, api_scope = ensure_scope(kc, root, args.scope_name)
    ensure_scope_roles(kc, root, api_scope, realm_mgmt, roles)
    ensure_audience_mapper(kc, root, api_scope)
    attach_default_scope(kc, root, client, roles_scope)
    attach_default_scope(kc, root, client, api_scope)
    secret_response = kc.request("GET", f"{root}/clients/{encoded(client['id'])}/client-secret")
    if not isinstance(secret_response, dict) or not secret_response.get("value"):
        raise SetupError("Keycloak did not return a client secret for the sync client")
    client_secret = secret_response["value"]
    verify_client_token(kc, args.realm, args.client_id, client_secret, args.ca_cert)
    if not args.no_cluster_secret:
        write_cluster_secret(args.context, args.namespace, args.secret_name, client_secret)
    else:
        print("Skipped Kubernetes Secret creation (--no-cluster-secret).")
        print("Store the sync client secret using a protected Secret-management workflow; do not pass it on a command line.")
    print(f"Keycloak setup complete for realm '{args.realm}' and sync client '{args.client_id}'.")
    return 0


if __name__ == "__main__":
    try:
        raise SystemExit(main())
    except SetupError as error:
        print(f"error: {error}", file=sys.stderr)
        raise SystemExit(1)
