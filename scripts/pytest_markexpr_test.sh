#!/usr/bin/env bash
# Unit tests for scripts/lib/pytest_markexpr.sh (no cluster required).
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
# shellcheck disable=SC1091
source "${ROOT}/scripts/lib/pytest_markexpr.sh"

fail=0
assert_eq() {
  local got="$1" want="$2" msg="$3"
  if [[ "$got" != "$want" ]]; then
    echo "FAIL: ${msg}" >&2
    echo "  got:  ${got}" >&2
    echo "  want: ${want}" >&2
    fail=1
  fi
}

# Prow / laptop: --no-ui -m smoke ANDs smoke with UI/perf exclusions.
# ROS is runtime-gated (ros.enabled), not dropped from -m.
# A second -m must not replace the exclude expression.
got="$(compose_pytest_markexpr --no-ui --extra-m smoke)"
assert_eq "$got" \
  "(smoke) and not ui and not performance and not helm" \
  "--no-ui -m smoke (Prow smoke step)"

got="$(compose_pytest_markexpr --no-ui --positive smoke)"
assert_eq "$got" \
  "(smoke) and not ui and not performance and not helm" \
  "--no-ui --smoke"

got="$(compose_pytest_markexpr --no-ui)"
assert_eq "$got" \
  "not ui and not performance and not helm" \
  "--no-ui full suite (ROS tests collect; skip if ros.enabled=false)"

got="$(compose_pytest_markexpr --no-ui --positive ros)"
assert_eq "$got" \
  "(ros) and not ui and not performance and not helm" \
  "--no-ui --ros keeps ROS tests"

got="$(compose_pytest_markexpr --positive helm)"
assert_eq "$got" \
  "(helm) and not performance" \
  "--helm keeps helm tests"

got="$(compose_pytest_markexpr)"
assert_eq "$got" \
  "not performance and not helm" \
  "default excludes performance and helm"

# ROS marker must stay selectable so enabled vs disabled is a CR concern.
case "$(compose_pytest_markexpr --no-ui --extra-m smoke)" in
  *"not ros"*)
    echo "FAIL: CI smoke expression must not deselect ROS tests" >&2
    fail=1
    ;;
esac

if [[ "$fail" -ne 0 ]]; then
  echo "pytest_markexpr_test: FAILED" >&2
  exit 1
fi
echo "pytest_markexpr_test: ok"
