# Review Follow-ups

Open, non-blocking items from code reviews. Closed entries are removed.
IDs are stable (gaps are intentional) so existing `#12` / `#13` links keep
working.

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
manager Deployment + bundle generation integration.

COST-7695 (bundle CSV/CRD/RBAC) is Closed; this airgap gap did not land
with it.

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

## 5. Controller does not watch StorageClasses — **accepted (wontfix)**

No StorageClass `Watches()`. Discovery re-lists the cluster default on every
reconcile; a default-SC change is picked up on the next CR event or the
5-minute drift requeue. Production BYOI sets `spec.global.storageClass` or
creates no PVCs (PVC builders read spec, not discovered status). Not worth a
cluster-scoped watch.

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
Envoy already trusts this CA (COST-7688 R2); this item is the CronJob-only
remaining gap.

---

## 10. OwnNamespace + CR finalizer: OLM uninstall still kills the manager first

**Source:** Cluster Bot pre-prod install (`hack/demo-preprod.sh --reset`), 2026-08-15.

The CR finalizer
`costmanagementserviceconfigs.service.costmanagement.openshift.io/cleanup`
deletes cluster-scoped ConsoleLink and Kruize ClusterRole/Binding, then
strips itself. That path is correct **when the CR is deleted and the
manager is still running**.

The operator lives in the **same namespace as the CR**. Deleting the
namespace (or the CSV/Deployment) first kills the manager before it can
process `deletionTimestamp`. The namespace stays `Terminating` and
ConsoleLink leaks.

**Documented:** [uninstall.md](install/uninstall.md) — delete the CR, wait
until it is gone, then the namespace or operator.
`hack/demo-preprod.sh --reset` strips the finalizer if the operator is already
gone. `scripts/install-cmsc.sh` cleanup deletes the CR first but does **not**
recover a stuck finalizer.

**Still open:** OLM uninstall that removes the CSV before the CR still hits
the trap. A bundle that deletes (or blocks on) the CR first would close it.
The CSV advertises AllNamespaces, but install still colocates operator and
CR. A webhook that blocks namespace deletion while this finalizer exists
is not worth it for beta.

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
`notReadyWait`. UI is worse than Ingress/Envoy: `UIReady=True` is set
when the OAuth client Secret exists; the UI Deployment is never
readiness-gated.

**Impact:** A stuck Ingress/Envoy/UI Deployment leaves `Progressing=True`
and requeues every 30s indefinitely. `Available` may stay `True`
`KokuAvailable` (Ingress/Envoy wait after core has already promoted it)
or keep a stale core wait reason if a later change stops promoting
`Available`. Operators never get `Degraded=True` `DeploymentNotReady`
for those components.

**Suggested fix:** Route Ingress (and optionally Envoy/UI) through
`notReadyWait` with dedicated wait reasons, same as ROS API. Add tests
that a 6-minute Ingress wait degrades and names the component. Gate UI
on Deployment readiness, not only the OAuth Secret.

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
replicas (same contract as `isStatefulSetReady`). Update
`TestIsDeploymentReady` and `markDeploymentReady`; add a stale-replica
case.

**Out of scope for PR #105** — changes every existing gate (RBAC, Koku
API, Ingress, Envoy, Valkey), not just COST-7686.

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
