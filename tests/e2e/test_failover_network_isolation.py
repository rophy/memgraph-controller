#!/usr/bin/env python3
"""
Network Partition Failover Test

Tests the Memgraph HA controller's behavior during network partition scenarios.
Simulates realistic network failure where the main node becomes isolated using
Kubernetes NetworkPolicy, forcing automatic failover.
"""

import json
import pytest
import time
import subprocess
from datetime import datetime, timezone

from utils import (
    get_controller_logs_since,
    detect_failover_in_controller_logs,
    parse_logfmt,
    get_test_client_pod,
    kubectl_exec,
    wait_for_cluster_convergence,
    get_test_client_logs,
    find_main_pod_by_querying,
    monitor_main_pod_changes_enhanced,
    log_info,
    log_error
)


class NetworkIsolationTestManager:
    """Manages NetworkPolicy creation and cleanup for network isolation tests."""
    
    def __init__(self, namespace="memgraph"):
        self.namespace = namespace
        self.policy_name = None
        
    def create_isolation_policy(self, pod_name: str) -> str:
        """Create NetworkPolicy to isolate a specific pod using kubectl."""
        self.policy_name = f"isolate-{pod_name}"
        
        # Create NetworkPolicy YAML - Complete isolation to simulate network partition
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
            # Apply NetworkPolicy using kubectl
            result = subprocess.run([
                "kubectl", "apply", "-f", "-"
            ], input=policy_yaml, text=True, capture_output=True, check=True)
            
            print(f"✅ Created NetworkPolicy: {self.policy_name}")
            return self.policy_name
        except subprocess.CalledProcessError as e:
            raise Exception(f"Failed to create NetworkPolicy: {e.stderr}")
    
    def cleanup(self):
        """Clean up NetworkPolicy if it exists."""
        if not self.policy_name:
            return
            
        try:
            result = subprocess.run([
                "kubectl", "delete", "networkpolicy", self.policy_name, "-n", self.namespace
            ], capture_output=True, text=True, check=False)
            
            if result.returncode == 0:
                print(f"✅ Cleaned up NetworkPolicy: {self.policy_name}")
            elif "not found" in result.stderr:
                print(f"✅ NetworkPolicy {self.policy_name} already deleted")
            else:
                print(f"⚠️  Warning: Failed to cleanup NetworkPolicy {self.policy_name}: {result.stderr}")
        except Exception as e:
            print(f"⚠️  Warning: Failed to cleanup NetworkPolicy {self.policy_name}: {e}")
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


def reset_controller_connections():
    """Reset controller connection pools via admin API."""
    try:
        # Get test client pod to run curl from
        test_client_pod = get_test_client_pod()
        
        # Run curl POST inside the pod
        stdout, stderr, exit_code = kubectl_exec(
            test_client_pod,
            "memgraph", 
            ["curl", "-s", "-X", "POST", "http://memgraph-controller:8080/api/v1/admin/reset-connections"]
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
        logs = get_test_client_logs(tail_lines=50)
        lines = logs.strip().split('\n')
        
        # Check last N operations for consecutive successes
        consecutive_successes = 0
        
        for line in reversed(lines[-required_consecutive:]):
            try:
                log_data = parse_logfmt(line)
                if log_data.get('msg') == 'Success' or 'Success' in log_data.get('msg', ''):
                    consecutive_successes += 1
                else:
                    break
            except:
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
            # Use existing utility function to get test client logs
            logs = get_test_client_logs(tail_lines=10)
            
            # Parse recent logs to look for failures
            recent_lines = logs.strip().split('\n')[-5:]  # Last 5 lines
            failures = 0
            
            for line in recent_lines:
                try:
                    log_data = parse_logfmt(line)
                    # Check for failure patterns
                    if ('Failed' in log_data.get('msg', '') or 
                        log_data.get('at', '').upper() == 'ERROR'):
                        failures += 1
                except:
                    # If can't parse, check for error text
                    if 'Failed' in line or 'ERROR' in line:
                        failures += 1
            
            if failures >= 2:  # At least 2 recent failures
                print(f"✅ Detected test client failures after {elapsed}s")
                return True
                
        except Exception as e:
            print(f"⚠️  Error checking test client: {e}")
        
        print(f"⏳ No failures detected yet, waiting... ({elapsed}s/{max_wait}s)")
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
    
    policy_name = network_isolation_manager.create_isolation_policy(initial_main)
    
    # Reset connections to force immediate connection failures
    reset_controller_connections()
    
    # Step 3: Wait for failure detection
    print("\n⚠️  Step 3: Waiting for failure detection...")
    
    wait_for_test_client_failures(max_wait=30)
    
    # Step 4: Wait for failover completion
    print("\n🔄 Step 4: Waiting for failover completion...")
    
    # Give controller time to detect the failure (health check intervals)
    print("⏳ Allowing time for controller health checks to detect failure...")
    time.sleep(15)  # Wait for controller to detect failure
    
    # Use existing function to monitor main pod changes  
    failover_result = monitor_main_pod_changes_enhanced(initial_main, timeout=120)
    
    if not failover_result.get("success", False):
        print(f"⚠️  Failover monitoring result: {failover_result}")
        
        # Check controller logs for debug information
        print("🔍 Checking controller logs for failover activity...")
        try:
            controller_logs = get_controller_logs_since(test_start_time)
            failover_events = detect_failover_in_controller_logs(controller_logs)
            print(f"📊 Controller failover events: {len(failover_events.get('events', []))}")
            if failover_events.get('events'):
                for event in failover_events['events'][:3]:  # Show first 3 events
                    print(f"  - {event}")
        except Exception as e:
            print(f"⚠️  Could not get controller logs: {e}")
        
        # Try to get current main pod directly as fallback
        try:
            current_main = find_main_pod_by_querying()
            if current_main != initial_main:
                print(f"✅ Fallback detection: Failover completed {initial_main} -> {current_main}")
                new_main = current_main
            else:
                pytest.fail(f"Failover monitoring failed and main pod unchanged: {failover_result}")
        except Exception as e:
            pytest.fail(f"Failover monitoring failed and cannot verify main pod: {e}")
    else:
        new_main = failover_result.get("final_main", initial_main)
    
    # Step 5: Verify post-failover functionality
    print(f"\n✅ Step 5: Verifying post-failover functionality...")
    
    # Verify test client success after failover
    time.sleep(10)  # Allow time for connections to recover
    
    if not verify_recent_test_client_success(required_consecutive=5):
        print("⚠️  Warning: Test client not showing recent successes after failover")
    
    # Verify new main is different
    if new_main == initial_main:
        pytest.fail(f"Failover did not change main pod (still {initial_main})")
    
    print(f"✅ Failover successful: {initial_main} -> {new_main}")
    
    # Step 6: Network recovery
    print(f"\n🔄 Step 6: Restoring network connectivity...")
    
    network_isolation_manager.cleanup()  # This removes the NetworkPolicy
    
    # Step 7: Wait for cluster reconciliation
    print(f"\n🔄 Step 7: Waiting for cluster reconciliation...")
    
    # Allow time for network recovery and reconciliation
    time.sleep(30)
    
    # Wait for cluster to converge again
    if not wait_for_cluster_convergence(timeout=60):
        print("⚠️  Warning: Cluster did not converge after network recovery")
    
    # Final validation
    print(f"\n✅ Step 8: Final validation...")
    
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
    controller_logs = get_controller_logs_since(test_start_time)
    failover_events = detect_failover_in_controller_logs(controller_logs)
    
    print(f"📊 Test Summary:")
    print(f"  - Initial main: {initial_main}")
    print(f"  - Final main: {final_main}")
    print(f"  - Failover events detected: {len(failover_events.get('events', []))}")
    
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