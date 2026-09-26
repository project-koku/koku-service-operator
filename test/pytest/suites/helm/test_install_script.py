"""
Unit tests for install-cmsc.sh bash functions via subprocess.

Tests S3 URL parsing, endpoint detection, bucket validation, region resolution,
and deploy_helm_chart --set injection logic without cluster access.
"""

import os
import shutil
import subprocess
from pathlib import Path

import pytest


def run_bash_function(script_path, bash_code, env=None):
    """Source install-cmsc.sh and execute bash code."""
    full_env = {
        **os.environ,
        "LOG_LEVEL": "ERROR",
        "NAMESPACE": "cost-onprem",
        "HELM_RELEASE_NAME": "cost-onprem",
    }
    if env:
        full_env.update(env)
    return subprocess.run(
        ["bash", "-c", f"source {script_path}\n{bash_code}"],
        capture_output=True,
        text=True,
        env=full_env,
        timeout=30,
    )


@pytest.fixture(scope="module")
def install_script(cluster_config):
    path = Path(cluster_config.project_root) / "scripts" / "install-cmsc.sh"
    assert path.exists(), "install-cmsc.sh not found"
    return str(path)


@pytest.fixture(scope="module")
def chart_dir(cluster_config):
    path = Path(cluster_config.project_root) / "cost-onprem"
    assert path.exists(), "cost-onprem chart directory not found"
    return str(path)


requires_yq = pytest.mark.skipif(
    not shutil.which("yq"), reason="yq not available"
)


# =============================================================================
# Group 1: parse_s3_host
# =============================================================================


@pytest.mark.helm
@pytest.mark.component
class TestParseS3Host:

    @pytest.mark.parametrize("url,expected", [
        ("https://s3.us-east-1.amazonaws.com", "s3.us-east-1.amazonaws.com"),
        ("http://s4.ns.svc.cluster.local:7480", "s4.ns.svc.cluster.local"),
        ("https://s3.example.com:443/", "s3.example.com"),
        ("s3.amazonaws.com", "s3.amazonaws.com"),
        ("s3.openshift-storage.svc/", "s3.openshift-storage.svc"),
        ("minio.local:9000", "minio.local"),
    ], ids=[
        "strip-https",
        "strip-http-port",
        "strip-all",
        "bare-passthrough",
        "strip-slash",
        "strip-port",
    ])
    def test_parse_s3_host(self, install_script, url, expected):
        result = run_bash_function(install_script, f'parse_s3_host "{url}"')
        assert result.returncode == 0
        assert result.stdout.strip() == expected


# =============================================================================
# Group 1b: parse_s3_namespace
# =============================================================================


@pytest.mark.helm
@pytest.mark.component
class TestParseS3Namespace:

    @pytest.mark.parametrize("url,expected", [
        ("http://s4.cost-onprem.svc.cluster.local:7480", "cost-onprem"),
        ("s4.my-ns.svc.cluster.local", "my-ns"),
        ("https://s3.openshift-storage.svc:443", "openshift-storage"),
    ], ids=["fqdn", "bare-fqdn", "odf-endpoint"])
    def test_parse_s3_namespace(self, install_script, url, expected):
        result = run_bash_function(install_script, f'parse_s3_namespace "{url}"')
        assert result.returncode == 0
        assert result.stdout.strip() == expected


# =============================================================================
# Group 2: is_aws_s3_endpoint_host
# =============================================================================


@pytest.mark.helm
@pytest.mark.component
class TestIsAwsS3EndpointHost:

    @pytest.mark.parametrize("hostname,is_aws", [
        ("s3.us-east-1.amazonaws.com", True),
        ("s3.amazonaws.com", True),
        ("s3.dualstack.us-west-2.amazonaws.com", True),
        ("s3-us-gov-west-1.amazonaws.com", True),
        ("s4.ns.svc.cluster.local", False),
        ("s4.ns.svc", False),
        ("s3.openshift-storage.svc.cluster.local", False),
        ("minio.example.com", False),
    ], ids=[
        "regional",
        "global",
        "dualstack",
        "govcloud",
        "s4-cluster-local",
        "s4-short",
        "odf-noobaa",
        "generic",
    ])
    def test_is_aws_s3_endpoint_host(self, install_script, hostname, is_aws):
        result = run_bash_function(
            install_script,
            f'is_aws_s3_endpoint_host "{hostname}"',
        )
        expected_rc = 0 if is_aws else 1
        assert result.returncode == expected_rc


# =============================================================================
# Group 3: validate_s3_bucket_name
# =============================================================================


@pytest.mark.helm
@pytest.mark.component
class TestValidateS3BucketName:

    @pytest.mark.parametrize("name,valid", [
        ("my-bucket", True),
        ("my.bucket.name", True),
        ("abc", True),
        ("a" * 63, True),
        ("ab", False),
        ("a" * 64, False),
        ("My-Bucket", False),
        ("my_bucket", False),
        ("-mybucket", False),
        ("mybucket-", False),
    ], ids=[
        "valid-simple",
        "valid-dots",
        "valid-min-3",
        "valid-max-63",
        "too-short-2",
        "too-long-64",
        "uppercase",
        "underscore",
        "leading-hyphen",
        "trailing-hyphen",
    ])
    def test_validate_s3_bucket_name(self, install_script, name, valid):
        result = run_bash_function(
            install_script,
            f'validate_s3_bucket_name "{name}" "test" 2>/dev/null',
        )
        expected_rc = 0 if valid else 1
        assert result.returncode == expected_rc


# =============================================================================
# Group 4: find_explicit_s3_region
# =============================================================================


@pytest.mark.helm
@pytest.mark.component
class TestFindExplicitS3Region:

    def test_env_var_found(self, install_script):
        result = run_bash_function(
            install_script,
            "S3_REGION=eu-west-1 find_explicit_s3_region",
        )
        assert result.returncode == 0
        assert result.stdout.strip() == "eu-west-1"

    @requires_yq
    def test_values_file_region(self, install_script, tmp_path):
        values = tmp_path / "values-region.yaml"
        values.write_text("objectStorage:\n  s3:\n    region: us-west-2\n")
        result = run_bash_function(
            install_script,
            "unset S3_REGION; find_explicit_s3_region",
            env={
                "VALUES_FILE": str(values),
                "CHART_DIR": str(tmp_path / "nonexistent"),
            },
        )
        assert result.returncode == 0
        assert result.stdout.strip() == "us-west-2"

    @requires_yq
    def test_onprem_filtered_out(self, install_script, tmp_path):
        values = tmp_path / "values-onprem.yaml"
        values.write_text("objectStorage:\n  s3:\n    region: onprem\n")
        result = run_bash_function(
            install_script,
            "unset S3_REGION; find_explicit_s3_region 2>/dev/null",
            env={
                "VALUES_FILE": str(values),
                "CHART_DIR": str(tmp_path / "nonexistent"),
            },
        )
        assert result.returncode == 1

    @requires_yq
    def test_env_var_takes_precedence(self, install_script, tmp_path):
        values = tmp_path / "values-region.yaml"
        values.write_text("objectStorage:\n  s3:\n    region: us-west-2\n")
        result = run_bash_function(
            install_script,
            "find_explicit_s3_region",
            env={
                "S3_REGION": "ap-southeast-1",
                "VALUES_FILE": str(values),
            },
        )
        assert result.stdout.strip() == "ap-southeast-1"

    def test_no_region_anywhere(self, install_script, tmp_path):
        result = run_bash_function(
            install_script,
            "unset S3_REGION; find_explicit_s3_region 2>/dev/null",
            env={
                "VALUES_FILE": "",
                "CHART_DIR": str(tmp_path / "nonexistent"),
            },
        )
        assert result.returncode == 1


# =============================================================================
# Group 5: resolve_s3_cli_region
# =============================================================================


@pytest.mark.helm
@pytest.mark.component
class TestResolveS3CliRegion:

    def test_region_found(self, install_script):
        result = run_bash_function(
            install_script,
            "S3_REGION=ap-southeast-1 resolve_s3_cli_region",
        )
        assert result.stdout.strip() == "ap-southeast-1"

    def test_no_region_fallback(self, install_script, tmp_path):
        result = run_bash_function(
            install_script,
            "unset S3_REGION; resolve_s3_cli_region",
            env={
                "VALUES_FILE": "",
                "CHART_DIR": str(tmp_path / "nonexistent"),
            },
        )
        assert result.stdout.strip() == "us-east-1"


# =============================================================================
# Group 5b: resolve_install_bucket_name
# =============================================================================


@pytest.mark.helm
@pytest.mark.component
class TestResolveInstallBucketName:

    @requires_yq
    def test_chart_default_ingress(self, install_script, chart_dir):
        result = run_bash_function(
            install_script,
            "resolve_install_bucket_name '.ingress.storage.bucket' 'fallback-default'",
            env={"CHART_DIR": chart_dir, "VALUES_FILE": ""},
        )
        assert result.stdout.strip() == "insights-upload-perma"

    @requires_yq
    def test_chart_default_koku(self, install_script, chart_dir):
        result = run_bash_function(
            install_script,
            "resolve_install_bucket_name '.costManagement.storage.bucketName' 'fallback-default'",
            env={"CHART_DIR": chart_dir, "VALUES_FILE": ""},
        )
        assert result.stdout.strip() == "koku-bucket"

    @requires_yq
    def test_user_values_override(self, install_script, chart_dir, tmp_path):
        values = tmp_path / "values-buckets.yaml"
        values.write_text("ingress:\n  storage:\n    bucket: my-custom-ingress\n")
        result = run_bash_function(
            install_script,
            "resolve_install_bucket_name '.ingress.storage.bucket' 'fallback-default'",
            env={"CHART_DIR": chart_dir, "VALUES_FILE": str(values)},
        )
        assert result.stdout.strip() == "my-custom-ingress"

    @requires_yq
    def test_missing_key_fallback(self, install_script, tmp_path):
        result = run_bash_function(
            install_script,
            "resolve_install_bucket_name '.nonexistent.key' 'fallback-default'",
            env={"CHART_DIR": str(tmp_path / "nonexistent"), "VALUES_FILE": ""},
        )
        assert result.stdout.strip() == "fallback-default"

    @requires_yq
    def test_yaml_null_fallback(self, install_script, tmp_path):
        values = tmp_path / "values-null.yaml"
        values.write_text("ingress:\n  storage:\n    bucket: null\n")
        result = run_bash_function(
            install_script,
            "resolve_install_bucket_name '.ingress.storage.bucket' 'fallback-default'",
            env={"CHART_DIR": str(tmp_path / "nonexistent"), "VALUES_FILE": str(values)},
        )
        assert result.stdout.strip() == "fallback-default"


# =============================================================================
# Group 6: compute_and_export_install_bucket_names
# =============================================================================


@pytest.mark.helm
@pytest.mark.component
class TestComputeAndExportInstallBucketNames:

    def _run(self, install_script, chart_dir, endpoint, env_extras=None):
        unsets = "unset S3_BUCKET_PREFIX S3_BUCKET_INGRESS S3_BUCKET_KOKU S3_BUCKET_ROS"
        exports = ""
        if env_extras:
            exports = " ".join(f'export {k}="{v}"' for k, v in env_extras.items())
            if any(k.startswith("S3_BUCKET_") for k in env_extras):
                unset_keys = {"S3_BUCKET_PREFIX", "S3_BUCKET_INGRESS", "S3_BUCKET_KOKU", "S3_BUCKET_ROS"} - set(env_extras.keys())
                unsets = "unset " + " ".join(unset_keys) if unset_keys else ""
        return run_bash_function(
            install_script,
            f'{unsets}\n{exports}\ncompute_and_export_install_bucket_names "{endpoint}" 2>&1',
            env={"CHART_DIR": chart_dir},
        )

    def test_aws_chart_defaults_rejected(self, install_script, chart_dir):
        result = self._run(install_script, chart_dir, "s3.us-east-1.amazonaws.com")
        assert result.returncode == 1
        assert "globally unique" in result.stdout

    def test_aws_bucket_prefix_accepted(self, install_script, chart_dir):
        result = self._run(
            install_script, chart_dir, "s3.us-east-1.amazonaws.com",
            {"S3_BUCKET_PREFIX": "myorg-costonprem-prod"},
        )
        assert result.returncode == 0

    def test_aws_individual_overrides_accepted(self, install_script, chart_dir):
        result = self._run(
            install_script, chart_dir, "s3.us-east-1.amazonaws.com",
            {"S3_BUCKET_INGRESS": "my-ingress", "S3_BUCKET_KOKU": "my-koku", "S3_BUCKET_ROS": "my-ros"},
        )
        assert result.returncode == 0

    def test_non_aws_chart_defaults_allowed(self, install_script, chart_dir):
        result = self._run(install_script, chart_dir, "s4.ns.svc.cluster.local")
        assert result.returncode == 0


# =============================================================================
# Group 7: create_s3_buckets decision logic
# =============================================================================


@pytest.mark.helm
@pytest.mark.component
class TestCreateS3BucketsDecisionLogic:

    def _run_decision_test(self, install_script, chart_dir, env_setup):
        bash_code = f'''
export LOG_LEVEL=INFO CHART_DIR="{chart_dir}" HELM_RELEASE_NAME=cost-onprem NAMESPACE=cost-onprem
kubectl() {{
    case "$*" in
        *"get secret"*"access-key"*) echo "ZmFrZWtleQ==" ;;
        *"get secret"*"secret-key"*) echo "ZmFrZXNlY3JldA==" ;;
        *"get crd"*) return 1 ;;
        *"apply"*|*"create"*|*"wait"*|*"delete"*) return 0 ;;
        *"logs"*) echo "bucket created" ;;
        *) return 0 ;;
    esac
}}
export -f kubectl
{env_setup}
create_s3_buckets 2>&1
'''
        return run_bash_function(install_script, bash_code)

    def _aws_base_env(self, **overrides):
        env = {
            "S3_ENDPOINT": "s3.us-east-1.amazonaws.com",
            "S3_REGION": "us-east-1",
            "S3_BUCKET_PREFIX": "test-unit",
            "S3_ACCESS_KEY": "fakekey",
            "S3_SECRET_KEY": "fakesecret",
        }
        env.update(overrides)
        lines = [f'export {k}="{v}"' for k, v in env.items()]
        return "\n".join(lines) + "\nunset S3_VERIFY_SSL"

    def test_aws_auto_enables_ssl(self, install_script, chart_dir):
        result = self._run_decision_test(
            install_script, chart_dir, self._aws_base_env(),
        )
        assert "Auto-enabled SSL verification" in result.stdout

    def test_aws_explicit_ssl_false_no_auto_enable(self, install_script, chart_dir):
        env = self._aws_base_env().replace("unset S3_VERIFY_SSL", 'export S3_VERIFY_SSL=false')
        result = self._run_decision_test(install_script, chart_dir, env)
        assert "Using S3_ENDPOINT" in result.stdout
        assert "Auto-enabled SSL verification" not in result.stdout

    def test_aws_without_region_fails(self, install_script, chart_dir):
        env = self._aws_base_env()
        env = env.replace('export S3_REGION="us-east-1"\n', '') + "\nunset S3_REGION"
        result = self._run_decision_test(install_script, chart_dir, env)
        assert result.returncode == 1
        assert "Region is required" in result.stdout

    def test_aws_addressing_style_auto(self, install_script, chart_dir):
        result = self._run_decision_test(
            install_script, chart_dir, self._aws_base_env(),
        )
        assert "addressing_style=auto" in result.stdout

    def test_region_mismatch_warning(self, install_script, chart_dir):
        result = self._run_decision_test(
            install_script, chart_dir,
            self._aws_base_env(S3_ENDPOINT="s3.us-west-2.amazonaws.com"),
        )
        assert "Region mismatch" in result.stdout

    def test_dualstack_aws_detected(self, install_script, chart_dir):
        result = self._run_decision_test(
            install_script, chart_dir,
            self._aws_base_env(
                S3_ENDPOINT="s3.dualstack.eu-west-1.amazonaws.com",
                S3_REGION="eu-west-1",
            ),
        )
        assert "addressing_style=auto" in result.stdout
        assert "Region mismatch" not in result.stdout

    def test_dualstack_region_mismatch(self, install_script, chart_dir):
        result = self._run_decision_test(
            install_script, chart_dir,
            self._aws_base_env(S3_ENDPOINT="s3.dualstack.eu-west-1.amazonaws.com"),
        )
        assert "Region mismatch" in result.stdout

    def test_aws_explicit_ssl_true_no_auto_enable(self, install_script, chart_dir):
        env = self._aws_base_env().replace("unset S3_VERIFY_SSL", 'export S3_VERIFY_SSL=true')
        result = self._run_decision_test(install_script, chart_dir, env)
        assert "Using S3_ENDPOINT" in result.stdout
        assert "Auto-enabled SSL verification" not in result.stdout

    def test_non_aws_no_auto_addressing(self, install_script, chart_dir):
        env = '''
export S3_ENDPOINT=s4.ns.svc.cluster.local S3_PORT=7480 S3_USE_SSL=false
export S3_ACCESS_KEY=fakekey S3_SECRET_KEY=fakesecret
unset S3_VERIFY_SSL S3_REGION
'''
        result = self._run_decision_test(install_script, chart_dir, env)
        assert "Using S3_ENDPOINT" in result.stdout
        assert "addressing_style=auto" not in result.stdout


# =============================================================================
# Group 8: deploy_helm_chart --set injection
# =============================================================================


@pytest.mark.helm
@pytest.mark.component
class TestDeployHelmChartSetInjection:

    def _run_deploy_test(self, install_script, chart_dir, env_setup):
        bash_code = f'''
export LOG_LEVEL=INFO
export CHART_DIR="{chart_dir}" LOCAL_CHART_PATH="{chart_dir}" USE_LOCAL_CHART=true
export HELM_RELEASE_NAME=cost-onprem NAMESPACE=cost-onprem
export PLATFORM=openshift KEYCLOAK_FOUND=false
export VALUES_FILE="" CHART_VERSION="" HELM_TIMEOUT=60s
export USER_S3_CONFIGURED=false USING_EXTERNAL_OBC=false
export STORAGE_CREDENTIALS_SECRET=""
export HELM_EXTRA_ARGS=()
unset RESOLVED_S3_BUCKET_INGRESS RESOLVED_S3_BUCKET_KOKU RESOLVED_S3_BUCKET_ROS

helm() {{ echo "HELM_CMD: $*"; return 0; }}
export -f helm
kubectl() {{
    case "$*" in
        *"get sc"*) echo "gp2" ;;
        *"get crd"*) return 1 ;;
        *) return 0 ;;
    esac
}}
export -f kubectl
oc() {{
    case "$*" in
        *"get ns"*"supplemental-groups"*) echo "1000740000/10000" ;;
        *) return 0 ;;
    esac
}}
export -f oc
get_helm_value() {{
    local key="$1" default="${{2:-}}"
    case "$key" in
        "valkey.deploy") echo "true" ;;
        *) echo "$default" ;;
    esac
}}
export -f get_helm_value

{env_setup}
deploy_helm_chart 2>&1
'''
        return run_bash_function(install_script, bash_code)

    def test_user_s3_configured_skips_overrides(self, install_script, chart_dir):
        result = self._run_deploy_test(install_script, chart_dir, '''
export USER_S3_CONFIGURED=true
export S3_ENDPOINT="s3.us-east-1.amazonaws.com" S3_REGION="us-east-1"
''')
        assert "objectStorage.endpoint" not in result.stdout
        assert "addressingStyle" not in result.stdout
        assert "values file" in result.stdout

    def test_no_resolved_buckets_no_overrides(self, install_script, chart_dir):
        result = self._run_deploy_test(install_script, chart_dir, '''
unset RESOLVED_S3_BUCKET_INGRESS RESOLVED_S3_BUCKET_KOKU RESOLVED_S3_BUCKET_ROS
''')
        assert "ingress.storage.bucket" not in result.stdout

    def test_obc_injects_all_flags(self, install_script, chart_dir):
        result = self._run_deploy_test(install_script, chart_dir, '''
export USING_EXTERNAL_OBC=true
export EXTERNAL_OBC_ENDPOINT="rgw.openshift-storage.svc"
export EXTERNAL_OBC_PORT=443
export EXTERNAL_OBC_BUCKET_NAME="obc-my-bucket"
''')
        assert "objectStorage.endpoint" in result.stdout
        assert "objectStorage.port" in result.stdout
        assert "objectStorage.useSSL=true" in result.stdout
        assert "ingress.storage.bucket" in result.stdout
        assert "costManagement.storage.bucketName" in result.stdout
        assert "costManagement.storage.rosBucketName" in result.stdout
        assert "ros.storage.bucketName" in result.stdout
        assert "obc-my-bucket" in result.stdout

    def test_obc_takes_precedence_over_s3_endpoint(self, install_script, chart_dir):
        result = self._run_deploy_test(install_script, chart_dir, '''
export USING_EXTERNAL_OBC=true
export EXTERNAL_OBC_ENDPOINT="rgw.openshift-storage.svc"
export EXTERNAL_OBC_PORT=443
export EXTERNAL_OBC_BUCKET_NAME="obc-my-bucket"
export S3_ENDPOINT="s3.us-east-1.amazonaws.com"
''')
        assert "rgw.openshift-storage.svc" in result.stdout
        assert "addressingStyle" not in result.stdout

    def test_noobaa_fallback(self, install_script, chart_dir):
        result = self._run_deploy_test(install_script, chart_dir, '''
unset S3_ENDPOINT S3_PORT S3_USE_SSL
kubectl() {
    case "$*" in
        *"get sc"*) echo "gp2" ;;
        *"get crd"*"noobaa"*) return 0 ;;
        *"get noobaa"*) return 0 ;;
        *) return 0 ;;
    esac
}
export -f kubectl
''')
        assert "s3.openshift-storage.svc" in result.stdout
        assert "objectStorage.port=443" in result.stdout
        assert "objectStorage.useSSL=true" in result.stdout

    def test_user_s3_configured_skips_buckets(self, install_script, chart_dir):
        result = self._run_deploy_test(install_script, chart_dir, '''
export USER_S3_CONFIGURED=true
export S3_ENDPOINT="s3.us-east-1.amazonaws.com"
export RESOLVED_S3_BUCKET_INGRESS="my-ingress"
export RESOLVED_S3_BUCKET_KOKU="my-koku"
export RESOLVED_S3_BUCKET_ROS="my-ros"
''')
        assert "objectStorage.endpoint" not in result.stdout
        assert "ingress.storage.bucket" not in result.stdout
