"""Operator-CI smoke: which pods must be Ready."""


def critical_smoke_components(*, database_deployed: bool, ros_enabled: bool):
    """Return (name, label) pairs for TestE2ESmoke.test_all_critical_pods_running.

    Include kruize and ros-api only when spec.ros.enabled is true.
    """
    components = []
    if database_deployed:
        components.append(("database", "app.kubernetes.io/component=database"))
    components.append(("ingress", "app.kubernetes.io/component=ingress"))
    if ros_enabled:
        components.append(("kruize", "app.kubernetes.io/component=ros-optimization"))
        components.append(("ros-api", "app.kubernetes.io/component=ros-api"))
    return components
