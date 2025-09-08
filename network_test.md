# Network Partition Failover Test Specification

## Overview

This document describes a comprehensive end-to-end test for validating the Memgraph HA controller's behavior during network partition scenarios. The test simulates a realistic network failure where the main Memgraph node becomes isolated, forcing the controller to detect the failure and perform automatic failover.

## Test Objectives

- Verify automatic failover detection and execution during network partitions
- Validate service continuity during and after failover events
- Ensure proper cluster reconciliation after network recovery
- Test the robustness of the health monitoring and connection management systems

## Prerequisites

### Environment Requirements
- Kubernetes cluster with Calico CNI (required for NetworkPolicy support)
- Memgraph HA deployment with 3 pods (main-sync-async topology)
- Memgraph controller with health prober enabled
- Test client for continuous write operations
- Admin API enabled on controller (`ENABLE_ADMIN_API=true`)

### Initial Cluster State
- **Pod Count**: 3 Memgraph pods (memgraph-ha-0, memgraph-ha-1, memgraph-ha-2)
- **Replication Topology**: 1 main + 1 sync replica + 1 async replica
- **Controller**: 2 replicas with leader election (1 active, 1 standby)
- **Test Client**: Continuously writing data every 1 second

## Test Steps

### Step 1: Pre-Condition Validation

**Purpose**: Ensure the cluster is in a healthy state before starting the partition test.

**Actions**:
1. Verify all 3 Memgraph pods are in Ready state
2. Query controller status API (`/api/v1/status`) to confirm:
   - One pod has `memgraph_role: "main"`
   - One pod has `memgraph_role: "replica"` with sync mode
   - One pod has `memgraph_role: "replica"` with async mode
3. Execute test write operations to confirm data path is functional
4. Query `SHOW REPLICAS` from main pod to verify replication registration:
   - Expect 1 sync replica with `sync_mode: "sync"`
   - Expect 1 async replica with `sync_mode: "async"`
   - All replicas should show `status: "ready"`

**Success Criteria**:
- All pods ready and accessible
- Controller reports correct main-sync-async topology
- Test writes succeed consistently
- Replication topology matches expected configuration

### Step 2: Network Isolation Setup

**Purpose**: Isolate the main node using NetworkPolicy and force connection resets.

**Actions**:
1. Identify current main pod from controller status API
2. Create NetworkPolicy to isolate the main pod:
   ```yaml
   apiVersion: networking.k8s.io/v1
   kind: NetworkPolicy
   metadata:
     name: isolate-{main-pod-name}
     namespace: memgraph
   spec:
     podSelector:
       matchLabels:
         statefulset.kubernetes.io/pod-name: {main-pod-name}
     policyTypes:
     - Ingress
     - Egress
     ingress: []
     egress: []
   ```
3. Call admin API to reset all connections:
   - `POST /api/v1/admin/reset-connections`
   - Expect HTTP 200 response
   - Should reset both controller and gateway connection pools

**Success Criteria**:
- NetworkPolicy created successfully
- Admin API call returns success
- Existing connections to main pod are terminated

### Step 3: Failure Detection Phase

**Purpose**: Verify the controller detects the network partition and begins failover.

**Actions**:
1. Monitor test client logs for write failures
2. Expect write operations to start failing within 2-3 seconds
3. Monitor controller logs for health check failures
4. Wait for health prober to detect failure (default: 3 consecutive failures)

**Expected Behavior**:
- Test client writes fail due to connection timeouts
- Controller health prober logs connection failures to main pod
- Health prober triggers failover after failure threshold reached
- Controller begins leader election process among remaining pods

**Success Criteria**:
- Test writes fail consistently during isolation period
- Controller logs show health check failures
- Failover process initiated (visible in controller logs)

### Step 4: Failover Execution

**Purpose**: Validate automatic failover completes successfully.

**Actions**:
1. Wait for controller to complete failover (timeout: 60 seconds)
2. Monitor for new main pod selection
3. Verify controller status API shows new topology
4. Test data writes resume successfully

**Expected Behavior**:
- Controller selects new main from available sync replica (memgraph-ha-0 or memgraph-ha-1)
- Gateway routing updates to new main pod
- Replication reconfigures with new topology
- Write operations resume after brief interruption

**Success Criteria**:
- Controller status shows different pod as main
- Test writes succeed again after failover
- New main pod accepts write operations
- Failover completes within reasonable time (< 60 seconds)

### Step 5: Post-Failover Topology Verification

**Purpose**: Confirm the cluster operates correctly after failover.

**Actions**:
1. Query controller status API for new topology
2. Execute `SHOW REPLICAS` from new main pod
3. Verify replication is functioning:
   - Query node count from all accessible pods
   - Confirm data consistency across replicas
4. Validate the isolated pod is not included in replica set

**Expected Topology**:
- New main pod (different from original)
- Remaining pod configured as replica
- Isolated pod excluded from replication (temporarily)
- Reduced but functional cluster topology

**Success Criteria**:
- New main pod different from isolated pod
- Replication functional with available pods
- Data writes and reads work correctly
- No split-brain condition detected

### Step 6: Network Recovery

**Purpose**: Restore network connectivity and test cluster reconciliation.

**Actions**:
1. Delete the NetworkPolicy to restore connectivity
2. Wait for network connectivity to be re-established
3. Allow time for cluster reconciliation (30-60 seconds)
4. Monitor controller logs for reconciliation events

**Expected Behavior**:
- Isolated pod becomes reachable again
- Controller detects recovered pod
- Cluster reconciliation process begins
- Previously isolated pod rejoins as replica

**Success Criteria**:
- NetworkPolicy removed successfully
- All pods become accessible
- No network connectivity errors in logs
- Controller begins reconciliation process

### Step 7: Final Reconciliation Validation

**Purpose**: Ensure the cluster returns to a healthy state after network recovery.

**Actions**:
1. Wait for cluster convergence (allow up to 60 seconds)
2. Verify all 3 pods are accessible and ready
3. Query final replication topology:
   - Execute `SHOW REPLICAS` from current main
   - Verify all pods are registered as replicas
4. Test data consistency:
   - Execute test writes and reads
   - Verify data is replicated across all pods
5. Confirm final topology matches expected configuration

**Expected Final State**:
- All 3 pods operational and ready
- Replication restored (1 sync + 1 async replica)
- Data consistency across all nodes
- Normal cluster operation resumed

**Success Criteria**:
- All pods ready and functional
- Replication topology restored to expected configuration
- Data writes and reads successful
- No error conditions in controller or pod logs

## Test Data and Metrics

### Monitoring Points
- **Failover Detection Time**: Time from network partition to health check failure
- **Failover Completion Time**: Time from failure detection to write operations resuming
- **Write Failure Duration**: Duration of write operation failures during failover
- **Reconciliation Time**: Time for cluster to fully recover after network restoration

### Success Metrics
- **Failover Time**: < 60 seconds from partition to functional writes
- **Write Interruption**: < 30 seconds of write failures
- **Zero Data Loss**: All committed writes before partition are preserved
- **Full Recovery**: Complete cluster functionality restored within 120 seconds

### Test Data Patterns
- **Pre-Partition Writes**: Unique identifiers with timestamps
- **Failure Period Tracking**: Document expected write failures
- **Post-Failover Writes**: Verify new writes succeed on new main
- **Recovery Validation**: Confirm all historical data accessible

## Error Scenarios and Handling

### Potential Failure Points
1. **NetworkPolicy Not Applied**: Verify CNI supports NetworkPolicy
2. **Admin API Unavailable**: Check ENABLE_ADMIN_API environment variable
3. **Failover Timeout**: Extended failure detection or execution time
4. **Split-Brain Condition**: Multiple nodes claiming main role
5. **Incomplete Reconciliation**: Pods not rejoining cluster after recovery

### Cleanup Requirements
- Remove any created NetworkPolicies
- Ensure all connection pools are reset
- Verify no persistent network restrictions
- Clean up test data to prevent interference with subsequent tests

## Implementation Notes

### API Endpoints Used
- **Controller Status**: `GET /api/v1/status`
- **Admin Reset**: `POST /api/v1/admin/reset-connections`

### Kubernetes Resources
- **NetworkPolicy**: For network isolation simulation
- **Pod Status**: Monitor pod readiness and IP addresses
- **Service Endpoints**: Verify service routing during failover

### Memgraph Queries
- **Replication Role**: `SHOW REPLICATION ROLE`
- **Replica Status**: `SHOW REPLICAS`
- **Data Verification**: Custom queries for consistency checking

## Test Automation Considerations

This test should be implemented with:
- **Configurable Timeouts**: Allow adjustment for different cluster performance
- **Detailed Logging**: Comprehensive logging for debugging failures
- **Cleanup Handlers**: Ensure cleanup even if test fails mid-execution
- **Parallel Safety**: Can run alongside other tests without interference
- **Environment Validation**: Pre-flight checks for required components

## Expected Outcomes

A successful test execution demonstrates:
- Robust failure detection mechanisms
- Automatic failover without manual intervention
- Service continuity with minimal disruption
- Complete recovery and reconciliation capabilities
- Production-ready high availability behavior