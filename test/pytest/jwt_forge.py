"""Forge JWTs with an ephemeral RSA key for gateway rejection tests.

Envoy must reject these tokens (invalid signature and/or expired claims) before
backend services run. Used by ``TestRBACSecurityBoundaries`` in
``test_rbac_gateway.py``.
"""

from __future__ import annotations

import time
from typing import Any, Dict, Optional

from cryptography.hazmat.backends import default_backend
from cryptography.hazmat.primitives.asymmetric import rsa
from jose import jwt

_PRIVATE_KEY: Optional[Any] = None


def _private_key():
    global _PRIVATE_KEY
    if _PRIVATE_KEY is None:
        _PRIVATE_KEY = rsa.generate_private_key(
            public_exponent=65537,
            key_size=2048,
            backend=default_backend(),
        )
    return _PRIVATE_KEY


def forge_jwt(claims: Dict[str, Any], *, algorithm: str = "RS256") -> str:
    """Return a signed JWT that Keycloak did not issue."""
    return jwt.encode(claims, _private_key(), algorithm=algorithm)


def forge_expired_jwt(
    *,
    sub: str = "forge-expired",
    org_id: str = "1234567",
    account_number: str = "7890123",
    skew_seconds: int = 3600,
) -> str:
    """JWT with ``exp`` in the past."""
    return forge_jwt(
        {
            "sub": sub,
            "exp": int(time.time()) - skew_seconds,
            "org_id": org_id,
            "account_number": account_number,
        }
    )


def forge_jwt_missing_sub(
    *,
    org_id: str = "1234567",
    ttl_seconds: int = 300,
) -> str:
    """JWT without ``sub`` (and without other required identity claims)."""
    return forge_jwt(
        {
            "org_id": org_id,
            "exp": int(time.time()) + ttl_seconds,
        }
    )
