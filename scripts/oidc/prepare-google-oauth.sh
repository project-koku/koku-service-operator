#!/usr/bin/env bash
# Prepare a Google Cloud project and print the exact Keycloak broker callback.
# Google requires creating/configuring the Google Sign-In OAuth client in the
# Cloud Console; this script intentionally does not call unsupported/private APIs.
set -euo pipefail

usage() {
  cat <<'EOF'
Usage:
  scripts/oidc/prepare-google-oauth.sh --project-id ID [options]

Options:
  --project-id ID        Existing or new Google Cloud project ID (required).
  --create-project       Create the project if it is not visible to the active account.
  --project-name NAME    Display name when creating a project.
  --organization ID      Parent organization when creating a project.
  --folder ID            Parent folder when creating a project.
  --keycloak-url URL     Public Keycloak base URL; defaults to the clusterbot route.
  --cluster-context NAME Context used to discover the route (default: clusterbot).
  --keycloak-namespace NS Namespace used to discover the route (default: keycloak).
  -h, --help             Show this help.

The script does not create an OAuth client or change gcloud's default project.
EOF
}

die() {
  printf 'error: %s\n' "$*" >&2
  exit 1
}

PROJECT_ID=""
PROJECT_NAME="COST-8520 Google OIDC test"
CREATE_PROJECT=0
ORGANIZATION_ID=""
FOLDER_ID=""
KEYCLOAK_URL=""
CLUSTER_CONTEXT="${CLUSTERBOT_CONTEXT:-clusterbot}"
KEYCLOAK_NAMESPACE="${KEYCLOAK_NAMESPACE:-keycloak}"

while (($#)); do
  case "$1" in
    --project-id)
      (($# >= 2)) || die "--project-id requires a value"
      PROJECT_ID="$2"
      shift 2
      ;;
    --create-project)
      CREATE_PROJECT=1
      shift
      ;;
    --project-name)
      (($# >= 2)) || die "--project-name requires a value"
      PROJECT_NAME="$2"
      shift 2
      ;;
    --organization)
      (($# >= 2)) || die "--organization requires a value"
      ORGANIZATION_ID="$2"
      shift 2
      ;;
    --folder)
      (($# >= 2)) || die "--folder requires a value"
      FOLDER_ID="$2"
      shift 2
      ;;
    --keycloak-url)
      (($# >= 2)) || die "--keycloak-url requires a value"
      KEYCLOAK_URL="$2"
      shift 2
      ;;
    --cluster-context)
      (($# >= 2)) || die "--cluster-context requires a value"
      CLUSTER_CONTEXT="$2"
      shift 2
      ;;
    --keycloak-namespace)
      (($# >= 2)) || die "--keycloak-namespace requires a value"
      KEYCLOAK_NAMESPACE="$2"
      shift 2
      ;;
    -h|--help)
      usage
      exit 0
      ;;
    *)
      usage >&2
      die "unknown argument: $1"
      ;;
  esac
done

[[ -n "$PROJECT_ID" ]] || { usage >&2; die "--project-id is required"; }
[[ "$PROJECT_ID" =~ ^[a-z][a-z0-9-]{4,28}[a-z0-9]$ ]] || die "project ID must be 6-30 lowercase letters, digits, or hyphens; start with a letter and end with a letter or digit"
[[ -z "$ORGANIZATION_ID" || -z "$FOLDER_ID" ]] || die "use --organization or --folder, not both"
if [[ "$CREATE_PROJECT" -eq 0 && ( -n "$ORGANIZATION_ID" || -n "$FOLDER_ID" ) ]]; then
  die "--organization and --folder are only used with --create-project"
fi
command -v gcloud >/dev/null 2>&1 || die "gcloud CLI is required"

ACTIVE_ACCOUNT="$(gcloud auth list --filter='status:ACTIVE' --format='value(account)')"
[[ -n "$ACTIVE_ACCOUNT" ]] || die "no active gcloud account; run gcloud auth login first"
printf 'Active gcloud account: %s\n' "$ACTIVE_ACCOUNT"

if [[ -z "$KEYCLOAK_URL" ]]; then
  command -v oc >/dev/null 2>&1 || die "oc is needed to discover the Keycloak route; pass --keycloak-url to avoid this dependency"
  route_host="$(oc --context="$CLUSTER_CONTEXT" -n "$KEYCLOAK_NAMESPACE" get route keycloak -o jsonpath='{.spec.host}')"
  [[ -n "$route_host" ]] || die "could not read route keycloak in namespace $KEYCLOAK_NAMESPACE using context $CLUSTER_CONTEXT"
  KEYCLOAK_URL="https://${route_host}"
fi
KEYCLOAK_URL="${KEYCLOAK_URL%/}"
[[ "$KEYCLOAK_URL" =~ ^https://[^/]+$ ]] || die "--keycloak-url must be an HTTPS origin with no path"

PROJECT_VISIBLE=0
if gcloud projects describe "$PROJECT_ID" --account="$ACTIVE_ACCOUNT" --format='value(projectId)' >/dev/null 2>&1; then
  PROJECT_VISIBLE=1
fi

if [[ "$PROJECT_VISIBLE" -eq 0 ]]; then
  [[ "$CREATE_PROJECT" -eq 1 ]] || die "project $PROJECT_ID is not visible; create it first or rerun with --create-project"
  create_args=(projects create "$PROJECT_ID" "--name=$PROJECT_NAME" --no-enable-cloud-apis "--account=$ACTIVE_ACCOUNT" --quiet)
  [[ -z "$ORGANIZATION_ID" ]] || create_args+=("--organization=$ORGANIZATION_ID")
  [[ -z "$FOLDER_ID" ]] || create_args+=("--folder=$FOLDER_ID")
  gcloud "${create_args[@]}"
else
  printf 'Project %s is accessible.\n' "$PROJECT_ID"
fi

gcloud projects describe "$PROJECT_ID" --account="$ACTIVE_ACCOUNT" --format='value(projectId)' >/dev/null
CALLBACK_URI="${KEYCLOAK_URL}/realms/kubernetes/broker/google/endpoint"
CONSOLE_URL="https://console.cloud.google.com/auth/clients?project=${PROJECT_ID}"

cat <<EOF

Google project is ready for the manual OAuth client configuration.
Project:                 ${PROJECT_ID}
Keycloak public URL:     ${KEYCLOAK_URL}
Google redirect URI:     ${CALLBACK_URI}
Google Auth Platform:    ${CONSOLE_URL}

In Google Auth Platform:
  1. Set the audience for your test account (External + Testing, or Internal
     if the account belongs to your Google Workspace organization).
  2. Create a Web application OAuth client and add the redirect URI above.
  3. Leave Authorized JavaScript origins empty for this server-side broker.
  4. Keep the client ID and secret private; enter them only in Keycloak's
     Identity Providers > Google configuration (alias: google).

Google requires OAuth clients to be configured in Cloud Console. This script
creates or checks only the project; it does not print, store, or request a
client secret.
EOF
