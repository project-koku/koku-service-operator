#!/usr/bin/env bash
# Helpers for hack/demo-preprod.sh. Sourced; do not execute.
# OpenShift's UBI go-toolset image runs as a random UID. Writing the Go binary
# into WORKDIR /workspace fails with "open manager: permission denied".
patch_operator_dockerfile() {
  local file="${1:?Dockerfile path required}"
  # Portable in-place edit (macOS/Linux).
  local tmp
  tmp="$(mktemp)"
  sed \
    -e 's|go build -a -o manager cmd/main.go|go build -a -o /tmp/manager cmd/main.go|' \
    -e 's|go build -a -o wait-for ./cmd/wait-for/|go build -a -o /tmp/wait-for ./cmd/wait-for/|' \
    -e 's|COPY --from=builder /workspace/manager .|COPY --from=builder /tmp/manager .|' \
    -e 's|COPY --from=builder /workspace/wait-for .|COPY --from=builder /tmp/wait-for .|' \
    "$file" >"$tmp"
  mv "$tmp" "$file"
}

# Fill the BYOI sample CR: apps domain, public Keycloak issuer, lab TLS skip-verify.
#
# Optional workload image overrides (used by `demo-preprod.sh --crc` for arm64;
# unset on the amd64/clusterbot path, so the rendered CR is byte-identical there):
#   DEMO_KOKU_IMAGE / DEMO_KOKU_TAG              koku api + masu
#   DEMO_RBAC_IMAGE / DEMO_RBAC_TAG              insights-rbac
#   DEMO_INGRESS_IMAGE / DEMO_INGRESS_TAG        ingress
#   DEMO_UI_IMAGE / DEMO_UI_TAG                  koku-ui-onprem
#   DEMO_ENVOY_IMAGE / DEMO_ENVOY_TAG            envoy / proxyv2
#   DEMO_OAUTHPROXY_IMAGE / DEMO_OAUTHPROXY_TAG  oauth2-proxy sidecar
render_preprod_cr() {
  local src="${1:?sample CR path}"
  local dest="${2:?output path}"
  local domain="${3:?cluster apps domain}"
  local issuer="${4:?Keycloak issuer base URL}"
  python3 - "$src" "$dest" "$domain" "$issuer" <<'PY'
import os
import sys
from pathlib import Path

src, dest, domain, issuer = sys.argv[1:5]
text = Path(src).read_text()
text = text.replace('clusterDomain: "apps.cluster.example.com"', f'clusterDomain: "{domain}"')
old_url = '      url: "http://keycloak-service.keycloak.svc.cluster.local:8080"'
new_block = (
    old_url
    + f'\n      issuerURL: "{issuer}"'
    + "\n      tls:\n        insecureSkipVerify: true"
)
if old_url not in text:
    raise SystemExit("render_preprod_cr: keycloak url line not found in sample CR")
text = text.replace(old_url, new_block, 1)
# Drop the commented placeholders so the live values are the only ones.
text = text.replace('      # issuerURL: "https://keycloak-keycloak.apps.example.com"\n', "")
text = text.replace("      # tls:\n", "")
text = text.replace("      #   insecureSkipVerify: true   # dev only when issuer uses a private CA\n", "")

# Optional per-workload image overrides. Each needle is the verbatim line from
# config/samples/byoi/app/costmanagementserviceconfig.yaml; when the matching
# env var is empty the line is left untouched (amd64 path renders unchanged).
_overrides = [
    ("DEMO_KOKU_IMAGE",       "repository: quay.io/redhat-services-prod/cost-mgmt-dev-tenant/koku"),
    ("DEMO_KOKU_TAG",         'tag: "768be82"'),
    ("DEMO_RBAC_IMAGE",       "repository: quay.io/redhat-services-prod/hcc-accessmanagement-tenant/insights-rbac"),
    ("DEMO_RBAC_TAG",         'tag: "73870d8"'),
    ("DEMO_INGRESS_IMAGE",    "repository: quay.io/iop/ingress"),
    ("DEMO_INGRESS_TAG",      'tag: "master"'),
    ("DEMO_UI_IMAGE",         "repository: quay.io/insights-onprem/koku-ui-onprem"),
    ("DEMO_UI_TAG",           'tag: "2f23c646581028bd385856b6713e6bf367baf953"'),
    ("DEMO_ENVOY_IMAGE",      "repository: registry.redhat.io/openshift-service-mesh/proxyv2-rhel9"),
    ("DEMO_ENVOY_TAG",        'tag: "2.6"'),
    ("DEMO_OAUTHPROXY_IMAGE", "repository: registry.redhat.io/rhceph/oauth2-proxy-rhel9"),
    ("DEMO_OAUTHPROXY_TAG",   'tag: "v7.6.0"'),
]
for env_key, needle in _overrides:
    val = os.environ.get(env_key, "").strip()
    if not val:
        continue
    if needle.startswith("repository: "):
        repl = "repository: " + val
    else:
        repl = 'tag: "' + val + '"'
    if needle not in text:
        raise SystemExit(f"render_preprod_cr: {env_key} set but sample line not found: {needle}")
    text = text.replace(needle, repl)

Path(dest).write_text(text)
PY
}

# cost-onprem-chart is a sibling of the main checkout, not of a git worktree dir.
# Paths are string-computed so this works in tests without the dirs existing.
default_chart_root() {
  local repo_root="${1:?}"
  local git_common_dir="${2:-}"
  local main_root parent
  if [[ -n "$git_common_dir" ]]; then
    main_root="$(dirname "$git_common_dir")"
  else
    main_root="$repo_root"
  fi
  parent="$(dirname "$main_root")"
  echo "${parent}/cost-onprem-chart"
}

# Fail with the real oc/kubectl error when the API is down (expired Cluster Bot).
require_reachable_cluster() {
  local kubectl="${1:?kubectl/oc binary}"
  local err
  err="$(mktemp)"
  if ! "$kubectl" whoami >/dev/null 2>"$err"; then
    echo "error: kube API is not reachable (context may point at an expired Cluster Bot)" >&2
    sed 's/^/  /' "$err" >&2
    rm -f "$err"
    return 1
  fi
  rm -f "$err"
}

# Print spec.domain from ingress.config.openshift.io/cluster. Do not swallow stderr.
read_apps_domain() {
  local kubectl="${1:?kubectl/oc binary}"
  local err out
  err="$(mktemp)"
  if ! out="$("$kubectl" get ingress.config.openshift.io cluster -o jsonpath='{.spec.domain}' 2>"$err")"; then
    echo "error: failed to get ingress.config.openshift.io/cluster (need a live OpenShift API)" >&2
    sed 's/^/  /' "$err" >&2
    rm -f "$err"
    return 1
  fi
  rm -f "$err"
  out="${out//[$'\t\r\n ']/}"
  if [[ -z "$out" ]]; then
    echo "error: ingress.config.openshift.io/cluster has an empty spec.domain" >&2
    return 1
  fi
  printf '%s\n' "$out"
}
