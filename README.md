# koku-service-operator

[![CI](https://github.com/project-koku/koku-service-operator/actions/workflows/ci.yml/badge.svg)](https://github.com/project-koku/koku-service-operator/actions/workflows/ci.yml)

Kubernetes operator for self-managed (on-premise) Cost Management on OpenShift.
Users install via OLM, apply a single `CostManagementServiceConfig` CR, and the
operator deploys and manages the full Cost Management stack against their
existing external infrastructure (PostgreSQL, Kafka, S3, OIDC).

## Documentation

| Document | Description |
|----------|-------------|
| [CONTRIBUTING.md](CONTRIBUTING.md) | Branch/PR naming with Jira (`COST-####`) |
| [docs/install/](docs/install/) | **Install and configure** (prerequisites, quickstart, production, CMMO) |
| [docs/development/clusterbot.md](docs/development/clusterbot.md) | Cluster Bot day-one: Redpanda BYOI → in-cluster operator |
| [docs/development/pre-prod-install.md](docs/development/pre-prod-install.md) | Pre-prod BYOI → operator → UI install walkthrough |
| [docs/development/allnamespaces.md](docs/development/allnamespaces.md) | AllNamespaces install/watch model and RBAC shape |
| [docs/development/crc-testing.md](docs/development/crc-testing.md) | Local development and CRC testing guide |
| [docs/development/olm-bundle-testing.md](docs/development/olm-bundle-testing.md) | Build/push/run OLM bundle via `operator-sdk run bundle` |
| [config/samples/byoi/README.md](config/samples/byoi/README.md) | BYOI fixture (Postgres, Valkey, Kafka, MinIO, OAuth mirror) |
| [docs/tasks.md](docs/tasks.md) | Implementation status per JIRA ticket |
| [docs/code-review-fixmes.md](docs/code-review-fixmes.md) | Open code review issues |
| [docs/design/design-vs-jira.md](docs/design/design-vs-jira.md) | Design decisions and Kubernetes best-practice analysis |
| [docs/jira/](docs/jira/) | JIRA ticket source (COST-7678–7700) |

## Quick start

Customer install (BYOI, `ros.enabled: false`): [docs/install/quickstart.md](docs/install/quickstart.md).
Prerequisites and Secret keys: [docs/install/prerequisites.md](docs/install/prerequisites.md).

Contributor local loop:

```bash
make generate manifests    # regenerate CRD and deep-copy code
make build                 # compile to bin/manager
NAMESPACE=<cr-ns> IMG=<operator-image> make run # local (pins cache; requires IMG)
```

See [docs/development/clusterbot.md](docs/development/clusterbot.md) for Cluster Bot,
[docs/development/crc-testing.md](docs/development/crc-testing.md) for CRC, or
[docs/development/pre-prod-install.md](docs/development/pre-prod-install.md)
for a full in-cluster BYOI + UI smoke.

**CRD:** `service.costmanagement.openshift.io/v1alpha1` — Kind `CostManagementServiceConfig` (short name `cmsc`).
Installs in AllNamespaces mode ([docs](docs/development/allnamespaces.md)).
OwnNamespace is not supported.

## Project status

Core reconciler, all application services (Koku, RBAC, ROS, Kruize, Ingress,
Envoy, UI), Keycloak sync, BYOI fixtures, and multiple install paths (CRC,
Cluster Bot, pre-prod) are implemented and tested. See
[docs/tasks.md](docs/tasks.md) for per-ticket status and
[docs/code-review-fixmes.md](docs/code-review-fixmes.md) for open issues.
