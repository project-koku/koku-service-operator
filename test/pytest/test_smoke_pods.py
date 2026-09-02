"""Unit tests for operator-CI smoke pod requirements (no cluster)."""

from smoke_pods import critical_smoke_components


def test_critical_smoke_pods_omit_ros_when_disabled():
    names = [name for name, _ in critical_smoke_components(database_deployed=False, ros_enabled=False)]
    assert names == ["ingress"]


def test_critical_smoke_pods_include_ros_when_enabled():
    names = [name for name, _ in critical_smoke_components(database_deployed=False, ros_enabled=True)]
    assert names == ["ingress", "kruize", "ros-api"]


def test_critical_smoke_pods_include_bundled_database():
    names = [name for name, _ in critical_smoke_components(database_deployed=True, ros_enabled=False)]
    assert names == ["database", "ingress"]
