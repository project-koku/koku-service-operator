#!/usr/bin/env bash
# scripts/lib/perf-common.sh — Shared naming and CMSC helpers for perf scripts
#
# Unifies Helm chart and operator paths: both use {release-prefix}-koku-listener,
# {prefix}-kruize, etc.  Prefix resolves CMSC_NAME → HELM_RELEASE_NAME → cost-onprem.

[[ -n "${_PERF_COMMON_SOURCED:-}" ]] && return 0
_PERF_COMMON_SOURCED=1

perf_kubectl() {
    if command -v oc >/dev/null 2>&1; then
        oc "$@"
    else
        kubectl "$@"
    fi
}

# CMSC / Helm release prefix used for deployment names ({prefix}-kruize, …).
perf_release_prefix() {
    echo "${CMSC_NAME:-${HELM_RELEASE_NAME:-cost-onprem}}"
}

perf_deploy_name() {
    echo "$(perf_release_prefix)-$1"
}

# Keep CMSC_NAME and HELM_RELEASE_NAME aligned for pytest and oc scale helpers.
perf_sync_release_env() {
    local prefix
    prefix="$(perf_release_prefix)"
    export CMSC_NAME="${prefix}"
    export HELM_RELEASE_NAME="${prefix}"
}

perf_suite_needs_ros() {
    local suite="${PERF_SUITE:-all}"
    [[ "$suite" == "all" ]] && return 0
    [[ "$suite" == *"ros"* ]] && return 0
    return 1
}

perf_deployment_exists() {
    local name="$1"
    local namespace="${NAMESPACE:-cost-onprem}"
    perf_kubectl get deployment "${name}" -n "${namespace}" >/dev/null 2>&1
}

perf_ros_enabled_on_cmsc() {
    local namespace="${NAMESPACE:-cost-onprem}"
    local cr_name
    cr_name="$(perf_release_prefix)"
    local enabled
    enabled="$(perf_kubectl get cmsc "${cr_name}" -n "${namespace}" \
        -o jsonpath='{.spec.ros.enabled}' 2>/dev/null || true)"
    [[ "${enabled}" == "true" ]]
}

# True when ros-processor / kruize must exist (suite or CMSC spec).
perf_ros_workloads_required() {
    if perf_suite_needs_ros; then
        return 0
    fi
    if perf_ros_enabled_on_cmsc; then
        return 0
    fi
    return 1
}

# Version slug for TEST_RUN_ID: helm chart version, else operator image tag.
perf_detect_version_slug() {
    local slug="unknown"

    if command -v helm >/dev/null 2>&1; then
        local helm_chart
        helm_chart="$(helm list -n "${NAMESPACE:-cost-onprem}" -o json 2>/dev/null \
            | jq -r ".[0].chart // empty" 2>/dev/null || true)"
        if [[ -n "${helm_chart}" ]]; then
            slug="$(echo "${helm_chart}" | sed 's/.*-\([0-9][0-9.]*\)/\1/' | tr '.' '-')"
        fi
    fi

    if [[ "${slug}" == "unknown" ]]; then
        local _img
        _img="$(perf_kubectl get deploy -n "${NAMESPACE:-cost-onprem}" \
            -l control-plane=controller-manager \
            -o jsonpath='{.items[0].spec.template.spec.containers[0].image}' 2>/dev/null || true)"
        if [[ -n "${_img}" ]]; then
            slug="${_img##*:}"
        fi
    fi

    echo "${slug}"
}
