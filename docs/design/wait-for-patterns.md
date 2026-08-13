# Operator Engineering Reference

Resources and projects researched while building koku-service-operator.
Maintained outside the repo so it's accessible across conversations.

---

## Wait-for / readiness patterns in Go operators

### Problem
Init containers need to block until TCP endpoints or HTTP services are ready.
The naive approach (bash + `/dev/tcp`) is bash-specific, shell-injection-prone,
and not available in distroless images.

### What we researched

**prometheus-operator/prometheus-operator**
https://github.com/prometheus-operator/prometheus-operator
Uses CLI flags (`--prometheus-config-reloader=...`) for every sidecar image.
Defaults are assembled at runtime from a version string injected at build time
via `-ldflags "-X github.com/prometheus/common/version.Version=vX.Y.Z"`.
No env vars, no Downward API.

**cert-manager/cert-manager**
https://github.com/cert-manager/cert-manager
Same pattern: `pkg/util/version.go` holds `var AppVersion = "canary"`,
overridden by ldflags. Default sidecar image is `quay.io/jetstack/cert-manager-acmesolver:$(AppVersion)`,
exposed as `--acme-http01-solver-image` CLI flag.

**grafana/grafana-operator**
https://github.com/grafana/grafana-operator
Uses OLM `RELATED_IMAGE_GRAFANA` env var (`os.Getenv("RELATED_IMAGE_GRAFANA")`),
only consumed when value contains `@sha256:` (digest-pinned) for airgap support.
Plain `value:` in manager.yaml; OLM replaces at deploy time.

**openshift/cluster-monitoring-operator** (Red Hat)
https://github.com/openshift/cluster-monitoring-operator
Custom `--images key=value,...` CLI flag. No env vars, no ldflags for images.
Whoever launches the operator passes all image references explicitly.

### What we chose

**`--operator-image` CLI flag** — follows the CMO/cert-manager pattern.
Set in `config/manager/manager.yaml` args; kustomize `replacements` keeps
it in sync with the container `image:` field automatically.
OLM CSV injects it at deploy time. Fully registry-agnostic and overridable.

---

## wait4x — TCP/HTTP readiness library

**Repo:** https://github.com/wait4x/wait4x
**Import path:** `wait4x.dev/v3`
**License:** Apache 2.0
**Version used:** v3.6.0 (October 2025, actively maintained)

Replaces bash + `/dev/tcp` init containers with a purpose-built Go binary
(`cmd/wait-for/`) compiled alongside the manager in the same Dockerfile stage.
No separate image needed — init containers reuse the operator image.

**API used:**
```go
tcpchecker.New("host:port")          // TCP check
httpchecker.New(url, WithExpectStatusCode(200))  // HTTP check
waiter.WaitContext(ctx, checker, WithInterval(2*time.Second))
```

**Why wait4x over alternatives:**
- `groundnuty/k8s-wait-for` — shell script, waits for K8s objects, not TCP
- `bitnami/wait-for-port` — TCP only, no HTTP
- `patrickdappollonio/wait-for` — minimal/scratch, HTTP 2xx range built-in,
  good alternative if image size matters more than ecosystem breadth
- Rolling our own — 50-line option, avoids the dependency entirely

**CVE note:** wait4x's HTTP checker pulls `github.com/antchfx/xpath` for
XPath body matching. Ensure `xpath >= v1.3.6` (fixes GO-2026-4526 / CVE-2026-32287,
infinite loop on boolean XPath expressions). Verified in go.mod.

---

## Kubernetes Downward API — what actually works for env vars

`fieldRef.fieldPath` supports: `metadata.name`, `metadata.namespace`,
`metadata.uid`, `metadata.labels[key]`, `metadata.annotations[key]`,
`spec.nodeName`, `spec.serviceAccountName`, `status.podIP`, `status.hostIP`.

**`spec.containers[0].image` is NOT supported** — Kubernetes API rejects it.
Use a CLI flag, OLM RELATED_IMAGE env var, or ldflags instead.

