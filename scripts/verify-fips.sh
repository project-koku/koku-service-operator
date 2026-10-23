#!/usr/bin/env bash
# Exercise the crypto mode of both shipped binaries without a cluster.
set -euo pipefail

image=${1:?usage: verify-fips.sh IMAGE}
container_tool=${CONTAINER_TOOL:-docker}

for binary in /manager /wait-for; do
  for mode in auto on only; do
    # Leave GODEBUG unset for auto: this verifies the compiled-in default.
    env_args=(--env GOLANG_NATIVE_HOSTFIPS_OVERRIDE=1)
    if [[ $mode != auto ]]; then
      env_args+=(--env "GODEBUG=fips140=$mode")
    fi
    status=$("$container_tool" run --rm "${env_args[@]}" --entrypoint "$binary" "$image" --fips-status)
    python3 -c '
import json, sys
status = json.loads(sys.argv[1])
if status != {"enabled": True, "version": "v1.0.0"}:
    raise SystemExit(f"Unexpected native FIPS status: {status}")
' "$status"
    printf '%s: %s enabled with validated module v1.0.0\n' "$binary" "$mode"
  done

  # A runtime override must not disable FIPS on a FIPS host. This test override
  # exercises the toolchain's native host check; a real FIPS cluster is still
  # required for acceptance testing.
  if output=$("$container_tool" run --rm \
      --env GOLANG_NATIVE_HOSTFIPS_OVERRIDE=1 --env GODEBUG=fips140=off \
      --entrypoint "$binary" "$image" --fips-status 2>&1); then
    printf '%s unexpectedly allowed FIPS to be disabled\n' "$binary" >&2
    exit 1
  fi
  if [[ $output != *'Host FIPS mode is enabled, but the required GODEBUG=fips140 module is disabled'* ]]; then
    printf '%s failed without the expected strict FIPS diagnostic: %s\n' "$binary" "$output" >&2
    exit 1
  fi
  printf '%s: disabling FIPS on a simulated FIPS host is rejected\n' "$binary"
done
