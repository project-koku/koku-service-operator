#!/usr/bin/env bash
# AllNamespaces in-cluster deploy: CRDs + RBAC + manager Deployment.
# Suggested NS is cost-onprem; the manager watches CMSC in every namespace.
# BYOI infra may live elsewhere (CR connection fields).
#
# Prefer this over `make run` when the CR points at in-cluster BYOI hosts
# (*.svc.cluster.local) — those names are not resolvable from a laptop.
#
# Usage (from repo root):
#   IMG=quay.io/example/koku-service-operator:tag ./hack/deploy-incluster.sh cost-byoi
#   IMG=... ./hack/deploy-incluster.sh cost-tests
#
# Build an amd64 image for typical OpenShift nodes:
#   docker buildx build --platform linux/amd64 -t "$IMG" --push .
#
# The manager always registers admission webhooks and expects TLS material at
# /tmp/k8s-webhook-server/serving-certs. This script creates a lab-only
# self-signed Secret and mounts it so the pod can start. It does NOT install
# ValidatingWebhookConfiguration / MutatingWebhookConfiguration — CR apply is
# not admission-gated in this path (OLM/cert-manager covers that for real
# installs).
#
set -euo pipefail

NS="${1:-cost-byoi}"
IMG="${IMG:?IMG is required (e.g. quay.io/<org>/koku-service-operator:<tag>)}"
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

WEBHOOK_SECRET="${WEBHOOK_SECRET:-koku-webhook-server-cert}"

if ! command -v oc >/dev/null 2>&1; then
  echo "error: oc is required" >&2
  exit 1
fi
if ! command -v openssl >/dev/null 2>&1; then
  echo "error: openssl is required to generate lab webhook serving certs" >&2
  exit 1
fi

echo "=== In-cluster AllNamespaces deploy ==="
echo "Namespace: $NS"
echo "Image:     $IMG"
echo "Cluster:   $(oc whoami --show-server)"
echo ""

# CRDs + ClusterRoleBindings (default SA) + anyuid SCC.
./hack/deploy-dev.sh "$NS"

echo "[in-cluster] Ensuring webhook serving-cert Secret (${WEBHOOK_SECRET})..."
# controller-runtime defaults to tls.crt / tls.key under this mount path.
CERT_DIR="$(mktemp -d)"
cleanup_certs() { rm -rf "$CERT_DIR"; }
trap cleanup_certs EXIT
openssl req -x509 -newkey rsa:2048 -nodes \
  -keyout "${CERT_DIR}/tls.key" -out "${CERT_DIR}/tls.crt" -days 365 \
  -subj "/CN=koku-service-operator.${NS}.svc" \
  >/dev/null 2>&1
oc -n "$NS" create secret tls "$WEBHOOK_SECRET" \
  --cert="${CERT_DIR}/tls.crt" --key="${CERT_DIR}/tls.key" \
  --dry-run=client -o yaml | oc apply -f -

echo "[in-cluster] Applying manager Deployment (SA=default)..."
oc apply -f - <<EOF
apiVersion: apps/v1
kind: Deployment
metadata:
  name: koku-service-operator
  namespace: ${NS}
  labels:
    app.kubernetes.io/name: koku-service-operator
    control-plane: controller-manager
spec:
  replicas: 1
  selector:
    matchLabels:
      control-plane: controller-manager
      app.kubernetes.io/name: koku-service-operator
  template:
    metadata:
      labels:
        control-plane: controller-manager
        app.kubernetes.io/name: koku-service-operator
    spec:
      serviceAccountName: default
      securityContext:
        runAsNonRoot: true
        seccompProfile:
          type: RuntimeDefault
      containers:
      - name: manager
        image: ${IMG}
        imagePullPolicy: Always
        command: ["/manager"]
        args:
        - --leader-elect
        - --health-probe-bind-address=:8081
        - --operator-image=${IMG}
        securityContext:
          allowPrivilegeEscalation: false
          capabilities:
            drop: ["ALL"]
        livenessProbe:
          httpGet:
            path: /healthz
            port: 8081
          initialDelaySeconds: 15
          periodSeconds: 20
        readinessProbe:
          httpGet:
            path: /readyz
            port: 8081
          initialDelaySeconds: 5
          periodSeconds: 10
        resources:
          requests:
            cpu: 10m
            memory: 64Mi
        volumeMounts:
        - name: webhook-certs
          mountPath: /tmp/k8s-webhook-server/serving-certs
          readOnly: true
      volumes:
      - name: webhook-certs
        secret:
          secretName: ${WEBHOOK_SECRET}
EOF

oc -n "$NS" rollout status deploy/koku-service-operator --timeout=180s

echo ""
echo "Operator is running in ${NS}."
echo ""
echo "If you already ran ./hack/clusterbot-smoke.sh, Secrets + the Redpanda smoke CR"
echo "are applied — watch conditions (do not re-apply the AMQ Streams sample CR;"
echo "that would overwrite bootstrapServers):"
echo "  oc -n ${NS} get cmsc -w"
echo "Day-one (no Keycloak): SchemaUpToDate + Available=True (KokuAvailable)."
echo "Phase stays Progressing until UIReady — see docs/development/clusterbot.md"
echo ""
echo "Otherwise apply Secrets + CostManagementServiceConfig in ${NS}, then for UI:"
echo "mirror the OAuth client Secret and open the UI Route."
echo "Full UI path: docs/development/pre-prod-install.md"
