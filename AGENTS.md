# Agent instructions

Read [CLAUDE.md](CLAUDE.md) before making changes. It is the authoritative
guide for repository conventions, testing, and review requirements.

## Worktrees and tests

New Git worktrees do not include the ignored `bin/` directory. Before running
repository tooling or controller integration tests in a worktree, create the
machine-local symlink documented in `CLAUDE.md`:

```sh
ln -s <main-checkout>/bin <worktree>/bin
```

Use `make test` for the full Go test suite. It selects the envtest assets via
`KUBEBUILDER_ASSETS`; running the controller package directly without that
environment can fall back to a missing global Kubebuilder installation.

## Generated files (CRD and OLM bundle)

API or CRD changes touch **two** generated trees. Do not mix them up.

| Command | Updates | CI job |
|---------|---------|--------|
| `make generate` | `api/.../zz_generated.deepcopy.go` | `check-generated` |
| `make manifests` | `config/crd/bases/`, `config/webhook/`, RBAC YAML | `check-generated` |
| `make bundle` | `bundle/manifests/`, `bundle/metadata/`, `bundle.Dockerfile` | `bundle-validate` |

**Never** `cp config/crd/bases/*.yaml bundle/manifests/`. Operator-sdk adds
bundle-only fields (`creationTimestamp`, `status`, …) that a raw copy omits, so
`bundle-validate` fails with "bundle/ is out of sync".

After CRD or webhook marker changes:

```bash
make generate manifests   # if types or kubebuilder markers changed
make bundle             # always when bundle/ must ship the new CRD
git diff --stat bundle/ config/crd/
```

`bundle-validate` ignores CSV `createdAt` drift. If `git status` shows only
`createdAt` changed in `bundle/manifests/koku-service-operator.clusterserviceversion.yaml`
after a local `make bundle`, discard it (`git checkout --` that file) — it is
not a real sync gap.
