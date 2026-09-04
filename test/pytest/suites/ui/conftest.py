"""
Playwright fixtures for UI tests.

This module provides Playwright-specific fixtures that integrate with
the existing pytest infrastructure (cluster_config, keycloak_config, etc.).

Rich Reporting Configuration:
    Environment variables control what artifacts are captured:
    
    PLAYWRIGHT_VIDEO: "off", "on", "retain-on-failure" (default)
        Video recording - only failures are kept by default (videos are large)
    
    PLAYWRIGHT_TRACE: "off", "on", "retain-on-failure" (default)
        Rich trace with DOM snapshots, network requests, action log
        (produces .zip files viewable at trace.playwright.dev)
        Only failures are kept by default to reduce storage costs
    
    PLAYWRIGHT_SCREENSHOT: "off", "on" (default), "only-on-failure"
        Screenshot capture - always captured by default (~50-100KB)
    
    Artifacts are saved to:
    - tests/reports/videos/      - Video recordings (.webm)
    - tests/reports/screenshots/ - Screenshots (.png)
    - tests/reports/traces/      - Trace files (.zip)
    
    For CI, set ARTIFACT_DIR to copy reports to the artifact collection location.
    Orphaned video files (from passing tests) are cleaned up before copying.

Artifact Storage Considerations:
    Expected artifact sizes per test:
    - Screenshots: ~50-100KB each
    - Traces: ~3-5MB each  
    - Videos: ~5-15MB each (30 seconds at 720p)
    
    With default settings (retain-on-failure for video/trace, on for screenshots):
    - Passing tests: ~100KB each (screenshot only)
    - Failing tests: ~10-20MB each (screenshot + trace + video)
    
    For CI environments, consider setting artifact retention policies (e.g., 30-day TTL).

Parallel Execution Limitations:
    These fixtures are designed for single-threaded execution. If using pytest-xdist
    for parallel test execution, be aware:
    
    - The session-scoped `browser` fixture is NOT thread-safe. Each worker would
      need its own browser instance. Consider using worker_id fixture from xdist.
    
    - The module-scoped `sources_api_session` in test_sources.py uses requests.Session
      which is NOT thread-safe. For parallel execution, change to function scope
      or implement thread-local storage.
    
    - Artifact filenames use test names which may collide for parametrized tests.
      Add UUID suffix for parallel safety if needed.
    
    - The `ensure_no_sources` fixture deletes sources by prefix to avoid conflicts
      with other tests, but parallel test runs may still interfere with each other.

CMMO Source Creation:
    By default, CMMO creates a source when CostManagementMetricsConfig has 
    create_source=true. This interferes with empty-state UI tests.
    
    When running deploy-test-cost-onprem.sh with --include-ui, CMMO_CREATE_SOURCE
    is automatically set to false. For manual deployments, set:
        export CMMO_CREATE_SOURCE=false
    before running setup-cost-mgmt-tls.sh.
"""

import os
import re
import shutil
from typing import Generator
from urllib.parse import urlparse

import pytest
from playwright.sync_api import Browser, BrowserContext, Page, Playwright, sync_playwright

from conftest import ClusterConfig, KeycloakConfig
from utils import get_route_url

# Make cost_validation_data visible to UI tests. That fixture lives in
# suites/cost_management/conftest.py, which pytest does not load for -m ui.
# Rebind (don't `from … import` the fixture) so linters see a used name and
# pytest still discovers it here without pytest_plugins (pytest 9 double-register).
from suites.cost_management import conftest as _cost_management_conftest

cost_validation_data = _cost_management_conftest.cost_validation_data


# =============================================================================
# Reporting Configuration
# =============================================================================

# Video recording mode: "off", "on", "retain-on-failure"
# Default to "retain-on-failure" - videos are large, only keep for failures
VIDEO_MODE = os.environ.get("PLAYWRIGHT_VIDEO", "retain-on-failure")

# Trace recording mode: "off", "on", "retain-on-failure"
# Traces provide the richest debugging: DOM snapshots, network, action log
# Default to "retain-on-failure" to reduce CI artifact storage
# (traces are ~3-5MB each; with many tests this adds up)
TRACE_MODE = os.environ.get("PLAYWRIGHT_TRACE", "retain-on-failure")

# Screenshot mode: "off", "on", "only-on-failure"
# Default to "on" - screenshots are small (~50-100KB) and useful for all tests
SCREENSHOT_MODE = os.environ.get("PLAYWRIGHT_SCREENSHOT", "on")


# =============================================================================
# Playwright Browser Fixtures
# =============================================================================


@pytest.fixture(scope="session")
def playwright_instance() -> Generator[Playwright, None, None]:
    """Create a Playwright instance for the test session."""
    with sync_playwright() as p:
        yield p


@pytest.fixture(scope="session")
def browser(playwright_instance: Playwright) -> Generator[Browser, None, None]:
    """Launch a browser for the test session.
    
    Set PLAYWRIGHT_BROWSER env var to change:
    - chromium (default)
    - firefox
    - webkit
    """
    browser_type = os.environ.get("PLAYWRIGHT_BROWSER", "chromium")
    headless = os.environ.get("PLAYWRIGHT_HEADLESS", "true").lower() == "true"
    
    browser_launcher = getattr(playwright_instance, browser_type)
    browser = browser_launcher.launch(
        headless=headless,
        slow_mo=int(os.environ.get("PLAYWRIGHT_SLOW_MO", "0")),
    )
    yield browser
    browser.close()


@pytest.fixture(scope="function")
def browser_context(browser: Browser) -> Generator[BrowserContext, None, None]:
    """Create a fresh browser context for each test.
    
    Each test gets an isolated context with:
    - Fresh cookies/storage
    - Configured viewport
    - SSL certificate handling for self-signed certs
    """
    context = browser.new_context(
        viewport={"width": 1920, "height": 1080},
        ignore_https_errors=True,  # Handle self-signed certs in test environments
    )
    yield context
    context.close()


@pytest.fixture(scope="function")
def page(browser_context: BrowserContext) -> Generator[Page, None, None]:
    """Create a new page for each test."""
    page = browser_context.new_page()
    yield page
    page.close()


# =============================================================================
# Application URL Fixtures
# =============================================================================


@pytest.fixture(scope="session")
def ui_url(cluster_config: ClusterConfig) -> str:
    """Get the Cost Management UI URL."""
    route_name = f"{cluster_config.helm_release_name}-ui"
    url = get_route_url(cluster_config.namespace, route_name)
    if not url:
        pytest.skip(f"UI route '{route_name}' not found in namespace {cluster_config.namespace}")
    return url


@pytest.fixture(scope="session")
def keycloak_login_url(keycloak_config: KeycloakConfig) -> str:
    """Get the Keycloak login URL for the kubernetes realm."""
    return f"{keycloak_config.url}/realms/{keycloak_config.realm}/protocol/openid-connect/auth"


# =============================================================================
# Authentication Fixtures
# =============================================================================


def _assert_callback_completed(
    page,
    context: BrowserContext,
    ui_url: str,
    timeout: int = 10000,
) -> None:
    """Verify the OAuth2 proxy callback completed and session cookies are set.

    After Keycloak redirects to /oauth2/callback, oauth2-proxy must exchange
    the code for tokens and redirect to the app.  If the page is still at
    /oauth2/callback, the token exchange failed (e.g. bad TLS, malformed
    claim names, missing audience).
    """
    if "/oauth2/callback" in page.url:
        try:
            page.wait_for_url(
                re.compile(rf".*{re.escape(urlparse(ui_url).netloc)}(?!/oauth2/callback).*"),
                timeout=timeout,
            )
        except Exception:
            raise RuntimeError(
                f"OAuth2 callback did not complete. Page stuck at: {page.url}\n"
                "oauth2-proxy failed to exchange the authorization code for tokens. "
                "Check oauth-proxy container logs for errors (TLS, redirect_uri mismatch, "
                "malformed roles scope, etc.)."
            )

    session_cookies = [
        c for c in context.cookies()
        if c["name"].startswith("_oauth2_proxy")
        and c["name"] != "_oauth2_proxy_csrf"
    ]
    if not session_cookies:
        raise RuntimeError(
            f"Login completed (URL: {page.url}) but no _oauth2_proxy session "
            "cookies were set. The OAuth2 callback may have returned an error page."
        )


def _login_context(
    browser: Browser,
    ui_url: str,
    keycloak_config: KeycloakConfig,
    username: str,
    password: str,
) -> BrowserContext:
    """Create a browser context authenticated via Keycloak login form.

    Verifies the OAuth2 callback completes and session cookies are set.
    """
    context = browser.new_context(
        viewport={"width": 1920, "height": 1080},
        ignore_https_errors=True,
    )

    page = context.new_page()
    page.goto(ui_url)
    page.wait_for_url(f"**/{keycloak_config.realm}/**", timeout=10000)

    page.fill('input[name="username"]', username)
    page.fill('input[name="password"]', password)
    page.click('input[type="submit"], button[type="submit"]')

    page.wait_for_url(f"{ui_url}**", timeout=30000)
    _assert_callback_completed(page, context, ui_url)
    page.close()
    return context


def _get_reports_base_dir() -> str:
    """Get the base reports directory."""
    return os.path.join(
        os.path.dirname(os.path.dirname(os.path.dirname(__file__))),
        "reports"
    )


def _get_videos_dir() -> str:
    """Get the directory for storing video recordings."""
    return os.path.join(_get_reports_base_dir(), "videos")


def _get_traces_dir() -> str:
    """Get the directory for storing trace files."""
    return os.path.join(_get_reports_base_dir(), "traces")


def _get_screenshots_dir() -> str:
    """Get the directory for storing screenshots."""
    return os.path.join(_get_reports_base_dir(), "screenshots")


@pytest.fixture(scope="function")
def authenticated_context(
    browser: Browser,
    ui_url: str,
    keycloak_config: KeycloakConfig,
    request,
) -> Generator[BrowserContext, None, None]:
    """Create a browser context with authenticated session.
    
    Performs Keycloak login and stores the session for the test.
    
    Credentials:
        Default credentials are admin/admin, configurable via environment variables:
        - TEST_UI_USERNAME: Keycloak username (default: "admin")
        - TEST_UI_PASSWORD: Keycloak password (default: "admin")
        
        SECURITY NOTE: These credentials are ONLY valid in ephemeral CI test
        environments. The test Keycloak user is provisioned by the test harness
        bootstrap (see scripts/deploy-rhbk.sh). These credentials must never
        match any staging or production credentials.
    
    Artifact recording is controlled by environment variables:
    - PLAYWRIGHT_VIDEO: "off", "on", "retain-on-failure" (default)
    - PLAYWRIGHT_TRACE: "off", "on", "retain-on-failure" (default)
    - PLAYWRIGHT_SCREENSHOT: "off", "on" (default), "only-on-failure"
    
    Lifecycle:
        This fixture owns the browser context lifecycle. It creates, yields, and
        closes the context. The authenticated_page fixture creates pages within
        this context but delegates context cleanup back here.
    """
    # Ensure output directories exist
    videos_dir = _get_videos_dir()
    traces_dir = _get_traces_dir()
    screenshots_dir = _get_screenshots_dir()
    for d in [videos_dir, traces_dir, screenshots_dir]:
        os.makedirs(d, exist_ok=True)
    
    # Configure video recording
    context_options = {
        "viewport": {"width": 1920, "height": 1080},
        "ignore_https_errors": True,
    }
    
    if VIDEO_MODE != "off":
        context_options["record_video_dir"] = videos_dir
        context_options["record_video_size"] = {"width": 1280, "height": 720}
    
    context = browser.new_context(**context_options)
    
    # Start tracing if enabled
    if TRACE_MODE != "off":
        context.tracing.start(screenshots=True, snapshots=True, sources=True)
    
    page = context.new_page()
    
    # Navigate to UI (will redirect to Keycloak)
    page.goto(ui_url)
    
    # Wait for Keycloak login page
    page.wait_for_url(f"**/{keycloak_config.realm}/**", timeout=10000)
    
    # Fill login form (see docstring for security notes on these defaults)
    username = os.environ.get("TEST_UI_USERNAME", "admin")
    password = os.environ.get("TEST_UI_PASSWORD", "admin")
    
    page.fill('input[name="username"]', username)
    page.fill('input[name="password"]', password)
    page.click('input[type="submit"], button[type="submit"]')
    
    # Wait for redirect back to UI host (any path)
    parsed = urlparse(ui_url)
    ui_host_pattern = f"**{parsed.netloc}**"
    page.wait_for_url(ui_host_pattern, timeout=30000)
    
    # Verify the OAuth2 callback completed and cookies are set
    _assert_callback_completed(page, context, ui_url)
    
    # Wait for page to fully load
    page.wait_for_load_state("networkidle")
    
    page.close()
    
    yield context
    
    # Context cleanup is handled by authenticated_page fixture's finally block.
    # This ensures cleanup happens even if artifact capture fails.
    # If authenticated_context is used directly (without authenticated_page),
    # the context must be closed explicitly by the test or another fixture.


@pytest.fixture(scope="function")
def non_admin_authenticated_context(
    browser: Browser,
    ui_url: str,
    keycloak_config: KeycloakConfig,
) -> Generator[BrowserContext, None, None]:
    """Browser context authenticated as viewer (no org-admin role)."""
    context = _login_context(browser, ui_url, keycloak_config, "viewer", "viewer")
    yield context
    context.close()


@pytest.fixture(scope="function")
def non_admin_authenticated_page(
    non_admin_authenticated_context: BrowserContext,
) -> Generator[Page, None, None]:
    """Create a page with non-admin (viewer) session."""
    page = non_admin_authenticated_context.new_page()
    yield page
    page.close()


# =============================================================================
# Screenshot/Trace Fixtures
# =============================================================================


@pytest.fixture(scope="function")
def authenticated_page(authenticated_context: BrowserContext, request) -> Generator[Page, None, None]:
    """Create a page with authenticated session.
    
    Automatically captures artifacts based on configuration:
    - Screenshots: PLAYWRIGHT_SCREENSHOT (default: on)
    - Videos: PLAYWRIGHT_VIDEO (default: retain-on-failure)
    - Traces: PLAYWRIGHT_TRACE (default: retain-on-failure)
    
    Traces provide the richest debugging experience with:
    - Step-by-step action log
    - DOM snapshots at each step
    - Network requests
    - Console logs
    
    View traces at: https://trace.playwright.dev
    
    Lifecycle:
        This fixture creates pages within the authenticated_context and handles
        artifact capture during teardown. Context cleanup is guaranteed via
        try/finally to prevent browser context leaks even if artifact capture fails.
    """
    page = authenticated_context.new_page()
    yield page
    
    # Teardown wrapped in try/finally to guarantee context cleanup
    video_path = None
    try:
        test_name = request.node.name.replace("/", "_").replace("::", "_").replace("[", "_").replace("]", "_")
        test_failed = hasattr(request.node, "rep_call") and request.node.rep_call.failed
        
        # Determine if we should save artifacts
        should_save_screenshot = (
            SCREENSHOT_MODE == "on" or 
            (SCREENSHOT_MODE == "only-on-failure" and test_failed)
        )
        should_save_video = (
            VIDEO_MODE == "on" or 
            (VIDEO_MODE == "retain-on-failure" and test_failed)
        )
        should_save_trace = (
            TRACE_MODE == "on" or 
            (TRACE_MODE == "retain-on-failure" and test_failed)
        )
        
        # Capture screenshot
        if should_save_screenshot:
            screenshots_dir = _get_screenshots_dir()
            os.makedirs(screenshots_dir, exist_ok=True)
            screenshot_path = os.path.join(screenshots_dir, f"{test_name}.png")
            try:
                page.screenshot(path=screenshot_path, full_page=True)
                status = "FAILED" if test_failed else "passed"
                print(f"\n📸 Screenshot saved ({status}): {screenshot_path}")
            except Exception as e:
                print(f"\n⚠️ Failed to capture screenshot: {e}")
        
        # Use Playwright's recommended save_as() API for video (avoids race with async finalization)
        if VIDEO_MODE != "off" and page.video:
            try:
                if should_save_video:
                    videos_dir = _get_videos_dir()
                    video_path = os.path.join(videos_dir, f"{test_name}.webm")
                    page.video.save_as(video_path)
                    status = "FAILED" if test_failed else "passed"
                    print(f"\n🎬 Video saved ({status}): {video_path}")
                else:
                    # Mark for deletion after page close
                    video_path = page.video.path()
            except Exception as e:
                print(f"\n⚠️ Failed to save video: {e}")
        
        page.close()
        
        # Delete video for passing tests in retain-on-failure mode (after page close)
        if video_path and not should_save_video and os.path.exists(video_path):
            try:
                os.remove(video_path)
            except Exception:
                pass
        
        # Save trace if enabled (stop tracing before context teardown)
        if TRACE_MODE != "off":
            traces_dir = _get_traces_dir()
            trace_path = os.path.join(traces_dir, f"{test_name}.zip")
            try:
                if should_save_trace:
                    authenticated_context.tracing.stop(path=trace_path)
                    status = "FAILED" if test_failed else "passed"
                    print(f"\n🔍 Trace saved ({status}): {trace_path}")
                    print(f"   View at: https://trace.playwright.dev (upload {test_name}.zip)")
                else:
                    # Stop tracing without saving
                    authenticated_context.tracing.stop()
            except Exception as e:
                print(f"\n⚠️ Failed to save trace: {e}")
    
    finally:
        # Guaranteed context cleanup - prevents browser context leaks
        try:
            if not page.is_closed():
                page.close()
        except Exception:
            pass
        
        try:
            authenticated_context.close()
        except Exception:
            pass


@pytest.hookimpl(tryfirst=True, hookwrapper=True)
def pytest_runtest_makereport(item, call):
    """Store test result for use in fixtures."""
    outcome = yield
    rep = outcome.get_result()
    setattr(item, f"rep_{rep.when}", rep)


@pytest.hookimpl(hookwrapper=True)
def pytest_runtest_teardown(item, nextitem):
    """Embed artifacts in HTML report after test teardown (when artifacts exist)."""
    yield
    
    # Only process UI tests
    if "ui" not in [m.name for m in item.iter_markers()]:
        return
    
    # Get the call report to add extras to
    rep = getattr(item, "rep_call", None)
    if rep is None:
        return
    
    test_name = item.name.replace("/", "_").replace("::", "_").replace("[", "_").replace("]", "_")
    extras = getattr(rep, "extras", [])
    
    try:
        from pytest_html import extras as html_extras
        import base64
        
        test_failed = rep.failed
        
        # Add screenshot if exists (thumbnail that expands on click)
        screenshot_path = os.path.join(_get_screenshots_dir(), f"{test_name}.png")
        if os.path.exists(screenshot_path):
            with open(screenshot_path, "rb") as f:
                screenshot_data = base64.b64encode(f.read()).decode()
            extras.append(html_extras.html(
                f'<details><summary>📸 Screenshot (click to expand)</summary>'
                f'<div class="image" style="margin-top:8px;">'
                f'<img src="data:image/png;base64,{screenshot_data}" '
                f'alt="Screenshot" style="max-width:100%; border:1px solid #ccc;"/>'
                f'</div></details>'
            ))
        
        # Add video link if exists (only for failed tests - passing tests shouldn't have videos)
        # Note: Videos are linked rather than embedded to avoid bloating HTML report
        # (a 30-second WebM can be 5-15MB; base64 adds ~33% overhead)
        video_path = os.path.join(_get_videos_dir(), f"{test_name}.webm")
        if os.path.exists(video_path):
            video_size = os.path.getsize(video_path)
            size_str = f"{video_size / 1024 / 1024:.1f} MB" if video_size > 1024*1024 else f"{video_size / 1024:.0f} KB"
            extras.append(html_extras.html(
                f'<details><summary>🎬 Video Recording ({size_str})</summary>'
                f'<div class="video" style="margin-top:8px;">'
                f'<video controls style="max-width:100%; border:1px solid #ccc;">'
                f'<source src="videos/{test_name}.webm" type="video/webm">'
                f'Your browser does not support the video tag.'
                f'</video>'
                f'<p style="margin-top:4px;"><a href="videos/{test_name}.webm" download>📥 Download video</a></p>'
                f'</div></details>'
            ))
        
        # Add trace link if exists
        trace_path = os.path.join(_get_traces_dir(), f"{test_name}.zip")
        if os.path.exists(trace_path):
            # Get file size for display
            trace_size = os.path.getsize(trace_path)
            size_str = f"{trace_size / 1024 / 1024:.1f} MB" if trace_size > 1024*1024 else f"{trace_size / 1024:.0f} KB"
            # Link to trace.playwright.dev with the trace file
            trace_viewer_url = f"https://trace.playwright.dev/?trace=traces/{test_name}.zip"
            extras.append(html_extras.html(
                f'<details open><summary>🔍 Trace ({size_str})</summary>'
                f'<div style="margin:8px 0; padding:8px; background:#f5f5f5; border-radius:4px; font-size:0.9em;">'
                f'<p style="margin:0 0 8px 0;"><strong>View trace:</strong></p>'
                f'<ul style="margin:0; padding-left:20px;">'
                f'<li><a href="https://trace.playwright.dev" target="_blank">trace.playwright.dev</a> '
                f'(upload <code>traces/{test_name}.zip</code>)</li>'
                f'<li>Local: <code>playwright show-trace reports/traces/{test_name}.zip</code></li>'
                f'</ul>'
                f'<p style="margin:8px 0 0 0;"><a href="traces/{test_name}.zip" download>📥 Download trace</a></p>'
                f'</div></details>'
            ))
        
        rep.extras = extras
    except ImportError:
        pass


def _cleanup_orphaned_videos(videos_dir: str) -> None:
    """Remove orphaned video files (those with hash names instead of test names).
    
    Playwright creates videos with hash names during recording. We rename them
    to test names when we want to keep them. Any remaining hash-named files
    are orphans from the authentication context or tests that passed.
    """
    if not os.path.exists(videos_dir):
        return
    
    for filename in os.listdir(videos_dir):
        if filename.endswith(".webm"):
            # Keep files that start with "test_" (properly named test videos)
            # Remove files that are just hash names (32 hex chars + .webm)
            name_without_ext = filename[:-5]  # Remove .webm
            if len(name_without_ext) == 32 and all(c in "0123456789abcdef" for c in name_without_ext):
                filepath = os.path.join(videos_dir, filename)
                try:
                    os.remove(filepath)
                except Exception:
                    pass


def pytest_sessionfinish(session, exitstatus):
    """Copy UI test artifacts to ARTIFACT_DIR if set (for CI).
    
    This enables CI systems to collect Playwright artifacts alongside
    other test outputs like JUnit XML.
    
    Before copying, cleans up orphaned video files that weren't renamed
    (i.e., videos from passing tests in retain-on-failure mode).
    """
    reports_dir = _get_reports_base_dir()
    
    # Clean up orphaned videos before copying (or just for local cleanup)
    videos_dir = os.path.join(reports_dir, "videos")
    _cleanup_orphaned_videos(videos_dir)
    
    artifact_dir = os.environ.get("ARTIFACT_DIR")
    if not artifact_dir:
        return
    
    if not os.path.exists(reports_dir):
        return
    
    # Create playwright subdirectory in artifact dir
    pw_artifact_dir = os.path.join(artifact_dir, "playwright")
    os.makedirs(pw_artifact_dir, exist_ok=True)
    
    # Copy HTML report if it exists
    html_report = os.path.join(reports_dir, "report.html")
    if os.path.exists(html_report):
        try:
            shutil.copy2(html_report, os.path.join(pw_artifact_dir, "report.html"))
            print(f"\n📄 Copied HTML report to {pw_artifact_dir}/report.html")
        except Exception as e:
            print(f"\n⚠️ Failed to copy HTML report: {e}")
    
    # Copy each artifact type if it has meaningful content
    artifact_types = [
        ("screenshots", "screenshots", ".png"),
        ("videos", "videos", ".webm"),
        ("traces", "traces", ".zip"),
    ]
    
    for src_name, dst_name, ext in artifact_types:
        src_dir = os.path.join(reports_dir, src_name)
        if os.path.exists(src_dir):
            # Only copy if there are actual artifact files (not just .DS_Store etc)
            artifact_files = [f for f in os.listdir(src_dir) if f.endswith(ext)]
            if artifact_files:
                dst_dir = os.path.join(pw_artifact_dir, dst_name)
                try:
                    shutil.copytree(src_dir, dst_dir, dirs_exist_ok=True)
                    print(f"\n📦 Copied {len(artifact_files)} {src_name} to {dst_dir}")
                except Exception as e:
                    print(f"\n⚠️ Failed to copy {src_name}: {e}")
    
    # Generate an index.html for easy navigation
    _generate_artifact_index(pw_artifact_dir)


def _generate_artifact_index(artifact_dir: str) -> None:
    """Generate a simple HTML index for browsing artifacts."""
    screenshots = []
    videos = []
    traces = []
    
    screenshots_dir = os.path.join(artifact_dir, "screenshots")
    videos_dir = os.path.join(artifact_dir, "videos")
    traces_dir = os.path.join(artifact_dir, "traces")
    
    if os.path.exists(screenshots_dir):
        screenshots = [f for f in os.listdir(screenshots_dir) if f.endswith(".png")]
    if os.path.exists(videos_dir):
        videos = [f for f in os.listdir(videos_dir) if f.endswith(".webm")]
    if os.path.exists(traces_dir):
        traces = [f for f in os.listdir(traces_dir) if f.endswith(".zip")]
    
    html = f"""<!DOCTYPE html>
<html>
<head>
    <title>Playwright Test Artifacts</title>
    <style>
        body {{ font-family: -apple-system, BlinkMacSystemFont, 'Segoe UI', Roboto, sans-serif; margin: 2rem; }}
        h1 {{ color: #1a1a1a; }}
        h2 {{ color: #444; margin-top: 2rem; }}
        ul {{ list-style: none; padding: 0; }}
        li {{ padding: 0.5rem 0; }}
        a {{ color: #0066cc; text-decoration: none; }}
        a:hover {{ text-decoration: underline; }}
        .empty {{ color: #888; font-style: italic; }}
        .section {{ background: #f5f5f5; padding: 1rem; border-radius: 8px; margin: 1rem 0; }}
        .trace-info {{ font-size: 0.9em; color: #666; margin-left: 1rem; }}
    </style>
</head>
<body>
    <h1>🎭 Playwright Test Artifacts</h1>
    
    <div class="section">
        <h2>📸 Screenshots ({len(screenshots)})</h2>
        {"<ul>" + "".join(f'<li><a href="screenshots/{f}">{f}</a></li>' for f in sorted(screenshots)) + "</ul>" if screenshots else '<p class="empty">No screenshots captured</p>'}
    </div>
    
    <div class="section">
        <h2>🎬 Videos ({len(videos)})</h2>
        {"<ul>" + "".join(f'<li><a href="videos/{f}">{f}</a></li>' for f in sorted(videos)) + "</ul>" if videos else '<p class="empty">No videos captured</p>'}
    </div>
    
    <div class="section">
        <h2>🔍 Traces ({len(traces)})</h2>
        {"<ul>" + "".join(f'<li><a href="traces/{f}">{f}</a> <span class="trace-info">→ Upload to <a href="https://trace.playwright.dev" target="_blank">trace.playwright.dev</a></span></li>' for f in sorted(traces)) + "</ul>" if traces else '<p class="empty">No traces captured</p>'}
    </div>
    
    <p style="margin-top: 2rem; color: #888; font-size: 0.9em;">
        Generated by cost-onprem-chart UI tests
    </p>
</body>
</html>"""
    
    index_path = os.path.join(artifact_dir, "index.html")
    try:
        with open(index_path, "w") as f:
            f.write(html)
        print(f"\n📄 Artifact index: {index_path}")
    except Exception as e:
        print(f"\n⚠️ Failed to generate index: {e}")
