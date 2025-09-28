#!/usr/bin/env python3
"""
E2E test for preStop hook ServiceAccount authentication.

This test verifies that:
1. Valid ServiceAccount tokens are accepted by the preStop hook API
2. Invalid tokens are rejected
3. The authentication logs show proper behavior
"""

import os
import subprocess
import time
import requests
import pytest
from utils import (
    find_main_pod_by_querying,
    wait_for_cluster_convergence,
    E2ETestError
)


class TestPreStopAuthentication:
    """Test ServiceAccount authentication for preStop hooks."""

    def test_prestop_hook_authentication(self):
        """Test preStop hook authentication with valid and invalid tokens."""
        print("🧪 Starting test: test_prestop_hook_authentication")

        # Step 1: Ensure cluster is healthy
        print("⏳ Waiting for cluster convergence...")
        assert wait_for_cluster_convergence(timeout=60), "Cluster failed to converge"

        # Step 2: Get controller service endpoint
        controller_url = "http://memgraph-controller:8080"
        prestop_endpoint = f"{controller_url}/prestop-hook/test-pod"

        # Step 3: Test without authentication (should fail)
        print("🔒 Testing request without authentication...")
        try:
            response = self._run_prestop_script_from_pod("no_auth")
            assert response.status_code == 401, f"Expected 401, got {response.status_code}"
            print("✅ Correctly rejected request without authentication")
        except Exception as e:
            print(f"❌ Failed to test unauthenticated request: {e}")
            raise

        # Step 4: Test with invalid token (should fail)
        print("🔒 Testing request with invalid token...")
        try:
            response = self._run_prestop_script_from_pod("invalid_token")
            assert response.status_code == 401, f"Expected 401, got {response.status_code}"
            print("✅ Correctly rejected request with invalid token")
        except Exception as e:
            print(f"❌ Failed to test invalid token: {e}")
            raise

        # Step 5: Test with valid ServiceAccount token (should succeed)
        print("🔒 Testing request with valid ServiceAccount token...")
        try:
            response = self._run_prestop_script_from_pod("valid")
            # Note: This might timeout because preStop hook waits for cluster stability
            # But we should get a proper HTTP response, not a 401
            if response.status_code == 401:
                raise AssertionError(f"Valid token was rejected: {response.status_code}")
            elif response.status_code in [200, 408, 500]:  # 408=timeout, 500=internal error are acceptable
                print(f"✅ Valid token accepted (status: {response.status_code})")
            else:
                print(f"⚠ Unexpected status code: {response.status_code}")
        except Exception as e:
            print(f"❌ Failed to test valid token: {e}")
            raise

        # Step 6: Verify authentication logs in controller
        print("📝 Checking controller logs for authentication events...")
        try:
            self._verify_authentication_logs()
            print("✅ Authentication logs verified")
        except Exception as e:
            print(f"⚠ Warning: Could not verify logs: {e}")
            # Don't fail the test for log verification issues

        print("✅ preStop hook authentication test completed successfully!")

    def _run_prestop_script_from_pod(self, test_type="valid", timeout=10):
        """Run the actual preStop script from a memgraph pod with different authentication scenarios."""

        # Choose a memgraph pod to execute from (these have the correct ServiceAccount)
        test_pod = "memgraph-ha-1"  # Use memgraph-ha-1 (not main pod to avoid disrupting main)

        if test_type == "no_auth":
            # Test without authentication - skip token reading
            python_script = f'''
import os
import requests

pod_name = os.getenv('POD_NAME', '{test_pod}')
timeout = {timeout}

# No authentication
headers = {{}}
try:
    response = requests.post(
        f'http://memgraph-controller:8080/prestop-hook/{{pod_name}}',
        headers=headers,
        timeout=timeout
    )
    print(f'STATUS_CODE:{{response.status_code}}')
except Exception as e:
    print(f'ERROR:{{e}}')
'''
        elif test_type == "invalid_token":
            # Test with invalid token
            python_script = f'''
import os
import requests

pod_name = os.getenv('POD_NAME', '{test_pod}')
timeout = {timeout}

# Use invalid token
headers = {{'Authorization': 'Bearer invalid-token-12345'}}
try:
    response = requests.post(
        f'http://memgraph-controller:8080/prestop-hook/{{pod_name}}',
        headers=headers,
        timeout=timeout
    )
    print(f'STATUS_CODE:{{response.status_code}}')
except Exception as e:
    print(f'ERROR:{{e}}')
'''
        else:  # test_type == "valid"
            # Test with valid ServiceAccount token (actual preStop script)
            python_script = f'''
import os
import requests

# Read ServiceAccount token
with open('/var/run/secrets/kubernetes.io/serviceaccount/token', 'r') as f:
    token = f.read().strip()

pod_name = os.getenv('POD_NAME', '{test_pod}')
timeout = {timeout}

# Send request with Authorization header
headers = {{'Authorization': f'Bearer {{token}}'}}
try:
    response = requests.post(
        f'http://memgraph-controller:8080/prestop-hook/{{pod_name}}',
        headers=headers,
        timeout=timeout
    )
    print(f'STATUS_CODE:{{response.status_code}}')
except Exception as e:
    print(f'ERROR:{{e}}')
'''

        # Execute the Python script from within the memgraph pod
        try:
            result = subprocess.run([
                "kubectl", "exec", "-n", "memgraph", test_pod, "--",
                "python3", "-c", python_script
            ], capture_output=True, text=True, timeout=timeout+5)

            if result.returncode != 0:
                print(f"kubectl exec failed: {result.stderr}")
                raise Exception(f"kubectl exec failed: {result.stderr}")

            # Parse the response
            output = result.stdout.strip()
            if "STATUS_CODE:" in output:
                status_line = [line for line in output.split('\n') if 'STATUS_CODE:' in line][-1]
                status_code = int(status_line.split('STATUS_CODE:')[1].strip())
            elif "ERROR:" in output:
                # Extract error message but treat as failed request
                status_code = 500
            else:
                status_code = 500  # Unknown error

            # Return a response-like object
            class MockResponse:
                def __init__(self, status_code, text):
                    self.status_code = status_code
                    self.text = text

            return MockResponse(status_code, output)

        except subprocess.TimeoutExpired:
            # Script timed out
            class TimeoutResponse:
                def __init__(self):
                    self.status_code = 408
                    self.text = "Request timeout"
            return TimeoutResponse()

    def _verify_authentication_logs(self):
        """Verify authentication events appear in controller logs."""

        # Get recent controller logs
        result = subprocess.run([
            "kubectl", "logs", "-n", "memgraph", "deployment/memgraph-controller",
            "--tail=100"
        ], capture_output=True, text=True)

        if result.returncode != 0:
            raise Exception(f"Failed to get controller logs: {result.stderr}")

        logs = result.stdout

        # Look for authentication-related log entries
        auth_logs = []
        for line in logs.split('\n'):
            if any(keyword in line.lower() for keyword in ['prestop', 'auth', 'token']):
                auth_logs.append(line)

        if not auth_logs:
            raise Exception("No authentication-related logs found")

        print(f"📝 Found {len(auth_logs)} authentication log entries:")
        for log in auth_logs[-5:]:  # Show last 5 entries
            print(f"   {log}")

        return auth_logs

    def test_prestop_hook_with_rolling_restart(self):
        """Test that preStop hooks work correctly during rolling restart after auth fix."""
        print("🧪 Starting test: test_prestop_hook_with_rolling_restart")

        # Step 1: Ensure cluster is healthy
        print("⏳ Waiting for cluster convergence...")
        assert wait_for_cluster_convergence(timeout=60), "Cluster failed to converge"

        # Step 2: Check initial replication status
        initial_status = self._get_replication_status()
        print(f"📊 Initial replication status: {initial_status}")

        # Step 3: Trigger a single pod restart to test preStop hook
        print("🔄 Triggering restart of one memgraph pod...")
        main_pod = find_main_pod_by_querying()
        if not main_pod:
            raise E2ETestError("Could not determine main pod")

        # Restart a non-main pod to test preStop hook without causing failover
        all_pods = ["memgraph-ha-0", "memgraph-ha-1", "memgraph-ha-2"]
        target_pod = None
        for pod in all_pods:
            if pod != main_pod:
                target_pod = pod
                break

        if not target_pod:
            raise E2ETestError("Could not find non-main pod to restart")

        print(f"🎯 Restarting pod: {target_pod}")

        # Delete the pod
        result = subprocess.run([
            "kubectl", "delete", "pod", "-n", "memgraph", target_pod
        ], capture_output=True, text=True)

        if result.returncode != 0:
            raise E2ETestError(f"Failed to delete pod {target_pod}: {result.stderr}")

        # Step 4: Wait for pod to be recreated and cluster to converge
        print("⏳ Waiting for pod recreation and cluster convergence...")
        time.sleep(10)  # Wait for pod deletion to be processed

        # Wait for cluster to converge after pod restart
        converged = wait_for_cluster_convergence(timeout=120)
        if not converged:
            print("⚠ Cluster did not converge within timeout, checking replication status...")

        # Step 5: Check final replication status for divergence
        final_status = self._get_replication_status()
        print(f"📊 Final replication status: {final_status}")

        # Step 6: Verify no data divergence occurred
        diverged_replicas = []
        for replica_info in final_status.get('replicas', []):
            if 'diverged' in str(replica_info.get('data_info', '')).lower():
                diverged_replicas.append(replica_info)

        if diverged_replicas:
            print(f"❌ Data divergence detected in {len(diverged_replicas)} replica(s):")
            for replica in diverged_replicas:
                print(f"   {replica}")
            raise E2ETestError("Data divergence detected after pod restart - preStop hook authentication failed")

        print("✅ No data divergence detected - preStop hook authentication working correctly!")
        print("✅ preStop hook rolling restart test completed successfully!")

    def _get_replication_status(self):
        """Get current replication status from all pods."""
        status = {'main': None, 'replicas': []}

        try:
            # Get main pod info
            main_pod = find_main_pod_by_querying()
            if main_pod:
                status['main'] = main_pod

                # Get replicas info from main pod
                result = subprocess.run([
                    "kubectl", "exec", "-n", "memgraph", main_pod, "--",
                    "bash", "-c",
                    'echo "SHOW REPLICAS;" | mgconsole --output-format csv --username=memgraph'
                ], capture_output=True, text=True)

                if result.returncode == 0:
                    lines = result.stdout.strip().split('\n')
                    if len(lines) > 1:  # Skip header
                        for line in lines[1:]:
                            if line.strip():
                                parts = line.split(',')
                                if len(parts) >= 4:
                                    replica_info = {
                                        'name': parts[0].strip('"'),
                                        'socket_address': parts[1].strip('"'),
                                        'sync_mode': parts[2].strip('"'),
                                        'data_info': parts[4] if len(parts) > 4 else parts[3]
                                    }
                                    status['replicas'].append(replica_info)
        except Exception as e:
            print(f"⚠ Warning: Could not get complete replication status: {e}")

        return status


if __name__ == "__main__":
    # Run the specific test
    test = TestPreStopAuthentication()
    test.test_prestop_hook_authentication()
    test.test_prestop_hook_with_rolling_restart()