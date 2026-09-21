# First use after install

After the `CostManagementServiceConfig` reaches `Available=True` and you can
log in to the UI, you still need data and a **cost model** before dollar
amounts appear in Overview and Cost Explorer.

**Beta.** These guides assume `spec.ros.enabled: false` (Cost-only). AWS, GCP,
and Azure views require cloud integrations that are not part of the on-prem
beta path.

## Data paths

| Source | How data arrives | What you see first |
|--------|------------------|-------------------|
| OpenShift cluster metrics | [CMMO](cmmo.md) upload from a reporting cluster | Usage and inventory after Listener ingest |
| Lab / test payloads | Your own upload tooling (not covered here) | Depends on payload type |

Ingest alone can show **usage** (CPU, memory, node counts) before any **cost
in currency**.

## Cost model (required for dollar amounts)

OpenShift costs are calculated from a **cost model** assigned to the OpenShift
source. Without it, Overview and Cost Explorer may show **$0.00** even when
usage graphs have data.

1. In the UI, open **Cost management** → **Cost models**.
2. Create an OpenShift cost model (or use your organization's template).
3. Open **Integrations** (or **Sources**) and assign that cost model to the
   OpenShift cluster source.

Red Hat documents cost models in the
[Cost Management product documentation](https://access.redhat.com/documentation/en-us/red_hat_hybrid_cloud_console/1.0/html-single/cost_management/index).

## Beta UI scope

With ROS disabled on the CR, Resource Optimization pages and some SaaS-only
navigation entries are not applicable. Errors on cloud provider pages when you
have no AWS, GCP, or Azure sources are expected for an OpenShift-only deploy.

## Next

- Reporting cluster setup: [cmmo.md](cmmo.md)
- TLS and sizing: [production.md](production.md)
