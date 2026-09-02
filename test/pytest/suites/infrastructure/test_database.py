"""
Database schema and migration tests.

Tests for database schema validation and migration status.
"""

import pytest

from utils import (
    assert_log_contains,
    exec_in_pod,
    execute_db_query,
    run_oc_command,
    get_job_logs,
    validate_logs,
)

# Koku migrate job wording varies by image; accept any known success line.
_MIGRATION_LOG_SUCCESS_PATTERN = (
    r"(Migrations completed successfully|=== Koku migrations completed ===)"
)


@pytest.mark.infrastructure
@pytest.mark.component
class TestDatabaseSchema:
    """Tests for database schema validation."""

    def test_api_provider_table_exists(self, cluster_config, database_config):
        """Verify api_provider table exists (core Koku table)."""
        result = execute_db_query(
            database_config.namespace,
            database_config.pod_name,
            database_config.database,
            database_config.user,
            "SELECT EXISTS (SELECT FROM information_schema.tables WHERE table_name = 'api_provider')",
            password=database_config.password,
        )
        
        assert result is not None, "Query failed"
        assert result[0][0] in ["t", "True", True, "1"], "api_provider table not found"

    def test_api_customer_table_exists(self, cluster_config, database_config):
        """Verify api_customer table exists."""
        result = execute_db_query(
            database_config.namespace,
            database_config.pod_name,
            database_config.database,
            database_config.user,
            "SELECT EXISTS (SELECT FROM information_schema.tables WHERE table_name = 'api_customer')",
            password=database_config.password,
        )
        
        assert result is not None, "Query failed"
        assert result[0][0] in ["t", "True", True, "1"], "api_customer table not found"

    def test_manifest_table_exists(self, cluster_config, database_config):
        """Verify cost usage report manifest table exists."""
        result = execute_db_query(
            database_config.namespace,
            database_config.pod_name,
            database_config.database,
            database_config.user,
            "SELECT EXISTS (SELECT FROM information_schema.tables WHERE table_name = 'reporting_common_costusagereportmanifest')",
            password=database_config.password,
        )
        
        assert result is not None, "Query failed"
        assert result[0][0] in ["t", "True", True, "1"], "Manifest table not found"


@pytest.mark.infrastructure
@pytest.mark.component
class TestDatabaseMigrations:
    """Tests for database migration status."""

    def test_django_migrations_table_exists(self, cluster_config, database_config):
        """Verify Django migrations table exists."""
        result = execute_db_query(
            database_config.namespace,
            database_config.pod_name,
            database_config.database,
            database_config.user,
            "SELECT EXISTS (SELECT FROM information_schema.tables WHERE table_name = 'django_migrations')",
            password=database_config.password,
        )
        
        assert result is not None, "Query failed"
        assert result[0][0] in ["t", "True", True, "1"], "django_migrations table not found"

    def test_migrations_applied(self, cluster_config, database_config):
        """Verify migrations have been applied."""
        result = execute_db_query(
            database_config.namespace,
            database_config.pod_name,
            database_config.database,
            database_config.user,
            "SELECT COUNT(*) FROM django_migrations",
            password=database_config.password,
        )
        
        assert result is not None, "Query failed"
        count = int(result[0][0])
        assert count > 0, "No migrations have been applied"

    def test_no_pending_migrations(self, cluster_config, database_config):
        """Verify no migrations are pending (informational)."""
        # This is informational - we just check that the app tables exist
        # which indicates migrations have run
        result = execute_db_query(
            database_config.namespace,
            database_config.pod_name,
            database_config.database,
            database_config.user,
            "SELECT app FROM django_migrations GROUP BY app ORDER BY app",
            password=database_config.password,
        )
        
        assert result is not None, "Query failed"
        apps = [row[0] for row in result]
        
        # Check for expected Django apps
        expected_apps = ["api", "reporting", "reporting_common"]
        for app in expected_apps:
            assert app in apps, f"Migrations for '{app}' not found"

    def test_migration_job_completed(self, cluster_config):
        """Verify database migration job completed successfully.
        
        FLPATH-3858: Verify Database Initialization Jobs
        
        The koku-migrate job is a Helm pre-install/pre-upgrade hook that runs
        Django migrations before the application pods start.
        """
        # Get the migration job status
        result = run_oc_command([
            "get", "job",
            "-n", cluster_config.namespace,
            "-l", "app.kubernetes.io/component=cost-management-migration",
            "-o", "jsonpath={.items[*].status.succeeded}"
        ], check=False)
        
        if result.returncode != 0:
            pytest.skip("Migration job not found (may be cleaned up by Helm hook policy)")
        
        # Check if job completed successfully
        succeeded = result.stdout.strip()
        
        if not succeeded:
            # Job exists but hasn't completed - check for failures
            failure_result = run_oc_command([
                "get", "job",
                "-n", cluster_config.namespace,
                "-l", "app.kubernetes.io/component=cost-management-migration",
                "-o", "jsonpath={.items[*].status.failed}"
            ], check=False)
            
            failed = failure_result.stdout.strip()
            if failed and int(failed) > 0:
                pytest.fail(
                    f"Migration job failed {failed} time(s). "
                    "Check logs: oc logs -l app.kubernetes.io/component=cost-management-migration"
                )
            else:
                pytest.skip(
                    "Migration job not completed yet (this is informational - "
                    "tables already validated in other tests)"
                )
        
        # Job completed - verify it succeeded
        succeeded_count = int(succeeded) if succeeded else 0
        assert succeeded_count >= 1, (
            f"Migration job did not succeed (succeeded={succeeded}). "
            "Check logs: oc logs -l app.kubernetes.io/component=cost-management-migration"
        )

    def test_migration_job_logs_success(self, cluster_config):
        """Verify migration job completed successfully via logs.
        
        This test validates that the Koku migration job ran without errors
        by checking its log output for success indicators.
        """
        job_name = f"{cluster_config.helm_release_name}-koku-migrate"
        logs = get_job_logs(
            cluster_config.namespace,
            job_name,
            container="migrate",
        )
        
        if logs is None:
            pytest.skip("Migration job logs not available (job may have been cleaned up)")
        
        assert_log_contains(
            logs,
            _MIGRATION_LOG_SUCCESS_PATTERN,
            regex=True,
            message="Migration job did not log a known success marker",
        )
        validate_logs(
            logs,
            must_not_contain=[
                "Migration failed",
                "Traceback (most recent call last)",
            ],
        )


@pytest.mark.infrastructure
@pytest.mark.component
class TestHiveWorkaround:
    """Tests for FLPATH-3265: Hive database workaround removal.
    
    FLPATH-3265 removed unnecessary hive role/database creation from on-prem
    deployments. The hive role and database are only needed for Trino integration
    in cloud deployments; on-prem uses PostgreSQL directly.
    
    These tests verify:
    1. The hive role does NOT exist (workaround removed from db-init)
    2. The hive database does NOT exist (workaround removed from db-init)
    3. Migration 0039 was skipped (ONPREM=True behavior in Koku)
    
    References:
    - https://issues.redhat.com/browse/FLPATH-3265
    - https://github.com/project-koku/koku/pull/5900
    - https://github.com/insights-onprem/cost-onprem-chart/pull/96
    """

    def test_hive_role_not_created(self, cluster_config, database_config):
        """Verify hive role is NOT created (FLPATH-3265 fix).
        
        The hive role was previously created as a workaround because Koku
        migration 0039 tried to create it but koku_user lacked CREATEROLE
        privilege. With the fix in Koku PR #5900, migration 0039 is skipped
        when ONPREM=True, so the workaround is no longer needed.
        """
        result = execute_db_query(
            database_config.namespace,
            database_config.pod_name,
            database_config.database,
            database_config.user,
            "SELECT EXISTS (SELECT FROM pg_catalog.pg_roles WHERE rolname = 'hive')",
            password=database_config.password,
        )
        
        assert result is not None, "Query failed"
        assert result[0][0] in ["f", "False", False, "0"], (
            "hive role should NOT exist in on-prem deployment (FLPATH-3265). "
            "This indicates the old workaround is still in place or migration 0039 "
            "ran unexpectedly. Check ONPREM environment variable is set to True."
        )

    def test_hive_database_not_created(self, cluster_config, database_config):
        """Verify hive database is NOT created (FLPATH-3265 fix).
        
        Similar to the hive role, the hive database was pre-created as a
        workaround. It should not exist in on-prem deployments.
        """
        result = execute_db_query(
            database_config.namespace,
            database_config.pod_name,
            "postgres",  # Query pg_database from postgres db
            database_config.user,
            "SELECT EXISTS (SELECT FROM pg_database WHERE datname = 'hive')",
            password=database_config.password,
        )
        
        assert result is not None, "Query failed"
        assert result[0][0] in ["f", "False", False, "0"], (
            "hive database should NOT exist in on-prem deployment (FLPATH-3265). "
            "This indicates the old workaround is still in place."
        )

    def test_koku_user_lacks_createrole_privilege(self, cluster_config, database_config):
        """Verify koku_user does NOT have CREATEROLE privilege (FLPATH-3265 fix).
        
        The old workaround granted CREATEROLE to koku_user so it could create
        the hive role. This is no longer needed and represents unnecessary
        privilege escalation.
        """
        result = execute_db_query(
            database_config.namespace,
            database_config.pod_name,
            database_config.database,
            database_config.user,
            f"SELECT rolcreaterole FROM pg_roles WHERE rolname = '{database_config.user}'",
            password=database_config.password,
        )
        
        assert result is not None, "Query failed"
        assert result[0][0] in ["f", "False", False, "0"], (
            f"{database_config.user} should NOT have CREATEROLE privilege (FLPATH-3265). "
            "This privilege was only needed for the old hive workaround."
        )

    def test_koku_user_lacks_createdb_privilege(self, cluster_config, database_config):
        """Verify koku_user does NOT have CREATEDB privilege (FLPATH-3265 fix).
        
        The old workaround granted CREATEDB to koku_user so it could create
        the hive database. This is no longer needed.
        """
        result = execute_db_query(
            database_config.namespace,
            database_config.pod_name,
            database_config.database,
            database_config.user,
            f"SELECT rolcreatedb FROM pg_roles WHERE rolname = '{database_config.user}'",
            password=database_config.password,
        )
        
        assert result is not None, "Query failed"
        assert result[0][0] in ["f", "False", False, "0"], (
            f"{database_config.user} should NOT have CREATEDB privilege (FLPATH-3265). "
            "This privilege was only needed for the old hive workaround."
        )


@pytest.mark.infrastructure
@pytest.mark.component
class TestKruizeDatabase:
    """Tests for Kruize database schema."""

    @pytest.fixture(autouse=True)
    def _skip_when_ros_disabled(self, require_ros_enabled) -> None:
        """Skip Kruize schema checks when spec.ros.enabled is false."""

    @pytest.fixture
    def kruize_credentials(self, cluster_config):
        """Get Kruize database credentials."""
        from utils import get_secret_value
        
        secret_name = f"{cluster_config.helm_release_name}-db-credentials"
        user = get_secret_value(cluster_config.namespace, secret_name, "kruize-user")
        password = get_secret_value(cluster_config.namespace, secret_name, "kruize-password")
        
        if not user or not password:
            pytest.skip("Kruize database credentials not found")
        
        return {"user": user, "password": password}

    def test_kruize_experiments_table_exists(
        self, cluster_config, kruize_database_config
    ):
        """Verify kruize_experiments table exists."""
        result = execute_db_query(
            kruize_database_config.namespace,
            kruize_database_config.pod_name,
            kruize_database_config.database,
            kruize_database_config.user,
            "SELECT EXISTS (SELECT FROM information_schema.tables WHERE table_name = 'kruize_experiments')",
            password=kruize_database_config.password,
        )
        
        assert result is not None, "Query failed"
        assert result[0][0] in ["t", "True", True, "1"], "kruize_experiments table not found"

    def test_kruize_recommendations_table_exists(
        self, cluster_config, kruize_database_config
    ):
        """Verify kruize_recommendations table exists."""
        result = execute_db_query(
            kruize_database_config.namespace,
            kruize_database_config.pod_name,
            kruize_database_config.database,
            kruize_database_config.user,
            "SELECT EXISTS (SELECT FROM information_schema.tables WHERE table_name = 'kruize_recommendations')",
            password=kruize_database_config.password,
        )
        
        assert result is not None, "Query failed"
        assert result[0][0] in ["t", "True", True, "1"], "kruize_recommendations table not found"
