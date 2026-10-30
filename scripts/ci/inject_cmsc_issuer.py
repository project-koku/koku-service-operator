#!/usr/bin/env python3
"""Set spec.auth.keycloak issuerURL (+ lab TLS skip) in a CMSC YAML file.

Prow e2e-pytest applies this CR in the stack step.
- issuerURL: pytest tokens use the Keycloak Route as iss.
- insecureSkipVerify: oauth2-proxy talks to that Route; claimed-cluster
  ingress certs are not in the proxy image trust store (x509 in logs).
JWKS stays on the in-cluster url (HTTP).
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
COMMENTED_TLS = "      # tls:\n"
COMMENTED_SKIP = (
    "      #   insecureSkipVerify: true   # dev only when issuer uses a private CA\n"
)
TLS_BLOCK = "      tls:\n        insecureSkipVerify: true\n"


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
    text = text.replace(COMMENTED_TLS, "")
    text = text.replace(COMMENTED_SKIP, "")
    issuer_line = f'      issuerURL: "{issuer}"\n'
    if f'issuerURL: "{issuer}"' not in text:
        replaced, n = re.subn(
            r'^      issuerURL: ".*"\n',
            issuer_line,
            text,
            count=1,
            flags=re.M,
        )
        if n:
            text = replaced
        else:
            text = text.replace(
                KEYCLOAK_URL_LINE,
                KEYCLOAK_URL_LINE + "\n" + issuer_line.rstrip("\n"),
                1,
            )
    if "insecureSkipVerify: true" not in text:
        text = text.replace(issuer_line, issuer_line + TLS_BLOCK, 1)
    src.write_text(text)


def main() -> None:
    if len(sys.argv) != 3:
        raise SystemExit("usage: inject_cmsc_issuer.py <cmsc.yaml> <https://issuer>")
    inject(sys.argv[1], sys.argv[2])


if __name__ == "__main__":
    main()
