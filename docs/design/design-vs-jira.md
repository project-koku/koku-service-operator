# Design Decisions vs JIRA Specification

This document explains where and why the operator implementation intentionally
diverges from the JIRA backlog (COST-7678–7700). The source tickets are in
`docs/jira/`. Where we differ, the reasoning is grounded in Kubernetes API
conventions and operator best practices — not in disagreement with the goal of
the ticket.

---

## 1. Status API: Conditions over Phase enum

### What the JIRAs specify (COST-7678, COST-7680)

A `Phase` field with a linear enum mirroring the internal reconciler pipeline:

```
Pending → Discovering → Validating → Migrating → Deploying → Ready → Degraded
```

Each phase corresponds to one reconciler stage. The phase advances
sequentially; only one phase is visible at a time.

### What we implement

The [Kubernetes API conventions][k8s-conventions] explicitly state:

> *"Think twice before adding a phase field. Conditions are generally a better
> replacement."*

The OpenShift operator standard (CVO, cluster-logging-operator, cert-manager)
uses **three top-level conditions** as the primary machine-readable API:

| Condition | Meaning |
|-----------|---------|
| `Available` | core functionality is working and the CR is serving its purpose |
| `Progressing` | the operator is actively working toward the desired state |
| `Degraded` | the operator has failed and cannot make progress without intervention |

Below these, **component-specific conditions** carry the detail:
`DatabaseReady`, `CacheReady`, `KafkaReady`, `StorageReady`,
`AuthenticationReady`, `SchemaUpToDate`, `DiscoveryComplete`.

We keep a `Phase` field — **Pending / Progressing / Ready / Degraded** —
as a human-readable convenience for `kubectl get cmsc`, not as the primary
API. It is derived from conditions, not the other way around.

### Why conditions are better than a linear phase enum

1. **Non-linear failures.** If the database is ready but S3 fails, the phase
   can only show one thing. Conditions are independent: `DatabaseReady: True`,
   `StorageReady: False`. External tools (monitoring, scripts) can react to
   each independently.

2. **Machine-queryability.** `--field-selector status.conditions[?type=Available].status=True`
   is a stable API contract. A phase enum value has no such guarantee.

3. **Activity vs outcome.** `Migrating` tells you what the operator is doing.
   `SchemaUpToDate: False` tells you what is wrong from the user's perspective.
   Kubernetes consumers care about the latter.

4. **Backward compatibility.** Renaming or reordering phase enum values is a
   breaking change. Adding a new condition type is not.

### Practical reconciliation

The reconciler has nine internal stages (Discovery, SharedConfig, Infrastructure,
Validation, Migration, CoreServices, Workers, Edge, Monitoring). They are
surfaced via the `Progressing` condition's `reason` and `message` fields, and
via Kubernetes Events emitted at key transitions.

---

## 2. Bundled Infrastructure (Dev/Test Convenience Only)

### What the JIRAs specify (COST-7678, COST-7686)

All infrastructure is external-only. The CR accepts connection details and a
`credentialsSecretRef` for each dependency (PostgreSQL, Redis/Valkey, Kafka,
S3, OIDC). The operator does not provision any infrastructure. **The JIRAs
are correct for the production target.**

### What we implement

The CRD has a `deploy: true` option for database and cache only:

```yaml
database:
  deploy: true   # TESTING ONLY — provisions a bare PostgreSQL StatefulSet
  deploy: false  # PRODUCTION — connects to an external instance
```

This is a **developer and CI convenience** only — no HA, no backup, no
day-2 operations. Kafka cannot be bundled at all (AMQ Streams is always
external). See `CLAUDE.md` for the full rationale.

---

## 3. CRD File Structure

### What the JIRAs specify (COST-7678)

Types split across six files including `defaults.go` and `validation.go`
for admission webhooks.

### What we implement

A single `costmanagementserviceconfig_types.go`. The file split is
intentionally deferred — Go files in a package are semantically identical and
the file is not yet large enough to warrant splitting.

`defaults.go` and `validation.go` will be added when admission webhooks are
implemented (still pending).

---

## 4. Known Remaining Gaps

These items are implementation gaps — not intentional design decisions. They
will be addressed in upcoming tickets.

### 4a. `RealmUser.Password` is dead code in the operator

`RealmUser` is used by `ResolveBootstrapAdmin()` to seed the RBAC admin
identity (`Username`, `OrgID`, `AccountNumber`) into the RBAC Job. The
`Password` field is present in the type but **the operator never reads or uses
it** — Keycloak user provisioning is done by the external `deploy-rhbk.sh`
install script, not the operator.

The `Password` field should be **removed from the CRD** entirely:
- It appears in `kubectl get cmsc -o yaml` and etcd (a security exposure for
  no benefit since the operator ignores it)
- It creates a false expectation that the operator provisions Keycloak users
- The Helm chart equivalent (`jwtAuth.realmUsers[].password`) is consumed by
  `deploy-rhbk.sh` — this is out of scope for the operator

The remaining `RealmUsers` fields (`Username`, `OrgID`, `AccountNumber`,
`OrgAdmin`) are used and should stay.

### 4b. `Env map[string]string` raw override

`KokuAPISpec.Env`, `MasuSpec.Env`, and `ListenerSpec.Env` allow arbitrary
environment variables in the CR. Sensitive values could be stored in etcd
unencrypted. **Fix:** Expose only typed fields for known configuration; rename
to `additionalEnv` and validate that sensitive key names are rejected via
webhook.

### 4c. StatefulSet update bypasses SSA

The PostgreSQL StatefulSet uses `r.Update` with a partial patch rather than
Server-Side Apply, because `VolumeClaimTemplates` are immutable. Changes to
init containers, volumes, or security context are silently ignored on
subsequent reconciliations. **Fix:** Apply only mutable spec fields via SSA;
detect VCT changes separately and surface as a condition.

### 4d. `MergeEnv` deduplication ✅ Fixed

`MergeEnv` was sorting and deduplicating overrides. This is now done — keys
are sorted for stable SSA apply and duplicates replaced rather than appended.

---

## Summary

| Topic | JIRA spec | Our implementation | Status |
|-------|-----------|--------------------|--------|
| Status primary API | Phase enum (linear) | Conditions (composable) + Phase as convenience | ✅ Intentional — Kubernetes best practices |
| Phase names | Discovering/Validating/Migrating/Deploying/Ready | Pending/Progressing/Ready/Degraded | ✅ Fixed |
| Bundled infra | External-only (correct for production) | `deploy: true` for dev/CI only | ✅ Intentional — testing convenience |
| CRD file split | 6 files + webhooks | Single file, webhooks pending | 🔄 Deferred |
| Migration scope | Koku + ROS + RBAC sequential | All three Jobs implemented | ✅ Done (COST-7685) |
| Migration `backoffLimit` | 3 | 3 | ✅ Done |
| Migration `activeDeadlineSeconds` | 600 | 600 | ✅ Done |
| Django key charset | `a-z0-9!@#$%^&*(-_=+)` | Correct charset | ✅ Done (PR #16) |
| `*bool` for defaulted fields | — | Used throughout | ✅ Done |
| Conditions over ComponentStatus | `metav1.Condition` | `metav1.Condition` used | ✅ Done |
| `RealmUser.Password` | — | Present in spec but unused by operator; operator never provisions Keycloak users | ❌ Should be removed (§4a) |
| `Env map[string]string` | — | Still in spec | ❌ Gap (§4b) |
| OwnerReference Controller+BlockOwnerDeletion | required | Set correctly | ✅ Done |
| Finalizer for cluster-scoped resources | required (COST-7681) | `costmanagementserviceconfigs.service.costmanagement.openshift.io/cleanup` | ✅ Done |
| StatefulSet via SSA | SSA preferred | `r.Update` partial patch | ❌ Gap (§4c) |
| Periodic requeue / drift correction | 5 min (COST-7681) | `requeueDrift = 5 * time.Minute` | ✅ Done |
| `MergeEnv` deduplication | — | Fixed — sorted keys, dedup on write | ✅ Done (§4d) |

[k8s-conventions]: https://github.com/kubernetes/community/blob/master/contributors/devel/sig-architecture/api-conventions.md#typical-status-properties
