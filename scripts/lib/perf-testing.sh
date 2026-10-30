#!/usr/bin/env bash
# scripts/lib/perf-testing.sh — Performance test orchestration and per-profile config
#
# Requires: log_info, log_success, log_warning, log_error, log_step,
#           log_verbose (from parent script)
# Requires: parse_cpu_to_millicores, calculate_max_listener_cpu,
#           validate_cpu_limit, set_listener_cpu (from lib/listener-cpu.sh)
# Requires: start_metrics_collection, stop_metrics_collection,
#           upload_perf_results_to_s3, generate_metadata_json (from lib/perf-observability.sh)
# Globals:  NAMESPACE, CMSC_NAME, DRY_RUN, PERF_PROFILE, PERF_SUITE,
#           VERBOSE, LISTENER_CPU_LIMIT, CPU_BOOST_APPLIED,
#           ORIGINAL_LISTENER_CPU_LIMIT, MAX_LISTENER_CPU,
#           TEST_RUN_ID, PERF_OUTPUT_DIR, LOCAL_SCRIPTS_DIR,
#           PROJECT_ROOT, SKIP_GRAFANA_LINKS,
#           GRAFANA_URL, GRAFANA_USER, GRAFANA_PASSWORD, GRAFANA_NAMESPACE

[[ -n "${_PERF_TESTING_SOURCED:-}" ]] && return 0
_PERF_TESTING_SOURCED=1

[[ -f "$(dirname "${BASH_SOURCE[0]}")/perf-common.sh" ]] \
    && source "$(dirname "${BASH_SOURCE[0]}")/perf-common.sh"

################################################################################
# Per-Profile Cluster Configuration
################################################################################

# apply_perf_profile_config: Brings the live cluster into the correct state for
# the given PERF_PROFILE before tests run.  Called at the start of
# run_performance_tests() unless --skip-profile-config is set (COST-7599).
#
# Operator path (COST-8095): sizing is owned by the operator.  The harness
# patches CostManagementServiceConfig spec.profile (small|medium|large|xlarge;
# "baseline" maps to "small") and the operator derives replicas, resources,
# upload limits and timeouts from it.  Direct `oc scale` is NOT used: the
# reconciler re-applies desired state (SSA, 5-minute drift requeue) and would
# revert it.
#
# The same single merge patch sets spec.profile and nulls the explicit
# per-component fields earlier harness versions wrote (listener, celery ocp /
# summary workers, ros processor, kruize resources, ingress upload limits) so
# stale explicit values cannot override the profile.  kruize replicas is left
# alone (validation caps it at 1).
#
# With DRY_RUN=true the exact oc command and expected replicas are printed
# instead of executed.
#
# Expected replicas (listener/ocp/summary/ros-processor/kruize):
#   small (baseline) : 2/2/2/1/1
#   medium           : 2/2/2/2/1
#   large, xlarge    : 3/3/3/3/1
apply_perf_profile_config() {
    perf_sync_release_env
    local release
    release="$(perf_release_prefix)"
    local namespace="${NAMESPACE:-cost-onprem}"
    local ros_workloads_required=false
    if perf_ros_workloads_required; then
        ros_workloads_required=true
    fi

    local profile="${PERF_PROFILE:-}"
    local ros_processor_replicas=1
    local listener_replicas=2
    local ocp_worker_replicas=2
    local summary_worker_replicas=2
    local kruize_cpu_lim="2000m"

    case "${profile}" in
        baseline|small|"")
            profile="small"
            ;;
        medium)
            ros_processor_replicas=2
            ;;
        large|xlarge)
            ros_processor_replicas=3
            listener_replicas=3
            ocp_worker_replicas=3
            summary_worker_replicas=3
            ;;
        *)
            log_error "Unknown PERF_PROFILE '${profile}' (valid: baseline|small|medium|large|xlarge)"
            return 1
            ;;
    esac

    local patch
    patch="{\"spec\":{\"profile\":\"${profile}\""
    patch+=',"costManagement":{"listener":{"replicas":null,"resources":null}'
    patch+=',"celery":{"workers":{"ocp":{"replicas":null,"resources":null},"summary":{"replicas":null,"resources":null}}}}'
    patch+=',"ros":{"processor":{"replicas":null,"resources":null}}'
    patch+=',"kruize":{"resources":null}'
    patch+=',"ingress":{"maxUploadSize":null,"maxMemory":null}}}'

    log_step "Applying per-profile cluster config for profile: ${PERF_PROFILE:-} (spec.profile=${profile})"
    log_info "  expected replicas: listener=${listener_replicas} ocp=${ocp_worker_replicas} summary=${summary_worker_replicas} ros-processor=${ros_processor_replicas} kruize=1"

    if [[ "${DRY_RUN:-false}" == "true" ]]; then
        log_info "DRY RUN: oc patch cmsc ${release} -n ${namespace} --type merge -p '${patch}'"
        log_info "DRY RUN: would wait up to 600s for CMSC status.observedGeneration == metadata.generation and status.profile == ${profile}"
        log_info "DRY RUN: would verify replicas: listener=${listener_replicas} ocp=${ocp_worker_replicas} summary=${summary_worker_replicas} ros-processor=${ros_processor_replicas} kruize=1"
        return 0
    fi

    if ! perf_kubectl patch cmsc "${release}" -n "${namespace}" --type merge -p "${patch}"; then
        log_error "Could not patch CMSC ${namespace}/${release} spec.profile=${profile}"
        return 1
    fi
    log_info "Patched CMSC ${namespace}/${release}: spec.profile=${profile}"

    # Wait for the operator to reconcile the new profile before looking at
    # Deployments; otherwise rollout/verify can read the pre-patch state.
    log_info "Waiting for operator to observe profile ${profile} (up to 600s)..."
    local waited=0 gen observed status_profile
    while true; do
        gen="$(perf_kubectl get cmsc "${release}" -n "${namespace}" -o jsonpath='{.metadata.generation}' 2>/dev/null || true)"
        observed="$(perf_kubectl get cmsc "${release}" -n "${namespace}" -o jsonpath='{.status.observedGeneration}' 2>/dev/null || true)"
        status_profile="$(perf_kubectl get cmsc "${release}" -n "${namespace}" -o jsonpath='{.status.profile}' 2>/dev/null || true)"
        # status.profile is written once the sized workloads were applied for
        # the current generation, even if readiness-only gates (UI OAuth Secret,
        # Progressing) are unmet, so do not wait on Progressing (design 3.4).
        # Rollout readiness is checked by the waits below.
        if [[ -n "${gen}" && "${gen}" == "${observed}" && "${status_profile}" == "${profile}" ]]; then
            log_info "  operator observed generation ${gen}, status.profile=${status_profile}"
            break
        fi
        if (( waited >= 600 )); then
            log_error "Timed out waiting for CMSC ${namespace}/${release} to report profile ${profile} (generation=${gen:-?} observedGeneration=${observed:-?} status.profile=${status_profile:-?})"
            return 1
        fi
        sleep 5
        waited=$((waited + 5))
    done

    log_info "Waiting for operator to roll out profile (replica rollouts)..."
    local rollout_ok=true
    _wait_rollout() {
        local deploy="$1" required="${2:-true}"
        if ! perf_deployment_exists "${deploy}"; then
            if [[ "${required}" == "true" ]]; then
                log_error "  rollout skipped — ${deploy} not found"
                if [[ "${deploy}" == *"-ros-processor" || "${deploy}" == *"-kruize" ]]; then
                    log_error "  ROS workloads require spec.ros.enabled=true on CMSC ${namespace}/${release}"
                fi
                rollout_ok=false
            fi
            return
        fi
        if ! perf_kubectl rollout status deployment "${deploy}" -n "${namespace}" --timeout=3m 2>/dev/null; then
            log_warning "  rollout timeout for ${deploy}"
            rollout_ok=false
        fi
    }

    _wait_rollout "${release}-koku-listener"          true
    _wait_rollout "${release}-celery-worker-ocp"      true
    _wait_rollout "${release}-celery-worker-summary"  true
    _wait_rollout "${release}-ros-processor"          "${ros_workloads_required}"
    _wait_rollout "${release}-kruize"                 "${ros_workloads_required}"
    [[ "${rollout_ok}" == "true" ]] && log_success "Rollouts complete" || log_warning "Some rollouts timed out"

    # Verify replica counts
    log_info "Verifying deployed replica counts..."
    local verified=true
    _verify_replicas() {
        local deploy_name="$1" expected="$2" required="${3:-true}"
        if ! perf_deployment_exists "${deploy_name}"; then
            if [[ "${required}" == "true" ]]; then
                log_warning "  ✗ ${deploy_name}: not found (expected ${expected} replica(s))"
                verified=false
            fi
            return
        fi
        local actual
        actual=$(perf_kubectl get deployment "${deploy_name}" -n "${namespace}" \
                    -o jsonpath='{.spec.replicas}' 2>/dev/null)
        if [[ "${actual}" == "${expected}" ]]; then
            log_info "  ✓ ${deploy_name}: ${actual} replica(s)"
        else
            log_warning "  ✗ ${deploy_name}: expected ${expected}, got '${actual}'"
            verified=false
        fi
    }

    _verify_replicas "${release}-koku-listener"          "${listener_replicas}"          true
    _verify_replicas "${release}-celery-worker-ocp"      "${ocp_worker_replicas}"        true
    _verify_replicas "${release}-celery-worker-summary"  "${summary_worker_replicas}"    true
    _verify_replicas "${release}-ros-processor"          "${ros_processor_replicas}"     "${ros_workloads_required}"
    _verify_replicas "${release}-kruize"                 "1"                             "${ros_workloads_required}"

    if [[ "${verified}" == "true" ]]; then
        log_success "All replica counts verified for ${profile} profile"
    else
        log_warning "Some replica counts did not match — tests may not reflect expected scaling"
    fi

    # Verify Kruize pod actually has the expected CPU limit (catches scheduling
    # failures where the old ReplicaSet's pod keeps running with default resources)
    local actual_kruize_cpu_lim
    if ! perf_deployment_exists "${release}-kruize"; then
        log_verbose "  skipping Kruize CPU verification (not deployed)"
        return 0
    fi

    actual_kruize_cpu_lim=$(perf_kubectl get pods -n "${namespace}" -l app.kubernetes.io/component=ros-optimization \
        --field-selector=status.phase=Running -o jsonpath='{.items[0].spec.containers[0].resources.limits.cpu}' 2>/dev/null)
    if [[ -n "${actual_kruize_cpu_lim}" ]]; then
        log_info "  Kruize running pod CPU limit: ${actual_kruize_cpu_lim} (expected: ${kruize_cpu_lim})"
        # Normalize both values to millicores for comparison (K8s returns "2" for "2000m")
        local actual_m expected_m
        if [[ "${actual_kruize_cpu_lim}" =~ ^[0-9]+$ ]]; then
            actual_m=$(( actual_kruize_cpu_lim * 1000 ))
        else
            actual_m="${actual_kruize_cpu_lim%m}"
        fi
        if [[ "${kruize_cpu_lim}" =~ ^[0-9]+$ ]]; then
            expected_m=$(( kruize_cpu_lim * 1000 ))
        else
            expected_m="${kruize_cpu_lim%m}"
        fi
        if [[ "${actual_m}" != "${expected_m}" ]]; then
            log_warning "  Kruize pod has stale CPU limit — cleaning up stuck ReplicaSet"
            # Delete any Pending Kruize pods (from failed scheduling)
            perf_kubectl delete pods -n "${namespace}" -l app.kubernetes.io/component=ros-optimization \
                --field-selector=status.phase=Pending --grace-period=0 2>/dev/null || true
            # Scale stale ReplicaSets to 0
            for rs in $(perf_kubectl get rs -n "${namespace}" -l app.kubernetes.io/component=ros-optimization \
                -o jsonpath='{range .items[?(@.status.readyReplicas==0)]}{.metadata.name}{"\n"}{end}' 2>/dev/null); do
                perf_kubectl scale rs "${rs}" -n "${namespace}" --replicas=0 2>/dev/null || true
            done
            # Restart the deployment to pick up new resources
            perf_kubectl rollout restart deployment "${release}-kruize" -n "${namespace}" 2>/dev/null || true
            if perf_kubectl rollout status deployment "${release}-kruize" -n "${namespace}" --timeout=3m 2>/dev/null; then
                log_success "  Kruize restarted with correct CPU limit"
            else
                log_warning "  Kruize restart timed out — ROS tests may be slower than expected"
            fi
        fi
    fi
}

################################################################################
# Performance Test Execution (FLPATH-4036)
################################################################################

run_performance_tests() {
    log_step "Running performance tests (FLPATH-4036)"
    perf_sync_release_env

    local pytest_script="${LOCAL_SCRIPTS_DIR}/run-pytest.sh"
    if [[ ! -x "${pytest_script}" ]]; then
        log_error "Pytest runner not found at: ${pytest_script}"
        return 1
    fi

    log_info "Running performance tests with profile: ${PERF_PROFILE}"

    export PERF_PROFILE="${PERF_PROFILE}"

    # --skip-profile-config: test with whatever the chart deployed — no runtime
    # overrides.  Used by COST-7599 to validate that values.yaml defaults are
    # self-sufficient.  When set, both apply_perf_profile_config() and the
    # automatic listener CPU boost are skipped.  An explicit --listener-cpu value
    # is still honoured (e.g. --listener-cpu 500m sets exactly that value without
    # the profile config touching anything else).
    if [[ "${SKIP_PROFILE_CONFIG:-false}" == "true" ]]; then
        log_warning "Skipping apply_perf_profile_config() (--skip-profile-config)"
        log_info "Tests will run against chart defaults — no replica scaling, resource overrides, or timeout changes"

        # Still allow an explicit --listener-cpu value so VTC-001a can sweep
        # specific CPU levels without the rest of the profile config.
        if [[ -n "${LISTENER_CPU_LIMIT:-}" ]] && [[ "${LISTENER_CPU_LIMIT}" != "none" ]] \
                && [[ "${CPU_BOOST_APPLIED:-false}" != "true" ]]; then
            local explicit_cpu="${LISTENER_CPU_LIMIT}"
            if [[ "${explicit_cpu}" == "max" ]]; then
                calculate_max_listener_cpu
                explicit_cpu="${MAX_LISTENER_CPU}m"
            fi
            if validate_cpu_limit "${explicit_cpu}"; then
                log_step "Setting listener CPU to ${explicit_cpu} (explicit override, no profile config)"
                if set_listener_cpu "${explicit_cpu}"; then
                    CPU_BOOST_APPLIED=true
                    log_success "Listener CPU set to ${explicit_cpu} (was ${ORIGINAL_LISTENER_CPU_LIMIT})"
                fi
            fi
        fi
    else
        # Apply profile-specific replica scaling before tests run.
        if ! apply_perf_profile_config; then
            if perf_operator_path; then
                log_error "apply_perf_profile_config failed — aborting performance tests (operator path)"
                return 1
            fi
            log_warning "apply_perf_profile_config failed — continuing (chart path)"
        fi

        # Listener CPU boost — always applied for perf tests unless explicitly
        # disabled.  The listener is the principal processing bottleneck: at the
        # chart default (300m) it throttles every ingestion test, producing results
        # that measure the CPU cap rather than actual pipeline throughput.
        local perf_listener_cpu="${LISTENER_CPU_LIMIT:-max}"
        if [[ "${perf_listener_cpu}" != "none" ]] && [[ "${CPU_BOOST_APPLIED:-false}" != "true" ]]; then
            local effective_cpu_limit="${perf_listener_cpu}"
            if [[ "${perf_listener_cpu}" == "max" ]]; then
                calculate_max_listener_cpu
                effective_cpu_limit="${MAX_LISTENER_CPU}m"
                log_info "Listener CPU boost: calculated max = ${effective_cpu_limit}"
            fi
            if validate_cpu_limit "${effective_cpu_limit}"; then
                log_step "Boosting listener CPU to ${effective_cpu_limit} for performance tests"
                if set_listener_cpu "${effective_cpu_limit}"; then
                    CPU_BOOST_APPLIED=true
                    log_success "Listener CPU boosted to ${effective_cpu_limit} (was ${ORIGINAL_LISTENER_CPU_LIMIT})"
                else
                    if perf_operator_path; then
                        log_error "Could not boost listener CPU via CMSC — aborting performance tests (operator path)"
                        return 1
                    fi
                    log_warning "Could not boost listener CPU — results may reflect the 300m throttle"
                fi
            fi
        elif [[ "${CPU_BOOST_APPLIED:-false}" == "true" ]]; then
            log_info "Listener CPU already boosted by run_tests() — skipping duplicate boost"
        else
            log_warning "Listener CPU boost disabled (--listener-cpu none) — results will reflect chart defaults"
        fi
    fi

    start_metrics_collection

    local perf_args=()
    if [[ "${PERF_SUITE}" == "all" ]]; then
        # "all" runs every suite EXCEPT stress and soak tests (which can
        # take hours to days). Request those explicitly when needed.
        # NOTE: When adding a new perf suite, add it here too.
        perf_args+=("--perf-api" "--perf-ros" "--perf-ingestion" "--perf-scale"
                    "--perf-valkey" "--perf-db" "--perf-kafka"
                    "--perf-celery" "--perf-rbac")
    else
        IFS=',' read -ra suites <<< "${PERF_SUITE}"
        for suite in "${suites[@]}"; do
            case "${suite}" in
                api)              perf_args+=("--perf-api") ;;
                ros)              perf_args+=("--perf-ros") ;;
                ingestion)        perf_args+=("--perf-ingestion") ;;
                scale)            perf_args+=("--perf-scale") ;;
                soak)             perf_args+=("--perf-soak") ;;
                valkey)           perf_args+=("--perf-valkey") ;;
                db)               perf_args+=("--perf-db") ;;
                kafka)            perf_args+=("--perf-kafka") ;;
                celery)           perf_args+=("--perf-celery") ;;
                stress)           perf_args+=("--perf-stress") ;;
                stress_ramp)      perf_args+=("--perf-stress-ramp") ;;
                stress_recovery)  perf_args+=("--perf-stress-recovery") ;;
                rbac)             perf_args+=("--perf-rbac") ;;
            esac
        done
    fi
    if [[ "${VERBOSE}" == "true" ]]; then
        perf_args+=("-v")
    fi
    perf_args+=("-s")

    local output_info="tests/reports/performance/"
    if [[ -n "${TEST_RUN_ID:-}" ]]; then
        output_info="${PERF_OUTPUT_DIR}/${TEST_RUN_ID}/"
    fi

    if [[ "${DRY_RUN}" == "true" ]]; then
        log_info "DRY RUN: Would execute: ${pytest_script} ${perf_args[*]}"
        stop_metrics_collection
        return 0
    fi

    log_info "Performance test command: ${pytest_script} ${perf_args[*]}"
    log_info "Output directory: ${output_info}"

    local test_result=0
    if ! "${pytest_script}" "${perf_args[@]}"; then
        log_error "Performance tests failed"
        log_info "Check ${output_info} for detailed results"
        test_result=1
    else
        log_success "Performance tests completed"
        log_info "Results: ${output_info}"
    fi

    stop_metrics_collection

    # Generate reports BEFORE uploading so they're included in the S3 upload
    if [[ -n "${TEST_RUN_ID:-}" ]]; then
        local run_dir="${PERF_OUTPUT_DIR}/${TEST_RUN_ID}"
        local scripts_dir="$(dirname "${BASH_SOURCE[0]}")/../observability"

        generate_metadata_json

        local run_report_script="${scripts_dir}/generate-perf-run-report.py"
        local _report_python=""
        local _venv_py="$(dirname "${BASH_SOURCE[0]}")/../../test/pytest/.venv/bin/python"
        if [[ -x "${_venv_py}" ]]; then
            _report_python="${_venv_py}"
        elif command -v python3 &>/dev/null; then
            _report_python="python3"
        fi
        if [[ -f "${run_report_script}" ]] && [[ -n "${_report_python}" ]]; then
            log_info "Generating visual run report..."
            if "${_report_python}" "${run_report_script}" --run-dir "${run_dir}"; then
                log_success "Visual report: ${run_dir}/reports/perf-run-report.html"
            else
                log_warning "Could not generate visual run report (non-fatal)"
            fi
        fi

        if [[ "${SKIP_GRAFANA_LINKS}" != "true" ]]; then
            local grafana_script="${scripts_dir}/push-grafana-snapshot.py"
            if [[ -f "${grafana_script}" ]] && command -v python3 &>/dev/null; then
                log_info "Linking Grafana dashboard (if cluster is up)..."
                if python3 "${grafana_script}" \
                    --run-dir "${run_dir}" \
                    ${GRAFANA_URL:+--grafana-url "${GRAFANA_URL}"} \
                    ${GRAFANA_USER:+--grafana-user "${GRAFANA_USER}"} \
                    ${GRAFANA_PASSWORD:+--grafana-pass "${GRAFANA_PASSWORD}"} \
                    --namespace "${GRAFANA_NAMESPACE:-grafana}" 2>/dev/null; then
                    local links_file="${run_dir}/reports/grafana-links.json"
                    if [[ -f "${links_file}" ]]; then
                        local snap_url
                        snap_url=$(python3 -c "import json; d=json.load(open('${links_file}')); print(d.get('snapshot_url',''))" 2>/dev/null)
                        [[ -n "${snap_url}" ]] && log_success "Grafana snapshot: ${snap_url}"
                    fi
                fi
            fi
        else
            log_info "Skipping Grafana links (SKIP_GRAFANA_LINKS=true)"
        fi
    fi

    # Create tarball of the full run directory for easy sharing
    if [[ -n "${TEST_RUN_ID:-}" ]]; then
        local run_dir="${PERF_OUTPUT_DIR}/${TEST_RUN_ID}"
        local tarball="${PERF_OUTPUT_DIR}/${TEST_RUN_ID}.tar.gz"
        log_info "Creating run archive..."
        if tar -czf "${tarball}" -C "${PERF_OUTPUT_DIR}" "${TEST_RUN_ID}" 2>/dev/null; then
            local tar_size
            tar_size=$(du -h "${tarball}" | cut -f1)
            log_success "Archive: ${tarball} (${tar_size})"
        else
            log_warning "Could not create tarball (non-fatal)"
        fi
    fi

    upload_perf_results_to_s3

    return ${test_result}
}

generate_metadata_json() {
    local metadata_file="${PERF_OUTPUT_DIR}/${TEST_RUN_ID}/metadata.json"

    log_info "Generating metadata.json..."

    local ocp_version="unknown"
    local node_count=0
    local storage_type="unknown"

    ocp_version=$(perf_kubectl get clusterversion version -o jsonpath='{.status.desired.version}' 2>/dev/null || echo "unknown")
    node_count=$(perf_kubectl get nodes --no-headers 2>/dev/null | wc -l | tr -d ' ' || echo "0")

    if perf_kubectl get storagecluster -n openshift-storage &>/dev/null; then
        storage_type="ODF"
    elif perf_kubectl get storageclass s4-storage &>/dev/null; then
        storage_type="S4"
    fi

    local operator_version
    operator_version="$(perf_detect_version_slug)"

    local metrics_count=$(find "${PERF_OUTPUT_DIR}/${TEST_RUN_ID}/metrics" -name "*.json" 2>/dev/null | wc -l | tr -d ' ')
    local results_count=$(find "${PERF_OUTPUT_DIR}/${TEST_RUN_ID}/results" -name "*.json" 2>/dev/null | wc -l | tr -d ' ')
    local reports_count=$(find "${PERF_OUTPUT_DIR}/${TEST_RUN_ID}/reports" -type f 2>/dev/null | wc -l | tr -d ' ')

    cat > "${metadata_file}" <<EOF
{
  "test_run_id": "${TEST_RUN_ID}",
  "operator_version": "${operator_version}",
  "perf_profile": "${PERF_PROFILE}",
  "perf_suite": "${PERF_SUITE}",
  "listener_cpu_limit": "${LISTENER_CPU:-default}",
  "namespace": "${NAMESPACE}",
  "created_at": "$(date -u +%Y-%m-%dT%H:%M:%SZ)",
  "cluster_info": {
    "ocp_version": "${ocp_version}",
    "node_count": ${node_count},
    "storage_type": "${storage_type}",
    "platform": "${CLUSTER_PLATFORM:-unknown}"
  },
  "file_counts": {
    "metrics": ${metrics_count},
    "results": ${results_count},
    "reports": ${reports_count}
  }
}
EOF

    log_success "Generated: ${metadata_file}"
}
