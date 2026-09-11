# OpenShift CI log and artifact redaction

Prow job logs and `${ARTIFACT_DIR}` are **world-readable**. koku-service-operator e2e jobs (`e2e-pytest`, `e2e-iqe`) therefore fail closed: if a stream or file cannot be sanitized, it is withheld or the step fails rather than publishing secrets, session IDs, PII, kubeconfigs, or claimed-cluster API URLs.

The sanitizer **does not live in this repository**. It is the OpenShift CI step [`project-koku-koso-sanitize`](https://github.com/openshift/release/tree/master/ci-operator/step-registry/project-koku/koso-sanitize):

| File in `openshift/release` | Role |
|-----------------------------|------|
| [`project-koku-koso-sanitize-ref.yaml`](https://github.com/openshift/release/blob/master/ci-operator/step-registry/project-koku/koso-sanitize/project-koku-koso-sanitize-ref.yaml) | Step metadata; runs from image `koku-service-operator-e2e` |
| [`project-koku-koso-sanitize-commands.py`](https://github.com/openshift/release/blob/master/ci-operator/step-registry/project-koku/koso-sanitize/project-koku-koso-sanitize-commands.py) | Redaction engine, CLI, bash wrappers, self-test |

`e2e-olm` does **not** use this step (install-only, no application reports).

High-level job flow: [openshift-ci.md](openshift-ci.md). Per-step usage: [openshift-ci-jobs.md](openshift-ci-jobs.md).

## How it is installed into a job

The first test step on `e2e-pytest` / `e2e-iqe` is:

```yaml
- ref: project-koku-koso-sanitize
```

That step:

1. Copies itself to `${SHARED_DIR}/koso-sanitize.py`.
2. Writes `${SHARED_DIR}/koso-sanitize.sh` (bash helpers).
3. Runs an in-process **self-test**. If any case fails, the job stops before install.

Every later step must:

```bash
source "${SHARED_DIR}/koso-sanitize.sh"
```

`SHARED_DIR` is how ci-operator passes files between steps on the same pod. The helper is not in the git checkout of this repo.

## Helpers later steps must use

Defined in `write_shell_wrappers()` in the Python file above.

| Helper | Use for | Behavior |
|--------|---------|----------|
| `_sanitize` | Pipe a command’s combined stdout/stderr | `cmd 2>&1 \| _sanitize` |
| `_run_sanitized cmd…` | `oc` / `kubectl` / operator commands that print diagnostics | Redacts stdout+stderr; returns **cmd** exit code; **exits 1** if redaction itself fails (`\|\| true` cannot swallow that) |
| `_capture_sanitized VAR cmd…` | Values needed later (CSV name, phase) | Assigns **raw** stdout to `VAR`; redacts stderr onto the log. **Do not echo `$VAR`** if it might be sensitive. CSV names are safe; tokens are not. |
| `_publish_sanitized SRC DEST` | JUnit / HTML into `${ARTIFACT_DIR}` | Copy only if sanitizable; unsanitizable source is withheld (success for the job, no leak) |
| `_sanitize_tree [DIR…]` | Workspace report dirs, then `${ARTIFACT_DIR}` | In-place redact or delete. No args → `${ARTIFACT_DIR}` |
| `_sanitize_shared_reports` | `${SHARED_DIR}` | Only **report** files: names starting with `junit` (any extension), plus `iqe_junit.xml`, `report.html`, `pytest_report.html`, `iqe_report.html`, and any file under a `reports/` subdirectory. Cluster metadata, YAML, logs, kubeconfig in `SHARED_DIR` are left alone so later steps still work |

Pattern used around pytest / IQE / `e2e.sh`:

```bash
set +e
./scripts/run-pytest.sh ... 2>&1 | _sanitize
pipe_status=("${PIPESTATUS[@]}")
set -e
if [[ "${pipe_status[1]:-1}" -ne 0 ]]; then
  echo "redaction step failed; refusing to publish unsanitized output" >&2
  exit 1
fi
rc=${pipe_status[0]}
_publish_sanitized test/pytest/reports/junit.xml "${ARTIFACT_DIR}/junit_e2e.xml"
_sanitize_tree test/pytest/reports tests/reports
_sanitize_shared_reports
_sanitize_tree
exit "${rc}"
```

The test’s exit code is preserved **only if** redaction succeeded. A sanitizer failure fails the job even when pytest passed.

## What is redacted vs withheld

`sanitize_text()` walks mixed logs/reports (JSON, YAML, XML, HTML, Python repr, env assignments). Placeholders:

| Placeholder | Typical match |
|-------------|----------------|
| `[credential redacted]` | passwords, tokens, AWS keys, `Authorization`, cookies, PEM blocks, JWTs, Bearer tokens |
| `[customer-data redacted]` | email/ssn/card/account/customer-* keys |
| `[URL redacted]` | `https?://…` |
| `[email redacted]` | email addresses |
| `[ssn redacted]` / `[card redacted]` | SSN / PAN patterns (including adjacent repeats) |
| `[internal-host redacted]` | `*.svc`, `*.apps.`, internal DNS, RFC1918, `oc` “connection to the server … was refused”, `dial tcp …` |
| `[response-body redacted]` | dumped HTTP bodies |

**Withheld entirely** (deleted from the publish path, not redacted in place):

- Files named `kubeconfig` / `kubeadmin-password` / `*.kubeconfig`
- Documents that look like kubeconfigs (`kind: Config` + user cert/key data)
- Unknown binaries (`\x00`, low text ratio, `.png` / `.so` / …)
- Archives (`.gz` / `.tgz` / `.zip`) whose inner files cannot be sanitized

The sanitizer’s own sources (`koso-sanitize.sh`, `koso-sanitize.py`) are never rewritten.

## Why `SHARED_DIR` is special

ci-operator puts **cluster credentials and step inputs** in `SHARED_DIR` (including kubeconfig). A naive “sanitize every JSON/YAML in SHARED_DIR” would rewrite or delete files the next step still needs.

`sanitize_shared_reports()` therefore only touches:

- Names: `junit.xml`, `iqe_junit.xml`, `report.html`, `pytest_report.html`, `iqe_report.html`, or names starting with `junit` (any extension)
- Any file under a `reports/` subdirectory

## Self-test

`--selftest` (run at install) includes:

- Stream cases: JSON/YAML/XML/HTML/headers/Python repr, adjacent SSNs/cards/RFC1918, `oc` connection-refused and dial-tcp messages
- `_run_sanitized` / `_capture_sanitized` wrapper tests: command exit codes preserved, hosts stripped from logs, captured CSV names not printed, heredoc stdin still delivered to `oc apply`
- Shared-dir: reports redacted, kubeconfig and non-report YAML/JSON/logs left intact

If you change redaction rules, extend `STREAM_CASES` and `_run_wrapper_selftest` in the same PR in `openshift/release`.

## This repo’s own redaction (stack script)

[`hack/ci/e2e.sh`](../../hack/ci/e2e.sh) has a **second**, narrower `redact_filter` for dumps it writes itself (operator logs, CMSC JSON with `managedFields` stripped, events). That is defense in depth for local/Cluster Bot runs where `koso-sanitize` is not installed. `make test-hack` on GitHub Actions covers issuer injection and RHBK port-forward helpers; it does **not** run `koso-sanitize` (that lives only in `openshift/release`).

On Prow, `e2e.sh` stdout still goes through `_sanitize`, and install-step dumps go through `_run_sanitized`. Do not log Secret or ConfigMap **payloads** in either layer — table output of names is fine; `-o yaml` of secrets is not.

## Changing job scripts in `openshift/release`

When editing inline `commands:` in the ci-operator config:

- `source "${SHARED_DIR}/koso-sanitize.sh"` at the top of every e2e-pytest/e2e-iqe step after sanitize.
- Use `_run_sanitized oc …` for cluster commands that print errors.
- Use `_capture_sanitized` for values; never `echo` captured kube API output.
- Pipe long-running scripts through `_sanitize` and check `PIPESTATUS[1]`.
- Publish JUnit/HTML only via `_publish_sanitized`.
- End with `_sanitize_tree` on workspace reports and `${ARTIFACT_DIR}`.

ci-operator docs on secrets in logs: treat Prow logs as public. `set -x` will leak arguments; the koso wrappers temporarily disable `errexit` around pipes but do not make `set -x` safe.
