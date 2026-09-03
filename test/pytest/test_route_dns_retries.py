"""Unit tests for Prow Route DNS retries (no cluster required)."""

from __future__ import annotations

import socket
import threading
from http.server import BaseHTTPRequestHandler, HTTPServer
from unittest.mock import patch

import requests

from utils import install_route_dns_retries


def test_requests_post_retries_nxdomain_then_succeeds():
    """CI failed on obtain_password_grant_token -> requests.post (no adapter retries)."""
    install_route_dns_retries()

    class _Handler(BaseHTTPRequestHandler):
        def do_POST(self):
            length = int(self.headers.get("Content-Length", "0"))
            if length:
                self.rfile.read(length)
            body = b'{"access_token":"x"}'
            self.send_response(200)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(body)))
            self.end_headers()
            self.wfile.write(body)

        def log_message(self, *_args):
            pass

    httpd = HTTPServer(("127.0.0.1", 0), _Handler)
    port = httpd.server_address[1]
    threading.Thread(target=httpd.serve_forever, daemon=True).start()

    real = socket.getaddrinfo
    state = {"n": 0}

    def flaky(host, port_, *args, **kwargs):
        if host == "flaky.test.invalid":
            state["n"] += 1
            if state["n"] <= 3:
                raise socket.gaierror(-2, "Name or service not known")
            return real("127.0.0.1", port_, *args, **kwargs)
        return real(host, port_, *args, **kwargs)

    url = f"http://flaky.test.invalid:{port}/realms/kubernetes/protocol/openid-connect/token"
    try:
        with patch("socket.getaddrinfo", side_effect=flaky):
            response = requests.post(url, data={"grant_type": "password"}, timeout=5)
        assert response.status_code == 200
        assert response.json()["access_token"] == "x"
        assert state["n"] == 4
    finally:
        httpd.shutdown()


def test_new_session_has_connect_retries():
    install_route_dns_retries()
    session = requests.Session()
    retry = session.get_adapter("https://").max_retries
    assert retry.connect == 6
    assert retry.total == 6
