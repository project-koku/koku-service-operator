#!/usr/bin/env bash
# Helpers for hack/deploy-test-operator.sh. Sourced; do not execute.

dto_log_info() { echo -e "\033[0;34mℹ INFO:\033[0m $*"; }
dto_log_success() { echo -e "\033[0;32m✅ SUCCESS:\033[0m $*"; }
dto_log_warning() { echo -e "\033[1;33m⚠ WARNING:\033[0m $*" >&2; }
dto_log_error() { echo -e "\033[0;31m❌ ERROR:\033[0m $*" >&2; }
dto_log_step() { echo -e "\033[0;36m▶\033[0m $*"; }
dto_log_verbose() {
  if [[ "${VERBOSE:-false}" == "true" ]]; then
    echo -e "\033[0;36m[VERBOSE]\033[0m $*" >&2
  fi
}

dto_kubectl() {
  if command -v oc >/dev/null 2>&1; then
    oc "$@"
  else
    kubectl "$@"
  fi
}

# create / apply / patch must target the pinned context (KUBE_CONTEXT is required).
dto_kubectl_mutate() {
  dto_kubectl --context="${KUBE_CONTEXT}" "$@"
}

dto_print_plan() {
  cat <<EOF

Cost Management operator deploy + test
  namespace:     ${NAMESPACE}
  CR name:       ${CR_NAME}
  kafka NS:      ${KAFKA_NAMESPACE}
  keycloak NS:   ${KEYCLOAK_NAMESPACE}
  S4:            ${DEPLOY_S4} (ns=${S4_NAMESPACE})
  ODF S3 CA:     $([[ "${DEPLOY_S4}" == "true" ]] && echo "n/a (S4 HTTP)" || echo "${ODF_S3_CA_SECRET_NAME:-odf-s3-ca} (when openshift-storage present)")
  IMG:           ${IMG:-<skip>}
  tests only:    ${TESTS_ONLY}
  skip test:     ${SKIP_TEST}

EOF
}

dto_check_prerequisites() {
  dto_log_step "Checking prerequisites"
  if [[ "${DRY_RUN:-false}" == "true" ]]; then
    dto_log_info "DRY RUN: skipping tool checks"
    return 0
  fi
  local missing=()
  if ! command -v oc >/dev/null 2>&1 && ! command -v kubectl >/dev/null 2>&1; then
    missing+=("oc or kubectl")
  fi
  command -v yq >/dev/null 2>&1 || missing+=("yq")
  if [[ ${#missing[@]} -gt 0 ]]; then
    dto_log_error "missing required tools: ${missing[*]}"
    exit 1
  fi
  if [[ "${SKIP_OPERATOR:-false}" != "true" && "${TESTS_ONLY:-false}" != "true" ]]; then
    command -v openssl >/dev/null 2>&1 || {
      dto_log_error "openssl is required for deploy-incluster.sh webhook certs"
      exit 1
    }
  fi
  dto_log_success "prerequisites OK"
}

dto_check_oc_connection() {
  dto_log_step "Checking OpenShift connection"
  if [[ "${DRY_RUN:-false}" == "true" ]]; then
    dto_log_info "DRY RUN: would run oc whoami"
    return 0
  fi
  if ! dto_kubectl whoami >/dev/null 2>&1; then
    dto_log_error "not logged in — run oc login first"
    exit 1
  fi
  dto_log_success "connected as $(dto_kubectl whoami) ($(dto_kubectl whoami --show-server))"
}

dto_pin_kube_context() {
  if [[ -z "${KUBE_CONTEXT:-}" ]]; then
    dto_log_error "KUBE_CONTEXT is required (pin kubectl/oc to the target cluster)"
    exit 1
  fi
  if [[ "${DRY_RUN:-false}" == "true" ]]; then
    dto_log_verbose "DRY RUN: context pinned to ${KUBE_CONTEXT}"
    return 0
  fi
  local current
  current="$(dto_kubectl config current-context 2>/dev/null || true)"
  if [[ "$current" != "$KUBE_CONTEXT" ]]; then
    dto_log_error "current-context is '${current:-<unset>}', expected '${KUBE_CONTEXT}'"
    exit 1
  fi
  dto_log_verbose "context pinned: ${KUBE_CONTEXT}"
}

dto_execute_script() {
  local script_path="$1"
  shift
  if [[ ! -f "$script_path" ]]; then
    dto_log_error "script not found: ${script_path}"
    return 1
  fi
  chmod +x "$script_path" 2>/dev/null || true
  if [[ "${DRY_RUN:-false}" == "true" ]]; then
    dto_log_info "DRY RUN: would execute: ${script_path} $*"
    return 0
  fi
  dto_log_info "executing: ${script_path} $*"
  if [[ "${VERBOSE:-false}" == "true" ]]; then
    bash -x "$script_path" "$@"
  else
    "$script_path" "$@"
  fi
}

dto_create_app_namespace() {
  dto_log_step "Ensuring app namespace ${NAMESPACE}"
  if [[ "${DRY_RUN:-false}" == "true" ]]; then
    dto_log_info "DRY RUN: would create namespace ${NAMESPACE}"
    return 0
  fi
  if dto_kubectl get namespace "${NAMESPACE}" >/dev/null 2>&1; then
    dto_log_info "namespace ${NAMESPACE} already exists"
  else
    dto_kubectl_mutate create namespace "${NAMESPACE}"
    dto_log_success "created namespace ${NAMESPACE}"
  fi
  dto_kubectl label namespace "${NAMESPACE}" cost_management_optimizations=true --overwrite >/dev/null
}

dto_deploy_rhbk() {
  if [[ "${SKIP_RHBK:-false}" == "true" ]]; then
    dto_log_warning "skipping RHBK (--skip-rhbk)"
    return 0
  fi
  dto_log_step "Deploying Red Hat Build of Keycloak (1/5)"
  export RHBK_NAMESPACE="${KEYCLOAK_NAMESPACE}"
  dto_execute_script "${ROOT}/scripts/deploy-rhbk.sh"
  dto_log_success "RHBK deployment step finished"
}

dto_deploy_kafka() {
  if [[ "${SKIP_KAFKA:-false}" == "true" ]]; then
    dto_log_warning "skipping Kafka (--skip-kafka)"
    return 0
  fi
  dto_log_step "Deploying AMQ Streams / Kafka (2/5)"
  export KAFKA_NAMESPACE
  export STORAGE_CLASS
  dto_execute_script "${ROOT}/scripts/deploy-kafka.sh"
  dto_log_success "Kafka deployment step finished"
}

dto_copy_s4_storage_credentials() {
  local source_ns="$1"
  local target_ns="$2"
  local storage_secret="cost-onprem-storage-credentials"
  local source_secret="" candidate

  for candidate in s4-credentials "${storage_secret}"; do
    if dto_kubectl get secret "${candidate}" -n "${source_ns}" >/dev/null 2>&1; then
      source_secret="${candidate}"
      break
    fi
  done
  if [[ -z "$source_secret" ]]; then
    dto_log_error "no S4 secret in ${source_ns} (expected s4-credentials)"
    return 1
  fi

  local access_key secret_key
  access_key="$(dto_kubectl get secret "${source_secret}" -n "${source_ns}" -o jsonpath='{.data.access-key}' | base64 -d)"
  secret_key="$(dto_kubectl get secret "${source_secret}" -n "${source_ns}" -o jsonpath='{.data.secret-key}' | base64 -d)"
  if [[ -z "$access_key" || -z "$secret_key" ]]; then
    dto_log_error "secret ${source_ns}/${source_secret} missing access-key or secret-key"
    return 1
  fi

  dto_kubectl_mutate create secret generic "${storage_secret}" \
    --namespace="${target_ns}" \
    --from-literal=access-key="${access_key}" \
    --from-literal=secret-key="${secret_key}" \
    --dry-run=client -o yaml | dto_kubectl_mutate apply -f -
  dto_log_success "synced ${source_ns}/${source_secret} → ${target_ns}/${storage_secret}"
}

dto_deploy_s4() {
  if [[ "${DEPLOY_S4:-false}" != "true" ]]; then
    dto_log_verbose "skipping S4 (pass --deploy-s4 to enable)"
    return 0
  fi
  dto_log_step "Deploying S4 storage stand-in (3/5)"
  export STORAGE_CLASS
  dto_execute_script "${ROOT}/scripts/deploy-s4-test.sh" "${S4_NAMESPACE}"
  if [[ "${DRY_RUN:-false}" == "true" ]]; then
    dto_log_info "DRY RUN: would sync S4 credentials ${S4_NAMESPACE} → ${NAMESPACE}"
    return 0
  fi
  dto_copy_s4_storage_credentials "${S4_NAMESPACE}" "${NAMESPACE}"
  dto_log_success "S4 deployment step finished"
}

dto_deploy_operator() {
  if [[ "${SKIP_OPERATOR:-false}" == "true" ]]; then
    dto_log_warning "skipping operator (--skip-operator)"
    return 0
  fi
  dto_log_step "Deploying operator in-cluster (4/5)"
  if [[ "${DRY_RUN:-false}" == "true" ]]; then
    dto_log_info "DRY RUN: would run IMG=${IMG} ${ROOT}/hack/deploy-incluster.sh ${NAMESPACE}"
    return 0
  fi
  IMG="${IMG}" "${ROOT}/hack/deploy-incluster.sh" "${NAMESPACE}"
  dto_kubectl -n "${NAMESPACE}" rollout status deploy/koku-service-operator --timeout=180s
  dto_log_success "operator deployment step finished"
}

# Defaults for ODF/NooBaa TLS (operator ListBuckets probe; see docs/gap_analysis/COST-7684.md).
ODF_S3_CA_SECRET_NAME="${ODF_S3_CA_SECRET_NAME:-odf-s3-ca}"
OPENSHIFT_SERVICE_CA_CONFIGMAP="${OPENSHIFT_SERVICE_CA_CONFIGMAP:-openshift-service-ca.crt}"
OPENSHIFT_SERVICE_CA_NAMESPACE="${OPENSHIFT_SERVICE_CA_NAMESPACE:-openshift-config-managed}"

dto_fetch_openshift_service_ca_pem() {
  local pem=""
  pem="$(dto_kubectl get configmap "${OPENSHIFT_SERVICE_CA_CONFIGMAP}" -n "${OPENSHIFT_SERVICE_CA_NAMESPACE}" \
    -o jsonpath='{.data.service-ca\.crt}' 2>/dev/null || true)"
  if [[ -z "$pem" ]]; then
    pem="$(dto_kubectl get configmap service-ca-bundle -n openshift-config \
      -o jsonpath='{.data.service-ca\.crt}' 2>/dev/null || true)"
  fi
  if [[ -z "$pem" ]]; then
    return 1
  fi
  if [[ "$pem" != *"BEGIN CERTIFICATE"* ]]; then
    dto_log_error "cluster service CA data is not valid PEM (expected BEGIN CERTIFICATE)"
    return 1
  fi
  printf '%s' "$pem"
}

# ODF/NooBaa S3 uses certs signed by the OpenShift service CA. App pods trust it via
# AWS_CA_BUNDLE; the operator probe needs spec.objectStorage.caCertSecretName (key ca.crt).
# See docs/install/prerequisites.md and docs/install/production.md (prefer CA over insecureSkipVerify).
dto_ensure_odf_s3_ca_secret() {
  if [[ "${DEPLOY_S4:-false}" == "true" ]]; then
    return 0
  fi
  if [[ "${DRY_RUN:-false}" == "true" ]]; then
    dto_log_info "DRY RUN: would ensure Secret ${NAMESPACE}/${ODF_S3_CA_SECRET_NAME} (OpenShift service CA for objectStorage TLS)"
    return 0
  fi
  if ! dto_kubectl get namespace openshift-storage >/dev/null 2>&1; then
    dto_log_warning "namespace openshift-storage not found; assuming BYOI or external S3 (skipping ODF service-CA Secret)"
    ODF_S3_CA_SECRET_NAME=""
    return 0
  fi
  local pem
  if ! pem="$(dto_fetch_openshift_service_ca_pem)"; then
    dto_log_warning "OpenShift service CA ConfigMap not found; StorageReady ListBuckets may fail on ODF HTTPS"
    dto_log_warning "Create a Secret with key ca.crt and set spec.objectStorage.caCertSecretName, or use --deploy-s4"
    ODF_S3_CA_SECRET_NAME=""
    return 0
  fi
  dto_log_info "Ensuring Secret ${NAMESPACE}/${ODF_S3_CA_SECRET_NAME} from cluster service CA (objectStorage TLS)"
  printf '%s' "$pem" | dto_kubectl_mutate -n "${NAMESPACE}" create secret generic "${ODF_S3_CA_SECRET_NAME}" \
    --from-file=ca.crt=/dev/stdin \
    --dry-run=client -o yaml | dto_kubectl_mutate apply -f -
}

dto_apply_cmsc() {
  if [[ "${SKIP_CMSC:-false}" == "true" ]]; then
    dto_log_warning "skipping CMSC (--skip-cmsc)"
    return 0
  fi
  dto_log_step "Applying CostManagementServiceConfig (5/5)"

  if [[ "${DRY_RUN:-false}" == "true" ]]; then
    dto_log_info "DRY RUN: would apply ${CMSC_SAMPLE} and patch lab endpoints"
    dto_ensure_odf_s3_ca_secret
    return 0
  fi

  local domain keycloak_host keycloak_url s4_endpoint patch_file
  domain="$(dto_kubectl get ingresses.config cluster -o jsonpath='{.spec.domain}' 2>/dev/null || true)"
  if [[ -z "$domain" ]]; then
    dto_log_warning "cluster ingress domain unset; Routes may not resolve externally"
  fi

  keycloak_host="$(dto_kubectl get route keycloak -n "${KEYCLOAK_NAMESPACE}" -o jsonpath='{.spec.host}' 2>/dev/null || true)"
  if [[ -z "$keycloak_host" ]]; then
    dto_log_error "Keycloak route not found in ${KEYCLOAK_NAMESPACE} (deploy RHBK first or pass --skip-cmsc)"
    exit 1
  fi
  keycloak_url="https://${keycloak_host}"
  s4_endpoint="s4.${S4_NAMESPACE}.svc.cluster.local"

  yq e ".metadata.namespace = \"${NAMESPACE}\" | .metadata.name = \"${CR_NAME}\"" "${CMSC_SAMPLE}" \
    | dto_kubectl_mutate apply -f -

  dto_ensure_odf_s3_ca_secret

  patch_file="$(mktemp)"
  trap 'rm -f "${patch_file:-}"' RETURN
  python3 - "$patch_file" "$domain" "$s4_endpoint" "$keycloak_url" "${KEYCLOAK_NAMESPACE}" "${DEPLOY_S4:-false}" "${ODF_S3_CA_SECRET_NAME:-}" <<'PY'
import json, sys
path, domain, s4_endpoint, keycloak_url, keycloak_ns, deploy_s4, odf_ca_secret = sys.argv[1:8]
spec = {
    "auth": {
        "keycloak": {
            "url": f"http://keycloak-service.{keycloak_ns}.svc:8080",
            "issuerURL": keycloak_url,
        }
    },
}
if domain:
    spec["global"] = {"clusterDomain": domain}
if deploy_s4 == "true":
    spec["objectStorage"] = {
        "endpoint": s4_endpoint,
        "port": 7480,
        "useSSL": False,
        "secretName": "cost-onprem-storage-credentials",
        "s3": {"region": "us-east-1"},
    }
elif odf_ca_secret:
    # Merge patch: keep sample endpoint (ODF) and add CA for operator StorageReady probe.
    spec["objectStorage"] = {"caCertSecretName": odf_ca_secret}
open(path, "w").write(json.dumps({"spec": spec}))
PY
  dto_kubectl_mutate patch cmsc "${CR_NAME}" -n "${NAMESPACE}" --type merge --patch-file "${patch_file}"
  if [[ "${DEPLOY_S4:-false}" == "true" ]]; then
    dto_log_success "CMSC ${NAMESPACE}/${CR_NAME} applied and patched for lab (S4 + Keycloak)"
  elif [[ -n "${ODF_S3_CA_SECRET_NAME:-}" ]]; then
    dto_log_success "CMSC ${NAMESPACE}/${CR_NAME} applied and patched for lab (Keycloak + ODF service CA; objectStorage endpoint from sample)"
  else
    dto_log_success "CMSC ${NAMESPACE}/${CR_NAME} applied and patched for lab (Keycloak; objectStorage from sample / ODF discovery)"
  fi
}

dto_wait_cmsc_day_one() {
  if [[ "${SKIP_CMSC:-false}" == "true" ]]; then
    return 0
  fi
  if [[ "${DRY_RUN:-false}" == "true" ]]; then
    dto_log_info "DRY RUN: would wait for SchemaUpToDate=True and Available=True"
    return 0
  fi

  dto_log_step "Waiting for CMSC day-one success (${CMSC_READY_TIMEOUT})"
  dto_log_info "Expect SchemaUpToDate=True and Available=True (Phase may stay Progressing without UI OAuth)"

  local deadline schema avail
  deadline=$((SECONDS + $(dto_parse_duration_seconds "${CMSC_READY_TIMEOUT}")))
  while (( SECONDS < deadline )); do
    schema="$(dto_kubectl get cmsc "${CR_NAME}" -n "${NAMESPACE}" \
      -o jsonpath='{.status.conditions[?(@.type=="SchemaUpToDate")].status}' 2>/dev/null || true)"
    avail="$(dto_kubectl get cmsc "${CR_NAME}" -n "${NAMESPACE}" \
      -o jsonpath='{.status.conditions[?(@.type=="Available")].status}' 2>/dev/null || true)"
    if [[ "$schema" == "True" && "$avail" == "True" ]]; then
      dto_log_success "CMSC day-one conditions met"
      dto_kubectl -n "${NAMESPACE}" get cmsc "${CR_NAME}" \
        -o jsonpath='{range .status.conditions[*]}{.type}={.status} ({.reason}){"\n"}{end}' 2>/dev/null || true
      return 0
    fi
    sleep 15
  done

  dto_log_error "CMSC did not reach day-one success within ${CMSC_READY_TIMEOUT}"
  dto_kubectl -n "${NAMESPACE}" get cmsc "${CR_NAME}" \
    -o jsonpath='{range .status.conditions[*]}{.type}={.status} ({.reason}): {.message}{"\n"}{end}' 2>/dev/null || true
  exit 1
}

dto_parse_duration_seconds() {
  local spec="$1"
  if [[ "$spec" =~ ^([0-9]+)m$ ]]; then
    echo $(( "${BASH_REMATCH[1]}" * 60 ))
  elif [[ "$spec" =~ ^([0-9]+)s$ ]]; then
    echo "${BASH_REMATCH[1]}"
  else
    dto_log_warning "unrecognized duration '${spec}', defaulting to 2700s (45m); supported: Nm, Ns"
    echo 2700
  fi
}

dto_run_pytest() {
  dto_log_step "Running pytest suite"
  export NAMESPACE HELM_RELEASE_NAME KEYCLOAK_NAMESPACE
  if [[ "${VERBOSE:-false}" == "true" ]]; then
    export VERBOSE=true
  fi

  local pytest_script="${ROOT}/scripts/run-pytest.sh"
  if [[ ! -f "$pytest_script" ]]; then
    dto_log_error "pytest runner not found: ${pytest_script}"
    exit 1
  fi
  chmod +x "$pytest_script"

  local -a pytest_args=()
  if [[ "${VERBOSE:-false}" == "true" ]]; then
    pytest_args+=("-v")
  fi
  if [[ "${NO_UI:-false}" == "true" ]]; then
    pytest_args+=("--no-ui")
  fi

  if [[ "${DRY_RUN:-false}" == "true" ]]; then
    dto_log_info "DRY RUN: would execute: ${pytest_script} ${pytest_args[*]:-}"
    return 0
  fi

  # Homebrew Python often sets REQUESTS_CA_BUNDLE; breaks in-cluster TLS in pytest.
  unset REQUESTS_CA_BUNDLE SSL_CERT_FILE

  if ! "${pytest_script}" ${pytest_args[@]+"${pytest_args[@]}"}; then
    dto_log_error "pytest failed — see test/pytest/reports/"
    exit 1
  fi
  dto_log_success "pytest completed"
}
