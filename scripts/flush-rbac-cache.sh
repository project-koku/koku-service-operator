#!/usr/bin/env bash
#
# Flush insights-rbac Django cache and Koku RBAC response cache (Valkey).
#
# Use after RBAC permission changes when immediate effect is required.
# For most cases the 300s TTL is enough without manual intervention.
#
# Usage:
#   ./scripts/flush-rbac-cache.sh <namespace> [options]
#
# Examples:
#   ./scripts/flush-rbac-cache.sh cost-onprem
#   ./scripts/flush-rbac-cache.sh cost-onprem --instance cost-onprem
#   NAMESPACE=cost-onprem ./scripts/flush-rbac-cache.sh
#   ./scripts/flush-rbac-cache.sh cost-onprem --django-only
#
set -euo pipefail

SCRIPT_NAME="$(basename "$0")"
NAMESPACE="${NAMESPACE:-}"
INSTANCE="${INSTANCE:-}"
DJANGO_ONLY=false
VALKEY_ONLY=false
DRY_RUN=false

RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
NC='\033[0m'

log_info() { echo -e "${BLUE}[${SCRIPT_NAME}]${NC} $*"; }
log_success() { echo -e "${GREEN}[${SCRIPT_NAME}]${NC} $*"; }
log_warn() { echo -e "${YELLOW}[${SCRIPT_NAME}]${NC} $*"; }
log_error() { echo -e "${RED}[${SCRIPT_NAME}]${NC} $*" >&2; }

usage() {
    cat <<EOF
Usage: ${SCRIPT_NAME} [namespace] [options]

Flush insights-rbac Django cache and Koku RBAC response cache in Valkey.

Arguments:
  namespace             Target namespace (or set NAMESPACE)

Options:
  --instance <name>     Filter pods by app.kubernetes.io/instance (operator CMSC name)
  --django-only         Flush only the insights-rbac Django cache
  --valkey-only         Flush only the Valkey cache
  --dry-run             Print actions without executing them
  -h, --help            Show this help

Environment:
  NAMESPACE             Default namespace when omitted on the command line
  KUBECLI               kubectl or oc binary (default: auto-detect oc, then kubectl)

Notes:
  - Bundled Valkey is flushed via valkey-cli FLUSHALL in-cluster (dev/CI).
  - External cache (BYOI) has no local pod; the script prints manual instructions.
  - FLUSHALL clears all Valkey keys, including Celery metadata.
EOF
}

detect_kubecli() {
    if [[ -n "${KUBECLI:-}" ]]; then
        echo "$KUBECLI"
        return
    fi
    if command -v oc >/dev/null 2>&1; then
        echo "oc"
        return
    fi
    if command -v kubectl >/dev/null 2>&1; then
        echo "kubectl"
        return
    fi
    log_error "Neither oc nor kubectl found in PATH (set KUBECLI to override)"
    exit 1
}

build_label_selector() {
    local component="$1"
    local selector="app.kubernetes.io/component=${component}"
    if [[ -n "$INSTANCE" ]]; then
        selector="${selector},app.kubernetes.io/instance=${INSTANCE}"
    fi
    printf '%s' "$selector"
}

get_pod_by_component() {
    local kubecli="$1"
    local component="$2"
    local selector
    selector="$(build_label_selector "$component")"
    "$kubecli" get pods -n "$NAMESPACE" -l "$selector" \
        -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || true
}

find_valkey_pod() {
    local kubecli="$1"
    local pod

    for component in cache valkey; do
        pod="$(get_pod_by_component "$kubecli" "$component")"
        if [[ -n "$pod" ]]; then
            printf '%s' "$pod"
            return 0
        fi
    done
    return 1
}

flush_django_cache() {
    local kubecli="$1"
    local rbac_pod
    local manage_py_cmd=(
        python manage.py shell -c
        "from django.core.cache import cache; cache.clear(); print('RBAC cache cleared')"
    )
    local fallback_cmd=(
        python /opt/rbac/rbac/manage.py shell -c
        "from django.core.cache import cache; cache.clear(); print('RBAC cache cleared')"
    )

    rbac_pod="$(get_pod_by_component "$kubecli" "rbac-api")"
    if [[ -z "$rbac_pod" ]]; then
        log_error "No rbac-api pod found in namespace '${NAMESPACE}'"
        if [[ -n "$INSTANCE" ]]; then
            log_error "Check --instance '${INSTANCE}' or omit it to match any instance"
        fi
        return 1
    fi

    log_info "Flushing insights-rbac Django cache in pod ${rbac_pod}..."
    if [[ "$DRY_RUN" == "true" ]]; then
        log_info "[dry-run] ${kubecli} exec -n ${NAMESPACE} ${rbac_pod} -- ${manage_py_cmd[*]}"
        return 0
    fi

    if "$kubecli" exec -n "$NAMESPACE" "$rbac_pod" -- "${manage_py_cmd[@]}"; then
        log_success "insights-rbac Django cache cleared"
        return 0
    fi

    log_warn "manage.py not found on default PATH; retrying with /opt/rbac/rbac/manage.py"
    if "$kubecli" exec -n "$NAMESPACE" "$rbac_pod" -- "${fallback_cmd[@]}"; then
        log_success "insights-rbac Django cache cleared"
        return 0
    fi

    log_error "Failed to clear insights-rbac Django cache"
    return 1
}

flush_valkey_cache() {
    local kubecli="$1"
    local valkey_pod

    if ! valkey_pod="$(find_valkey_pod "$kubecli")"; then
        log_warn "No bundled Valkey pod found (labels: component=cache or component=valkey)"
        log_warn "If you use external cache (BYOI), flush manually against your Redis/Valkey endpoint:"
        echo "  valkey-cli -h <host> -p <port> FLUSHALL"
        echo "  # or: redis-cli -h <host> -p <port> FLUSHALL"
        echo "See docs/operations/rbac-cache.md for details."
        return 1
    fi

    log_info "Flushing Valkey cache in pod ${valkey_pod} (FLUSHALL)..."
    if [[ "$DRY_RUN" == "true" ]]; then
        log_info "[dry-run] ${kubecli} exec -n ${NAMESPACE} ${valkey_pod} -- valkey-cli FLUSHALL"
        return 0
    fi

    if "$kubecli" exec -n "$NAMESPACE" "$valkey_pod" -- valkey-cli FLUSHALL; then
        log_success "Valkey cache flushed"
        return 0
    fi

    log_warn "valkey-cli failed; retrying with redis-cli FLUSHALL"
    if "$kubecli" exec -n "$NAMESPACE" "$valkey_pod" -- redis-cli FLUSHALL; then
        log_success "Valkey cache flushed (redis-cli)"
        return 0
    fi

    log_error "Failed to flush Valkey cache"
    return 1
}

parse_args() {
    while [[ $# -gt 0 ]]; do
        case "$1" in
            -h|--help)
                usage
                exit 0
                ;;
            --instance)
                INSTANCE="${2:?--instance requires a value}"
                shift 2
                ;;
            --django-only)
                DJANGO_ONLY=true
                shift
                ;;
            --valkey-only)
                VALKEY_ONLY=true
                shift
                ;;
            --dry-run)
                DRY_RUN=true
                shift
                ;;
            --)
                shift
                break
                ;;
            -*)
                log_error "Unknown option: $1"
                usage >&2
                exit 1
                ;;
            *)
                if [[ -z "$NAMESPACE" ]]; then
                    NAMESPACE="$1"
                else
                    log_error "Unexpected argument: $1"
                    usage >&2
                    exit 1
                fi
                shift
                ;;
        esac
    done
}

main() {
    parse_args "$@"

    if [[ -z "$NAMESPACE" ]]; then
        log_error "Namespace is required (argument or NAMESPACE env)"
        usage >&2
        exit 1
    fi

    if [[ "$DJANGO_ONLY" == "true" && "$VALKEY_ONLY" == "true" ]]; then
        log_error "Use only one of --django-only or --valkey-only"
        exit 1
    fi

    local kubecli
    kubecli="$(detect_kubecli)"

    log_info "Namespace: ${NAMESPACE}"
    if [[ -n "$INSTANCE" ]]; then
        log_info "Instance filter: ${INSTANCE}"
    fi
    if [[ "$DRY_RUN" == "true" ]]; then
        log_info "Dry run enabled"
    fi

    local django_status=0
    local valkey_status=0

    if [[ "$VALKEY_ONLY" != "true" ]]; then
        flush_django_cache "$kubecli" || django_status=$?
    fi

    if [[ "$DJANGO_ONLY" != "true" ]]; then
        flush_valkey_cache "$kubecli" || valkey_status=$?
    fi

    if [[ "$django_status" -ne 0 || "$valkey_status" -ne 0 ]]; then
        if [[ "$VALKEY_ONLY" == "true" ]]; then
            exit "$valkey_status"
        fi
        if [[ "$DJANGO_ONLY" == "true" ]]; then
            exit "$django_status"
        fi
        # Django flush is required; Valkey missing is a warning for BYOI.
        if [[ "$django_status" -ne 0 ]]; then
            exit "$django_status"
        fi
        if [[ "$valkey_status" -ne 0 ]]; then
            log_warn "Django cache cleared; Valkey flush skipped or failed (see above)"
            exit 0
        fi
    fi

    log_success "RBAC cache flush complete — permission changes are active immediately"
}

main "$@"
