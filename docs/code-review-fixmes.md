# Code Review — Open Issues (2026-08-15)

Stable IDs in brackets (`[D1]`, `[F1]`, `[I7]`) come from the 2026-08-09/10
reviews this file replaces. **D** = defect, **F** = finding, **I** =
improvement. This list is **open items only**; closed IDs still appear in
gap-analysis notes (for example D11, D18, D20, F3, F4).

### P1 — Status / lifecycle

#### 1. Ready ignores Celery workload health [D1, partial]

Most workloads are now readiness-gated before `Available=True` /
`AllComponentsReady`: bundled Database StatefulSet and Cache, RBAC API (`RBACReady`,
COST-7689 G2), Koku API, Masu, Listener, Kruize and ROS API/Processor
(`notReadyWait`, COST-7686 G3, `727ae04`), Ingress (`IngressReady`,
`81e9240`) and Envoy (`GatewayReady`). The RBAC worker is reported separately
(`RBACWorkerReady`) and intentionally does not gate `Available`. The UI
Deployment will be gated by COST-8103 (PR #198, in review).

**Still open:** Celery beat and the Celery worker Deployments are applied
without readiness checks — a bad image there leaves the CR reporting
`Available=True`. Gating them should follow the Available / Progressing /
Degraded semantics follow-up rather than add more `Available=False` waits.

#### 2. Progressing status causes a continuous reconcile loop [D2] — **closed**

Every successful pass transitioned `Progressing` True → False. Each transition
refreshed `LastTransitionTime`, so the status patch differed from what was read.
No generation-change predicate on the primary watch, so the status update
re-enqueued immediately (measured: ~46 passes/min on an idle CR). Fixed in
[COST-8517](https://redhat.atlassian.net/browse/COST-8517): a pass that starts
and ends at `Progressing=False` keeps the prior `LastTransitionTime`, so the
steady-state status patch is empty.

#### 3. Failed OIDC check overwritten with success [D3] — **closed**

Edge writes `GatewayReady`. Validation keeps `AuthenticationReady` for the
JWKS probe. See [COST-7688](gap_analysis/COST-7688.md).

#### 4. Rollout readiness accepts stale replicas [D14] — **closed**

`isDeploymentReady` now requires `ObservedGeneration >= Generation`,
`UpdatedReplicas >= spec.replicas` (#176) and no old pods left
(`status.replicas <= updatedReplicas`, COST-8518) before
`AvailableReplicas >= spec.replicas` counts. Previously a crash-looping new
pod plus a serving old pod passed the gate.

### P1 — Resources

#### 5. ConsoleLink name collides across namespaces [F1, partial]

Kruize ClusterRole/Binding names now include a namespace hash — fixed. But
`NameConsoleLink` still uses only `cfg.Name + "-cost-management"`. Two CRs
with the same name in different namespaces collide. Finalizer deletion still
does not verify ownership before deleting cluster-scoped resources.

### P2 — Data integrity

#### 6. Migration identity is image-tag only [D4]

Migration Job completion is keyed on the image tag annotation alone. Changing
the database host, credentials Secret, or image repository while keeping the
same tag skips migrations. Applications start against a potentially empty
database.

### P2 — Validation / discovery

#### 7. BYOI deployments require a default StorageClass [D21] — **closed**

Discovery requires a StorageClass only when bundled PostgreSQL or Valkey
create PVCs (`database.deploy` / `cache.deploy`). A fully external install
records the default StorageClass when one exists and otherwise continues
(COST-8521), as [prerequisites](install/prerequisites.md#openshift) states.

### P2 — API contract

#### 8. Accepted Route TLS modes cannot reach plaintext Envoy [D19] — **closed**

`RouteTLSSpec.Termination` is now `+kubebuilder:validation:Enum=edge` (default
`edge`), so the CRD rejects `passthrough` / `reencrypt` that a plaintext Envoy
listener cannot serve.

#### 9. Zero replicas are silently overridden to 1 [D13] — **closed**

`ProfileReplicas` returns an explicit `replicas` value as-is, including 0;
only an unset field falls back to the profile / default (COST-8095, #195).
Covered by `TestKokuAPIDeployment_ExplicitZeroReplicas`. `spec.profile` is no
longer a no-op (#195).

### P2 — Monitoring

#### 10. ServiceMonitors cannot scrape most targets [D17] — **closed**

Koku API, Masu and Ingress Services expose a dedicated `metrics` port scraped
by the app ServiceMonitor (`df053d8`, `72ba854`); Celery workers have their
own `metrics` port and ServiceMonitor; RBAC API is scraped on `http`
(#144). The Kruize ServiceMonitor (`http`, `/q/metrics`) is defined but only
used for cleanup until ROS scrape lands (COST-8054). The Listener exposes no
ports and has no metrics endpoint, so it has nothing to scrape.

### P3 — Operational

#### 11. No watches on Routes, ServiceMonitors, ClusterRoles [I7]

`Owns()` covers core resources but not Routes, ServiceMonitors,
PrometheusRules, or cluster-scoped resources. External deletion relies on the
5-minute drift requeue. Needs discovery-gated dynamic watches.

#### 12. Long CR names make generated resources invalid [D22] — **closed**

The CRD rejects `metadata.name` longer than 33 characters (27 with
`spec.ros.enabled`) via a root CEL rule — the longest generated CronJob names
(`-rbac-keycloak-sync`, ROS `-kruize-delete-partitions`) must fit 52
characters (COST-8521). `TestMaxCRNameFitsGeneratedNames` proves the caps are
tight. Route host DNS labels also depend on the namespace and are documented
in [prerequisites](install/prerequisites.md#cr-name-length), not enforced.

