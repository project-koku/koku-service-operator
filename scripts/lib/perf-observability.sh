#!/usr/bin/env bash
# scripts/lib/perf-observability.sh — Observability, metrics collection, and S3 upload
#
# Requires: log_info, log_success, log_warning, log_error, log_step,
#           log_verbose, generate_metadata_json (all from parent/sibling scripts)
# Globals:  NAMESPACE, HELM_RELEASE_NAME, DRY_RUN, DEPLOY_OBSERVABILITY,
#           COLLECT_METRICS, UPLOAD_METRICS, S3_BUCKET, S3_ENDPOINT, S3_PREFIX,
#           S3_UPLOAD_TIMEOUT, TEST_RUN_ID, PERF_OUTPUT_DIR, PERF_PROFILE,
#           PERF_SUITE, METRICS_INTERVAL, METRICS_COLLECTOR_PID,
#           LOCAL_SCRIPTS_DIR, SKIP_GRAFANA_LINKS

[[ -n "${_PERF_OBSERVABILITY_SOURCED:-}" ]] && return 0
_PERF_OBSERVABILITY_SOURCED=1

set -euo pipefail

[[ -f "$(dirname "${BASH_SOURCE[0]}")/perf-common.sh" ]] \
    && source "$(dirname "${BASH_SOURCE[0]}")/perf-common.sh"

METRICS_COLLECTOR_PID=""

################################################################################
# Observability Stack (FLPATH-4061)
################################################################################

deploy_observability() {
    if [[ "${DEPLOY_OBSERVABILITY}" != "true" ]]; then
        log_verbose "Skipping observability deployment (--deploy-observability not specified)"
        return 0
    fi

    log_step "Deploying observability stack (FLPATH-4061)"

    local observability_script="${LOCAL_SCRIPTS_DIR}/deploy-observability.sh"
    if [[ ! -x "${observability_script}" ]]; then
        log_error "Observability deployment script not found at: ${observability_script}"
        return 1
    fi

    perf_sync_release_env
    export NAMESPACE="${NAMESPACE}"
    export SKIP_GRAFANA="true"

    if [[ "${DRY_RUN}" == "true" ]]; then
        log_info "DRY RUN: Would execute: ${observability_script}"
        return 0
    fi

    if ! "${observability_script}"; then
        log_warning "Observability deployment had issues, continuing..."
    else
        log_success "Observability stack deployed"
    fi
}

start_metrics_collection() {
    if [[ "${COLLECT_METRICS}" != "true" ]]; then
        return 0
    fi

    log_step "Starting metrics collection"

    local collect_script="${LOCAL_SCRIPTS_DIR}/observability/collect-metrics.sh"
    if [[ ! -x "${collect_script}" ]]; then
        log_error "Metrics collection script not found at: ${collect_script}"
        return 1
    fi

    # Generate TEST_RUN_ID if not already set
    if [[ -z "${TEST_RUN_ID:-}" ]]; then
        local version_slug
        version_slug="$(perf_detect_version_slug)"
        local epoch_time
        epoch_time=$(date +%s)

        if [[ "${PERF_SUITE}" != "all" ]]; then
            local suite_slug="${PERF_SUITE//,/+}"
            TEST_RUN_ID="${version_slug}-${PERF_PROFILE}-${suite_slug}-${epoch_time}"
        else
            TEST_RUN_ID="${version_slug}-${PERF_PROFILE}-${epoch_time}"
        fi
    fi

    mkdir -p "${PERF_OUTPUT_DIR}/${TEST_RUN_ID}/metrics"
    mkdir -p "${PERF_OUTPUT_DIR}/${TEST_RUN_ID}/results"
    mkdir -p "${PERF_OUTPUT_DIR}/${TEST_RUN_ID}/reports"

    perf_sync_release_env
    export NAMESPACE="${NAMESPACE}"
    export PERF_PROFILE="${PERF_PROFILE}"
    export TEST_RUN_ID="${TEST_RUN_ID}"
    export PERF_OUTPUT_DIR="${PERF_OUTPUT_DIR}"
    export OUTPUT_DIR="${PERF_OUTPUT_DIR}/${TEST_RUN_ID}/metrics"
    export METRICS_FLAT_OUTPUT=true

    if [[ "${DRY_RUN}" == "true" ]]; then
        log_info "DRY RUN: Would start metrics collection"
        log_info "  TEST_RUN_ID: ${TEST_RUN_ID}"
        log_info "  Output: ${PERF_OUTPUT_DIR}/${TEST_RUN_ID}/"
        return 0
    fi

    log_info "Starting metrics collection (interval: ${METRICS_INTERVAL}s)"
    log_info "  TEST_RUN_ID: ${TEST_RUN_ID}"
    log_info "  Output: ${PERF_OUTPUT_DIR}/${TEST_RUN_ID}/"
    log_info "    metrics/  - Prometheus snapshots"
    log_info "    results/  - Test results JSON"
    log_info "    reports/  - JUnit XML, HTML report"

    local max_dur="${METRICS_MAX_DURATION:-14400}"
    "${collect_script}" --continuous "${METRICS_INTERVAL}" --max-duration "${max_dur}" &
    METRICS_COLLECTOR_PID=$!

    log_success "Metrics collection started (PID: ${METRICS_COLLECTOR_PID})"
}

stop_metrics_collection() {
    if [[ -z "${METRICS_COLLECTOR_PID}" ]]; then
        return 0
    fi

    log_step "Stopping metrics collection"

    if [[ "${DRY_RUN}" == "true" ]]; then
        log_info "DRY RUN: Would stop metrics collection"
        return 0
    fi

    if kill -0 "${METRICS_COLLECTOR_PID}" 2>/dev/null; then
        kill -TERM "${METRICS_COLLECTOR_PID}" 2>/dev/null || true
        wait "${METRICS_COLLECTOR_PID}" 2>/dev/null || true
        log_success "Metrics collection stopped"
    fi

    METRICS_COLLECTOR_PID=""
}

upload_perf_results_to_s3() {
    if [[ "${UPLOAD_METRICS}" != "true" ]]; then
        return 0
    fi

    log_step "Uploading performance results to S3"

    if [[ -z "${S3_BUCKET:-}" ]]; then
        log_warning "S3_BUCKET not set, skipping upload"
        return 0
    fi

    if [[ -z "${TEST_RUN_ID:-}" ]]; then
        log_warning "TEST_RUN_ID not set, skipping upload"
        return 0
    fi

    local test_run_dir="${PERF_OUTPUT_DIR}/${TEST_RUN_ID}"
    if [[ ! -d "${test_run_dir}" ]]; then
        log_warning "Test run directory not found: ${test_run_dir}"
        return 0
    fi

    if [[ "${DRY_RUN}" == "true" ]]; then
        log_info "DRY RUN: Would upload ${test_run_dir}.tar.gz to s3://${S3_BUCKET}/${S3_PREFIX:-cost-onprem-performance/}${TEST_RUN_ID}.tar.gz"
        return 0
    fi

    generate_metadata_json

    local summary_script="$(dirname "${BASH_SOURCE[0]}")/../observability/generate-perf-summary.py"
    local s3_upload_script="$(dirname "${BASH_SOURCE[0]}")/../s3-upload.py"

    # Resolve a Python with boto3 — test venv is preferred since it already
    # has boto3 in requirements.txt.
    local _s3_python=""
    local _venv_python="$(dirname "${BASH_SOURCE[0]}")/../../tests/.venv/bin/python"
    if [[ -x "${_venv_python}" ]] && "${_venv_python}" -c "import boto3" 2>/dev/null; then
        _s3_python="${_venv_python}"
    elif command -v python3 &>/dev/null && python3 -c "import boto3" 2>/dev/null; then
        _s3_python="python3"
    fi

    if [[ -z "${_s3_python}" ]] || [[ ! -f "${s3_upload_script}" ]]; then
        log_error "S3 upload requires python3 with boto3 and scripts/s3-upload.py"
        return 1
    fi

    if [[ -f "${summary_script}" ]]; then
        "${_s3_python}" "${summary_script}" --run-dir "${test_run_dir}" 2>/dev/null \
            && log_info "Generated perf-summary.json" \
            || log_warning "Could not generate perf-summary.json (non-fatal)"
    fi

    # The perf-results bucket (eco-bucket-perf-scale) is public/anonymous and
    # the MinIO ingress uses an untrusted cert.  Default to skip-verify and
    # no-sign so uploads work without credentials or a custom CA bundle.
    # Override with S3_NO_VERIFY_SSL=false / S3_NO_SIGN_REQUEST=false for a
    # different S3 backend that requires TLS verification or signed requests.
    local endpoint_arg=""
    [[ -n "${S3_ENDPOINT:-}" ]] && endpoint_arg="--endpoint-url ${S3_ENDPOINT}"
    [[ "${S3_NO_VERIFY_SSL:-true}" == "true" ]] && endpoint_arg="${endpoint_arg} --no-verify-ssl"
    [[ "${S3_NO_SIGN_REQUEST:-true}" == "true" ]] && endpoint_arg="${endpoint_arg} --no-sign-request"
    local upload_timeout="${S3_UPLOAD_TIMEOUT:-120}"

    local s3_display="${S3_ENDPOINT:+${S3_ENDPOINT}/}${S3_BUCKET}"
    log_info "S3 preflight: checking ${s3_display} (verify_ssl=${S3_NO_VERIFY_SSL:-true}, sign=${S3_NO_SIGN_REQUEST:-true})"
    local preflight_out
    if preflight_out=$(timeout 30 "${_s3_python}" "${s3_upload_script}" ls "s3://${S3_BUCKET}/" ${endpoint_arg} 2>&1); then
        log_info "S3 preflight OK — ${s3_display} reachable"
    else
        log_error "S3 preflight FAILED — cannot reach ${s3_display}"
        log_error "  output: ${preflight_out}"
        return 1
    fi

    local tarball_path="${test_run_dir}.tar.gz"
    if [[ ! -f "${tarball_path}" ]]; then
        log_warning "Tarball not found: ${tarball_path}, skipping S3 upload"
        return 0
    fi

    local tarball_s3_key="${S3_PREFIX:-cost-onprem-performance/}${TEST_RUN_ID}.tar.gz"
    log_info "Uploading ${tarball_path##*/} to ${s3_display}/${tarball_s3_key}"

    local upload_rc=0
    timeout "${upload_timeout}" \
        "${_s3_python}" "${s3_upload_script}" cp "${tarball_path}" "s3://${S3_BUCKET}/${tarball_s3_key}" \
        ${endpoint_arg} \
        || upload_rc=$?

    if [[ ${upload_rc} -eq 124 ]]; then
        log_warning "S3 upload timed out after ${upload_timeout}s (non-fatal)"
    elif [[ ${upload_rc} -ne 0 ]]; then
        log_warning "S3 upload failed (exit code ${upload_rc}, non-fatal)"
    else
        log_success "Uploaded to ${s3_display}/${tarball_s3_key}"
    fi

    # Update the bucket-level index.json so Grafana can list all runs
    if [[ ${upload_rc} -eq 0 ]] && [[ -f "${summary_script}" ]]; then
        S3_ENDPOINT="${S3_ENDPOINT:-}" S3_BUCKET="${S3_BUCKET}" \
        S3_PREFIX="${S3_PREFIX:-cost-onprem-performance}" \
        "${_s3_python}" "${summary_script}" --run-dir "${test_run_dir}" --update-index 2>/dev/null \
            || log_warning "Could not update bucket index.json (non-fatal)"
    fi
}
