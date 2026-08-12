# koku-service-operator

Kubernetes operator that deploys and manages the on-premise Cost Management
stack on OpenShift. Users install it via OLM, apply one `CostManagementServiceConfig`
CR, and the operator wires all application services to their pre-existing
external infrastructure.

## Key identifiers

| Item | Value |
|------|-------|
| Module | `github.com/project-koku/koku-service-operator` |
| API group | `service.costmanagement.openshift.io/v1alpha1` |
| Kind | `CostManagementServiceConfig` |
| Short name | `cmsc` |
| Field manager | `koku-service-operator` |
| Leader election ID | `costmanagementserviceconfigs.service.costmanagement.openshift.io` |
| Finalizer | `costmanagementserviceconfigs.service.costmanagement.openshift.io/cleanup` |
| Install model | OwnNamespace — operator NS == CR NS; see [docs/development/ownnamespace.md](docs/development/ownnamespace.md) |

## Build and test

```bash
make generate          # regenerate deep-copy methods
make manifests         # regenerate CRD YAML + RBAC ClusterRole
make build             # compile manager binary to bin/manager
NAMESPACE=<cr-ns> make run   # run locally (OwnNamespace requires NAMESPACE)
make test              # generate, fmt, vet, setup-envtest, run all tests with coverage
golangci-lint run ./...
govulncheck ./...
```

The `bin/controller-gen` binary is checked in (v0.18.0 arm64). CI
(`.github/workflows/ci.yml`) runs lint, govulncheck, build, `make test`,
generated-file drift checks, link checks, container build, and Kind e2e.

`make test` automatically runs the `setup-envtest` target which downloads
etcd + kube-apiserver binaries needed by the controller integration tests
(envtest). Without these, `internal/controller` tests fail with
"no such file or directory: etcd".

## Production design target

**All infrastructure is external (BYOI).** In production, PostgreSQL, Kafka,
and object storage are the customer's responsibility. The operator connects to
them via connection details and `credentialsSecretRef` fields in the CR.

The `database.deploy: true` and `cache.deploy: true` options exist **for
local development and CI only** — no HA, no backup, no day-2 operations.
Kafka cannot be bundled at all (always AMQ Streams). Do not design features
or reconciler logic around the bundled path.

See [docs/design/design-vs-jira.md](docs/design/design-vs-jira.md) for the full rationale.

## Reconciler architecture

The controller uses a phase-gated pipeline. The ordered list of `PhaseFn`
calls lives in the `reconcile()` method in
`internal/controller/costmanagementserviceconfig_controller.go` — look at the
`runPhases([]PhaseFn{...})` call for the authoritative stage order.

Key conventions:

- **Server-Side Apply (SSA)** with `ForceOwnership` is used for all
  namespace-scoped resources (`r.apply()`). This means the operator owns
  every field it sets and will revert manual edits on the next reconcile.
- **Secrets are create-only** — `r.ensureSecret()` creates if absent but
  never overwrites. This preserves generated credentials across reconciles.
- **Cluster-scoped resources** (ConsoleLink, Kruize ClusterRole/Binding)
  cannot use `ownerReferences` for GC. They are cleaned up by the CR
  finalizer in `reconcileDelete()`. Any new cluster-scoped resource must
  be added to that list.
- **Drift correction** — the reconciler re-applies desired state every 5
  minutes (`requeueDrift`) so manual edits to managed resources are reverted.

## Status API

Conditions are the primary API — **not** the Phase field. The three top-level
conditions follow the OpenShift/Kubernetes operator convention:

- `Available` — core functionality working
- `Progressing` — operator is actively reconciling
- `Degraded` — operator cannot make progress without intervention

Component-specific conditions (e.g. `DatabaseReady`, `CacheReady`,
`SchemaUpToDate`) go into the same `status.conditions` slice. The full list
of condition type constants is in `api/v1alpha1/costmanagementserviceconfig_types.go`.

The `Phase` field is a human-readable convenience only — not for machine
consumption. Phase values are defined by the `Phase` type in the same file.

## Key design decisions (vs JIRA spec)

Full analysis in [docs/design/design-vs-jira.md](docs/design/design-vs-jira.md). Short version:

| Decision | Why |
|----------|-----|
| Conditions over phase enum | Kubernetes API conventions; phases are linear, conditions compose |
| Bundled infra is dev-only | JIRAs are correct — production is BYOI |
| `*bool` needed for opt-out fields | `bool+omitempty+default:true` loses `false` on marshal |
| No passwords in CR spec | etcd stores CR plaintext; use Secret references |
| Finalizers required for cluster-scoped resources | `ownerReferences` don't work cross-namespace |

## PR review checklist

### Coverage analysis

Before submitting or updating any PR, run coverage on the packages touched
by the change and look for 0% functions in the diff area:

```bash
go test ./internal/resources/... -coverprofile /tmp/cover.out
go tool cover -func /tmp/cover.out
```

Adjust the package path to whatever the PR touches. If a builder/constructor
is at 0% but the pattern for testing it already exists in the same test file,
close the gap before pushing.

### LSP call hierarchy checks

Use the LSP tool (gopls) to trace call graphs on every new or modified
function:

- **Blast radius** — `incomingCalls` on any changed function to find all
  callers. Every caller is potentially affected and needs review.
- **Data flow tracing** — `outgoingCalls` from entry points on
  security-sensitive paths (CR spec fields that reach shell scripts, DB
  queries, env vars).
- **Dead code** — `incomingCalls` on every new function. Zero callers =
  dead code or forgotten wiring.
- **Coverage gap diagnosis** — `incomingCalls` to find which builder/caller
  to test, `outgoingCalls` to understand what setup the test needs.

### YAML round-trip validation

Any test that asserts on generated YAML (Envoy config, Kubernetes manifests)
must round-trip through `yaml.Unmarshal` — not just `strings.Contains`.
String matching misses silent type confusion where YAML parses without error
but produces the wrong Go types. `err == nil` alone is NOT sufficient.

### Static analysis

```bash
golangci-lint run ./...
govulncheck ./...
```

## Reference material

- [docs/development/ownnamespace.md](docs/development/ownnamespace.md) — OwnNamespace install/watch model and RBAC shape
- [docs/development/crc-testing.md](docs/development/crc-testing.md) — local development and CRC testing guide
- [docs/tasks.md](docs/tasks.md) — implementation status per JIRA ticket
- [docs/design/design-vs-jira.md](docs/design/design-vs-jira.md) — design decisions and best-practice analysis
- [docs/design/security-context-strategy.md](docs/design/security-context-strategy.md) — OpenShift SCC strategy
- [docs/design/koku-django-log-handler-problem.md](docs/design/koku-django-log-handler-problem.md) — why readOnlyRootFilesystem is blocked on koku containers
- [docs/jira/](docs/jira/) — JIRA ticket source (COST-7678–7700)
- `../cost-onprem-chart/cost-onprem/` — Helm chart this operator replaces (reference for resource shapes, env vars, volumes)
- [config/samples/byoi/README.md](config/samples/byoi/README.md) — BYOI dev fixture (PostgreSQL, Valkey, Kafka, MinIO, optional Prometheus + Grafana)
