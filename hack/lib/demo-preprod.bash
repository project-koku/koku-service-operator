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
    -e 's|COPY --from=builder /workspace/manager /manager|COPY --from=builder /tmp/manager /manager|' \
    -e 's|COPY --from=builder /workspace/wait-for /wait-for|COPY --from=builder /tmp/wait-for /wait-for|' \
    "$file" >"$tmp"
  mv "$tmp" "$file"
}

# Fill the BYOI sample CR: apps domain, public Keycloak issuer, lab TLS skip-verify.
render_preprod_cr() {
  local src="${1:?sample CR path}"
  local dest="${2:?output path}"
  local domain="${3:?cluster apps domain}"
  local issuer="${4:?Keycloak issuer base URL}"
  python3 - "$src" "$dest" "$domain" "$issuer" <<'PY'
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
