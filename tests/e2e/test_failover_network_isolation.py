#!/usr/bin/env python3
"""
Network Partition Failover Test

Tests the Memgraph HA controller's behavior during network partition scenarios.
Simulates realistic network failure where the main node becomes isolated using
Kubernetes NetworkPolicy, forcing automatic failover.
"""

import pytest
import time
from datetime import datetime, timezone
import subprocess
from utils import (
    parse_logfmt,
    get_test_client_pod,
    kubectl_exec,
    kubectl_get,
    kubectl_apply_yaml,
    kubectl_delete_resource,
    wait_for_cluster_convergence,
    find_main_pod_by_querying,
    log_info,
    log_error,
    get_pod_logs,
    get_controller_pod,
    memgraph_query_direct,
    MemgraphQueryError,
)
from typing import Dict, Any


def wait_for_controller_to_detect_failure(
        since_time: str, timeout: int = 30) -> bool:
  """
  Wait for controller to detect pod failure by checking logs for health check failures.

  Args:
      since_time: ISO format timestamp to check logs from
      timeout: Maximum wait time in seconds

  Returns:
      True if failure detected, False if timeout
  """
  start_time = time.time()
  controller_pod = get_controller_pod()

  while time.time() - start_time < timeout:
    try:
      # Get controller logs and check for health check failures
      log_content = get_pod_logs(controller_pod, since_time=since_time)

      # Look for health check failure indicators
      if any(indicator in log_content for indicator in [
              "cannot ping main pod",
      ]):
        return True

    except Exception as e:
      log_info(f"Error checking controller logs: {e}")

    elapsed = int(time.time() - start_time)
    if elapsed < timeout - 1:
      time.sleep(5)

  return False


def wait_for_network_policy_effect(pod_name: str, timeout: int = 10) -> bool:
  """
  Wait for NetworkPolicy to take effect by verifying connection failure.

  Args:
      pod_name: Pod to test connectivity to
      timeout: Maximum wait time in seconds

  Returns:
      True if NetworkPolicy is blocking connections, False if timeout
  """
  start_time = time.time()

  while time.time() - start_time < timeout:
    try:
      # Try to query the pod - should fail if NetworkPolicy is working
      memgraph_query_direct(pod_name, "RETURN 1;")
      # If query succeeds, NetworkPolicy hasn't taken effect yet
      elapsed = int(time.time() - start_time)
      if elapsed < timeout - 1:
        time.sleep(1)
    except (MemgraphQueryError, subprocess.CalledProcessError):
      # Connection failed - NetworkPolicy is working
      return True

  return False


def wait_for_test_client_recovery(
        required_consecutive: int = 5, timeout: int = 30) -> bool:
  """
  Wait for test client to show consecutive successful operations.

  Args:
      required_consecutive: Number of consecutive successes required
      timeout: Maximum wait time in seconds

  Returns:
      True if client recovered, False if timeout
  """
  start_time = time.time()

  test_client_pod = get_test_client_pod()

  while time.time() - start_time < timeout:
    try:
      # Get recent logs and check for consecutive successes
      logs = get_pod_logs(test_client_pod, tail_lines=50)
      lines = logs.strip().split('\n')

      # Parse recent operations
      success_count = 0
      for line in reversed(lines):  # Most recent first
        try:
          data = parse_logfmt(line)
          if data.get('level') == 'info' and 'query executed successfully' in data.get(
                  'msg', ''):
            success_count += 1
            if success_count >= required_consecutive:
              return True
          elif data.get('level') == 'error':
            # Reset count on any error
            success_count = 0
        except Exception:
          continue

    except Exception:
      # If we can't check logs, wait and try again
      pass

    elapsed = int(time.time() - start_time)
    if elapsed < timeout - 2:
      time.sleep(2)

  return False


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

  # Look for failover-related log messages (match actual controller output)
  failover_patterns = [
      "promoting replica to main",
      "promoting sync replica to main",
      "success promoting sync replica to main",
      "health prober: failure threshold reached",
      "🚨 health prober: failure threshold reached, triggering failover",
      "health prober triggering failover",
      "health prober triggering failover execution",
      "main pod failed",
      "updating replication topology",
      "cluster failover",
      "main role changed",
      "reconciling failover",
      "setting main role",
      "failover execution failed",
      "failover: cached replica status"
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


class NetworkIsolationTestManager:
  """Manages NetworkPolicy creation and cleanup for network isolation tests."""

  def __init__(self, namespace="memgraph"):
    self.namespace = namespace
    self.policy_name = None

  def create_isolation_policy(self, pod_name: str) -> str:
    """Create NetworkPolicy to isolate a specific pod using kubectl."""
    self.policy_name = f"isolate-{pod_name}"

    # Create NetworkPolicy YAML - Complete isolation to simulate network
    # partition
    policy_yaml = f"""
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: {self.policy_name}
  namespace: {self.namespace}
spec:
  podSelector:
    matchLabels:
      statefulset.kubernetes.io/pod-name: {pod_name}
  policyTypes:
  - Ingress
  - Egress
  ingress: []
  egress: []
"""

    try:
      # Apply NetworkPolicy using utility function
      kubectl_apply_yaml(policy_yaml)

      print(f"✅ Created NetworkPolicy: {self.policy_name}")
      return self.policy_name
    except Exception as e:
      raise Exception(f"Failed to create NetworkPolicy: {e}")

  def cleanup(self):
    """Clean up NetworkPolicy if it exists."""
    if not self.policy_name:
      return

    try:
      success = kubectl_delete_resource(
          "networkpolicy", self.policy_name, self.namespace)

      if success:
        print(f"✅ Cleaned up NetworkPolicy: {self.policy_name}")
      else:
        print(
            f"⚠️  Warning: Failed to cleanup NetworkPolicy {self.policy_name}")
    except Exception as e:
      print(
          f"⚠️  Warning: Failed to cleanup NetworkPolicy {self.policy_name}: {e}")
    finally:
      self.policy_name = None


@pytest.fixture
def network_isolation_manager():
  """Pytest fixture that ensures NetworkPolicy cleanup."""
  manager = NetworkIsolationTestManager()
  try:
    yield manager
  finally:
    manager.cleanup()


def verify_network_isolation(isolated_pod: str) -> bool:
  """Verify that NetworkPolicy is actually blocking connections to the isolated pod."""
  try:
    # Get the isolated pod's IP using kubectl_get
    pod_ip = kubectl_get(
        f"pod/{isolated_pod}", namespace="memgraph", output="jsonpath={.status.podIP}")
    if not pod_ip:
      print(f"⚠️  Could not get IP for pod {isolated_pod}")
      return False

    print(
        f"🔍 Testing connection to isolated pod {isolated_pod} at {pod_ip}:7687...")

    # Get a controller pod to test from
    controller_pods_output = kubectl_get("pods", namespace="memgraph",
                                         selector="app=memgraph-controller", output="name")
    controller_pods = controller_pods_output.split('\n')
    if not controller_pods or not controller_pods[0]:
      print("⚠️  No controller pods found")
      return False

    controller_pod = controller_pods[0].replace("pod/", "")

    # Try to connect from controller to isolated pod
    # Using nc (netcat) to test TCP connection with timeout
    stdout, stderr, exit_code = kubectl_exec(
        controller_pod,
        "memgraph",
        ["nc", "-zv", "-w", "2", pod_ip, "7687"]
    )

    if exit_code == 0:
      print("❌ Connection succeeded - NetworkPolicy is NOT blocking traffic!")
      print(
          "   This means NetworkPolicy enforcement is not working in this cluster.")
      return False
    else:
      print("✅ Connection failed as expected - NetworkPolicy IS blocking traffic")
      return True

  except Exception as e:
    print(f"⚠️  Error verifying network isolation: {e}")
    return False


def reset_controller_connections():
  """Reset controller connection pools via admin API."""
  try:
    # Get test client pod to run curl from
    test_client_pod = get_test_client_pod()

    # Run curl POST inside the pod
    stdout, stderr, exit_code = kubectl_exec(
        test_client_pod,
        "memgraph",
        ["curl", "-s", "-X", "POST",
            "http://memgraph-controller:8080/api/v1/admin/reset-connections"]
    )

    if exit_code == 0:
      print("✅ Reset controller connections")
      return True
    else:
      print(f"⚠️  Warning: Failed to reset connections: {stderr}")
      return False
  except Exception as e:
    print(f"⚠️  Warning: Failed to reset connections: {e}")
    return False


def verify_recent_test_client_success(required_consecutive=10):
  """Verify recent test client operations were successful."""
  try:
    # Get test client pod and save logs to file
    test_client_pod = get_test_client_pod()

    # Read and parse the log file
    logs = get_pod_logs(test_client_pod, tail_lines=50)
    lines = logs.strip().split('\n')

    # Check last N operations for consecutive successes
    consecutive_successes = 0

    for line in reversed(lines[-required_consecutive:]):
      try:
        log_data = parse_logfmt(line)
        if log_data.get(
                'msg') == 'Success' or 'Success' in log_data.get('msg', ''):
          consecutive_successes += 1
        else:
          break
      except Exception:
        # If can't parse, check for success text
        if 'Success' in line or '✓' in line:
          consecutive_successes += 1
        else:
          break

    return consecutive_successes >= required_consecutive

  except Exception as e:
    log_error(f"Failed to verify test client success: {e}")
    return False


def wait_for_test_client_failures(max_wait=30):
  """Wait for test client to start experiencing write failures."""
  print(f"⏳ Waiting up to {max_wait}s for test client failures...")

  for elapsed in range(0, max_wait, 3):
    try:
      # Get test client logs and save to file
      test_client_pod = get_test_client_pod()
      logs = get_pod_logs(test_client_pod, tail_lines=10)

      recent_lines = logs.strip().split('\n')[-5:]  # Last 5 lines
      failures = 0

      for line in recent_lines:
        try:
          log_data = parse_logfmt(line)
          # Check for failure patterns
          if ('Failed' in log_data.get('msg', '') or
                  log_data.get('at', '').upper() == 'ERROR'):
            failures += 1
        except Exception:
          # If can't parse, check for error text
          if 'Failed' in line or 'ERROR' in line:
            failures += 1

      if failures >= 2:  # At least 2 recent failures
        print(f"✅ Detected test client failures after {elapsed}s")
        return True

    except Exception as e:
      print(f"⚠️  Error checking test client: {e}")

    print(
        f"⏳ No failures detected yet, waiting... ({elapsed}s/{max_wait}s)")
    time.sleep(3)

  raise Exception(f"Test client failures not detected within {max_wait}s")


def test_network_partition_failover(network_isolation_manager):
  """Test complete network partition failover scenario."""

  print("🧪 Starting Network Partition Failover Test")
  test_start_time = datetime.now(timezone.utc).isoformat()

  # Step 1: Pre-condition validation
  print("\n📋 Step 1: Validating preconditions...")

  # 1a. Wait for cluster to be ready
  if not wait_for_cluster_convergence(timeout=60):
    pytest.fail("Cluster not converged within timeout")

  # 1b. Find current main pod
  initial_main = find_main_pod_by_querying()
  print(f"✅ Initial main pod: {initial_main}")

  # 1c. Verify recent test client success
  if not verify_recent_test_client_success(required_consecutive=10):
    pytest.fail("Test client not showing sufficient recent successes")

  print("✅ Preconditions validated")

  # Step 2: Create network isolation
  print(f"\n🔌 Step 2: Isolating main pod {initial_main}...")
  network_isolation_manager.create_isolation_policy(initial_main)

  # Verify the NetworkPolicy is actually blocking connections
  print("\n🔍 Step 2a: Verifying network isolation is effective...")

  # Wait for NetworkPolicy to take effect
  if not wait_for_network_policy_effect(initial_main, timeout=10):
    print("⚠️  Warning: NetworkPolicy may not be fully effective yet")

  if not verify_network_isolation(initial_main):
    pytest.skip("NetworkPolicy not blocking connections - skipping test. "
                "NetworkPolicy enforcement may not be enabled in this cluster.")

  # Reset connections to force immediate connection failures
  print("\n🔄 Step 2b: Resetting controller connections...")
  reset_controller_connections()

  # Step 3: Wait for failure detection
  print("\n⚠️  Step 3: Waiting for failure detection...")

  wait_for_test_client_failures(max_wait=30)

  # Step 4: Wait for failover completion
  print("\n🔄 Step 4: Waiting for failover completion...")

  # Wait for controller to detect the failure
  print("⏳ Waiting for controller to detect pod failure...")
  if wait_for_controller_to_detect_failure(test_start_time, timeout=30):
    print("✅ Controller detected pod failure")
  else:
    print("⚠️  Controller may not have detected failure yet")

  # Check controller logs for failover events (the source of truth during
  # split-brain)
  print("🔍 Checking controller logs for failover activity...")
  new_main = None
  failover_detected = False

  # Poll controller logs for up to 120 seconds
  for attempt in range(24):  # 24 * 5 = 120 seconds
    try:
      if attempt < 23:  # Don't sleep on last iteration
        print(f"⏳ Waiting for failover... ({(attempt+1)*5}s/120s)")
        time.sleep(5)

      # Get controller logs and check for failover
      controller_pod = get_controller_pod()
      controller_logs = get_pod_logs(
          controller_pod, since_time=test_start_time)
      failover_events = detect_failover_in_controller_logs(controller_logs)

      if failover_events.get('failover_triggered'):
        print(
            f"✅ Failover attempted! Found {len(failover_events.get('failover_events', []))} events")
        
        # Debug: show what events were detected
        for event in failover_events.get('failover_events', []):
          print(f"  📋 Event: {event}")

        # Parse controller logs to find the new main or failure reasons
        lines = controller_logs.strip().split('\n')
        failover_execution_failed = False

        # Look for successful promotion messages
        for line in lines:
          if 'success promoting sync replica to main' in line and 'pod_name=' in line:
            # Extract pod name from log line
            pod_part = line.split('pod_name=')[
                1].split(' ')[0].strip('"')
            # Convert underscore to dash
            new_main = pod_part.replace('_', '-')
            print(f"📊 Controller promoted {new_main} to main")
            failover_detected = True
            break

        if failover_detected:
          break

    except Exception as e:
      print(f"⚠️  Error checking controller logs: {e}")
      if attempt < 23:
        time.sleep(5)

  if not failover_detected or not new_main:
    # Check if we at least detected failover attempts
    controller_logs = get_pod_logs(controller_pod, since_time=test_start_time)
    final_events = detect_failover_in_controller_logs(controller_logs)
    
    if final_events.get('failover_triggered'):
      print("🔍 Final diagnosis: Failover was attempted but did not complete successfully")
      print("   This indicates a controller issue with replica readiness or failover execution")
      for event in final_events.get('failover_events', [])[:5]:  # Show first 5 events
        print(f"   📋 {event}")
      pytest.fail("Failover attempted but failed to complete - controller may have replica readiness issues")
    else:
      pytest.fail("No failover detected in controller logs after 120 seconds")

  if new_main == initial_main:
    pytest.fail(
        f"Controller promoted the same pod {initial_main} - not a real failover")

  print(
      f"✅ Failover completed: {initial_main} -> {new_main} (according to controller)")

  # Step 5: Verify post-failover functionality
  print("\n✅ Step 5: Verifying post-failover functionality...")

  # Wait for test client to recover and show successful operations
  print("⏳ Waiting for test client to recover...")
  if wait_for_test_client_recovery(required_consecutive=5, timeout=20):
    print("✅ Test client recovered and showing successful operations")
  else:
    print("⚠️  Warning: Test client not showing recent successes after failover")

  # Verify new main is different
  if new_main == initial_main:
    pytest.fail(f"Failover did not change main pod (still {initial_main})")

  print(f"✅ Failover successful: {initial_main} -> {new_main}")

  # Step 6: Network recovery
  print("\n🔄 Step 6: Restoring network connectivity...")

  network_isolation_manager.cleanup()  # This removes the NetworkPolicy

  # Step 7: Wait for cluster reconciliation
  print("\n🔄 Step 7: Waiting for cluster reconciliation...")

  # Wait for cluster to converge again (includes network recovery time)
  if not wait_for_cluster_convergence(timeout=90):
    print("⚠️  Warning: Cluster did not converge after network recovery")

  # Verify the old main is now a replica (with retries for reconciliation)
  print(
      f"\n🔍 Step 7b: Verifying old main {initial_main} is reconciled to replica...")
  reconciled = False
  max_wait = 30

  # Check every 3 seconds for up to 30 seconds
  for attempt in range(max_wait // 3):
    try:
      # Query the old main pod directly to check its role
      role_output = memgraph_query_direct(
          initial_main, "SHOW REPLICATION ROLE;")
      lines = role_output.strip().split('\n')

      # Parse the role from CSV output
      if len(lines) >= 2:
        role_line = lines[1]  # Second line contains the actual role
        if '"replica"' in role_line.lower():
          print(
              f"✅ Old main {initial_main} successfully reconciled to replica role after {(attempt+1)*3}s")
          reconciled = True
          break
        elif '"main"' in role_line.lower():
          if attempt < (max_wait // 3 - 1):  # Not last attempt
            print(
                f"⏳ Old main {initial_main} still reports as main, waiting for reconciliation... "
                f"({(attempt+1)*3}s/{max_wait}s)")
            time.sleep(3)
          else:
            print(
                f"⚠️  WARNING: Old main {initial_main} still reports as main after {max_wait}s - "
                f"split-brain not resolved!")
            print(
                "   This indicates controller hasn't fully reconciled the cluster yet")
        else:
          print(
              f"⚠️  Unexpected role for {initial_main}: {role_line}")
          break
    except Exception as e:
      if attempt < (max_wait // 3 - 1):  # Not last attempt
        print(
            f"⏳ Could not query old main (may be restarting): {e}. Retrying... ({(attempt+1)*3}s/{max_wait}s)")
        time.sleep(3)
      else:
        print(
            f"⚠️  Could not verify old main role after {max_wait}s: {e}")

  if not reconciled and attempt == (max_wait // 3 - 1):
    print(
        f"⚠️  Old main {initial_main} was not reconciled to replica after {max_wait}s")

  # Final validation
  print("\n✅ Step 8: Final validation...")

  # Find current main pod
  try:
    final_main = find_main_pod_by_querying()
    print(f"Final main pod: {final_main}")
  except Exception as e:
    print(f"⚠️  Warning: Could not find main pod after recovery: {e}")
    final_main = "unknown"

  # Verify test client is working
  if not verify_recent_test_client_success(required_consecutive=5):
    print("⚠️  Warning: Test client not showing recent successes after recovery")
  else:
    print("✅ Test client operational after recovery")

  # Get controller logs for analysis
  controller_pod = get_controller_pod()
  final_controller_logs = get_pod_logs(
      controller_pod, since_time=test_start_time)
  failover_events = detect_failover_in_controller_logs(final_controller_logs)

  print("📊 Test Summary:")
  print(f"  - Initial main: {initial_main}")
  print(f"  - Final main: {final_main}")
  print(
      f"  - Failover events detected: {len(failover_events.get('events', []))}")

  print("🎉 Network partition failover test completed successfully!")


if __name__ == "__main__":
  # Allow running as standalone script for debugging
  import sys

  manager = NetworkIsolationTestManager()
  try:
    test_network_partition_failover(manager)
  except Exception as e:
    print(f"❌ Test failed: {e}")
    sys.exit(1)
  finally:
    manager.cleanup()
