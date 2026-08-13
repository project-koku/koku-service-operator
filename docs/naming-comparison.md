# CRD Naming Comparison

> **Superseded.** The live API group is `service.costmanagement.openshift.io`
> (see `api/v1alpha1` and `config/crd/bases/service.costmanagement.openshift.io_…`).
> The `costmanagement-service-cfg.openshift.io` names below are historical —
> an earlier scaffold iteration before the rename.

## Selected naming vs sibling operator (historical)

|                 | koku-metrics-operator                                    | koku-service-operator (early scaffold)                    |
|-----------------|----------------------------------------------------------|-----------------------------------------------------------|
| Domain          | `openshift.io`                                           | `openshift.io` ✓                                          |
| Group           | `costmanagement-metrics-cfg`                             | `costmanagement-service-cfg` (later → `service.costmanagement`) |
| Kind            | `CostManagementMetricsConfig`                            | `CostManagementServiceConfig` ✓                           |
| Full API group  | `costmanagement-metrics-cfg.openshift.io`                | `costmanagement-service-cfg.openshift.io` (stale)         |
| CRD name        | `costmanagementmetricsconfigs.costmanagement-metrics-cfg.openshift.io` | `costmanagementserviceconfigs.costmanagement-service-cfg.openshift.io` (stale) |
| apiVersion      | `costmanagement-metrics-cfg.openshift.io/v1beta1`        | `costmanagement-service-cfg.openshift.io/v1alpha1` (stale) |

## Scaffold command

```bash
operator-sdk init --domain openshift.io --repo github.com/project-koku/koku-service-operator
operator-sdk create api --group costmanagement-service-cfg --version v1alpha1 --kind CostManagementServiceConfig --resource --controller
```

## PROJECT file

```yaml
domain: openshift.io
group: costmanagement-service-cfg
kind: CostManagementServiceConfig
version: v1alpha1
```
