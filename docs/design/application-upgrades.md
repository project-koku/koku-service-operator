# Application upgrades and recovery (COST-7693)

The controller manages application upgrades from CMSC image edits and operator
release changes. Production PostgreSQL upgrades, backups and restores remain
customer-owned. The operator never reverses schema migrations or upgrades the
PostgreSQL engine.

## Image ownership

`spec.upgrade.imagePolicy` has two modes:

- **Pinned** (default): all enabled application images are explicit CMSC pins.
  Operator replacement preserves them and reconciles updated workload templates.
- **FollowOperator**: omitted application images resolve from the operator's
  embedded `internal/release/images.json`. Complete explicit pins win. Omitted
  Masu inherits the effective Koku image; partial overrides fail closed.

References accept a repository plus tag or SHA-256 digest. A digest takes
precedence over a descriptive tag. The controller resolves defaults in a copy;
it never writes them into the requested spec. Changing the operator's bundle can
therefore change the target without a CMSC generation change.

Automatic release transitions require a declared source bundle in
`internal/release/compatible-from.json` and a complete shipped application
combination. A bundle ID does not certify custom overrides. Unknown source
releases and untested mixtures block before mutation. Administrators can retain
all explicit pins and select their own compatible target instead.

Release authors update the image manifest and supported source IDs together,
validate the release pair, and regenerate the bundle. OLM `relatedImages`
contains the same shipped operand references for catalog/mirroring tooling.
Development database/cache images are outside the application bundle.

## Persisted upgrade operation

A healthy reconciliation records the installed application revision. Status
contains resolved images, migration execution identities, discovered connection
references, and a frozen spec. Secret objects are never copied into status.
Literal environment values supplied in the spec retain the CMSC's access boundary.

Before changing children, the controller checkpoints an operation containing the
source, frozen target, target generation, stage, timestamps and failure state.
A failed status write cannot launch an unjournaled migration. Mid-flight edits
remain pending; restart resumes the recorded target.

Existing installations are adopted only when their images and successful
migration receipts match the requested configuration. If an upgrade was requested
before a baseline could be recorded, restore the installed pins/configuration,
let reconciliation record the baseline, then request the upgrade.

The execution sequence is:

```text
Plan -> [Quiesce -> Migrate] -> Rollout -> Ready
                      failure -> Restore source images
                                -> RolledBack (safe to resume)
                                -> RecoveryRequired (writers remain stopped)
```

UI-only changes and other updates with matching migration receipts use rolling
rollout directly. Migration-bearing changes use maintenance: suspend managed
CronJobs (resumed by the target rollout), scale application Deployments to zero, and wait for application Pods
and earlier migration writers to stop. Database/cache infrastructure remains up.
Target shared configuration is applied after quiescence.

Koku, optional ROS and RBAC migrate/seed Jobs run sequentially. The controller
uses uncached safety reads, retains unfinished/failed Jobs, waits for surviving
Pods, and foreground-deletes completed mismatched receipts with UID preconditions.
Migration identity includes image, command, database/input references and seed
revision. Runtime-only settings (object storage, cache, Kafka, schedules, retention,
logging) and helper-image changes do not invalidate successful migrations; they roll
out without maintenance. Unknown environment inputs still count.
Mounted migration input changes require a script/seed revision.
If a replacement operator changes those execution inputs during a saved
operation, execution stops before target ConfigMaps or Jobs are changed. The
operation enters rollback/recovery; an explicit Retry creates a new plan using
the replacement operator's inputs.

## Automatic rollback and explicit recovery

Before creating a migration, status records that schema mutation is possible.
This marker survives Job deletion, missing Pod logs and controller restart.

On failure, the operator restores the previous application image/configuration
references without changing the requested CMSC pins. Subsequent reconciles keep
that restored state, rather than immediately reapplying the failed target.

- If no migration could have executed, the operator resumes and verifies the
  previous release automatically. `Available=True` can coexist with
  `UpgradeFailed=True`: the source works, but the requested target failed.
- If a migration may have committed changes, previous images are restored with
  replicas held at zero. `RecoveryRequired=True` prevents old processes from
  running against an unknown schema. Reverting image tags is not schema recovery.

After inspecting/repairing the database, submit a recovery request bound to the
failed operation. Use a new request ID for each deliberate attempt:

```yaml
spec:
  upgrade:
    recovery:
      operationID: <status.upgrade.operation.id>
      requestID: repair-attempt-1
      action: Retry
```

**Retry** accepts the currently requested target, retains the original source
checkpoint, and removes failed migration Jobs only after writer exclusion. A
forward-fix image can be selected before submitting the request.

**ResumePrevious** is an explicit administrator confirmation that external
repair/restore made the recorded source configuration and connection references
safe again. It resumes that source without reverse migrations. A restored
endpoint with different bindings can instead be supplied as an explicit Retry
target. An image edit or operator restart alone never clears a rollback or
recovery hold; submit Retry when ready to attempt another target.

`spec.upgrade.timeoutSeconds` defaults to 7200. Expiry requests rollback, but
never force-deletes active migrations or terminating Pods. Inspect remaining
writers if rollback is waiting. This deadline covers drain and rollout readiness;
the ordinary readiness timeout does not prematurely abort a journaled upgrade.
First-install migration failure retains its Job;
there is no previous installed application revision to restore.

## Rollout and completion

All application Deployment builders set explicit strategies:

- Stateless services: `maxSurge: 25%`, `maxUnavailable: 25%`.
- Workers, listener and Beat: `maxSurge: 0`, `maxUnavailable: 1`.

Celery commands use `exec` so SIGTERM reaches the worker. Worker termination
has a one-hour grace budget for warm shutdown; application task acknowledgement
and redelivery semantics still determine behavior after a forced node/process
failure. Beat publishes a startup PID file for readiness. The controller waits
for old Beat Pods to terminate before applying a changed template, providing
stop-before-start behavior beyond rollout percentages alone.
The listener installs a SIGTERM handler that uses its normal Python interrupt
shutdown path; Python running as PID 1 would otherwise ignore the default signal.

Readiness uses application endpoints where available, targeted Celery ping for
RBAC workers, and process startup checks for services without a health endpoint.
Completion checks every enabled application Deployment, including UI, workers,
Beat and optional ROS: target images, intended replica count, observed generation,
updated/ready/available replicas and old ReplicaSet Pod termination. Zero-replica
queues must have no remaining active Pods. Percentage settings alone do not
promise zero downtime; schema maintenance deliberately interrupts service.

## Monitoring and verification

Monitoring objects are still reconciled while an operation is blocked.
Conditions and warning Events report the failed migration Job name and UID;
restarts do not repeat a failure Event, and a new failed retry is reported.
Upgrade-specific metrics and alerts follow the operator monitoring fix
(COST-8519).

Regression tests cover release selection/edges, unchanged-generation operator
replacement, checkpointing and frozen targets, quiescence, successful sequential
migration/rollout, automatic rollback before schema mutation, failure retention,
explicit forward repair/source recovery, singleton Beat replacement and complete
workload readiness. Live release-pair, rollback and BYOI restore qualification
remain acceptance verification; unit/envtest results are not evidence of an
external PostgreSQL engine support matrix or backup/restore RTO/RPO.
