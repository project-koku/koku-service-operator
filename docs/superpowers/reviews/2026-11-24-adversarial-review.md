# Adversarial review — merged demo-preprod installer

Date: 2026-11-24
Branch: `pgarciaq-installation-script-enhancements` (22 commits vs `main`, ~2,500 insertions / 992 deletions)
Method: three independent fresh reviewers (Saboteur, New Hire, Security Auditor), each with a hostile
mandate and a mandatory-findings rule; findings deduplicated and cross-promoted (2+ personas → +1 severity).
Verdict: **BLOCK** — 4 critical findings; no PR until resolved. Fix work: fork issues #13–#24.

## Critical findings

### C1 — tmux re-exec silently drops most command-line flags
`scripts/demo-preprod.sh` (`start_tmux`): the inner invocation forwards only
`DEMO_RESET/DEMO_REBUILD/DEMO_NO_OPEN/PROFILE`; `PREREQS_ONLY`, `SKIP_PREREQS`,
`USER_CR_FILE` (--cr-file), `CHANNEL` are never exported or forwarded, and tmux
is the **default** path whenever tmux is installed. `--prereqs-only` in tmux
mode performs a full install; `--skip-prereqs --cr-file` installs the
prerequisites and applies the rendered sample CR instead of the user's;
`--channel stable` installs `beta`. `--version`/`--catalog-repo` survive only
by accident (exported `CATALOG_IMG`). Empirically verified by the reviewer.
Fix: forward `"$@"` on the inner invocation; add a behavior test with a tmux
stub.

### C2 — .local.env is dead for every uncommented key in .env.example
`load_env_file` only sets unset keys ("first file wins") and `env.example`
loads before `local.env` — so the example's uncommented defaults
(`NAMESPACE`, `CR_NAME`, the other namespaces, `BUILD_MODE`, …) permanently
defeat `local.env`, while the example's own header tells users to copy it to
`local.env` and edit. `NAMESPACE=my-ns` in `local.env` silently installs into
`cost-byoi`; with `--reset` this deletes the wrong namespaces. The script
header, an in-code comment, and the new runbook each document the *intended*
precedence as if it worked. Verified by two reviewers with repros.
Pre-existing on `main`, but this branch enshrined it in documentation.
Fix: load `local.env` first; correct the three documents.

### C3 — `wait_for_ui || exit 1` disables errexit inside the function
Bash suppresses `set -e` for every command in a function used as the LHS of
`||`. A failed `oc rollout status` (stuck rollout) does not abort: the script
prints "UI ready", opens the browser, returns 0 — ending with "UI ready"
followed by a failed verification table. Verified with a minimal repro; the
test suite enforces the broken pattern via a source grep.
Fix: call `wait_for_ui` plainly, make `rollout status` failure `return 1`
explicitly, fix the test.

### C4 — skip-if-ready checks treat zero available replicas as healthy
Kubernetes omits `.status.availableReplicas` when it is zero, so
`[[ "$(… || echo 0)" != "0" ]]` sees an empty string and passes. A
crash-looping operator / postgresql / UI deployment passes "already
Available"/"already rolled out", is skipped, and the run dies 15 minutes later
in a timeout that never names the crashed deployment. (Promoted from WARNING:
flagged independently by two reviewers.) Pre-existing on `main`; this branch's
idempotency features are built on it.
Fix: coalesce empty to 0 in `ui_deploy_ready`, `operator_ready`,
`infra_ready`; stub tests for the zero/empty/absent cases.

## Warnings

1. **Unvalidated `--version` interpolated into cluster-admin-applied YAML** —
   multiline values (via the `@sha256:` suffix or the registry-unreachable
   passthrough) can inject arbitrary Kubernetes objects into the CatalogSource
   apply. Fix: whitelist tag (`^v?[0-9A-Za-z._-]+$`) and digest
   (`sha256:[0-9a-f]{64}`) at parse time.
2. **`REALM_ADMIN_PASSWORD` spliced into a JSON literal unescaped** in
   `deploy-rhbk.sh`; `"` or `\` breaks the JSON, the failure is masked
   downstream (`local` discards jq's exit code), and the demo ends
   "successfully" printing a password that cannot log in. Fix: build the JSON
   with `jq -n --arg`; fail hard on parse errors.
3. **Fixture test hooks honored in production** — `REGISTRY_TAG_FIXTURE` /
   `IMAGE_ARCH_FIXTURE` activate with no gate; a lingering variable silently
   fakes version discovery and no-ops the arm64 protection (a missing fixture
   yields `unknown`, downgraded to a warning). Fix: require an explicit
   opt-in.
4. **Supply-chain TOCTOU** — the tag is resolved, then the Subscription pins
   the *tag* (a moving target) with Automatic approval. Fix: pin
   `repo:tag@sha256:<digest>` at resolution (the quay tag API already returns
   `manifest_digest`).
5. **Credentials travel in `curl` argv and over TLS-unverified connections**
   (pre-existing `deploy-rhbk.sh`: `curl -sk`, `-d "password=…"`, Bearer in
   argv) — the branch's random password transits this path; the hardening is
   partly cosmetic. Fix (upstream): stdin bodies, `--cacert`, no `-k`.
6. **`OPERATOR_VERSION` is a hardcoded `v0.0.1`** printed next to the real
   resolved catalog tag; the manual instructions tell users to "verify the
   catalog offers v0.0.1" even when they pinned something else. Fix: derive
   or delete.
7. **`print_plan`'s step list is written for the `local` path** — wrong steps
   and numbering for the default registry path; with `--cr-file` it describes
   a render that never happens. Fix: source-conditional step list.
8. **`render_preprod_cr`'s primary substitutions are silent no-ops on sample
   drift** (name/namespace/clusterDomain needles unguarded, unlike the others)
   — a drifted sample renders a CR for the wrong namespace/domain.
   Fix: strict needle guards.
9. **Tests grep source text instead of behavior** (~15 assertions) — they pass
   on dead code and fail on harmless refactors, and they are why C1 shipped.
   Fix: behavior tests (tmux stub, ready-check stubs) for the critical paths.

## Notes (triaged)

macOS bash 3.2 empty-array crash under `set -u`; SHA-prefix ambiguity in
version matching and the 20-page tag-list cap; `verify_bundle_match` takes the
first bundle in multi-bundle catalogs; `fix_keycloak_hostname_v2`'s 600s
deadline starts before provisioning; mixed-architecture clusters resolve amd64
and proceed; `KUBECTL=kubectl` passes preflight then dies on hardcoded `oc`;
network I/O before mode dispatch (`--watch`/`--console`/`--reset` hit quay);
the `USER_CR_FILE`/`CR_RENDER_TMP`/`CR_TO_APPLY`/`CR_FILE` shuffle;
`usage()`'s sed-range fragility and tmux undocumented; stale `[crc]` labels
and triplicated helpers; the "offline" test suite making live quay calls;
password printed to terminal/scrollback without a retrieval-command
alternative; silent `admin` fallback when the Secret read-back fails;
`insecureSkipVerify` unconditional with no knob.

## Summary

The branch's default execution paths — tmux re-exec, env-file precedence,
skip-if-ready, and the UI wait — can silently do the opposite of what the user
asked while printing that they did the right thing. Three of the four
criticals are pre-existing on `main`; this branch built its headline features
on top of them, and the branch's own e2e runs never caught them because they
used `--no-tmux` and healthy clusters.

---

## Re-review of the fixes (2026-11-25)

A fresh hostile reviewer verified all twelve fix claims against the code (all
hold; both offline suites pass) and hunted for regressions. Verdict:
**REGRESSION** — three warnings, all fixed immediately after:

1. **`--no-open` broken by the #15 parameterization** — the lib `wait_for_ui`
   lost the `DEMO_NO_OPEN` gate; macOS hosts opened the browser at UI-ready.
   Fixed: `should_open_browser` folds both flags; both call sites and the
   watch-tmux forwarding use it / forward `DEMO_NO_OPEN`.
2. **`--watch` printed the legacy-default password** — the read-back ran after
   the watch block exited; and `start_watch_tmux` dropped combined flags (the
   #13 bug class). Fixed: the read-back moved before the watch dispatch
   (verified live: `--watch` now prints the Secret's password); the watch tmux
   pane forwards the raw invocation and the flag-backed env forms.
3. **Env forms lost in tmux** — `PREREQS_ONLY=1 ./script` inherits the tmux
   server's environment, not the caller's. Fixed: the inner command forwards
   the flag-backed variables (`PREREQS_ONLY`, `SKIP_PREREQS`, `USER_CR_FILE`,
   `VERSION`, `CHANNEL`, `CATALOG_REPO`, `ARCH_CHECK`,
   `REALM_ADMIN_PASSWORD`, `CONTAINER_TOOL`, `KUBE_CONTEXT`, …).

Also addressed from the notes: signal exits no longer leak `safe_curl`'s
secret-bearing files (INT/TERM/HUP trap); an ungated fixture variable aborts
the install instead of degrading to `:latest`; `CATALOG_IMG` gets the same
charset validation as `--version` and its derived version shows the tag, not a
digest fragment; dead `ui_deploy_ready` removed; `safe_curl` handles the
`--header`/`--data-binary` long forms and quotes its removals. Accepted: OCI
charset false positives (`+` in tags) — loud and rare.
