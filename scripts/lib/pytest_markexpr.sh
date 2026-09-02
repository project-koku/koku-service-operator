# Compose a single pytest -m expression. Sourced by run-pytest.sh.
# Never emit two -m flags: pytest stores only the last one.
#
# CI (--no-ui / smoke): no Playwright. ROS tests are not dropped from -m;
# suites/ros skip at runtime when spec.ros.enabled is false.

compose_pytest_markexpr() {
  local no_ui=0 ui_only=0 helm=0 ros=0 perf=0
  local extra_m=""
  local positives=()

  while [[ $# -gt 0 ]]; do
    case "$1" in
      --no-ui) no_ui=1 ;;
      --ui-only) ui_only=1 ;;
      --helm) helm=1 ;;
      --ros) ros=1 ;;
      --perf) perf=1 ;;
      --positive)
        positives+=("$2")
        shift
        ;;
      --extra-m)
        extra_m="$2"
        shift
        ;;
      *)
        echo "compose_pytest_markexpr: unknown arg: $1" >&2
        return 2
        ;;
    esac
    shift
  done

  local p
  for p in "${positives[@]+"${positives[@]}"}"; do
    case "$p" in
      helm) helm=1 ;;
      ros) ros=1 ;;
      ui) ui_only=1 ;;
    esac
    if [[ "$p" == *performance* ]]; then
      perf=1
    fi
  done

  if [[ "$ui_only" -eq 1 ]]; then
    printf '%s\n' "ui"
    return 0
  fi

  local core=""
  for p in "${positives[@]+"${positives[@]}"}"; do
    if [[ -z "$core" ]]; then
      core="$p"
    else
      core="(${core}) or (${p})"
    fi
  done

  if [[ -n "$extra_m" ]]; then
    if [[ -z "$core" ]]; then
      core="$extra_m"
    else
      core="(${core}) and (${extra_m})"
    fi
  fi

  if [[ -n "$core" ]]; then
    core="(${core})"
  fi

  if [[ "$perf" -eq 1 ]]; then
    printf '%s\n' "${core:-performance}"
    return 0
  fi

  local wants_smoke=0
  if [[ "$core" == *smoke* ]]; then
    wants_smoke=1
  fi

  local parts=()
  if [[ -n "$core" ]]; then
    parts+=("$core")
  fi

  if [[ "$no_ui" -eq 1 || "$wants_smoke" -eq 1 ]]; then
    parts+=("not ui")
    parts+=("not performance")
    if [[ "$helm" -eq 0 ]]; then
      parts+=("not helm")
    fi
    # Do not add "not ros": require_ros_enabled skips when
    # spec.ros.enabled=false. Same -m still collects ROS tests when enabled.
  else
    parts+=("not performance")
    if [[ "$helm" -eq 0 ]]; then
      parts+=("not helm")
    fi
  fi

  local out=""
  local part
  for part in "${parts[@]}"; do
    if [[ -z "$out" ]]; then
      out="$part"
    else
      out="${out} and ${part}"
    fi
  done
  printf '%s\n' "$out"
}
