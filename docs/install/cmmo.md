# Configure Cost Management Metrics Operator (CMMO)

How to point **Cost Management Metrics Operator** on a reporting cluster at the
Cost Management instance this operator deployed — not at `console.redhat.com`.

CMMO is a **separate** product. This operator does not install or manage it.

**Lab-derived.** The `api_url` + `ingress_path` combination below comes from
the in-repo lab script (`scripts/setup-cost-mgmt-tls.sh`). Treat it as the
path we use in testing, not a certified CMMO support contract. Confirm with
CMMO owners before a GA install.

**Beta.** Disable Resource Optimization collection on CMMO. Do not enable
`spec.ros.enabled` on the Cost Management CR.

## What CMMO talks to

Uploads go through the **Envoy gateway Route**, which only matches path `/api`
and forwards `/api/ingress/` to insights-ingress-go.

| Piece | Value |
|-------|--------|
| Gateway host | `{cr-name}-gateway-{namespace}.{apps-domain}` |
| `spec.api_url` | `https://<gateway-host>` — **no path**, not `console.redhat.com` |
| `spec.upload.ingress_path` | `/api/ingress/v1/upload` |
| Resulting upload URL | `https://<gateway-host>/api/ingress/v1/upload` |
| Auth | Keycloak `client_credentials` (CMMO `authentication.type: service-account`) |
| JWT audiences Envoy accepts | `cost-management-operator`, `cost-management-ui` |

Find the host:

```bash
oc -n "$NAMESPACE" get route -l app.kubernetes.io/component=gateway \
  -o jsonpath='{.items[0].spec.host}{"\n"}'
```

## Keycloak client for CMMO

Create a confidential client in the same realm the Cost Management CR uses
(samples: `kubernetes`) with the **client credentials** grant. The access token
must be accepted by Envoy (audience in `spec.auth.keycloak.audiences`). Realm
and claim setup: [keycloak.md](keycloak.md).

On **each reporting cluster**, store that client in a Secret. CMMO’s field
names use underscores:

| Key | Required |
|-----|----------|
| `client_id` | Yes |
| `client_secret` | Yes |

```bash
# On the reporting cluster, in the namespace where you install CMMO
oc -n costmanagement-metrics-operator create secret generic cost-management-auth-secret \
  --from-literal=client_id='<cmmo-client-id>' \
  --from-literal=client_secret='<cmmo-client-secret>'
```

Token URL (replace host and realm):

`https://<keycloak-issuer-host>/realms/kubernetes/protocol/openid-connect/token`

Use the same issuer host that matches `spec.auth.keycloak.issuerURL` (or `url`
when issuer is derived from it).

## CostManagementMetricsConfig

Install CMMO from OperatorHub on the reporting cluster (OpenShift documentation
for that operator), then apply a config. Example:

```yaml
apiVersion: costmanagement-metrics-cfg.openshift.io/v1beta1
kind: CostManagementMetricsConfig
metadata:
  name: costmanagementmetricscfg
  namespace: costmanagement-metrics-operator
spec:
  # Must be THIS instance's gateway. Empty/default uploads to console.redhat.com.
  api_url: "https://cost-management-minimal-gateway-cost-onprem.apps.cluster.example.com"

  authentication:
    type: service-account
    token_url: "https://keycloak-keycloak.apps.cluster.example.com/realms/kubernetes/protocol/openid-connect/token"
    secret_name: cost-management-auth-secret

  upload:
    upload_toggle: true
    upload_cycle: 360
    ingress_path: /api/ingress/v1/upload
    # Production: true, with a CA that trusts the gateway Route.
    # Lab self-signed only: false.
    validate_cert: true

  prometheus_config:
    service_address: "https://thanos-querier.openshift-monitoring.svc:9091"
    skip_tls_verification: false
    collect_previous_data: true
    context_timeout: 120
    disable_metrics_collection_cost_management: false
    disable_metrics_collection_resource_optimization: true

  source:
    create_source: true
    check_cycle: 1440
    sources_path: /api/cost-management/v1/
    name: ""
```

Set `disable_metrics_collection_resource_optimization: true` for beta.

`validate_cert: false` appears in the lab script for self-signed Keycloak and
must not be copied into production. Trust the gateway and token URL
certificates instead.

## Do not

- Do not run `scripts/setup-cost-mgmt-tls.sh` on a customer cluster. It is a
  same-cluster E2E helper (installs CMMO next to the stack, skips cert
  validation).
- Do not leave `api_url` unset — CMMO then talks to hosted console.redhat.com.
- Do not enable ROS collection against a Cost-only (`ros.enabled: false`)
  instance.

## Check

On the reporting cluster:

```bash
oc -n costmanagement-metrics-operator get costmanagementmetricsconfig
oc -n costmanagement-metrics-operator logs deploy/costmanagement-metrics-operator
```

On the Cost Management cluster, Listener and Ingress should see uploads on
topic `platform.upload.announce` after a successful CMMO cycle. The UI Sources
page lists the cluster when `source.create_source` is true.
