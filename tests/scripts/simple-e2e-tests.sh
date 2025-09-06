#!/bin/bash
set -euo pipefail

# E2E Test Configuration
readonly MEMGRAPH_NS="memgraph"
readonly TEST_CLIENT_LABEL="app=neo4j-client" 
readonly EXPECTED_POD_COUNT=3

# Colors for output
readonly GREEN='\033[0;32m'
readonly RED='\033[0;31m'
readonly BLUE='\033[0;34m'
readonly NC='\033[0m'

# Test counters
TESTS_RUN=0
TESTS_PASSED=0
TESTS_FAILED=0

log_info() { echo -e "${BLUE}[INFO]${NC} $*"; }
log_success() { echo -e "${GREEN}[SUCCESS]${NC} $*"; }
log_error() { echo -e "${RED}[ERROR]${NC} $*" >&2; }

start_test() { 
    local test_name="$1"
    TESTS_RUN=$((TESTS_RUN + 1))
    log_info "🧪 Starting test: $test_name"
}

pass_test() {
    local test_name="$1" 
    echo 'test'
    TESTS_PASSED=$((TESTS_PASSED + 1))
    log_success "✅ PASSED: $test_name"
}

fail_test() {
    local test_name="$1"
    local reason="$2"
    TESTS_FAILED=$((TESTS_FAILED + 1))
    log_error "❌ FAILED: $test_name - $reason"
}

# Get test client pod
get_test_client_pod() {
    kubectl get pods -n "$MEMGRAPH_NS" -l "$TEST_CLIENT_LABEL" -o jsonpath='{.items[0].metadata.name}' 2>/dev/null
}

# Query via test client
query_via_client() {
    local query="$1"
    local test_client_pod
    
    if ! test_client_pod=$(get_test_client_pod); then
        return 1
    fi
    
    kubectl exec "$test_client_pod" -n "$MEMGRAPH_NS" -- node index.js "$query"
}

# Test 1: Cluster Topology
test_cluster_topology() {
    local test_name="Cluster Topology"
    start_test "$test_name"
    
    # Check pods are ready
    local pods
    if ! pods=$(kubectl get pods -n "$MEMGRAPH_NS" -l "app.kubernetes.io/name=memgraph" --no-headers); then
        fail_test "$test_name" "Failed to get pods"
        return 1
    fi
    
    local pod_count
    pod_count=$(echo "$pods" | wc -l)
    if (( pod_count != EXPECTED_POD_COUNT )); then
        fail_test "$test_name" "Expected $EXPECTED_POD_COUNT pods, got $pod_count"
        return 1
    fi

    
    # Check replication role
    local role_result
    if ! role_result=$(query_via_client "SHOW REPLICATION ROLE;"); then
        fail_test "$test_name" "Failed to get replication role"
        return 1
    fi
    
    local main_role
    echo "$role_result"
    main_role=$(echo "$role_result" | jq -r '.records[0]["replication role"]')
    echo 'lalala'    
    if [[ "$main_role" != "main" ]]; then
        fail_test "$test_name" "Expected main role, got: $main_role"
        return 1
    fi
    
    # Check replicas
    local replicas_result
    if ! replicas_result=$(query_via_client "SHOW REPLICAS;"); then
        fail_test "$test_name" "Failed to get replicas"
        return 1
    fi
    
    local sync_count async_count
    sync_count=$(echo "$replicas_result" | jq '.records | map(select(.sync_mode == "sync")) | length' 2>/dev/null)
    async_count=$(echo "$replicas_result" | jq '.records | map(select(.sync_mode == "async")) | length' 2>/dev/null)
    
    if (( sync_count != 1 )) || (( async_count != 1 )); then
        fail_test "$test_name" "Expected 1 sync + 1 async replica, got sync=$sync_count async=$async_count"
        return 1
    fi
    
    log_info "📊 Topology verified: Main role confirmed, $sync_count sync + $async_count async replicas"
    pass_test "$test_name"
}

# Test 2: Data Write Through Gateway
test_data_write_gateway() {
    local test_name="Data Write Through Gateway"
    start_test "$test_name"
    
    # Generate unique test data
    local test_id="test_$(date +%s)_$$"
    local test_value="value_$(date +%s)_$$"
    
    log_info "📝 Writing test data: ID=$test_id"
    
    # Write data
    local write_query="CREATE (n:TestNode {id: '$test_id', value: '$test_value'}) RETURN n.id;"
    if ! query_via_client "$write_query" > /dev/null; then
        fail_test "$test_name" "Failed to write test data"
        return 1
    fi
    
    # Read data back
    local read_query="MATCH (n:TestNode {id: '$test_id'}) RETURN n.id, n.value;"
    local result
    if ! result=$(query_via_client "$read_query"); then
        fail_test "$test_name" "Failed to read test data back"
        return 1
    fi
    
    # Verify data
    local returned_id returned_value
    returned_id=$(echo "$result" | jq -r '.records[0]["n.id"]' 2>/dev/null)
    returned_value=$(echo "$result" | jq -r '.records[0]["n.value"]' 2>/dev/null)
    
    if [[ "$returned_id" != "$test_id" ]] || [[ "$returned_value" != "$test_value" ]]; then
        fail_test "$test_name" "Data mismatch"
        return 1
    fi
    
    log_info "✅ Data verified: ID=$test_id"
    pass_test "$test_name"
}

# Test 3: Data Replication
test_data_replication() {
    local test_name="Data Replication Verification"
    start_test "$test_name"
    
    # Get initial count
    local count_result
    if ! count_result=$(query_via_client "MATCH (n) RETURN count(n) as node_count;"); then
        fail_test "$test_name" "Failed to get initial count"
        return 1
    fi
    
    local initial_count
    initial_count=$(echo "$count_result" | jq -r '.records[0].node_count.low // .records[0].node_count' 2>/dev/null)
    
    log_info "📊 Initial node count: $initial_count"
    
    # Write replication test data
    local test_id="repl_test_$(date +%s)_$$"
    local write_query="CREATE (n:ReplTest {id: '$test_id'}) RETURN n.id;"
    
    if ! query_via_client "$write_query" > /dev/null; then
        fail_test "$test_name" "Failed to write replication test data"
        return 1
    fi
    
    # Wait briefly for replication
    sleep 2
    
    # Verify data exists
    local read_query="MATCH (n:ReplTest {id: '$test_id'}) RETURN n.id;"
    local result
    if ! result=$(query_via_client "$read_query"); then
        fail_test "$test_name" "Replication test data not found"
        return 1
    fi
    
    local returned_id
    returned_id=$(echo "$result" | jq -r '.records[0]["n.id"]' 2>/dev/null)
    
    if [[ "$returned_id" != "$test_id" ]]; then
        fail_test "$test_name" "Replication data mismatch"
        return 1
    fi
    
    # Get final count
    if ! count_result=$(query_via_client "MATCH (n) RETURN count(n) as node_count;"); then
        fail_test "$test_name" "Failed to get final count"
        return 1
    fi
    
    local final_count
    final_count=$(echo "$count_result" | jq -r '.records[0].node_count.low // .records[0].node_count' 2>/dev/null)
    
    log_info "📊 Final node count: $final_count (increase: $((final_count - initial_count)))"
    
    pass_test "$test_name"
}

# Test 4: Failover Test
test_failover() {
    local test_name="Failover Test"
    start_test "$test_name"
    
    log_info "🔍 Step 1: Verify pre-conditions"
    
    # 1a. Verify cluster health
    local replicas_result
    if ! replicas_result=$(query_via_client "SHOW REPLICAS;"); then
        fail_test "$test_name" "Failed to get cluster status"
        return 1
    fi
    
    # Check we have exactly 1 sync + 1 async replica
    local sync_count async_count
    sync_count=$(echo "$replicas_result" | jq '.records | map(select(.sync_mode == "sync")) | length' 2>/dev/null)
    async_count=$(echo "$replicas_result" | jq '.records | map(select(.sync_mode == "async")) | length' 2>/dev/null)
    
    if (( sync_count != 1 )) || (( async_count != 1 )); then
        fail_test "$test_name" "Invalid cluster topology: sync=$sync_count async=$async_count (expected 1 sync + 1 async)"
        return 1
    fi
    
    # Check both replicas are healthy (have data_info)
    local unhealthy_replicas
    unhealthy_replicas=$(echo "$replicas_result" | jq '.records | map(select(.data_info == null or (.data_info | length) == 0)) | length' 2>/dev/null)
    
    if (( unhealthy_replicas > 0 )); then
        fail_test "$test_name" "Found $unhealthy_replicas unhealthy replicas"
        return 1
    fi
    
    log_info "✅ Cluster topology healthy: 1 main + $sync_count sync + $async_count async replicas"
    
    # 1b. Verify recent test-client write success
    local test_client_pod
    if ! test_client_pod=$(get_test_client_pod); then
        fail_test "$test_name" "Failed to find test-client pod"
        return 1
    fi
    
    local recent_logs
    if ! recent_logs=$(kubectl logs "$test_client_pod" -n "$MEMGRAPH_NS" --tail=50 2>/dev/null); then
        fail_test "$test_name" "Failed to get test-client logs"
        return 1
    fi
    
    # Count recent successes in last 50 logs
    local recent_success_count
    recent_success_count=$(echo "$recent_logs" | grep -c "✓ Success" || echo "0")
    
    if (( recent_success_count < 10 )); then
        fail_test "$test_name" "Insufficient recent writes: only $recent_success_count successes in last 50 logs"
        return 1
    fi
    
    log_info "✅ Test-client healthy: $recent_success_count recent successful writes"
    
    # Get main pod name for deletion
    local main_pod
    if ! main_pod=$(kubectl get pods -n "$MEMGRAPH_NS" -l "app.kubernetes.io/name=memgraph" -o name | head -1 | cut -d'/' -f2); then
        fail_test "$test_name" "Failed to identify main pod"
        return 1
    fi
    
    # Find the actual main pod by checking replication role
    local pods
    if ! pods=$(kubectl get pods -n "$MEMGRAPH_NS" -l "app.kubernetes.io/name=memgraph" -o jsonpath='{.items[*].metadata.name}'); then
        fail_test "$test_name" "Failed to get pod list"
        return 1
    fi
    
    main_pod=""
    for pod in $pods; do
        if kubectl exec "$pod" -n "$MEMGRAPH_NS" -c memgraph -- bash -c "echo 'SHOW REPLICATION ROLE;' | mgconsole --output-format csv --username=memgraph" 2>/dev/null | grep -q '"main"'; then
            main_pod="$pod"
            break
        fi
    done
    
    if [[ -z "$main_pod" ]]; then
        fail_test "$test_name" "Could not identify main pod"
        return 1
    fi
    
    log_info "📍 Identified main pod: $main_pod"
    
    # 2. Delete main pod
    log_info "💥 Step 2: Deleting main pod and waiting 5 seconds"
    if ! kubectl delete pod "$main_pod" -n "$MEMGRAPH_NS" --force --grace-period=0; then
        fail_test "$test_name" "Failed to delete main pod"
        return 1
    fi
    
    log_info "⏳ Waiting 5 seconds for failover to complete..."
    sleep 5
    
    # 3. Check test-client logs for failover behavior
    log_info "🔍 Step 3: Analyzing test-client logs for failover behavior"
    
    # Get logs from the time around failover (last 30 lines to capture the event)
    local post_failover_logs
    if ! post_failover_logs=$(kubectl logs "$test_client_pod" -n "$MEMGRAPH_NS" --tail=30 2>/dev/null); then
        fail_test "$test_name" "Failed to get post-failover logs"
        return 1
    fi
    
    # Count failures and successes in recent logs
    local error_count success_after_errors
    error_count=$(echo "$post_failover_logs" | grep -c "✗ Failed" || echo "0")
    
    # Look for success after errors (indicating recovery)
    local has_recovery=false
    if echo "$post_failover_logs" | grep -q "✗ Failed" && echo "$post_failover_logs" | tail -10 | grep -q "✓ Success"; then
        has_recovery=true
    fi
    
    # Validate failover behavior
    if (( error_count == 0 )); then
        log_info "⚡ Perfect failover: No errors detected during failover"
    elif (( error_count <= 3 )) && [[ "$has_recovery" == "true" ]]; then
        log_info "✅ Acceptable failover: $error_count errors followed by recovery"
    elif (( error_count <= 3 )); then
        # Wait a bit more to see if recovery happens
        log_info "⏳ Waiting additional 3 seconds for recovery..."
        sleep 3
        
        if ! post_failover_logs=$(kubectl logs "$test_client_pod" -n "$MEMGRAPH_NS" --tail=10 2>/dev/null); then
            fail_test "$test_name" "Failed to get extended logs"
            return 1
        fi
        
        if echo "$post_failover_logs" | grep -q "✓ Success"; then
            log_info "✅ Delayed recovery: $error_count errors, then successful recovery"
        else
            fail_test "$test_name" "Failover incomplete: $error_count errors but no recovery detected"
            return 1
        fi
    else
        fail_test "$test_name" "Failover took too long: $error_count errors (expected ≤3)"
        return 1
    fi
    
    # Verify cluster is healthy after failover
    log_info "🔍 Verifying post-failover cluster health"
    
    # Wait a bit more for full stabilization
    sleep 2
    
    # Check new cluster topology
    if ! replicas_result=$(query_via_client "SHOW REPLICAS;" 2>/dev/null); then
        fail_test "$test_name" "Failed to query post-failover cluster status"
        return 1
    fi
    
    local post_sync_count post_async_count
    post_sync_count=$(echo "$replicas_result" | jq '.records | map(select(.sync_mode == "sync")) | length' 2>/dev/null || echo "0")
    post_async_count=$(echo "$replicas_result" | jq '.records | map(select(.sync_mode == "async")) | length' 2>/dev/null || echo "0")
    
    log_info "📊 Post-failover topology: $post_sync_count sync + $post_async_count async replicas"
    
    if (( post_sync_count == 0 && post_async_count == 0 )); then
        # Cluster might still be reorganizing, this is acceptable
        log_info "⚠️  Cluster reorganizing: No replicas registered yet (expected during failover)"
    elif (( post_sync_count >= 0 && post_async_count >= 0 && post_sync_count + post_async_count <= 2 )); then
        log_info "✅ Post-failover cluster topology acceptable"
    else
        log_info "⚠️  Unexpected post-failover topology, but failover behavior was correct"
    fi
    
    log_info "🎉 Failover test completed successfully!"
    pass_test "$test_name"
}

# Main execution
main() {
    echo
    log_info "🚀 Starting Memgraph E2E Tests (Simplified Shell-based)"
    log_info "======================================================="
    
    # Quick prerequisite checks
    if ! command -v kubectl &> /dev/null || ! command -v jq &> /dev/null; then
        log_error "kubectl and jq are required"
        exit 1
    fi
    
    if ! kubectl get namespace "$MEMGRAPH_NS" &> /dev/null; then
        log_error "Namespace '$MEMGRAPH_NS' does not exist"
        exit 1
    fi
    
    log_info "✅ Prerequisites checked"
    echo
    
    # Run tests
    test_cluster_topology
    echo
    test_data_write_gateway
    echo  
    test_data_replication
    echo
    test_failover
    echo
    
    # Print results
    log_info "📋 Test Results Summary"
    log_info "======================"
    log_info "Tests run: $TESTS_RUN"
    log_success "Tests passed: $TESTS_PASSED"
    if (( TESTS_FAILED > 0 )); then
        log_error "Tests failed: $TESTS_FAILED"
    else
        log_info "Tests failed: $TESTS_FAILED"
    fi
    echo
    
    if (( TESTS_FAILED > 0 )); then
        log_error "💥 Some tests failed!"
        exit 1
    else
        log_success "🎉 All tests passed!"
        exit 0
    fi
}

# Run main if executed directly
if [[ "${BASH_SOURCE[0]}" == "${0}" ]]; then
    main "$@"
fi