# Operator Task Tracker

Tracks implementation status against the COST-7678–7700 Jira backlog.
Last audited: 2026-08-09 against implementation in `internal/controller/` and `internal/resources/`.

## Legend
- ✅ Done — implements the ticket's acceptance criteria
- 🔄 In Progress — partially implemented, specific gaps noted
- ❌ Not started

---

## CRD & API

| Ticket | Summary | Status | Notes |
|--------|---------|--------|-------|
| [COST-7678](https://redhat.atlassian.net/browse/COST-7678) | Define CostManagement CRD types | 🔄 | `*bool` for 12 defaulted fields ✅, `metav1.Condition` replacing `ComponentStatuses` ✅, `DiscoveredConfig` in status ✅, `Profile` enum (standard only; `ha` removed until sizing maps are implemented) ✅, phase names fixed (Pending/Progressing/Ready/Degraded) ✅, Django key charset ✅. File split skipped intentionally (see design doc §3). Missing: webhooks (`defaults.go`, `validation.go`), **profile-based sizing** (`spec.profile` is accepted but not read by the reconciler — the Helm chart's 4 sizing overlays need operator equivalents; use per-component `resources` fields until then). |
| [COST-7679](https://redhat.atlassian.net/browse/COST-7679) | Create sample CRs and generate manifests | 🔄 | Bundled CR ✅, BYOI CR ✅, BYOI kustomize fixture ✅, CRD installs on CRC ✅. Missing: HA profile sample, CEL validation verified. |

## Reconciler Core

| Ticket | Summary | Status | Notes |
|--------|---------|--------|-------|
| [COST-7680](https://redhat.atlassian.net/browse/COST-7680) | Implement phase-gated reconciler skeleton | 🔄 | `runPhases()` + `PhaseError` pattern ✅, 9-stage pipeline ✅, Kubernetes Events (Ready/MigrationStarted/Complete/Failed/ReconcileError) ✅. Missing: pause/resume via annotation. |
| [COST-7681](https://redhat.atlassian.net/browse/COST-7681) | Implement Server-Side Apply and ownership model | ✅ | SSA with `ForceOwnership` ✅, `Controller: true` + `BlockOwnerDeletion: true` on ownerRefs ✅, finalizer `costmanagementserviceconfigs.service.costmanagement.openshift.io/cleanup` ✅, `reconcileDelete()` removes ConsoleLink + Kruize ClusterRole/Binding ✅, drift correction 5-min requeue ✅. |
| [COST-7682](https://redhat.atlassian.net/browse/COST-7682) | Implement cluster discovery | ✅ | Cluster domain from `config.openshift.io/v1/Ingress/cluster` ✅, default StorageClass by annotation ✅, `DiscoveryComplete` condition ✅, `status.discoveredConfig` populated ✅, user override via `spec.global.*` ✅. Tests: 11 unit tests with fake client. |
| [COST-7683](https://redhat.atlassian.net/browse/COST-7683) | Implement S3 backend auto-detection | ✅ | Three-path resolution: user `secretName` → Bound OBC → NooBaa ✅. Sets `StorageReady` condition + `status.discoveredConfig.s3` ✅. Copies OBC/NooBaa credentials into `<cr>-storage-credentials` Secret ✅. Failure does not block the pipeline. |
| [COST-7684](https://redhat.atlassian.net/browse/COST-7684) | Implement external dependency validation | ✅ | TCP probes for external DB + Cache (blocking) ✅, Kafka TCP probe (non-blocking) ✅, OIDC JWKS HTTP probe when `keycloak.url` set (non-blocking) ✅. Secret key validation for `database.secretName`, `cache.auth.secretName`, `kafka.sasl.existingSecret` ✅. Conditions: `DatabaseReady`, `CacheReady`, `KafkaReady`, `AuthenticationReady` ✅. |

## Infrastructure

| Ticket | Summary | Status | Notes |
|--------|---------|--------|-------|
| [COST-7685](https://redhat.atlassian.net/browse/COST-7685) | Implement migration Job lifecycle | ✅ | Three sequential Jobs: Koku → ROS → RBAC migrate+seed ✅. `backoffLimit: 3` ✅, `activeDeadlineSeconds: 600` ✅, `RestartPolicy: OnFailure` ✅. Upgrade detection by image tag per service ✅. Succeeded Jobs not re-run ✅. `SchemaUpToDate` condition with per-step progress messages ✅. |

## Application Services

| Ticket | Summary | Status | Notes |
|--------|---------|--------|-------|
| [COST-7686](https://redhat.atlassian.net/browse/COST-7686) | Implement application services | 🔄 | Koku API + Masu + Listener `1/1 Running` on CRC ✅, ROS API + Processor ✅ (optional via `spec.ros.enabled`), Kruize Deployment + Service + ClusterRole/Binding ✅ (gated with ROS), Bundled DB/Cache (dev-only extension) ✅. **Beta: ROS/Kruize not required** — see Beta scope below. Missing: profile-based sizing, 5-minute readiness timeout with Degraded condition. |
| [COST-7687](https://redhat.atlassian.net/browse/COST-7687) | Implement workers and scheduled jobs | ✅ | Celery Beat + 10 workers ✅, ROS Processor + Recommendation Poller + Housekeeper ✅ (when `spec.ros.enabled`), ROS Partition Cleaner CronJob ✅, Kruize DeletePartitions CronJob ✅. Ticket's six on-prem queues all present. |
| [COST-7688](https://redhat.atlassian.net/browse/COST-7688) | Implement Gateway and Ingress | ✅ | Envoy JWT proxy Deployment + Service + ConfigMap wired to OIDC issuer/audiences ✅, OpenShift Route for gateway API ✅, insights-ingress-go Deployment + Service ✅ (S3/Kafka/CA wiring, port 8080 matching Envoy backend config). |
| [COST-7689](https://redhat.atlassian.net/browse/COST-7689) | Implement RBAC Service | ✅ | RBAC API Deployment + Service + RBAC Celery worker Deployment ✅. Deployed in Stage 4 before Koku. Both wired with rbac-user/rbac-password from DB credentials secret + cache env vars. |
| [COST-7690](https://redhat.atlassian.net/browse/COST-7690) | Implement UI and ConsoleLink | ✅ | UI Deployment (oauth2-proxy sidecar + nginx app container) ✅, ClusterIP Service with OpenShift service-CA TLS annotation ✅, UINginxConfigMap (proxies `/api/` to Envoy) ✅, operator-generated cookie Secret ✅, ConsoleLink (cluster-scoped, finalizer cleanup in `reconcileDelete`) ✅. UIRoute deferred to COST-7691. |
| [COST-7691](https://redhat.atlassian.net/browse/COST-7691) | Implement Routes, NetworkPolicies, and TLS | ✅ | NetworkPolicies (gateway, ingress, kruize, rbac-api, koku-api) ✅, restricted-v2 SCC compliance (no hardcoded runAsUser, seccompProfile: RuntimeDefault) ✅, `status.phase → Ready` ✅, Gateway Route ✅, UI Route (deferred until cluster domain resolved) ✅. Dedicated SAs: koku, ros, kruize ✅. |
| [COST-7692](https://redhat.atlassian.net/browse/COST-7692) | Implement monitoring and alerting | ✅ | Kubernetes Events (Ready, MigrationStarted/Complete/Failed, ReconcileError) ✅, AppServiceMonitor + KruizeServiceMonitor ✅, PrometheusRules (5 alert rules) ✅. All guarded by `spec.monitoring.enabled`. ServiceMonitors/Rules applied as unstructured — silently skipped when Prometheus Operator CRDs absent. |

## Lifecycle

| Ticket | Summary | Status | Notes |
|--------|---------|--------|-------|
| [COST-7693](https://redhat.atlassian.net/browse/COST-7693) | Implement upgrade and scaling flows | 🔄 | Image-tag-triggered migration re-run per service ✅, SSA re-applies desired replicas ✅. Missing: automatic rollback on migration failure, rolling update strategy (maxSurge/maxUnavailable per workload type), profile-based replica scaling. |
| [COST-7694](https://redhat.atlassian.net/browse/COST-7694) | Implement secret rotation and CA management | 🔄 | CA bundle combine init container ✅, service-ca ConfigMap with OCP injection annotation ✅, Django key charset fixed (`abcdefghijklmnopqrstuvwxyz0123456789!@#$%^&*(-_=+)`) ✅. Missing: `cost.redhat.com/rotate-secrets` annotation trigger, pod template annotation rolling restart, `SecretRotated` Event. |

## OLM & CI

| Ticket | Summary | Status | Notes |
|--------|---------|--------|-------|
| [COST-7695](https://redhat.atlassian.net/browse/COST-7695) | Create OLM bundle | 🔄 | Channel `beta`, CSV base polished, generated `bundle/` validates. Makefile: `bundle`, `bundle-build`, `bundle-push`, `bundle-run`, `bundle-cleanup`. Remaining: commit/PR review; optional `minKubeVersion`. |
| [COST-7696](https://redhat.atlassian.net/browse/COST-7696) | Build CI pipeline for bundle | ❌ | GitHub Actions CI with lint/build/test/check-generated/container-build ✅. Missing: bundle validation, scorecard tests, CatalogSource, OLM install verification. |
| [COST-7697](https://redhat.atlassian.net/browse/COST-7697) | Adapt existing E2E suite for operator | ❌ | |
| [COST-7698](https://redhat.atlassian.net/browse/COST-7698) | Implement operator-specific E2E scenarios | ❌ | Unit tests for discovery, ownership, migration pipeline present. Full operator E2E not written. |
| [COST-7699](https://redhat.atlassian.net/browse/COST-7699) | Set up OpenShift CI integration | ❌ | |
| [COST-7700](https://redhat.atlassian.net/browse/COST-7700) | Write installation and configuration guides | ❌ | README, CLAUDE.md, CRC dev guide, design docs present. Formal installation/configuration/quickstart guides not written. |

---

## Beta scope

**ROS is not required for Beta.** `spec.ros.enabled` (default `true`) can be set to
`false` to skip ROS and its Kruize dependency (migrations, Deployments, CronJobs,
Envoy recommendation routes, NetworkPolicies, ServiceMonitors, cluster-scoped
Kruize RBAC). BYOI samples default this to `false`. Full ROS/Kruize delivery
remains tracked under COST-7686 / COST-7687 for post-Beta.

## Intentional Deviations and Known Gaps

See [docs/design/design-vs-jira.md](design/design-vs-jira.md) for the full analysis.
Short version: bundled infra is dev-only (intentional), profile-based sizing is not yet implemented (COST-7693 gap), `RealmUser.Password` in spec is a security issue to fix pre-GA.

---

## Technical Debt

| Item | Notes |
|------|-------|
| **waitForTCP Go binary** | Implemented in `cmd/wait-for/` using wait4x.dev/v3. See `docs/design/wait-for-patterns.md` for rationale. |
| **Image digest pinning** | Tags are mutable; pin to `tag@sha256:digest` for Dependabot tracking. Priority: before GA. See [review follow-ups](review-follow-ups.md#2-image-digest-pinning). |
| **`relatedImages` in OLM bundle** | Runtime-constructed images not in CSV `relatedImages`; breaks airgapped deployments. COST-7695. See [review follow-ups](review-follow-ups.md#3-relatedimages-in-olm-bundle-cost-7695). |
| **RBAC migration/bootstrap code provenance** | Heredoc-embedded Django ORM scripts fail code-provenance audit. Needs `insights-rbac` management commands. See [review follow-ups](review-follow-ups.md#4-rbac-migrationbootstrap-code-provenance). |
| **`ResolveBootstrapAdmin` fallback values** | Silently substitutes test-fixture IDs (`org1234567`) when CR fields are empty. Pre-existing. See [review follow-ups](review-follow-ups.md#1-resolvebootstrapadmin-silently-substitutes-test-fixture-ids). |

---

## Next Priority

1. **[COST-7695](https://redhat.atlassian.net/browse/COST-7695)** — OLM bundle generation and validation
2. **[COST-7694](https://redhat.atlassian.net/browse/COST-7694)** — Secret rotation trigger + `SecretRotated` Event
3. **[COST-7696](https://redhat.atlassian.net/browse/COST-7696)** — CI bundle pipeline (needs COST-7695 first)
4. **[COST-7678](https://redhat.atlassian.net/browse/COST-7678)** — Admission webhooks (defaults + validation)
