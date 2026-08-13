# OwnNamespace operator model

The Cost Management service operator runs in **OwnNamespace** mode:

| Namespace | Role |
|-----------|------|
| Operator install NS | Same as the `CostManagementServiceConfig` (operand) NS |
| Informer cache | Restricted to that watch NS (`Cache.DefaultNamespaces`) |
| BYOI infra (Postgres, Kafka, MinIO, …) | May live in **other** namespaces; connect via CR fields — do **not** watch/own them |
| Cluster-scoped exceptions | StorageClass / OpenShift Ingress discovery, ConsoleLink, Kruize ClusterRole/Binding, and a narrow `get` on Secret `noobaa-admin` (typically `openshift-storage`) |

## RBAC shape

- **`manager-role` + RoleBinding** — namespace-scoped manage rights (Secrets, Jobs, Deployments, CMSC, …) **only** in the watch namespace.
- **`manager-cluster-role` + ClusterRoleBinding** — the cluster exceptions above (including `secrets` get with `resourceNames: [noobaa-admin]`). Not a blanket Secrets grant.

## Local / CRC

```bash
./hack/deploy-crc.sh cost-onprem
NAMESPACE=cost-onprem make run
# or: NAMESPACE=cost-onprem go run ./cmd/main.go --dev
```

`make deploy` still scaffolds into `koku-service-operator-system`. Under OwnNamespace the CR must live in that same namespace, or you must change the deploy namespace when packaging. Full OLM `installModes` (COST-7695) is out of scope here; this document is the intended runtime model.
