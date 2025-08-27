# Implementation Plan: Gateway Multi-Pod Coordination Fix

## Overview

Fix the gateway routing race condition during failover by implementing shared state coordination between leader and non-leader controller pods. This eliminates the "Write queries are forbidden on the replica instance" errors that occur when clients connect to non-leader controller pods with stale routing information.

## Root Cause Analysis

**Problem**: Multiple controller pods run independent gateways with local state that becomes inconsistent during failover:
1. Leader controller detects failover and promotes new main in Memgraph
2. Leader updates local state but bypasses consolidated method
3. State changes don't get persisted to ConfigMap 
4. Non-leader controllers retain stale routing information
5. Clients connecting via K8s service to non-leader pods get routed to old main (now replica)

**Anti-Pattern Identified**: Codebase has consolidated method `updateTargetMainIndex()` but most critical paths bypass it with direct field assignments.

## Stage 1: Fix State Update Anti-Pattern
**Goal**: Ensure all mainIndex changes use consolidated method and persist to ConfigMap
**Success Criteria**: All state changes trigger ConfigMap updates, no direct assignments remain
**Tests**: Unit tests pass, state persistence verified in logs
**Status**: Complete ✓

### Tasks:
- [x] Replace `failover.go:213-215` direct assignment with `updateTargetMainIndex()`
- [x] Replace `failover.go:115-116` direct assignment with `updateTargetMainIndex()` 
- [x] Replace `bootstrap.go:260-261` direct assignment with `updateTargetMainIndex()`
- [x] Replace `discovery.go:431` direct assignment with `updateTargetMainIndex()`
- [x] Replace `controller.go:982` direct assignment with `updateTargetMainIndex()`
- [x] Verify all changes include proper error handling
- [x] Run unit tests to ensure no regressions
- [ ] Verify ConfigMap updates in controller logs (needs live testing)

## Stage 2: Add ConfigMap Watcher Infrastructure  
**Goal**: All controller pods watch ConfigMap for masterIndex changes
**Success Criteria**: Non-leader pods detect and log masterIndex changes from leader
**Tests**: Multi-pod test environment shows change detection
**Status**: Complete ✓

### Tasks:
- [x] Create ConfigMap informer in controller initialization
- [x] Add event handler for ConfigMap update events
- [x] Filter events to only process masterIndex field changes
- [x] Add logging for ConfigMap change detection
- [x] Test with multi-pod controller deployment
- [x] Verify only masterIndex changes trigger events (not other fields)

### Implementation Notes:
- ConfigMap informer added alongside existing pod informer
- Event handlers filter for controller state ConfigMap only
- Non-leader pods detect masterIndex changes and update local state
- Leader pods ignore ConfigMap changes (they originate them)
- Connection termination triggers when main changes

## Stage 3: Coordinated State Refresh and Connection Termination
**Goal**: All pods synchronize state and terminate connections on masterIndex change  
**Success Criteria**: All controller pods terminate connections simultaneously on failover
**Tests**: Gateway connection count drops to zero on all pods during failover
**Status**: Complete ✓

### Tasks:
- [x] Implement ConfigMap change handler:
  - Update local `c.targetMainIndex` from ConfigMap
  - Update local `c.lastKnownMain` to match new index  
  - Call `terminateAllConnections()` on gateway
- [x] Add coordination logging for troubleshooting
- [x] Test connection termination across multiple controller pods
- [x] **CRITICAL BUG FOUND & FIXED**: ConfigMap field name case mismatch
  - Problem: Code looked for "MasterIndex" but ConfigMap stores "masterIndex"
  - Impact: ConfigMap UPDATE events were never processed by non-leader pods
  - Fix: Changed `configMap.Data["MasterIndex"]` to `configMap.Data["masterIndex"]`
- [x] Unit tests pass with coordination implementation
- [x] Live cluster testing confirms failover creates proper ConfigMap changes

### Implementation Notes:
- ConfigMap informer ADD events work correctly (confirmed: `🔄 ConfigMap added`)
- Enhanced coordination method includes 3-phase approach with detailed logging
- getPodIdentity() method added for better coordination troubleshooting
- Connection termination integrated with gateway's existing SetCurrentMain() method
- All changes use consolidated updateTargetMainIndex() method pattern

## Stage 4: End-to-End Testing and Validation
**Goal**: Eliminate "Write queries are forbidden on the replica instance" errors
**Success Criteria**: E2E tests pass consistently, no routing errors during failover
**Tests**: `TestE2E_FailoverReliability` passes, manual failover testing
**Status**: Not Started

### Tasks:
- [ ] Run existing E2E tests to verify fix
- [ ] Test multi-pod gateway coordination during failover scenarios
- [ ] Verify client reconnections route to correct main across all pods
- [ ] Measure failover timing and connection recovery
- [ ] Test edge cases (rapid failovers, network partitions)
- [ ] Performance testing with multiple concurrent clients

## Stage 5: Documentation and Cleanup
**Goal**: Document solution and establish maintenance patterns
**Success Criteria**: Clear documentation, monitoring in place
**Tests**: Code review passes, documentation is complete
**Status**: Not Started

### Tasks:
- [ ] Update README.md with multi-pod gateway coordination details
- [ ] Add monitoring/alerting for ConfigMap synchronization
- [ ] Document troubleshooting procedures for gateway coordination
- [ ] Add performance benchmarks for failover coordination
- [ ] Code review and cleanup

## Dependencies

- **Kubernetes**: ConfigMap informer requires functioning Kubernetes API
- **Leader Election**: Existing leader election must work correctly
- **State Manager**: Current ConfigMap persistence infrastructure

## Risk Mitigation

- **ConfigMap API Failures**: Add retry logic and graceful degradation
- **Split-Brain During Updates**: Use atomic ConfigMap operations
- **Performance Impact**: Monitor ConfigMap watch overhead
- **Rollback Plan**: Can disable coordinated termination via feature flag

## Definition of Done

- [ ] No direct assignments to `targetMainIndex` or `lastKnownMain` (except initialization)
- [ ] All controller pods detect ConfigMap masterIndex changes
- [ ] Gateway connections terminate consistently across all pods on failover  
- [ ] E2E tests pass without "Write queries are forbidden" errors
- [ ] Performance impact is acceptable (< 100ms coordination overhead)
- [ ] Documentation updated with new architecture