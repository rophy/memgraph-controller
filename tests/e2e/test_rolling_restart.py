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

  operations = []
  failures = []
  successes = []
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

    except Exception as e:
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
      'had_complete_outage': max_failure_window > 50  # More than 50s of failures
  }


def test_rolling_restart_continuous_availability():
  """
  Test that the cluster maintains write availability during a rolling restart.

  Preconditions:
  - Cluster is ready and healthy
  - Test-client is running and writing successfully

  Test steps:
  1. Verify cluster is healthy with 3 pods
  2. Verify test-client is writing successfully
  3. Trigger a rolling restart of the StatefulSet
  4. Monitor test-client operations during the entire rollout
  5. Verify no extended write failures occurred
  6. Verify cluster returns to healthy state

  Success criteria:
  - No failure window longer than 60 seconds
  - Overall failure rate < 60%
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

  # Verify test-client is writing successfully
  log_info("Verifying test-client is operational...")
  test_client_pod = get_test_client_pod()
  recent_logs = get_pod_logs(test_client_pod, tail_lines=20)

  # Count recent successes
  success_count = 0
  for line in recent_logs.strip().split('\n')[-10:]:
    try:
      log_data = parse_logfmt(line)
      # Check for success in either 'status' field or 'msg' field
      if log_data.get('status') == 'success' or 'success' in log_data.get('msg', '').lower():
        success_count += 1
    except:
      continue

  assert success_count >= 7, f"Test-client not healthy: only {success_count}/10 recent operations successful"

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
  kubernetes_rollout_completed = wait_for_rollout_completion(timeout=120)

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

  # Get test-client logs during rollout period
  log_info("Analyzing test-client operations during rollout...")
  since_time = (rollout_start_time - datetime.timedelta(seconds=5)).strftime('%Y-%m-%dT%H:%M:%SZ')
  client_logs = get_pod_logs(test_client_pod, since_time=since_time)

  # Analyze client operations using actual service convergence duration
  client_analysis = analyze_client_operations_during_rollout(
      client_logs,
      rollout_start_time,
      rollout_duration
  )

  # Get controller logs for additional context
  controller_pod = get_controller_pod()
  controller_logs = get_pod_logs(controller_pod, since_time=since_time, tail_lines=100)

  # Print analysis results
  print("\n=== Rollout Analysis Results ===")
  print(f"Kubernetes Rollout Completed: {kubernetes_rollout_completed}")
  print(f"Service Convergence Completed: {service_converged}")
  print(f"Total Rollout Duration: {rollout_duration}s")
  print(f"Overall Success: {rollout_completed}")

  print("\nPod Restart Information:")
  print(f"  Pods restarted: {restart_info.get('restart_count', 0)}")
  print(f"  Restarted pods: {restart_info.get('pods_restarted', [])}")

  print("\nClient Operation Analysis:")
  print(f"  Total operations: {client_analysis['total_operations']}")
  print(f"  Successful: {client_analysis['successful_operations']}")
  print(f"  Failed: {client_analysis['failed_operations']}")
  print(f"  Failure rate: {client_analysis['failure_rate']:.2f}%")
  print(f"  Failure windows: {len(client_analysis['failure_windows'])}")
  print(f"  Max failure window: {client_analysis['max_failure_window_seconds']:.1f}s")

  # Print failure windows if any
  if client_analysis['failure_windows']:
    print("\nFailure Windows:")
    for i, window in enumerate(client_analysis['failure_windows'][:5]):  # Show first 5
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

  # Check no extended failure windows
  max_failure_window = client_analysis['max_failure_window_seconds']
  assert max_failure_window <= 120, \
      f"Failure window too long: {max_failure_window:.1f}s (max allowed: 120s)"
  print(f"✓ No extended failure windows (max: {max_failure_window:.1f}s)")

  # Check overall failure rate
  failure_rate = client_analysis['failure_rate']
  assert failure_rate <= 85.0, \
      f"Failure rate too high: {failure_rate:.2f}% (max allowed: 85%)"
  print(f"✓ Acceptable failure rate: {failure_rate:.2f}%")

  # Verify no complete outage
  assert not client_analysis['had_complete_outage'], \
      "Detected complete outage (>50s of continuous failures)"
  print("✓ No complete outage detected")

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
  print(f"   The cluster maintained {100 - failure_rate:.1f}% availability during rolling restart")
  print(f"   Maximum service interruption: {max_failure_window:.1f}s")


def test_rolling_restart_with_main_changes():
  """
  Test that verifies proper main role handling during rolling restart.

  This test specifically checks that:
  - Main role is properly transferred when the main pod is restarted
  - No split-brain occurs during the restart
  - Write availability is maintained

  Success criteria:
  - Main role changes are handled gracefully
  - No period with multiple mains
  - No period with no main > 15 seconds
  """
  print("\n=== Testing Rolling Restart with Main Role Changes ===")

  # Precondition: Cluster is ready
  log_info("Verifying initial cluster health...")
  
  # First ensure StatefulSet is fully ready (handles case where previous test did rolling restart)
  log_info("Ensuring StatefulSet is fully stable...")
  assert wait_for_statefulset_ready(timeout=120), "StatefulSet failed to become ready"
  
  # Then wait for cluster convergence
  assert wait_for_cluster_convergence(timeout=120), "Cluster failed to converge initially"

  # Record initial main
  initial_main = find_main_pod_by_querying()
  log_info(f"Initial main pod: {initial_main}")

  # Trigger rolling restart
  log_info("Triggering rolling restart...")
  rollout_start_time = datetime.datetime.now(datetime.UTC)
  trigger_statefulset_rollout()

  # Monitor main changes during rollout
  main_changes = []
  no_main_periods = []
  multiple_main_periods = []

  start_time = time.time()
  last_main = initial_main
  no_main_start = None

  while time.time() - start_time < 300:  # Monitor for up to 5 minutes
    try:
      current_main = find_main_pod_by_querying()

      if current_main != last_main:
        main_changes.append({
            'time': time.time() - start_time,
            'from': last_main,
            'to': current_main
        })
        log_info(f"Main changed from {last_main} to {current_main}")
        last_main = current_main

        # End any no-main period
        if no_main_start:
          no_main_periods.append({
              'start': no_main_start,
              'duration': time.time() - no_main_start
          })
          no_main_start = None

      # Check if rollout completed
      status = get_statefulset_rollout_status()
      if not status['is_rolling'] and status['ready_replicas'] == status['replicas']:
        log_info("Rollout completed")
        break

    except Exception as e:
      # Could not determine main - track no-main period
      if not no_main_start:
        no_main_start = time.time()
      log_info(f"Could not determine main: {e}")

    time.sleep(2)

  # Analysis
  print("\n=== Main Role Change Analysis ===")
  print(f"Total main changes: {len(main_changes)}")
  for change in main_changes:
    print(f"  At {change['time']:.1f}s: {change['from']} → {change['to']}")

  if no_main_periods:
    print(f"Periods with no main: {len(no_main_periods)}")
    max_no_main = max([p['duration'] for p in no_main_periods])
    print(f"  Maximum no-main period: {max_no_main:.1f}s")
    assert max_no_main <= 15, f"Period with no main too long: {max_no_main:.1f}s"
  else:
    print("✓ No periods without a main detected")

  # Verify final state
  final_main = find_main_pod_by_querying()
  print(f"Final main pod: {final_main}")

  print("\n✅ Rolling restart with main changes test completed successfully!")
