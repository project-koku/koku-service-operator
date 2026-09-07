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
  Jobs, Deployments, CMSC, …) in **every** namespace. That a CMSC outside the
  operator's own namespace actually reconciles (the reason a ClusterRoleBinding
  is needed over a namespaced RoleBinding) is exercised by the cross-namespace
  envtest in `internal/controller/cross_namespace_test.go`.
- **`manager-cluster-role` + ClusterRoleBinding** — the cluster exceptions
  above (including `storageclasses` `get;list;watch`, and `secrets` get with
  `resourceNames: [noobaa-admin]`). Not a blanket extra Secrets grant beyond
  `manager-role`.
- Leader election stays a namespaced RoleBinding in the operator install NS.

### Secrets: no cluster-wide list/watch

AllNamespaces means broad *reach* — the operator must manage operands in any
namespace a CMSC lands. It does **not** mean the operator may enumerate Secret
contents cluster-wide. `manager-role` grants Secrets `get;create;update;patch;delete`
but **not** `list` or `watch`. The `get` is still cluster-wide (any namespace,
any name — the AllNamespaces reach), so a compromised operator can read a Secret
it can *name*, but without `list`/`watch` it cannot enumerate or harvest every
Secret in the cluster. Removing enumeration is the win; the residual by-name
`get` is structural (see the least-privilege note below).

This holds because the manager runs **no Secret informer**:

- `SetupWithManager` does not `Owns(&corev1.Secret{})`.
- The client cache `DisableFor`s Secret (`cmd/main.go`), so `Get` on a Secret
  goes straight to the API server rather than lazily starting a `list;watch`
  informer.

Trade-off: because there is no Secret watch, a *deleted* operator-managed Secret
is not recreated on a watch event. `ensureSecret` already never overwrites an
existing Secret (generated credentials are preserved), so only deletion recovery
is affected, and it is bounded by the manager `SyncPeriod` (1h) or the next CMSC
reconcile.

This is OLMv1-aligned: RBAC stays static and cluster-wide (no runtime
RoleBinding provisioning), and the trimmed verb set is directly visible in the
bundle ClusterRole that a cluster admin audits before installing the
ClusterExtension.

### Least privilege: grants must map to a code path

AllNamespaces makes *reach* cluster-wide and, under static OLMv1 RBAC, that reach
is not reducible without per-namespace runtime RoleBindings (rejected). So the
security lever is the **grant set** (kinds × verbs), not the scope. The rule:
**every `(kind, verb)` in `manager-role`/`manager-cluster-role` must trace to an
actual client call.** The Secret split above is the template — apply the same
reasoning everywhere:

- If a kind is only read **by name** (a `Get`, never a `List`), it needs no
  `list`/`watch` — but only if it is also kept out of the informer cache
  (`Owns()`-free **and** cache `DisableFor`, or read via `APIReader`). A cached
  `Get` silently starts a `list;watch` informer, so RBAC and cache config must be
  trimmed together.
- If a kind is written via **Server-Side Apply**, it needs `patch` (and `create`
  when absent), **not** `update`.
- If the operator never constructs a kind, it gets **no grant** — an unused
  cluster-wide grant is pure attack surface (worst case: an escalation primitive
  like `roles`/`rolebindings`).

Applied trims (all locked by `TestManagerRole_TierAGrantsTrimmed`, which checks
`role.yaml` **and** the CSV `clusterPermissions`):

| Kind | Verbs | Rationale |
|------|-------|-----------|
| `secrets` | `get;create;update;patch;delete` (no `list`/`watch`) | No Secret informer; by-name `Get` only. |
| `rbac.../roles`,`rolebindings` | **none** | No namespaced Role/RoleBinding is ever built; would be an unused cluster-wide escalation primitive. |
| `monitoring.coreos.com/servicemonitors`,`prometheusrules` | `create;delete;get;patch` | SSA-applied + deleted; never `Owns()`'d or `List`'d. |
| `service.costmanagement.../costmanagementserviceconfigs` | `get;list;watch;update` | Owned CR: watched via `For()`, `Update`d only for the finalizer; users create/delete/edit it. |

When adding or changing a `+kubebuilder:rbac` marker, name the call site in a
comment, run `make manifests` **and** `make bundle`, and add/extend an assertion
in `internal/controller/rbac_manifest_test.go` so the grant cannot silently widen
(or a required rule silently vanish from the CSV).

## Local / CRC (out-of-cluster)

Works when the CR uses addresses reachable from your laptop (bundled DB/cache
on CRC, or port-forwards). A laptop run **must** pin the cache with `NAMESPACE`
(or `WATCH_NAMESPACE`) so it does not list the whole cluster through your
kubeconfig. The manager **fails closed** here: out-of-cluster with neither set,
it refuses to start rather than defaulting to AllNamespaces — so the bare
`go run …` form needs the pin too, not just the `make run` wrapper.

```bash
./hack/deploy-dev.sh cost-onprem   # alias: ./hack/deploy-crc.sh
NAMESPACE=cost-onprem IMG=quay.io/project-koku/koku-service-operator:v0.0.1 make run
# or: NAMESPACE=… go run ./cmd/main.go --dev --operator-image=…
```

In-cluster, omit `WATCH_NAMESPACE` (empty = watch all — the fail-closed guard
applies only out-of-cluster). Do not inject `NAMESPACE` from the pod SA file as
a watch pin — that would turn AllNamespaces into OwnNamespace.

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
