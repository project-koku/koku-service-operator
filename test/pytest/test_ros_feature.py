"""Unit tests for ROS enablement detection (COST-8136)."""

import os
from unittest.mock import patch

from ros_feature import (
    RosEnablementSignals,
    detect_ros_enabled,
    parse_ros_enabled_env,
    resolve_ros_enabled,
)


def test_parse_ros_enabled_env():
    assert parse_ros_enabled_env("true") is True
    assert parse_ros_enabled_env("FALSE") is False
    assert parse_ros_enabled_env("  yes ") is True
    assert parse_ros_enabled_env("") is None
    assert parse_ros_enabled_env(None) is None


def test_resolve_ros_enabled_env_override_wins():
    signals = RosEnablementSignals(
        env_override="false",
        spec_enabled="true",
        condition_status="True",
        ros_api_present=True,
        cmsc_found=True,
    )
    assert resolve_ros_enabled(signals) is False


def test_resolve_ros_enabled_spec_false():
    signals = RosEnablementSignals(
        spec_enabled="false",
        ros_api_present=True,
        cmsc_found=True,
    )
    assert resolve_ros_enabled(signals) is False


def test_resolve_ros_enabled_spec_omitted_defaults_false():
    signals = RosEnablementSignals(
        spec_enabled="",
        ros_api_present=True,
        cmsc_found=True,
    )
    assert resolve_ros_enabled(signals) is False


def test_resolve_ros_enabled_falls_back_to_workload():
    signals = RosEnablementSignals(ros_api_present=True, cmsc_found=False)
    assert resolve_ros_enabled(signals) is True


def test_resolve_ros_enabled_condition_false_without_cmsc():
    signals = RosEnablementSignals(
        condition_status="False",
        ros_api_present=True,
        cmsc_found=False,
    )
    assert resolve_ros_enabled(signals) is False


@patch("ros_feature.check_pod_exists")
@patch("ros_feature.run_oc_command")
def test_detect_ros_enabled_env_override_skips_cluster_probes(mock_run_oc, mock_check_pod):
    with patch.dict(os.environ, {"ROS_ENABLED": "false"}, clear=False):
        assert detect_ros_enabled("cost-onprem", "cost-onprem", "cost-onprem") is False
    mock_run_oc.assert_not_called()
    mock_check_pod.assert_not_called()
