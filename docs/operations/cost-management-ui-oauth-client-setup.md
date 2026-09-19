# Configure the Cost Management UI OAuth Client

Use this guide when Keycloak or another compatible OIDC provider is managed
outside of the Cost Management service operator. It configures the OAuth client
that supports browser sign-in to the Cost Management UI.

This is a one-time identity-provider task. Create users and assign their
organization attributes separately after the client is configured.

## What this client does

The Cost Management UI includes oauth2-proxy, which redirects a browser to
your identity provider for sign-in. After a successful login, the provider
redirects the browser back to oauth2-proxy. oauth2-proxy exchanges the
authorization code using this client's ID and secret, then maintains the user
session.

The provider must issue an access token that contains the user's `org_id` and
`account_number` claims. The Cost Management gateway rejects a token that is
missing either claim.

## Before you begin

Collect the following values from the CostManagementServiceConfig and your
OpenShift cluster:

- The configured Keycloak realm. The default is `kubernetes`.
- The CostManagementServiceConfig name, namespace, and cluster apps domain.
- The UI callback URL:

  ```text
  https://{cr-name}-ui-{namespace}.{apps-domain}/oauth2/callback
  ```

- The UI base URL:

  ```text
  https://{cr-name}-ui-{namespace}.{apps-domain}
  ```

- The values configured in `spec.auth.keycloak.audiences`. Defaults are
  `cost-management-operator` and `cost-management-ui`.

The operator must be able to reach `spec.auth.keycloak.url` for JSON Web Key
Set discovery. If the token `iss` value uses a different public URL, set
`spec.auth.keycloak.issuerURL` to that issuer URL.

## Create the client

In the Keycloak Admin Console, select the realm configured for Cost Management
and create a client with these settings.

| Setting | Value |
|---|---|
| Client type | OpenID Connect |
| Client ID | `cost-management-ui`, or another stable client ID that you also store in the Kubernetes Secret |
| Client authentication | Enabled; this is a confidential client |
| Standard flow | Enabled |
| Direct access grants | Disabled in production |
| Valid redirect URI | The exact UI callback URL shown above |
| Web origins | The UI base URL |

Copy the client secret from the client's Credentials tab. Do not put this
secret in the CostManagementServiceConfig itself.

## Configure access token claims

### Organization claims

Create two User Attribute protocol mappers on the UI client, or put them on a
client scope that is assigned to the UI client.

| Mapper setting | `org_id` | `account_number` |
|---|---|---|
| Mapper type | User Attribute | User Attribute |
| User Attribute | `org_id` | `account_number` |
| Token Claim Name | `org_id` | `account_number` |
| Claim JSON Type | String | String |
| Add to access token | Enabled | Enabled |
| Add to ID token | Disabled | Disabled |
| Multivalued | Disabled | Disabled |

For each user, set both attributes. Use the same stable values for every user
in the same Cost Management organization. Each value must use only letters,
numbers, periods, underscores, or hyphens, and must be no more than 128
characters long.

### Audience claim

Ensure the access token `aud` claim includes at least one value from
`spec.auth.keycloak.audiences`. A dedicated client scope with Audience mappers
is recommended when several Cost Management clients share the same setup.

For the default CMSC settings, add these audiences to the access token:

- `cost-management-operator`
- `cost-management-ui`

### Administrator role

Create the `org-admin` realm role if it does not already exist. Assign this
realm role only to people who should administer Cost Management or delegate
User Access roles. Ensure the UI client includes realm roles in access tokens,
so `org-admin` appears in `realm_access.roles`.

An `org-admin` role in a client, or a Keycloak group with the same name, is not
equivalent to the required realm role.

## Store the client credentials in Kubernetes

Create a Secret in the same namespace as the CostManagementServiceConfig. Its
default name is `{cr-name}-ui-oauth-client`; use
`spec.ui.oauthClientSecretRef.name` if you choose a different name.

```yaml
apiVersion: v1
kind: Secret
metadata:
  name: {cr-name}-ui-oauth-client
  namespace: {namespace}
type: Opaque
stringData:
  client-id: cost-management-ui
  client-secret: replace-with-the-client-secret
```

Apply the Secret, then allow the operator to reconcile. The UI Deployment's
oauth2-proxy container reads these two values; neither key may be empty.

## Validate the configuration

1. Open the Cost Management UI URL in a private browser window.
2. Confirm that you are redirected to the identity provider and then returned
   to the UI after sign-in.
3. Inspect the resulting access token with your approved token-inspection
   method. Confirm that it includes `org_id`, `account_number`, a matching
   `aud` value, and `org-admin` in `realm_access.roles` for an administrator.
4. Confirm that an administrator can open Cost Management and User Access.

If `UIReady=False` has reason `OAuthClientSecretMissing`, check the Secret name
and its non-empty `client-id` and `client-secret` keys. If login returns to the
UI with an error, verify that the registered redirect URI exactly matches the
UI callback URL.
