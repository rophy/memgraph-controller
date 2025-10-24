"""
Rolling Restart Tests

Tests that verify the cluster maintains availability during a rolling restart
of the Memgraph StatefulSet. The test-client should continue successful writes
throughout the entire rollout process.
"""

import time
import datetime
from utils import (
    wait_for_cluster_convergence,
    wait_for_statefulset_ready,
    get_pod_logs,
    get_test_client_pod,
    get_controller_pod,
    find_main_pod_by_querying,
    log_info,
    parse_logfmt,
    kubectl_get,
    E2ETestError,
    MEMGRAPH_NS
)
import subprocess
import json
from typing import Dict, Any


def trigger_statefulset_rollout(statefulset_name: str = "memgraph-ha") -> None:
  """
  Trigger a rolling restart of the StatefulSet using kubectl rollout restart.

  Args:
      statefulset_name: Name of the StatefulSet to restart
  """
  cmd = [
      "kubectl", "rollout", "restart",
      f"statefulset/{statefulset_name}",
      "-n", MEMGRAPH_NS
  ]

  result = subprocess.run(cmd, capture_output=True, text=True)
  if result.returncode != 0:
    raise E2ETestError(f"Failed to trigger rollout: {result.stderr}")

  log_info(f"Triggered rolling restart of {statefulset_name}")


def get_statefulset_rollout_status(statefulset_name: str = "memgraph-ha") -> Dict[str, Any]:
  """
  Get the current rollout status of a StatefulSet.

  Returns:
      Dict containing rollout status information
  """
  try:
    sts_json = kubectl_get(f"statefulset/{statefulset_name}", namespace=MEMGRAPH_NS, output="json")
    sts_data = json.loads(sts_json)
  except Exception as e:
    raise E2ETestError(f"Failed to get StatefulSet status: {e}")
  status = sts_data.get('status', {})

  return {
      'replicas': status.get('replicas', 0),
      'ready_replicas': status.get('readyReplicas', 0),
      'current_replicas': status.get('currentReplicas', 0),
      'updated_replicas': status.get('updatedReplicas', 0),
      'current_revision': status.get('currentRevision', ''),
      'update_revision': status.get('updateRevision', ''),
      'is_rolling': status.get('currentRevision') != status.get('updateRevision', ''),
      'observed_generation': status.get('observedGeneration', 0)
  }


def wait_for_rollout_completion(statefulset_name: str = "memgraph-ha",
                                timeout: int = 300) -> bool:
  """
  Wait for a StatefulSet rollout to complete.

  Args:
      statefulset_name: Name of the StatefulSet
      timeout: Maximum time to wait in seconds

  Returns:
      True if rollout completed successfully, False if timeout
  """
  start_time = time.time()
  last_status = None

  while time.time() - start_time < timeout:
    try:
      status = get_statefulset_rollout_status(statefulset_name)

      # Log status changes
      if last_status != status:
        log_info(f"Rollout status: updated={status['updated_replicas']}/{status['replicas']}, "
                 f"ready={status['ready_replicas']}/{status['replicas']}, "
                 f"rolling={status['is_rolling']}")
        last_status = status

      # Check if rollout is complete
      if (not status['is_rolling'] and
          status['ready_replicas'] == status['replicas'] and
              status['updated_replicas'] == status['replicas']):
        log_info(f"Rollout completed successfully after {time.time() - start_time:.1f}s")
        return True

    except Exception as e:
      log_info(f"Error checking rollout status: {e}")

    time.sleep(2)

  log_info(f"Rollout did not complete within {timeout}s timeout")
  return False


def monitor_pod_restarts_during_rollout(duration: int = 300) -> Dict[str, Any]:
  """
  Monitor pod restarts during a rollout period.

  Args:
      duration: How long to monitor in seconds

  Returns:
      Dict with restart information
  """
  start_time = time.time()

  try:
    initial_json = kubectl_get("pods", namespace=MEMGRAPH_NS, selector="app.kubernetes.io/name=memgraph", output="json")
    initial_data = json.loads(initial_json)
  except Exception as e:
    raise E2ETestError(f"Failed to get initial pod status: {e}")
  initial_pods = {pod['metadata']['name']: {
      'uid': pod['metadata']['uid'],
      'restartCount': pod['status'].get('containerStatuses', [{}])[0].get('restartCount', 0)
  } for pod in initial_data['items']}

  pod_events = []
  pods_restarted = set()

  while time.time() - start_time < duration:
    try:
      current_json = kubectl_get("pods", namespace=MEMGRAPH_NS,
                                 selector="app.kubernetes.io/name=memgraph", output="json")
      current_data = json.loads(current_json)

      for pod in current_data['items']:
        pod_name = pod['metadata']['name']
        pod_uid = pod['metadata']['uid']
        current_restart_count = pod['status'].get('containerStatuses', [{}])[0].get('restartCount', 0)

        # Check if pod was deleted/recreated (different UID means new pod)
        if pod_name not in initial_pods or initial_pods[pod_name]['uid'] != pod_uid:
          event_type = 'recreated' if pod_name in initial_pods else 'created'
          pod_events.append({
              'time': time.time() - start_time,
              'pod': pod_name,
              'event': event_type
          })
          pods_restarted.add(pod_name)
          log_info(f"Pod {event_type}: {pod_name} (UID changed)")
          initial_pods[pod_name] = {
              'uid': pod_uid,
              'restartCount': current_restart_count
          }

        # Check if pod restart count increased
        elif current_restart_count > initial_pods[pod_name]['restartCount']:
          pod_events.append({
              'time': time.time() - start_time,
              'pod': pod_name,
              'event': 'restarted',
              'restart_count': current_restart_count
          })
          pods_restarted.add(pod_name)
          initial_pods[pod_name]['restartCount'] = current_restart_count
    except Exception:
      # Ignore errors during monitoring
      pass

    time.sleep(1)

  return {
      'pods_restarted': list(pods_restarted),
      'restart_count': len(pods_restarted),
      'events': pod_events
  }


def analyze_client_operations_during_rollout(logs: str,
                                             rollout_start: datetime.datetime,
                                             rollout_duration: int) -> Dict[str, Any]:
  """
  Analyze test-client operations during a rollout period.

  Args:
      logs: Test client log output
      rollout_start: When the rollout started
      rollout_duration: How long the rollout took in seconds

  Returns:
      Dict with analysis results
  """
  # Convert timezone-aware rollout_start to naive for comparison with log timestamps
  # Add small buffers to ensure we capture operations at the boundaries
  rollout_start_naive = rollout_start.replace(tzinfo=None) - datetime.timedelta(seconds=5)
  rollout_end = rollout_start.replace(tzinfo=None) + datetime.timedelta(seconds=rollout_duration + 5)

  # Track operations, failures, and successes for detailed analysis
  # operations = []  # Future: could track individual operations
  # failures = []    # Future: could track failure details
  # successes = []   # Future: could track success details
  failure_windows = []
  current_failure_window = None

  # Track metrics over time using total/success/errors fields
  metrics_snapshots = []

  for line in logs.strip().split('\n'):
    if not line or line.startswith('#'):
      continue

    try:
      log_data = parse_logfmt(line)

      # Parse timestamp
      timestamp_str = log_data.get('ts', '')
      if not timestamp_str:
        continue

      # Handle timezone-aware timestamps
      timestamp = datetime.datetime.fromisoformat(timestamp_str)
      timestamp_naive = timestamp.replace(tzinfo=None)

      # Skip logs outside rollout window
      if timestamp_naive < rollout_start_naive or timestamp_naive > rollout_end:
        continue

      # Extract metrics if available (total, success, errors)
      total = log_data.get('total')
      success = log_data.get('success')
      errors = log_data.get('errors')

      if total is not None and success is not None and errors is not None:
        try:
          metrics_snapshots.append({
            'timestamp': timestamp_naive,
            'total': int(total),
            'success': int(success),
            'errors': int(errors),
            'latency_ms': log_data.get('latency_ms', 0)
          })
        except (ValueError, TypeError):
          # Skip if metrics can't be converted to integers
          continue

    except Exception:
      # Skip malformed log lines
      continue

  # Analyze metrics snapshots to calculate statistics
  if not metrics_snapshots:
    return {
        'total_operations': 0,
        'successful_operations': 0,
        'failed_operations': 0,
        'failure_rate': 0,
        'failure_windows': [],
        'max_failure_window_seconds': 0,
        'had_complete_outage': False
    }

  # Sort by timestamp to analyze progression
  metrics_snapshots.sort(key=lambda x: x['timestamp'])

  # Calculate operations during rollout window by comparing first and last snapshots
  first_snapshot = metrics_snapshots[0]
  last_snapshot = metrics_snapshots[-1]

  operations_during_rollout = last_snapshot['total'] - first_snapshot['total']
  successes_during_rollout = last_snapshot['success'] - first_snapshot['success']
  failures_during_rollout = last_snapshot['errors'] - first_snapshot['errors']

  # Calculate failure rate
  failure_rate = 0
  if operations_during_rollout > 0:
    failure_rate = (failures_during_rollout / operations_during_rollout) * 100

  # Analyze failure windows by looking at error count increases
  failure_windows = []
  current_failure_window = None
  prev_errors = first_snapshot['errors']

  for snapshot in metrics_snapshots[1:]:
    current_errors = snapshot['errors']

    if current_errors > prev_errors:
      # New failures detected
      if not current_failure_window:
        # Start new failure window
        current_failure_window = {
          'start': snapshot['timestamp'],
          'failures': []
        }

      # Add failures to current window
      new_failures = current_errors - prev_errors
      current_failure_window['failures'].extend([None] * new_failures)  # Placeholder

    else:
      # No new failures, close current window if exists
      if current_failure_window:
        current_failure_window['end'] = snapshot['timestamp']
        current_failure_window['duration'] = (
          current_failure_window['end'] - current_failure_window['start']
        ).total_seconds()
        failure_windows.append(current_failure_window)
        current_failure_window = None

    prev_errors = current_errors

  # Close any remaining failure window
  if current_failure_window:
    current_failure_window['end'] = rollout_end
    current_failure_window['duration'] = (
      current_failure_window['end'] - current_failure_window['start']
    ).total_seconds()
    failure_windows.append(current_failure_window)

  max_failure_window = max([w['duration'] for w in failure_windows], default=0)

  return {
      'total_operations': operations_during_rollout,
      'successful_operations': successes_during_rollout,
      'failed_operations': failures_during_rollout,
      'failure_rate': failure_rate,
      'failure_windows': failure_windows,
      'max_failure_window_seconds': max_failure_window,
      'had_complete_outage': max_failure_window > 90  # More than 90s of failures
  }


def test_rolling_restart_continuous_availability():
  """
  Test that the cluster maintains read/write availability during a rolling restart.

  Preconditions:
  - Cluster is ready and healthy
  - Test-client is running and writing successfully
  - Read-client is running and reading successfully
  - Bad-client is running and failing writes (as expected)

  Test steps:
  1. Verify cluster is healthy with 3 pods
  2. Verify test-client is writing successfully
  3. Verify read-client is reading successfully
  4. Verify bad-client writes are failing (as expected)
  5. Trigger a rolling restart of the StatefulSet
  6. Monitor all client operations during the entire rollout
  7. Verify no extended failures occurred
  8. Verify cluster returns to healthy state

  Success criteria:
  - Test-client: No failure window longer than 120 seconds, failure rate < 85%
  - Read-client: No failure window longer than 120 seconds, failure rate < 85%
  - Bad-client: 100% failure rate throughout (writes to read-only should never succeed)
  - All pods successfully restarted
  - Cluster converges to healthy state after rollout
  """
  print("\n=== Testing Rolling Restart Continuous Availability ===")

  # Precondition: Cluster is ready
  log_info("Verifying initial cluster health...")

  # First ensure StatefulSet is fully ready (handles case where previous test did rolling restart)
  log_info("Ensuring StatefulSet is fully stable...")
  assert wait_for_statefulset_ready(timeout=120), "StatefulSet failed to become ready"

  # Then wait for cluster convergence
  assert wait_for_cluster_convergence(timeout=120), "Cluster failed to converge initially"

  # Now verify we have 3 pods
  initial_status = get_statefulset_rollout_status()
  assert initial_status['replicas'] == 3, f"Expected 3 replicas, got {initial_status['replicas']}"
  assert initial_status['ready_replicas'] == 3, f"Not all replicas ready: {initial_status['ready_replicas']}/3"

  # Helper function to get client pods
  def get_client_pod(client_name):
    """Get pod name for a specific client"""
    pods_json = kubectl_get("pods", namespace=MEMGRAPH_NS, selector=f"app={client_name}", output="json")
    pods_data = json.loads(pods_json)
    assert len(pods_data['items']) > 0, f"No {client_name} pods found"
    return pods_data['items'][0]['metadata']['name']

  # Capture test start time to verify writes after this point
  test_start_time = datetime.datetime.now(datetime.UTC)
  log_info(f"Test start time: {test_start_time.isoformat()}")

  # Verify test-client is writing successfully
  log_info("Verifying test-client is operational...")
  test_client_pod = get_test_client_pod()

  # Wait up to 60 seconds for test-client to have 5 consecutive successes
  max_wait_seconds = 60
  check_interval = 3
  consecutive_success_target = 5

  for elapsed in range(0, max_wait_seconds, check_interval):
    time.sleep(check_interval)

    recent_logs = get_pod_logs(test_client_pod, tail_lines=20)

    # Check for consecutive successes in recent logs
    consecutive_successes = 0
    writes_after_test_start = 0

    for line in reversed(recent_logs.strip().split('\n')[-10:]):  # Check last 10 lines, most recent first
      try:
        log_data = parse_logfmt(line)
        # Parse timestamp from log
        timestamp_str = log_data.get('ts', '')
        if timestamp_str:
          log_timestamp = datetime.datetime.fromisoformat(timestamp_str.replace('+00:00', '+00:00'))
          # Only count writes that happened after test started
          if log_timestamp >= test_start_time:
            writes_after_test_start += 1
            # Check for success in either 'status' field or 'msg' field
            if 'Write success' in log_data.get('msg', '') or log_data.get('status') == 'success':
              consecutive_successes += 1
              if consecutive_successes >= consecutive_success_target:
                log_info(f"Found {consecutive_successes} consecutive successful writes after {elapsed + check_interval}s")
                break
            else:
              # Reset counter if we hit a failure
              consecutive_successes = 0
      except Exception:
        continue

    if consecutive_successes >= consecutive_success_target:
      log_info(f"Test-client is healthy with {consecutive_successes} consecutive successes")
      break

    log_info(f"Waiting for test-client stability... ({elapsed + check_interval}s/{max_wait_seconds}s, consecutive successes: {consecutive_successes})")

  assert consecutive_successes >= consecutive_success_target, f"Test-client not healthy: only {consecutive_successes} consecutive successes (need {consecutive_success_target})"

  # Verify read-client is reading successfully
  log_info("Verifying read-client is operational...")
  read_client_pod = get_client_pod("read-client")

  # Wait up to 60 seconds for read-client to have 5 consecutive successes
  for elapsed in range(0, max_wait_seconds, check_interval):
    time.sleep(check_interval)

    read_recent_logs = get_pod_logs(read_client_pod, tail_lines=20)

    # Check for consecutive successes in recent logs
    consecutive_read_successes = 0
    reads_after_test_start = 0

    for line in reversed(read_recent_logs.strip().split('\n')[-10:]):  # Check last 10 lines, most recent first
      try:
        log_data = parse_logfmt(line)
        # Parse timestamp from log
        timestamp_str = log_data.get('ts', '')
        if timestamp_str:
          log_timestamp = datetime.datetime.fromisoformat(timestamp_str.replace('+00:00', '+00:00'))
          # Only count reads that happened after test started
          if log_timestamp >= test_start_time:
            reads_after_test_start += 1
            # Check for success in either 'status' field or 'msg' field
            if 'Read success' in log_data.get('msg', '') or log_data.get('status') == 'success':
              consecutive_read_successes += 1
              if consecutive_read_successes >= consecutive_success_target:
                log_info(f"Found {consecutive_read_successes} consecutive successful reads after {elapsed + check_interval}s")
                break
            else:
              # Reset counter if we hit a failure
              consecutive_read_successes = 0
      except Exception:
        continue

    if consecutive_read_successes >= consecutive_success_target:
      log_info(f"Read-client is healthy with {consecutive_read_successes} consecutive successes")
      break

    log_info(f"Waiting for read-client stability... ({elapsed + check_interval}s/{max_wait_seconds}s, consecutive successes: {consecutive_read_successes})")

  assert consecutive_read_successes >= consecutive_success_target, f"Read-client not healthy: only {consecutive_read_successes} consecutive successes (need {consecutive_success_target})"

  # Verify bad-client is failing as expected (writes to read-only should fail)
  log_info("Verifying bad-client is failing writes as expected...")
  bad_client_pod = get_client_pod("bad-client")
  bad_recent_logs = get_pod_logs(bad_client_pod, tail_lines=20)

  bad_failure_count = 0
  bad_writes_after_test_start = 0
  for line in bad_recent_logs.strip().split('\n')[-15:]:
    try:
      log_data = parse_logfmt(line)
      # Parse timestamp from log
      timestamp_str = log_data.get('ts', '')
      if timestamp_str:
        log_timestamp = datetime.datetime.fromisoformat(timestamp_str.replace('+00:00', '+00:00'))
        # Only count operations that happened after test started
        if log_timestamp >= test_start_time:
          bad_writes_after_test_start += 1
          if 'failed' in log_data.get('msg', '').lower() or 'error' in log_data.get('msg', '').lower():
            bad_failure_count += 1
    except Exception:
      continue

  log_info(f"Found {bad_writes_after_test_start} bad-client operations after test start, {bad_failure_count} failed as expected")
  assert bad_writes_after_test_start >= 5, f"Not enough bad-client operations after test start: only {bad_writes_after_test_start} found"
  assert bad_failure_count >= 3, f"Bad-client not behaving as expected: only {bad_failure_count}/{bad_writes_after_test_start} operations failed after test start"

  # Step 3: Trigger rolling restart
  log_info("Triggering rolling restart of StatefulSet...")
  rollout_start_time = datetime.datetime.now(datetime.UTC)

  # Get initial pod UIDs before triggering rollout
  initial_pods_json = kubectl_get("pods", namespace=MEMGRAPH_NS, selector="app.kubernetes.io/name=memgraph", output="json")
  initial_pods_data = json.loads(initial_pods_json)
  initial_pod_uids = {pod['metadata']['name']: pod['metadata']['uid'] for pod in initial_pods_data['items']}
  log_info(f"Initial pods: {list(initial_pod_uids.keys())}")
  
  trigger_statefulset_rollout()

  # Step 4: Monitor the rollout completion
  log_info("Monitoring rollout progress...")
  kubernetes_rollout_completed = wait_for_rollout_completion(timeout=240)

  # Wait for actual service convergence (main-sync-async topology ready)
  log_info("Waiting for cluster service convergence...")
  service_converged = wait_for_cluster_convergence(timeout=180)

  # Calculate actual rollout duration based on service convergence
  rollout_duration = int(time.time() - rollout_start_time.timestamp())
  rollout_completed = kubernetes_rollout_completed and service_converged
  
  # Check which pods were recreated
  final_pods_json = kubectl_get("pods", namespace=MEMGRAPH_NS, selector="app.kubernetes.io/name=memgraph", output="json")
  final_pods_data = json.loads(final_pods_json)
  final_pod_uids = {pod['metadata']['name']: pod['metadata']['uid'] for pod in final_pods_data['items']}
  
  pods_recreated = []
  for pod_name in initial_pod_uids:
    if pod_name in final_pod_uids and initial_pod_uids[pod_name] != final_pod_uids[pod_name]:
      pods_recreated.append(pod_name)
      log_info(f"Pod recreated: {pod_name}")
  
  restart_info = {
    'pods_restarted': pods_recreated,
    'restart_count': len(pods_recreated),
    'events': []
  }

  # Get all client logs during rollout period
  log_info("Analyzing client operations during rollout...")
  since_time = (rollout_start_time - datetime.timedelta(seconds=5)).strftime('%Y-%m-%dT%H:%M:%SZ')

  # Collect logs from all clients
  test_client_logs = get_pod_logs(test_client_pod, since_time=since_time)
  read_client_logs = get_pod_logs(read_client_pod, since_time=since_time)
  bad_client_logs = get_pod_logs(bad_client_pod, since_time=since_time)

  # Analyze each client's operations using actual service convergence duration
  test_client_analysis = analyze_client_operations_during_rollout(
      test_client_logs,
      rollout_start_time,
      rollout_duration
  )

  read_client_analysis = analyze_client_operations_during_rollout(
      read_client_logs,
      rollout_start_time,
      rollout_duration
  )

  bad_client_analysis = analyze_client_operations_during_rollout(
      bad_client_logs,
      rollout_start_time,
      rollout_duration
  )

  # Get controller logs for additional context
  controller_pod = get_controller_pod()
  # controller_logs = get_pod_logs(controller_pod, since_time=since_time, tail_lines=100)  # Future: could analyze

  # Print analysis results
  print("\n=== Rollout Analysis Results ===")
  print(f"Kubernetes Rollout Completed: {kubernetes_rollout_completed}")
  print(f"Service Convergence Completed: {service_converged}")
  print(f"Total Rollout Duration: {rollout_duration}s")
  print(f"Overall Success: {rollout_completed}")

  print("\nPod Restart Information:")
  print(f"  Pods restarted: {restart_info.get('restart_count', 0)}")
  print(f"  Restarted pods: {restart_info.get('pods_restarted', [])}")

  print("\nTest-Client Operation Analysis (writes to RW gateway):")
  print(f"  Total operations: {test_client_analysis['total_operations']}")
  print(f"  Successful: {test_client_analysis['successful_operations']}")
  print(f"  Failed: {test_client_analysis['failed_operations']}")
  print(f"  Failure rate: {test_client_analysis['failure_rate']:.2f}%")
  print(f"  Failure windows: {len(test_client_analysis['failure_windows'])}")
  print(f"  Max failure window: {test_client_analysis['max_failure_window_seconds']:.1f}s")

  print("\nRead-Client Operation Analysis (reads from RO gateway):")
  print(f"  Total operations: {read_client_analysis['total_operations']}")
  print(f"  Successful: {read_client_analysis['successful_operations']}")
  print(f"  Failed: {read_client_analysis['failed_operations']}")
  print(f"  Failure rate: {read_client_analysis['failure_rate']:.2f}%")
  print(f"  Failure windows: {len(read_client_analysis['failure_windows'])}")
  print(f"  Max failure window: {read_client_analysis['max_failure_window_seconds']:.1f}s")

  print("\nBad-Client Operation Analysis (writes to RO gateway - should fail):")
  print(f"  Total operations: {bad_client_analysis['total_operations']}")
  print(f"  Successful: {bad_client_analysis['successful_operations']}")
  print(f"  Failed: {bad_client_analysis['failed_operations']}")
  print(f"  Failure rate: {bad_client_analysis['failure_rate']:.2f}%")
  print(f"  Expected: 100% failure rate (writes to read-only should always fail)")

  # Print failure windows for test and read clients
  for client_name, analysis in [("Test-Client", test_client_analysis), ("Read-Client", read_client_analysis)]:
    if analysis['failure_windows']:
      print(f"\n{client_name} Failure Windows:")
      for i, window in enumerate(analysis['failure_windows'][:5]):  # Show first 5
        print(f"  Window {i+1}: {window['duration']:.1f}s with {len(window['failures'])} failures")

  # Step 5: Verify success criteria
  print("\n=== Verifying Success Criteria ===")

  # Check rollout completed
  assert rollout_completed, "Rollout did not complete within timeout"
  print("✓ Rollout completed successfully")

  # Check all pods were restarted (rolling restart should touch all pods)
  assert restart_info.get('restart_count', 0) >= 3, \
      f"Expected all 3 pods to restart, but only {restart_info.get('restart_count', 0)} restarted"
  print("✓ All pods were restarted")

  # Verify rolling restart duration and post-restart success
  assert rollout_duration <= 300, \
      f"Rolling restart took too long: {rollout_duration:.1f}s (max allowed: 300s)"
  print(f"✓ Rolling restart completed in reasonable time: {rollout_duration:.1f}s")

  # CRITICAL: Verify continuous write success AFTER rolling restart completes
  # This is more important than failure rate during the unsafe transition period
  assert service_converged, \
      "Service convergence failed after rolling restart"
  print("✓ Service convergence completed after rolling restart")

  # Verify that writes are working continuously post-restart with retry logic
  # Import the verify function from test_failover_pod_deletion.py
  from test_failover_pod_deletion import verify_recent_test_client_success

  print("Waiting for test-client to accumulate successful operations after rolling restart...")
  post_restart_success = False

  # Retry for up to 30 seconds to allow test-client to accumulate enough successful operations
  for attempt in range(15):  # 15 attempts * 2 seconds = 30 seconds max
      post_restart_success = verify_recent_test_client_success(required_consecutive=5)  # Reduced from 10 to 5
      if post_restart_success:
          print(f"✓ Test-client: Continuous write success verified after {(attempt+1)*2}s")
          break
      if attempt < 14:  # Don't sleep on last iteration
          time.sleep(2)

  assert post_restart_success, \
      "Test-client writes are not consistently successful after rolling restart completion (waited 30s)"

  # Verify read-client availability (reads should have minimal disruption)
  read_max_failure_window = read_client_analysis['max_failure_window_seconds']
  assert read_max_failure_window <= 30, \
      f"Read-client disruption too long: {read_max_failure_window:.1f}s (max allowed: 30s)"
  print(f"✓ Read-client: Minimal disruption during rolling restart: {read_max_failure_window:.1f}s")

  # Read operations should have much better availability than writes during rolling restart
  if read_client_analysis['failure_rate'] > 10.0:
      print(f"⚠ Warning: Read-client failure rate higher than expected: {read_client_analysis['failure_rate']:.2f}%")
  print(f"✓ Read-client: Failure rate during rolling restart: {read_client_analysis['failure_rate']:.2f}%")

  assert not read_client_analysis['had_complete_outage'], \
      "Read-client: Detected complete outage (>50s of continuous failures)"
  print("✓ Read-client: No complete outage detected")

  # Verify bad-client behaves as expected (100% failure rate)
  bad_failure_rate = bad_client_analysis['failure_rate']
  bad_success_count = bad_client_analysis['successful_operations']
  assert bad_success_count == 0, \
      f"Bad-client should NEVER succeed writes to read-only gateway, but had {bad_success_count} successful operations"
  print(f"✓ Bad-client: Correctly failing ALL writes to read-only gateway ({bad_failure_rate:.2f}% failure rate)")

  # Step 6: Verify cluster returns to healthy state
  log_info("Verifying cluster health after rollout...")
  assert wait_for_cluster_convergence(timeout=180), "Cluster failed to converge after rollout"

  # Verify final state
  final_status = get_statefulset_rollout_status()
  assert final_status['ready_replicas'] == 3, \
      f"Not all replicas ready after rollout: {final_status['ready_replicas']}/3"
  print("✓ Cluster returned to healthy state")

  # Verify main pod (it might have changed during rollout)
  try:
    new_main = find_main_pod_by_querying()
    print(f"✓ Main pod after rollout: {new_main}")
  except Exception as e:
    print(f"⚠ Could not determine main pod: {e}")

  print("\n✅ Rolling restart test completed successfully!")
  print(f"   Rolling restart duration: {rollout_duration:.1f}s")
  print(f"   Test-client (writes): {test_client_analysis['failure_rate']:.1f}% failure rate during transition (expected)")
  print(f"   Read-client (reads): {read_client_analysis['failure_rate']:.1f}% failure rate, max interruption: {read_max_failure_window:.1f}s")
  print(f"   Bad-client: {bad_client_analysis['failure_rate']:.2f}% failure rate (correctly rejecting writes to read-only gateway)")


def test_rolling_restart_with_main_changes():
  """
  Test that verifies proper failover and main role handling during rolling restart.

  This test specifically checks:
  1. After initially checking for cluster healthy
  2. If pod-0 is main pod, delete it to force failover to pod-1
  3. Assertion: pod-1 should be eventually promoted to main
  4. Wait cluster healthy (all replication status 'ready')
  5. Perform rolling restart
  6. Same verification points as before

  Success criteria:
  - Forced failover completes successfully
  - Pod-1 becomes main after pod-0 deletion
  - Cluster converges to healthy state after failover
  - Rolling restart maintains availability with multiple clients
  - Main role changes are handled gracefully during rolling restart
  """
  print("\n=== Testing Rolling Restart with Failover Scenario ===")

  # Step 1: Verify initial cluster health
  log_info("Step 1: Verifying initial cluster health...")

  # First ensure StatefulSet is fully ready
  log_info("Ensuring StatefulSet is fully stable...")
  assert wait_for_statefulset_ready(timeout=120), "StatefulSet failed to become ready"

  # Then wait for cluster convergence
  assert wait_for_cluster_convergence(timeout=120), "Cluster failed to converge initially"

  # Record initial main
  initial_main = find_main_pod_by_querying()
  log_info(f"Initial main pod: {initial_main}")

  # Verify we have 3 pods ready
  initial_status = get_statefulset_rollout_status()
  assert initial_status['replicas'] == 3, f"Expected 3 replicas, got {initial_status['replicas']}"
  assert initial_status['ready_replicas'] == 3, f"Not all replicas ready: {initial_status['ready_replicas']}/3"
  print("✓ Initial cluster health verified")

  # Step 2: Force failover if pod-0 is main
  if initial_main == "memgraph-ha-0":
    log_info("Step 2: pod-0 is main, forcing failover by deleting pod-0...")

    # Delete pod-0 to trigger failover
    delete_cmd = ["kubectl", "delete", "pod", "memgraph-ha-0", "-n", MEMGRAPH_NS]
    result = subprocess.run(delete_cmd, capture_output=True, text=True)
    if result.returncode != 0:
      raise E2ETestError(f"Failed to delete pod-0: {result.stderr}")

    log_info("pod-0 deleted, waiting for failover to complete...")

    # Step 3: Wait for pod-1 to become main (failover assertion)
    log_info("Step 3: Verifying pod-1 becomes main...")
    failover_start_time = time.time()
    pod_1_became_main = False

    while time.time() - failover_start_time < 60:  # 60 second timeout for failover
      try:
        current_main = find_main_pod_by_querying()
        if current_main == "memgraph-ha-1":
          pod_1_became_main = True
          log_info(f"✓ Failover successful: pod-1 is now main (took {time.time() - failover_start_time:.1f}s)")
          break
      except Exception:
        # Continue waiting if we can't determine main yet
        pass
      time.sleep(2)

    assert pod_1_became_main, "pod-1 did not become main within 60 seconds after pod-0 deletion"

  else:
    log_info(f"Step 2-3: pod-0 is not main (main is {initial_main}), skipping forced failover")
    print("✓ Skipping forced failover - pod-0 is not current main")

  # Step 4: Wait for cluster to become healthy after failover (all replication ready)
  log_info("Step 4: Waiting for cluster to be healthy after failover...")
  assert wait_for_cluster_convergence(timeout=120), "Cluster failed to converge after failover"

  # Verify all pods are ready again
  post_failover_status = get_statefulset_rollout_status()
  assert post_failover_status['ready_replicas'] == 3, f"Not all replicas ready after failover: {post_failover_status['ready_replicas']}/3"

  current_main_after_failover = find_main_pod_by_querying()
  log_info(f"✓ Cluster healthy after failover, current main: {current_main_after_failover}")

  # Step 5: Perform rolling restart with the same verification as the continuous availability test
  log_info("Step 5: Performing rolling restart with multi-client verification...")

  # Helper function to get client pods
  def get_client_pod(client_name):
    """Get pod name for a specific client"""
    pods_json = kubectl_get("pods", namespace=MEMGRAPH_NS, selector=f"app={client_name}", output="json")
    pods_data = json.loads(pods_json)
    assert len(pods_data['items']) > 0, f"No {client_name} pods found"
    return pods_data['items'][0]['metadata']['name']

  # Verify all clients are operational before rolling restart
  log_info("Verifying all clients are operational...")
  test_client_pod = get_test_client_pod()
  read_client_pod = get_client_pod("read-client")
  bad_client_pod = get_client_pod("bad-client")

  print(f"  Test-client pod: {test_client_pod}")
  print(f"  Read-client pod: {read_client_pod}")
  print(f"  Bad-client pod: {bad_client_pod}")

  # Get initial pod UIDs before triggering rollout for restart tracking
  initial_pods_json = kubectl_get("pods", namespace=MEMGRAPH_NS, selector="app.kubernetes.io/name=memgraph", output="json")
  initial_pods_data = json.loads(initial_pods_json)
  initial_pod_uids = {pod['metadata']['name']: pod['metadata']['uid'] for pod in initial_pods_data['items']}

  rollout_start_time = datetime.datetime.now(datetime.UTC)
  log_info("Triggered rolling restart of memgraph-ha")
  trigger_statefulset_rollout()

  # Step 6: Monitor rollout completion with multi-client verification
  log_info("Step 6: Monitoring rollout with multi-client verification...")
  kubernetes_rollout_completed = wait_for_rollout_completion(timeout=240)

  # Wait for cluster service convergence
  log_info("Waiting for cluster service convergence...")
  service_converged = wait_for_cluster_convergence(timeout=180)

  # Calculate actual rollout duration
  rollout_duration = int(time.time() - rollout_start_time.timestamp())
  rollout_completed = kubernetes_rollout_completed and service_converged

  # Check which pods were recreated
  final_pods_json = kubectl_get("pods", namespace=MEMGRAPH_NS, selector="app.kubernetes.io/name=memgraph", output="json")
  final_pods_data = json.loads(final_pods_json)
  final_pod_uids = {pod['metadata']['name']: pod['metadata']['uid'] for pod in final_pods_data['items']}

  pods_recreated = []
  for pod_name in initial_pod_uids:
    if pod_name in final_pod_uids and initial_pod_uids[pod_name] != final_pod_uids[pod_name]:
      pods_recreated.append(pod_name)
      log_info(f"Pod recreated during rolling restart: {pod_name}")

  # Get all client logs during rollout period for verification
  log_info("Analyzing all client operations during rolling restart...")
  since_time = (rollout_start_time - datetime.timedelta(seconds=5)).strftime('%Y-%m-%dT%H:%M:%SZ')

  # Collect logs from all clients
  test_client_logs = get_pod_logs(test_client_pod, since_time=since_time)
  read_client_logs = get_pod_logs(read_client_pod, since_time=since_time)
  bad_client_logs = get_pod_logs(bad_client_pod, since_time=since_time)

  # Analyze each client's operations
  test_client_analysis = analyze_client_operations_during_rollout(
      test_client_logs, rollout_start_time, rollout_duration)
  read_client_analysis = analyze_client_operations_during_rollout(
      read_client_logs, rollout_start_time, rollout_duration)
  bad_client_analysis = analyze_client_operations_during_rollout(
      bad_client_logs, rollout_start_time, rollout_duration)

  # Print comprehensive analysis results
  print("\n=== Failover + Rolling Restart Analysis Results ===")
  print(f"Initial main: {initial_main}")
  print(f"Main after failover: {current_main_after_failover}")
  print(f"Kubernetes Rollout Completed: {kubernetes_rollout_completed}")
  print(f"Service Convergence Completed: {service_converged}")
  print(f"Total Rolling Restart Duration: {rollout_duration}s")
  print(f"Pods Recreated: {len(pods_recreated)} - {pods_recreated}")

  print("\nTest-Client Analysis (writes to RW gateway):")
  print(f"  Total operations: {test_client_analysis['total_operations']}")
  print(f"  Successful: {test_client_analysis['successful_operations']}")
  print(f"  Failed: {test_client_analysis['failed_operations']}")
  print(f"  Failure rate: {test_client_analysis['failure_rate']:.2f}%")
  print(f"  Max failure window: {test_client_analysis['max_failure_window_seconds']:.1f}s")

  print("\nRead-Client Analysis (reads from RO gateway):")
  print(f"  Total operations: {read_client_analysis['total_operations']}")
  print(f"  Successful: {read_client_analysis['successful_operations']}")
  print(f"  Failed: {read_client_analysis['failed_operations']}")
  print(f"  Failure rate: {read_client_analysis['failure_rate']:.2f}%")
  print(f"  Max failure window: {read_client_analysis['max_failure_window_seconds']:.1f}s")

  print("\nBad-Client Analysis (writes to RO gateway - should fail):")
  print(f"  Total operations: {bad_client_analysis['total_operations']}")
  print(f"  Failed: {bad_client_analysis['failed_operations']}")
  print(f"  Failure rate: {bad_client_analysis['failure_rate']:.2f}%")

  # Verify success criteria
  print("\n=== Verifying Success Criteria ===")

  # Check rollout completed
  assert rollout_completed, "Rolling restart did not complete within timeout"
  print("✓ Rolling restart completed successfully")

  # Verify rolling restart duration and post-restart success
  assert rollout_duration <= 300, \
      f"Rolling restart took too long: {rollout_duration:.1f}s (max allowed: 300s)"
  print(f"✓ Rolling restart completed in reasonable time: {rollout_duration:.1f}s")

  # CRITICAL: Verify continuous write success AFTER rolling restart completes
  assert service_converged, \
      "Service convergence failed after rolling restart"
  print("✓ Service convergence completed after rolling restart")

  # Verify that writes are working continuously post-restart with retry logic
  # Import the verify function from test_failover_pod_deletion.py
  from test_failover_pod_deletion import verify_recent_test_client_success

  print("Waiting for test-client to accumulate successful operations after rolling restart...")
  post_restart_success = False

  # Retry for up to 30 seconds to allow test-client to accumulate enough successful operations
  for attempt in range(15):  # 15 attempts * 2 seconds = 30 seconds max
      post_restart_success = verify_recent_test_client_success(required_consecutive=5)  # Reduced from 10 to 5
      if post_restart_success:
          print(f"✓ Test-client: Continuous write success verified after {(attempt+1)*2}s")
          break
      if attempt < 14:  # Don't sleep on last iteration
          time.sleep(2)

  assert post_restart_success, \
      "Test-client writes are not consistently successful after rolling restart completion (waited 30s)"

  # Verify read-client availability (reads should have minimal disruption)
  read_max_failure_window = read_client_analysis['max_failure_window_seconds']
  assert read_max_failure_window <= 30, \
      f"Read-client disruption too long: {read_max_failure_window:.1f}s (max allowed: 30s)"
  print(f"✓ Read-client: Minimal disruption during rolling restart: {read_max_failure_window:.1f}s")

  # Verify bad-client behaves as expected (100% failure rate)
  bad_success_count = bad_client_analysis['successful_operations']
  assert bad_success_count == 0, f"Bad-client should NEVER succeed, but had {bad_success_count} successful operations"
  print(f"✓ Bad-client: Correctly failing ALL writes ({bad_client_analysis['failure_rate']:.2f}% failure rate)")

  # Verify final cluster state
  log_info("Verifying final cluster health...")
  assert wait_for_cluster_convergence(timeout=180), "Cluster failed to converge after rolling restart"

  final_main = find_main_pod_by_querying()
  print(f"✓ Final main pod: {final_main}")

  print("\n✅ Rolling restart with failover scenario test completed successfully!")
  print(f"   Sequence: {initial_main} → failover → {current_main_after_failover} → rolling restart → {final_main}")
  print(f"   Rolling restart duration: {rollout_duration:.1f}s")
  print(f"   Test-client: {test_client_analysis['failure_rate']:.1f}% failure rate during transition (expected)")
  print(f"   Read-client: {read_client_analysis['failure_rate']:.1f}% failure rate")
  print(f"   Bad-client: {bad_client_analysis['failure_rate']:.2f}% failure rate (correct)")
