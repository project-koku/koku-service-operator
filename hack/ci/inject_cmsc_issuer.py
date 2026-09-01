#!/usr/bin/env python3
"""Set spec.auth.keycloak.issuerURL in a CMSC YAML file (stdlib only).

Prow e2e-pytest applies this CR in the stack step. Pytest tokens use the
Keycloak Route as iss; without issuerURL Envoy returns 401
"Jwt issuer is not configured".
"""
from __future__ import annotations

import re
import sys
from pathlib import Path

KEYCLOAK_URL_LINE = (
    '      url: "http://keycloak-service.keycloak.svc.cluster.local:8080"'
)
COMMENTED_ISSUER = (
    '      # issuerURL: "https://keycloak-keycloak.apps.example.com"\n'
)


def inject(path: str, issuer: str) -> None:
    if not issuer.startswith("https://"):
        raise SystemExit(f"issuerURL must be https://, got {issuer!r}")
    src = Path(path)
    text = src.read_text()
    if KEYCLOAK_URL_LINE not in text:
        raise SystemExit(
            "inject_cmsc_issuer: keycloak url line not found in CMSC YAML"
        )
    text = text.replace(COMMENTED_ISSUER, "")
    issuer_line = f'      issuerURL: "{issuer}"\n'
    if f'issuerURL: "{issuer}"' in text:
        src.write_text(text)
        return
    replaced, n = re.subn(
        r'^      issuerURL: ".*"\n',
        issuer_line,
        text,
        count=1,
        flags=re.M,
    )
    if n:
        src.write_text(replaced)
        return
    src.write_text(
        text.replace(KEYCLOAK_URL_LINE, KEYCLOAK_URL_LINE + "\n" + issuer_line.rstrip("\n"), 1)
    )


def main() -> None:
    if len(sys.argv) != 3:
        raise SystemExit("usage: inject_cmsc_issuer.py <cmsc.yaml> <https://issuer>")
    inject(sys.argv[1], sys.argv[2])


if __name__ == "__main__":
    main()
