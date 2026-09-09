#!/usr/bin/env bash
# soak-loop.sh — Outer loop for 7-day soak testing (operator path, COST-8147)
#
# Runs 1-hour pytest soak iterations back-to-back for N days, publishing
# a JSON checkpoint to S3 after each iteration. Designed to run on a
# hypervisor. Does NOT require screen/tmux — use --background (nohup-based)
# to keep it running after the SSH session ends, or run it inside
# screen/tmux yourself if one happens to be available.
#
# Usage:
#   SOAK_TESTS=true SOAK_DURATION_HOURS=1 \
#     ./scripts/soak-loop.sh --days 7 --background \
#       --s3-bucket "$S3_BUCKET" --s3-endpoint "$S3_ENDPOINT"
#
# S3_BUCKET/S3_ENDPOINT: same values you use for publishing results with
# collect-metrics.sh / deploy-test-cost-onprem.sh.
#
# Environment (inherited by run-pytest.sh):
#   SOAK_TESTS=true           Required — enables soak suite
#   SOAK_DURATION_HOURS=1     Duration of each iteration (default: 1)
#   NAMESPACE                 Target namespace (default: cost-onprem)
#   KUBECONFIG                Path to kubeconfig
#
# Stop signal: touch /tmp/soak-stop to gracefully stop after current iteration.
# Status:      tail -f /tmp/soak-loop.log (or check PID in /tmp/soak-loop.pid)

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"

# ── Defaults ──────────────────────────────────────────────────────────────

SOAK_DAYS=7
ITERATION_HOURS="${SOAK_DURATION_HOURS:-1}"
EXPLICIT_ITERATIONS=""
S3_BUCKET=""
S3_ENDPOINT=""
S3_NO_VERIFY_SSL="${S3_NO_VERIFY_SSL:-true}"
S3_NO_SIGN_REQUEST="${S3_NO_SIGN_REQUEST:-true}"
NAMESPACE="${NAMESPACE:-cost-onprem}"
STOP_FILE="/tmp/soak-stop"
LISTENER_CPU="${LISTENER_CPU:-none}"
DRY_RUN=false
BACKGROUND=false
LOG_FILE="/tmp/soak-loop.log"
PID_FILE="/tmp/soak-loop.pid"

# ── Parse Arguments ───────────────────────────────────────────────────────

usage() {
    cat <<EOF
Usage: $(basename "$0") [OPTIONS]

Options:
  --days N              Number of days to run (default: 7)
  --iteration-hours N   Duration of each pytest iteration in hours (default: 1).
                         Accepts fractional values (e.g. 0.25 for condensed mode).
  --iterations N        Run exactly N iterations, overriding --days (useful for
                         short condensed validation runs, e.g. --iterations 4
                         --iteration-hours 0.25 for a ~1h validation)
  --s3-bucket BUCKET    S3 bucket for checkpoints (required for S3 upload)
  --s3-endpoint URL     S3 endpoint URL (your team's perf-results storage)
  --listener-cpu MODE   Listener CPU mode: none, max (default: none)
  --namespace NS        Kubernetes namespace (default: cost-onprem)
  --stop-file PATH      Stop signal file (default: /tmp/soak-stop)
  --background, -d      Daemonize via nohup and return immediately (no
                         screen/tmux required). Survives SSH disconnects.
  --log-file PATH       Log file when backgrounded (default: /tmp/soak-loop.log)
  --pid-file PATH       PID file when backgrounded (default: /tmp/soak-loop.pid)
  --dry-run             Print what would run without executing
  -h, --help            Show this help

Condensed validation (~1h, exercises the full loop + checkpoint machinery):
  SOAK_TESTS=true SOAK_CONDENSED=true ./scripts/soak-loop.sh \\
    --iterations 4 --iteration-hours 0.25 \\
    --s3-bucket "\$S3_BUCKET" --s3-endpoint "\$S3_ENDPOINT"

Backgrounding (no screen/tmux needed):
  SOAK_TESTS=true ./scripts/soak-loop.sh --days 7 --background \\
    --s3-bucket "\$S3_BUCKET" --s3-endpoint "\$S3_ENDPOINT"

  # Monitor:
  tail -f /tmp/soak-loop.log
  # Stop gracefully after the current iteration:
  touch /tmp/soak-stop
  # Force kill (last resort):
  kill \$(cat /tmp/soak-loop.pid)
EOF
}

# Keep the raw args around so --background can re-exec without itself.
_ORIGINAL_ARGS=("$@")

while [[ $# -gt 0 ]]; do
    case "$1" in
        --days)          SOAK_DAYS="$2"; shift 2 ;;
        --iteration-hours) ITERATION_HOURS="$2"; shift 2 ;;
        --iterations)    EXPLICIT_ITERATIONS="$2"; shift 2 ;;
        --s3-bucket)     S3_BUCKET="$2"; shift 2 ;;
        --s3-endpoint)   S3_ENDPOINT="$2"; shift 2 ;;
        --listener-cpu)  LISTENER_CPU="$2"; shift 2 ;;
        --namespace)     NAMESPACE="$2"; shift 2 ;;
        --stop-file)     STOP_FILE="$2"; shift 2 ;;
        --background|-d) BACKGROUND=true; shift ;;
        --log-file)      LOG_FILE="$2"; shift 2 ;;
        --pid-file)      PID_FILE="$2"; shift 2 ;;
        --dry-run)       DRY_RUN=true; shift ;;
        -h|--help)       usage; exit 0 ;;
        *)               echo "Unknown option: $1"; usage; exit 1 ;;
    esac
done

# ── Daemonize (nohup-based; no screen/tmux dependency) ───────────────────
#
# Re-exec ourselves in the background with the same args minus --background,
# detached from the controlling terminal via nohup + disown, so the loop
# survives the SSH session ending. This is the standard fallback pattern
# when screen/tmux aren't installed on the host.

if [[ "${BACKGROUND}" == "true" ]]; then
    child_args=()
    for arg in "${_ORIGINAL_ARGS[@]}"; do
        case "${arg}" in
            --background|-d) continue ;;
            *) child_args+=("${arg}") ;;
        esac
    done

    if [[ -f "${PID_FILE}" ]] && kill -0 "$(cat "${PID_FILE}")" 2>/dev/null; then
        echo "ERROR: soak-loop already running with PID $(cat "${PID_FILE}") (${PID_FILE})" >&2
        exit 1
    fi

    export _SOAK_PID_FILE="${PID_FILE}"
    nohup "${BASH_SOURCE[0]}" "${child_args[@]}" > "${LOG_FILE}" 2>&1 &
    child_pid=$!
    disown "${child_pid}"
    echo "${child_pid}" > "${PID_FILE}"

    # Pre-flight (cluster reachability, SOAK_TESTS, S3) typically fails fast
    # if something's wrong. Give it a moment and verify the child actually
    # survived pre-flight before declaring success — otherwise a fast failure
    # looks identical to "started fine" and the EXIT trap will have already
    # removed the PID file by the time anyone checks on it.
    # Skip this check for --dry-run, which is expected to exit quickly.
    sleep 3
    if [[ "${DRY_RUN}" == "true" ]] || kill -0 "${child_pid}" 2>/dev/null; then
        echo "soak-loop started in background (PID ${child_pid})"
        echo "  Log:  ${LOG_FILE}"
        echo "  PID:  ${PID_FILE}"
        echo "  Stop: touch ${STOP_FILE}   (graceful, after current iteration)"
        echo "  Kill: kill ${child_pid}    (last resort)"
        exit 0
    else
        echo "ERROR: soak-loop exited immediately after starting (PID ${child_pid} is dead)." >&2
        echo "This usually means pre-flight failed (SOAK_TESTS not set, cluster" >&2
        echo "unreachable, or unhealthy pods). Log output (${LOG_FILE}):" >&2
        echo "---" >&2
        cat "${LOG_FILE}" >&2 2>/dev/null || echo "(log file not found)" >&2
        echo "---" >&2
        rm -f "${PID_FILE}"
        exit 1
    fi
fi

# ── Logging ───────────────────────────────────────────────────────────────

log()      { echo "[$(date '+%Y-%m-%d %H:%M:%S')] $*"; }
log_info() { log "INFO  $*"; }
log_warn() { log "WARN  $*"; }
log_err()  { log "ERROR $*"; }

# ── Derived Values ────────────────────────────────────────────────────────

RUN_ID="soak-$(date +%Y%m%d-%H%M%S)"
if [[ -n "${EXPLICIT_ITERATIONS}" ]]; then
    TOTAL_ITERATIONS="${EXPLICIT_ITERATIONS}"
else
    # ITERATION_HOURS may be fractional (e.g. 0.25 for condensed mode), so use
    # awk rather than bash's integer-only `$(( ))` arithmetic.
    TOTAL_ITERATIONS=$(awk -v d="${SOAK_DAYS}" -v h="${ITERATION_HOURS}" 'BEGIN { printf "%d", (d * 24) / h }')
fi
if [[ "${TOTAL_ITERATIONS}" -lt 1 ]]; then
    TOTAL_ITERATIONS=1
fi
RESULTS_DIR="${PROJECT_ROOT}/test/pytest/soak-runs/${RUN_ID}"
CHECKPOINT_DIR="${RESULTS_DIR}/checkpoints"
S3_UPLOAD_SCRIPT="${SCRIPT_DIR}/s3-upload.py"

# Resolve a Python interpreter with boto3 available for S3 uploads.
#
# This is intentionally NOT cached in a global at script startup: the test
# venv (test/pytest/.venv) that provides boto3 doesn't exist yet on a fresh
# checkout — it's created by run-pytest.sh during the *first* iteration's
# dependency setup. Caching this once, before the loop has even run once,
# would permanently disable S3 uploads for the whole run even though boto3
# becomes available moments later. Call this fresh each time it's needed.
resolve_s3_python() {
    if [[ -f "${PROJECT_ROOT}/test/pytest/.venv/bin/python" ]] && \
       "${PROJECT_ROOT}/test/pytest/.venv/bin/python" -c "import boto3" 2>/dev/null; then
        echo "${PROJECT_ROOT}/test/pytest/.venv/bin/python"
    elif python3 -c "import boto3" 2>/dev/null; then
        echo "python3"
    fi
}

# ── Pre-flight ────────────────────────────────────────────────────────────

preflight() {
    log_info "=== Soak Loop Pre-flight ==="
    log_info "Run ID:          ${RUN_ID}"
    log_info "Days:            ${SOAK_DAYS}"
    log_info "Iteration hours: ${ITERATION_HOURS}"
    log_info "Condensed mode: ${SOAK_CONDENSED:-false}"
    log_info "Total iterations: ${TOTAL_ITERATIONS}"
    log_info "Namespace:       ${NAMESPACE}"
    log_info "Listener CPU:    ${LISTENER_CPU}"
    log_info "Results dir:     ${RESULTS_DIR}"
    log_info "Stop file:       ${STOP_FILE}"
    log_info "S3 bucket:       ${S3_BUCKET:-<none>}"
    log_info "S3 endpoint:     ${S3_ENDPOINT:-<none>}"
    log_info ""

    # Verify soak test environment
    if [[ "${SOAK_TESTS:-}" != "true" ]]; then
        log_err "SOAK_TESTS must be set to 'true'"
        exit 1
    fi

    # Skip cluster/S3 checks for dry-run
    if [[ "${DRY_RUN}" == "true" ]]; then
        mkdir -p "${CHECKPOINT_DIR}"
        return
    fi

    # Verify cluster access
    if ! kubectl cluster-info &>/dev/null; then
        log_err "Cannot reach cluster — check KUBECONFIG"
        exit 1
    fi
    log_info "Cluster:         $(kubectl cluster-info 2>&1 | head -1)"

    # Verify pods are healthy.
    # NOTE: `grep -v "Running"` exits 1 when *every* pod is Running (i.e. the
    # healthy case) — with `pipefail` that would otherwise kill the whole
    # script via `set -e`. The `|| not_running=0` fallback guards against
    # exactly that "zero matches" outcome (not against a kubectl failure,
    # which still surfaces as an empty/erroneous count here).
    local not_running
    not_running=$(kubectl get pods -n "${NAMESPACE}" -l "app.kubernetes.io/managed-by=koku-service-operator" \
        --field-selector=status.phase!=Succeeded \
        --no-headers 2>/dev/null | grep -v "Running" | wc -l) || not_running=0
    if [[ "${not_running}" -gt 0 ]]; then
        log_warn "${not_running} pod(s) not in Running state"
        kubectl get pods -n "${NAMESPACE}" --no-headers | grep -v "Running\|Completed" || true
    else
        log_info "All pods healthy"
    fi

    # Verify S3 connectivity (if configured). Note: we do NOT permanently
    # disable S3_BUCKET here just because boto3 isn't available yet — on a
    # fresh checkout the test venv is created by the *first* iteration
    # (see resolve_s3_python() above), so it may become available moments
    # after this check runs. publish_checkpoint()/publish_final_summary()
    # re-resolve the interpreter on every call and will pick it up then.
    if [[ -n "${S3_BUCKET}" ]]; then
        local preflight_s3_python
        preflight_s3_python="$(resolve_s3_python)"
        if [[ -n "${preflight_s3_python}" ]] && [[ -f "${S3_UPLOAD_SCRIPT}" ]]; then
            local -a s3_args=()
            [[ -n "${S3_ENDPOINT}" ]] && s3_args+=("--endpoint-url" "${S3_ENDPOINT}")
            [[ "${S3_NO_VERIFY_SSL}" == "true" ]] && s3_args+=("--no-verify-ssl")
            [[ "${S3_NO_SIGN_REQUEST}" == "true" ]] && s3_args+=("--no-sign-request")

            if timeout 15 "${preflight_s3_python}" "${S3_UPLOAD_SCRIPT}" ls "s3://${S3_BUCKET}/" "${s3_args[@]}" &>/dev/null; then
                log_info "S3 preflight OK"
            else
                log_warn "S3 preflight failed (bucket/endpoint unreachable) — checkpoints will be local only for this run"
                S3_BUCKET=""
            fi
        else
            log_warn "boto3 not yet available (test venv not created) — will retry automatically once the first iteration sets it up"
        fi
    fi

    # Clean any stale stop signal
    rm -f "${STOP_FILE}"

    mkdir -p "${CHECKPOINT_DIR}"
    log_info ""
}

# ── S3 Checkpoint ─────────────────────────────────────────────────────────

publish_checkpoint() {
    local iteration="$1"
    local status="$2"
    local duration_s="$3"
    local junit_file="$4"

    local checkpoint_file
    checkpoint_file="${CHECKPOINT_DIR}/checkpoint-$(printf '%03d' "${iteration}").json"
    local elapsed_total_s=$(( $(date +%s) - SOAK_START_EPOCH ))

    # Extract test counts from JUnit XML if available
    local tests=0 passed=0 failures=0 skipped=0 errors=0
    if [[ -f "${junit_file}" ]]; then
        tests=$(grep -oP 'tests="\K[0-9]+' "${junit_file}" 2>/dev/null | head -1 || echo 0)
        failures=$(grep -oP 'failures="\K[0-9]+' "${junit_file}" 2>/dev/null | head -1 || echo 0)
        errors=$(grep -oP 'errors="\K[0-9]+' "${junit_file}" 2>/dev/null | head -1 || echo 0)
        skipped=$(grep -oP 'skipped="\K[0-9]+' "${junit_file}" 2>/dev/null | head -1 || echo 0)
        passed=$(( tests - failures - errors - skipped ))
    fi

    # Collect quick cluster health snapshot
    local pod_restarts=0 pods_not_ready=0
    pod_restarts=$(kubectl get pods -n "${NAMESPACE}" -l "app.kubernetes.io/managed-by=koku-service-operator" \
        -o jsonpath='{range .items[*]}{range .status.containerStatuses[*]}{.restartCount}{"\n"}{end}{end}' 2>/dev/null \
        | awk '{s+=$1} END {print s+0}') || pod_restarts=0
    pods_not_ready=$(kubectl get pods -n "${NAMESPACE}" -l "app.kubernetes.io/managed-by=koku-service-operator" \
        --field-selector=status.phase!=Succeeded \
        --no-headers 2>/dev/null | grep -cv "Running" 2>/dev/null) || pods_not_ready=0

    cat > "${checkpoint_file}" <<CHECKPOINT_EOF
{
  "run_id": "${RUN_ID}",
  "iteration": ${iteration},
  "total_iterations": ${TOTAL_ITERATIONS},
  "status": "${status}",
  "timestamp": "$(date -u '+%Y-%m-%dT%H:%M:%SZ')",
  "iteration_duration_s": ${duration_s},
  "elapsed_total_s": ${elapsed_total_s},
  "elapsed_total_hours": $(echo "scale=1; ${elapsed_total_s} / 3600" | bc),
  "remaining_iterations": $(( TOTAL_ITERATIONS - iteration )),
  "test_results": {
    "tests": ${tests},
    "passed": ${passed},
    "failed": ${failures:-0},
    "errors": ${errors},
    "skipped": ${skipped}
  },
  "cluster_health": {
    "total_pod_restarts": ${pod_restarts},
    "pods_not_ready": ${pods_not_ready}
  },
  "config": {
    "namespace": "${NAMESPACE}",
    "iteration_hours": ${ITERATION_HOURS},
    "soak_days": ${SOAK_DAYS},
    "listener_cpu": "${LISTENER_CPU}",
    "condensed": $([ "${SOAK_CONDENSED:-}" = "true" ] && echo "true" || echo "false")
  }
}
CHECKPOINT_EOF

    log_info "Checkpoint saved: ${checkpoint_file}"

    # Also write a "latest" symlink for easy monitoring
    cp "${checkpoint_file}" "${CHECKPOINT_DIR}/checkpoint-latest.json"

    # Upload to S3 if configured. Resolved fresh (not cached) since the test
    # venv providing boto3 may not have existed yet at script startup.
    local s3_python
    s3_python="$(resolve_s3_python)"
    if [[ -n "${S3_BUCKET}" ]] && [[ -n "${s3_python}" ]] && [[ -f "${S3_UPLOAD_SCRIPT}" ]]; then
        local s3_prefix="${S3_PREFIX:-cost-onprem-performance}/${RUN_ID}"
        local -a s3_args=()
        [[ -n "${S3_ENDPOINT}" ]] && s3_args+=("--endpoint-url" "${S3_ENDPOINT}")
        [[ "${S3_NO_VERIFY_SSL}" == "true" ]] && s3_args+=("--no-verify-ssl")
        [[ "${S3_NO_SIGN_REQUEST}" == "true" ]] && s3_args+=("--no-sign-request")

        if timeout 30 "${s3_python}" "${S3_UPLOAD_SCRIPT}" cp \
            "${checkpoint_file}" "s3://${S3_BUCKET}/${s3_prefix}/$(basename "${checkpoint_file}")" \
            "${s3_args[@]}" 2>/dev/null; then
            log_info "Checkpoint uploaded to s3://${S3_BUCKET}/${s3_prefix}/$(basename "${checkpoint_file}")"
        else
            log_warn "S3 upload failed (non-fatal) — checkpoint available locally"
        fi

        # Also upload latest
        timeout 30 "${s3_python}" "${S3_UPLOAD_SCRIPT}" cp \
            "${CHECKPOINT_DIR}/checkpoint-latest.json" \
            "s3://${S3_BUCKET}/${s3_prefix}/checkpoint-latest.json" \
            "${s3_args[@]}" 2>/dev/null || true
    elif [[ -n "${S3_BUCKET}" ]]; then
        log_warn "boto3 still not available — checkpoint ${iteration} saved locally only (will retry next iteration)"
    fi
}

# ── Final Summary ─────────────────────────────────────────────────────────

publish_final_summary() {
    local completed="$1"
    local total_passed="$2"
    local total_failed="$3"
    local elapsed_s=$(( $(date +%s) - SOAK_START_EPOCH ))

    local summary_file="${RESULTS_DIR}/final-results.json"
    cat > "${summary_file}" <<SUMMARY_EOF
{
  "run_id": "${RUN_ID}",
  "status": "$([ "${total_failed}" -eq 0 ] && echo "PASSED" || echo "FAILED")",
  "completed_iterations": ${completed},
  "total_iterations": ${TOTAL_ITERATIONS},
  "total_passed": ${total_passed},
  "total_failed": ${total_failed},
  "elapsed_hours": $(echo "scale=1; ${elapsed_s} / 3600" | bc),
  "start_time": "${SOAK_START_TIME}",
  "end_time": "$(date -u '+%Y-%m-%dT%H:%M:%SZ')",
  "config": {
    "namespace": "${NAMESPACE}",
    "iteration_hours": ${ITERATION_HOURS},
    "soak_days": ${SOAK_DAYS},
    "listener_cpu": "${LISTENER_CPU}",
    "condensed": $([ "${SOAK_CONDENSED:-}" = "true" ] && echo "true" || echo "false")
  }
}
SUMMARY_EOF

    log_info "Final summary: ${summary_file}"

    # Upload to S3 (resolved fresh — see resolve_s3_python() for why)
    local s3_python
    s3_python="$(resolve_s3_python)"
    if [[ -n "${S3_BUCKET}" ]] && [[ -n "${s3_python}" ]] && [[ -f "${S3_UPLOAD_SCRIPT}" ]]; then
        local s3_prefix="${S3_PREFIX:-cost-onprem-performance}/${RUN_ID}"
        local -a s3_args=()
        [[ -n "${S3_ENDPOINT}" ]] && s3_args+=("--endpoint-url" "${S3_ENDPOINT}")
        [[ "${S3_NO_VERIFY_SSL}" == "true" ]] && s3_args+=("--no-verify-ssl")
        [[ "${S3_NO_SIGN_REQUEST}" == "true" ]] && s3_args+=("--no-sign-request")

        timeout 30 "${s3_python}" "${S3_UPLOAD_SCRIPT}" cp \
            "${summary_file}" "s3://${S3_BUCKET}/${s3_prefix}/final-results.json" \
            "${s3_args[@]}" 2>/dev/null \
            && log_info "Final summary uploaded to s3://${S3_BUCKET}/${s3_prefix}/final-results.json" \
            || log_warn "S3 upload of final summary failed"
    elif [[ -n "${S3_BUCKET}" ]]; then
        log_warn "boto3 not available — final summary saved locally only"
    fi
}

# ── Main Loop ─────────────────────────────────────────────────────────────

main() {
    # If launched by our own --background re-exec, clean up the PID file
    # we were told about once this process exits (success, failure, or signal).
    if [[ -n "${_SOAK_PID_FILE:-}" ]]; then
        trap 'rm -f "${_SOAK_PID_FILE}"' EXIT
    fi

    preflight

    if [[ "${DRY_RUN}" == "true" ]]; then
        log_info "[DRY RUN] Would run ${TOTAL_ITERATIONS} iterations of ${ITERATION_HOURS}h each over ${SOAK_DAYS} days"
        log_info "[DRY RUN] Command per iteration:"
        log_info "  NAMESPACE=${NAMESPACE} SOAK_TESTS=true SOAK_DURATION_HOURS=${ITERATION_HOURS} \\"
        log_info "    ./scripts/run-pytest.sh --perf-soak --no-ui"
        exit 0
    fi

    SOAK_START_EPOCH=$(date +%s)
    SOAK_START_TIME="$(date -u '+%Y-%m-%dT%H:%M:%SZ')"
    local total_passed=0
    local total_failed=0

    log_info "=== Starting soak loop: ${TOTAL_ITERATIONS} iterations over ${SOAK_DAYS} days ==="
    log_info ""

    for (( i=1; i<=TOTAL_ITERATIONS; i++ )); do
        # Check stop signal
        if [[ -f "${STOP_FILE}" ]]; then
            log_info "Stop signal detected (${STOP_FILE}). Stopping after $(( i - 1 )) iterations."
            rm -f "${STOP_FILE}"
            break
        fi

        log_info "━━━ Iteration ${i}/${TOTAL_ITERATIONS} ━━━ ($(date '+%Y-%m-%d %H:%M')) ━━━"
        local iter_start
        iter_start=$(date +%s)
        local iter_status="passed"
        local iter_junit="${RESULTS_DIR}/iteration-${i}/reports/junit.xml"

        mkdir -p "${RESULTS_DIR}/iteration-${i}/reports"

        # Run the soak test iteration
        export SOAK_TESTS=true
        export SOAK_DURATION_HOURS="${ITERATION_HOURS}"
        export NAMESPACE="${NAMESPACE}"
        export PERF_OUTPUT_DIR="${RESULTS_DIR}/iteration-${i}"
        TEST_RUN_ID="iter-$(printf '%03d' "${i}")"
        export TEST_RUN_ID

        if NAMESPACE="${NAMESPACE}" SOAK_TESTS=true SOAK_DURATION_HOURS="${ITERATION_HOURS}" \
            "${SCRIPT_DIR}/run-pytest.sh" --perf-soak --no-ui; then
            log_info "Iteration ${i} PASSED"
            total_passed=$((total_passed + 1))
        else
            log_warn "Iteration ${i} FAILED (exit code $?)"
            iter_status="failed"
            total_failed=$((total_failed + 1))
        fi

        local iter_end
        iter_end=$(date +%s)
        local iter_duration=$(( iter_end - iter_start ))
        log_info "Iteration ${i} completed in $(( iter_duration / 60 ))m $(( iter_duration % 60 ))s"

        # Find the JUnit file (may be in the perf output dir or default reports dir)
        if [[ ! -f "${iter_junit}" ]]; then
            iter_junit=$(find "${RESULTS_DIR}/iteration-${i}" -name "junit.xml" 2>/dev/null | head -1 || echo "")
        fi

        # Publish checkpoint
        publish_checkpoint "${i}" "${iter_status}" "${iter_duration}" "${iter_junit:-}"

        # Brief pause between iterations to let pods settle
        if [[ ${i} -lt ${TOTAL_ITERATIONS} ]]; then
            log_info "Pausing 60s before next iteration..."
            sleep 60
        fi

        log_info ""
    done

    # Final summary
    local completed=$(( total_passed + total_failed ))
    publish_final_summary "${completed}" "${total_passed}" "${total_failed}"

    log_info ""
    log_info "=== Soak Loop Complete ==="
    log_info "Iterations: ${completed}/${TOTAL_ITERATIONS} (${total_passed} passed, ${total_failed} failed)"
    log_info "Duration:   $(( ($(date +%s) - SOAK_START_EPOCH) / 3600 )) hours"
    log_info "Results:    ${RESULTS_DIR}"

    if [[ ${total_failed} -gt 0 ]]; then
        log_err "SOAK FAILED — ${total_failed} iteration(s) had failures"
        exit 1
    else
        log_info "SOAK PASSED — all ${completed} iterations succeeded"
        exit 0
    fi
}

main "$@"
