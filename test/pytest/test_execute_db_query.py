"""Unit tests for execute_db_query oc-exec retries (no cluster required)."""

from __future__ import annotations

import subprocess
from unittest.mock import patch

import utils


def _proc(rc: int, stdout: str = "", stderr: str = "") -> subprocess.CompletedProcess:
    return subprocess.CompletedProcess(
        args=["oc", "exec"],
        returncode=rc,
        stdout=stdout,
        stderr=stderr,
    )


def test_execute_db_query_retries_empty_exec_then_succeeds():
    """CI: django_migrations EXISTS returned None; next test queried the table."""
    calls = {"n": 0}

    def flaky(*_args, **_kwargs):
        calls["n"] += 1
        if calls["n"] <= 3:
            return _proc(1, stderr="error: unable to upgrade connection: EOF")
        return _proc(0, stdout="t\n")

    with patch.object(utils, "exec_in_pod_raw", side_effect=flaky), patch.object(
        utils.time, "sleep"
    ):
        rows = utils.execute_db_query("ns", "postgres-0", "koku", "koku_user", "SELECT 1")

    assert rows == [("t",)]
    assert calls["n"] == 4


def test_execute_db_query_does_not_retry_sql_error():
    def sql_error(*_args, **_kwargs):
        return _proc(1, stderr='ERROR:  relation "missing" does not exist')

    with patch.object(utils, "exec_in_pod_raw", side_effect=sql_error), patch.object(
        utils.time, "sleep"
    ) as sleep:
        rows = utils.execute_db_query("ns", "postgres-0", "koku", "koku_user", "SELECT 1")

    assert rows is None
    sleep.assert_not_called()


def test_exec_in_pod_raw_retries_upgrade_connection():
    calls = {"n": 0}

    def flaky(*_args, **_kwargs):
        calls["n"] += 1
        if calls["n"] <= 2:
            return _proc(1, stderr="error: unable to upgrade connection: EOF")
        return _proc(0, stdout="ok\n")

    with patch.object(utils, "run_oc_command", side_effect=flaky), patch.object(
        utils.time, "sleep"
    ):
        result = utils.exec_in_pod_raw("ns", "pod", ["true"])

    assert result.returncode == 0
    assert result.stdout == "ok\n"
    assert calls["n"] == 3


def test_exec_in_pod_raw_does_not_retry_psql_error():
    def sql_error(*_args, **_kwargs):
        return _proc(1, stderr='ERROR:  relation "missing" does not exist')

    with patch.object(utils, "run_oc_command", side_effect=sql_error), patch.object(
        utils.time, "sleep"
    ) as sleep:
        result = utils.exec_in_pod_raw("ns", "pod", ["psql", "-c", "SELECT 1"])

    assert result.returncode == 1
    sleep.assert_not_called()
