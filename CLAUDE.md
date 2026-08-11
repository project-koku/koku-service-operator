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

## Build

```bash
make generate          # regenerate deep-copy methods
make manifests         # regenerate CRD YAML + RBAC ClusterRole
make build             # compile manager binary to bin/manager
NAMESPACE=<cr-ns> make run   # run locally (OwnNamespace requires NAMESPACE)
go test -race ./internal/...
golangci-lint run ./...
govulncheck ./...
```

The `bin/controller-gen` binary is checked in (v0.18.0 arm64). CI
(`.github/workflows/ci.yml`) runs lint, govulncheck, build, `make test`,
generated-file drift checks, link checks, container build, and Kind e2e.

## Production design target

**All infrastructure is external (BYOI).** In production, PostgreSQL, Kafka,
and object storage are the customer's responsibility. The operator connects to
them via connection details and `credentialsSecretRef` fields in the CR.

The `database.deploy: true` and `cache.deploy: true` options exist **for
local development and CI only** — no HA, no backup, no day-2 operations.
Kafka cannot be bundled at all (always AMQ Streams). Do not design features
or reconciler logic around the bundled path.

See [docs/design/design-vs-jira.md](docs/design/design-vs-jira.md) for the full rationale.

## Reconciler stages

The controller uses a phase-gated pipeline (internal stages, not exposed as
Phase values):

1. **Shared config** — secrets (create-only), ConfigMaps, ServiceAccount
2. **Infrastructure** — validate/provision DB and cache; readiness gate
3. **Migration** — Koku schema migration Job; blocks stage 4 until complete
4. **Core services** — Koku API, Masu, Listener
5. **Workers** — Celery beat + workers, ROS, RBAC, Kruize, Ingress
6. **Edge** — Envoy gateway, UI, OpenShift Routes

Stages 1–4 are implemented and tested on CRC. Stages 5–6 are partially
implemented (Celery workers done; ROS, RBAC, Kruize, Ingress, gateway, UI
are stubs).

## Status API

Conditions are the primary API — **not** the Phase field. The three top-level
conditions follow the OpenShift/Kubernetes operator convention:

- `Available` — core functionality working
- `Progressing` — operator is actively reconciling
- `Degraded` — operator cannot make progress without intervention

Component-specific conditions (`DatabaseReady`, `CacheReady`, `SchemaUpToDate`,
etc.) go into the same `status.conditions` slice as `metav1.Condition` entries.
The `Phase` field (`Provisioning` / `Running` / `Degraded`) is a human-readable
convenience only — not for machine consumption.

The current `ComponentStatuses` struct (with `Ready bool`) is a known gap
and will be replaced by proper `metav1.Condition` entries.

## Key design decisions (vs JIRA spec)

Full analysis in [docs/design/design-vs-jira.md](docs/design/design-vs-jira.md). Short version:

| Decision | Why |
|----------|-----|
| Conditions over phase enum | Kubernetes API conventions; phases are linear, conditions compose |
| Bundled infra is dev-only | JIRAs are correct — production is BYOI |
| `*bool` needed for opt-out fields | `bool+omitempty+default:true` loses `false` on marshal |
| No passwords in CR spec | etcd stores CR plaintext; use Secret references |
| Finalizers required for cluster-scoped resources | `ownerReferences` don't work cross-namespace |

## Reference material

- [docs/development/ownnamespace.md](docs/development/ownnamespace.md) — OwnNamespace install/watch model and RBAC shape
- [docs/development/crc-testing.md](docs/development/crc-testing.md) — local development and CRC testing guide
- [docs/tasks.md](docs/tasks.md) — implementation status per JIRA ticket
- [docs/design/design-vs-jira.md](docs/design/design-vs-jira.md) — design decisions and best-practice analysis
- [docs/design/security-context-strategy.md](docs/design/security-context-strategy.md) — OpenShift SCC strategy: why no runAsUser, restricted-v2 vs anyuid, comparison with Helm chart and SaaS
- [docs/design/koku-django-log-handler-problem.md](docs/design/koku-django-log-handler-problem.md) — why readOnlyRootFilesystem is blocked on koku containers; fix required in koku
- [docs/jira/](docs/jira/) — JIRA ticket source (COST-7678–7700)
- `../cost-onprem-chart/cost-onprem/` — Helm chart this operator replaces (reference for resource shapes, env vars, volumes)
- [config/samples/byoi/README.md](config/samples/byoi/README.md) — BYOI dev fixture (PostgreSQL, Valkey, Kafka, MinIO, optional Prometheus + Grafana)
