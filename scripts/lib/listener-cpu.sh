#!/usr/bin/env bash
# scripts/lib/listener-cpu.sh — Listener CPU management for test runs
#
# Requires: log_info, log_success, log_warning, log_error, log_step,
#           log_verbose (all from parent script)
# Globals:  NAMESPACE, HELM_RELEASE_NAME, DRY_RUN,
#           ORIGINAL_LISTENER_CPU_LIMIT, ORIGINAL_LISTENER_CPU_REQUEST,
#           MAX_LISTENER_CPU, CPU_BOOST_APPLIED

[[ -n "${_LISTENER_CPU_SOURCED:-}" ]] && return 0
_LISTENER_CPU_SOURCED=1

set -euo pipefail

[[ -f "$(dirname "${BASH_SOURCE[0]}")/perf-common.sh" ]] \
    && source "$(dirname "${BASH_SOURCE[0]}")/perf-common.sh"

ORIGINAL_LISTENER_CPU_LIMIT=""
ORIGINAL_LISTENER_CPU_REQUEST=""
MAX_LISTENER_CPU=""

# Chart small-profile default (COST-7599). Used when CMSC/deployment omit limits.
CHART_LISTENER_CPU_LIMIT="${CHART_LISTENER_CPU_LIMIT:-300m}"
CHART_LISTENER_CPU_REQUEST="${CHART_LISTENER_CPU_REQUEST:-150m}"

parse_cpu_to_millicores() {
    local cpu_value="$1"
    if [[ "${cpu_value}" =~ ^([0-9]+)m$ ]]; then
        echo "${BASH_REMATCH[1]}"
    elif [[ "${cpu_value}" =~ ^([0-9]+)$ ]]; then
        echo "$((${BASH_REMATCH[1]} * 1000))"
    else
        echo "0"
    fi
}

calculate_max_listener_cpu() {
    MAX_LISTENER_CPU=""
    local listener_deploy
    listener_deploy="$(perf_deploy_name koku-listener)"

    local listener_node
    listener_node=$(perf_kubectl get pods -n "${NAMESPACE}" -l "app.kubernetes.io/component=listener" \
        -o jsonpath='{.items[0].spec.nodeName}' 2>/dev/null)

    if [[ -z "${listener_node}" ]]; then
        log_warning "Could not determine listener node, using default max of 2000m"
        MAX_LISTENER_CPU="2000"
        return
    fi

    local replica_count
    replica_count=$(perf_kubectl get deploy "${listener_deploy}" -n "${NAMESPACE}" \
        -o jsonpath='{.spec.replicas}' 2>/dev/null || echo "1")
    [[ "${replica_count}" -lt 1 ]] && replica_count=1

    local allocatable_cpu
    allocatable_cpu=$(perf_kubectl get node "${listener_node}" -o jsonpath='{.status.allocatable.cpu}' 2>/dev/null)
    local allocatable_millicores
    allocatable_millicores=$(parse_cpu_to_millicores "${allocatable_cpu}")

    local used_requests
    used_requests=$(perf_kubectl describe node "${listener_node}" 2>/dev/null | grep -A5 "Allocated resources" | grep "cpu" | awk '{print $2}' | sed 's/[^0-9]//g')
    if [[ -z "${used_requests}" ]]; then
        used_requests=0
    fi

    local listener_request
    if perf_operator_path; then
        listener_request="$(perf_cmsc_listener_cpu_request)"
    else
        listener_request=$(perf_kubectl get deploy "${listener_deploy}" -n "${NAMESPACE}" \
            -o jsonpath='{.spec.template.spec.containers[0].resources.requests.cpu}' 2>/dev/null || echo "")
    fi
    [[ -z "${listener_request}" ]] && listener_request="${CHART_LISTENER_CPU_REQUEST}"
    local listener_request_millicores
    listener_request_millicores=$(parse_cpu_to_millicores "${listener_request}")

    local available=$((allocatable_millicores - used_requests + listener_request_millicores - 500))

    if [[ "${replica_count}" -gt 1 ]]; then
        available=$((available / replica_count))
        log_verbose "Adjusting for ${replica_count} listener replicas: ${available}m per replica"
    fi

    if [[ "${available}" -gt 4000 ]]; then
        available=4000
    fi
    if [[ "${available}" -lt 500 ]]; then
        available=500
    fi

    log_verbose "Node ${listener_node}: allocatable=${allocatable_millicores}m, used=${used_requests}m, listener=${listener_request_millicores}m, replicas=${replica_count}, available=${available}m"
    MAX_LISTENER_CPU="${available}"
}

validate_cpu_limit() {
    local cpu_limit="$1"

    if [[ "${cpu_limit}" == "max" ]] || [[ "${cpu_limit}" == "none" ]]; then
        return 0
    fi

    if [[ ! "${cpu_limit}" =~ ^[0-9]+m?$ ]]; then
        log_error "Invalid CPU limit format: ${cpu_limit}"
        log_error "Expected format: <number>m (e.g., 500m, 1000m), <number> (e.g., 1, 2), or 'max'"
        return 1
    fi

    local millicores
    millicores=$(parse_cpu_to_millicores "${cpu_limit}")

    if [[ "${millicores}" -lt 100 ]]; then
        log_error "CPU limit too low: ${cpu_limit} (minimum: 100m)"
        return 1
    fi
    if [[ "${millicores}" -gt 4000 ]]; then
        log_error "CPU limit too high: ${cpu_limit} (maximum: 4000m / 4 cores)"
        return 1
    fi
    return 0
}

_read_listener_cpu_from_deploy() {
    local listener_deploy
    listener_deploy="$(perf_deploy_name koku-listener)"
    ORIGINAL_LISTENER_CPU_LIMIT=$(perf_kubectl get deploy "${listener_deploy}" -n "${NAMESPACE}" \
        -o jsonpath='{.spec.template.spec.containers[?(@.name=="listener")].resources.limits.cpu}' 2>/dev/null || echo "")
    ORIGINAL_LISTENER_CPU_REQUEST=$(perf_kubectl get deploy "${listener_deploy}" -n "${NAMESPACE}" \
        -o jsonpath='{.spec.template.spec.containers[?(@.name=="listener")].resources.requests.cpu}' 2>/dev/null || echo "")
}

_apply_listener_cpu_deploy() {
    local listener_deploy new_limit new_request
    listener_deploy="$(perf_deploy_name koku-listener)"
    new_limit="$1"
    new_request="$2"

    if perf_kubectl patch deploy "${listener_deploy}" -n "${NAMESPACE}" --type='json' \
        -p="[{\"op\": \"replace\", \"path\": \"/spec/template/spec/containers/0/resources/limits/cpu\", \"value\": \"${new_limit}\"},
             {\"op\": \"replace\", \"path\": \"/spec/template/spec/containers/0/resources/requests/cpu\", \"value\": \"${new_request}\"}]" \
        &>/dev/null; then
        return 0
    fi

    # Deployment may lack resources — use merge patch on the listener container.
    perf_kubectl patch deploy "${listener_deploy}" -n "${NAMESPACE}" --type merge -p "$(cat <<EOF
{
  "spec": {
    "template": {
      "spec": {
        "containers": [
          {
            "name": "listener",
            "resources": {
              "limits": {"cpu": "${new_limit}"},
              "requests": {"cpu": "${new_request}"}
            }
          }
        ]
      }
    }
  }
}
EOF
)"
}

set_listener_cpu() {
    local new_limit="$1"

    log_step "Setting listener CPU limit to ${new_limit}"

    if perf_operator_path; then
        ORIGINAL_LISTENER_CPU_LIMIT="$(perf_cmsc_listener_cpu_limit)"
        ORIGINAL_LISTENER_CPU_REQUEST="$(perf_cmsc_listener_cpu_request)"
        if [[ -z "${ORIGINAL_LISTENER_CPU_LIMIT}" ]]; then
            ORIGINAL_LISTENER_CPU_LIMIT="${CHART_LISTENER_CPU_LIMIT}"
            ORIGINAL_LISTENER_CPU_REQUEST="${CHART_LISTENER_CPU_REQUEST}"
            log_info "CMSC listener has no CPU limit — using chart default ${ORIGINAL_LISTENER_CPU_LIMIT}"
        fi
    else
        _read_listener_cpu_from_deploy
        if [[ -z "${ORIGINAL_LISTENER_CPU_LIMIT}" ]]; then
            ORIGINAL_LISTENER_CPU_LIMIT="${CHART_LISTENER_CPU_LIMIT}"
            ORIGINAL_LISTENER_CPU_REQUEST="${CHART_LISTENER_CPU_REQUEST}"
            log_info "Deployment listener has no CPU limit — using chart default ${ORIGINAL_LISTENER_CPU_LIMIT}"
        fi
    fi

    local current_millicores new_millicores
    current_millicores=$(parse_cpu_to_millicores "${ORIGINAL_LISTENER_CPU_LIMIT}")
    new_millicores=$(parse_cpu_to_millicores "${new_limit}")

    log_info "Current listener CPU: limit=${ORIGINAL_LISTENER_CPU_LIMIT}, request=${ORIGINAL_LISTENER_CPU_REQUEST}"

    if [[ "${current_millicores}" -eq "${new_millicores}" ]]; then
        log_info "Listener CPU limit already set to ${new_limit}, no change needed"
        ORIGINAL_LISTENER_CPU_LIMIT=""
        return 0
    fi

    if [[ "${new_millicores}" -lt "${current_millicores}" ]]; then
        log_warning "Decreasing CPU limit from ${ORIGINAL_LISTENER_CPU_LIMIT} to ${new_limit}"
    fi

    local new_request="$((new_millicores / 2))m"

    log_info "Setting listener CPU: limit=${new_limit}, request=${new_request}"

    if [[ "${DRY_RUN}" == "true" ]]; then
        if perf_operator_path; then
            log_info "DRY RUN: Would patch CMSC listener CPU to limit=${new_limit}, request=${new_request}"
        else
            log_info "DRY RUN: Would patch $(perf_deploy_name koku-listener) CPU to limit=${new_limit}, request=${new_request}"
        fi
        return 0
    fi

    if perf_operator_path; then
        if perf_patch_cmsc_listener_cpu "${new_limit}" "${new_request}"; then
            log_success "CMSC listener CPU set to ${new_limit}"
            if ! perf_wait_listener_rollout 180; then
                log_warning "Listener rollout timed out after CMSC patch, but continuing..."
            fi
        else
            log_error "Failed to patch CMSC listener CPU"
            ORIGINAL_LISTENER_CPU_LIMIT=""
            return 1
        fi
    elif _apply_listener_cpu_deploy "${new_limit}" "${new_request}"; then
        log_success "Listener CPU set to ${new_limit}"
        log_info "Waiting for listener rollout..."
        if ! perf_kubectl rollout status deploy/"$(perf_deploy_name koku-listener)" -n "${NAMESPACE}" --timeout=120s; then
            log_warning "Rollout timed out, but continuing..."
        fi
    else
        log_error "Failed to set listener CPU"
        ORIGINAL_LISTENER_CPU_LIMIT=""
        return 1
    fi
}

reset_listener_cpu() {
    if [[ -z "${ORIGINAL_LISTENER_CPU_LIMIT}" ]]; then
        return 0
    fi

    log_step "Resetting listener CPU to original values"
    log_info "Resetting listener CPU: limit=${ORIGINAL_LISTENER_CPU_LIMIT}, request=${ORIGINAL_LISTENER_CPU_REQUEST}"

    if [[ "${DRY_RUN}" == "true" ]]; then
        log_info "DRY RUN: Would reset listener CPU"
        return 0
    fi

    if perf_operator_path; then
        if perf_patch_cmsc_listener_cpu "${ORIGINAL_LISTENER_CPU_LIMIT}" "${ORIGINAL_LISTENER_CPU_REQUEST}"; then
            log_success "CMSC listener CPU reset to ${ORIGINAL_LISTENER_CPU_LIMIT}"
            perf_wait_listener_rollout 180 || true
        else
            log_warning "Failed to reset CMSC listener CPU"
        fi
    elif _apply_listener_cpu_deploy "${ORIGINAL_LISTENER_CPU_LIMIT}" "${ORIGINAL_LISTENER_CPU_REQUEST}"; then
        log_success "Listener CPU reset to ${ORIGINAL_LISTENER_CPU_LIMIT}"
    else
        log_warning "Failed to reset listener CPU - manual intervention may be needed"
    fi
}
