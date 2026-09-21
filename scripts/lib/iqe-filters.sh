#!/bin/bash
# =============================================================================
# IQE Test Filter Configuration
# =============================================================================
# Shared filter definitions for IQE cost-management tests.
# Source this file from run-iqe-tests.sh and run-iqe-tests-local.sh.
#
# Tests are grouped by skip reason. Each group can be toggled independently.
# Set SKIP_*=false to include those tests in the run.
# Skip groups are documented in this file. Prow smoke profile:
# docs/openshift-ci/openshift-ci-jobs.md
#
# Profiles (use --profile flag):
#   smoke     - Source + cost model tests (~71 selected, ~17 min) - FOR PR CHECKS
#   extended  - All except infra tests (~2100 tests, ~33 min) - DAILY CI
#   stable    - All validated tests (~2350 tests, ~40 min) - WEEKLY CI
#   full      - All cost_ocp_on_prem tests (~3324 tests, 2-3 hours) - RELEASE VALIDATION
# =============================================================================

# Prevent multiple sourcing
if [[ -n "${_IQE_FILTERS_SOURCED:-}" ]]; then
    return 0
fi
_IQE_FILTERS_SOURCED=1

# =============================================================================
# Profile Selection
# =============================================================================

# Profile selection (set via --profile flag, overrides individual SKIP_* vars)
# Empty means use individual SKIP_* settings (defaults to stable-like behavior)
TEST_PROFILE="${TEST_PROFILE:-}"

# Apply profile settings (called after arg parsing)
apply_profile() {
    case "${TEST_PROFILE}" in
        smoke)
            # Quick validation (~71 selected parameterized cases, ~17 min)
            # Uses positive -k filter to select source + cost model tests
            SMOKE_FILTER="test_api_ocp_source or test_api_cost_model_ocp"
            # Skip all optional groups for fastest run
            SKIP_INFRA_TESTS=true
            SKIP_SLOW_TESTS=true
            SKIP_DELTA_TESTS=true
            SKIP_FLAKY_TESTS=true
            ;;
        extended)
            # Daily CI (~2100 tests, ~33 min)
            # All tests except blocked groups and infrastructure
            # No positive filter - runs broader set than smoke
            SKIP_INFRA_TESTS=true
            SKIP_SLOW_TESTS=false
            SKIP_DELTA_TESTS=false
            SKIP_FLAKY_TESTS=false
            ;;
        stable)
            # Weekly CI (~2350 tests, ~40 min)
            # All validated groups including infrastructure
            # No positive filter - runs all tests except blocked groups
            SKIP_INFRA_TESTS=false
            SKIP_SLOW_TESTS=false
            SKIP_DELTA_TESTS=false
            SKIP_FLAKY_TESTS=false
            ;;
        full)
            # Release validation (~2350 tests, ~55 min)
            # All cost_ocp_on_prem tests except 90-day date range tests (~120 tests)
            # 90-day tests require RETAIN_NUM_MONTHS=4 in chart (FLPATH-4131, COST-7253)
            SKIP_DATE_RANGE_TESTS=true
            SKIP_INFRA_TESTS=false
            SKIP_SLOW_TESTS=false
            SKIP_DELTA_TESTS=false
            SKIP_FLAKY_TESTS=false
            ;;
        *)
            # Default: same as stable (all validated groups)
            SKIP_INFRA_TESTS=false
            SKIP_SLOW_TESTS=false
            SKIP_DELTA_TESTS=false
            SKIP_FLAKY_TESTS=false
            ;;
    esac
}

# =============================================================================
# Skip Group Definitions
# =============================================================================
# Each group has:
#   SKIP_*_TESTS - Boolean to enable/disable the skip (default: true for blocked, false for optional)
#   FILTER_* - Pytest -k expression to match tests in this group

# --- GPU/MIG Tests ---
SKIP_GPU_TESTS="${SKIP_GPU_TESTS:-false}"
FILTER_GPU="ai_workloads or mig_workloads or distro or test_api_ocp_gpu or test_api_gpu or test_api_cost_model_ocp_gpu or test_api_cost_model_ocp_cost_gpu or test_api_ocp_resource_types_gpu or test_api_ocp_mig"

# --- ROS Tests ---
# ROS S3 bucket and credentials are now configured via S3_ENDPOINT env vars.
# test_api_ocp_ros_kafka_content still skips (clowder_smoke only).
# 3 tests (2 runnable, 1 skips on on-prem)
SKIP_ROS_TESTS="${SKIP_ROS_TESTS:-true}"
FILTER_ROS="test_api_ocp_ros"

# --- Date Range Tests (Insufficient Historical Data) ---
# On-prem generates ~60 days of data; 90-day queries fail with 400 errors
# ~120 tests affected (hardcoded last-90-days params)
# Jira: COST-7253 (retention config), COST-573 (>90 days epic)
# Note: Use underscores in patterns - pytest -k treats hyphens as "and not"
SKIP_DATE_RANGE_TESTS="${SKIP_DATE_RANGE_TESTS:-true}"
FILTER_DATE_RANGE="(last and 90 and days) or random_date_range or random_daily_time_filter"

# --- Order By Tests (Backend Timeout - COST-7179 related) ---
# Fixtures timeout waiting for completed_datetime
# ~66 tests affected
SKIP_ORDER_BY_TESTS="${SKIP_ORDER_BY_TESTS:-true}"
FILTER_ORDER_BY="test_api_ocp_all_limit_order_by_cost or test_api_ocp_tagging_limit_order_by_cost or test_api_ocp_volume_order_by"

# --- Tag Validation Tests (Missing Tag Data) ---
# Volume tag data not present in generated NISE data
# ~6 tests affected
# Note: Use "and" between words - pytest -k treats hyphens as "and not"
SKIP_TAG_TESTS="${SKIP_TAG_TESTS:-true}"
FILTER_TAG="(volume and tag and exact_match)"

# --- Cost Distribution Tests (Backend Timeout - COST-7179 related) ---
# Fixtures timeout waiting for completed_datetime
# 5 tests affected
SKIP_COST_DISTRIBUTION_TESTS="${SKIP_COST_DISTRIBUTION_TESTS:-true}"
FILTER_COST_DISTRIBUTION="test_api_cost_model_ocp_cost_distribution"

# --- Source CRUD Update Test (Backend Bug) ---
# Backend PATCH endpoint returns 500 error
# 1 test affected
SKIP_SOURCE_CRUD_TESTS="${SKIP_SOURCE_CRUD_TESTS:-false}"
FILTER_SOURCE_CRUD="test_api_ocp_source_crud"

# --- Tag-Based Rates Update Test (COST-7179 related) ---
# Fixtures timeout waiting for completed_datetime after tag-based rate update
# 1 test affected
SKIP_TAG_RATES_TESTS="${SKIP_TAG_RATES_TESTS:-true}"
FILTER_TAG_RATES="test_api_cost_model_rates_update_to_tag_based"

# --- Unstable Tests (Timing/Data-Dependent Failures) ---
# Tests that fail intermittently due to timing, date calculations, or data dependencies
# Jira: FLPATH-2689
# Failures observed 2026-03-17 in stable profile run:
#   - date_range_end_negative: Expects API exception not raised (3 tests)
#   - virtual_machines_report_content: Calculation mismatch (1 test)
#   - volume_deltas_monthly: Delta calculation mismatch (3 tests)
#   - currency tests: IndexError - empty response (8 tests)
#   - forecast tests: Date offset off-by-one (5 tests)
# Failures observed 2026-06-02 in e2e PR run:
#   - price_list_intervals: assert 0.0 != 0 (cost data not yet processed)
#   - price_list_recalc_logic: recalculation mismatch (timing-dependent)
# ~22 tests affected
SKIP_UNSTABLE_TESTS="${SKIP_UNSTABLE_TESTS:-true}"
FILTER_UNSTABLE="test_api_ocp_network_endpoint_date_range_end_negative or test_api_ocp_volume_endpoint_date_range_end_negative or test_api_ocp_tagging_endpoint_date_range_end_negative or test_api_ocp_virtual_machines_report_content or test_api_ocp_volume_deltas_monthly or test_api_ocp_currency_report_param or test_api_ocp_currency_compute or test_api_ocp_currency_memory or test_api_ocp_currency_volume or test_api_ocp_forecast_values or test_api_ocp_forecast_data_other_params or test_api_ocp_forecast_prediction_days or test_api_ocp_source_raw_price_list_intervals or test_api_ocp_source_raw_price_list_recalc_logic"

# --- Infrastructure/Config Tests (On-prem Incompatible) ---
# Tests that were expected to require cloud infrastructure but now pass
# Validated 2026-03-16: 256/258 passed (see skip-group-validation-plan.md Phase 8)
# Note: test_api_cost_model_rates_update_to_tag_based moved to SKIP_TAG_RATES_TESTS (COST-7179)
# ~257 tests affected (includes parameterized variants)
SKIP_INFRA_TESTS="${SKIP_INFRA_TESTS:-false}"
FILTER_INFRA="test_api_ocp_all_validate_items_date_range_monthly or test_api_ocp_ingest_source_static or test_api_ocp_ingest_source_eur or test_api_ocp_for_aws or test_api_ocp_cost_filtered_top_projects or test_api_ocp_all_bucketing or test_api_ocp_coros_distribution_negative_filtering"

# --- Long Running Tests (Performance) ---
# Tests that take >2 minutes each - slow but stable
# Validated 2026-03-17: 13/13 passed (see skip-group-validation-plan.md Phase 10)
# ~13 tests affected, ~20 minutes total
# Set SKIP_SLOW_TESTS=true for faster feedback loops
SKIP_SLOW_TESTS="${SKIP_SLOW_TESTS:-false}"
FILTER_SLOW="test_api_ocp_source_raw_node_cluster_capacity or test_api_source_cluster_info_sources or test_api_ocp_source_all_bucketing_platform_update or test_api_ocp_all_project_classification or test_api_ocp_daily_flow_ingest"

# --- Delta/Calculation Tests (Data Timing Issues) ---
# Tests that compare month-over-month deltas - previously expected to fail but now pass
# Validated 2026-03-16: 12/12 passed (see skip-group-validation-plan.md Phase 7)
# ~12 tests affected (includes parameterized variants)
SKIP_DELTA_TESTS="${SKIP_DELTA_TESTS:-false}"
FILTER_DELTA="deltas_monthly or test_api_ocp_coros_distribution_deltas"

# --- Flaky/Data-Dependent Tests ---
# Tests that were previously marked as flaky but now pass consistently
# Validated 2026-03-16: 54/54 passed (see skip-group-validation-plan.md Phase 6)
# NOTE: These tests overlap with SKIP_UNSTABLE_TESTS. When both are false (stable profile),
# the duplicate patterns are harmless. SKIP_FLAKY_TESTS is kept for backward compatibility
# but SKIP_UNSTABLE_TESTS should be used for new additions.
# ~54 tests affected (includes parameterized variants)
SKIP_FLAKY_TESTS="${SKIP_FLAKY_TESTS:-false}"
FILTER_FLAKY="test_api_ocp_resource_types_nodes_search or test_api_ocp_resource_types_clusters_search or test_api_ocp_resource_types_projects_search or test_api_ocp_tags_filtered_total_match_group_by_total"

# =============================================================================
# Filter Builder
# =============================================================================

# Build the combined filter from enabled skip groups
build_test_filter() {
    local filters=()
    
    if [[ "${SKIP_GPU_TESTS}" == "true" ]]; then
        filters+=("(${FILTER_GPU})")
    fi
    if [[ "${SKIP_ROS_TESTS}" == "true" ]]; then
        filters+=("(${FILTER_ROS})")
    fi
    if [[ "${SKIP_DATE_RANGE_TESTS}" == "true" ]]; then
        filters+=("(${FILTER_DATE_RANGE})")
    fi
    if [[ "${SKIP_ORDER_BY_TESTS}" == "true" ]]; then
        filters+=("(${FILTER_ORDER_BY})")
    fi
    if [[ "${SKIP_TAG_TESTS}" == "true" ]]; then
        filters+=("(${FILTER_TAG})")
    fi
    if [[ "${SKIP_COST_DISTRIBUTION_TESTS}" == "true" ]]; then
        filters+=("(${FILTER_COST_DISTRIBUTION})")
    fi
    if [[ "${SKIP_SOURCE_CRUD_TESTS}" == "true" ]]; then
        filters+=("(${FILTER_SOURCE_CRUD})")
    fi
    if [[ "${SKIP_TAG_RATES_TESTS}" == "true" ]]; then
        filters+=("(${FILTER_TAG_RATES})")
    fi
    if [[ "${SKIP_UNSTABLE_TESTS}" == "true" ]]; then
        filters+=("(${FILTER_UNSTABLE})")
    fi
    if [[ "${SKIP_INFRA_TESTS}" == "true" ]]; then
        filters+=("(${FILTER_INFRA})")
    fi
    if [[ "${SKIP_SLOW_TESTS}" == "true" ]]; then
        filters+=("(${FILTER_SLOW})")
    fi
    if [[ "${SKIP_DELTA_TESTS}" == "true" ]]; then
        filters+=("(${FILTER_DELTA})")
    fi
    if [[ "${SKIP_FLAKY_TESTS}" == "true" ]]; then
        filters+=("(${FILTER_FLAKY})")
    fi
    
    if [[ ${#filters[@]} -eq 0 ]]; then
        echo ""
        return
    fi
    
    # Join with " or " and wrap in "not (...)"
    local combined=""
    for i in "${!filters[@]}"; do
        if [[ $i -eq 0 ]]; then
            combined="${filters[$i]}"
        else
            combined="${combined} or ${filters[$i]}"
        fi
    done
    
    echo "not (${combined})"
}
