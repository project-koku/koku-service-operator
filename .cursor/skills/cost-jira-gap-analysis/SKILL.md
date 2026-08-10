---
name: cost-jira-gap-analysis
description: >-
  Produces a handoff gap analysis of a COST JIRA ticket against the
  koku-service-operator codebase and writes docs/gap_analysis/COST-XXXX.md.
  Use only when explicitly invoked (e.g. /cost-jira-gap-analysis or the user
  asks to run this skill on a COST ticket).
disable-model-invocation: true
---

# COST JIRA Gap Analysis

Audit one COST ticket against the current implementation. Output a standalone
handoff doc under `docs/gap_analysis/`. Do **not** update `docs/tasks.md`,
`docs/design/design-vs-jira.md`, `CLAUDE.md`, sample CRs, or other trackers
unless the user explicitly asks.

## Inputs

Require a ticket key (`COST-7678`, etc.). If missing, ask once before researching.

## Workflow

Copy and track:

```
Gap analysis progress:
- [ ] 1. Load ticket sources
- [ ] 2. Inventory implementation
- [ ] 3. Classify findings
- [ ] 4. Write docs/gap_analysis/COST-XXXX.md
- [ ] 5. Confirm scope (no collateral doc edits)
```

### 1. Load ticket sources

In parallel:

1. Fetch live Jira via Atlassian MCP (`getJiraIssue` on `redhat.atlassian.net`, key `COST-XXXX`, markdown body).
2. Read local mirror `docs/jira/COST-XXXX.md` if present.
3. Read relevant rows in `docs/tasks.md` and intentional deviations in `docs/design/design-vs-jira.md`.
4. Skim linked design notes under `docs/design/` when the ticket references them.

Extract acceptance criteria from the Jira description (deliverables, out-of-scope, named files/fields/phases).

### 2. Inventory implementation

Compare each acceptance criterion to code and generated manifests. Prefer:

| Area | Look here |
|------|-----------|
| CRD / API types | `api/v1alpha1/` |
| Generated CRD | `config/crd/bases/` |
| Reconciler / phases | `internal/controller/` |
| Resource builders | `internal/resources/` |
| Samples | `config/samples/` |
| Webhooks | `api/v1alpha1/*webhook*`, `config/webhook/`, `cmd/main.go` (`SetupWebhookWithManager`) |
| Tests | `internal/**/*_test.go`, `test/` |

Verify claims with grep/reads (field names, kubebuilder markers, condition constants, missing files). Do not treat `docs/tasks.md` checkmarks as ground truth.

Use explore subagents for broad inventories; keep needle lookups local.

### 3. Classify findings

Every ticket item goes in exactly one bucket:

| Bucket | Meaning |
|--------|---------|
| **Done** | Meets or supersedes ticket intent |
| **Intentional deviation** | Differs from Jira; justified in design docs or CLAUDE.md |
| **Gap** | Required by ticket (or blocking GA API surface) and not done |

Separate **ticket-scoped gaps** from **related GA risks** found in the same surface (e.g. passwords in Spec) so the consumer can prioritize.

Call out false friends: e.g. `internal/controller/validation.go` is reconciler probing, not an admission webhook.

### 4. Write the gap analysis doc

Create or overwrite:

`docs/gap_analysis/COST-XXXX.md`

Use today's date for **Audited**. Follow the template below. Match tone of the example at [docs/gap_analysis/COST-7678.md](../../../docs/gap_analysis/COST-7678.md).

Relative links from `docs/gap_analysis/` to repo paths (`../jira/`, `../../api/`, etc.).

### 5. Confirm scope

- Only the gap analysis file (and `docs/gap_analysis/` if creating the dir).
- No code changes.
- Summarize for the user: path written + one-line verdict + gap IDs.

If Plan mode is active: research and present the analysis in the plan first; write the file only after the user approves execution.

## Output template

```markdown
# COST-XXXX Gap Analysis: <short summary from Jira>

**Jira:** [COST-XXXX](https://redhat.atlassian.net/browse/COST-XXXX)
**Audited:** YYYY-MM-DD
**Purpose:** Handoff audit vs ticket acceptance criteria. Does not update `docs/tasks.md` or other trackers.

## Verdict

<2–4 sentences: done vs not done; name the remaining gaps>

## Sources

- [Jira COST-XXXX](https://redhat.atlassian.net/browse/COST-XXXX)
- [docs/jira/COST-XXXX.md](../jira/COST-XXXX.md)   <!-- omit if missing -->
- <key implementation paths>
- [docs/design/design-vs-jira.md](../design/design-vs-jira.md)  <!-- when relevant -->

Out of scope per ticket: <quote or paraphrase>

---

## Ticket acceptance criteria

| Deliverable | Status |
|-------------|--------|
| ... | Done / Partial / Missing / Deviated |

---

## Done (meets or supersedes ticket intent)

- Bullet inventory with paths

---

## Intentional deviations (do not treat as bugs)

| JIRA | Implementation | Rationale |
|------|----------------|-----------|
| ... | ... | ... |

---

## Remaining gaps (ticket-scoped)

### G1. <title> — <missing|partial>

Evidence and impact. Tables for rule-by-rule validation/defaulting when useful.

### G2. ...

## Related gaps (not in Jira body; same surface)

Optional. Security/API risks discovered while auditing.

---
```

## Quality bar

- Ground every Done/Gap claim in a file path (and marker/field name when relevant).
- Prefer precise Status cells: Done / Partial / Missing / Deviated — not emoji-only.
- Number gaps `G1`, `G2`, … for easy handoff.
- Distinguish CRD OpenAPI defaults from admission webhooks.
- For BYOI vs bundled `deploy` flags, note when validation must be conditional.
- Keep the doc self-contained; the consumer should not need this chat.

## Example

Completed run: [docs/gap_analysis/COST-7678.md](../../../docs/gap_analysis/COST-7678.md)
