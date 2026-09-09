# Install and configure Cost Management (on-prem)

Customer-facing guides for the Cost Management service operator. You install
the operator, apply one `CostManagementServiceConfig` (short name `cmsc`), and
the operator deploys the application stack against **your** PostgreSQL, Kafka,
object storage, and OIDC.

**Beta.** Resource Optimization (ROS) and Kruize are not supported. Leave
`spec.ros.enabled: false` (the CRD default). These guides describe that
Cost-only path.

There is no OperatorHub catalog yet. The CSV advertises **AllNamespaces**
(suggested install NS `cost-onprem`). OwnNamespace is not supported. See
[quickstart.md](quickstart.md#install-the-operator).

| Guide | Use when |
|-------|----------|
| [Prerequisites](prerequisites.md) | You need the external services, buckets, Kafka topic, and Secret key names |
| [Keycloak](keycloak.md) | You need realm, clients, audience mappers, and JWT claims for Envoy |
| [Quickstart](quickstart.md) | Prerequisites and the operator are ready; you want a working CR in under 30 minutes |
| [Production](production.md) | You are sizing, hardening TLS, and planning backup for a lasting deploy |
| [Uninstall](uninstall.md) | You need to remove a CR, the operator, or the namespace without getting stuck in `Terminating` |
| [CMMO](cmmo.md) | You need reporting clusters to upload metrics into this instance |
| [RBAC cache flush](../operations/rbac-cache.md) | Permission changes need immediate effect (skip 300s TTL) |

Each guide is self-contained. The quickstart clock starts **after**
prerequisites and the operator are in place.

## Not these guides

Contributor and lab paths (Cluster Bot, CRC bundled Postgres, `demo-preprod.sh`)
live under [docs/development/](../development/).
