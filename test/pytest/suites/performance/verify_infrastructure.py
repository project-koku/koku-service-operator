#!/usr/bin/env python3
"""
Verification script for performance test infrastructure.

Run this to validate the performance testing framework without
executing actual performance tests against a cluster.

Usage:
    python3 tests/suites/performance/verify_infrastructure.py
    
Or via pytest (for fixture verification):
    pytest tests/suites/performance/verify_infrastructure.py -v
"""

import json
import os
import sys
import tempfile
from datetime import datetime, timedelta, timezone
from pathlib import Path

# When running standalone (not via pytest), ensure the tests/ directory is
# on sys.path.  ``pytest`` handles this automatically.
_tests_dir = str(Path(__file__).parent.parent.parent)
if _tests_dir not in sys.path:
    sys.path.insert(0, _tests_dir)


def test_imports():
    """Verify all modules import correctly."""
    print("=" * 60)
    print("1. Testing imports...")
    print("=" * 60)
    
    try:
        from suites.performance import conftest
        print("  ✓ conftest.py imports")
    except ImportError as e:
        print(f"  ✗ conftest.py failed: {e}")
        return False
    
    try:
        from suites.performance import profiles
        print("  ✓ profiles.py imports")
    except ImportError as e:
        print(f"  ✗ profiles.py failed: {e}")
        return False
    
    try:
        from suites.performance import test_ingestion
        print("  ✓ test_ingestion.py imports")
    except ImportError as e:
        print(f"  ✗ test_ingestion.py failed: {e}")
        return False
    
    try:
        from suites.performance import test_api_latency
        print("  ✓ test_api_latency.py imports")
    except ImportError as e:
        print(f"  ✗ test_api_latency.py failed: {e}")
        return False
    
    try:
        from suites.performance import test_scale
        print("  ✓ test_scale.py imports")
    except ImportError as e:
        print(f"  ✗ test_scale.py failed: {e}")
        return False
    
    print("  All imports successful!\n")
    return True


def test_profiles():
    """Verify profile definitions and calculations."""
    print("=" * 60)
    print("2. Testing profile definitions...")
    print("=" * 60)
    
    from suites.performance.profiles import (
        PROFILES,
        get_profile_metrics,
        calculate_daily_rows,
        calculate_upload_size_mb,
    )
    
    required_profiles = ["baseline", "small", "medium", "large", "xlarge"]
    
    for profile_name in required_profiles:
        if profile_name not in PROFILES:
            print(f"  ✗ Missing required profile: {profile_name}")
            return False
        
        profile = PROFILES[profile_name]
        metrics = get_profile_metrics(profile_name)
        
        # Validate structure
        required_keys = ["clusters", "nodes_per_cluster", "cpu_cores_per_node"]
        for key in required_keys:
            if key not in profile:
                print(f"  ✗ Profile '{profile_name}' missing key: {key}")
                return False
        
        # Validate calculations
        daily_rows = calculate_daily_rows(profile)
        if daily_rows <= 0:
            print(f"  ✗ Profile '{profile_name}' has invalid daily_rows: {daily_rows}")
            return False
        
        upload_mb = calculate_upload_size_mb(profile, 1)
        if upload_mb <= 0:
            print(f"  ✗ Profile '{profile_name}' has invalid upload_mb: {upload_mb}")
            return False
        
        print(f"  ✓ {profile_name}: {metrics['total_nodes']} nodes, "
              f"{metrics['daily_rows']:,} rows/day, ~{metrics['daily_upload_mb']} MB/day")
    
    print(f"  All {len(required_profiles)} required profiles valid!\n")
    return True


def test_nise_yaml_generation():
    """Verify NISE YAML generation."""
    print("=" * 60)
    print("3. Testing NISE YAML generation...")
    print("=" * 60)
    
    from suites.performance.profiles import get_profile_nise_yaml
    import yaml
    
    start_date = datetime.now(timezone.utc) - timedelta(days=1)
    end_date = datetime.now(timezone.utc)
    cluster_id = "test-cluster-12345678"
    
    for profile_name in ["baseline", "small"]:
        try:
            yaml_content = get_profile_nise_yaml(
                profile_name, start_date, end_date, cluster_id, 0
            )
            
            # Validate it's parseable YAML
            data = yaml.safe_load(yaml_content)
            
            if "generators" not in data:
                print(f"  ✗ {profile_name}: Missing 'generators' key")
                return False
            
            if "OCPGenerator" not in data["generators"][0]:
                print(f"  ✗ {profile_name}: Missing 'OCPGenerator'")
                return False
            
            gen = data["generators"][0]["OCPGenerator"]
            if "nodes" not in gen:
                print(f"  ✗ {profile_name}: Missing 'nodes'")
                return False
            
            node_count = len(gen["nodes"])
            print(f"  ✓ {profile_name}: Generated valid YAML with {node_count} nodes")
            
        except Exception as e:
            print(f"  ✗ {profile_name}: YAML generation failed: {e}")
            return False
    
    print("  NISE YAML generation working!\n")
    return True


def test_data_classes():
    """Verify data classes serialize correctly."""
    print("=" * 60)
    print("4. Testing data classes...")
    print("=" * 60)
    
    from suites.performance.data_classes import (
        ClusterInfo,
        TimingMetric,
        PerformanceResult,
        ResourceSnapshot,
    )
    
    # Test ClusterInfo
    cluster_info = ClusterInfo(
        ocp_version="4.20.0",
        node_count=5,
        worker_node_count=3,
        total_cpu_cores=48,
        total_memory_gib=192.0,
        storage_class="ocs-storagecluster-ceph-rbd",
        storage_type="ODF",
        s3_backend="NooBaa",
        platform="bare-metal",
    )
    
    cluster_dict = cluster_info.to_dict()
    if cluster_dict["ocp_version"] != "4.20.0":
        print("  ✗ ClusterInfo serialization failed")
        return False
    print("  ✓ ClusterInfo serializes correctly")
    
    # Test TimingMetric
    timing = TimingMetric(
        name="test_operation",
        duration_seconds=1.234,
        start_time="2026-04-16T12:00:00Z",
        end_time="2026-04-16T12:00:01.234Z",
        metadata={"key": "value"},
    )
    
    timing_dict = timing.to_dict()
    if timing_dict["duration_seconds"] != 1.234:
        print("  ✗ TimingMetric serialization failed")
        return False
    print("  ✓ TimingMetric serializes correctly")
    
    # Test PerformanceResult
    result = PerformanceResult(
        test_id="test-123",
        test_name="test_example",
        profile="small",
        chart_version="0.2.20",
        timestamp="2026-04-16T12:00:00Z",
        cluster_info=cluster_info,
        timings=[timing],
        metrics={"key": "value"},
        passed=True,
    )
    
    result_dict = result.to_dict()
    
    # Validate JSON serializable
    try:
        json_str = json.dumps(result_dict, indent=2)
        parsed = json.loads(json_str)
        if parsed["test_id"] != "test-123":
            raise ValueError("Round-trip failed")
        print("  ✓ PerformanceResult serializes to valid JSON")
    except Exception as e:
        print(f"  ✗ PerformanceResult JSON serialization failed: {e}")
        return False
    
    print("  All data classes working!\n")
    return True


def test_perf_timer():
    """Verify PerfTimer works correctly."""
    print("=" * 60)
    print("5. Testing PerfTimer...")
    print("=" * 60)
    
    import time
    from suites.performance.helpers import PerfTimer
    
    timer = PerfTimer()
    
    # Test manual start/stop
    timer.start("manual_test")
    time.sleep(0.1)
    duration = timer.stop("manual_test")
    
    if duration < 0.09 or duration > 0.2:
        print(f"  ✗ Manual timing inaccurate: {duration}s (expected ~0.1s)")
        return False
    print(f"  ✓ Manual timing: {duration:.3f}s (expected ~0.1s)")
    
    # Test context manager
    with timer.measure("context_test"):
        time.sleep(0.05)
    
    timing = timer.get_timing("context_test")
    if timing is None:
        print("  ✗ Context manager timing not recorded")
        return False
    
    if timing.duration_seconds < 0.04 or timing.duration_seconds > 0.15:
        print(f"  ✗ Context timing inaccurate: {timing.duration_seconds}s")
        return False
    print(f"  ✓ Context manager timing: {timing.duration_seconds:.3f}s")
    
    # Test get_timings
    all_timings = timer.get_timings()
    if len(all_timings) != 2:
        print(f"  ✗ Expected 2 timings, got {len(all_timings)}")
        return False
    print(f"  ✓ Collected {len(all_timings)} timings")
    
    print("  PerfTimer working correctly!\n")
    return True


def test_json_schema():
    """Verify JSON schema is valid."""
    print("=" * 60)
    print("6. Testing JSON schema...")
    print("=" * 60)
    
    schema_path = Path(__file__).parent / "schema.json"
    
    if not schema_path.exists():
        print(f"  ✗ Schema file not found: {schema_path}")
        return False
    
    try:
        with open(schema_path) as f:
            schema = json.load(f)
        
        if "$schema" not in schema:
            print("  ✗ Missing $schema field")
            return False
        
        if "definitions" not in schema:
            print("  ✗ Missing definitions")
            return False
        
        required_defs = ["ClusterInfo", "TimingMetric", "ResourceSnapshot"]
        for def_name in required_defs:
            if def_name not in schema["definitions"]:
                print(f"  ✗ Missing definition: {def_name}")
                return False
        
        print(f"  ✓ Schema valid with {len(schema['definitions'])} definitions")
        
    except json.JSONDecodeError as e:
        print(f"  ✗ Invalid JSON: {e}")
        return False
    
    print("  JSON schema valid!\n")
    return True


def test_latency_helpers():
    """Verify latency calculation helpers."""
    print("=" * 60)
    print("7. Testing latency helpers...")
    print("=" * 60)
    
    from suites.performance.test_api_latency import calculate_percentiles
    
    # Test with known values
    latencies = [0.1, 0.2, 0.3, 0.4, 0.5, 0.6, 0.7, 0.8, 0.9, 1.0]
    
    result = calculate_percentiles(latencies)
    
    if result["min"] != 0.1:
        print(f"  ✗ Min incorrect: {result['min']} (expected 0.1)")
        return False
    
    if result["max"] != 1.0:
        print(f"  ✗ Max incorrect: {result['max']} (expected 1.0)")
        return False
    
    if abs(result["avg"] - 0.55) > 0.01:
        print(f"  ✗ Avg incorrect: {result['avg']} (expected 0.55)")
        return False
    
    if result["count"] != 10:
        print(f"  ✗ Count incorrect: {result['count']} (expected 10)")
        return False
    
    print(f"  ✓ Percentiles: p50={result['p50']}, p95={result['p95']}, p99={result['p99']}")
    print(f"  ✓ Stats: min={result['min']}, max={result['max']}, avg={result['avg']}")
    
    # Test empty list
    empty_result = calculate_percentiles([])
    if empty_result.get("count", 0) != 0 and empty_result.get("p50", 0) != 0:
        print("  ✗ Empty list handling failed")
        return False
    print("  ✓ Empty list handled correctly")
    
    print("  Latency helpers working!\n")
    return True


def test_report_saving():
    """Verify report saving works."""
    print("=" * 60)
    print("8. Testing report saving...")
    print("=" * 60)
    
    from suites.performance.data_classes import ClusterInfo, PerformanceResult
    from suites.performance.helpers import save_perf_result
    
    with tempfile.TemporaryDirectory() as tmpdir:
        result = PerformanceResult(
            test_id="test-save-123",
            test_name="test_save_example",
            profile="small",
            chart_version="0.2.20",
            timestamp="2026-04-16T12:00:00Z",
            cluster_info=ClusterInfo(),
            metrics={"test_key": "test_value"},
            passed=True,
        )
        
        output_path = save_perf_result(result, Path(tmpdir))
        
        if not output_path.exists():
            print(f"  ✗ Report file not created: {output_path}")
            return False
        
        with open(output_path) as f:
            saved = json.load(f)
        
        if saved["test_id"] != "test-save-123":
            print("  ✗ Saved data doesn't match")
            return False
        
        print(f"  ✓ Report saved to: {output_path.name}")
        print(f"  ✓ File size: {output_path.stat().st_size} bytes")
    
    print("  Report saving working!\n")
    return True


def main():
    """Run all verification tests."""
    print("\n" + "=" * 60)
    print("PERFORMANCE TEST INFRASTRUCTURE VERIFICATION")
    print("=" * 60 + "\n")
    
    tests = [
        ("Imports", test_imports),
        ("Profiles", test_profiles),
        ("NISE YAML Generation", test_nise_yaml_generation),
        ("Data Classes", test_data_classes),
        ("PerfTimer", test_perf_timer),
        ("JSON Schema", test_json_schema),
        ("Latency Helpers", test_latency_helpers),
        ("Report Saving", test_report_saving),
    ]
    
    results = []
    for name, test_func in tests:
        try:
            passed = test_func()
            results.append((name, passed))
        except Exception as e:
            print(f"  ✗ {name} raised exception: {e}")
            results.append((name, False))
    
    # Summary
    print("=" * 60)
    print("SUMMARY")
    print("=" * 60)
    
    passed = sum(1 for _, p in results if p)
    failed = len(results) - passed
    
    for name, p in results:
        status = "✓ PASS" if p else "✗ FAIL"
        print(f"  {status}: {name}")
    
    print()
    print(f"  Total: {passed}/{len(results)} passed")
    
    if failed > 0:
        print(f"\n  {failed} test(s) failed!")
        return 1
    else:
        print("\n  All infrastructure tests passed!")
        return 0


if __name__ == "__main__":
    sys.exit(main())
