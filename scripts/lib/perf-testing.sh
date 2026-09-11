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
# Operator path (COST-8147): resource limits and ingress timeouts are managed
# via the CostManagementServiceConfig CR — not via helm upgrade.  Phase 1 is
# intentionally a no-op stub.  To adjust resources for a profile, patch the
# CMSC spec before invoking this script.
#
# Phase 2 — oc scale
#   Replica counts are set directly (idempotent, fast).
#   Kruize is always kept at replicas=1 (scaling degrades throughput,
#   see PERF-FINDING-004).
#
# Profile matrix (chart defaults = small since COST-7599):
#   baseline/small : chart defaults (no-op — 2 replicas, 180s timeouts, etc.)
#   medium         : replicas=2; raised CPU/memory, 200MB upload
#   large          : replicas=3; raised resources, 500MB upload, 600s timeouts
#   xlarge         : replicas=3; higher worker CPU (1000m/2000m) for tag processing
apply_perf_profile_config() {
    perf_sync_release_env
    local release
    release="$(perf_release_prefix)"
    local namespace="${NAMESPACE:-cost-onprem}"
    local ros_workloads_required=false
    if perf_ros_workloads_required; then
        ros_workloads_required=true
    fi

    # Baseline defaults match the chart's values.yaml (small profile).
    # After COST-7599 these are the new chart defaults — the baseline/small
    # case below is intentionally a no-op.
    local ros_processor_replicas=1
    local listener_replicas=2
    local ocp_worker_replicas=2
    local summary_worker_replicas=2

    local kruize_cpu_req="1000m"  kruize_cpu_lim="2000m"
    local ros_mem_req="1Gi"       ros_mem_lim="1Gi"
    local listener_mem_req="300Mi" listener_mem_lim="600Mi"
    local ocp_worker_cpu_req="250m"  ocp_worker_cpu_lim="500m"
    local ocp_worker_mem_req="512Mi" ocp_worker_mem_lim="1Gi"
    local summary_worker_cpu_req="250m" summary_worker_cpu_lim="500m"
    local max_upload_size="104857600"   # 100MB chart default
    local max_upload_mem="33554432"    # 32MB chart default (in-memory buffer before spilling to disk)
    local app_mem_req="1Gi"  app_mem_lim="1Gi"   # resources.application (ingress pod + others)
    local haproxy_timeout="180s"
    local ingress_timeout="180s"   ingress_per_try_timeout="60s"

    case "${PERF_PROFILE}" in
        small)
            # Small profile now matches chart defaults — no overrides needed.
            ;;
        medium)
            ros_processor_replicas=2
            listener_replicas=2
            ocp_worker_replicas=2
            summary_worker_replicas=2
            kruize_cpu_lim="2000m"
            ros_mem_req="2Gi";          ros_mem_lim="4Gi"
            listener_mem_req="1Gi";     listener_mem_lim="2Gi"
            ocp_worker_cpu_req="250m";  ocp_worker_cpu_lim="1000m"
            ocp_worker_mem_req="512Mi"; ocp_worker_mem_lim="2Gi"
            summary_worker_cpu_req="250m"; summary_worker_cpu_lim="1000m"
            max_upload_size="209715200"   # 200MB
            max_upload_mem="67108864"     # 64MB — reduce disk spill for medium payloads
            app_mem_req="1Gi";          app_mem_lim="2Gi"   # PERF-FINDING-022: ingress OOM on large uploads
            haproxy_timeout="180s"
            ingress_timeout="180s";       ingress_per_try_timeout="180s"
            ;;
        large)
            ros_processor_replicas=3
            listener_replicas=3
            ocp_worker_replicas=3
            summary_worker_replicas=3
            kruize_cpu_lim="2000m"
            ros_mem_req="2Gi";          ros_mem_lim="4Gi"
            listener_mem_req="2Gi";     listener_mem_lim="4Gi"
            ocp_worker_cpu_req="500m";  ocp_worker_cpu_lim="1000m"
            ocp_worker_mem_req="1Gi";   ocp_worker_mem_lim="2Gi"
            summary_worker_cpu_req="500m"; summary_worker_cpu_lim="1000m"
            max_upload_size="524288000"   # 500MB
            max_upload_mem="134217728"    # 128MB — matches prior validated runs; larger values destabilize the pipeline
            app_mem_req="2Gi";          app_mem_lim="4Gi"   # PERF-FINDING-022: ingress OOM on large uploads
            haproxy_timeout="600s"
            ingress_timeout="600s";       ingress_per_try_timeout="300s"
            ;;
        xlarge)
            # 3.3x data volume vs large (23 clusters, 346 nodes, 6954 cores)
            # Same replica/resource config as large — cluster headroom is the constraint.
            # Worker CPU raised to 1000m/2000m to handle tag-based cost model processing.
            ros_processor_replicas=3
            listener_replicas=3
            ocp_worker_replicas=3
            summary_worker_replicas=3
            kruize_cpu_req="1000m";     kruize_cpu_lim="2000m"
            ros_mem_req="2Gi";          ros_mem_lim="4Gi"
            listener_mem_req="2Gi";     listener_mem_lim="4Gi"
            ocp_worker_cpu_req="1000m"; ocp_worker_cpu_lim="2000m"
            ocp_worker_mem_req="2Gi";   ocp_worker_mem_lim="4Gi"
            summary_worker_cpu_req="1000m"; summary_worker_cpu_lim="2000m"
            max_upload_size="524288000"   # 500MB
            max_upload_mem="134217728"    # 128MB
            app_mem_req="2Gi";          app_mem_lim="4Gi"
            haproxy_timeout="600s"
            ingress_timeout="600s";       ingress_per_try_timeout="300s"
            ;;
        baseline|*)
            ;;
    esac

    log_step "Applying per-profile cluster config for profile: ${PERF_PROFILE}"
    log_info "  replicas (processor/listener/ocp/summary) = ${ros_processor_replicas}/${listener_replicas}/${ocp_worker_replicas}/${summary_worker_replicas}"
    log_info "  kruize replicas                           = 1 (always)"
    log_info "  kruize cpu req/lim                        = ${kruize_cpu_req}/${kruize_cpu_lim}"
    log_info "  ros-processor memory req/lim              = ${ros_mem_req}/${ros_mem_lim}"
    log_info "  listener memory req/lim                   = ${listener_mem_req}/${listener_mem_lim}"
    log_info "  ocp worker cpu req/lim                    = ${ocp_worker_cpu_req}/${ocp_worker_cpu_lim}"
    log_info "  ocp worker memory req/lim                = ${ocp_worker_mem_req}/${ocp_worker_mem_lim}"
    log_info "  summary worker cpu req/lim               = ${summary_worker_cpu_req}/${summary_worker_cpu_lim}"
    log_info "  ingress max upload size                   = ${max_upload_size}"
    log_info "  ingress max upload memory                 = ${max_upload_mem}"
    log_info "  application memory req/lim (ingress pod)  = ${app_mem_req}/${app_mem_lim}"
    log_info "  haproxy/envoy ingress timeout             = ${haproxy_timeout}"

    if [[ "${DRY_RUN}" == "true" ]]; then
        log_info "DRY RUN: Would scale: processor=${ros_processor_replicas} listener=${listener_replicas} ocp=${ocp_worker_replicas} summary=${summary_worker_replicas} kruize=1"
        log_info "DRY RUN: Resource limits (cpu/memory) and ingress timeouts are managed via the CMSC CR — patch spec before running."
        return 0
    fi

    # Phase 1 — STUB (operator path)
    # Resource limits, ingress upload sizes, and timeouts are driven by the
    # CostManagementServiceConfig CR, not by helm upgrade.  Patch the CMSC spec
    # before invoking this function if profile-specific resource overrides are
    # needed.  The suggested per-profile values are logged above for reference.
    log_info "Phase 1 skipped: resource/timeout configuration is owned by the CMSC CR (operator path)."
    log_info "  To apply profile overrides, patch spec.CostManagement.* in the CMSC before running tests."

    # Phase 2: oc scale — replica counts (idempotent)
    local scale_failed=false
    _scale_deploy() {
        local name="$1" replicas="$2" required="${3:-true}"

        if ! perf_deployment_exists "${name}"; then
            if [[ "${required}" == "true" ]]; then
                log_error "  deployment ${name} not found — cannot scale to ${replicas}"
                if [[ "${name}" == *"-ros-processor" || "${name}" == *"-kruize" ]]; then
                    log_error "  ROS workloads require spec.ros.enabled=true on CMSC ${namespace}/${release}"
                fi
                scale_failed=true
            else
                log_verbose "  skipping ${name} (not deployed)"
            fi
            return
        fi

        if perf_kubectl scale deployment "${name}" --replicas="${replicas}" -n "${namespace}" 2>/dev/null; then
            log_info "  scaled ${name} → ${replicas}"
        else
            log_error "  could not scale ${name}"
            scale_failed=true
        fi
    }

    _scale_deploy "${release}-koku-listener"          "${listener_replicas}"          true
    _scale_deploy "${release}-celery-worker-ocp"      "${ocp_worker_replicas}"        true
    _scale_deploy "${release}-celery-worker-summary"  "${summary_worker_replicas}"    true
    _scale_deploy "${release}-ros-processor"          "${ros_processor_replicas}"     "${ros_workloads_required}"
    _scale_deploy "${release}-kruize"                 "1"                             "${ros_workloads_required}"

    if [[ "${scale_failed}" == "true" ]]; then
        log_error "One or more deployments could not be scaled — verify cluster state before running tests"
        return 1
    fi

    log_info "Waiting for replica rollouts..."
    local rollout_ok=true
    _wait_rollout() {
        local deploy="$1" required="${2:-true}"
        if ! perf_deployment_exists "${deploy}"; then
            if [[ "${required}" == "true" ]]; then
                log_error "  rollout skipped — ${deploy} not found"
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
        log_success "All replica counts verified for ${PERF_PROFILE} profile"
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

    if command -v oc &>/dev/null; then
        ocp_version=$(oc get clusterversion version -o jsonpath='{.status.desired.version}' 2>/dev/null || echo "unknown")
        node_count=$(oc get nodes --no-headers 2>/dev/null | wc -l | tr -d ' ' || echo "0")

        if oc get storagecluster -n openshift-storage &>/dev/null; then
            storage_type="ODF"
        elif oc get storageclass s4-storage &>/dev/null; then
            storage_type="S4"
        fi
    fi

    local operator_version="unknown"
    if command -v oc &>/dev/null; then
        local _img
        _img=$(oc get deploy -n "${NAMESPACE}" \
            -l control-plane=controller-manager \
            -o jsonpath='{.items[0].spec.template.spec.containers[0].image}' 2>/dev/null || true)
        if [[ -n "${_img}" ]]; then
            operator_version="${_img##*:}"
        fi
    fi

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
