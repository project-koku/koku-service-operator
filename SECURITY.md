# Security Policy

## Reporting a Vulnerability

Please **do not** open a public GitHub issue for security vulnerabilities.

Report privately via [GitHub Security Advisories](https://github.com/project-koku/koku-service-operator/security/advisories/new) for this repository.

Include:

- A description of the issue and its impact
- Steps to reproduce (PoC if available)
- Affected versions / commit SHAs if known

We will acknowledge receipt and work with you on a fix and coordinated disclosure timeline.

If this operator is consumed as part of a Red Hat product build, also follow [Red Hat Product Security](https://access.redhat.com/security/team/contact) processes for that product channel.

## Supported Versions

Security fixes are applied to the default branch (`main`) and to the latest released version when releases exist. Older tags are best-effort unless a release train is explicitly maintained.

## Security Model (high level)

This operator deploys and manages the on-premise Cost Management stack on OpenShift. Production design assumes **Bring Your Own Infrastructure (BYOI)**:

- PostgreSQL, Kafka, object storage, and OIDC/Keycloak (RHBK) are external.
- Credentials and client secrets belong in Kubernetes `Secret` objects referenced from the CR — **not** in `CostManagementServiceConfig` spec fields stored plaintext in etcd.
- The operator does not invent Keycloak realm users or UI OAuth client secrets; those are provisioned outside the operator (for example RHBK / `deploy-rhbk.sh`) and mirrored into the CR namespace as needed.

Dev-only fixtures under `config/samples/byoi/` intentionally use fixed, non-production passwords. Do not reuse them outside a lab cluster.

## Scope

In scope for this project:

- Operator controller code and generated manifests/RBAC
- Handling of Secret references and avoidance of secret material in the CR API
- Supply-chain hygiene for this repository (CI, dependency and secret scanning)

Out of scope (owned by adjacent components / the customer):

- Hardening of BYOI databases, Kafka, object storage, or IdP
- Application-layer vulnerabilities inside upstream images (koku, RBAC, ROS, …) — report those to the respective projects
- Cluster-wide OpenShift SCC / network policy posture beyond what the operator documents

## Automated Checks

This repository runs complementary OpenSSF-oriented workflows (non-blocking unless made required checks):

| Check | Purpose |
|-------|---------|
| Gitleaks | Detect accidental secrets in the tree / history |
| CodeQL | Static analysis of Go sources |
| OpenSSF Scorecard | Repository process / supply-chain posture |

GitHub secret scanning and push protection are also enabled on the repository.
