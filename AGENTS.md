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
