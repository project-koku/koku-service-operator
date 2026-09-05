"""
Keycloak connectivity and configuration tests.
"""

import os

import pytest
import requests

from utils import run_oc_command

_DEFAULT_KEYCLOAK_AUDIENCES = frozenset(
    {"cost-management-operator", "cost-management-ui"}
)


def _audiences_from_cmsc(cluster_config) -> set[str]:
    """Read spec.auth.keycloak.audiences from the CMSC under test.

    Falls back to the CRD defaults when the field is omitted or the CR
    cannot be read.
    """
    cmsc_name = os.environ.get("CMSC_NAME", cluster_config.helm_release_name)
    result = run_oc_command(
        [
            "get",
            "cmsc",
            cmsc_name,
            "-n",
            cluster_config.namespace,
            "-o",
            "jsonpath={.spec.auth.keycloak.audiences[*]}",
        ],
        check=False,
    )
    if result.returncode != 0:
        return set(_DEFAULT_KEYCLOAK_AUDIENCES)
    values = {part for part in result.stdout.split() if part}
    if not values:
        return set(_DEFAULT_KEYCLOAK_AUDIENCES)
    return values


@pytest.mark.auth
@pytest.mark.component
@pytest.mark.smoke
class TestKeycloakConnectivity:
    """Tests for Keycloak connectivity."""

    def test_keycloak_reachable(self, keycloak_config, http_session: requests.Session):
        """Verify Keycloak is reachable."""
        well_known_url = (
            f"{keycloak_config.url}/realms/{keycloak_config.realm}/"
            ".well-known/openid-configuration"
        )
        response = http_session.get(well_known_url, timeout=10)

        assert response.status_code == 200, (
            f"Keycloak not reachable at {well_known_url}: {response.status_code}"
        )

        data = response.json()
        assert "token_endpoint" in data, "Invalid OpenID configuration response"

    def test_oidc_discovery_endpoint(self, keycloak_config, http_session: requests.Session):
        """Verify OIDC discovery endpoint returns expected fields."""
        well_known_url = (
            f"{keycloak_config.url}/realms/{keycloak_config.realm}/"
            ".well-known/openid-configuration"
        )
        response = http_session.get(well_known_url, timeout=10)
        
        assert response.status_code == 200
        data = response.json()
        
        required_fields = [
            "issuer",
            "authorization_endpoint",
            "token_endpoint",
            "jwks_uri",
        ]
        
        for field in required_fields:
            assert field in data, f"OIDC config missing '{field}'"


@pytest.mark.auth
@pytest.mark.component
class TestJWTTokenAcquisition:
    """Tests for JWT token acquisition."""

    @pytest.mark.smoke
    def test_jwt_token_obtained(self, jwt_token):
        """Verify we can obtain a JWT token from Keycloak."""
        assert jwt_token.access_token, "JWT token is empty"
        assert len(jwt_token.access_token) > 100, "JWT token seems too short"
        assert not jwt_token.is_expired, "JWT token is already expired"

    def test_token_has_valid_structure(self, jwt_token):
        """Verify JWT token has valid structure (3 parts)."""
        parts = jwt_token.access_token.split(".")
        assert len(parts) == 3, "JWT should have 3 parts (header.payload.signature)"

    def test_token_payload_decodable(self, jwt_token, cluster_config):
        """Verify JWT payload can be decoded."""
        from conftest import decode_jwt_payload

        payload = decode_jwt_payload(jwt_token.access_token)

        assert "exp" in payload, "Token missing 'exp' claim"
        assert "iss" in payload, "Token missing 'iss' claim"
        assert "org_id" in payload, "Token missing org_id claim"
        assert "account_number" in payload, "Token missing account_number claim"

        aud = payload.get("aud")
        if isinstance(aud, str):
            aud_values = {aud}
        elif isinstance(aud, list):
            aud_values = {a for a in aud if isinstance(a, str)}
        else:
            aud_values = set()
        expected_audiences = _audiences_from_cmsc(cluster_config)
        assert aud_values & expected_audiences, (
            f"Token aud {aud!r} does not intersect {sorted(expected_audiences)}"
        )
