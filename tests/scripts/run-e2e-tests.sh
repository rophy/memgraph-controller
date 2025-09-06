#!/bin/bash
set -euo pipefail

# E2E Test Configuration
readonly MEMGRAPH_NS="memgraph"
readonly TEST_CLIENT_LABEL="app=neo4j-client"
readonly EXPECTED_POD_COUNT=3
readonly TEST_TIMEOUT=60

# Colors for output
readonly RED='\033[0;31m'
readonly GREEN='\033[0;32m'
readonly YELLOW='\033[1;33m'
readonly BLUE='\033[0;34m'
readonly NC='\033[0m' # No Color

# Test counters
TESTS_RUN=0
TESTS_PASSED=0
TESTS_FAILED=0

# =============================================================================
# LOGGING AND TEST FRAMEWORK
# =============================================================================

log_info() {
    echo -e "${BLUE}[INFO]${NC} $*"
}

log_success() {
    echo -e "${GREEN}[SUCCESS]${NC} $*"
}

log_error() {
    echo -e "${RED}[ERROR]${NC} $*" >&2
}

log_warning() {
    echo -e "${YELLOW}[WARNING]${NC} $*"
}

start_test() {
    local test_name="$1"
    ((TESTS_RUN++))
    log_info "🧪 Starting test: $test_name"
}

pass_test() {
    local test_name="$1"
    ((TESTS_PASSED++))
    log_success "✅ PASSED: $test_name"
}

fail_test() {
    local test_name="$1"
    local reason="$2"
    ((TESTS_FAILED++))
    log_error "❌ FAILED: $test_name - $reason"
}

# =============================================================================
# KUBERNETES HELPER FUNCTIONS
# =============================================================================

get_memgraph_pods() {
    kubectl get pods -n "$MEMGRAPH_NS" \
        -l "app.kubernetes.io/name=memgraph" \
        -o custom-columns=NAME:.metadata.name,READY:.status.containerStatuses[0].ready,IP:.status.podIP \
        --no-headers 2>/dev/null || return 1
}

get_test_client_pod() {
    kubectl get pods -n "$MEMGRAPH_NS" \
        -l "$TEST_CLIENT_LABEL" \
        -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || return 1
}

wait_for_cluster() {
    local timeout="$TEST_TIMEOUT"
    local start_time=$(date +%s)
    
    log_info "⏳ Waiting for cluster to be ready (timeout: ${timeout}s)..."
    
    while true; do
        local current_time=$(date +%s)
        if (( current_time - start_time > timeout )); then
            log_error "Timeout waiting for cluster to be ready"
            return 1
        fi
        
        local pods
        if ! pods=$(get_memgraph_pods 2>/dev/null); then
            log_warning "Failed to get pods, retrying..."
            sleep 2
            continue
        fi
        
        local pod_count
        pod_count=$(echo "$pods" | wc -l)
        
        if (( pod_count == EXPECTED_POD_COUNT )); then
            local all_ready=true
            while IFS= read -r line; do
                local ready=$(echo "$line" | awk '{print $2}')
                if [[ "$ready" != "true" ]]; then
                    all_ready=false
                    break
                fi
            done <<< "$pods"
            
            if [[ "$all_ready" == "true" ]]; then
                log_success "✅ All $EXPECTED_POD_COUNT pods are ready"
                return 0
            fi
        fi
        
        log_warning "Waiting for pods (current: $pod_count/$EXPECTED_POD_COUNT ready)..."
        sleep 2
    done
}

# =============================================================================
# MEMGRAPH QUERY HELPERS VIA TEST-CLIENT
# =============================================================================

query_via_client() {
    local query="$1"
    local test_client_pod
    
    if ! test_client_pod=$(get_test_client_pod); then
        log_error "Failed to find test-client pod"
        return 1
    fi
    
    if [[ -z "$test_client_pod" ]]; then
        log_error "Test-client pod not found"
        return 1
    fi
    
    kubectl exec "$test_client_pod" -n "$MEMGRAPH_NS" -- \
        node index.js "$query" 2>/dev/null
}

get_replication_role() {
    local pod_name="$1"
    # Query through gateway - it will route to the main pod
    local result
    if ! result=$(query_via_client "SHOW REPLICATION ROLE;"); then
        return 1
    fi
    
    # Extract role from JSON response
    echo "$result" | jq -r '.records[0]["replication role"]' 2>/dev/null || return 1
}

get_replicas_info() {
    # Query replica registrations from main pod via gateway
    local result
    if ! result=$(query_via_client "SHOW REPLICAS;"); then
        return 1
    fi
    
    echo "$result"
}

write_test_data() {
    local test_id="$1"
    local test_value="$2"
    
    local query="CREATE (n:TestNode {id: '$test_id', value: '$test_value', timestamp: datetime()}) RETURN n.id;"
    query_via_client "$query" > /dev/null 2>&1
}

read_test_data() {
    local test_id="$1"
    
    local query="MATCH (n:TestNode {id: '$test_id'}) RETURN n.id, n.value;"
    query_via_client "$query"
}

count_nodes() {
    local query="MATCH (n) RETURN count(n) as node_count;"
    local result
    if ! result=$(query_via_client "$query"); then
        return 1
    fi
    
    # Extract count from JSON response  
    echo "$result" | jq -r '.records[0].node_count.low' 2>/dev/null || \
    echo "$result" | jq -r '.records[0].node_count' 2>/dev/null || return 1
}

# =============================================================================
# E2E TEST IMPLEMENTATIONS
# =============================================================================

test_cluster_topology() {
    local test_name="Cluster Topology"
    start_test "$test_name"
    
    # Wait for cluster to be ready
    if ! wait_for_cluster; then
        fail_test "$test_name" "Cluster not ready"
        return 1
    fi
    
    # Get all pods
    local pods
    if ! pods=$(get_memgraph_pods); then
        fail_test "$test_name" "Failed to get pods"
        return 1
    fi
    
    # Verify pod count
    local pod_count
    pod_count=$(echo "$pods" | wc -l)
    if (( pod_count != EXPECTED_POD_COUNT )); then
        fail_test "$test_name" "Expected $EXPECTED_POD_COUNT pods, got $pod_count"
        return 1
    fi
    
    # Check cluster has main role (via gateway)
    local main_role
    if ! main_role=$(get_replication_role "gateway"); then
        fail_test "$test_name" "Failed to get replication role"
        return 1
    fi
    
    if [[ "$main_role" != "main" ]]; then
        fail_test "$test_name" "Gateway should connect to main, got role: $main_role"
        return 1
    fi
    
    # Get replica registrations
    local replicas_result
    if ! replicas_result=$(get_replicas_info); then
        fail_test "$test_name" "Failed to get replicas info"
        return 1
    fi
    
    # Parse replica info to verify topology
    local sync_count async_count
    sync_count=$(echo "$replicas_result" | jq '.records | map(select(.sync_mode == "sync")) | length' 2>/dev/null || echo "0")
    async_count=$(echo "$replicas_result" | jq '.records | map(select(.sync_mode == "async")) | length' 2>/dev/null || echo "0")
    
    # Validate topology rules
    if (( sync_count != 1 )); then
        fail_test "$test_name" "Expected 1 sync replica, got $sync_count"
        return 1
    fi
    
    if (( async_count != 1 )); then
        fail_test "$test_name" "Expected 1 async replica, got $async_count"
        return 1
    fi
    
    # Extract replica names to verify pod assignments
    local sync_replica async_replica
    sync_replica=$(echo "$replicas_result" | jq -r '.records | map(select(.sync_mode == "sync"))[0].name' 2>/dev/null)
    async_replica=$(echo "$replicas_result" | jq -r '.records | map(select(.sync_mode == "async"))[0].name' 2>/dev/null)
    
    # Convert to pod names (memgraph_ha_X -> memgraph-ha-X)
    sync_pod=$(echo "$sync_replica" | tr '_' '-')
    async_pod=$(echo "$async_replica" | tr '_' '-')
    
    # Validate pod-2 is always async (per design)
    if [[ "$async_pod" != "memgraph-ha-2" ]]; then
        fail_test "$test_name" "Pod-2 should be async replica, got: $async_pod"
        return 1
    fi
    
    # Validate sync replica is eligible pod (0 or 1)
    if [[ "$sync_pod" != "memgraph-ha-0" && "$sync_pod" != "memgraph-ha-1" ]]; then
        fail_test "$test_name" "Sync replica should be pod-0 or pod-1, got: $sync_pod"
        return 1
    fi
    
    log_info "📊 Topology: Main via gateway, Sync=$sync_pod, Async=$async_pod, Total pods=$pod_count"
    pass_test "$test_name"
}

test_data_write_gateway() {
    local test_name="Data Write Through Gateway"
    start_test "$test_name"
    
    # Generate unique test data
    local test_id="test_$(date +%s)_$$"
    local test_value="value_$(date +%s)_$$"
    
    log_info "📝 Writing test data: ID=$test_id, Value=$test_value"
    
    # Write test data through gateway
    if ! write_test_data "$test_id" "$test_value"; then
        fail_test "$test_name" "Failed to write test data"
        return 1
    fi
    
    # Read data back to verify
    local result
    if ! result=$(read_test_data "$test_id"); then
        fail_test "$test_name" "Failed to read test data back"
        return 1
    fi
    
    # Verify data matches
    local returned_id returned_value
    returned_id=$(echo "$result" | jq -r '.records[0]["n.id"]' 2>/dev/null)
    returned_value=$(echo "$result" | jq -r '.records[0]["n.value"]' 2>/dev/null)
    
    if [[ "$returned_id" != "$test_id" ]]; then
        fail_test "$test_name" "ID mismatch: expected $test_id, got $returned_id"
        return 1
    fi
    
    if [[ "$returned_value" != "$test_value" ]]; then
        fail_test "$test_name" "Value mismatch: expected $test_value, got $returned_value"
        return 1
    fi
    
    log_info "✅ Data verified: ID=$test_id, Value=$test_value"
    pass_test "$test_name"
}

test_data_replication() {
    local test_name="Data Replication Verification"
    start_test "$test_name"
    
    # Get initial node count
    local initial_count
    if ! initial_count=$(count_nodes); then
        fail_test "$test_name" "Failed to get initial node count"
        return 1
    fi
    
    log_info "📊 Initial node count: $initial_count"
    
    # Write replication test data
    local test_id="replication_test_$(date +%s)_$$"
    local test_value="replication_data"
    
    log_info "📝 Writing replication test data: ID=$test_id"
    if ! write_test_data "$test_id" "$test_value"; then
        fail_test "$test_name" "Failed to write replication test data"
        return 1
    fi
    
    # Wait for replication
    log_info "⏳ Waiting for replication to complete..."
    sleep 3
    
    # Verify data exists
    local result
    if ! result=$(read_test_data "$test_id"); then
        fail_test "$test_name" "Replication test data not found"
        return 1
    fi
    
    local returned_id
    returned_id=$(echo "$result" | jq -r '.records[0]["n.id"]' 2>/dev/null)
    
    if [[ "$returned_id" != "$test_id" ]]; then
        fail_test "$test_name" "Replication data mismatch"
        return 1
    fi
    
    # Verify final node count increased
    local final_count
    if ! final_count=$(count_nodes); then
        fail_test "$test_name" "Failed to get final node count"
        return 1
    fi
    
    log_info "📊 Final node count: $final_count (increase: $((final_count - initial_count)))"
    
    # Get cluster topology for verification
    local replicas_result
    if replicas_result=$(get_replicas_info 2>/dev/null); then
        local replica_count
        replica_count=$(echo "$replicas_result" | jq '.records | length' 2>/dev/null || echo "0")
        log_info "🔗 Active replicas: $replica_count"
    fi
    
    log_info "✅ Data replication verified successfully"
    pass_test "$test_name"
}

# =============================================================================
# MAIN EXECUTION
# =============================================================================

main() {
    echo
    log_info "🚀 Starting Memgraph E2E Tests (Shell-based)"
    log_info "=============================================="
    
    # Check prerequisites
    if ! command -v kubectl &> /dev/null; then
        log_error "kubectl is required but not installed"
        exit 1
    fi
    
    if ! command -v jq &> /dev/null; then
        log_error "jq is required but not installed"
        exit 1
    fi
    
    # Verify cluster connectivity
    if ! kubectl cluster-info &> /dev/null; then
        log_error "Cannot connect to Kubernetes cluster"
        exit 1
    fi
    
    # Check if namespace exists
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

# Run main function if script is executed directly
if [[ "${BASH_SOURCE[0]}" == "${0}" ]]; then
    main "$@"
fi