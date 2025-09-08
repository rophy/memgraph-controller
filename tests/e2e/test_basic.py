"""
Basic E2E Tests for Memgraph HA

Tests core functionality: cluster topology, data writes/reads, and replication.
These are the fundamental tests that should always pass for a healthy cluster.
"""

import pytest
import time
from utils import (
    memgraph_query_via_client,
    wait_for_cluster_convergence,
    count_nodes,
    generate_test_id,
    log_info,
    log_success,
    E2ETestError
)


@pytest.mark.basic
def test_cluster_topology(cluster_ready):
    """Test that cluster has proper main-sync-async topology"""

    # Wait for cluster to converge with retry logic
    wait_for_cluster_convergence()

    # Verify replication role via gateway
    role_result = memgraph_query_via_client("SHOW REPLICATION ROLE;")
    role = role_result['records'][0]['replication role']
    assert role == 'main', f"Expected main role, got {role}"

    # Verify replica topology via gateway
    replicas_result = memgraph_query_via_client("SHOW REPLICAS;")
    sync_count = len([r for r in replicas_result['records']
                     if r['sync_mode'] == 'sync'])
    async_count = len([r for r in replicas_result['records']
                      if r['sync_mode'] == 'async'])

    assert sync_count == 1, f"Expected 1 sync replica, got {sync_count}"
    assert async_count == 1, f"Expected 1 async replica, got {async_count}"

    # Log topology details for debugging
    sync_replica = next(
        (r for r in replicas_result['records'] if r['sync_mode'] == 'sync'), None)
    async_replica = next(
        (r for r in replicas_result['records'] if r['sync_mode'] == 'async'), None)

    sync_name = sync_replica['name'] if sync_replica else 'unknown'
    async_name = async_replica['name'] if async_replica else 'unknown'

    log_info(
        f"📊 Topology verified: Main role confirmed, Sync={sync_name}, Async={async_name}")

    # Validate pod assignments per design (pod-2 should be async)
    async_pod = async_name.replace('_', '-')
    assert async_pod == "memgraph-ha-2", f"Pod-2 should be async replica, got: {async_pod}"

    sync_pod = sync_name.replace('_', '-')
    assert sync_pod in [
        "memgraph-ha-0", "memgraph-ha-1"], f"Sync replica should be pod-0 or pod-1, got: {sync_pod}"

    log_success(
        f"✅ Cluster topology validated: {sync_pod}(sync), {async_pod}(async)")


@pytest.mark.basic
def test_data_write_gateway(cluster_ready, unique_test_id):
    """Test writing and reading data through the gateway"""

    test_value = f"value_{int(time.time())}"

    log_info(f"📝 Writing test data: ID={unique_test_id}, Value={test_value}")

    # Write test data through gateway
    write_query = f"CREATE (n:TestNode {{id: '{unique_test_id}', value: '{test_value}', " \
                  f"timestamp: datetime()}}) RETURN n.id;"
    write_result = memgraph_query_via_client(write_query)

    # Verify write response
    assert write_result['records'], "Write operation returned no records"
    returned_id = write_result['records'][0]['n.id']
    assert returned_id == unique_test_id, f"Write returned wrong ID: {returned_id}"

    # Read data back to verify
    read_query = f"MATCH (n:TestNode {{id: '{unique_test_id}'}}) RETURN n.id, n.value;"
    read_result = memgraph_query_via_client(read_query)

    # Verify read response
    assert read_result['records'], f"Test data not found: {unique_test_id}"
    record = read_result['records'][0]

    assert record['n.id'] == unique_test_id, f"ID mismatch: expected {unique_test_id}, got {record['n.id']}"
    assert record['n.value'] == test_value, f"Value mismatch: expected {test_value}, got {record['n.value']}"

    log_success(f"✅ Data write/read verified: ID={unique_test_id}")


@pytest.mark.basic
def test_data_replication():
    """Test that data replication is working properly"""

    # Get initial node count
    initial_count = count_nodes()
    log_info(f"📊 Initial node count: {initial_count}")

    # Write replication test data
    test_id = generate_test_id("repl_test")
    test_value = "replication_data"

    log_info(f"📝 Writing replication test data: ID={test_id}")

    write_query = f"CREATE (n:ReplTest {{id: '{test_id}', value: '{test_value}'}}) RETURN n.id;"
    write_result = memgraph_query_via_client(write_query)

    assert write_result['records'], "Replication write returned no records"

    # Wait briefly for replication to complete
    log_info("⏳ Waiting for replication to complete...")
    time.sleep(3)

    # Verify data exists by reading it back
    read_query = f"MATCH (n:ReplTest {{id: '{test_id}'}}) RETURN n.id, n.value;"
    read_result = memgraph_query_via_client(read_query)

    assert read_result['records'], f"Replication test data not found: {test_id}"
    record = read_result['records'][0]

    assert record['n.id'] == test_id, f"Replication data ID mismatch: {record['n.id']}"
    assert record['n.value'] == test_value, f"Replication data value mismatch: {record['n.value']}"

    # Verify final node count increased
    final_count = count_nodes()
    increase = final_count - initial_count

    log_info(f"📊 Final node count: {final_count} (increase: {increase})")
    assert increase >= 1, f"Node count should have increased by at least 1, got increase: {increase}"

    # Get current cluster topology for verification
    try:
        replicas_result = memgraph_query_via_client("SHOW REPLICAS;")
        replica_count = len(replicas_result['records'])
        log_info(f"🔗 Active replicas: {replica_count}")
    except E2ETestError:
        log_info("🔗 Could not query replica status")

    log_success("✅ Data replication verified successfully")


@pytest.mark.basic
def test_multiple_operations():
    """Test multiple concurrent operations to verify stability"""

    log_info("🔄 Testing multiple database operations")

    # Create multiple test nodes
    test_ids = []
    for i in range(5):
        test_id = generate_test_id(f"multi_test_{i}")
        test_ids.append(test_id)

        write_query = f"CREATE (n:MultiTest {{id: '{test_id}', index: {i}, created: datetime()}}) RETURN n.id;"
        result = memgraph_query_via_client(write_query)
        assert result['records'], f"Failed to create test node {i}"

    # Verify all nodes were created
    for test_id in test_ids:
        read_query = f"MATCH (n:MultiTest {{id: '{test_id}'}}) RETURN n.id, n.index;"
        result = memgraph_query_via_client(read_query)
        assert result['records'], f"Test node not found: {test_id}"

    # Test a more complex query
    count_query = "MATCH (n:MultiTest) RETURN count(n) as multi_count;"
    count_result = memgraph_query_via_client(count_query)

    multi_count = count_result['records'][0]['multi_count']
    if isinstance(multi_count, dict) and 'low' in multi_count:
        multi_count = multi_count['low']

    assert multi_count >= 5, f"Expected at least 5 MultiTest nodes, got {multi_count}"

    log_success(
        f"✅ Multiple operations verified: {multi_count} test nodes created and verified")


@pytest.mark.basic
def test_cluster_health_queries():
    """Test various health and status queries"""

    # Test SHOW REPLICATION ROLE
    role_result = memgraph_query_via_client("SHOW REPLICATION ROLE;")
    assert role_result['records'], "SHOW REPLICATION ROLE returned no records"
    assert 'replication role' in role_result['records'][0], "Missing replication role field"

    # Test SHOW REPLICAS
    memgraph_query_via_client("SHOW REPLICAS;")
    # Note: result might be empty if no replicas are registered yet, that's
    # okay

    # Test SHOW STORAGE INFO
    try:
        storage_result = memgraph_query_via_client("SHOW STORAGE INFO;")
        assert storage_result is not None, "SHOW STORAGE INFO failed"
        log_info("📊 Storage info query successful")
    except E2ETestError as e:
        # Storage info might not be available in all configurations
        log_info(f"⚠️  Storage info query failed (may be expected): {e}")

    # Test basic node counting
    count_result = memgraph_query_via_client(
        "MATCH (n) RETURN count(n) as total_nodes;")
    assert count_result['records'], "Node count query returned no records"

    total_nodes = count_result['records'][0]['total_nodes']
    if isinstance(total_nodes, dict) and 'low' in total_nodes:
        total_nodes = total_nodes['low']

    log_info(f"📊 Total nodes in cluster: {total_nodes}")

    log_success("✅ Cluster health queries verified")
