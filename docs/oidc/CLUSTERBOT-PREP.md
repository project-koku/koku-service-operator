# ClusterBot preparation for COST-8520 OIDC

**Purpose:** Test Google sign-in through the existing Keycloak broker for COST-8520 on ClusterBot.

**Status:** The 2026-11-18 Google broker and rbac-go flow succeeded. On 2026-11-19, the first new ClusterBot expired before RHBK setup. A second fresh cluster completed BYOI, RHBK, Google broker configuration, rbac-go migration, and manual sync. Its first Google login reached the Cost callback but returned HTTP 500 due to Keycloak's unverified-email state. Trust Email and the existing federated user's Email verified flag were corrected, after which Google login succeeded. Cost returned no access because the user has no Cost permissions; the latest inspection found tenant attributes set but no `/org-1234567` membership. See the [second fresh-cluster run record](#fresh-clusterbot-rerun-2026-11-19).
**Scope note:** This exercises the current broker path. The operator does not yet support direct external OIDC; see [DESIGN.md](DESIGN.md) and [PLAN.md](PLAN.md).

## What we can prepare before the cluster is ready

### Google Cloud Console

You can create/select the Google Cloud project and configure the consent screen now. The exact authorized redirect URI depends on the ClusterBot apps domain, so add that after the domain is available. The [Google Auth setup runbook](GOOGLE-AUTH-SETUP.md) includes a project bootstrap helper that prints the exact callback; Google requires OAuth client creation in Cloud Console.

- Choose the OAuth consent audience that matches the account you will test with. For a personal account, use External and leave the app in Testing; add your test account.
- Keep requested scopes to openid, email, and profile.
- Create an OAuth client of type Web application after the Keycloak host is known.
- Register the exact callback shown by Keycloak. With the default Keycloak namespace, realm, and provider alias, it is normally:

      https://keycloak-keycloak.<apps-domain>/realms/kubernetes/broker/google/endpoint

  Confirm the callback shown in Keycloak's Google identity-provider screen; use that exact value in the Google client. Google requires an exact redirect URI match and HTTPS for non-localhost web callbacks. Its policy also limits callback domains to domains you own, are authorized to use, or are licensed to use. See [Google's web-server OAuth guide](https://developers.google.com/identity/protocols/oauth2/web-server) and [OAuth policy](https://developers.google.com/identity/protocols/oauth2/policies).
- Keep the Google client ID and secret in your password manager until Keycloak is deployed. Enter them only in Keycloak's Google identity-provider configuration. They are distinct from the Cost UI client ID/secret stored for oauth2-proxy; do not put the Google secret in the UI OAuth Secret.

The current [Keycloak install guide](../install/keycloak.md) covers Cost claims and the org-admin role. The [Cost UI OAuth Secret guide](../operations/cost-management-ui-oauth-client-setup.md) documents the separate client credential used by oauth2-proxy.

### Tunnel decision

A ClusterBot OpenShift Route may already be publicly reachable, unlike the local CRC setup. First test the Keycloak Route from the browser and from a network outside the cluster. If the route is reachable at its HTTPS apps hostname, Google can redirect the browser directly to it and no tunnel is needed.

Use a tunnel only if the route cannot be reached from the Google sign-in browser. Do not expose the OpenShift API, database, Kafka, Envoy, or unrelated routes. If a tunnel is needed, expose only the Keycloak web Route and keep the Google callback hostname identical to the hostname configured on Keycloak.

## When the cluster is ready

### 1. Connect without changing the normal working context

Use the existing ClusterBot connection process after provisioning completes. The helper can merge the new connection into the default kubeconfig and switch the active context; record the current context first and restore it when done.

For the deployment scripts, use a dedicated kubeconfig containing the ClusterBot context and verify its current context before any mutation. The scripts operate through the current context; setting KUBE_CONTEXT makes the BYOI script fail if that selected context is not the expected one. For manual cluster reads and writes, pass the explicit ClusterBot context on every command.

### 2. Run a read-only preflight

Record these values before deploying:

    CLUSTERBOT_CONTEXT="$(oc config current-context)"  # after selecting the dedicated kubeconfig
    oc --context="$CLUSTERBOT_CONTEXT" whoami
    oc --context="$CLUSTERBOT_CONTEXT" get clusterversion version
    oc --context="$CLUSTERBOT_CONTEXT" get nodes -o wide
    oc --context="$CLUSTERBOT_CONTEXT" get storageclass
    oc --context="$CLUSTERBOT_CONTEXT" get ingress.config.openshift.io cluster -o jsonpath='{.spec.domain}'

The [ClusterBot operator runbook](../development/clusterbot-operator-pytest.md) recommends OCP 4.18 or later, amd64 workers, at least two workers for the full stack, and a default StorageClass. Confirm there is enough capacity for PostgreSQL, Valkey, MinIO, Kafka, Keycloak, the operator, Koku, Envoy, and the UI.

The default full-stack names used below are:

| Resource | Default |
|---|---|
| Application/CMSC namespace | cost-byoi |
| CMSC name | cost-management |
| Infrastructure namespace | cost-byoi-infra |
| Kafka namespace | kafka |
| Keycloak namespace | keycloak |
| Keycloak realm | kubernetes |

The full BYOI path is described in the [pre-production install guide](../development/pre-prod-install.md). The shorter [ClusterBot smoke path](../development/clusterbot.md) uses Redpanda and does not deploy Keycloak or a UI login, so it is not sufficient for the Google broker test.

### 3. Deploy the stack with the ClusterBot kubeconfig selected

Use the linked [BYOI deployment script](../../scripts/deploy-byoi.sh) for dependencies and app Secrets. It does not install the operator or apply the CMSC. Then build/push a linux/amd64 operator image and use the [in-cluster deployment script](../../scripts/deploy-incluster.sh). Apply the linked [BYOI application sample](../../config/samples/byoi/app/costmanagementserviceconfig.yaml), adjusting its namespace, CR name, cluster domain, and images to match the deployment.

Set the environment so every nested oc/kubectl call inherits the dedicated kubeconfig. Set KUBE_CONTEXT to that kubeconfig's current context so the BYOI script checks its target before mutation. Do not rely on whatever context happens to be active in the normal kubeconfig.

The [RHBK deploy helper](../../scripts/deploy-rhbk.sh#L637-L685) derives the Keycloak Route hostname from the cluster apps domain and Keycloak namespace. With the default namespace, expect a host shaped like keycloak-keycloak.<apps-domain>. It also creates the Cost UI client and the UI redirect URI; it does not configure Google as an identity provider.

### 4. Add Google as a Keycloak identity provider

After RHBK is Ready and the Route hostname is confirmed, open the Admin Console using the current Route and generated credentials in the [Google Auth setup guide](GOOGLE-AUTH-SETUP.md#admin-console-and-bootstrap-credentials):

1. Open the Keycloak Admin Console and select the kubernetes realm.
2. Add Google under Identity Providers. Use alias google and paste the Google Web client ID and secret.
3. Copy Keycloak's displayed Redirect URI into the Google OAuth client. Confirm it is exactly the URI registered in the Google Console.
4. Save. Keep the provider's basic OIDC scopes; do not request unrelated Google API scopes.
5. Verify the Keycloak HTTPS Route is reachable from the browser and the authorization/callback flow is not blocked by an ingress policy.

The [RHBK deploy script](../../scripts/deploy-rhbk.sh) does not configure Google. After registering the exact callback in Cloud Console, run the [Keycloak and rbac-mini helper](../../scripts/oidc/configure-keycloak-rbac-mini.py) as described in the [setup guide](KEYCLOAK-RBAC-MINI.md), or configure the IdP in the Admin Console. RHBK generates its initial admin credentials in the keycloak-initial-admin Secret; use that Secret securely and do not assume a fixed password.

### 5. Provision the Cost identity for the brokered user

Google authenticates the person, then Keycloak issues the token consumed by the current operator. Google profile claims do not provide Cost's `org_id`, `account_number`, or `org-admin` realm role.

After the first broker login creates or links the test user, use the Keycloak Admin Console in the `kubernetes` realm to set `org_id` and `account_number` on that user's **Attributes**. Use the synthetic values for the deployment's default org (the values in this test setup are `1234567` and `7890123`). Save, sign out, and sign in again so the new token contains both claims.

For the ordinary Google-linked test user, leave `org-admin` and all other Cost roles unassigned. This verifies authentication and tenant identification without granting permissions. A separate bootstrap administrator needs the `org-admin` realm role to manage access in User Access and assign roles to users; see the [Keycloak install guide](../install/keycloak.md#org-admin-realm-role-first-administrator). Do not infer admin access from email or Google Workspace domain.

The UI client secret mirror remains a separate setup step; use the [OAuth client Secret guide](../operations/cost-management-ui-oauth-client-setup.md) or the full BYOI script. Never copy the Google client secret into that Secret.

## Expired fresh-cluster attempt (2026-11-19, first cluster)

The new cluster was OCP 4.22.22 with six `amd64` workers, default `gp3-csi` storage, and apps domain `apps.chat-bot-q65hs-hwtsxt.crt-mce-aws.devcluster.openshift.com`. It was provisioned at 06:38 UTC with an announced lifetime of about 440 minutes. Its API later returned DNS `NXDOMAIN`; it had expired by the time the RHBK preflight ran. Do not reuse its API or apps hostname.

Completed before expiry:

- Ran [clusterbot-smoke.sh](../../scripts/clusterbot-smoke.sh): PostgreSQL, Valkey, MinIO, Redpanda, app Secrets, and `cost-byoi/cost-management` were created. All four infrastructure deployments rolled out and `minio-init` completed.
- Built the operator from clean KSO commit `1d285d6`, stored it in the `cost-byoi/koku-service-operator` ImageStream, and deployed it with [deploy-incluster.sh](../../scripts/deploy-incluster.sh). The operator rollout succeeded.
- The temporary OpenShift image-registry default Route was disabled before the operator image was built and deployed.

Build notes: the Mac `--from-dir` source archive was rejected by the OpenShift build pod. A ZIP upload worked, but the stock Dockerfile then failed when the restricted builder could not write binaries in `/workspace`. A temporary build-context Dockerfile directing build outputs to `/tmp` succeeded. The repository Dockerfile was not changed. The built image tag was `cost-8520-oidc-20261119-1d285d6` with registry digest `sha256:97a77262c8317b6cb961100206900103c2239ced8fc4f3267ededd6964808600`.

The RHBK script stopped at its read-only `kubectl cluster-info` prerequisite with DNS `NXDOMAIN`; it had not created the `keycloak` namespace or any RHBK resources. The Google/Keycloak Admin API helper was not run, no new Google callback was generated, and there was no fresh browser login or rbac-go sync. This attempt was superseded by the second fresh-cluster rerun below.

## Fresh ClusterBot rerun (2026-11-19)

The second new cluster used the API `api.chat-bot-ixx9x-cpm9aq.crt-mce-aws.devcluster.openshift.com`, apps domain `apps.chat-bot-ixx9x-cpm9aq.crt-mce-aws.devcluster.openshift.com`, six amd64 workers, and `gp3-csi`. The old `clusterbot` and `clusterbox` contexts were removed before installing the new cluster as `clusterbot`; the user's original active context was restored and preserved. Cluster operations used a separate kubeconfig containing only the new context.

Completed on the new cluster:

- Ran [clusterbot-smoke.sh](../../scripts/clusterbot-smoke.sh) for BYOI dependencies and the sample CMSC, installed RHBK, and imported the `kubernetes` realm.
- Built and deployed KSO commit `1d285d6` as image tag `cost-8520-oidc-20261119-1d285d6`, digest `sha256:4f0b130d91434d1a6e650d027bbb42ff2608c2759498f118d982e9f98d2db490`.
- Ran the [Keycloak/rbac-mini setup helper](../../scripts/oidc/configure-keycloak-rbac-mini.py). It configured Google federation, created `rbac-keycloak-sync`, assigned only the four required read-only realm-management roles, verified its token roles/audience/API reads, and created `cost-byoi/keycloak-client-secret-rbac-sync` with key `CLIENT_SECRET`.
- Set the CMSC public issuer to `https://keycloak-keycloak.apps.chat-bot-ixx9x-cpm9aq.crt-mce-aws.devcluster.openshift.com/realms/kubernetes`; kept the in-cluster Service URL for JWKS and sync. Added the public Route certificate bundle as Secret `cost-byoi/keycloak-ingress-ca` (`ca.crt`) so oauth2-proxy can validate the issuer with TLS verification enabled.
- Switched to `spec.rbac.backend: go` with `quay.io/martin_povolny/rbac-go:clusterbot-20261118-c747f58`, ran both schema migration jobs, and reached CMSC `Available=True`. The sync schedule is `0 0 1 1 *`, with `pruneOrphans=true` on the fresh test database.
- Ran `cost-management-rbac-keycloak-sync-manual-20261119-1`; it completed successfully with empty logs. Read-only API queries confirmed the bootstrap `admin` is active with `is_org_admin=true` and has two default admin permissions.
- Verified the UI route returns HTTP 302 into Keycloak, public realm discovery reports the configured issuer, and the Keycloak login page contains the Google provider. After the user registered the callback in Google Cloud Console, a verified request to Google's authorization endpoint used exactly:

      https://keycloak-keycloak.apps.chat-bot-ixx9x-cpm9aq.crt-mce-aws.devcluster.openshift.com/realms/kubernetes/broker/google/endpoint

The user's first login returned HTTP 500 at the Cost `/oauth2/callback`. A redacted oauth2-proxy log showed `email in id_token ... isn't verified`. Keycloak's Google identity provider had `trustEmail=false`, and the already-created Google-federated user had `emailVerified=false`. The fix matched the previous successful run: set **Trust Email** on the Google identity provider and mark the existing federated user's Email verified after confirming the Google identity's verified email. A fresh Google login then succeeded. If the Google OAuth consent screen is in Testing mode, the account must be listed as a test user; since the request reached the Cost callback, that gate had passed.

The isolated Playwright browser stopped on `ERR_CERT_AUTHORITY_INVALID` for the OpenShift ingress certificate; certificate validation was not disabled. The user continued in their own browser, trusted the cluster certificate, and completed Google sign-in. A read-only check then found the Google user's `org_id=1234567` and `account_number=7890123` attributes, but no membership in the existing top-level `/org-1234567` group and no `org-admin` role. rbac-go had an active principal with `is_org_admin=false`; `/access/` returned zero permissions. Add the user to `/org-1234567`, run one sync after the membership change, then have a Cost org-admin assign the needed Cost RBAC group/role. Follow the [setup guide](KEYCLOAK-RBAC-MINI.md) for the steps.

## Observed rbac-go backend and authorization result (2026-11-18)

The ClusterBot test selected the existing operator's Go backend and sync
configuration; no operator source change was required for this brokered test.
The CMSC used `spec.rbac.backend: go`, an amd64 rbac-go image, and
`spec.rbac.keycloakSync` with a Secret reference to the `CLIENT_SECRET` key.
The CronJob schedule was set in the future, one manual Job was run, and sync was
then disabled while leaving the Go backend active.

The sync client in the `kubernetes` realm needed the read-only
`realm-management` roles `query-groups`, `query-users`, `view-realm`, and
`view-users`, plus the built-in `roles` scope and a `kc-admin-api` scope mapping
those roles and adding the `realm-management` audience. The service-account
client secret was stored in the app namespace and was rotated during setup;
never record it in docs or logs. `pruneOrphans` remained true, its operator
default, for this test.

The sync made the Google-linked user an active principal with no `org-admin`
flag. The user could authenticate to the Cost UI, but had no Cost RBAC group or
role assignment, so `/access/` returned zero permissions and the UI denied
access. Membership in the Keycloak `org-<org_id>` group discovers the tenant
and principals; it does not itself assign Cost permissions. A separate
bootstrap administrator must assign a Cost RBAC role through the UI. The
completed Job record was removed when sync was disabled, so capture its status
and logs before disabling it on future runs.

This is a configuration and integration result, not direct OIDC support. The
operator currently has a Keycloak auth configuration and Keycloak Admin API
sync. A customer-facing direct OIDC mode still needs an additive API, JWT and
identity-claim wiring, and a principal onboarding contract; see [PLAN.md](PLAN.md).

## Direct OIDC follow-up after implementation

The current operator only supports Keycloak; do not treat the broker test as direct OIDC acceptance. After COST-8520 implementation, a direct Google client will use the Cost UI callback instead of the Keycloak broker callback. With the default namespace and CR name, its callback is normally:

    https://cost-management-ui-cost-byoi.<apps-domain>/oauth2/callback

Confirm the UI Route hostname and exact redirect value emitted by oauth2-proxy before registering it; the current route and redirect are constructed in [UIDeployment](../../internal/resources/ui.go#L153-L170) and [UIRoute](../../internal/resources/ui.go#L323-L355). Keycloak builds its broker callback from the provider alias; see the [Keycloak identity brokering guide](https://www.keycloak.org/docs/latest/server_admin/). Use a separate Google OAuth Web client for direct mode so its client credentials are not confused with the Google-to-Keycloak broker client or the Keycloak UI client. The direct client ID/secret would be placed in the UI OAuth Secret used by oauth2-proxy, not in the Keycloak Google identity-provider configuration.

Google OIDC does not provide Cost org_id, account_number, or org-admin by default; see the [Google OpenID Connect claims](https://developers.google.com/identity/openid-connect/openid-connect). Follow the explicit single-tenant/claim mapping and admin-false rules in [DESIGN.md](DESIGN.md); do not infer admin from email or hosted domain.

## Acceptance checks for this baseline

- The Google callback URL exactly matches the Keycloak identity-provider Redirect URI.
- Google login returns the browser to the ClusterBot Keycloak Route.
- The Cost UI redirects through Keycloak and completes its own callback.
- A fresh token for the ordinary Google-linked user carries org_id and account_number, with no org-admin role; requests pass Envoy tenant-claim validation. With no Cost roles, the user has no Cost permissions.
- Separately, a bootstrap administrator with org-admin can use User Access to assign roles when validating role delegation.
- The tunnel is absent or stopped after the run; OAuth secrets remain outside the repository.

This baseline validates Keycloak brokering and cluster reachability only. It does not validate direct OIDC support in the operator, generic claim mapping, or generic user/group provisioning.
