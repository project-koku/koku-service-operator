# Application image updates and migration safety

This change implements the migration safety foundation for
[COST-7693](https://redhat.atlassian.net/browse/COST-7693). It does not implement
the complete application upgrade lifecycle.

## Image selection

Application images are explicit `CostManagementServiceConfig` (CMSC) pins. Each
image requires a repository and either a tag or a valid SHA-256 digest:

```yaml
image:
  repository: quay.io/example/koku
  digest: sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
```

A digest takes precedence when both tag and digest are present. The tag is then
descriptive; editing it does not change the effective image. Legacy tag pins
remain supported, but retargeting a registry tag without changing the CMSC is
not detected. Use immutable digests for reproducible updates.

API, listener, workers, Beat and the Koku migration use the API image. Masu
defaults to that image and supports a complete explicit override. A partial Masu
override blocks reconciliation rather than silently falling back to the API.
Keep explicit API and Masu pins compatible when updating the backend.

OLM upgrades the operator binary. The replacement operator re-renders managed
resources, including helper init containers, while retaining explicit CMSC
application pins. Changing bundle examples or `relatedImages` does not update
existing CMSCs. Selecting application images automatically from an operator
release is not implemented.

## Migration gate

After shared configuration, infrastructure and dependency validation, the
controller runs Koku, optional ROS, and RBAC migrate/seed Jobs sequentially.
Application Deployments are applied only after all enabled migrations succeed.
Existing application processes continue running during migrations, so a target
must support coexistence with the running application and schema.

Completed fixed-name Jobs serve as migration receipts. The controller compares
the actual Job template with the desired execution rather than trusting its
annotations. The execution identity includes main-container image references,
commands, arguments, working directories, environment and input references,
volume mounts, direct Secret/ConfigMap volume references, and a seed revision.
Matching legacy Jobs can be reused without replaying migrations.

An executable image, script or connection-reference change invalidates its
receipt. UI-only updates and descriptive tag edits on digest pins do not.
Helper init images, resource requests, pull policy and retry settings are
excluded from execution identity. Secret/ConfigMap contents are not read or
hashed. Changes to mounted migration inputs require an explicit script/seed
revision; the RBAC builder supplies its seed revision.

Before changing any step, the controller reads every known migration Job from
the uncached API, including ROS when it has just been disabled. An unfinished,
deleting or failed Job holds the pipeline. Managed nonterminal Pods owned by a
known migration Job also hold it when the Job has already disappeared.
Replacement of a completed, mismatched Job uses foreground deletion with its
observed UID as a precondition.

These checks protect this controller's fixed-name replacement path. They do not
serialize independent CMSCs or manual clients using the same database.

## Failure and recovery

A failed migration prevents target workload rollout. The controller sets
`SchemaUpToDate=False`, `Available=False`, `Progressing=False`, `Degraded=True`
and `UpgradeFailed=True`, identifies the failing Job, exposes the migration
failure gauge, and emits warning Events once per failed Job UID. The UID remains
in the failure condition so restarts do not repeat Events for the same attempt,
and a failed recovery attempt using the same name reports its own failure.
These conditions describe reconciliation; they do not independently assess the
health of the previous release.

Changing the requested image does not erase the failed Job or automatically
retry it. Recovery requires inspecting database state and choosing a safe forward
repair or coordinated restore. Retained Jobs do not guarantee retained Pod logs;
capture logs while Pods exist or use external log retention.

After establishing a safe recovery plan, stopping remaining migration writers
and restoring the intended configuration, an administrator can delete the
failed fixed-name Job to permit another attempt. Deleting the Job alone is not
proof that a partly applied migration can be retried safely. `UpgradeFailed`
remains true during recovery and clears after all enabled migrations succeed.
Inspect the current Job state when distinguishing recovery from a new failure.
There is no automatic image rollback or reverse schema migration.

## BYOI PostgreSQL and remaining lifecycle work

Customers own production PostgreSQL engine upgrades, backups, restore and
failover. Current dependency validation checks connectivity and Secret keys;
it does not authenticate a compatibility probe or gate migrations on PostgreSQL
server version, privileges or schema history. The local `psql` client version
would not establish the connected server's compatibility.

Completing COST-7693 still requires certified release/engine requirements,
authenticated server and schema preflight, a durable frozen upgrade plan,
database exclusion across independent writers, maintenance/drain handling,
strong completion checks for every required workload, and certified recovery
paths. A surviving successful Job cannot detect an older database restored
behind the same endpoint.

Monitoring is currently the final reconcile stage: a blocked initial install
can prevent monitoring creation. Cross-namespace operator scraping, reserved
Prometheus `job`/`namespace` label collisions, durable migration logs and broader
lifecycle alerting also require follow-up work. Conditions alone do not prove
that alerts can be delivered.

## Verification scope

Regression coverage exercises digest propagation through workload builders and
both generated CRDs, matching legacy receipts, repository/script changes,
operator helper-image changes, active later steps and disabled ROS, surviving
Pods, stale informer state, API errors, foreground deletion with UID checks,
failure retention and explicit recovery.

Customer-style OLM/BYOI lab validation exercised CMSC image edits, a real forward
schema change, operator replacement with unchanged application pins, failure
alert firing/resolution, and authenticated API/ingestion checks. That evidence
does not certify release compatibility, partially committed DDL recovery,
backup/restore, cross-CMSC locking or the remaining lifecycle features above.
