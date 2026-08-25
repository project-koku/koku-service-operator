# Contributing

## Jira-linked branches and pull requests

Work tied to a Cost Management Jira ticket (`COST-####` on
[redhat.atlassian.net](https://redhat.atlassian.net)) should use consistent
branch and PR naming so reviews, release notes, and agent tooling stay aligned.

| Item | Format | Example |
|------|--------|---------|
| **Branch** | `cost-####-short-kebab-slug` | `cost-8136-skip-ros-pytest-when-disabled` |
| **PR title** | `[COST-####] Short English description` | `[COST-8136] Skip ROS pytest when ros.enabled is false` |
| **Commit** (recommended) | `[COST-####] Short English description` | `[COST-8136] Skip ROS pytest when ros.enabled is false` |

Rules:

- Put the Jira key at the **start** of the PR title (`[COST-####]`), not at the end.
- Branch slug: lowercase, hyphens, no spaces.
- Use `redhat.atlassian.net` links in PR bodies (`https://redhat.atlassian.net/browse/COST-####`).
- Chore/docs without a ticket may use `chore/` or `docs/` prefixes instead of `cost-####`.

Project rules for Cursor users live in [.cursor/rules/jira-cost-branches-prs.mdc](.cursor/rules/jira-cost-branches-prs.mdc).
Agent context also appears in [CLAUDE.md](CLAUDE.md).

## Development

See [CLAUDE.md](CLAUDE.md) for build, test, reconciler conventions, and the PR
review checklist.
