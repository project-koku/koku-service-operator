# Clusterbot smoke — 2026-08-10 / 2026-08-11

Smoke of branch `fix/clusterbot-kruize-rbac-ingress` (from upstream `main` @
`7908c48`) on OpenShift clusterbot. Draft PR:
https://github.com/project-koku/koku-service-operator/pull/17

## 2026-08-11 status (current cluster) — SUCCESS

Context: `clusterbot` →
`api.chat-bot-krm93-3ftzcr.crt-mce-aws.devcluster.openshift.com`

| Piece | Namespace | Notes |
|-------|-----------|--------|
| Operator | `koku-service-operator-system` | `quay.io/martin_povolny/koku-server-operator:fix-clusterbot-20260811` |
| CMSC `cost-management` | `cost-onprem` | Bundled DB/cache; MinIO + AMQ Streams BYOI |
| MinIO | `cost-byoi-infra` | Secret `cost-management-storage-credentials` |
| Kafka | `kafka` | `STORAGE_CLASS=gp3-csi ./config/samples/byoi/deploy-kafka.sh` |
| RHBK | `keycloak` | `cost-onprem-chart/scripts/deploy-rhbk.sh` — completed |
| UI OAuth | `cost-onprem` | Mirrored via `mirror-ui-oauth-secret.sh --force` |

Verified:

- CMSC `phase=Ready`, `Available=True`, `UIReady=True`, `AuthenticationReady=True`, `SchemaUpToDate=True`
- UI pod **2/2** Ready; UI Route **302 → Keycloak** login
- API Route (gateway host) **401** without JWT (Envoy JWT gate working)
- Kruize `ClusterRole/cost-management-kruize` created (RBAC fix validated)
- Ingress image `quay.io/iop/ingress:master` (default / sample)

URLs:

- UI: `https://cost-management-ui-cost-onprem.apps.chat-bot-krm93-3ftzcr.crt-mce-aws.devcluster.openshift.com`
- API: `https://cost-management-gateway-cost-onprem.apps.chat-bot-krm93-3ftzcr.crt-mce-aws.devcluster.openshift.com/api`
- Keycloak: `https://keycloak-keycloak.apps.chat-bot-krm93-3ftzcr.crt-mce-aws.devcluster.openshift.com`

Auth patch applied on CMSC:

```yaml
spec:
  auth:
    keycloak:
      url: http://keycloak-service.keycloak.svc.cluster.local:8080
      issuerURL: https://keycloak-keycloak.apps.chat-bot-krm93-3ftzcr.crt-mce-aws.devcluster.openshift.com
      realm: kubernetes
      tls:
        insecureSkipVerify: true
```

## Fixes in this PR (code)

1. **Kruize ClusterRole escalate** — manager SA could not create `{cr}-kruize`
   ClusterRole. Kubebuilder RBAC markers → `config/rbac/role.yaml`.
2. **Empty ingress image** — default to `quay.io/iop/ingress:master`; samples + unit tests.
3. **Sample polish** — amd64 koku tag; oauth2-proxy on `registry.redhat.io`.

## Findings / gaps

| Severity | Finding | Status |
|----------|---------|--------|
| **Blocker** | Manager cannot create Kruize ClusterRole (RBAC escalate) | **Fixed in PR** |
| **Blocker** | Empty ingress image → InvalidImageName | **Fixed in PR** |
| **High** | Sample oauth2-proxy on `registry.access.redhat.com` fails ToS pulls | **Fixed in PR samples** |
| **High** | `quay.io/martin_povolny/koku:latest` arm64-only on amd64 clusterbot | **Sample → cost-mgmt-dev-tenant/koku:d8055ac** |
| **Ops** | Bundled PostgreSQL STS uses **default** SA + `fsGroup: 26` → needs `oc adm policy add-scc-to-user anyuid -z default -n cost-onprem` (koku SA alone is not enough) | Document / consider SA on DB STS |
| **Ops** | Grant `anyuid` (+ often `privileged`) to `{cr}-koku` SA for migrate Jobs | Manual each cluster |
| **Expected BYOI** | RHBK + mirror UI OAuth Secret | Scripts; not operator-owned |
| **Config** | Set `issuerURL` to public RHBK Route when `iss` ≠ in-cluster URL | Required for JWT |
| **Ops** | `make deploy` OpenAPI validate can timeout — `--validate=false` | Workaround |
| **Note** | API Route hostname is `{cr}-gateway-{ns}.apps...` (not `…-api-…`) | Use `oc get route` |

## Resume checklist (new clusterbot)

```bash
kubectl config use-context clusterbot   # or new context

cd .worktrees/fix-clusterbot-kruize-ingress
export IMG=quay.io/martin_povolny/koku-server-operator:fix-clusterbot-20260811
# rebuild/push if needed; then:
(cd config/manager && ../../bin/kustomize edit set image controller=$IMG)
./bin/kustomize build config/default | kubectl apply --validate=false --server-side --force-conflicts -f -

STORAGE_CLASS=gp3-csi LOG_LEVEL=INFO ./config/samples/byoi/deploy-kafka.sh

# MinIO (+ SA anyuid)
kubectl apply -f config/samples/byoi/infra/{namespace,serviceaccount,credentials,minio}.yaml
oc adm policy add-scc-to-user anyuid -z byoi-infra -n cost-byoi-infra
kubectl create ns cost-onprem --dry-run=client -o yaml | kubectl apply -f -
kubectl -n cost-onprem create secret generic cost-management-storage-credentials \
  --from-literal=access-key=byoi-minio-access \
  --from-literal=secret-key=byoi-minio-secret-key \
  --dry-run=client -o yaml | kubectl apply -f -

# Apply CMSC (patch clusterDomain, gp3-csi, MinIO objectStorage, amd64 UI image)
# Then:
oc adm policy add-scc-to-user anyuid -z default -n cost-onprem
oc adm policy add-scc-to-user anyuid -z cost-management-koku -n cost-onprem

DOMAIN=$(kubectl get ingresses.config.openshift.io cluster -o jsonpath='{.spec.domain}')
cd ../cost-onprem-chart
STORAGE_CLASS=gp3-csi LOG_LEVEL=INFO \
  COST_MGMT_NAMESPACE=cost-onprem \
  COST_MGMT_RELEASE_NAME=cost-management \
  COST_MGMT_UI_BASE_URL="https://cost-management-ui-cost-onprem.${DOMAIN}" \
  ./scripts/deploy-rhbk.sh

cd - >/dev/null
NAMESPACE=cost-onprem CR_NAME=cost-management \
  ./config/samples/byoi/mirror-ui-oauth-secret.sh --force
# Patch auth.keycloak.url + issuerURL as above

kubectl -n cost-onprem get cmsc,deploy,pods,route
```

## Success criteria

- [x] CMSC `phase=Ready`, `Available=True`, `UIReady=True`, no `Degraded`
- [x] UI pod **2/2** Ready; browser login redirect to RHBK
- [x] API Route returns **401** without JWT (gateway up)
- [x] No ImagePullBackOff / InvalidImageName for ingress / koku
- [ ] Full browser login + authenticated `/api` call (manual)

## Related

- Code branch: `fix/clusterbot-kruize-rbac-ingress`
- Chart RHBK: `cost-onprem-chart/scripts/deploy-rhbk.sh`
- UI OAuth notes: wiki `docs/cmsc/koku-ui-oauth2-proxy.md` (personal wiki)
