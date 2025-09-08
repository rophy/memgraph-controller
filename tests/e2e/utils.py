"""
E2E Test Utilities

Simple utilities for running Memgraph e2e tests using kubectl exec approach.
Keeps the same reliable pattern as shell scripts but with better Python tooling.
"""

import subprocess
import json
import time
import sys
import re
import os
from io import StringIO
import logfmt
from datetime import datetime, timezone
from typing import Tuple, Dict, Any, Optional, List


# Configuration
MEMGRAPH_NS = "memgraph"
TEST_CLIENT_LABEL = "app=neo4j-client"
EXPECTED_POD_COUNT = 3


def parse_logfmt(log_line: str) -> Dict[str, Any]:
    """
    Parse logfmt formatted log line into a dictionary using the logfmt library.
    
    Example input: ts=2025-09-07T12:58:34.055123+00:00 at=INFO msg="Success" total=1
    Returns: {"ts": "2025-09-07T12:58:34.055123+00:00", "at": "INFO", "msg": "Success", "total": 1}
    """
    try:
        # Use logfmt library to parse the line
        input_stream = StringIO(log_line.strip())
        parsed_lines = list(logfmt.parse(input_stream))
        if parsed_lines:
            return parsed_lines[0]
        else:
            return {"msg": log_line.strip()}
    except Exception as e:
        print(f"Error parsing logfmt line: {log_line}, error: {e}", file=sys.stderr)
        # Fallback: return basic parsing
        return {"msg": log_line.strip()}


class E2ETestError(Exception):
    """Base exception for e2e test errors"""
    pass


def get_controller_pod() -> str:
    """Get the ready memgraph-controller pod name (leader pod)"""
    try:
        # Get controller pods with ready status in a simple format
        result = subprocess.run([
            "kubectl", "get", "pods", "-n", MEMGRAPH_NS, 
            "-l", "app=memgraph-controller", 
            "--no-headers", "-o", "custom-columns=NAME:.metadata.name,READY:.status.conditions[?(@.type=='Ready')].status"
        ], capture_output=True, text=True, check=True)
        
        # Find the ready pod (leader)
        for line in result.stdout.strip().split('\n'):
            if line.strip():
                parts = line.split()
                if len(parts) >= 2 and parts[1] == 'True':
                    return parts[0]
        
        # Fallback to first ready pod from standard output
        result = subprocess.run([
            "kubectl", "get", "pods", "-n", MEMGRAPH_NS, 
            "-l", "app=memgraph-controller", 
            "--field-selector", "status.phase=Running",
            "-o", "jsonpath={.items[0].metadata.name}"
        ], capture_output=True, text=True, check=True)
        return result.stdout.strip()
    except subprocess.CalledProcessError:
        raise E2ETestError("Failed to find memgraph-controller pod")


def get_pod_logs(pod_name: str, namespace: str = MEMGRAPH_NS, 
                 tail_lines: Optional[int] = None, since_time: Optional[str] = None,
                 output_dir: str = "logs") -> str:
    """
    Generalized function to get pod logs and save them to a file.
    
    Args:
        pod_name: Name of the pod to get logs from
        namespace: Kubernetes namespace (default: memgraph)  
        tail_lines: Number of recent lines to get (optional)
        since_time: RFC3339 time to get logs since (optional)
        output_dir: Directory to save log files (default: logs)
        
    Returns:
        Absolute path to the saved log file
    """
    try:
        # Create output directory if it doesn't exist
        os.makedirs(output_dir, exist_ok=True)
        
        # Generate timestamp for unique filename
        timestamp = datetime.now(timezone.utc).strftime("%Y%m%d_%H%M%S_%f")[:-3]  # microseconds to milliseconds
        log_filename = f"{pod_name}_{timestamp}.log"
        log_filepath = os.path.abspath(os.path.join(output_dir, log_filename))
        
        # Build kubectl logs command
        cmd = ["kubectl", "logs", pod_name, "-n", namespace]
        
        if tail_lines is not None:
            cmd.extend([f"--tail={tail_lines}"])
        if since_time is not None:
            cmd.extend([f"--since-time={since_time}"])
            
        print(f"📄 Getting logs from {pod_name}, saving to: {log_filepath}")
        
        # Execute command and capture logs
        result = subprocess.run(cmd, capture_output=True, text=True)
        
        if result.returncode != 0:
            raise E2ETestError(f"Failed to get logs from {pod_name}: {result.stderr}")
        
        # Write logs to file
        with open(log_filepath, 'w') as f:
            f.write(f"# Pod logs from: {pod_name} (namespace: {namespace})\n")
            f.write(f"# Retrieved at: {datetime.now(timezone.utc).isoformat()}\n")
            if tail_lines:
                f.write(f"# Tail lines: {tail_lines}\n")
            if since_time:
                f.write(f"# Since time: {since_time}\n")
            f.write(f"# Command: {' '.join(cmd)}\n")
            f.write("# " + "="*60 + "\n\n")
            f.write(result.stdout)
            
        print(f"✅ Saved {len(result.stdout.splitlines())} log lines to {log_filepath}")
        return log_filepath
        
    except Exception as e:
        raise E2ETestError(f"Failed to get logs from pod {pod_name}: {e}")


def read_log_file(log_filepath: str) -> str:
    """
    Read log content from a saved log file.
    
    Args:
        log_filepath: Path to the log file
        
    Returns:
        Log content as string (without header comments)
    """
    try:
        with open(log_filepath, 'r') as f:
            lines = f.readlines()
            
        # Skip header lines starting with '#'
        content_lines = []
        for line in lines:
            if not line.startswith('#'):
                content_lines.append(line)
                
        return ''.join(content_lines)
        
    except Exception as e:
        raise E2ETestError(f"Failed to read log file {log_filepath}: {e}")


def detect_failover_in_log_file(log_filepath: str) -> Dict[str, Any]:
    """
    Detect failover events in a saved controller log file.
    
    Args:
        log_filepath: Path to the controller log file
        
    Returns:
        dict with failover detection info
    """
    try:
        log_content = read_log_file(log_filepath)
        return detect_failover_in_controller_logs(log_content)
    except Exception as e:
        raise E2ETestError(f"Failed to detect failover in log file {log_filepath}: {e}")


def get_controller_logs(tail_lines: int = 50) -> str:
    """Get recent logs from memgraph-controller pod"""
    try:
        controller_pod = get_controller_pod()
        cmd = ["kubectl", "logs", controller_pod, "-n", MEMGRAPH_NS, f"--tail={tail_lines}"]
        result = subprocess.run(cmd, capture_output=True, text=True)
        
        if result.returncode != 0:
            raise E2ETestError(f"Failed to get controller logs: {result.stderr}")
        
        return result.stdout
    except Exception as e:
        raise E2ETestError(f"Failed to get controller logs: {e}")


def get_controller_logs_since(since_time: str) -> str:
    """Get controller logs since specified time"""
    try:
        controller_pod = get_controller_pod()
        cmd = ["kubectl", "logs", controller_pod, "-n", MEMGRAPH_NS, f"--since-time={since_time}"]
        result = subprocess.run(cmd, capture_output=True, text=True)
        
        if result.returncode != 0:
            raise E2ETestError(f"Failed to get controller logs since {since_time}: {result.stderr}")
        
        return result.stdout
    except Exception as e:
        raise E2ETestError(f"Failed to get controller logs since {since_time}: {e}")


def detect_failover_in_controller_logs(logs: str) -> Dict[str, Any]:
    """
    Detect failover events in controller logs
    
    Returns:
        dict with failover detection info
    """
    result = {
        "failover_triggered": False,
        "main_promotion_detected": False,
        "failover_events": []
    }
    
    # Look for failover-related log messages
    failover_patterns = [
        "promoting replica to main",
        "promoting sync replica to main",
        "health prober: failure threshold reached",
        "health prober triggering failover",
        "main pod failed",
        "updating replication topology", 
        "cluster failover",
        "main role changed",
        "reconciling failover",
        "setting main role"
    ]
    
    lines = logs.strip().split('\n')
    for line in lines:
        line_lower = line.lower()
        for pattern in failover_patterns:
            if pattern in line_lower:
                result["failover_events"].append(line.strip())
                if "promoting" in line_lower or "main role" in line_lower or "setting main" in line_lower:
                    result["main_promotion_detected"] = True
                result["failover_triggered"] = True
    
    return result


def monitor_main_pod_changes(initial_main: str, timeout: int = 30) -> Dict[str, Any]:
    """
    Monitor for main pod role changes by polling Memgraph directly
    
    Args:
        initial_main: The original main pod name
        timeout: Maximum seconds to monitor
        
    Returns:
        dict with change detection info
    """
    import time
    start_time = time.time()
    
    result = {
        "main_changed": False,
        "new_main": None,
        "change_time": None,
        "polling_count": 0
    }
    
    while time.time() - start_time < timeout:
        try:
            current_main = find_main_pod_by_querying()
            result["polling_count"] += 1
            
            if current_main != initial_main:
                result["main_changed"] = True
                result["new_main"] = current_main
                result["change_time"] = time.time() - start_time
                break
                
        except Exception:
            # Continue polling even if individual queries fail
            pass
            
        time.sleep(1)  # Poll every second
    
    return result


def get_pod_age_seconds(pod_name: str) -> int:
    """Get pod age in seconds"""
    try:
        result = subprocess.run([
            "kubectl", "get", "pod", pod_name, "-n", MEMGRAPH_NS,
            "-o", "jsonpath={.metadata.creationTimestamp}"
        ], capture_output=True, text=True, check=True)
        
        from datetime import datetime
        creation_time = datetime.fromisoformat(result.stdout.strip().replace('Z', '+00:00'))
        current_time = datetime.now(creation_time.tzinfo)
        age_seconds = int((current_time - creation_time).total_seconds())
        return age_seconds
    except Exception:
        return -1


def monitor_main_pod_changes_enhanced(initial_main: str, timeout: int = 30) -> Dict[str, Any]:
    """
    Enhanced monitoring for main pod role changes that also detects pod recreation.
    
    Args:
        initial_main: The original main pod name
        timeout: Maximum seconds to monitor
        
    Returns:
        dict with enhanced change detection info
    """
    import time
    start_time = time.time()
    
    # Get initial pod age to detect recreation
    initial_age = get_pod_age_seconds(initial_main)
    
    result = {
        "main_changed": False,
        "new_main": None,
        "change_time": None,
        "polling_count": 0,
        "pod_recreated": False,
        "recreation_detected_at": None,
        "initial_pod_age": initial_age
    }
    
    while time.time() - start_time < timeout:
        try:
            current_main = find_main_pod_by_querying()
            result["polling_count"] += 1
            
            # Check if main role moved to different pod
            if current_main != initial_main:
                result["main_changed"] = True
                result["new_main"] = current_main
                result["change_time"] = time.time() - start_time
                break
            
            # Check if the same-named pod was recreated (StatefulSet behavior)
            current_age = get_pod_age_seconds(initial_main)
            if current_age >= 0 and initial_age >= 0 and current_age < (initial_age - 10):  # 10s buffer
                result["pod_recreated"] = True
                result["recreation_detected_at"] = time.time() - start_time
                # In StatefulSet, recreation often means failover occurred then returned
                result["main_changed"] = True
                result["new_main"] = current_main  # Same name, but new pod
                result["change_time"] = time.time() - start_time
                break
                
        except Exception:
            # Continue polling even if individual queries fail
            pass
            
        time.sleep(1)  # Poll every second
    
    return result


class KubectlError(E2ETestError):
    """kubectl command failed"""
    pass


class MemgraphQueryError(E2ETestError):
    """Memgraph query failed"""
    pass


def log_info(message: str) -> None:
    """Log info message with timestamp"""
    timestamp = time.strftime("%Y-%m-%dT%H:%M:%S")
    print(f"\033[0;34m[INFO]\033[0m {timestamp} {message}")


def log_success(message: str) -> None:
    """Log success message with timestamp"""
    timestamp = time.strftime("%Y-%m-%dT%H:%M:%S")
    print(f"\033[0;32m[SUCCESS]\033[0m {timestamp} {message}")


def log_error(message: str) -> None:
    """Log error message with timestamp"""
    timestamp = time.strftime("%Y-%m-%dT%H:%M:%S")
    print(f"\033[0;31m[ERROR]\033[0m {timestamp} {message}", file=sys.stderr)


def kubectl_exec(pod: str, namespace: str, command: List[str], container: str = None) -> Tuple[str, str, int]:
    """
    Execute command in pod via kubectl exec
    
    Args:
        pod: Pod name
        namespace: Kubernetes namespace
        command: Command to execute
        container: Optional container name
    
    Returns:
        Tuple of (stdout, stderr, return_code)
    """
    cmd = ["kubectl", "exec", pod, "-n", namespace]
    if container:
        cmd.extend(["-c", container])
    cmd.extend(["--"] + command)
    result = subprocess.run(cmd, capture_output=True, text=True)
    return result.stdout, result.stderr, result.returncode


def kubectl_get(resource: str, namespace: str = None, selector: str = None, 
                output: str = None) -> str:
    """
    Execute kubectl get command
    
    Returns:
        stdout of kubectl command
    """
    cmd = ["kubectl", "get", resource]
    if namespace:
        cmd.extend(["-n", namespace])
    if selector:
        cmd.extend(["-l", selector])
    if output:
        cmd.extend(["-o", output])
    
    result = subprocess.run(cmd, capture_output=True, text=True)
    if result.returncode != 0:
        raise KubectlError(f"kubectl get failed: {result.stderr}")
    
    return result.stdout.strip()


def kubectl_apply_yaml(yaml_content: str, namespace: str = None) -> None:
    """
    Apply YAML content using kubectl apply
    
    Args:
        yaml_content: The YAML content to apply
        namespace: Optional namespace (if not specified in YAML)
    """
    cmd = ["kubectl", "apply", "-f", "-"]
    if namespace:
        cmd.extend(["-n", namespace])
    
    result = subprocess.run(cmd, input=yaml_content, text=True, capture_output=True)
    if result.returncode != 0:
        raise KubectlError(f"kubectl apply failed: {result.stderr}")


def kubectl_delete_resource(resource_type: str, name: str, namespace: str = None) -> bool:
    """
    Delete a Kubernetes resource using kubectl delete
    
    Args:
        resource_type: Type of resource (e.g., 'networkpolicy', 'pod')
        name: Name of the resource to delete
        namespace: Optional namespace
        
    Returns:
        True if deleted successfully or already gone, False on error
    """
    cmd = ["kubectl", "delete", resource_type, name]
    if namespace:
        cmd.extend(["-n", namespace])
    
    result = subprocess.run(cmd, capture_output=True, text=True)
    
    if result.returncode == 0:
        return True
    elif "not found" in result.stderr:
        return True  # Already deleted
    else:
        print(f"⚠️  Warning: Failed to delete {resource_type} {name}: {result.stderr}")
        return False


def get_test_client_pod() -> str:
    """Get the name of the test client pod"""
    try:
        pod_name = kubectl_get(
            "pods", 
            namespace=MEMGRAPH_NS,
            selector=TEST_CLIENT_LABEL,
            output="jsonpath={.items[0].metadata.name}"
        )
        if not pod_name:
            raise E2ETestError("Test client pod not found")
        return pod_name
    except KubectlError as e:
        raise E2ETestError(f"Failed to get test client pod: {e}")


def memgraph_query_via_client(query: str) -> Dict[str, Any]:
    """
    Execute Cypher query via test client pod (goes through gateway/service)
    
    Args:
        query: Cypher query string
        
    Returns:
        JSON response from Memgraph
    """
    pod = get_test_client_pod()
    stdout, stderr, code = kubectl_exec(pod, MEMGRAPH_NS, ["python", "client.py", query])
    
    if code != 0:
        raise MemgraphQueryError(f"Query via client failed: {stderr}")
    
    try:
        result = json.loads(stdout)
        # If result is a list, wrap in records format for consistency
        if isinstance(result, list):
            return {"records": result}
        return result
    except json.JSONDecodeError as e:
        raise MemgraphQueryError(f"Invalid JSON response: {e}\nResponse: {stdout}")


def memgraph_query_direct(pod: str, query: str) -> str:
    """
    Execute Cypher query directly on a specific memgraph pod using mgconsole
    
    Args:
        pod: Name of the memgraph pod
        query: Cypher query string
        
    Returns:
        Raw CSV output from mgconsole
    """
    command = ["bash", "-c", f"echo '{query}' | mgconsole --output-format csv --username=memgraph"]
    stdout, stderr, code = kubectl_exec(pod, MEMGRAPH_NS, command, container="memgraph")
    
    if code != 0:
        raise MemgraphQueryError(f"Direct query to {pod} failed: {stderr}")
    
    return stdout.strip()




def get_memgraph_pods() -> List[Dict[str, str]]:
    """Get list of memgraph pods with their status"""
    try:
        cmd = ["kubectl", "get", "pods", "-n", MEMGRAPH_NS, "-l", "app.kubernetes.io/name=memgraph", "-o", "json"]
        result = subprocess.run(cmd, capture_output=True, text=True)
        
        if result.returncode != 0:
            raise KubectlError(f"kubectl get failed: {result.stderr}")
        
        pods_data = json.loads(result.stdout)
        pods = []
        
        for item in pods_data.get('items', []):
            name = item['metadata']['name']
            # Check if container is ready
            ready = False
            container_statuses = item.get('status', {}).get('containerStatuses', [])
            if container_statuses and container_statuses[0].get('ready'):
                ready = True
            
            ip = item.get('status', {}).get('podIP', 'unknown')
            
            pods.append({
                'name': name,
                'ready': ready,
                'ip': ip
            })
        
        return pods
    except (subprocess.SubprocessError, json.JSONDecodeError, KeyError) as e:
        raise E2ETestError(f"Failed to get memgraph pods: {e}")


def wait_for_cluster_ready(timeout: int = 60) -> bool:
    """
    Wait for memgraph cluster to be ready
    
    Args:
        timeout: Maximum wait time in seconds
        
    Returns:
        True if cluster is ready, raises exception on timeout
    """
    start_time = time.time()
    log_info(f"⏳ Waiting for cluster to be ready (timeout: {timeout}s)...")
    
    while time.time() - start_time < timeout:
        try:
            pods = get_memgraph_pods()
            
            if len(pods) == EXPECTED_POD_COUNT:
                all_ready = all(pod['ready'] for pod in pods)
                if all_ready:
                    log_success(f"✅ All {EXPECTED_POD_COUNT} pods are ready")
                    return True
            
            ready_count = sum(1 for pod in pods if pod['ready'])
            log_info(f"Waiting for pods (current: {ready_count}/{EXPECTED_POD_COUNT} ready)...")
            
        except E2ETestError:
            log_info("Failed to get pods, retrying...")
        
        time.sleep(2)
    
    raise E2ETestError(f"Timeout waiting for cluster to be ready after {timeout}s")


def wait_for_cluster_convergence(timeout: int = 60) -> bool:
    """
    Wait for cluster to converge to main-sync-async topology by checking pods directly
    Uses the same approach as scripts/check.sh
    
    Args:
        timeout: Maximum wait time in seconds
        
    Returns:
        True if converged, raises exception on timeout
    """
    start_time = time.time()
    log_info("⏳ Waiting for cluster convergence...")
    
    while time.time() - start_time < timeout:
        try:
            # Get all memgraph pods using JSON
            cmd = ["kubectl", "get", "pods", "-n", MEMGRAPH_NS, "-l", "app.kubernetes.io/name=memgraph", "-o", "json"]
            result = subprocess.run(cmd, capture_output=True, text=True)
            
            if result.returncode != 0:
                raise KubectlError(f"kubectl get failed: {result.stderr}")
            
            pods_data = json.loads(result.stdout)
            items = pods_data.get('items', [])
            
            if not items:
                elapsed = int(time.time() - start_time)
                log_info(f"⏳ No pods found yet, waiting... ({elapsed}s/{timeout}s)")
                time.sleep(2)
                continue
            
            # Check pod status and roles
            running_pods = []
            main_pod = None
            
            for item in items:
                pod_name = item['metadata']['name']
                phase = item.get('status', {}).get('phase', 'Unknown')
                
                if phase == 'Running':
                    running_pods.append(pod_name)
                    
                    # Check replication role of this pod
                    try:
                        role_output = memgraph_query_direct(pod_name, "SHOW REPLICATION ROLE;")
                        # Parse CSV output: first line is header, second line has the role
                        lines = role_output.strip().split('\n')
                        if len(lines) >= 2 and '"main"' in lines[1]:
                            main_pod = pod_name
                    except MemgraphQueryError:
                        # Pod might not be ready for queries yet
                        continue
            
            if len(running_pods) != EXPECTED_POD_COUNT:
                elapsed = int(time.time() - start_time)
                log_info(f"⏳ Only {len(running_pods)}/{EXPECTED_POD_COUNT} pods running, waiting... ({elapsed}s/{timeout}s)")
                time.sleep(2)
                continue
            
            if not main_pod:
                elapsed = int(time.time() - start_time)
                log_info(f"⏳ No main pod found yet, waiting... ({elapsed}s/{timeout}s)")
                time.sleep(2)
                continue
            
            # Check replicas from main pod
            try:
                replicas_output = memgraph_query_direct(main_pod, "SHOW REPLICAS;")
                lines = replicas_output.strip().split('\n')
                
                if len(lines) < 2:  # No replicas registered yet
                    elapsed = int(time.time() - start_time)
                    log_info(f"⏳ No replicas registered on main pod {main_pod}, waiting... ({elapsed}s/{timeout}s)")
                    time.sleep(2)
                    continue
                
                # Count sync and async replicas from CSV output
                sync_count = 0
                async_count = 0
                for line in lines[1:]:  # Skip header
                    if '"sync"' in line:
                        sync_count += 1
                    elif '"async"' in line:
                        async_count += 1
                
                if sync_count == 1 and async_count == 1:
                    elapsed = int(time.time() - start_time)
                    log_info(f"✅ Cluster converged after {elapsed}s: main={main_pod}, 1 sync + 1 async replica")
                    return True
                else:
                    elapsed = int(time.time() - start_time)
                    log_info(f"⏳ Waiting for proper topology... main={main_pod}, sync={sync_count}, async={async_count} ({elapsed}s/{timeout}s)")
                    
            except MemgraphQueryError as e:
                elapsed = int(time.time() - start_time)
                log_info(f"⏳ Cannot query replicas on {main_pod}: {str(e)[:50]}, waiting... ({elapsed}s/{timeout}s)")
            
        except (KubectlError, E2ETestError, json.JSONDecodeError, subprocess.SubprocessError) as e:
            elapsed = int(time.time() - start_time)
            log_info(f"⏳ Cluster not ready: {str(e)[:50]}, waiting... ({elapsed}s/{timeout}s)")
        
        time.sleep(2)
    
    raise E2ETestError(f"Cluster failed to converge within {timeout}s")


def write_test_data(test_id: str, test_value: str) -> bool:
    """Write test data to memgraph via client"""
    try:
        query = f"CREATE (n:TestNode {{id: '{test_id}', value: '{test_value}', timestamp: datetime()}}) RETURN n.id;"
        memgraph_query_via_client(query)
        return True
    except MemgraphQueryError:
        return False


def read_test_data(test_id: str) -> Optional[Dict[str, Any]]:
    """Read test data from memgraph via client"""
    try:
        query = f"MATCH (n:TestNode {{id: '{test_id}'}}) RETURN n.id, n.value;"
        result = memgraph_query_via_client(query)
        if result['records']:
            return result['records'][0]
        return None
    except MemgraphQueryError:
        return None


def count_nodes() -> int:
    """Count total nodes in memgraph via client"""
    try:
        query = "MATCH (n) RETURN count(n) as node_count;"
        result = memgraph_query_via_client(query)
        
        # Handle different count response formats
        count_data = result['records'][0]['node_count']
        if isinstance(count_data, dict) and 'low' in count_data:
            return count_data['low']
        return int(count_data)
    except (MemgraphQueryError, KeyError, ValueError, TypeError):
        raise E2ETestError("Failed to count nodes")


def generate_test_id(prefix: str = "test") -> str:
    """Generate unique test ID with timestamp"""
    import os
    timestamp = int(time.time())
    pid = os.getpid()
    return f"{prefix}_{timestamp}_{pid}"




def get_pod_replication_role(pod: str) -> str:
    """
    Get replication role of a specific memgraph pod
    
    Args:
        pod: Name of the memgraph pod
        
    Returns:
        Replication role (main, replica, etc.)
    """
    try:
        result = memgraph_query_direct(pod, "SHOW REPLICATION ROLE;")
        # Parse CSV output to extract role
        lines = result.strip().split('\n')
        if len(lines) >= 2:  # Header + data
            # Extract role from CSV (usually second column in quotes)
            data_line = lines[1]
            if '"main"' in data_line:
                return "main"
            elif '"replica"' in data_line:
                return "replica"
        
        # Fallback: look for role keywords in output
        if '"main"' in result:
            return "main"
        elif '"replica"' in result:
            return "replica"
        
        return "unknown"
        
    except MemgraphQueryError as e:
        raise E2ETestError(f"Failed to get replication role for {pod}: {e}")




def get_test_client_logs(tail_lines: int = 50) -> str:
    """Get recent logs from test client pod"""
    try:
        pod = get_test_client_pod()
        cmd = ["kubectl", "logs", pod, "-n", MEMGRAPH_NS, f"--tail={tail_lines}"]
        result = subprocess.run(cmd, capture_output=True, text=True)
        
        if result.returncode != 0:
            raise KubectlError(f"Failed to get logs: {result.stderr}")
        
        return result.stdout
    except Exception as e:
        raise E2ETestError(f"Failed to get test client logs: {e}")


def get_test_client_logs_since(since_time: str) -> str:
    """
    Get test client logs since a specific time
    
    Args:
        since_time: Time in RFC3339 format (e.g., "2025-09-07T11:07:00Z")
        
    Returns:
        Log output as string
    """
    try:
        pod = get_test_client_pod()
        cmd = ["kubectl", "logs", pod, "-n", MEMGRAPH_NS, f"--since-time={since_time}"]
        result = subprocess.run(cmd, capture_output=True, text=True)
        
        if result.returncode != 0:
            raise KubectlError(f"Failed to get logs since {since_time}: {result.stderr}")
        
        return result.stdout
    except Exception as e:
        raise E2ETestError(f"Failed to get test client logs since {since_time}: {e}")


def count_log_patterns(logs: str, success_pattern: str = "✓ Success", 
                      failure_pattern: str = "✗ Failed") -> Dict[str, int]:
    """Count success and failure patterns in logs"""
    success_count = logs.count(success_pattern)
    failure_count = logs.count(failure_pattern)
    
    return {
        'success': success_count,
        'failure': failure_count
    }


def find_main_pod_by_querying() -> str:
    """Find main pod by querying each pod directly for replication role"""
    try:
        # Get all memgraph pods
        cmd = ["kubectl", "get", "pods", "-n", MEMGRAPH_NS, "-l", "app.kubernetes.io/name=memgraph", "-o", "json"]
        result = subprocess.run(cmd, capture_output=True, text=True)
        
        if result.returncode != 0:
            raise KubectlError(f"kubectl get failed: {result.stderr}")
        
        pods_data = json.loads(result.stdout)
        
        for item in pods_data.get('items', []):
            pod_name = item['metadata']['name']
            phase = item.get('status', {}).get('phase', 'Unknown')
            
            if phase == 'Running':
                try:
                    # Query this pod directly for replication role
                    role_output = memgraph_query_direct(pod_name, "SHOW REPLICATION ROLE;")
                    lines = role_output.strip().split('\n')
                    if len(lines) >= 2 and '"main"' in lines[1]:
                        return pod_name
                except MemgraphQueryError:
                    # Pod might not be ready for queries yet
                    continue
        
        raise E2ETestError("No main pod found")
        
    except (json.JSONDecodeError, subprocess.SubprocessError) as e:
        raise E2ETestError(f"Failed to find main pod: {e}")


def delete_pod_forcefully(pod_name: str) -> None:
    """Delete a pod forcefully with no grace period"""
    try:
        cmd = ["kubectl", "delete", "pod", pod_name, "-n", MEMGRAPH_NS, "--force", "--grace-period=0"]
        result = subprocess.run(cmd, capture_output=True, text=True)
        
        if result.returncode != 0:
            raise KubectlError(f"Failed to delete pod: {result.stderr}")
            
        log_info(f"💥 Deleted pod {pod_name}")
        
    except subprocess.SubprocessError as e:
        raise E2ETestError(f"Failed to delete pod {pod_name}: {e}")


def wait_for_failover_recovery(timeout: int = 30) -> bool:
    """
    Wait for failover recovery by monitoring test client logs
    
    Returns True if recovery detected, False if timeout
    """
    start_time = time.time()
    log_info("⏳ Waiting for failover recovery...")
    
    # Get baseline logs before waiting
    baseline_logs = get_test_client_logs(tail_lines=20)
    baseline_patterns = count_log_patterns(baseline_logs)
    
    while time.time() - start_time < timeout:
        try:
            # Wait a bit for new activity
            time.sleep(3)
            
            # Get recent logs 
            current_logs = get_test_client_logs(tail_lines=10)
            
            # Look for success pattern in recent logs
            if "✓ Success" in current_logs:
                elapsed = int(time.time() - start_time)
                log_info(f"✅ Recovery detected after {elapsed}s")
                return True
                
            elapsed = int(time.time() - start_time)
            log_info(f"⏳ Still waiting for recovery... ({elapsed}s/{timeout}s)")
            
        except E2ETestError:
            # Continue waiting even if log retrieval fails
            pass
    
    log_info(f"❌ No recovery detected within {timeout}s")
    return False


def get_statefulset_status(name: str = "memgraph-ha") -> Dict[str, Any]:
    """Get StatefulSet status information"""
    try:
        cmd = ["kubectl", "get", "statefulset", name, "-n", MEMGRAPH_NS, "-o", "json"]
        result = subprocess.run(cmd, capture_output=True, text=True)
        
        if result.returncode != 0:
            raise KubectlError(f"Failed to get StatefulSet: {result.stderr}")
        
        sts_data = json.loads(result.stdout)
        status = sts_data.get('status', {})
        
        return {
            'replicas': status.get('replicas', 0),
            'ready_replicas': status.get('readyReplicas', 0),
            'updated_replicas': status.get('updatedReplicas', 0),
            'current_replicas': status.get('currentReplicas', 0),
            'generation': sts_data.get('metadata', {}).get('generation', 0),
            'observed_generation': status.get('observedGeneration', 0)
        }
        
    except (json.JSONDecodeError, subprocess.SubprocessError) as e:
        raise E2ETestError(f"Failed to get StatefulSet status: {e}")


def trigger_rolling_restart(name: str = "memgraph-ha") -> int:
    """
    Trigger rolling restart of StatefulSet
    
    Returns the new generation number
    """
    try:
        # Get current generation
        current_status = get_statefulset_status(name)
        initial_generation = current_status['generation']
        
        # Trigger rolling restart
        cmd = ["kubectl", "rollout", "restart", f"statefulset/{name}", "-n", MEMGRAPH_NS]
        result = subprocess.run(cmd, capture_output=True, text=True)
        
        if result.returncode != 0:
            raise KubectlError(f"Failed to trigger rolling restart: {result.stderr}")
        
        log_info(f"🔄 Triggered rolling restart for {name}")
        
        # Wait for generation to change
        max_wait = 30
        elapsed = 0
        while elapsed < max_wait:
            time.sleep(2)
            elapsed += 2
            
            new_status = get_statefulset_status(name)
            new_generation = new_status['generation']
            
            if new_generation > initial_generation:
                log_info(f"✅ Rolling restart started (generation: {initial_generation} → {new_generation})")
                return new_generation
        
        raise E2ETestError(f"Rolling restart did not start within {max_wait}s")
        
    except subprocess.SubprocessError as e:
        raise E2ETestError(f"Failed to trigger rolling restart: {e}")


def wait_for_rolling_restart_complete(name: str = "memgraph-ha", timeout: int = 180) -> bool:
    """
    Wait for rolling restart to complete
    
    Args:
        name: StatefulSet name
        timeout: Maximum wait time in seconds
        
    Returns:
        True if completed successfully
    """
    start_time = time.time()
    log_info(f"⏳ Waiting for rolling restart of {name} to complete...")
    
    while time.time() - start_time < timeout:
        try:
            status = get_statefulset_status(name)
            
            replicas = status['replicas']
            ready_replicas = status['ready_replicas']
            updated_replicas = status['updated_replicas']
            
            if ready_replicas == replicas and updated_replicas == replicas:
                elapsed = int(time.time() - start_time)
                log_info(f"✅ Rolling restart completed after {elapsed}s: all {replicas} replicas ready and updated")
                return True
            
            elapsed = int(time.time() - start_time)
            log_info(f"⏳ Rolling restart in progress: ready={ready_replicas}/{replicas}, updated={updated_replicas}/{replicas} ({elapsed}s/{timeout}s)")
            
        except E2ETestError:
            # Continue waiting even if status check fails
            pass
        
        time.sleep(5)
    
    raise E2ETestError(f"Rolling restart did not complete within {timeout}s")


def check_prerequisites() -> None:
    """Check that required tools are available"""
    required_tools = ["kubectl", "jq"]
    
    for tool in required_tools:
        try:
            subprocess.run([tool, "--version"], capture_output=True, check=True)
        except (subprocess.CalledProcessError, FileNotFoundError):
            raise E2ETestError(f"{tool} is required but not available")
    
    # Check kubernetes connectivity
    try:
        subprocess.run(["kubectl", "cluster-info"], capture_output=True, check=True)
    except subprocess.CalledProcessError:
        raise E2ETestError("Cannot connect to Kubernetes cluster")
    
    # Check namespace exists
    try:
        kubectl_get("namespace", output=f"jsonpath='{{.metadata.name}}'")
        if MEMGRAPH_NS not in kubectl_get("namespaces", output="jsonpath='{.items[*].metadata.name}'"):
            raise E2ETestError(f"Namespace '{MEMGRAPH_NS}' does not exist")
    except KubectlError:
        raise E2ETestError(f"Cannot access namespace '{MEMGRAPH_NS}'")
    
    log_info("✅ Prerequisites checked")


def parse_logfmt_line(line: str) -> Dict[str, str]:
    """
    Parse a single logfmt line into key-value pairs.
    
    Example:
        'at=INFO msg="Starting client" uri=bolt://localhost:7687'
        -> {'at': 'INFO', 'msg': 'Starting client', 'uri': 'bolt://localhost:7687'}
    """
    pairs = {}
    
    # Regex to match key=value pairs, handling quoted values
    pattern = r'(\w+)=(?:"([^"]*)"|([^\s]+))'
    
    for match in re.finditer(pattern, line):
        key = match.group(1)
        # Use quoted value if present, otherwise unquoted value
        value = match.group(2) if match.group(2) is not None else match.group(3)
        pairs[key] = value
    
    return pairs


def get_client_logs(pod_name: Optional[str] = None, tail: int = 100) -> List[str]:
    """
    Get logs from test-client pod.
    
    Args:
        pod_name: Specific pod name, or None to auto-discover
        tail: Number of recent lines to retrieve
    
    Returns:
        List of log lines
    """
    if pod_name is None:
        # Auto-discover test-client pod
        try:
            result = subprocess.run([
                "kubectl", "get", "pods", "-n", MEMGRAPH_NS,
                "-l", TEST_CLIENT_LABEL,
                "-o", "jsonpath={.items[0].metadata.name}"
            ], capture_output=True, text=True, check=True)
            pod_name = result.stdout.strip()
        except subprocess.CalledProcessError as e:
            raise E2ETestError(f"Failed to find test-client pod: {e}")
    
    if not pod_name:
        raise E2ETestError("No test-client pod found")
    
    try:
        result = subprocess.run([
            "kubectl", "logs", "-n", MEMGRAPH_NS,
            pod_name, f"--tail={tail}"
        ], capture_output=True, text=True, check=True)
        
        return result.stdout.strip().split('\n') if result.stdout.strip() else []
        
    except subprocess.CalledProcessError as e:
        raise E2ETestError(f"Failed to get logs from pod {pod_name}: {e}")


def parse_client_logs(pod_name: Optional[str] = None, filter_level: Optional[str] = None) -> List[Dict[str, str]]:
    """
    Parse test-client logs and return structured data.
    
    Args:
        pod_name: Specific pod name, or None to auto-discover
        filter_level: Only return logs of this level (INFO, ERROR, etc.)
    
    Returns:
        List of parsed log entries as dictionaries
    """
    logs = get_client_logs(pod_name)
    parsed_logs = []
    
    for line in logs:
        if line.strip():
            try:
                parsed = parse_logfmt_line(line)
                
                # Apply level filter if specified
                if filter_level and parsed.get('at', '').upper() != filter_level.upper():
                    continue
                    
                parsed_logs.append(parsed)
            except Exception as e:
                # Skip lines that can't be parsed (might be startup logs, etc.)
                continue
    
    return parsed_logs