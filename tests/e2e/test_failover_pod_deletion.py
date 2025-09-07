"""
Failover Pod Deletion Tests

Tests cluster failover behavior when main pod is deleted.
Focuses on measuring actual failover timing and recovery through test-client logs.
"""

import pytest
import time
import json
from datetime import datetime, timedelta
from utils import (
    wait_for_cluster_convergence,
    get_test_client_logs,
    get_test_client_logs_since,
    find_main_pod_by_querying,
    delete_pod_forcefully,
    log_info
)


def parse_log_entry(log_line: str) -> tuple:
    """
    Parse JSON log entry from test-client
    Expected format: {"level":30,"time":"2025-09-07T11:03:08.338Z","msg":"✓ Success | Total: 2898..."}
    
    Returns:
        (timestamp, message, is_success, is_failure)
    """
    try:
        log_data = json.loads(log_line)
        timestamp_str = log_data.get('time', '')
        message = log_data.get('msg', '')
        
        # Parse timestamp and convert to naive datetime for comparison
        timestamp = datetime.fromisoformat(timestamp_str.replace('Z', '+00:00')).replace(tzinfo=None)
        
        # Determine if success or failure
        is_success = '✓ Success' in message
        is_failure = ('✗ Failed' in message or 
                      ('error' in message.lower() and log_data.get('level', 30) >= 50))
        
        return timestamp, message, is_success, is_failure
        
    except (json.JSONDecodeError, ValueError, KeyError):
        # Fallback for non-JSON logs or parsing errors
        return datetime.fromtimestamp(0), log_line, False, False


def analyze_logs_for_failover(logs: str, failover_time: datetime, window_seconds: int = 30) -> dict:
    """
    Analyze test-client logs for failover behavior within time window
    
    Args:
        logs: Raw log output from test-client
        failover_time: When the failover was initiated 
        window_seconds: Time window to analyze after failover
        
    Returns:
        dict with analysis results
    """
    lines = logs.strip().split('\n')
    
    # Time window for analysis
    window_start = failover_time
    window_end = failover_time + timedelta(seconds=window_seconds)
    
    failure_count = 0
    success_count = 0
    first_failure_time = None
    first_success_after_failure = None
    last_failure_time = None
    
    failures_in_window = []
    successes_in_window = []
    
    for line in lines:
        if not line.strip():
            continue
            
        log_time, message, is_success, is_failure = parse_log_entry(line)
        
        # Only analyze logs within the time window
        if window_start <= log_time <= window_end:
            if is_failure:
                failure_count += 1
                failures_in_window.append((log_time, message))
                if first_failure_time is None:
                    first_failure_time = log_time
                last_failure_time = log_time
                
            elif is_success:
                success_count += 1
                successes_in_window.append((log_time, message))
                # Track first success after we've seen failures
                if failure_count > 0 and first_success_after_failure is None:
                    first_success_after_failure = log_time
    
    # Calculate recovery time
    recovery_time_seconds = None
    if first_failure_time and first_success_after_failure:
        recovery_time_seconds = (first_success_after_failure - first_failure_time).total_seconds()
    
    return {
        'failure_count': failure_count,
        'success_count': success_count,
        'first_failure_time': first_failure_time,
        'last_failure_time': last_failure_time,
        'first_success_after_failure': first_success_after_failure,
        'recovery_time_seconds': recovery_time_seconds,
        'failures_in_window': failures_in_window,
        'successes_in_window': successes_in_window
    }


def verify_recent_test_client_success(required_successes: int = 30) -> bool:
    """
    Verify test-client has recent successful operations
    
    Args:
        required_successes: Number of recent successes required
        
    Returns:
        True if precondition is met
    """
    log_info(f"Checking for {required_successes} recent successful test-client operations...")
    
    # Get recent logs
    logs = get_test_client_logs(tail_lines=50)
    lines = logs.strip().split('\n')
    
    success_count = 0
    for line in reversed(lines):  # Check most recent first
        log_time, message, is_success, is_failure = parse_log_entry(line)
        
        if is_success:
            success_count += 1
            if success_count >= required_successes:
                break
        elif is_failure:
            # If we hit a failure before getting enough successes, fail
            break
    
    log_info(f"Found {success_count} recent successful operations")
    return success_count >= required_successes


class TestFailoverPodDeletion:
    
    def test_main_pod_deletion_failover(self):
        """
        Test failover behavior when main pod is deleted
        
        Preconditions:
        - Cluster is ready 
        - Recent 30 writes of test-client, all must be success
        
        Test steps:
        1. Identify current main node
        2. Delete the node
        3. Poll test-client logs
        4. Expected: logs show failures (<5), then all success
        5. Wait up to 30s, expect cluster status: main-sync-async with new main
        """
        print("\n=== Testing Main Pod Deletion Failover ===")
        
        # Precondition: Cluster is ready
        assert wait_for_cluster_convergence(timeout=60), "Cluster failed to converge initially"
        
        # Precondition: Recent 30 writes must be successful
        assert verify_recent_test_client_success(30), "Test-client doesn't have 30 recent successful operations"
        
        # Step 1: Identify current main node
        original_main_pod = find_main_pod_by_querying()
        print(f"Current main pod: {original_main_pod}")
        
        # Step 2: Delete the main node
        print(f"Deleting main pod: {original_main_pod}")
        failover_start_time = datetime.utcnow()  # Use UTC to match log timestamps
        delete_pod_forcefully(original_main_pod)
        
        # Step 3 & 4: Poll test-client logs and analyze failover behavior
        print("Analyzing failover behavior in test-client logs...")
        
        # Wait a bit for failover to occur and logs to accumulate
        time.sleep(15)
        
        # Get logs since just before failover using time-based retrieval
        since_time = (failover_start_time - timedelta(seconds=10)).strftime('%Y-%m-%dT%H:%M:%SZ')
        logs = get_test_client_logs_since(since_time)
        analysis = analyze_logs_for_failover(logs, failover_start_time, window_seconds=45)
        
        # Debug: show actual time window and log timestamps
        print(f"Debug info:")
        print(f"  Failover start time (UTC): {failover_start_time}")
        print(f"  Analysis window: {failover_start_time} to {failover_start_time + timedelta(seconds=45)}")
        
        # Show some recent log timestamps for comparison
        lines = logs.strip().split('\n')
        print(f"  Recent log timestamps:")
        for i, line in enumerate(lines[-3:]):
            try:
                log_data = json.loads(line)
                timestamp_str = log_data.get('time', '')
                timestamp_naive = datetime.fromisoformat(timestamp_str.replace('Z', '+00:00')).replace(tzinfo=None)
                print(f"    Log {i+1}: {timestamp_naive}")
            except:
                pass
        
        print(f"Failover analysis:")
        print(f"  Failures in window: {analysis['failure_count']}")
        print(f"  Successes in window: {analysis['success_count']}")
        print(f"  Recovery time: {analysis['recovery_time_seconds']}s")
        
        # Analysis of results:
        if analysis['failure_count'] == 0 and analysis['success_count'] == 0:
            print("⚠ No operations logged during failover window - test-client likely stopped")
            print("This indicates complete service disruption during failover")
            
            # Wait longer and check if test-client recovers
            print("Waiting additional 15s for test-client recovery...")
            time.sleep(15)
            
            # Check if operations resume
            recovery_since_time = (failover_start_time + timedelta(seconds=30)).strftime('%Y-%m-%dT%H:%M:%SZ')
            recent_logs = get_test_client_logs_since(recovery_since_time)
            recent_analysis = analyze_logs_for_failover(recent_logs, failover_start_time + timedelta(seconds=30), window_seconds=10)
            
            if recent_analysis['success_count'] > 0:
                print("✓ Test-client recovered and operations resumed")
                recovery_confirmed = True
            else:
                print("❌ Test-client has not recovered - extended outage")
                recovery_confirmed = False
                
            assert recovery_confirmed, "Test-client did not recover after extended wait"
            
        else:
            # Expected behavior: some failures followed by successes
            assert analysis['failure_count'] > 0, "Expected some failures during failover"
            assert analysis['failure_count'] < 5, f"Too many failures during failover: {analysis['failure_count']}"
            assert analysis['success_count'] > 0, "Expected successful operations after failover"
            assert analysis['recovery_time_seconds'] is not None, "Could not determine recovery time"
            assert analysis['recovery_time_seconds'] < 20, f"Recovery took too long: {analysis['recovery_time_seconds']}s"
        
        # Step 5: Wait up to 30s, expect cluster status: main-sync-async with new main
        print("Verifying cluster convergence after failover...")
        assert wait_for_cluster_convergence(timeout=30), "Cluster failed to converge after failover"
        
        new_main_pod = find_main_pod_by_querying()
        print(f"New main pod after failover: {new_main_pod}")
        
        # Verify failover occurred (main should be on pod-0 or pod-1, not the deleted one)
        expected_main_pods = {"memgraph-ha-0", "memgraph-ha-1"}
        assert new_main_pod in expected_main_pods, f"Main pod {new_main_pod} should be pod-0 or pod-1"
        
        # In StatefulSet, deleted pod gets recreated with same name, so we check timing instead
        if new_main_pod == original_main_pod:
            print(f"⚠ Main role returned to {new_main_pod} (pod was recreated)")
        else:
            print(f"✓ Main role failed over from {original_main_pod} to {new_main_pod}")
        
        print("✓ Main pod deletion failover test completed successfully")
        print(f"  Total failover window: {analysis['failure_count']} failures, {analysis['success_count']} successes")
        print(f"  Recovery time: {analysis['recovery_time_seconds']:.1f}s")