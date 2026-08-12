# koku-service-operator

[![CI](https://github.com/project-koku/koku-service-operator/actions/workflows/ci.yml/badge.svg)](https://github.com/project-koku/koku-service-operator/actions/workflows/ci.yml)

Kubernetes operator for self-managed (on-premise) Cost Management on OpenShift.
Users install via OLM, apply a single `CostManagementServiceConfig` CR, and the
operator deploys and manages the full Cost Management stack against their
existing external infrastructure (PostgreSQL, Kafka, S3, OIDC).

## Documentation

| Document | Description |
|----------|-------------|
| [docs/development/crc-testing.md](docs/development/crc-testing.md) | Local development and CRC testing guide |
| [docs/tasks.md](docs/tasks.md) | Implementation status per JIRA ticket |
| [docs/design/design-vs-jira.md](docs/design/design-vs-jira.md) | Design decisions and Kubernetes best-practice analysis |
| [docs/jira/](docs/jira/) | JIRA ticket source (COST-7678–7700) |

## Quick start

```bash
make generate manifests    # regenerate CRD and deep-copy code
make build                 # compile to bin/manager
make run                   # run locally against current kubeconfig
```

See [docs/development/crc-testing.md](docs/development/crc-testing.md) for
running against a local CRC cluster.

## API / CRD naming

| Identifier | Value |
|-----------|-------|
| API group | `service.costmanagement.openshift.io` |
| Version | `v1alpha1` |
| Kind | `CostManagementServiceConfig` |
| CRD name | `costmanagementserviceconfigs.service.costmanagement.openshift.io` |
| Short name | `cmsc` |
| `apiVersion` in CR | `service.costmanagement.openshift.io/v1alpha1` |
| Operator install model | OwnNamespace — operator NS == CR NS ([docs](docs/development/ownnamespace.md)); `make deploy` scaffold NS is `koku-service-operator-system` |
| Field manager | `koku-service-operator` |
| Leader election ID | `costmanagementserviceconfigs.service.costmanagement.openshift.io` |
| Finalizer | `costmanagementserviceconfigs.service.costmanagement.openshift.io/cleanup` |

Naming parallels the companion operator:
`costmanagement-metrics-cfg.openshift.io` / `CostManagementMetricsConfig` / `cmmc`

## Project status

Early development — core reconciler stages (infrastructure, DB migration, Koku
API, Celery workers) are implemented and tested on CRC. See
[docs/tasks.md](docs/tasks.md) for current status per ticket.
