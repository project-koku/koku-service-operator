# Google Auth setup for the ClusterBot experiment

This runbook prepares Google as an upstream identity provider for the existing Keycloak broker. It does not implement direct Google OIDC in the Cost Management operator; see [DESIGN.md](DESIGN.md) and [PLAN.md](PLAN.md).

## What the script automates

Run [prepare-google-oauth.sh](../../scripts/oidc/prepare-google-oauth.sh) with your own active `gcloud` account. It can create a project or verify access to an existing project, discovers the ClusterBot Keycloak Route (or accepts an explicit URL), and prints the exact Google callback URI. It does not change your default `gcloud` project and does not store credentials.

For example, to use an existing project:

```bash
./scripts/oidc/prepare-google-oauth.sh --project-id YOUR_PROJECT_ID
```

To create a project first:

```bash
./scripts/oidc/prepare-google-oauth.sh \
  --project-id YOUR_UNIQUE_PROJECT_ID \
  --project-name 'COST-8520 Google OIDC test' \
  --create-project
```

Use `--organization ID` or `--folder ID` with `--create-project` if the new project belongs under one of those parents. Project IDs are globally unique and immutable. The script does not enable unrelated Google APIs or attach billing.

Google does not provide a supported command-line/API path for creating or modifying Google Sign-In OAuth clients: its policy requires that step in Cloud Console. The similarly named [`gcloud iam oauth-clients`](https://docs.cloud.google.com/sdk/gcloud/reference/iam/oauth-clients) manages IAM OAuth clients for Workforce Identity Federation users accessing Google Cloud resources; it is not a Google Account Sign-In client. See Google's [OAuth client creation policy](https://developers.google.com/identity/protocols/oauth2/resources/best-practices) and the [IAM OAuth client resource](https://docs.cloud.google.com/iam/docs/reference/rest/v1/projects.locations.oauthClients).

## Required Cloud Console step

Open the Google Auth Platform Clients page printed by the script. Set the audience to **External / Testing** and add your test account, or use **Internal** if this is a Google Workspace project and the test account is in that organization. Configure the consent screen's app name and support/contact email. Keep scopes limited to `openid`, `email`, and `profile` for this sign-in test.

Create an OAuth client with application type **Web application**. Leave **Authorized JavaScript origins** empty; Keycloak performs the server-side broker flow. Add this exact URI under **Authorized redirect URIs**:

```text
https://<current-keycloak-route>/realms/kubernetes/broker/google/endpoint
```

ClusterBot hostnames are ephemeral. The 2026-11-18 and 2026-11-19 test-cluster callbacks are historical and expired; do not reuse them. After reprovisioning, rerun the helper and register its exact callback. Google requires an exact match including scheme, hostname, path, and trailing-slash behavior; see the [Google web-server OAuth guide](https://developers.google.com/identity/protocols/oauth2/web-server).

The redirect domain must be one you own, are authorized to use, or are licensed to use under Google's [OAuth policy](https://developers.google.com/identity/protocols/oauth2/policies). If Google rejects the ClusterBot domain, use a hostname/domain arrangement approved for this project.

## Configure Keycloak

In the `kubernetes` realm, add **Google** under **Identity Providers** with alias `google`. Enter the Google client ID and secret, keep the basic OIDC scopes, and save. Do not put the Google secret in the Cost UI OAuth Secret: that Secret is a separate Keycloak-client credential used by `oauth2-proxy`.

Keep the Google client secret out of the repository and chat. Enter it directly in Keycloak or use a secure secret-management path. Google shows a client secret only at creation time; store it securely.

For the repeatable broker plus rbac-mini setup, use the [Keycloak/rbac-mini guide](KEYCLOAK-RBAC-MINI.md) and the [configure-keycloak-rbac-mini.py helper](../../scripts/oidc/configure-keycloak-rbac-mini.py). The helper can set the Google identity provider and the dedicated sync client in one run. It can read the Google client ID/secret from the Cloud Console JSON file or prompt for the secret, can read the RHBK initial-admin Secret or prompt for the password, validates the sync service-account token, and writes the sync client credential to the explicitly selected namespace as CLIENT_SECRET. Keep the OAuth JSON outside the repository. The Google Web OAuth client itself still has to be created in Cloud Console.

### Admin Console and bootstrap credentials

For a fresh cluster, use the current Keycloak Route host with this Admin Console URL pattern:

```text
https://<current-keycloak-route>/admin/
```

ClusterBot hostnames can expire. Keep the normal kubeconfig context unchanged and use the isolated kubeconfig for this test:

```bash
export KUBECONFIG=/path/to/isolated-clusterbot.kubeconfig
CTX="$(oc config current-context)"
oc --context="$CTX" -n keycloak get route keycloak -o jsonpath='{.spec.host}'
```

Use the printed host with `https://` and `/admin/`. Do not assume the username or password is `admin/admin`: RHBK generates both and stores them in `keycloak-initial-admin`. On macOS, retrieve them with:

```bash
oc --context="$CTX" -n keycloak get secret keycloak-initial-admin -o jsonpath='{.data.username}' | base64 -D; echo
oc --context="$CTX" -n keycloak get secret keycloak-initial-admin -o jsonpath='{.data.password}' | base64 -D; echo
```

On Linux, use `base64 -d` instead of `base64 -D`. Treat the password as a secret; do not paste it into chat or commit it.

### Set tenant claims on the Google-linked user

Google authenticates the person but does not provide Cost's `org_id` or `account_number`. The Keycloak client mappers read these from user attributes, so a Google-linked user must have both attributes before the Cost UI can pass Envoy's tenant-claim checks.

After the user's first Google sign-in creates or links the Keycloak account, select the `kubernetes` realm, open **Users**, select that user, then set `org_id` and `account_number` under **Attributes** to the values for the default on-prem org. The synthetic values in this test setup are `org_id=1234567` and `account_number=7890123`; use the values configured for the current deployment. Save, sign out, and sign in again so Keycloak issues a fresh token with the attributes.

For an unprivileged-user test, leave the `org-admin` realm role and all other Cost roles unassigned. The user can authenticate but will have no Cost permissions until an administrator grants them. Keep a separate bootstrap administrator with the `org-admin` realm role; that account can delegate access through User Access. See the [Keycloak install guide](../install/keycloak.md#org-admin-realm-role-first-administrator) for the role behavior.

## Cluster route note

The public Keycloak Route resolves from the public internet. During this experiment, the route certificate was issued by the cluster's self-signed ingress CA and failed the local system's normal TLS verification. The Keycloak discovery endpoint responded when certificate verification was disabled for diagnosis only. A normal browser may therefore show a certificate trust error; public DNS reachability alone does not establish trusted HTTPS. A production-like browser test needs a trusted certificate or a public hostname/tunnel with trusted TLS. Do not use `curl --insecure` as authentication validation.

## Scope of this experiment

This verifies Google sign-in through Keycloak and the existing broker flow. It does not validate direct Google OIDC in `oauth2-proxy`, provider-neutral claim mapping, or generic RBAC principal provisioning. Google profile claims do not supply Cost's `org_id`, `account_number`, or `org-admin` role; see the claim contract and limitations in [DESIGN.md](DESIGN.md).
