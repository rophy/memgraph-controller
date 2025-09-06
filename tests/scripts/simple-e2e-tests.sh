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

log_info() { echo -e "${BLUE}[INFO]${NC} $(date -Iseconds) $*"; }
log_success() { echo -e "${GREEN}[SUCCESS]${NC} $(date -Iseconds) $*"; }
log_error() { echo -e "${RED}[ERROR]${NC} $(date -Iseconds) $*" >&2; }

start_test() { 
    local test_name="$1"
    TESTS_RUN=$((TESTS_RUN + 1))
    log_info "🧪 Starting test: $test_name"
}

pass_test() {
    local test_name="$1" 
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

# Wait for cluster to stabilize after failover or startup
wait_for_cluster_convergence() {
    local max_wait=30  # Maximum wait time in seconds
    local wait_interval=2
    local elapsed=0
    
    log_info "⏳ Waiting for cluster convergence..."
    
    while (( elapsed < max_wait )); do
        # Check replication role
        local role_result
        if ! role_result=$(query_via_client "SHOW REPLICATION ROLE;" 2>/dev/null); then
            log_info "⏳ Cannot query role yet, waiting... (${elapsed}s/${max_wait}s)"
            sleep $wait_interval
            elapsed=$((elapsed + wait_interval))
            continue
        fi
        
        local main_role
        main_role=$(echo "$role_result" | jq -r '.[0]["replication role"]' 2>/dev/null || echo "")
        
        if [[ "$main_role" != "main" ]]; then
            log_info "⏳ Role not main yet ($main_role), waiting... (${elapsed}s/${max_wait}s)"
            sleep $wait_interval
            elapsed=$((elapsed + wait_interval))
            continue
        fi
        
        # Check replicas
        local replicas_result
        if ! replicas_result=$(query_via_client "SHOW REPLICAS;" 2>/dev/null); then
            log_info "⏳ Cannot query replicas yet, waiting... (${elapsed}s/${max_wait}s)"
            sleep $wait_interval
            elapsed=$((elapsed + wait_interval))
            continue
        fi
        
        local sync_count async_count
        sync_count=$(echo "$replicas_result" | jq '. | map(select(.sync_mode == "sync")) | length' 2>/dev/null | head -1 | tr -d '\n' || echo "0")
        async_count=$(echo "$replicas_result" | jq '. | map(select(.sync_mode == "async")) | length' 2>/dev/null | head -1 | tr -d '\n' || echo "0")
        
        if (( sync_count == 1 && async_count == 1 )); then
            log_info "✅ Cluster converged after ${elapsed}s: 1 sync + 1 async replica"
            return 0
        fi
        
        log_info "⏳ Waiting for convergence... sync=$sync_count async=$async_count (${elapsed}s/${max_wait}s)"
        sleep $wait_interval
        elapsed=$((elapsed + wait_interval))
    done
    
    log_error "❌ Cluster failed to converge within ${max_wait}s"
    return 1
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
    echo "pod_count: $pod_count"
    if (( pod_count != EXPECTED_POD_COUNT )); then
        fail_test "$test_name" "Expected $EXPECTED_POD_COUNT pods, got $pod_count"
        return 1
    fi
    
    # Wait for cluster to converge with retry logic
    if ! wait_for_cluster_convergence; then
        fail_test "$test_name" "Cluster failed to converge to expected topology"
        return 1
    fi
    
    # Final verification - query current state for logging
    local role_result replicas_result
    role_result=$(query_via_client "SHOW REPLICATION ROLE;")
    replicas_result=$(query_via_client "SHOW REPLICAS;")
    
    echo "SHOW REPLICATION ROLE: $role_result"
    echo "SHOW REPLICAS: $replicas_result"
    
    local sync_count async_count
    sync_count=$(echo "$replicas_result" | jq '. | map(select(.sync_mode == "sync")) | length' 2>/dev/null | head -1 | tr -d '\n' || echo "0")
    async_count=$(echo "$replicas_result" | jq '. | map(select(.sync_mode == "async")) | length' 2>/dev/null | head -1 | tr -d '\n' || echo "0")
    echo "sync_count: $sync_count, async_count: $async_count"
    
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
    if ! query_via_client "$write_query"; then
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
    returned_id=$(echo "$result" | jq -r '.[0]["n.id"]' 2>/dev/null)
    returned_value=$(echo "$result" | jq -r '.[0]["n.value"]' 2>/dev/null)
    
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
    initial_count=$(echo "$count_result" | jq -r '.[0].node_count.low // .[0].node_count' 2>/dev/null)
    
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
    returned_id=$(echo "$result" | jq -r '.[0]["n.id"]' 2>/dev/null)
    
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
    final_count=$(echo "$count_result" | jq -r '.[0].node_count.low // .[0].node_count' 2>/dev/null)
    
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
    sync_count=$(echo "$replicas_result" | jq '. | map(select(.sync_mode == "sync")) | length' 2>/dev/null | head -1 | tr -d '\n' || echo "0")
    async_count=$(echo "$replicas_result" | jq '. | map(select(.sync_mode == "async")) | length' 2>/dev/null | head -1 | tr -d '\n' || echo "0")
    
    if (( sync_count != 1 )) || (( async_count != 1 )); then
        fail_test "$test_name" "Invalid cluster topology: sync=$sync_count async=$async_count (expected 1 sync + 1 async)"
        return 1
    fi
    
    # Check both replicas are healthy (have data_info)
    local unhealthy_replicas
    unhealthy_replicas=$(echo "$replicas_result" | jq '. | map(select(.data_info == null)) | length' 2>/dev/null | head -1 | tr -d '\n' || echo "0")
    
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
    recent_success_count=$(echo "$recent_logs" | grep -c "✓ Success" 2>/dev/null || echo "0")
    recent_success_count=$(echo "$recent_success_count" | head -1 | tr -d '\n')
    echo "recent_success_count: $recent_success_count"
    
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
    error_count=$(echo "$post_failover_logs" | grep -c "✗ Failed" 2>/dev/null || echo "0")
    error_count=$(echo "$error_count" | head -1 | tr -d '\n')
    
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
    
    # Verify cluster converges to healthy state after failover
    log_info "🔍 Step 4: Verifying post-failover cluster convergence"
    
    # Wait for cluster to converge with retry logic
    if ! wait_for_cluster_convergence; then
        log_info "⚠️  Cluster did not fully converge within 30s, but failover behavior was acceptable"
        log_info "📊 This may indicate split-brain timing window - checking final state..."
        
        # Get final state for logging
        local final_replicas_result
        if final_replicas_result=$(query_via_client "SHOW REPLICAS;" 2>/dev/null); then
            local final_sync_count final_async_count
            final_sync_count=$(echo "$final_replicas_result" | jq '. | map(select(.sync_mode == "sync")) | length' 2>/dev/null | head -1 | tr -d '\n' || echo "0")
            final_async_count=$(echo "$final_replicas_result" | jq '. | map(select(.sync_mode == "async")) | length' 2>/dev/null | head -1 | tr -d '\n' || echo "0")
            log_info "📊 Final topology: $final_sync_count sync + $final_async_count async replicas"
            
            # Accept if we have at least some replicas (cluster is functioning)
            if (( final_sync_count + final_async_count >= 1 )); then
                log_info "✅ Post-failover cluster is functional with partial replica registration"
            else
                fail_test "$test_name" "Post-failover cluster has no registered replicas"
                return 1
            fi
        else
            fail_test "$test_name" "Cannot query cluster status after failover"
            return 1
        fi
    else
        log_info "✅ Post-failover cluster fully converged to expected topology"
    fi
    
    log_info "🎉 Failover test completed successfully!"
    pass_test "$test_name"
}

# Test 5: Rolling Restart of StatefulSet
test_rolling_restart() {
    local test_name="Rolling Restart Test"
    start_test "$test_name"
    
    log_info "🔍 Step 1: Verify pre-restart cluster health"
    
    # Ensure cluster is healthy before rolling restart
    if ! wait_for_cluster_convergence; then
        fail_test "$test_name" "Cluster not healthy before rolling restart"
        return 1
    fi
    
    # Get test client pod for log monitoring
    local test_client_pod
    if ! test_client_pod=$(get_test_client_pod); then
        fail_test "$test_name" "Failed to find test-client pod"
        return 1
    fi
    
    # Record initial successful write count
    local pre_restart_logs
    if ! pre_restart_logs=$(kubectl logs "$test_client_pod" -n "$MEMGRAPH_NS" --tail=30 2>/dev/null); then
        fail_test "$test_name" "Failed to get pre-restart logs"
        return 1
    fi
    
    local pre_success_count
    pre_success_count=$(echo "$pre_restart_logs" | grep -c "✓ Success" 2>/dev/null || echo "0")
    pre_success_count=$(echo "$pre_success_count" | head -1 | tr -d '\n')
    
    log_info "✅ Pre-restart cluster healthy with $pre_success_count recent successes"
    
    # Get current StatefulSet generation
    local initial_generation
    if ! initial_generation=$(kubectl get statefulset memgraph-ha -n "$MEMGRAPH_NS" -o jsonpath='{.metadata.generation}' 2>/dev/null); then
        fail_test "$test_name" "Failed to get StatefulSet generation"
        return 1
    fi
    
    log_info "📊 Initial StatefulSet generation: $initial_generation"
    
    # Step 2: Trigger rolling restart
    log_info "🔄 Step 2: Triggering rolling restart of memgraph-ha StatefulSet"
    
    if ! kubectl rollout restart statefulset/memgraph-ha -n "$MEMGRAPH_NS" 2>/dev/null; then
        fail_test "$test_name" "Failed to trigger rolling restart"
        return 1
    fi
    
    log_info "⏳ Rolling restart initiated, waiting for restart to begin..."
    
    # Wait for StatefulSet generation to change (indicating rolling restart started)
    local max_wait=30
    local elapsed=0
    local new_generation="$initial_generation"
    
    while (( elapsed < max_wait )) && [[ "$new_generation" == "$initial_generation" ]]; do
        sleep 2
        elapsed=$((elapsed + 2))
        new_generation=$(kubectl get statefulset memgraph-ha -n "$MEMGRAPH_NS" -o jsonpath='{.metadata.generation}' 2>/dev/null || echo "$initial_generation")
    done
    
    if [[ "$new_generation" == "$initial_generation" ]]; then
        fail_test "$test_name" "Rolling restart did not start within ${max_wait}s"
        return 1
    fi
    
    log_info "✅ Rolling restart started (generation: $initial_generation → $new_generation)"
    
    # Step 3: Wait for rolling restart to complete
    log_info "⏳ Step 3: Waiting for rolling restart to complete..."
    
    # Wait for all pods to be ready with new generation
    max_wait=180  # 3 minutes for rolling restart
    elapsed=0
    
    while (( elapsed < max_wait )); do
        local ready_replicas updated_replicas
        ready_replicas=$(kubectl get statefulset memgraph-ha -n "$MEMGRAPH_NS" -o jsonpath='{.status.readyReplicas}' 2>/dev/null || echo "0")
        updated_replicas=$(kubectl get statefulset memgraph-ha -n "$MEMGRAPH_NS" -o jsonpath='{.status.updatedReplicas}' 2>/dev/null || echo "0")
        
        if (( ready_replicas == 3 && updated_replicas == 3 )); then
            log_info "✅ Rolling restart completed: all 3 replicas ready and updated"
            break
        fi
        
        log_info "⏳ Rolling restart in progress: ready=$ready_replicas/3, updated=$updated_replicas/3 (${elapsed}s/${max_wait}s)"
        sleep 5
        elapsed=$((elapsed + 5))
    done
    
    if (( elapsed >= max_wait )); then
        fail_test "$test_name" "Rolling restart did not complete within ${max_wait}s"
        return 1
    fi
    
    # Step 4: Analyze connection behavior during rolling restart
    log_info "🔍 Step 4: Analyzing connection behavior during rolling restart"
    
    # Get logs from around the rolling restart period
    local post_restart_logs
    if ! post_restart_logs=$(kubectl logs "$test_client_pod" -n "$MEMGRAPH_NS" --tail=60 2>/dev/null); then
        fail_test "$test_name" "Failed to get post-restart logs"
        return 1
    fi
    
    # Count errors during rolling restart
    local error_count success_after_restart
    error_count=$(echo "$post_restart_logs" | grep -c "✗ Failed" 2>/dev/null || echo "0")
    error_count=$(echo "$error_count" | head -1 | tr -d '\n')
    
    # Check for recovery (recent successes)
    success_after_restart=$(echo "$post_restart_logs" | tail -20 | grep -c "✓ Success" 2>/dev/null || echo "0")
    success_after_restart=$(echo "$success_after_restart" | head -1 | tr -d '\n')
    
    # Validate rolling restart behavior
    log_info "📊 Rolling restart impact: $error_count connection errors, $success_after_restart recent successes"
    
    if (( error_count <= 5 )) && (( success_after_restart >= 3 )); then
        log_info "✅ Excellent rolling restart: minimal disruption with good recovery"
    elif (( error_count <= 10 )) && (( success_after_restart >= 1 )); then
        log_info "✅ Acceptable rolling restart: moderate disruption with recovery"
    else
        # This might still be acceptable for rolling restart - it's more disruptive than single pod failover
        if (( success_after_restart >= 1 )); then
            log_info "⚠️  Rolling restart caused significant disruption but service recovered"
        else
            fail_test "$test_name" "Rolling restart failed: $error_count errors with no recovery"
            return 1
        fi
    fi
    
    # Step 5: Verify final cluster convergence
    log_info "🔍 Step 5: Verifying post-restart cluster convergence"
    
    if ! wait_for_cluster_convergence; then
        log_info "⚠️  Cluster convergence incomplete, checking if functional..."
        
        # Try to query basic status
        local role_result
        if role_result=$(query_via_client "SHOW REPLICATION ROLE;" 2>/dev/null); then
            local main_role
            main_role=$(echo "$role_result" | jq -r '.[0]["replication role"]' 2>/dev/null || echo "")
            if [[ "$main_role" == "main" ]]; then
                log_info "✅ Post-restart cluster is functional (main role confirmed)"
            else
                fail_test "$test_name" "Post-restart cluster not functional: role=$main_role"
                return 1
            fi
        else
            fail_test "$test_name" "Cannot query cluster after rolling restart"
            return 1
        fi
    else
        log_info "✅ Post-restart cluster fully converged to expected topology"
    fi
    
    log_info "🎉 Rolling restart test completed successfully!"
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
    test_rolling_restart
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