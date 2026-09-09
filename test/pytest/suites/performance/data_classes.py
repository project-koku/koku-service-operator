"""Data classes for performance test metrics and results."""

from dataclasses import asdict, dataclass, field
from typing import Any, Dict, List, Optional


@dataclass
class ClusterInfo:
    """Information about the target cluster for performance context."""

    ocp_version: str = ""
    node_count: int = 0
    worker_node_count: int = 0
    total_cpu_cores: int = 0
    total_memory_gib: float = 0.0
    storage_class: str = ""
    storage_type: str = ""
    s3_backend: str = ""
    platform: str = ""

    def to_dict(self) -> Dict[str, Any]:
        return asdict(self)


@dataclass
class ResourceSnapshot:
    """Point-in-time resource usage snapshot."""

    timestamp: str
    pod_name: str
    cpu_usage_cores: float
    cpu_request_cores: float
    cpu_limit_cores: float
    memory_usage_mib: float
    memory_request_mib: float
    memory_limit_mib: float

    def to_dict(self) -> Dict[str, Any]:
        return asdict(self)


@dataclass
class TimingMetric:
    """Timing measurement for a specific operation."""

    name: str
    duration_seconds: float
    start_time: str
    end_time: str
    metadata: Dict[str, Any] = field(default_factory=dict)

    def to_dict(self) -> Dict[str, Any]:
        return asdict(self)


@dataclass
class PerformanceResult:
    """Complete performance test result."""

    test_id: str
    test_name: str
    profile: str
    chart_version: str
    timestamp: str
    cluster_info: ClusterInfo
    timings: List[TimingMetric] = field(default_factory=list)
    resource_snapshots: List[ResourceSnapshot] = field(default_factory=list)
    metrics: Dict[str, Any] = field(default_factory=dict)
    passed: bool = True
    error_message: Optional[str] = None

    def to_dict(self) -> Dict[str, Any]:
        result = asdict(self)
        result["cluster_info"] = self.cluster_info.to_dict()
        result["timings"] = [t.to_dict() for t in self.timings]
        result["resource_snapshots"] = [r.to_dict() for r in self.resource_snapshots]
        return result
