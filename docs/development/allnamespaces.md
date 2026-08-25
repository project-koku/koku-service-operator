# AllNamespaces operator model

The Cost Management service operator runs in **AllNamespaces** mode. That is
the CSV `installModes` contract, the in-cluster default, and the OLMv1
destination. **OwnNamespace is not supported** — do not require it for
CatalogSource, Subscription, or ClusterExtension.

| Namespace | Role |
|-----------|------|
| Operator install NS | Where the **pod** runs. CSV suggests `cost-onprem`. Users may install elsewhere (`openshift-operators`, …). |
| Informer cache | **Every** namespace unless `WATCH_NAMESPACE` is set |
| CMSC / operands | `metadata.namespace` of each `CostManagementServiceConfig` (`cfg.Namespace`) |
| BYOI infra (Postgres, Kafka, MinIO, …) | May live in **other** namespaces; connect via CR fields — do **not** own them |
| Cluster-scoped exceptions | StorageClass / OpenShift Ingress discovery, ConsoleLink, Kruize ClusterRole/Binding, and a narrow `get` on Secret `noobaa-admin` (`spec.objectStorage.noobaaNamespace`: `openshift-storage` or `noobaa` only) |

Install vs watch are independent. Putting the operator pod in `cost-onprem`
does not pin watch. A CMSC in another namespace still reconciles.

OLMv0 (OCP 4.x CatalogSource + Subscription) and OLMv1 (OCP 5 ClusterExtension)
both use AllNamespaces. Do not add OperatorConditions. Do not set
`spec.config.inline.watchNamespace` on ClusterExtension.

## RBAC shape

- **`manager-role` + ClusterRoleBinding** — namespace-scoped kinds (Secrets,
  Jobs, Deployments, CMSC, …) in **every** namespace.
- **`manager-cluster-role` + ClusterRoleBinding** — the cluster exceptions
  above (including `storageclasses` `get;list;watch`, and `secrets` get with
  `resourceNames: [noobaa-admin]`). Not a blanket extra Secrets grant beyond
  `manager-role`.
- Leader election stays a namespaced RoleBinding in the operator install NS.

Follow-up: bind operands with a RoleBinding in each CMSC namespace instead of
cluster-wide `manager-role` (keep cluster-wide CMSC get/list/watch/status).

## Local / CRC (out-of-cluster)

Works when the CR uses addresses reachable from your laptop (bundled DB/cache
on CRC, or port-forwards). `make run` pins the cache with `NAMESPACE` so a
laptop does not list the whole cluster:

```bash
./hack/deploy-dev.sh cost-onprem   # alias: ./hack/deploy-crc.sh
NAMESPACE=cost-onprem IMG=quay.io/project-koku/koku-service-operator:v0.0.1 make run
# or: NAMESPACE=… go run ./cmd/main.go --dev --operator-image=…
```

In-cluster, omit `WATCH_NAMESPACE` (empty = watch all). Do not inject
`NAMESPACE` from the pod SA file as a watch pin — that would turn AllNamespaces
into OwnNamespace.

## In-cluster (BYOI / Cluster Bot / pre-prod)

When the CR points at `*.svc.cluster.local` hosts, run the operator **inside**
the cluster (amd64 image on typical OCP nodes). Laptop `make run` will fail
probes.

```bash
# Day-one Cluster Bot (Redpanda, no AMQ Streams):
./hack/clusterbot-smoke.sh
IMG=quay.io/<org>/koku-service-operator:<tag> ./hack/deploy-incluster.sh cost-onprem
```

Recommended: operator + CMSC both in `cost-onprem`. BYOI may stay in
`cost-byoi-infra`, `kafka`, `keycloak`, `openshift-storage`, etc.

`make deploy` scaffolds into `cost-onprem`. Full walkthrough:
[clusterbot.md](clusterbot.md). UI + Keycloak path: [pre-prod-install.md](pre-prod-install.md).
OLMv1 sample: [config/samples/olmv1/](../../config/samples/olmv1/).

## Cluster Bot / pytest

Pytest `NAMESPACE` is the **CR** namespace (operands), not necessarily the
operator pod NS. Prefer one NS for the lab:

```bash
IMG=quay.io/<you>/koku-service-operator:<tag> ./hack/deploy-incluster.sh cost-onprem
```

See [clusterbot-operator-pytest.md](clusterbot-operator-pytest.md).
