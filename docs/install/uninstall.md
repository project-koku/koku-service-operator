# Uninstall

Remove the `CostManagementServiceConfig` **before** the operator or the
namespace.

The CR carries a finalizer
(`costmanagementserviceconfigs.service.costmanagement.openshift.io/cleanup`)
that deletes cluster-scoped objects the operator created: the ConsoleLink, and
(only if ROS was enabled) the Kruize ClusterRole and ClusterRoleBinding. That
cleanup runs **only while the operator pod is still running**.

The operator is installed in the **same namespace as the CR**. Deleting that
namespace (or the operator Deployment / CSV) first kills the manager before it
can strip the finalizer. The namespace then stays `Terminating` and the
ConsoleLink leaks cluster-wide.

This does **not** delete your PostgreSQL, Kafka, object storage, or Keycloak.
Those stay until you remove them.

## Procedure

Replace the namespace and CR name with yours. The `if` gate is required: a
`--timeout` expiry still returns, and without `set -e` the next command would
delete the namespace while the finalizer is present.

```bash
export NAMESPACE=cost-onprem
export CR_NAME=cost-management-minimal

# Confirm the operator is still running
oc -n "$NAMESPACE" get deploy,pods

# Delete the CR and wait for finalizer cleanup
oc -n "$NAMESPACE" delete cmsc "$CR_NAME" --timeout=180s
if oc -n "$NAMESPACE" get cmsc "$CR_NAME" >/dev/null 2>&1; then
  echo "CMSC still present; not deleting the namespace. See recovery below." >&2
  exit 1
fi

# Then remove the operator and/or the namespace
oc delete ns "$NAMESPACE"
```

If you installed via OLM, delete the CR (and confirm NotFound) **before** the
Subscription / CSV — not the other way around.

## If the namespace is already Terminating

The operator is gone and the finalizer is stuck. Strip it, then delete the
leaked cluster-scoped objects:

```bash
export NAMESPACE=cost-onprem
export CR_NAME=cost-management-minimal

oc -n "$NAMESPACE" patch cmsc "$CR_NAME" --type=merge \
  -p '{"metadata":{"finalizers":[]}}'
oc delete consolelink "${CR_NAME}-cost-management" --ignore-not-found
oc delete clusterrole,clusterrolebinding \
  -l app.kubernetes.io/managed-by=koku-service-operator,app.kubernetes.io/instance="${CR_NAME}",app.kubernetes.io/component=ros-optimization \
  --ignore-not-found
```

The namespace should finish terminating within a few seconds.

If `oc patch` is denied with a webhook timeout, the operator Service is gone
but the cluster-scoped admission configs remain (`failurePolicy: Fail` on CMSC
CREATE/UPDATE). That is the OLM path; in-cluster `hack/deploy-incluster.sh`
does not install those configs. Delete the Cost Management webhook
configurations (or the CSV, which owns them), then retry the patch:

```bash
oc get mutatingwebhookconfiguration,validatingwebhookconfiguration -o name \
  | grep costmanagementserviceconfig
# oc delete mutatingwebhookconfiguration/<name> validatingwebhookconfiguration/<name>
```

Then retry the `oc patch` above, then ConsoleLink / Kruize RBAC.

## Lab reset

`hack/demo-preprod.sh --reset` deletes the CR first while the operator is
Available, and strips the finalizer (plus ConsoleLink / Kruize RBAC) if the
operator is already gone.

`scripts/install-cmsc.sh` cleanup also deletes the CR first, but it does **not**
strip a stuck finalizer — if the operator is down, that path can still leave
the namespace `Terminating`. Use the recovery steps above.

Cluster Bot tear-down in [clusterbot.md](../development/clusterbot.md) follows
the CR-first order.
