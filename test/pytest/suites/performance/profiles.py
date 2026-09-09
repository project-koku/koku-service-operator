"""
Performance Profile Definitions for NISE Data Generation (FLPATH-4036).

Based on production data analysis from Pau Garcia Quiles (April 2026).

These profiles define workload characteristics that can be used to generate
realistic test data via NISE for different customer size categories.

Usage:
    from suites.performance.profiles import get_profile_nise_yaml, PROFILES
    
    # Get NISE YAML for a profile
    yaml_content = get_profile_nise_yaml("small", start_date, end_date, cluster_id)
    
    # Get profile configuration
    profile = PROFILES["medium"]
    print(f"Clusters: {profile['clusters']}")
"""

import os
from dataclasses import dataclass, field
from datetime import datetime
from typing import Any, Dict, List, Optional
import uuid

# Active profile selected by the test runner — used by all perf test files.
ACTIVE_PROFILE: str = os.environ.get("PERF_PROFILE", "baseline")


# =============================================================================
# Profile Definitions (from Pau's Production Data - April 2026)
# =============================================================================

PROFILES: Dict[str, Dict[str, Any]] = {
    "small": {
        "description": "37% of customers - Single cluster, 15 nodes, 200 cores",
        "percentile": "median-small",
        "clusters": 1,
        "nodes_per_cluster": 15,
        "cpu_cores_per_node": 13,
        "memory_gib_per_node": 73,
        "namespaces_per_cluster": 10,
        "pods_per_namespace": 5,
        "pvcs_per_cluster": 48,
        "cost_models": 1,
        "cost_model_type": "cpu_distribution",
        "data_days": 30,
        "upload_interval_hours": 6,
    },
    "medium": {
        "description": "35% of customers - 2 clusters, 49 nodes, 544 cores",
        "percentile": "median-medium",
        "clusters": 2,
        "nodes_per_cluster": 25,
        "cpu_cores_per_node": 11,
        "memory_gib_per_node": 57,
        "namespaces_per_cluster": 20,
        "pods_per_namespace": 8,
        "pvcs_per_cluster": 89,
        "cost_models": 1,
        "cost_model_type": "cpu_distribution",
        "data_days": 30,
        "upload_interval_hours": 6,
    },
    "large": {
        "description": "21% of customers - 7 clusters, 133 nodes, 1964 cores",
        "percentile": "median-large",
        "clusters": 7,
        "nodes_per_cluster": 19,
        "cpu_cores_per_node": 15,
        "memory_gib_per_node": 73,
        "namespaces_per_cluster": 30,
        "pods_per_namespace": 10,
        "pvcs_per_cluster": 70,
        "cost_models": 2,
        "cost_model_type": "cpu_distribution",
        "data_days": 30,
        "upload_interval_hours": 6,
    },
    "xlarge": {
        "description": "6% of customers - 23 clusters, 346 nodes, 6954 cores",
        "percentile": "median-xlarge",
        "clusters": 23,
        "nodes_per_cluster": 15,
        "cpu_cores_per_node": 20,
        "memory_gib_per_node": 140,
        "namespaces_per_cluster": 40,
        "pods_per_namespace": 15,
        "pvcs_per_cluster": 55,
        "cost_models": 3,
        "cost_model_type": "cpu_distribution_with_tags",
        "data_days": 30,
        "upload_interval_hours": 6,
    },
    "stress_p99": {
        "description": "P99 stress test - 33 clusters, 1072 nodes, 57k cores",
        "percentile": "p99",
        "clusters": 33,
        "nodes_per_cluster": 32,
        "cpu_cores_per_node": 54,
        "memory_gib_per_node": 128,
        "namespaces_per_cluster": 50,
        "pods_per_namespace": 20,
        "pvcs_per_cluster": 185,
        "cost_models": 7,
        "cost_model_type": "cpu_distribution_with_tags",
        "data_days": 30,
        "upload_interval_hours": 6,
    },
    "stress_max": {
        "description": "Max observed - 67 clusters, 4311 nodes, 793k cores",
        "percentile": "max",
        "clusters": 67,
        "nodes_per_cluster": 64,
        "cpu_cores_per_node": 185,
        "memory_gib_per_node": 256,
        "namespaces_per_cluster": 100,
        "pods_per_namespace": 50,
        "pvcs_per_cluster": 484,
        "cost_models": 12,
        "cost_model_type": "cpu_distribution_with_tags",
        "data_days": 30,
        "upload_interval_hours": 6,
    },
    "baseline": {
        "description": "Minimal baseline - 1 cluster, 3 nodes, single namespace",
        "percentile": "baseline",
        "clusters": 1,
        "nodes_per_cluster": 3,
        "cpu_cores_per_node": 4,
        "memory_gib_per_node": 16,
        "namespaces_per_cluster": 1,
        "pods_per_namespace": 3,
        "pvcs_per_cluster": 3,
        "cost_models": 0,
        "cost_model_type": None,
        "data_days": 1,
        "upload_interval_hours": 6,
    },
    "single_source_burst": {
        "description": "PERF-ING-002 - Single source with 90 days of data",
        "percentile": "burst",
        "clusters": 1,
        "nodes_per_cluster": 15,
        "cpu_cores_per_node": 16,
        "memory_gib_per_node": 64,
        "namespaces_per_cluster": 20,
        "pods_per_namespace": 10,
        "pvcs_per_cluster": 50,
        "cost_models": 1,
        "cost_model_type": "cpu_distribution",
        "data_days": 90,
        "upload_interval_hours": 6,
    },
}


# =============================================================================
# Row Count Calculations
# =============================================================================

def calculate_daily_rows(profile: Dict[str, Any]) -> int:
    """Calculate expected daily row count for a profile.
    
    Formula: pods × 288 intervals/day × (pod_usage + storage_usage factor ~1.0)
    """
    total_pods = (
        profile["nodes_per_cluster"]
        * profile["namespaces_per_cluster"]
        * profile["pods_per_namespace"]
        * profile["clusters"]
    )
    intervals_per_day = 288  # 5-minute intervals
    storage_factor = 1.0  # Approximate storage usage rows
    
    return int(total_pods * intervals_per_day * (1 + storage_factor))


def calculate_monthly_rows(profile: Dict[str, Any]) -> int:
    """Calculate expected monthly row count."""
    return calculate_daily_rows(profile) * 30


def calculate_upload_size_mb(profile: Dict[str, Any], days: int = 1) -> float:
    """Estimate upload size in MB.
    
    Based on: ~43 bytes/CSV row, ~10:1 compression ratio
    """
    daily_rows = calculate_daily_rows(profile)
    total_rows = daily_rows * days
    uncompressed_bytes = total_rows * 43
    compressed_bytes = uncompressed_bytes / 10
    return compressed_bytes / (1024 * 1024)


def get_profile_metrics(profile_name: str) -> Dict[str, Any]:
    """Get computed metrics for a profile."""
    if profile_name not in PROFILES:
        raise ValueError(f"Unknown profile: {profile_name}")
    
    profile = PROFILES[profile_name]
    
    total_nodes = profile["nodes_per_cluster"] * profile["clusters"]
    total_cores = profile["cpu_cores_per_node"] * total_nodes
    total_memory_tb = (profile["memory_gib_per_node"] * total_nodes) / 1024
    total_pods = (
        profile["namespaces_per_cluster"]
        * profile["pods_per_namespace"]
        * profile["clusters"]
    )
    total_pvcs = profile["pvcs_per_cluster"] * profile["clusters"]
    
    return {
        "profile_name": profile_name,
        "description": profile["description"],
        "total_clusters": profile["clusters"],
        "total_nodes": total_nodes,
        "total_cpu_cores": total_cores,
        "total_memory_tb": round(total_memory_tb, 1),
        "total_namespaces": profile["namespaces_per_cluster"] * profile["clusters"],
        "total_pods": total_pods,
        "total_pvcs": total_pvcs,
        "daily_rows": calculate_daily_rows(profile),
        "monthly_rows": calculate_monthly_rows(profile),
        "daily_upload_mb": round(calculate_upload_size_mb(profile, 1), 2),
        "monthly_upload_mb": round(calculate_upload_size_mb(profile, 30), 2),
    }


# =============================================================================
# NISE YAML Generation
# =============================================================================

def generate_node_yaml(
    node_index: int,
    cpu_cores: int,
    memory_gib: int,
    namespaces: int,
    pods_per_namespace: int,
    base_name: str = "perf",
) -> str:
    """Generate YAML for a single node.
    
    NISE YAML format uses SIBLING keys for list items (2-space indent after marker):
    - node: followed by node_name: at +2 spaces (sibling, not nested)
    - pod: followed by pod_name: at +2 spaces (sibling, not nested)
    - node_labels for node labels (not 'labels')
    - namespace_labels for namespace labels (not 'labels')
    - labels for pod labels
    
    Example of correct NISE format:
        - node:
          node_name: my-node  # 2 spaces after list marker = sibling
        - pod:
          pod_name: my-pod    # 2 spaces after list marker = sibling
    """
    node_name = f"{base_name}-node-{node_index:03d}"
    resource_id = f"{base_name}-resource-{node_index:03d}"
    
    namespaces_yaml = []
    for ns_idx in range(namespaces):
        ns_name = f"{base_name}-ns-{ns_idx:03d}"
        pods_yaml = []
        
        for pod_idx in range(pods_per_namespace):
            pod_name = f"{base_name}-pod-{ns_idx:03d}-{pod_idx:03d}"
            # Vary CPU/memory slightly per pod for realistic data
            cpu_request = 0.25 + (pod_idx % 4) * 0.25  # 0.25, 0.5, 0.75, 1.0
            mem_request = 0.5 + (pod_idx % 4) * 0.5  # 0.5, 1.0, 1.5, 2.0
            cpu_usage = cpu_request * (0.4 + (pod_idx % 3) * 0.2)  # 40-80% utilization
            mem_usage = mem_request * (0.5 + (pod_idx % 3) * 0.15)  # 50-80% utilization
            
            # Varied labels to ensure we have 10+ unique tag keys
            # Using NATO phonetic alphabet names to avoid any reserved word issues
            val_b = ['alfa', 'bravo', 'charlie', 'delta'][pod_idx % 4]
            val_d = ['echo', 'foxtrot', 'golf', 'hotel'][(pod_idx + ns_idx) % 4]
            val_f = ['india', 'juliet', 'kilo'][pod_idx % 3]
            val_h = ['lima', 'mike', 'november'][ns_idx % 3]
            val_j = ['oscar', 'papa', 'quebec', 'romeo'][pod_idx % 4]
            val_l = ['sierra', 'tango', 'uniform'][pod_idx % 3]
            
            # Build labels string with 10 unique keys using NATO alphabet
            # IMPORTANT: NISE requires 'label_' prefix - it's stripped during processing
            labels = (
                f"label_tagone:perf|"
                f"label_tagtwo:{base_name[:8]}|"
                f"label_tagthree:{val_b}|"
                f"label_tagfour:{val_d}|"
                f"label_tagfive:{val_f}|"
                f"label_tagsix:{val_h}|"
                f"label_tagseven:{val_j}|"
                f"label_tageight:{val_l}|"
                f"label_tagnine:static|"
                f"label_tagten:fixed"
            )
            
            # Pod YAML with SIBLING keys (2-space indent = sibling of 'pod:')
            # pods: is at 14 spaces, list marker at 16, sibling props at 18
            pods_yaml.append(f"""                - pod:
                  pod_name: {pod_name}
                  cpu_request: {cpu_request}
                  mem_request_gig: {mem_request}
                  cpu_limit: {cpu_request * 2}
                  mem_limit_gig: {mem_request * 2}
                  pod_seconds: 3600
                  cpu_usage:
                    full_period: {cpu_usage:.3f}
                  mem_usage_gig:
                    full_period: {mem_usage:.3f}
                  labels: {labels}""")
        
        # Namespace YAML with namespace_labels (dict key, so properties are nested)
        # IMPORTANT: NISE requires 'label_' prefix for namespace labels too
        namespaces_yaml.append(f"""            {ns_name}:
              namespace_labels: label_nslabel1:nsval1|label_nslabel2:nsval2
              pods:
{chr(10).join(pods_yaml)}""")
    
    # Node YAML with SIBLING keys (2-space indent = sibling of 'node:')
    # IMPORTANT: NISE requires 'label_' prefix for node labels too
    # nodes: is at 6 spaces, so - node: must be at 8 spaces, properties at 10
    return f"""        - node:
          node_name: {node_name}
          node_labels: label_nodelabel1:nodeval1|label_nodelabel2:nodeval2|label_nodelabel3:nodeval3
          cpu_cores: {cpu_cores}
          memory_gig: {memory_gib}
          resource_id: {resource_id}
          namespaces:
{chr(10).join(namespaces_yaml)}"""


def get_profile_nise_yaml(
    profile_name: str,
    start_date: datetime,
    end_date: datetime,
    cluster_id: str,
    cluster_index: int = 0,
) -> str:
    """Generate NISE static report YAML for a profile.
    
    Args:
        profile_name: Name of the profile (small, medium, large, xlarge, etc.)
        start_date: Start date for the report
        end_date: End date for the report
        cluster_id: UUID for the cluster
        cluster_index: Index for multi-cluster profiles (0-based)
        
    Returns:
        NISE-compatible YAML string
    """
    if profile_name not in PROFILES:
        raise ValueError(f"Unknown profile: {profile_name}")
    
    profile = PROFILES[profile_name]
    base_name = f"perf-{profile_name}-c{cluster_index:02d}"
    
    # Generate nodes
    nodes_yaml = []
    for node_idx in range(profile["nodes_per_cluster"]):
        nodes_yaml.append(generate_node_yaml(
            node_index=node_idx,
            cpu_cores=profile["cpu_cores_per_node"],
            memory_gib=profile["memory_gib_per_node"],
            namespaces=profile["namespaces_per_cluster"],
            pods_per_namespace=profile["pods_per_namespace"],
            base_name=base_name,
        ))
    
    return f"""---
# Performance Profile: {profile_name}
# {profile['description']}
# Cluster {cluster_index + 1} of {profile['clusters']}
# Generated: {datetime.utcnow().isoformat()}
generators:
  - OCPGenerator:
      start_date: {start_date.strftime('%Y-%m-%d')}
      end_date: {end_date.strftime('%Y-%m-%d')}
      nodes:
{chr(10).join(nodes_yaml)}
"""


def generate_all_cluster_yamls(
    profile_name: str,
    start_date: datetime,
    end_date: datetime,
) -> List[Dict[str, str]]:
    """Generate NISE YAMLs for all clusters in a profile.
    
    Returns:
        List of dicts with 'cluster_id' and 'yaml_content' keys
    """
    if profile_name not in PROFILES:
        raise ValueError(f"Unknown profile: {profile_name}")
    
    profile = PROFILES[profile_name]
    results = []
    
    for cluster_idx in range(profile["clusters"]):
        cluster_id = str(uuid.uuid4())
        yaml_content = get_profile_nise_yaml(
            profile_name,
            start_date,
            end_date,
            cluster_id,
            cluster_idx,
        )
        results.append({
            "cluster_id": cluster_id,
            "cluster_index": cluster_idx,
            "yaml_content": yaml_content,
        })
    
    return results


# =============================================================================
# Profile Summary
# =============================================================================

def print_profile_summary():
    """Print a summary of all profiles and their metrics."""
    print("\n" + "=" * 80)
    print("Performance Profile Summary")
    print("=" * 80)
    
    for name in PROFILES:
        metrics = get_profile_metrics(name)
        print(f"\n{name.upper()}: {metrics['description']}")
        print(f"  Clusters: {metrics['total_clusters']}, Nodes: {metrics['total_nodes']}, "
              f"Cores: {metrics['total_cpu_cores']}, Memory: {metrics['total_memory_tb']} TB")
        print(f"  Namespaces: {metrics['total_namespaces']}, Pods: {metrics['total_pods']}, "
              f"PVCs: {metrics['total_pvcs']}")
        print(f"  Daily rows: {metrics['daily_rows']:,}, Monthly rows: {metrics['monthly_rows']:,}")
        print(f"  Daily upload: ~{metrics['daily_upload_mb']} MB, "
              f"Monthly upload: ~{metrics['monthly_upload_mb']} MB")
    
    print("\n" + "=" * 80)


if __name__ == "__main__":
    print_profile_summary()
