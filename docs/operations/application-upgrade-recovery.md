# Application upgrade recovery

Use this guide when a migration fails or the CMSC reports
`RecoveryRequired=True`. The operator does not roll back PostgreSQL schema.
Keep a verified daily database backup and confirm that its restore procedure
works before upgrading.

## Inspect and protect evidence

1. Read the CMSC conditions and operation ID:

   ```sh
   oc get cmsc -n <namespace> <name> -o yaml
   ```

2. Find the failed migration Job, then save logs from every retry Pod before
   deleting or replacing it:

   ```sh
   oc get jobs -n <namespace>
   oc logs -n <namespace> -l 'batch.kubernetes.io/job-name=<migration-job>' --all-containers=true --prefix=true --tail=-1
   ```

3. Ask the PostgreSQL owner to inspect the database and decide whether to
   restore a backup or repair the schema forward. Do not start either the old
   or target application against an unknown schema.

## Recover an upgrade

After the database is safe for the selected application revision, submit a new
request ID bound to the operation shown in status.

- `Retry` runs the currently requested target. Use this after a database restore
  or forward repair when the target is compatible with the repaired schema.
- `ResumePrevious` restores the saved source revision. Use it only after a
  restore or repair made the database safe for that source revision.

While database state is unknown, the operator holds application replicas at zero.
After an explicit recovery request and the required drain/migration checks, it
starts the selected revision and verifies readiness. An image pin change by itself
does not clear the hold.

## Recover a first installation

There is no source revision to resume. After saving logs and making the database
safe, delete the failed migration Job; the operator will create it again from
the current CMSC image pins. Keep workloads stopped until the migration and
application rollout succeed.
