# Pre-prod OLM demo runbook

Use this on a freshly provisioned Cluster Bot OpenShift cluster. It keeps the
existing `scripts/demo-preprod.sh` path untouched and installs the published OLM
catalog through the OpenShift console instead of building or pushing an
operator image locally.

This is a lab/demo workflow. The BYOI fixture credentials and the Keycloak
user (`admin` / `admin`) are not production settings.

## 1. Confirm Cluster Bot access

Provision the Cluster Bot cluster, log in, and ensure its kubeconfig context is
named `clusterbot`:

```bash
oc config get-contexts clusterbot
oc whoami --context=clusterbot
oc get nodes --context=clusterbot
```

All nodes should be `Ready` before beginning. The demo defaults below use the
four disposable namespaces `cost-byoi`, `cost-byoi-infra`, `kafka`, and
`keycloak`.

## 2. Prepare the infrastructure and CMSC

Run the new script. It deploys Kafka, Postgres, Valkey, MinIO, Keycloak,
the UI OAuth Secret, and the other CMSC Secrets. It also renders the CMSC but
does **not** install the operator or create the CMSC.

```bash
KUBE_CONTEXT=clusterbot \
  NAMESPACE=cost-byoi \
  CR_NAME=cost-management \
  INFRA_NAMESPACE=cost-byoi-infra \
  ./scripts/demo-preprod-olm.sh --prepare
```

Wait for this command to complete. It writes the rendered CMSC to:

```text
/tmp/koku-demo-preprod-olm-cost-byoi.yaml
```

If infrastructure is already present, do not rerun `--prepare`. Print the
current cluster Console URL and username instead:

```bash
./scripts/demo-preprod-olm.sh --console
```

Use the `kubeadmin` password issued with the ClusterBot provisioning output.
It cannot be retrieved from the cluster: `kube-system/kubeadmin` contains only
a one-way password hash. If you want the helper to repeat the issued password,
set `CONSOLE_PASSWORD` in `scripts/demo-preprod.local.env` (which remains
untracked). The application UI uses the separate demo realm login `admin` /
`admin` after it is deployed.

## 3. Add the published catalog and install from the OpenShift console

The catalog is already published on Quay; no source build, image push, or
operator code change is needed. OpenShift uses a CatalogSource to make that
catalog available in its Software Catalog.

In the OpenShift console, use the **Administrator** perspective:

1. Go to **Cluster Settings → Configuration → OperatorHub → Sources** and
   select **Create CatalogSource**.
2. Set the name to `koku-service-operator-catalog`, the namespace to
   `openshift-marketplace`, source type to `grpc`, and image to
   `quay.io/project-koku/koku-service-operator-catalog:latest`.
3. Wait for the source to become Ready, then open **Ecosystem → Software
   Catalog**, find **Cost Management Service Operator**, confirm its offered
   version is `v0.0.1`, and click **Install**.
4. Choose channel **beta**, installation mode **All namespaces on the
   cluster**, installed namespace **cost-byoi**, and **Automatic** approval.
   Click **Install**. Do not accept a newer release for this demo.

Although the package currently advertises the AllNamespaces OLM mode, select
`cost-byoi` as its installed namespace for this demo. The controller watches
its own service-account namespace, so the CMSC and application workloads must
remain in `cost-byoi`.

Confirm OLM installed the package before applying the CMSC:

```bash
oc -n cost-byoi get csv,pods
oc get crd costmanagementserviceconfigs.service.costmanagement.openshift.io
```

The CSV should be `Succeeded` and the
`koku-service-operator-controller-manager` pod should be running.

## 4. Create the prepared CMSC

```bash
oc apply -f /tmp/koku-demo-preprod-olm-cost-byoi.yaml
oc -n cost-byoi get cmsc cost-management
```

## 5. Watch and open the UI

The script starts tmux panes for CMSC status and workloads. A third pane waits
for the UI Deployment and opens the browser once it is ready.

```bash
KUBE_CONTEXT=clusterbot ./scripts/demo-preprod-olm.sh --watch
```

Success is `Available=True` and `UIReady=True` on the CMSC, followed by a
ready `cost-management-ui` Deployment. The script opens the resulting UI route;
log in with `admin` / `admin`.

If you do not want the browser to open automatically:

```bash
KUBE_CONTEXT=clusterbot ./scripts/demo-preprod-olm.sh --watch --no-open
```

## Cleanup

Delete the CMSC first, wait for it to disappear, then remove the demo
namespaces. This lets the operator remove its finalizer and cluster-scoped
resources cleanly.

```bash
oc -n cost-byoi delete cmsc cost-management --wait=true --timeout=180s
oc delete namespace cost-byoi cost-byoi-infra kafka keycloak --wait=false
```

For the OLM cleanup order, see [uninstall.md](../install/uninstall.md).
