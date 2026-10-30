#!/usr/bin/env bash
# Temporary compatibility entrypoint for openshift/release Prow jobs.
#
# The release repository still invokes ./hack/ci/e2e.sh. Keep this wrapper
# until both e2e-pytest and e2e-iqe stack steps switch to scripts/ci/e2e.sh.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
exec "${ROOT}/scripts/ci/e2e.sh" "$@"
