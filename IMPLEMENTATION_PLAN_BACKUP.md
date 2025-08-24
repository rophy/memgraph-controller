# Implementation Plan: data_info Field Support

## Overview
Add support for parsing and utilizing the `data_info` field from `SHOW REPLICAS` to enable early detection of replication failures and automatic recovery mechanisms.

## Stage 1: Data Structure Enhancement
**Goal**: Extend ReplicaInfo struct to capture data_info field
**Success Criteria**: 
- ReplicaInfo struct includes DataInfo field
- Parsing logic extracts data_info from SHOW REPLICAS output
- Unit tests validate data_info parsing with known good/bad values

**Tests**: 
- Test parsing healthy data_info: `{memgraph: {behind: 0, status: "ready", ts: 2}}`
- Test parsing unhealthy data_info: `{memgraph: {behind: -20, status: "invalid", ts: 0}}`
- Test parsing empty data_info: `{}`
- Test malformed data_info handling

**Status**: ✅ Complete

### Implementation Details:
1. **Update ReplicaInfo struct** in `pkg/controller/memgraph_client.go`:
   ```go
   type ReplicaInfo struct {
       Name           string
       Address        string
       SyncMode       string
       SystemTimestamp string
       DataInfo       string  // NEW: Raw data_info field
       ParsedDataInfo *DataInfoStatus  // NEW: Parsed structure
   }
   
   type DataInfoStatus struct {
       Behind int    `json:"behind"`
       Status string `json:"status"`  
       Timestamp int `json:"ts"`
       IsHealthy bool
       ErrorReason string
   }
   ```

2. **Update parsing logic** in `parseShowReplicasResult()`:
   - Extract data_info field from CSV output (column index to be determined)
   - Parse JSON-like structure within data_info string
   - Handle parsing errors gracefully with detailed logging

3. **Add parsing helper**:
   ```go
   func parseDataInfo(dataInfoStr string) (*DataInfoStatus, error) {
       // Parse JSON-like structure: "{memgraph: {behind: 0, status: \"ready\", ts: 2}}"
       // Handle special cases: empty "{}", malformed strings
       // Return structured data with health assessment
   }
   ```

---

## Stage 2: Health Assessment Logic
**Goal**: Implement health assessment based on parsed data_info
**Success Criteria**:
- IsHealthy logic correctly identifies healthy vs unhealthy replicas
- Clear error reasons provided for unhealthy states
- Health status available in controller decision-making

**Tests**:
- Test healthy replica identification (behind=0, status="ready")
- Test unhealthy replica identification (behind<0, status="invalid") 
- Test unknown/ambiguous states (empty data_info)
- Test edge cases (high lag, unknown status values)

**Status**: ✅ Complete

### Implementation Details:
1. **Health assessment rules**:
   - `status: "ready" && behind >= 0` → Healthy
   - `status: "invalid"` → Connection/registration failure
   - `behind < 0` → Replication error condition
   - Empty `{}` → Connection establishment failure
   - Missing/malformed data_info → Unknown health

2. **Add methods to ReplicaInfo**:
   ```go
   func (ri *ReplicaInfo) IsHealthy() bool
   func (ri *ReplicaInfo) GetHealthReason() string
   func (ri *ReplicaInfo) RequiresRecovery() bool
   ```

3. **Integration with controller logic**:
   - Update replication configuration functions to check replica health
   - Log health status changes for monitoring
   - Use health status in SYNC replica selection

---

## Stage 3: Automatic Recovery Mechanisms  
**Goal**: Implement automatic recovery for failed replicas based on data_info
**Success Criteria**:
- Failed replicas are automatically detected and recovered
- Recovery attempts logged with success/failure status
- Graceful degradation when recovery fails repeatedly

**Tests**:
- Test recovery of replica with status="invalid" 
- Test recovery of replica with empty data_info
- Test recovery failure handling and retry limits
- Test recovery of SYNC replica vs ASYNC replica prioritization

**Status**: Not Started

### Implementation Details:
1. **Recovery strategies by failure type**:
   - `status: "invalid"` → Drop and re-register replica
   - Empty `{}` → Check master logs, potentially restart pod or re-register
   - High `behind` values → Monitor and alert, potential manual intervention flag

2. **Add recovery functions**:
   ```go
   func (c *MemgraphController) recoverFailedReplica(ctx context.Context, masterPod *PodInfo, replicaInfo *ReplicaInfo) error
   func (c *MemgraphController) assessReplicaRecoveryNeeds(clusterState *ClusterState) []ReplicaRecoveryAction
   ```

3. **Recovery action types**:
   ```go
   type ReplicaRecoveryAction struct {
       ReplicaName string
       ActionType  string // "re-register", "restart-pod", "manual-intervention"
       Priority    int    // SYNC replicas = high priority
       Reason      string
   }
   ```

4. **Integration points**:
   - Add recovery assessment to main reconciliation loop
   - Execute recovery actions during replication configuration phase
   - Implement exponential backoff for failed recovery attempts

---

## Stage 4: Enhanced Monitoring and Alerting
**Goal**: Provide comprehensive monitoring of replication health via data_info
**Success Criteria**: 
- Health metrics exposed via HTTP status endpoint
- Detailed logging of replication health changes
- Early warning system for degraded replication

**Tests**:
- Test HTTP endpoint includes replica health summary
- Test health transition logging (healthy→unhealthy, recovery events)
- Test alert conditions (SYNC replica failures, high lag warnings)

**Status**: Not Started

### Implementation Details:
1. **Extend status API** in `pkg/controller/status_api.go`:
   ```go
   type ReplicaStatus struct {
       Name        string `json:"name"`
       Address     string `json:"address"`
       SyncMode    string `json:"sync_mode"`
       IsHealthy   bool   `json:"is_healthy"`
       Behind      int    `json:"behind,omitempty"`
       Status      string `json:"status,omitempty"`
       LastChecked string `json:"last_checked"`
       ErrorReason string `json:"error_reason,omitempty"`
   }
   ```

2. **Add health transition logging**:
   ```go
   func (c *MemgraphController) logReplicaHealthTransition(replicaName string, oldHealth, newHealth *DataInfoStatus)
   ```

3. **Alerting conditions**:
   - SYNC replica becomes unhealthy → Critical alert
   - Multiple ASYNC replicas unhealthy → Warning alert  
   - Replica lag exceeds threshold → Info alert
   - Recovery actions succeeding/failing → Status updates

---

## Stage 5: Integration Testing and Documentation
**Goal**: Comprehensive testing and documentation of data_info functionality
**Success Criteria**:
- E2E tests validate data_info parsing in real cluster scenarios
- Documentation updated with replication health monitoring capabilities
- Known data_info values documented from testing

**Tests**:
- E2E test that intentionally breaks replication and verifies detection
- E2E test of automatic recovery mechanisms
- Performance test of health monitoring overhead
- Integration test with existing controller functionality

**Status**: Not Started

### Implementation Details:
1. **E2E test scenarios**:
   - Simulate network partitions causing replication failures
   - Test pod restarts and recovery
   - Test SYNC replica failure and automatic replacement

2. **Documentation updates**:
   - Update CLAUDE.md with data_info monitoring capabilities
   - Document health assessment rules and recovery strategies
   - Expand data_info values section with findings from testing

3. **Performance considerations**:
   - Measure overhead of data_info parsing on reconciliation cycles
   - Implement caching if health checks become expensive
   - Consider rate limiting for recovery attempts

---

## Dependencies and Risks

### Dependencies:
- Current connectivity fix (Pod IP addressing) should be working
- Understanding of all possible data_info field values from real testing
- Access to cluster scenarios that generate unhealthy replication states

### Risks:
- **JSON parsing complexity**: data_info format may be more complex than documented examples
- **Recovery action safety**: Automatic recovery could cause additional disruption if not carefully implemented  
- **Performance impact**: Additional parsing and health checks may slow reconciliation
- **State management**: Need to track health transitions and recovery attempts to prevent infinite loops

### Mitigation Strategies:
- Implement robust error handling and graceful degradation for parsing failures
- Add feature flags to disable automatic recovery if needed
- Implement circuit breakers for recovery attempts
- Comprehensive logging and monitoring of data_info functionality

---

## Success Metrics

### Stage 1-2 Success:
- All SHOW REPLICAS calls include parsed data_info health status
- Health assessment correctly identifies known good/bad scenarios
- Zero parsing errors on well-formed data_info values

### Stage 3-4 Success:  
- Automatic recovery successfully resolves >90% of detectable replication failures
- SYNC replica failures are detected and resolved within 30 seconds
- No false positive recovery attempts on healthy replicas

### Stage 5 Success:
- E2E tests demonstrate end-to-end replication health monitoring
- Documentation enables users to understand and troubleshoot replication health
- Performance impact <5% on reconciliation cycle time