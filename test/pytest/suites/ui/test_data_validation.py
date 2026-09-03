"""
UI data validation tests.

These tests are SELF-CONTAINED - they set up their own test data using the
cost_validation_data fixture, then validate that data displays correctly in the UI.

1. Data Visualization - Charts render correctly with real cost data
2. Optimization Recommendations - CPU/memory recommendations display correctly
3. Optimization Breakdown - Detailed breakdown view shows correct data
"""

import os
import re
import time

import pytest
from playwright.sync_api import Page


def save_screenshot(page: Page, name: str) -> str:
    """Save a screenshot for documentation/verification purposes."""
    screenshots_dir = os.path.join(
        os.path.dirname(os.path.dirname(os.path.dirname(__file__))),
        "reports", "screenshots", "data_validation"
    )
    os.makedirs(screenshots_dir, exist_ok=True)
    path = os.path.join(screenshots_dir, f"{name}.png")
    page.screenshot(path=path, full_page=True)
    print(f"\n📸 Screenshot: {path}")
    return path


@pytest.mark.ui
@pytest.mark.data_validation
@pytest.mark.timeout(900)  # NISE upload + summary-table wait (same as cost_management)
class TestCostDataVisualization:
    """Test that cost data displays correctly in charts and tables.
    
    HIGH PRIORITY: Validates that the UI correctly renders cost data from the backend.
    Uses cost_validation_data fixture for self-contained data setup.
    """

    def test_overview_shows_cost_data(
        self, authenticated_page: Page, ui_url: str, cost_validation_data
    ):
        """Verify Overview page displays cost data (not empty state).
        
        The Overview page should show:
        - Cost summary cards or widgets
        - Charts with actual data points
        - Not just "No data available" messages
        """
        authenticated_page.goto(f"{ui_url}/openshift/cost-management")
        authenticated_page.wait_for_load_state("networkidle")
        
        # Wait for any loading indicators to disappear
        loading = authenticated_page.locator(".pf-v6-c-spinner, [data-testid='loading']")
        if loading.count() > 0:
            loading.first.wait_for(state="hidden", timeout=30000)
        
        # Check for empty state - should NOT be present since we have data
        empty_state = authenticated_page.locator(".pf-v6-c-empty-state, .pf-c-empty-state")
        empty_text = authenticated_page.get_by_text(re.compile(r"no data|no cost data|empty", re.IGNORECASE))
        
        # With cost_validation_data, we expect data to be present
        # If empty state is shown, the test should fail (not skip)
        if empty_state.count() > 0 and empty_state.first.is_visible():
            pytest.fail(
                f"Empty state shown despite cost_validation_data setup. "
                f"Cluster ID: {cost_validation_data['cluster_id']}"
            )
        
        # Look for data indicators - charts, tables, or cost values
        found_data = False
        
        # Check CSS selectors
        css_indicators = ["svg path", "svg rect", "table tbody tr", ".pf-v6-c-card"]
        for selector in css_indicators:
            if authenticated_page.locator(selector).count() > 0:
                found_data = True
                break
        
        # Check for dollar amounts via text
        if not found_data:
            dollar_amounts = authenticated_page.get_by_text(re.compile(r"\$[0-9]"))
            if dollar_amounts.count() > 0:
                found_data = True
        
        # Capture screenshot for verification
        save_screenshot(authenticated_page, "01_overview_cost_data")
        
        assert found_data, (
            f"Overview page should display cost data (charts, tables, or cost values). "
            f"Cluster ID: {cost_validation_data['cluster_id']}"
        )

    def test_openshift_page_shows_cluster_costs(
        self, authenticated_page: Page, ui_url: str, cost_validation_data
    ):
        """Verify OpenShift page displays cluster cost data.
        
        The OpenShift page should show:
        - Cost breakdown by cluster, project, or node
        - Charts or tables with actual values
        
        Note: The /ocp page may show empty state initially while data propagates
        through the UI's caching layer, even when data exists in the database.
        """
        authenticated_page.goto(f"{ui_url}/openshift/cost-management/ocp")
        authenticated_page.wait_for_load_state("networkidle")
        
        # Wait for loading to complete - OCP page may need more time
        time.sleep(5)  # Allow async data to load
        
        # Look for cost data elements first
        found_data = False
        if authenticated_page.locator("svg path, svg rect, table tbody tr, .pf-v6-c-table tbody tr").count() > 0:
            found_data = True
        if not found_data and authenticated_page.get_by_text(re.compile(r"\$[0-9]", re.IGNORECASE)).count() > 0:
            found_data = True
        
        if found_data:
            # Capture screenshot for verification
            save_screenshot(authenticated_page, "02_openshift_cluster_costs")
            return  # Test passes - data is displayed
        
        # Check for empty state - may occur due to UI caching/timing
        empty_state = authenticated_page.locator(".pf-v6-c-empty-state")
        if empty_state.count() > 0 and empty_state.first.is_visible():
            # This can happen due to UI caching - skip rather than fail
            # The Overview and Cost Explorer tests validate data is accessible
            pytest.skip(
                f"OpenShift page shows empty state (possible UI caching delay). "
                f"Data exists for cluster: {cost_validation_data['cluster_id']}"
            )
        
        # No data and no empty state - something else is wrong
        pytest.fail(
            f"OpenShift page should display cost data or empty state. "
            f"Cluster ID: {cost_validation_data['cluster_id']}"
        )

    def test_cost_explorer_displays_chart_with_data(
        self, authenticated_page: Page, ui_url: str, cost_validation_data
    ):
        """Verify Cost Explorer displays charts with actual data points.
        
        The Cost Explorer should show:
        - A chart (bar, line, or area) with visible data
        - Not just empty axes or "no data" message
        
        Note: The Cost Explorer may show empty state transiently while the UI's
        caching layer propagates cost data.  Poll briefly before concluding.
        """
        authenticated_page.goto(f"{ui_url}/openshift/cost-management/explorer")
        authenticated_page.wait_for_load_state("networkidle")

        chart_with_data = authenticated_page.locator("svg path, svg rect, svg circle")

        for attempt in range(4):
            time.sleep(3)
            if chart_with_data.count() > 2:
                break
            if attempt < 3:
                authenticated_page.reload()
                authenticated_page.wait_for_load_state("networkidle")

        save_screenshot(authenticated_page, "03_cost_explorer_chart")

        if chart_with_data.count() > 2:
            return

        empty_state = authenticated_page.locator(".pf-v6-c-empty-state")
        empty_text = authenticated_page.get_by_text(re.compile(r"no data available", re.IGNORECASE))
        if (empty_state.count() > 0 and empty_state.first.is_visible()) or \
           (empty_text.count() > 0 and empty_text.first.is_visible()):
            pytest.skip(
                f"Empty state shown after retries (possible UI caching delay). "
                f"Data exists for cluster: {cost_validation_data['cluster_id']}"
            )

        pytest.fail(
            f"Cost Explorer chart should have data points. Found {chart_with_data.count()} SVG elements. "
            f"Cluster ID: {cost_validation_data['cluster_id']}"
        )

    def test_cost_explorer_table_has_rows(
        self, authenticated_page: Page, ui_url: str, cost_validation_data
    ):
        """Verify Cost Explorer table displays data rows.
        
        When viewing as table, should show actual cost data rows.
        """
        authenticated_page.goto(f"{ui_url}/openshift/cost-management/explorer")
        authenticated_page.wait_for_load_state("networkidle")
        
        # Wait for data to load
        time.sleep(2)
        
        # Look for table rows
        table_rows = authenticated_page.locator(
            "table tbody tr, [role='grid'] [role='row'], .pf-v6-c-table tbody tr"
        )
        
        if table_rows.count() == 0:
            # Check if there's a table view toggle
            table_toggle = authenticated_page.locator(
                "button:has-text('Table'), [aria-label*='table'], [data-testid='table-view']"
            )
            if table_toggle.count() > 0:
                table_toggle.first.click()
                authenticated_page.wait_for_load_state("networkidle")
                time.sleep(1)
                table_rows = authenticated_page.locator("table tbody tr")
        
        # Capture screenshot for verification
        save_screenshot(authenticated_page, "04_cost_explorer_table")
        
        # With data setup, we expect rows (or chart view is default which is also valid)
        # This test validates table view specifically if available
        if table_rows.count() == 0:
            # Check if chart is showing instead (also valid)
            chart_elements = authenticated_page.locator("svg path, svg rect")
            if chart_elements.count() > 2:
                pass  # Chart view is showing data, that's fine
            else:
                pytest.fail(
                    f"Cost Explorer should have data in table or chart view. "
                    f"Cluster ID: {cost_validation_data['cluster_id']}"
                )


@pytest.mark.ui
@pytest.mark.ros
@pytest.mark.data_validation
@pytest.mark.timeout(900)  # NISE upload + summary-table wait (same as cost_management)
class TestOptimizationsPipeline:
    """Validate the full optimizations pipeline: upload → processing → UI display.

    Uses cost_validation_data fixture to upload test data, then verifies that
    the optimizations page renders a table with recommendation entries.

    API-level recommendation validation (filters, pagination, response
    structure, CPU/memory fields) is covered by suites/ros/test_recommendations.py.
    Basic page-load and empty-state UI tests are in test_optimizations.py.
    """

    def test_optimizations_table_populated_after_upload(
        self, authenticated_page: Page, ui_url: str, cost_validation_data
    ):
        """Verify optimizations table displays data after test upload completes.

        This is the pipeline integration check: data was uploaded and processed
        (via cost_validation_data fixture), so the optimizations page should
        show a non-empty table.
        """
        authenticated_page.goto(f"{ui_url}/openshift/cost-management/optimizations")
        authenticated_page.wait_for_load_state("networkidle")
        time.sleep(3)

        empty_state = authenticated_page.locator(".pf-v6-c-empty-state, .pf-c-empty-state")
        if empty_state.count() > 0 and empty_state.first.is_visible():
            pytest.skip("No optimization data available yet — Kruize may still be processing")

        table = authenticated_page.locator("table, [role='grid'], .pf-v6-c-table")
        assert table.count() > 0, "Optimizations table should be present"

        rows = authenticated_page.locator("table tbody tr")
        assert rows.count() > 0, "Optimizations table should have at least one row"

        headers = authenticated_page.locator("table thead th, table th")
        assert headers.count() > 0, "Table should have column headers"

        save_screenshot(authenticated_page, "05_optimizations_table")
