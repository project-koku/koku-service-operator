"""Detect whether ROS/Kruize are enabled for pytest skip logic."""

import os
from dataclasses import dataclass
from typing import Optional

from utils import check_pod_exists, run_oc_command

ROS_DISABLED_SKIP_REASON = (
    "ROS is disabled (spec.ros.enabled=false or ROSEnabled=False); "
    "recommendations API and ROS workloads are not deployed on the Cost-only beta path. "
    "Set spec.ros.enabled=true (and ROS/Kruize images) to run ROS API tests."
)


@dataclass
class RosEnablementSignals:
    """Inputs used to decide whether ROS workloads should be considered active."""

    env_override: Optional[str] = None
    spec_enabled: Optional[str] = None
    condition_status: Optional[str] = None
    ros_api_present: bool = False
    cmsc_found: bool = False


def parse_ros_enabled_env(value: Optional[str]) -> Optional[bool]:
    """Parse ROS_ENABLED (or similar) environment override."""
    if value is None:
        return None
    normalized = value.strip().lower()
    if not normalized:
        return None
    if normalized in ("true", "1", "yes"):
        return True
    if normalized in ("false", "0", "no"):
        return False
    return None


def resolve_ros_enabled(signals: RosEnablementSignals) -> bool:
    """Resolve ROS enablement from CMSC signals and optional workload fallback."""
    env = parse_ros_enabled_env(signals.env_override)
    if env is not None:
        return env

    if signals.cmsc_found:
        spec_val = (signals.spec_enabled or "").strip().lower()
        if spec_val == "true":
            return True
        # CRD default is false; omitted field yields empty jsonpath.
        if spec_val == "false" or spec_val == "":
            return False

    condition = (signals.condition_status or "").strip()
    if condition == "True":
        return True
    if condition == "False":
        return False

    return signals.ros_api_present


def detect_ros_enabled(
    namespace: str,
    cr_name: str,
    helm_release_name: str,
) -> bool:
    """Detect ROS enablement from CMSC spec/condition or ros-api workload."""
    env_override = os.environ.get("ROS_ENABLED")
    cmsc_name = os.environ.get("CMSC_NAME", cr_name or helm_release_name)

    cmsc_found = False
    spec_enabled: Optional[str] = None
    condition_status: Optional[str] = None

    spec_result = run_oc_command(
        [
            "get",
            "cmsc",
            cmsc_name,
            "-n",
            namespace,
            "-o",
            "jsonpath={.spec.ros.enabled}",
        ],
        check=False,
    )
    if spec_result.returncode == 0:
        cmsc_found = True
        spec_enabled = spec_result.stdout.strip()

    condition_result = run_oc_command(
        [
            "get",
            "cmsc",
            cmsc_name,
            "-n",
            namespace,
            "-o",
            "jsonpath={.status.conditions[?(@.type=='ROSEnabled')].status}",
        ],
        check=False,
    )
    if condition_result.returncode == 0:
        cmsc_found = True
        condition_status = condition_result.stdout.strip()

    ros_api_present = check_pod_exists(
        namespace, "app.kubernetes.io/component=ros-api"
    )

    return resolve_ros_enabled(
        RosEnablementSignals(
            env_override=env_override,
            spec_enabled=spec_enabled,
            condition_status=condition_status,
            ros_api_present=ros_api_present,
            cmsc_found=cmsc_found,
        )
    )
