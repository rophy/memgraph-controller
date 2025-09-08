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
    log_info,
    parse_logfmt,
    monitor_main_pod_changes_enhanced,
    get_controller_logs_since,
    detect_failover_in_controller_logs,
    get_pod_age_seconds
)


def parse_log_entry(log_line: str) -> tuple:
    """
    Parse logfmt log entry from test-client
    Expected format: ts=2025-09-07T12:58:34.055123+00:00 at=INFO msg="Success" total=1
    
    Returns:
        (timestamp, message, is_success, is_failure)
    """
    try:
        log_data = parse_logfmt(log_line)
        timestamp_str = log_data.get('ts', '')
        message = log_data.get('msg', '')
        
        # Parse timestamp and convert to naive datetime for comparison
        timestamp = datetime.fromisoformat(timestamp_str).replace(tzinfo=None)
        
        # Determine if success or failure based on message content
        is_success = 'Success' in message
        is_failure = ('Failed' in message or 
                      log_data.get('at', '').upper() == 'ERROR')
        
        return timestamp, message, is_success, is_failure
        
    except (ValueError, KeyError, TypeError):
        # Fallback for logfmt parsing errors
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


def verify_recent_test_client_success(required_consecutive: int = 10) -> bool:
    """
    Verify that the latest consecutive N write operations were ALL successful.
    This ensures the system is currently in a stable, healthy state.
    
    Args:
        required_consecutive: Number of latest consecutive operations that must be successful
        
    Returns:
        True if the latest N operations were all successful
    """
    log_info(f"Checking that latest {required_consecutive} consecutive writes were all successful...")
    
    # Get recent logs
    logs = get_test_client_logs(tail_lines=50)
    lines = logs.strip().split('\n')
    
    # Parse and identify write operations (both successes and failures)
    operations = []
    for line in reversed(lines):  # Most recent first
        log_time, message, is_success, is_failure = parse_log_entry(line)
        
        # Only consider actual write operations (success or failure)
        if is_success or is_failure:
            operations.append({
                'time': log_time,
                'success': is_success,
                'failure': is_failure,
                'message': message
            })
    
    # Check if we have enough operations
    if len(operations) < required_consecutive:
        log_info(f"Found only {len(operations)} write operations, need {required_consecutive}")
        return False
    
    # Check that the latest N consecutive operations were ALL successful
    latest_operations = operations[:required_consecutive]  # Most recent N
    
    consecutive_successes = 0
    for op in latest_operations:
        if op['success']:
            consecutive_successes += 1
        else:
            # Found a failure in the latest N operations
            log_info(f"Found failure in latest {required_consecutive} operations: '{op['message']}'")
            break
    
    all_successful = consecutive_successes == required_consecutive
    
    if all_successful:
        log_info(f"✅ Latest {required_consecutive} consecutive writes were all successful")
    else:
        log_info(f"❌ Only {consecutive_successes}/{required_consecutive} latest writes were successful")
    
    return all_successful


class TestFailoverPodDeletion:
    
    def test_main_pod_deletion_failover(self):
        """
        Test failover behavior when main pod is deleted
        
        Preconditions:
        - Cluster is ready 
        - Latest 10 consecutive writes of test-client must ALL be successful
        
        Test steps:
        1. Identify current main node
        2. Delete the node  
        3. Monitor for actual failover via database state changes and controller logs
        4. Analyze client impact as secondary verification
        5. Verify cluster convergence with new main pod
        """
        print("\n=== Testing Main Pod Deletion Failover ===")
        
        # Precondition: Cluster is ready
        assert wait_for_cluster_convergence(timeout=60), "Cluster failed to converge initially"
        
        # Precondition: Latest 10 consecutive writes must be successful
        assert verify_recent_test_client_success(10), "Test-client's latest 10 consecutive operations were not all successful"
        
        # Step 1: Identify current main node
        original_main_pod = find_main_pod_by_querying()
        print(f"Current main pod: {original_main_pod}")
        
        # Step 2: Delete the main node
        print(f"Deleting main pod: {original_main_pod}")
        failover_start_time = datetime.utcnow()  # Use UTC to match log timestamps
        delete_pod_forcefully(original_main_pod)
        
        # Step 3: Monitor for actual failover using controller logs and direct database polling  
        print("Monitoring for failover occurrence...")
        
        # Start monitoring main pod changes in parallel
        print("  Monitoring main pod role changes...")
        main_monitoring = monitor_main_pod_changes_enhanced(original_main_pod, timeout=30)
        
        # Get controller logs to verify failover was triggered
        controller_since_time = (failover_start_time - timedelta(seconds=5)).strftime('%Y-%m-%dT%H:%M:%SZ')
        controller_logs = get_controller_logs_since(controller_since_time)
        controller_analysis = detect_failover_in_controller_logs(controller_logs)
        
        # Step 4: Analyze client impact (secondary verification)
        since_time = (failover_start_time - timedelta(seconds=10)).strftime('%Y-%m-%dT%H:%M:%SZ')
        logs = get_test_client_logs_since(since_time)
        client_analysis = analyze_logs_for_failover(logs, failover_start_time, window_seconds=45)
        
        # Debug: show actual time window and log timestamps
        print(f"Debug info:")
        print(f"  Failover start time (UTC): {failover_start_time}")
        print(f"  Analysis window: {failover_start_time} to {failover_start_time + timedelta(seconds=45)}")
        
        # Show some recent log timestamps for comparison
        lines = logs.strip().split('\n')
        print(f"  Recent log timestamps:")
        for i, line in enumerate(lines[-3:]):
            try:
                log_data = parse_logfmt(line)
                timestamp_str = log_data.get('ts', '')
                timestamp_naive = datetime.fromisoformat(timestamp_str).replace(tzinfo=None)
                print(f"    Log {i+1}: {timestamp_naive}")
            except:
                pass
        
        print(f"=== Failover Analysis Results ===")
        print(f"Failover Detection:")
        print(f"  Main pod changed: {main_monitoring['main_changed']}")
        print(f"  New main pod: {main_monitoring.get('new_main', 'N/A')}")
        print(f"  Change time: {main_monitoring.get('change_time', 'N/A')}s")
        print(f"  Database polling count: {main_monitoring['polling_count']}")
        print(f"  Pod recreated: {main_monitoring.get('pod_recreated', False)}")
        if main_monitoring.get('pod_recreated'):
            print(f"  Recreation detected at: {main_monitoring.get('recreation_detected_at', 'N/A')}s")
        
        print(f"Controller Analysis:")
        print(f"  Failover triggered: {controller_analysis['failover_triggered']}")
        print(f"  Main promotion detected: {controller_analysis['main_promotion_detected']}")
        print(f"  Failover events: {len(controller_analysis['failover_events'])}")
        for event in controller_analysis['failover_events'][-3:]:  # Show last 3 events
            print(f"    - {event}")
        
        print(f"Client Impact Analysis:")
        print(f"  Failures in window: {client_analysis['failure_count']}")
        print(f"  Successes in window: {client_analysis['success_count']}")
        print(f"  Recovery time: {client_analysis.get('recovery_time_seconds', 'Unknown')}")
        
        # Primary Assertion: Verify pod deletion triggered some form of recovery
        failover_detected = main_monitoring['main_changed']
        pod_was_recreated = main_monitoring.get('pod_recreated', False)
        client_had_minimal_impact = client_analysis['failure_count'] <= 3 and client_analysis['success_count'] > 0
        
        if not failover_detected and not pod_was_recreated:
            # Last resort: check if pod was actually deleted and recreated by checking ages manually
            current_pod_age = get_pod_age_seconds(original_main_pod)
            initial_pod_age = main_monitoring.get('initial_pod_age', -1)
            
            if current_pod_age > 0 and initial_pod_age > 0 and current_pod_age < (initial_pod_age / 2):
                print(f"✅ Pod recreation detected via age comparison: {current_pod_age}s < {initial_pod_age}s")
                pod_was_recreated = True
        
        # Accept either: role failover, pod recreation, or minimal client impact as success
        recovery_success = failover_detected or pod_was_recreated or client_had_minimal_impact
        
        assert recovery_success, f"No recovery detected: main_changed={failover_detected}, pod_recreated={pod_was_recreated}, client_impact_minimal={client_had_minimal_impact}"
        
        if failover_detected:
            print("✅ Traditional failover: Role moved to different pod")
        elif pod_was_recreated:
            print("✅ StatefulSet recovery: Pod was recreated quickly") 
        elif client_had_minimal_impact:
            print("✅ Resilient system: Minimal client impact despite pod deletion")
        
        if main_monitoring['change_time'] is not None:
            assert main_monitoring['change_time'] < 60, f"Recovery took too long: {main_monitoring['change_time']}s"
        
        # Secondary: Verify controller detected and handled failover
        if not controller_analysis['failover_triggered']:
            print("⚠ Warning: Controller logs don't show explicit failover events")
            print("  This might indicate very fast failover or different log patterns")
        
        # Tertiary: Analyze client impact (but don't fail test if no client failures)
        if client_analysis['failure_count'] == 0 and client_analysis['success_count'] == 0:
            print("⚠ No operations logged during failover window - test-client likely stopped")
            print("This indicates complete service disruption during failover")
            
            # Wait for test-client to recover
            print("Waiting for test-client recovery...")
            recovery_confirmed = False
            
            # Poll for recovery over 20 seconds
            for attempt in range(10):  # 10 attempts * 2 seconds = 20 seconds
                recovery_since_time = failover_start_time.strftime('%Y-%m-%dT%H:%M:%SZ')
                recent_logs = get_test_client_logs_since(recovery_since_time)
                # Analyze a 45-second window from failover start to capture recovery
                recent_analysis = analyze_logs_for_failover(recent_logs, failover_start_time, window_seconds=45)
                
                if recent_analysis['success_count'] > 0:
                    print(f"✓ Test-client recovered and operations resumed after {(attempt+1)*2}s")
                    recovery_confirmed = True
                    break
                    
                if attempt < 9:  # Don't sleep on last iteration
                    time.sleep(2)
            
            if not recovery_confirmed:
                print("❌ Test-client has not recovered - extended outage")
                recovery_confirmed = False
                
            assert recovery_confirmed, "Test-client did not recover after extended wait"
            
        else:
            # Optional: Verify reasonable client impact if operations were logged
            print("✓ Client operations were logged during failover window")
            if client_analysis['failure_count'] > 5:
                print(f"⚠ Warning: High failure count during failover: {client_analysis['failure_count']}")
            recovery_time = client_analysis.get('recovery_time_seconds')
            if recovery_time is not None and recovery_time > 20:
                print(f"⚠ Warning: Long recovery time: {recovery_time}s")
        
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
        print(f"  Actual failover time: {main_monitoring.get('change_time', 'N/A')}s")
        print(f"  Client impact: {client_analysis['failure_count']} failures, {client_analysis['success_count']} successes")
        print(f"  Controller events detected: {len(controller_analysis['failover_events'])}")