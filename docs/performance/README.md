# Performance Documentation

This directory contains all performance-related documentation for the
koku-service-operator (COST-8147, ported from FLPATH-4036 / COST-7567).

**Status**: Small through XLarge profiles validated with 0-failure runs on the
chart. Stress profiles (P99/Max) and soak tests pending on the operator path.

## Contents

| Document | Description |
|----------|-------------|
| [performance-testing-plan.md](performance-testing-plan.md) | Strategy, success criteria, and progress tracking |
| [TEST-MATRIX.md](TEST-MATRIX.md) | Complete test matrix with all permutations and parameters |
| [FINDINGS.md](FINDINGS.md) | Product issues discovered during testing (Jira-ready summaries) |
| [sizing-guide.md](sizing-guide.md) | Resource sizing recommendations validated through testing |
| [operator-profile-crd-mapping.md](operator-profile-crd-mapping.md) | Helm profile → `CostManagementServiceConfig` CR field mapping |
| [OBSERVABILITY.md](OBSERVABILITY.md) | Metrics collection, S3 archival, and report generation |
## Running Performance Tests

```bash
# All performance suites (excludes stress/soak)
./scripts/run-pytest.sh --performance

# Individual suites
./scripts/run-pytest.sh --perf-ingestion
./scripts/run-pytest.sh --perf-api
./scripts/run-pytest.sh --perf-scale
./scripts/run-pytest.sh --perf-ros
./scripts/run-pytest.sh --perf-valkey
./scripts/run-pytest.sh --perf-db
./scripts/run-pytest.sh --perf-kafka
./scripts/run-pytest.sh --perf-celery
./scripts/run-pytest.sh --perf-rbac

# Long-running
./scripts/run-pytest.sh --perf-soak
./scripts/run-pytest.sh --perf-stress

# 7-day soak loop with S3 checkpoints
SOAK_TESTS=true ./scripts/soak-loop.sh --days 7 \
  --s3-bucket "$S3_BUCKET" --s3-endpoint "$S3_ENDPOINT"
```

## Tracking Findings

When you discover a performance issue:

1. **Document immediately** in `FINDINGS.md` with evidence (logs, metrics),
   root cause analysis, and a proposed fix.
2. **Create a Jira ticket** for actionable items.
3. **Update status** as fixes are implemented.
