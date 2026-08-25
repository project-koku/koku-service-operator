"""Unit tests for ROS enablement detection (COST-8136)."""

from ros_feature import (
    RosEnablementSignals,
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
