"""Celery/Valkey queue depth monitoring helpers."""

import time

from utils import get_pod_by_label, run_oc_command


def get_celery_queue_depths(namespace: str) -> dict:
    """Query Valkey (Celery broker) for queue lengths via oc exec.

    Returns a dict of {queue_name: length} for all active Celery queues,
    or an empty dict if the query fails.
    """
    valkey_pod = get_pod_by_label(namespace, "app.kubernetes.io/component=cache")
    if not valkey_pod:
        return {}

    queues = ["celery", "priority", "summary", "ocp", "cost_model", "download", "ros"]
    depths = {}
    for q in queues:
        result = run_oc_command(
            ["exec", "-n", namespace, valkey_pod, "--",
             "valkey-cli", "LLEN", q],
            check=False,
        )
        if result.returncode == 0:
            try:
                depths[q] = int(result.stdout.strip())
            except ValueError:
                pass
    return depths


def wait_for_queue_drain(
    namespace: str,
    max_wait_seconds: int = 600,
    poll_interval: int = 15,
    label: str = "",
) -> dict:
    """Block until all Celery queues are empty or max_wait_seconds elapses.

    Returns a dict with drain result and final queue depths.
    """
    prefix = f"[queue-drain{' ' + label if label else ''}]"
    start = time.time()
    last_depths: dict = {}

    while time.time() - start < max_wait_seconds:
        depths = get_celery_queue_depths(namespace)
        total = sum(depths.values())
        last_depths = depths
        if total == 0:
            elapsed = round(time.time() - start, 1)
            print(f"{prefix} Queues empty after {elapsed}s")
            return {"drained": True, "elapsed_s": elapsed, "final_depths": depths}
        non_empty = {k: v for k, v in depths.items() if v > 0}
        print(f"{prefix} {non_empty} — waiting...")
        time.sleep(poll_interval)

    elapsed = round(time.time() - start, 1)
    print(f"{prefix} Timed out after {elapsed}s. Final depths: {last_depths}")
    return {"drained": False, "elapsed_s": elapsed, "final_depths": last_depths}
