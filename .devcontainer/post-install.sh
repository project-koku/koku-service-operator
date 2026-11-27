#!/bin/bash
set -euo pipefail
set -x

# This hook runs inside the Linux devcontainer. Keep the OS guard explicit so
# accidental host-side execution fails clearly instead of downloading the wrong
# binaries.
os=$(uname -s | tr '[:upper:]' '[:lower:]')
arch=$(uname -m)

case "$os" in
  linux) ;; 
  *)
    echo "Unsupported OS: $os" >&2
    exit 1
    ;;
esac

case "$arch" in
  x86_64|amd64)
    arch=amd64
    ;;
  aarch64|arm64)
    arch=arm64
    ;;
  *)
    echo "Unsupported architecture: $arch" >&2
    exit 1
    ;;
esac

curl -Lo ./kind "https://kind.sigs.k8s.io/dl/latest/kind-${os}-${arch}"
chmod +x ./kind
mv ./kind /usr/local/bin/kind

curl -L -o kubebuilder "https://go.kubebuilder.io/dl/latest/${os}/${arch}"
chmod +x kubebuilder
mv kubebuilder /usr/local/bin/

KUBECTL_VERSION=$(curl -L -s https://dl.k8s.io/release/stable.txt)
curl -Lo kubectl "https://dl.k8s.io/release/$KUBECTL_VERSION/bin/${os}/${arch}/kubectl"
chmod +x kubectl
mv kubectl /usr/local/bin/kubectl

if ! docker network inspect kind >/dev/null 2>&1; then
  docker network create -d=bridge --subnet=172.19.0.0/24 kind
fi

kind version
kubebuilder version
docker --version
go version
kubectl version --client
