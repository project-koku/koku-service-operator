# Review Follow-ups — PR #22 (shell injection fix)

Follow-up items from [jordigilh's review](https://github.com/project-koku/koku-service-operator/pull/22)
of the wait4x migration PR.

---

## 1. `ResolveBootstrapAdmin` silently substitutes test-fixture IDs

**Source:** [inline comment on migration.go:359](https://github.com/project-koku/koku-service-operator/pull/22#discussion_r2940893206)

**Problem:** `ResolveBootstrapAdmin` (migration.go:319) falls back to
`OrgID = "org1234567"` / `AccountNumber = "7890123"` when
`RealmUser.OrgID`/`AccountNumber` are empty. These are Koku's own
well-known internal test fixtures — not safe defaults for production.

Because `SYNC_ORG_ID`/`SYNC_ACCOUNT_NUMBER` feed directly into
`Tenant.objects.get_or_create(org_id=...)` in `rbacAdminBootstrapScript()`,
an operator deployed with an incomplete CR will silently provision a real
tenant under test-fixture IDs in the customer's database instead of
failing loudly on missing config.

The `orgId` and `accountNumber` fields are optional in the CRD with no
validation tying them to `orgAdmin: true`. `TestResolveBootstrapAdmin`
only exercises the explicit-value path (`OrgID: "org9"`), and
`TestAdminBootstrapJobGated` happens to pass `OrgID: "org1234567"`
explicitly — which looks like it tests the fallback but doesn't.
(`ResolveBootstrapAdmin` coverage: 75%, fallback branches uncovered.)

**Suggested fix (pick one):**

- **CRD validation:** require `orgId` + `accountNumber` when
  `orgAdmin: true` via `+kubebuilder:validation:XValidation`.
- **Go-level guard:** have `ResolveBootstrapAdmin` return `ok=false`
  when `OrgID` or `AccountNumber` are empty, rather than substituting
  values that coincide with this project's test fixtures.

Either way, add test cases that exercise the fallback branches and
confirm the chosen behavior (fail vs. default).

**Pre-existing gap** — not introduced by PR #22.

---

## 2. Image digest pinning

**Source:** [general comment](https://github.com/project-koku/koku-service-operator/pull/22#issuecomment-5253476144)

All operator and component image references use tags only (e.g.
`registry.redhat.io/rhel10/postgresql-16:10.1`). Tags are mutable;
pinning to `tag@sha256:digest` enables Dependabot `docker` ecosystem to
track and auto-bump them. Priority: before GA.

---

## 3. `relatedImages` in OLM bundle (COST-7695)

**Source:** [general comment](https://github.com/project-koku/koku-service-operator/pull/22#issuecomment-5253476144)

Images constructed at runtime in `internal/resources/*.go` (Postgres,
Valkey, Kruize, Koku, ROS, RBAC…) are not captured in the CSV
`relatedImages` list, so airgapped/`oc-mirror` deployments cannot
discover them. Needs a `RELATED_IMAGE_*` env-var convention on the
manager Deployment + bundle generation integration. Tracked under
COST-7695.

---

## 4. RBAC migration/bootstrap code provenance

**Source:** [general comment](https://github.com/project-koku/koku-service-operator/pull/22#issuecomment-5257084580)

`rbacMigrationScript` and `rbacAdminBootstrapScript` embed 60–130 lines
of Django ORM Python as Go string literals, executed via
`manage.py shell <<'HEREDOC'`. This fails the code-provenance question
an audit asks. Recommended fix: custom Django management commands in
`insights-rbac`. Long-term: versioned REST/gRPC API. Requires
`insights-rbac` maintainer buy-in. See [Jordi's comment](https://github.com/project-koku/koku-service-operator/pull/22#issuecomment-5257084580)
for full analysis including dropped alternatives.
