# OwnNamespace RBAC + Cache — Implementation Plan

> **Goal:** Fix Critical finding #3 (cluster-wide Secrets/Jobs access) per Moti’s design and JIRA direction: **OwnNamespace** — one operator instance watches/manages **one** app namespace (where the CR lives). BYOI may live elsewhere; only narrow cluster-scoped access is via ClusterRoleBinding.

> **Worktree:** `.worktrees/fix-ownnamespace-rbac` · **Branch:** `fix/ownnamespace-rbac-cache`

## Model (locked)

| Concern | Rule |
|---------|------|
| Operator install NS | Same as CR / operand NS |
| Cache `DefaultNamespaces` | That watch NS only |
| Secrets/Jobs/Deployments/… | `manager-role` ClusterRole + **RoleBinding** in watch NS |
| ConsoleLink, StorageClass, Ingress discovery, Kruize CR/CRB | `manager-cluster-role` + **ClusterRoleBinding** |
| NooBaa `openshift-storage/noobaa-admin` | `get` secrets `resourceNames: [noobaa-admin]` on cluster role + **uncached APIReader** |
| BYOI infra NS | Connect via CR; do not informer-watch or own |

Not in scope: full OLM CSV/`installModes` (COST-7695) — document OwnNamespace intent for the future CSV.

## Files

| File | Responsibility |
|------|----------------|
| `cmd/main.go` / `cmd/main_test.go` | `watchNamespace()`, cache restriction, wire `APIReader` |
| `internal/controller/*` | Markers, `APIReader` field, NooBaa uses APIReader |
| `config/rbac/role_binding.yaml` | RoleBinding |
| `config/rbac/cluster_access_*.yaml` | Minimal cluster access |
| `config/rbac/role.yaml` | Regenerated via `make manifests` |
| `hack/deploy-crc.sh` | OwnNamespace RBAC for CRC |
| `Makefile` `run` | Require/pass `NAMESPACE` |
| `README.md`, `docs/development/crc-testing.md`, `docs/development/ownnamespace.md` | Operator model docs |
| `docs/gap_analysis/COST-7683.md` | Update Secrets blast-radius note |
| `config/samples/byoi/README.md` | Clarify operator NS == CR NS vs BYOI infra NS |
| `config/default/kustomization.yaml` | Comment OwnNamespace constraint |
| `config/rbac/*_test.go` or `internal/controller/rbac_manifest_test.go` | Assert RoleBinding + cluster_access shape |
| `internal/controller/discovery_s3_test.go` | NooBaa prefers APIReader |

## Tasks

### T1 — Finish / verify controller + RBAC manifests
1. Confirm kubebuilder markers (split roles vs clusterroles; noobaa-admin get).
2. `make manifests` — regenerate `role.yaml`; ensure Check Generated will pass.
3. Confirm `role_binding.yaml` is `RoleBinding`; cluster_access files present in kustomization.

### T2 — Tests (must exist and fail without the fix where applicable)
1. **`cmd`:** `watchNamespace` empty / from `NAMESPACE` (done); add SA-file branch via injectable path var.
2. **`discovery_s3`:** NooBaa Get uses `APIReader` when set (Client without the secret → still succeeds via APIReader).
3. **RBAC manifests:** parse YAML — manager binding is RoleBinding; cluster_access has `noobaa-admin` resourceNames and does **not** grant blanket `secrets` list/watch; `manager-role` still lists secrets (for RoleBinding scope).

### T3 — Deploy scripts / Makefile
1. `hack/deploy-crc.sh` — already RoleBinding + cluster binding; keep idempotent.
2. `Makefile` `run` — fail or warn if `NAMESPACE` unset; document `NAMESPACE=… make run`.

### T4 — Documentation & examples
1. `docs/development/ownnamespace.md` — short canonical model (operator NS, CR NS, BYOI NS, cluster exceptions).
2. `README.md` — replace misleading “Operator namespace = koku-service-operator-system” with OwnNamespace + link; note `make deploy` scaffold NS.
3. `docs/development/crc-testing.md` — fix outdated “cluster-admin” wording; describe RoleBinding + cluster_access; require `NAMESPACE`.
4. `docs/gap_analysis/COST-7683.md` — update G2/Secrets cluster-wide note to reflect narrow NooBaa get.
5. `config/samples/byoi/README.md` — operator/CR co-location vs `cost-byoi-infra`.
6. `config/default/kustomization.yaml` — comment: OwnNamespace ⇒ CR must be in this deploy NS (or change namespace when packaging).
7. `CLAUDE.md` / workspace rule — one-line OwnNamespace pointer if present in worktree.

### T5 — Verification
1. `go test ./cmd/... ./internal/controller/...`
2. `golangci-lint` / `make manifests` drift check
3. Manual review: no doc still saying manager ClusterRoleBinding grants all Secrets cluster-wide

### T6 — Commit + upstream PR
1. Single or two commits (code+tests, then docs) matching repo style
2. PR against `project-koku/koku-service-operator` with model summary + test plan

## Done when
- [ ] Cluster-wide Secrets CRUD via manager ClusterRoleBinding is gone
- [ ] Cache restricted to watch NS; NooBaa still works via APIReader + narrow RBAC
- [ ] Tests cover watch NS, APIReader path, RBAC YAML shape
- [ ] CRC/README/BYOI/gap docs and deploy scripts match OwnNamespace
- [ ] CI green on PR
