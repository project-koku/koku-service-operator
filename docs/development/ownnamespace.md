# OwnNamespace operator model

The Cost Management service operator runs in **OwnNamespace** mode:

| Namespace | Role |
|-----------|------|
| Operator install NS | Same as the `CostManagementServiceConfig` (operand) NS |
| Informer cache | Restricted to that watch NS (`Cache.DefaultNamespaces`) |
| BYOI infra (Postgres, Kafka, MinIO, …) | May live in **other** namespaces; connect via CR fields — do **not** watch/own them |
| Cluster-scoped exceptions | StorageClass / OpenShift Ingress discovery, ConsoleLink, Kruize ClusterRole/Binding, and a narrow `get` on Secret `noobaa-admin` (`spec.objectStorage.noobaaNamespace`: `openshift-storage` or `noobaa` only) |

## RBAC shape

- **`manager-role` + RoleBinding** — namespace-scoped manage rights (Secrets, Jobs, Deployments, CMSC, …) **only** in the watch namespace.
- **`manager-cluster-role` + ClusterRoleBinding** — the cluster exceptions above (including `storageclasses` `get;list;watch`, and `secrets` get with `resourceNames: [noobaa-admin]`). Not a blanket Secrets grant.

## Local / CRC (out-of-cluster)

Works when the CR uses addresses reachable from your laptop (bundled DB/cache on CRC, or port-forwards). OwnNamespace still requires `NAMESPACE`:

```bash
./hack/deploy-dev.sh cost-onprem   # alias: ./hack/deploy-crc.sh
NAMESPACE=cost-onprem IMG=quay.io/project-koku/koku-service-operator:v0.0.1 make run
# or: NAMESPACE=… go run ./cmd/main.go --dev --operator-image=…
```

## In-cluster (BYOI / Cluster Bot / pre-prod)

When the CR points at `*.svc.cluster.local` hosts, run the operator **inside** the CR namespace (amd64 image on typical OCP nodes). Laptop `make run` will fail probes.

```bash
# Day-one Cluster Bot (Redpanda, no AMQ Streams):
./hack/clusterbot-smoke.sh
IMG=quay.io/<org>/koku-service-operator:<tag> ./hack/deploy-incluster.sh cost-byoi
```

Full walkthrough: [clusterbot.md](clusterbot.md). UI + Keycloak path: [pre-prod-install.md](pre-prod-install.md).

`make deploy` still scaffolds into `koku-service-operator-system`. Under OwnNamespace the CR must live in that same namespace, or you must change the deploy namespace / use `deploy-incluster.sh`. Full OLM `installModes` (COST-7695) is out of scope here; this document is the intended runtime model.

## Cluster Bot / pytest

For lab clusters running the ported pytest suite (AMQ Streams + S4), use a **single**
namespace end-to-end (operator + CR + pytest `NAMESPACE`). Prefer:

```bash
IMG=quay.io/<you>/koku-service-operator:<tag> ./hack/deploy-incluster.sh cost-onprem
```

Do not split operator (`koku-service-operator-system`) and CR (`cost-onprem`) — the
operator will not reconcile. See [clusterbot-operator-pytest.md](clusterbot-operator-pytest.md).

## Uninstall

Because the manager and the CR share a namespace, delete the
`CostManagementServiceConfig` and wait until it is gone **before** deleting
the namespace or the operator Deployment. Otherwise the CR finalizer never
runs and the namespace stays `Terminating` (ConsoleLink leak). Procedure and
recovery: [uninstall.md](../install/uninstall.md).
