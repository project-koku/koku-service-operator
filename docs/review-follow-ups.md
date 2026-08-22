# Review Follow-ups

Non-blocking follow-up items from code reviews. Each item is pre-existing
or deferred — not a blocker for the PR that surfaced it.

---

## 1. ~~`ResolveBootstrapAdmin` silently substitutes test-fixture IDs~~ — **CLOSED**

**Source:** [inline comment on migration.go:359](https://github.com/project-koku/koku-service-operator/pull/22#discussion_r2940893206)

**Fixed:** `ResolveBootstrapAdmin` no longer exists. `AdminBootstrapJob`
now reads org-id/account-number from a Secret via `EnvFromSecret` and
returns `nil` when `secretRef.name` is empty. No fallback to
`org1234567`/`7890123`. The code path is gated by
`ba.Enabled && ba.SecretRef.Name != ""`.

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

---

## 5. Controller does not watch StorageClasses

**Source:** [PR #34 review](https://github.com/project-koku/koku-service-operator/pull/34) — pre-existing, not introduced by the OwnNamespace change.

**Problem:** The discovery phase reads the default StorageClass via
`discoverDefaultStorageClass()` (a one-shot `List` call), but the
controller's `SetupWithManager` does not `Watches()` StorageClass
resources. If the cluster's default StorageClass changes after initial
discovery, the operator won't notice until the next 5-minute drift
requeue — and even then, `reconcileDiscovery` only runs the StorageClass
lookup when `spec.global.storageClass` is empty AND
`status.discoveredConfig.storageClass` is empty, so it won't pick up
a change to an already-discovered value.

**Impact:** Low. StorageClass changes are rare day-2 events, and the
operator only uses the discovered StorageClass for bundled (dev-only)
PVCs. Production (BYOI) users set `spec.global.storageClass` explicitly.

**Suggested fix:** Add a cluster-scoped `Watches()` on StorageClasses
with a `GenerationChangedPredicate` filter, or accept as low-priority
given the dev-only use case.

---

## 6. ~~Add manifest test for role.yaml scope~~ — **CLOSED**

**Source:** [PR #34 review — jordigilh](https://github.com/project-koku/koku-service-operator/pull/34#discussion_r2948488270)

**Fixed:** `TestManagerRole_NoClusterScopedResources` parses `role.yaml`
and CSV namespaced `permissions` and fails if they grant `consolelinks`,
`clusterroles`, `clusterrolebindings`, `storageclasses`,
`config.openshift.io/ingresses`, a `noobaa-admin` resourceName, or a
`resources: ["*"]` grant on those API groups. Those rules belong in
`cluster_access_role.yaml`.

---

## 7. Keycloak sync: mount CA bundle for private-CA Keycloak

**Source:** Code review finding.

**Problem:** The Keycloak sync CronJob (now implemented in [PR #53](https://github.com/project-koku/koku-service-operator/pull/53)) verifies
TLS against Keycloak. If Keycloak uses a private CA (common on-prem),
`ssl.create_default_context()` won't trust it. `KEYCLOAK_TLS_VERIFY` follows
`auth.keycloak.tls.insecureSkipVerify`, but `auth.keycloak.tls.caCertSecretName`
is not mounted into the sync CronJob and the script has no `SSL_CERT_FILE`
override.

The Helm chart's CronJob template has the same gap.

**Impact:** Private-CA Keycloak with `insecureSkipVerify=false` (the secure
default) → CronJob auth failures. Users must set `insecureSkipVerify=true`
as a workaround, defeating TLS verification.

**Suggested fix:** Mount the Keycloak CA Secret (or the combined CA bundle
from CACombineInitContainer) and set `SSL_CERT_FILE` in the container env.
Follow the same pattern used by the Envoy gateway and oauth2-proxy.

---

## 8. ~~Keycloak sync: enable/disable lifecycle test~~ — **CLOSED**

**Source:** Code review finding.

**Fixed:** `TestKeycloakSyncDeletedWhenDisabled` exists at
`internal/controller/keycloak_sync_disable_test.go:21`, following the
same pattern as `TestKruizeCronJobDeletedWhenDisabled`.

---

## 9. S3 TLS fields are probe-only — not wired to app pods

**Source:** Code review of PR #71 (validation follow-ups).

**Problem:** `spec.objectStorage.insecureSkipVerify` and
`spec.objectStorage.caCertSecretName` configure the operator's own
`ListBuckets` validation probe, but are not wired to application pod
env vars (`AWS_CA_BUNDLE`) or volume mounts. App pods that need to
reach the same S3 endpoint with a private CA rely on the combined CA
bundle from `CACombineInitContainer`, which does not read the
objectStorage TLS fields.

The Keycloak and Cache TLS equivalents (`auth.keycloak.tls`,
`cache.tls`) are wired to both the operator probe and the app
containers. S3 should follow the same pattern.

**Impact:** A user sets `caCertSecretName` expecting it to fix S3
connectivity for both the operator condition and the running workloads.
The `StorageReady` condition turns green, but uploads still fail if
the app pods don't have the CA in their trust store via another path.

**Suggested fix:** Wire `objectStorage.caCertSecretName` into the
`CACombineInitContainer` inputs (or set `AWS_CA_BUNDLE` env var on
Koku/Masu containers) so app pods trust the same CA.

---

## 10. OwnNamespace + CR finalizer: namespace delete sticks in Terminating

**Source:** Cluster Bot pre-prod install (`hack/demo-preprod.sh --reset`), 2026-08-15.
Cluster `chat-bot-jyn4z-xta9nw`; namespace `cost-byoi` stayed `Terminating` after
`kubectl delete ns`.

**Problem:** The CR finalizer
`costmanagementserviceconfigs.service.costmanagement.openshift.io/cleanup`
is required: ConsoleLink and Kruize ClusterRole/Binding are cluster-scoped, so
they cannot use `ownerReferences`. `reconcileDelete()` deletes those objects
then strips the finalizer. That path is correct **when the CR is deleted and
the manager is still running**.

OwnNamespace puts the manager Deployment in the **same namespace as the CR**.
Deleting the namespace (or tearing down the operator CSV/Deployment first)
kills the operator pod before it can process the CR's `deletionTimestamp`.
The finalizer is never removed.

Observed on clusterbot:

- Namespace condition `NamespaceFinalizersRemaining`:
  `costmanagementserviceconfigs.service.costmanagement.openshift.io/cleanup`
  on 1 resource
- `NamespaceContentRemaining`: the CMSC instance still present
- Operator pods already gone (`No resources found`)
- ConsoleLink `cost-management-cost-management` still present (cluster leak)

Unstick: patch `metadata.finalizers: []` on the CMSC and delete the ConsoleLink.
The namespace then finished terminating in seconds.

**Impact:** Medium for lab/uninstall. Any `oc delete ns <cr-ns>`, Cluster Bot
teardown, or OLM uninstall that removes the operator before the CR will leave
the namespace stuck and leak ConsoleLink (and Kruize cluster RBAC if ROS was
enabled). Production uninstall without a documented CR-first procedure hits
the same trap. Not a bug in `reconcileDelete` itself.

**Suggested fix (in order):**

1. **Document uninstall order** (pre-prod / ownnamespace / OLM): delete the
   `CostManagementServiceConfig`, wait until it is gone, *then* delete the
   namespace or the operator. Lab `--reset` must do the same while the
   operator is still Available; only strip the finalizer if the operator is
   already gone.
2. **OLM / COST-7695:** uninstall bundle should delete (or block on) the CR
   before removing the CSV. AllNamespaces / a dedicated operator namespace
   would also avoid killing the manager when the operand namespace is deleted.
3. Optional hardening (probably not worth it for beta): a webhook that warns
   or blocks namespace deletion while a CMSC with this finalizer exists.

**Not a COST-7688 gap** — surfaced while exercising the pre-prod path on this
branch.

---

## 11. ~~`jwksProbe` ignores `caCertSecretName` — false `AuthenticationReady: False`~~ — **CLOSED**

**Source:** PR #74 review (Jordi)

**Fixed:** `reconcileValidation` now delegates Keycloak CA loading to
`keycloakCACertPool`, which reads `ca.crt` from
`auth.keycloak.tls.caCertSecretName` and passes a custom `x509.CertPool` to
`jwksProbe`. Missing or invalid CA data reports `OIDCCACertInvalid`. When
`insecureSkipVerify=true`, the operator skips CA Secret loading and honors the
explicit insecure setting.

---

## 12. Ingress, Envoy, and UI skip the 5-minute `DeploymentNotReady` timeout

**Source:** [PR #105 CodeRabbit](https://github.com/project-koku/koku-service-operator/pull/105) (COST-7686 G2).

**Problem:** `notReadyWait` (5-minute `Available=False` clock →
`Degraded=True` reason `DeploymentNotReady` + backoff) is used for Koku
API, Masu, Listener, Kruize (when ROS is on), ROS API, and ROS
Processor. Ingress, Envoy, and UI still requeue on a component condition
only (`IngressReady` / `GatewayReady` / `UIReady`) and never start that
clock.

Kruize is **not** in this gap — it is on the core wait list and uses
`notReadyWait`.

**Impact:** A stuck Ingress/Envoy/UI Deployment leaves `Progressing=True`
and requeues every 30s indefinitely. `Available` may stay `True`
`KokuAvailable` (Ingress/Envoy wait after core has already promoted it)
or keep a stale core wait reason if a later change stops promoting
`Available`. Operators never get `Degraded=True` `DeploymentNotReady`
for those components.

**Suggested fix:** Route Ingress (and optionally Envoy/UI) through
`notReadyWait` with dedicated wait reasons, same as ROS API. Add tests
that a 6-minute Ingress wait degrades and names the component.

**Out of scope for PR #105** — COST-7686 G2/G3 cover the ticket-owned
app Deployments (Koku API, Masu, Listener, optional Kruize/ROS). Ingress
is COST-7687; Envoy/UI are COST-7688.

---

## 13. `isDeploymentReady` accepts stale replicas during a rollout

**Source:** [PR #105 CodeRabbit](https://github.com/project-koku/koku-service-operator/pull/105); already tracked as [D14](code-review-fixmes.md) in `docs/code-review-fixmes.md`.

**Problem:** `isDeploymentReady` is `AvailableReplicas >= spec.replicas`
(or 0 replicas). It does not require `status.observedGeneration >=
metadata.generation` or `status.updatedReplicas >= spec.replicas`. After
an image/spec change, old ready pods still satisfy the gate.

**Impact:** The CR can go `Available=True` / `AllComponentsReady` while
the current ReplicaSet has zero available pods. Pre-existing; this PR
only added more callers (Masu, Listener, Kruize, ROS API/Processor).

**Suggested fix:** Require observed generation + updated + available
replicas. Update `TestIsDeploymentReady` and `markDeploymentReady`; add
a stale-replica case.

**Out of scope for PR #105** — changes every existing gate (RBAC, Koku
API, Ingress, Envoy, Valkey), not just COST-7686.

---

## 14. No test that ROS Processor alone blocks worker readiness — **closed in PR #105**

**Source:** [PR #105 CodeRabbit](https://github.com/project-koku/koku-service-operator/pull/105); João's [request-changes review](https://github.com/project-koku/koku-service-operator/pull/105#issuecomment-5354975468).

**Closed:** `TestReconcileWorkers_ROSProcessorNotReady_BlocksProgress` marks Ingress + ROS API ready, leaves Processor down, and asserts `Available=False` `WaitingForROSProcessor`.

---

## 15. RBAC API wait has no 5-minute `DeploymentNotReady` timeout

**Source:** João's [PR #105 review](https://github.com/project-koku/koku-service-operator/pull/105#issuecomment-5354975468).

**Problem:** `reconcileCoreServices` still gates the RBAC API with a constant
`requeueSlow` (30s). It sets `Available=False` `WaitingForRBAC` but never
calls `notReadyWait`, so a stuck RBAC API never becomes
`Degraded=True` `DeploymentNotReady` and never backs off.

**Impact:** Same shape as Ingress/Envoy in #12: the CR stays
`Progressing` and requeues every 30s forever. COST-7689 closed the
`RBACReady` gate; it did not add the 5-minute named-component Degraded
clock that COST-7686 added for Koku/Masu/Listener/ROS.

**Suggested fix:** Route the RBAC API wait through `notReadyWait` (same
reasons `WaitingForRBAC` / component `"RBAC API"`). Leave the RBAC
worker as condition-only (it does not block `Available` today).

**Out of scope for PR #105** — COST-7689 leftover, not a COST-7686 AC.
