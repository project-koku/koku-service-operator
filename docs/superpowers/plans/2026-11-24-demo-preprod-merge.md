# demo-preprod Merge Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Merge `demo-preprod.sh` and `demo-preprod-olm.sh` into one customer-facing demo/evaluation installer with selectable operator install source (`registry|local|manual`), version discovery, architecture checks, demo hardening, and extended reset.

**Architecture:** One orchestrator script (`scripts/demo-preprod.sh`) keeps the existing step pipeline; all new logic that must be testable offline goes into `scripts/lib/demo-preprod.bash` as sourced functions with fixture-file test hooks. The OLM install path is new `oc apply` orchestration; the local-build path is unchanged. `demo-preprod-olm.sh` is deleted and its entry points absorbed.

**Tech Stack:** Bash (`set -euo pipefail`), python3 (registry API client, already a hard dependency), `oc`, optional `skopeo`/`opm`. Tests: the repo's `scripts/*_test.sh` sourced-function harness (no cluster).

**Spec:** `docs/superpowers/specs/2026-11-24-demo-preprod-merge-design.md`

**Issue tracking:** pgarciaq/koku-service-operator issues #1–#12 (each task names its issue). Upstream: project-koku/koku-service-operator#219 (arm64), #220 (versioning).

## Global Constraints

- Bash scripts run under `set -euo pipefail`; shellcheck-clean (disable only with a comment saying why).
- No new mandatory binary dependencies: `oc`, `python3`, `curl` required (already are); `skopeo`, `opm` strictly optional with graceful fallback.
- Every new lib function is offline-testable: registry/arch lookups read from a fixture when `REGISTRY_TAG_FIXTURE` / `IMAGE_ARCH_FIXTURE` is set (test hooks; empty in production).
- The default CR render never contains `IQE_TEST_RUN` or `CURRENCY_URL` (they stay in `costmanagementserviceconfig-smoke.yaml` for CI).
- Do not restructure `deploy-byoi.sh`, `deploy-rhbk.sh`, `deploy-incluster.sh` — the merged script orchestrates them as-is (only `deploy-rhbk.sh` gains the password env var).
- Semver default resolves on the *catalog* repo only; catalog tag = version selector; CSV contents are never parsed for version selection.
- `KUBE_CONTEXT` defaults to the current context; nothing auto-loads arm64 image overrides.
- One commit per task, conventional commit messages, issue reference in the body.

## Review Focus

Inputs/failure modes the design implies but no single task obviously covers; each is pinned by a named test in the owning task:

1. **Silent wrong version**: a repo with no semver tag must not install quietly — `resolve_catalog_version` fallback test asserts the warning text and `latest` result (Task 3).
2. **Arch check green, stack dead**: BYOI operand images chosen inside sub-scripts — post-deploy pod-image pass must flag an amd64-only image on arm64 (Task 5).
3. **Version switch no-op**: re-running with a different `--version` over an existing CSV must warn and advise `--reset`, not silently keep the old bundle (Task 4).
4. **First-run context failure**: a customer with no `clusterbot` context must still run — preflight uses the current context and prints it (Task 1).
5. **Demo flags leaking to customers**: rendered default CR must not contain `IQE_TEST_RUN`/`CURRENCY_URL` — render test asserts absence (Task 7).

---

### Task 1: `--source` plumbing and current-context default (issue #1, foundation)

**Files:**
- Modify: `scripts/demo-preprod.sh` (flags ~39–55, defaults ~95–115, `print_plan` ~164–196, preflight ~259–268)
- Modify: `scripts/demo-preprod.env.example` (comment `KUBE_CONTEXT=clusterbot` out)
- Modify: `scripts/lib/demo-preprod.bash` (no new functions yet)
- Test: `scripts/demo-preprod_test.sh`

**Interfaces:**
- Produces: env/flag `SOURCE` ∈ `registry|local|manual` (default `registry`); `--source` flag; plan line `source: ${SOURCE}` in `--dry-run` output. Later tasks branch on `$SOURCE`.

- [ ] **Step 1: Write the failing tests** — in `scripts/demo-preprod_test.sh`: (a) `--help` output contains `--source=registry|local|manual`; (b) `./scripts/demo-preprod.sh --dry-run` prints `source: registry` by default and `source: local` with `SOURCE=local` in the environment; (c) dry-run no longer mentions `clusterbot` as a default.
- [ ] **Step 2: Run** `bash scripts/demo-preprod_test.sh` — expect the new assertions FAIL, existing ones PASS.
- [ ] **Step 3: Implement** — add `--source)` flag parsing, `SOURCE="${SOURCE:-registry}"` default, `print_plan` line, and change `KUBE_CONTEXT` default from `clusterbot` to empty (= current context, printed in preflight). Comment out `KUBE_CONTEXT=clusterbot` in the env example, leaving a hint comment.
- [ ] **Step 4: Run** `bash scripts/demo-preprod_test.sh` — expect PASS (exit 0).
- [ ] **Step 5: Commit** — `feat(demo): add --source selector and current-context default (#1)`

### Task 2: Absorb `demo-preprod-olm.sh` entry points; delete the script (issue #1)

**Files:**
- Modify: `scripts/demo-preprod.sh` (add `--watch`, `--console` modes; manual-source branch)
- Delete: `scripts/demo-preprod-olm.sh`
- Delete/Modify: `scripts/demo-preprod-olm_test.sh` (port assertions, then remove the file)
- Test: `scripts/demo-preprod_test.sh`

**Interfaces:**
- Consumes: `$SOURCE` from Task 1.
- Produces: `--watch` mode (requires the CMSC to exist, tmux layout, unbounded→deadline-bounded wait via Task 6); `--console` info mode (prints console URL/username); `manual` source renders the CR to `CR_FILE` (default `/tmp/koku-demo-preprod-${NAMESPACE}.yaml`) and prints instructions, then exits 0.

- [ ] **Step 1: Write the failing tests** — `--help` contains `--watch` and `--console`; dry-run with `SOURCE=manual` prints `CR_FILE:` and the words `Install the operator through the OpenShift console`; `scripts/demo-preprod-olm.sh` no longer exists (test asserts `[[ ! -x ... ]]`).
- [ ] **Step 2: Run** `bash scripts/demo-preprod_test.sh` — expect FAIL on new assertions.
- [ ] **Step 3: Implement** — port the `load_env_file`/`print_console_access`/instruction-printing logic from `demo-preprod-olm.sh` into the merged script keyed on `$SOURCE == manual`; add `--watch`/`--console` flags; delete `demo-preprod-olm.sh` and its test file after porting every assertion.
- [ ] **Step 4: Run** `bash scripts/demo-preprod_test.sh` — expect PASS.
- [ ] **Step 5: Commit** — `feat(demo): absorb demo-preprod-olm.sh; add manual source, --watch, --console (#1)`

### Task 3: Version discovery and selection (issue #2)

**Files:**
- Modify: `scripts/lib/demo-preprod.bash` (new functions)
- Modify: `scripts/demo-preprod.sh` (`--version`, `--catalog-repo`, `--channel` flags; preflight resolution)
- Modify: `scripts/demo-preprod.env.example` (documented vars)
- Test: `scripts/demo-preprod_test.sh`

**Interfaces:**
- Produces (lib): `registry_list_tags <image-repo>` → newline-separated tags (HTTP via curl+python3 against `https://quay.io/api/v1/repository/<repo>/tag/?limit=100&page=N`; reads `$REGISTRY_TAG_FIXTURE` file instead when set); `resolve_catalog_version <repo> <version-hint>` → prints resolved tag, and sets `RESOLVE_WARN` when falling back (hint empty → highest `^v?\d+\.\d+\.\d+$`; `latest|main` passthrough; `[0-9a-f]{7,40}` normalized against the tag list; `@sha256:` suffix preserved).
- Produces (script): `CATALOG_REPO` (default `quay.io/project-koku`), `VERSION` (default: resolved), `CHANNEL` (default `beta`), `CATALOG_IMG="${CATALOG_REPO}/koku-service-operator-catalog:${VERSION}"`.

- [ ] **Step 1: Write the failing tests** — with fixture files: (a) tags `[main, latest, 474d937, v0.0.1, v0.0.3, v0.0.2]` → resolves `v0.0.3`; (b) empty hint + tags `[main, latest, 474d937]` → `latest` and `RESOLVE_WARN` non-empty containing `no semver`; (c) hint `474d937` → passthrough; (d) hint `v0.0.1@sha256:abc` → preserved; (e) dry-run prints `catalog: quay.io/project-koku/koku-service-operator-catalog:v0.0.3` (fixture-driven).
- [ ] **Step 2: Run** `bash scripts/demo-preprod_test.sh` — expect FAIL.
- [ ] **Step 3: Implement** the two lib functions (quay API pagination loop; python3 for JSON→lines) and the script-side flags/preflight resolution that prints the loud warning when `RESOLVE_WARN` is set.
- [ ] **Step 4: Run** `bash scripts/demo-preprod_test.sh` — expect PASS. Also run once without fixtures against the real quay API (`--dry-run`) and record the resolved tag (`v0.0.1` today) in the commit message.
- [ ] **Step 5: Commit** — `feat(demo): version discovery and selection via catalog tags (#2)`

### Task 4: Automated OLM install path (issue #3)

**Files:**
- Modify: `scripts/demo-preprod.sh` (registry-source branch of the operator step)
- Test: `scripts/demo-preprod_test.sh` (unit parts); e2e on a cluster

**Interfaces:**
- Consumes: `CATALOG_IMG`, `CHANNEL`, `NAMESPACE`, `CR_NAME` from Tasks 1/3.
- Produces: function `olm_install` (in the main script): applies CatalogSource `koku-service-operator-catalog` (grpc, `openshift-marketplace`, image `"$CATALOG_IMG"`), OperatorGroup (AllNamespaces) + Subscription (name `koku-service-operator`, channel `"$CHANNEL"`, Automatic) in `$NAMESPACE`; waits for CSV `Succeeded` (timeout 600s, 10s poll); before applying, if a CSV matching `koku-service-operator.*` exists it prints the pre-existing-install warning naming `--reset`.

- [ ] **Step 1: Write the failing tests** — unit-test what is offline-testable: a lib-level `olm_manifests` helper that *prints* the three YAMLs (CatalogSource/OperatorGroup/Subscription) given `CATALOG_IMG`/`CHANNEL`/`NAMESPACE`; assert the YAML fields (image, channel, installPlanApproval: Automatic, targetNamespaces absent). Test the warning predicate: given "csv exists" (stub `oc` via `KUBECTL=echo`-style fixture), the warning text with `--reset` is produced.
- [ ] **Step 2: Run** `bash scripts/demo-preprod_test.sh` — expect FAIL.
- [ ] **Step 3: Implement** `olm_manifests` in the lib and `olm_install` + the registry-source branch in the script. Optional `opm render` exact-bundle check only when `command -v opm`.
- [ ] **Step 4: Run** `bash scripts/demo-preprod_test.sh` — expect PASS.
- [ ] **Step 5: E2E on the local CRC cluster (amd64, OpenShift 5.0):** first run the OLM-on-5.0 spike — create the CatalogSource and confirm the package resolves and the CSV installs (the bundle pins `com.redhat.openshift.versions: v4.22`, so 5.0 behavior must be verified, not assumed). Then `SOURCE=registry ./scripts/demo-preprod.sh --no-tmux` installs CSV `koku-service-operator.v0.0.1` Succeeded and reaches the UI wait. Record the CSV name and Succeeded phase in the commit message.
- [ ] **Step 6: Commit** — `feat(demo): automated OLM install path for the registry source (#3)`

### Task 5: Architecture autodiscovery and image-arch checks (issue #4)

**Files:**
- Modify: `scripts/lib/demo-preprod.bash` (new functions)
- Modify: `scripts/demo-preprod.sh` (preflight wiring, post-deploy pass, docker build platform)
- Test: `scripts/demo-preprod_test.sh`

**Interfaces:**
- Produces (lib): `cluster_node_arch` → prints `amd64|arm64` (worker `nodeInfo.architecture`; mixed → warn, first worker; other values → `unsupported`); `image_arches <image-ref>` → space-separated arch set (`skopeo inspect --raw` when available, else python3 docker-registry-v2 anonymous flow; reads `$IMAGE_ARCH_FIXTURE/<ref>.txt` when the env var is set; prints `unknown` on failure); `assert_arch_compatible <arch> <image...>` → exit-nonzero with a per-image list, or warns `could not verify` for `unknown`.
- Produces (script): `NODE_ARCH` (set in preflight from `cluster_node_arch`; `amd64|arm64`), used by `build_docker` as `--platform linux/${NODE_ARCH}`; `ARCH_CHECK` env (default `1`; `0` skips with a warning).

- [ ] **Step 1: Write the failing tests** — fixtures: (a) `image_arches` multi-arch fixture → `amd64 arm64`; single-arch fixture → `amd64`; missing fixture → `unknown` (warning path, not failure). (b) `assert_arch_compatible arm64 <single-arch-img>` fails with the image name and the `#219` upstream reference in the message. (c) `ARCH_CHECK=0` converts the failure into a warning. (d) render-side: the list of workload images extracted from a rendered CR is checked (fixture CR text).
- [ ] **Step 2: Run** `bash scripts/demo-preprod_test.sh` — expect FAIL.
- [ ] **Step 3: Implement** the lib functions and wire them: preflight checks `CATALOG_IMG` + all workload images in the rendered CR (render happens before the operator step; keep the rendered file); `build_docker` uses `--platform linux/${NODE_ARCH}`; after the BYOI step, a post-deploy pass reads `oc get pods -o jsonpath` images in the prereq namespaces and runs the same check, reporting image-failing pods with arch context.
- [ ] **Step 4: Run** `bash scripts/demo-preprod_test.sh` — expect PASS.
- [ ] **Step 5: E2E negative on arm64 CRC:** with the default registry source and `v0.0.1`, preflight fails listing the amd64-only operator/workload images and the #219 explanation. Record the failure output in the commit message.
- [ ] **Step 6: Commit** — `feat(demo): architecture autodiscovery and image-arch checks (#4)`

### Task 6: Unified UI wait and post-install verification (issue #9)

**Files:**
- Modify: `scripts/demo-preprod.sh` (wait step; `--watch` uses the same helper)
- Test: `scripts/demo-preprod_test.sh`

**Interfaces:**
- Produces: `wait_for_ui` (deadline 900s; polls CMSC phase/conditions every 15s; on timeout prints the CMSC status tail and exits 1) and `verify_conditions` (prints a pass/fail table of CMSC conditions `Available, UIReady, Progressing, Degraded, DiscoveryComplete, DatabaseReady, CacheReady, KafkaReady, StorageReady, AuthenticationReady, SchemaUpToDate, RBACReady, IngressReady, GatewayReady`; `True` = pass; anything else = fail with `.reason`).

- [ ] **Step 1: Write the failing tests** — `verify_conditions` against a fixture CMSC status JSON (via `KUBECTL` stub printing the fixture): all-True fixture → exit 0 and table rows; a fixture with `KafkaReady=False (TopicMissing)` → exit non-zero and the row shows the reason.
- [ ] **Step 2: Run** `bash scripts/demo-preprod_test.sh` — expect FAIL.
- [ ] **Step 3: Implement** both helpers; replace the OLM-script-style unbounded wait (absorbed in Task 2) with `wait_for_ui`; call `verify_conditions` after the UI is ready.
- [ ] **Step 4: Run** `bash scripts/demo-preprod_test.sh` — expect PASS.
- [ ] **Step 5: Commit** — `feat(demo): deadline-bounded UI wait and CMSC verification table (#9)`

### Task 7: Demo-profile hardening (issue #5)

**Files:**
- Modify: `scripts/lib/demo-preprod.bash` (`render_preprod_cr`)
- Modify: `scripts/deploy-rhbk.sh` (password env var only)
- Modify: `scripts/demo-preprod.sh` (banners, summary, password generation)
- Test: `scripts/demo-preprod_test.sh`

**Interfaces:**
- Produces: `render_preprod_cr` drops the `IQE_TEST_RUN` env block (masu) and `CURRENCY_URL` env (koku api) from the rendered output; `deploy-rhbk.sh` honors `REALM_ADMIN_PASSWORD` (default unchanged: `admin`); `demo-preprod.sh` generates one via `openssl rand`, exports it to `deploy-rhbk.sh`, stores it in Secret `{CR_NAME}-realm-admin` (same namespace), prints it once at the end.

- [ ] **Step 1: Write the failing tests** — (a) rendered CR does not contain `IQE_TEST_RUN` or `CURRENCY_URL`; (b) rendered CR still contains them when `DEMO_SMOKE=1` is set (CI escape hatch); (c) end banner helper output contains `not for production` and `docs/install/production.md`.
- [ ] **Step 2: Run** `bash scripts/demo-preprod_test.sh` — expect FAIL.
- [ ] **Step 3: Implement** — render changes (drop lines post-substitution), `REALM_ADMIN_PASSWORD` in `deploy-rhbk.sh`'s `get_realm_users_json` path, generation + Secret + print in the demo script, banners and insecure-settings summary (realm credentials, Kafka PLAINTEXT, `sslMode: disable`, `insecureSkipVerify`).
- [ ] **Step 4: Run** `bash scripts/demo-preprod_test.sh` and the smoke-variant render — expect PASS; confirm `costmanagementserviceconfig-smoke.yaml` untouched.
- [ ] **Step 5: Commit** — `feat(demo): demo-profile hardening — no test routes, random realm password, banners (#5)`

### Task 8: `--prereqs-only`, `--skip-prereqs`, `--cr-file` (issue #6)

**Files:**
- Modify: `scripts/demo-preprod.sh` (flags, flow gates)
- Modify: `scripts/demo-preprod.env.example` (documentation comments)
- Test: `scripts/demo-preprod_test.sh`

**Interfaces:**
- Produces: `PREREQS_ONLY`/`--prereqs-only` (stop after BYOI with a summary + next-steps print), `SKIP_PREREQS`/`--skip-prereqs` (skip BYOI and the OAuth-secret mirror), `CR_FILE`/`--cr-file <path>` (apply that YAML instead of the rendered CR; mutually exclusive with `SOURCE=manual`'s generated file — `--cr-file` wins with a warning).

- [ ] **Step 1: Write the failing tests** — help documents all three; dry-run with `--prereqs-only` prints `stopping after prerequisites`; dry-run with `--skip-prereqs --cr-file /tmp/x.yaml` prints `CR: /tmp/x.yaml` and no BYOI step.
- [ ] **Step 2: Run** `bash scripts/demo-preprod_test.sh` — expect FAIL.
- [ ] **Step 3: Implement** the flags and flow gates (BYOI step guarded by `SKIP_PREREQS != 1`; CR-apply step uses `$CR_FILE` when set).
- [ ] **Step 4: Run** `bash scripts/demo-preprod_test.sh` — expect PASS.
- [ ] **Step 5: Commit** — `feat(demo): prereqs-only and operator-only modes with --cr-file (#6)`

### Task 9: `--profile=single-node` and `crc.env` split (issue #8)

**Files:**
- Modify: `scripts/demo-preprod.sh` (profile flag; Kafka sizing env)
- Create: `scripts/demo-preprod.arm64.env` (opt-in image-override template, content moved from `crc.env`)
- Delete: `scripts/demo-preprod.crc.env`
- Modify: `scripts/demo-preprod.env.example` (profile documentation)
- Test: `scripts/demo-preprod_test.sh`

**Interfaces:**
- Produces: `--profile=single-node` (alias `--crc`): sets `KAFKA_BROKER_REPLICAS=1 KAFKA_CONTROLLER_REPLICAS=1 KAFKA_BROKER_STORAGE=10Gi KAFKA_CONTROLLER_STORAGE=5Gi` only. The arch-override env layering from the old crc mode is removed; `demo-preprod.arm64.env` is never loaded by any flag.

- [ ] **Step 1: Write the failing tests** — dry-run with `--profile=single-node` shows `profile: single-node` and Kafka sizing; dry-run with `--crc` is identical; the plan does not mention arm64 image overrides; `demo-preprod.crc.env` no longer exists.
- [ ] **Step 2: Run** `bash scripts/demo-preprod_test.sh` — expect FAIL.
- [ ] **Step 3: Implement** the profile flag, move the arm64 override block verbatim into `demo-preprod.arm64.env` with a header saying "copy to demo-preprod.local.env; never auto-loaded", delete `crc.env`, and drop the crc-specific env layering from the script.
- [ ] **Step 4: Run** `bash scripts/demo-preprod_test.sh` — expect PASS.
- [ ] **Step 5: Commit** — `feat(demo): single-node sizing profile; split arm64 overrides into opt-in template (#8)`

### Task 10: Extend `--reset` to OLM objects (issue #7)

**Files:**
- Modify: `scripts/demo-preprod.sh` (reset block)
- Test: `scripts/demo-preprod_test.sh` (unit parts); e2e on a cluster

**Interfaces:**
- Consumes: reset sequence from the existing script; OLM object names from Task 4.
- Produces: after CMSC deletion and before namespace deletion: delete Subscription `koku-service-operator`, CSVs by label `operators.coreos.com/koku-service-operator.${NAMESPACE}`, OperatorGroup in `$NAMESPACE`, and CatalogSource `koku-service-operator-catalog` in `openshift-marketplace` (all `--ignore-not-found`).

- [ ] **Step 1: Write the failing tests** — the reset block's command list (extracted via a `KUBECTL` echo-stub) contains all four deletions with the right namespaces, in order after the CMSC deletion.
- [ ] **Step 2: Run** `bash scripts/demo-preprod_test.sh` — expect FAIL.
- [ ] **Step 3: Implement** the four deletions in the documented order.
- [ ] **Step 4: Run** `bash scripts/demo-preprod_test.sh` — expect PASS.
- [ ] **Step 5: E2E:** after a registry install (Task 4), `--reset` leaves none of: CatalogSource, CSV, Subscription, OperatorGroup, CMSC; namespaces are gone. Record `oc get` outputs in the commit message.
- [ ] **Step 6: Commit** — `feat(demo): reset removes OLM objects (#7)`

### Task 11: Test consolidation (issue #10)

**Files:**
- Modify: `scripts/demo-preprod_test.sh` (final pass)
- Test: `scripts/demo-preprod_test.sh` (self)

- [ ] **Step 1: Audit** every issue #1–#9 acceptance criterion against the test file; add any missing case (this is the consolidation the earlier tasks may have left partial: e.g. `KUBE_CONTEXT` default, `--cr-file` passthrough, `ARCH_CHECK=0`).
- [ ] **Step 2: Run** `bash scripts/demo-preprod_test.sh` and the repo's full script-test set (`bash scripts/deploy-rhbk_test.sh`, `bash scripts/deploy-test-operator_test.sh`) — expect PASS everywhere.
- [ ] **Step 3: Commit** — `test(demo): consolidate merged-script coverage (#10)`

### Task 12: Documentation rewrite (issue #11)

**Files:**
- Create: `docs/development/pre-prod-demo.md` (merged runbook)
- Delete: `docs/development/pre-prod-install.md`, `docs/development/pre-prod-olm-demo-runbook.md`
- Modify: `docs/development/crc-testing.md`, `scripts/README.md`, `scripts/demo-preprod.env.example`

- [ ] **Step 1: Write the merged runbook** covering: prerequisites, the three sources, version/arch discovery and their failure modes, `--prereqs-only`/`--skip-prereqs --cr-file`, profiles, reset, the verification table, manual-mode console instructions, and the arm64 template; cross-reference upstream #219/#220.
- [ ] **Step 2: Update** `crc-testing.md` (profile semantics, arm64 template path), `scripts/README.md` (new entrypoint surface), env example comments.
- [ ] **Step 3: Verify** every command in the runbook exists in `--help` (grep the help output for each documented flag).
- [ ] **Step 4: Commit** — `docs: single runbook for the merged demo installer (#11)`

---

### Task 13: Container tool abstraction — docker/podman (issue #12; executes after Task 5, see order table)

**Files:**
- Modify: `scripts/demo-preprod.sh` (`docker_up` ~551, `build_docker` ~570, auto-mode selection ~578–592)
- Modify: `scripts/demo-preprod.env.example` (document `CONTAINER_TOOL`)
- Test: `scripts/demo-preprod_test.sh`

**Interfaces:**
- Consumes: `NODE_ARCH` from Task 5.
- Produces (lib): `container_tool` → prints `docker|podman|none` (honors `CONTAINER_TOOL` env override; docker only when `docker buildx version` succeeds; else podman when `command -v podman`; else `none`). `docker_up` becomes the capability probe (buildx check — the podman-docker shim fails it). `build_docker` branches: docker → `docker buildx build --platform linux/${NODE_ARCH} -t "$IMG" --push "$ROOT"`; podman → `podman build --platform linux/${NODE_ARCH} -t "$IMG" "$ROOT"` then `podman push "$IMG"`. Auto-mode preference: openshift when `IMG` is the internal registry, else the detected tool, else openshift.

- [ ] **Step 1: Write the failing tests** — with a stub `bin` dir prepended to `PATH`: (a) stub `docker` whose `info` succeeds but `buildx version` fails (podman-docker shim emulation) → `docker_up` returns false and `container_tool` prints `none` (or `podman` when a stub podman is present); (b) capable docker stub → `container_tool` prints `docker`; (c) build-command emission: docker path emits one `buildx build … --push`; podman path emits `build` followed by `push`; (d) `CONTAINER_TOOL=podman` override wins.
- [ ] **Step 2: Run** `bash scripts/demo-preprod_test.sh` — expect FAIL on the new assertions.
- [ ] **Step 3: Implement** the lib helper, the capability probe, and the branching build function per the interfaces above; document `CONTAINER_TOOL` and the `oc registry login` note for podman pushes to the internal registry in the env example.
- [ ] **Step 4: Run** `bash scripts/demo-preprod_test.sh` — expect PASS.
- [ ] **Step 5: Commit** — `feat(demo): docker/podman container tool abstraction for local builds (#12)`

---

## Task order and issue mapping

| Order | Task | Issue | Depends on |
|---|---|---|---|
| 1 | Task 1 | #1 | — |
| 2 | Task 2 | #1 | Task 1 |
| 3 | Task 3 | #2 | Task 1 |
| 4 | Task 4 | #3 | Tasks 1, 3 |
| 5 | Task 5 | #4 | Task 1 |
| 6 | Task 13 | #12 | Tasks 1, 5 |
| 7 | Task 6 | #9 | Tasks 1, 2 |
| 8 | Task 7 | #5 | Task 1 |
| 9 | Task 8 | #6 | Task 1 |
| 10 | Task 9 | #8 | Task 1 |
| 11 | Task 10 | #7 | Task 4 |
| 12 | Task 11 | #10 | all functional tasks |
| 13 | Task 12 | #11 | all functional tasks |

Tasks 6–9 and 13 are mutually independent after their dependencies and can be executed in any order or in parallel; the order above is the recommended sequence (default path and safety first, then UX, then teardown, then consolidation). E2E environments (allowed clusters only): **local CRC** (amd64, OpenShift 5.0 — primary target for all e2e including the registry source, gated on the Task 4 OLM-on-5.0 spike) and the **developer sandbox** (no cluster-admin — usable only for `--prereqs-only`/BYOI namespaced testing; the operator itself cannot be installed there: no `openshift-marketplace` CatalogSource rights and the AllNamespaces-only CSV needs cluster-scoped RBAC). E2e checkpoints: end of Task 4 (CRC registry install), Task 5 (arm64 negative — offline fixture coverage; no arm64 cluster available), Task 10 (reset cycle on CRC).
