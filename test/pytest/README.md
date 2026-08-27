# Cost On-Prem Test Suite

Pytest-based test suite for validating Cost On-Prem deployments on OpenShift.

## Requirements

- **Python 3.10+** (CI uses Python 3.11)
- OpenShift CLI (`oc`) with cluster access
- Helm 3.x

## Test Architecture

The test suite is organized into **focused suites** that avoid redundancy:

```
tests/
├── conftest.py              # Root fixtures (cluster config, JWT, DB, etc.)
├── utils.py                 # Shared utility functions
├── e2e_helpers.py           # Centralized E2E helpers (NISE, source registration, upload)
├── cleanup.py               # E2E cleanup utilities
├── pytest.ini               # Pytest configuration and markers
├── requirements.txt         # Python dependencies
├── reports/                 # JUnit XML reports (generated)
└── suites/                  # Test suites organized by subject
    ├── helm/                # Helm chart validation
    ├── auth/                # JWT authentication
    ├── infrastructure/      # Infrastructure health (DB, S3, Kafka) → [README](suites/infrastructure/README.md)
    ├── cost_management/     # Koku pipeline validation → [README](suites/cost_management/README.md)
    ├── ros/                 # ROS/Kruize component health
    └── e2e/                 # Complete end-to-end pipeline → [README](suites/e2e/README.md)
```

## Suite Documentation

Each suite has its own README with detailed test descriptions:

| Suite | README | Highlights |
|-------|--------|------------|
| **infrastructure** | [suites/infrastructure/README.md](suites/infrastructure/README.md) | Kafka validation, S3/Storage preflight |
| **cost_management** | [suites/cost_management/README.md](suites/cost_management/README.md) | Cost calculation validation, Processing state |
| **e2e** | [suites/e2e/README.md](suites/e2e/README.md) | YAML-driven scenario tests |

### Suite Responsibilities

| Suite | Purpose | What It Tests |
|-------|---------|---------------|
| **helm** | Chart validation | Lint, template rendering, deployment health |
| **auth** | Authentication | Keycloak, JWT ingress/backend auth |
| **infrastructure** | Infrastructure health | Database, S3, Kafka connectivity |
| **cost_management** | Koku health | Sources API, Listener, MASU health |
| **ros** | ROS health | Kruize, ROS Processor, API health |
| **e2e** | Complete pipeline | **Full flow: Data → Ingress → Koku → ROS** |

### Test Type Markers

Tests are categorized by scope:

| Marker | Description | Count |
|--------|-------------|-------|
| `component` | Tests validating a single component in isolation | ~50 |
| `integration` | Tests validating interactions between multiple components | ~25 |
| `extended` | Long-running tests requiring extended processing (skipped by default) | ~60 |
| `scenario` | YAML-driven scenario tests | 35 |

### Test Counts by File

| Suite | Test File | Tests | Description |
|-------|-----------|-------|-------------|
| infrastructure | `test_kafka.py` | 10 | Kafka cluster, topics, listener validation |
| infrastructure | `test_storage.py` | 6 | S3 endpoint, buckets, connectivity (uses boto3) |
| cost_management | `test_cost_validation.py` | 14 | Cost metrics vs NISE expected values |
| cost_management | `test_processing_state.py` | 12 | Manifest/file processing state validation |
| e2e | `test_scenarios.py` | 35 | YAML-driven scenario definitions and validation |

**Total Tests**: ~163

### E2E Test Coverage

The `e2e` suite validates the **complete production data flow**:

```
┌─────────────────────────────────────────────────────────────────────────┐
│                        E2E Test Pipeline                                │
├─────────────────────────────────────────────────────────────────────────┤
│                                                                         │
│  1. Source Registration                                                 │
│     └── Register OCP source via Sources API                            │
│     └── Verify provider created in Koku DB (via Kafka)                 │
│                                                                         │
│  2. Data Upload (via Ingress)                                          │
│     └── Generate test CSV with realistic metrics                       │
│     └── Package into tar.gz with manifest                              │
│     └── Upload via JWT-authenticated ingress                           │
│                                                                         │
│  3. Koku Processing                                                     │
│     └── Listener consumes from platform.upload.announce                │
│     └── MASU processes cost data from S3                               │
│     └── Manifest/file status tracked in DB                             │
│     └── Summary tables populated                                        │
│                                                                         │
│  4. ROS Pipeline                                                        │
│     └── Koku emits events to hccm.ros.events                           │
│     └── ROS Processor sends to Kruize                                  │
│                                                                         │
│  5. Recommendations                                                     │
│     └── Kruize generates recommendations                               │
│     └── Accessible via JWT-authenticated API                           │
│                                                                         │
└─────────────────────────────────────────────────────────────────────────┘
```

## Quick Start

```bash
# Run all tests (excludes extended by default)
./scripts/run-pytest.sh

# Run smoke tests only (quick validation)
./scripts/run-pytest.sh --smoke

# Run the complete E2E pipeline test
./scripts/run-pytest.sh --e2e

# Run E2E with extended tests (summary tables, Kruize)
./scripts/run-pytest.sh --extended

# Run ALL tests including extended
./scripts/run-pytest.sh --all

# Run specific suite
./scripts/run-pytest.sh --helm
./scripts/run-pytest.sh --auth
./scripts/run-pytest.sh --infrastructure
./scripts/run-pytest.sh --cost-management
./scripts/run-pytest.sh --ros

# Run specific test files directly
pytest tests/suites/infrastructure/test_kafka.py -v      # Kafka validation
pytest tests/suites/infrastructure/test_storage.py -v    # S3/Storage preflight
pytest tests/suites/cost_management/test_cost_validation.py -v -m extended  # Cost validation
pytest tests/suites/cost_management/test_processing_state.py -v  # Processing state
pytest tests/suites/e2e/test_scenarios.py -v -m scenario  # YAML-driven scenarios
```

## Cluster Bot (operator path)

To run this suite against an **operator-deployed** stack on a Cluster Bot / MCE lab
(not the Helm chart), follow the full runbook:

**[docs/development/clusterbot-operator-pytest.md](../../docs/development/clusterbot-operator-pytest.md)**

Summary:

```bash
# 1. Infra: RHBK + Kafka + S4 (skip Helm/operator in this script)
./scripts/deploy-test-cost-onprem.sh --namespace cost-onprem --deploy-s4 \
  --skip-helm --skip-chart-tests --skip-tls

# 2. Operator in-cluster (OwnNamespace: same NS as CR)
export IMG=quay.io/<you>/koku-service-operator:<tag>
./hack/deploy-incluster.sh cost-onprem

# 3. Apply + patch CMSC (S4 endpoint, Keycloak, clusterDomain) — see runbook

# 4. Pytest
export NAMESPACE=cost-onprem HELM_RELEASE_NAME=cost-onprem KEYCLOAK_NAMESPACE=keycloak
./scripts/run-pytest.sh --no-ui -v
```

**Do not** use `NAMESPACE=koku-service-operator-system` unless the CR also lives
there. The default sample uses `cost-onprem`.

### macOS notes

- Use Homebrew bash for `deploy-test-cost-onprem.sh` if macOS `/bin/bash` is 3.2.
- Homebrew Python may set `REQUESTS_CA_BUNDLE`, causing mass
  `SSLCertVerificationError` even when tests set `verify=False`. Until the
  `trust_env` fix lands, run: `unset REQUESTS_CA_BUNDLE SSL_CERT_FILE` before pytest.
- UI tests: install Playwright browsers manually (`playwright install chromium`);
  `--no-ui` skips `ui`, `performance`, and `helm` markers.

## Running Tests

### Using the Runner Script

```bash
# All tests
./scripts/run-pytest.sh

# Single suite
./scripts/run-pytest.sh --helm
./scripts/run-pytest.sh --auth
./scripts/run-pytest.sh --infrastructure
./scripts/run-pytest.sh --cost-management
./scripts/run-pytest.sh --ros
./scripts/run-pytest.sh --e2e

# Multiple suites
./scripts/run-pytest.sh --auth --ros

# Smoke tests across all suites
./scripts/run-pytest.sh --smoke

# E2E smoke tests only
./scripts/run-pytest.sh --e2e --smoke

# Setup environment only (no tests)
./scripts/run-pytest.sh --setup-only
```

### Using Pytest Directly

> **Note:** Running `pytest` directly requires manual dependency setup. The `run-pytest.sh` 
> script handles this automatically. If you prefer running pytest directly, follow the 
> setup steps below.

**Manual Setup (required before running pytest directly):**

```bash
cd tests

# Create and activate virtual environment
python3 -m venv .venv
source .venv/bin/activate

# Install dependencies
pip install -r requirements.txt

# Install koku-nise for E2E data generation (optional)
pip install koku-nise

# Install Playwright browsers (required for UI tests)
playwright install chromium --with-deps
```

**Running Tests:**

```bash
cd tests

# All tests (includes UI - requires Playwright)
pytest

# Exclude UI tests (no Playwright required)
pytest -m "not ui"

# By marker
pytest -m helm
pytest -m "auth and smoke"
pytest -m "not slow"
pytest -m e2e

# By directory
pytest suites/helm/
pytest suites/e2e/

# By file
pytest suites/e2e/test_complete_flow.py

# By test name pattern
pytest -k "test_upload"

# Verbose output
pytest -v

# Stop on first failure
pytest -x
```

> **UI Tests:** Running `pytest` without `-m "not ui"` will include UI tests, which require
> Playwright and browser binaries. Use `pytest -m "not ui"` to skip them, or install 
> Playwright first with `playwright install chromium --with-deps`.

## Test Markers

### Suite Markers
| Marker | Description |
|--------|-------------|
| `helm` | Helm chart validation tests |
| `auth` | JWT authentication tests |
| `infrastructure` | Infrastructure health tests |
| `cost_management` | Koku component tests |
| `ros` | ROS/Kruize tests |
| `e2e` | End-to-end pipeline tests |

### Type Markers
| Marker | Description |
|--------|-------------|
| `component` | Single-component tests (isolation) |
| `integration` | Multi-component tests (interactions) |

### Filter Markers
| Marker | Description |
|--------|-------------|
| `smoke` | Quick validation tests (~1 min) |
| `slow` | Long-running tests |
| `extended` | Tests requiring extended processing (skipped by default in CI) |

## Configuration

### Environment Variables

| Variable | Default | Description |
|----------|---------|-------------|
| `NAMESPACE` | `cost-onprem` | Target Kubernetes namespace |
| `HELM_RELEASE_NAME` | `cost-onprem` | Helm release name |
| `KEYCLOAK_NAMESPACE` | `keycloak` | Keycloak namespace |
| `KAFKA_NAMESPACE` | `kafka` | Kafka cluster namespace |
| `PLATFORM` | `openshift` | Platform type |
| `PYTHON` | `python3` | Python interpreter |
| `E2E_COST_TOLERANCE` | `0.05` | Tolerance for cost validation (5%) |

## Centralized E2E Helpers

The `e2e_helpers.py` module centralizes common E2E functionality to avoid duplication:

| Component | Description |
|-----------|-------------|
| `NISEConfig` | Dataclass with default NISE configuration and `get_expected_values()` method |
| `E2E_CLUSTER_PREFIX` | Standard prefix `"e2e-pytest-"` for all E2E cluster IDs |
| `is_nise_available()` / `install_nise()` | NISE availability checking and installation |
| `generate_nise_data()` | Generate NISE data with categorized file output |
| `generate_cluster_id()` | Generate unique cluster IDs |
| `get_koku_api_url()` | Get internal Koku API URL (unified deployment) |
| `register_source()` / `delete_source()` | Source registration with proper `source_ref` |
| `upload_with_retry()` | Upload with retry logic for transient errors |
| `wait_for_provider()` / `wait_for_summary_tables()` | Processing wait utilities |
| `cleanup_database_records()` / `cleanup_e2e_sources()` | Cleanup utilities |

Both `test_complete_flow.py` and `cost_management/conftest.py` import from `e2e_helpers` to ensure consistent behavior.

## Data Generation

The test suite supports flexible data generation for different testing scenarios.

### Quick Start: Scenario-Based Setup

```bash
# List available scenarios
./scripts/setup-test-data.sh --list

# Set up data for scenario
./scripts/setup-test-data.sh --scenario baseline

# Run tests against the data
./scripts/run-pytest.sh --e2e
```

See **[Test Data Setup Guide](../docs/development/test-data-setup.md)** for complete documentation on:
- Available scenarios (minimal, baseline, perf-small, perf-medium, ros)
- On-demand data loading for manual testing
- Pre-test environment preparation
- Troubleshooting data issues

### NISE Data Generation (Default)
Uses [koku-nise](https://github.com/project-koku/nise) to generate realistic OCP cost data:
- Generates pod-level metrics for Koku processing
- Generates container-level metrics for ROS processing (via `--ros-ocp-info`)
- Creates proper manifest with `files` and `resource_optimization_files` sections

```bash
# E2E tests use NISE by default
./scripts/run-pytest.sh --e2e
```

### Simple Data Generation (Fallback)
For quick testing without NISE dependencies:
```bash
# Use simple CSV generation
USE_SIMPLE_DATA=true ./scripts/run-pytest.sh --e2e
```

## Relationship to e2e_validator

The `scripts/e2e_validator/` module provides a **CLI-based** E2E validation tool that:
- Uses NISE to generate synthetic cost data
- Uploads directly to S3 (bypassing ingress)
- Validates Koku summary tables and cost calculations

The **pytest E2E suite** (`tests/suites/e2e/`) provides:
- The same validation as a **pytest test**
- Upload via **ingress** (production path)
- NISE integration for realistic data generation
- Integration with JUnit reporting
- Runnable alongside other test suites

Both tools validate the same pipeline, but:
- Use `e2e_validator` for standalone CLI validation with NISE data
- Use pytest E2E for CI/CD integration and combined test runs

## CI/CD Integration

### OpenShift CI Execution

In OpenShift CI, tests are executed via the `insights-onprem-cost-onprem-chart-e2e` step:

```
release/ci-operator/step-registry/insights-onprem/cost-onprem-chart/e2e/
├── insights-onprem-cost-onprem-chart-e2e-commands.sh  # Main CI script
├── insights-onprem-cost-onprem-chart-e2e-ref.yaml     # Step definition
```

**CI Execution Sequence:**
1. Dependencies installed (yq, kubectl, helm, oc)
2. S4 configured from `insights-onprem-s4-deploy` step
3. Cost Management Metrics Operator installed via OLM
4. Helm wrapper injects S4 storage config
5. `scripts/deploy-test-cost-onprem.sh` runs:
   - Deploys RHBK (Keycloak)
   - Deploys AMQ Streams/Kafka
   - Installs cost-onprem Helm chart
   - Configures TLS
   - **Runs `scripts/run-pytest.sh`** (CI mode)

**Default CI Test Run:**
```bash
# What CI executes (via deploy-test-cost-onprem.sh):
NAMESPACE=cost-onprem ./scripts/run-pytest.sh

# Equivalent to:
pytest -m "not extended" --junit-xml=reports/junit.xml
```

**CI runs ~88 tests in ~3 minutes** (excludes extended tests that require ODF/S3).

### JUnit Reports

Tests automatically generate JUnit XML reports at `tests/reports/junit.xml`.

### GitHub Actions Example

```yaml
- name: Run Tests
  run: |
    ./scripts/run-pytest.sh --smoke
  env:
    NAMESPACE: cost-onprem
    KEYCLOAK_NAMESPACE: keycloak

- name: Upload Test Results
  uses: actions/upload-artifact@v4
  if: always()
  with:
    name: test-results
    path: tests/reports/junit.xml
```

## Troubleshooting

### Common Issues

**Tests skip with "route not found":**
- Verify the Helm release is deployed
- Check route names match expected patterns

**Authentication failures:**
- Verify Keycloak is deployed and accessible
- Check client secret exists in expected location

**E2E tests timeout:**
- Koku processing can take several minutes
- Kruize recommendations require multiple data points

### Debug Mode

```bash
# Verbose output with local variables
pytest -v -l

# Stop on first failure and drop to debugger
pytest -x --pdb

# Show print statements
pytest -s
```

## Extended Tests

Extended tests (`test_06_summary_tables_populated`, `test_07_kruize_experiments_created`, `test_08_recommendations_generated`) are **skipped by default** in CI because they:
- Require longer processing times (5-15 minutes)
- Depend on asynchronous Koku/Kruize processing
- May require multiple data uploads for Kruize recommendations

### Running Extended Tests

```bash
# Run full E2E flow INCLUDING extended tests
./scripts/run-pytest.sh --extended

# Run ALL tests including extended
./scripts/run-pytest.sh --all

# Run only extended tests (not recommended - missing prerequisites)
pytest -m extended
```

**Note:** When using `--extended`, the entire `TestCompleteDataFlow` class runs to ensure prerequisites (source registration, data upload) complete before extended tests execute.

### Known Issues with Extended Tests

1. **Summary tables**: Require `start`/`end` dates in manifest (fixed in `tests/utils.py`)
2. **Kruize experiments**: Require `ros-ocp-backend` fixes for S3 URL encoding
3. **Recommendations**: May require multiple data uploads over time
