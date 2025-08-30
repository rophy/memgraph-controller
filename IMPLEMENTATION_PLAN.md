# Implementation Plan: Controller Simplification

## Overview

This implementation plan simplifies the current Memgraph controller codebase to eliminate race conditions and align with the contract-based DESIGN.md specification. The goal is to reduce complexity by 60-70% while fixing the 20% E2E test failure rate.

## Root Cause Analysis

**Current Issue**: Gateway routing race condition during failover (KNOWN_ISSUES.md)
- 20% failure rate due to timing gap between Memgraph role changes and gateway updates
- Complex discovery-based logic creates delays in failover coordination

**Root Fix Strategy**: Replace discovery-based logic with authority-based design contract
- Controller maintains `TargetMainPod` and `TargetSyncReplica` authority from bootstrap
- Failover = atomic state flip + immediate gateway notification + best-effort Memgraph commands

## Stage 1: Simplify Bootstrap Phase

**Goal**: Replace complex BootstrapController with simple DESIGN.md bootstrap rules
**Success Criteria**: Bootstrap follows exact 7-step process from DESIGN.md
**Status**: Not Started

### Implementation Tasks

1. **Remove Complex Bootstrap Logic**:
   - Delete `pkg/controller/bootstrap.go` (entire BootstrapController class)
   - Remove complex state classification methods
   - Remove multi-phase bootstrap workflow

2. **Implement Simple Bootstrap Method**:
   - Create `executeBootstrap(ctx)` method in controller.go
   - Implement exact DESIGN.md rules:
     1. Check configmap existence
     2. Load state if exists → go to OPERATIONAL  
     3. If not leader, wait for leader or become leader
     4. Wait for pod-0, pod-1 readiness
     5. Check both MAIN + empty data → initialize
     6. Check one MAIN, one REPLICA → learn existing → go to OPERATIONAL
     7. Otherwise → crash immediately

3. **Simplify ConfigMap State**:
   - Remove `StateManager` complexity
   - Store only `masterIndex` (0 or 1) in configmap
   - Remove `LastUpdated`, `ControllerVersion`, `BootstrapCompleted` fields

### Tests
- Bootstrap handles fresh cluster (both pods MAIN, empty data)
- Bootstrap handles existing cluster (one MAIN, one REPLICA) 
- Bootstrap crashes on unknown states
- Bootstrap waits for pod readiness
- Bootstrap respects leader election

## Stage 2: Implement Exact Reconciliation Algorithm

**Goal**: Replace scattered reconciliation methods with single 10-step DESIGN.md algorithm  
**Success Criteria**: Reconciliation follows exact steps 1-10 from DESIGN.md
**Status**: Not Started

### Implementation Tasks

1. **Remove Scattered Reconciliation Methods**:
   - Delete `enforceExpectedTopology()`
   - Delete `ConfigureReplication()`  
   - Delete `SyncPodLabels()`
   - Delete complex discovery methods in `discovery.go`

2. **Implement Single Reconciliation Method**:
   - Create `performReconciliation(ctx)` implementing exact 10 steps:
     - Step 1: Check OPERATIONAL phase
     - Step 2: Use `TargetMainPod`, `TargetSyncReplica` from controller state
     - Steps 3-10: Follow DESIGN.md exactly

3. **Authority-Based State Management**:
   - Controller maintains `TargetMainPod`, `TargetSyncReplica` from bootstrap
   - No runtime discovery - use persisted state authority
   - ConfigMap as single source of truth for controller coordination

### Tests  
- Reconciliation handles TargetMainPod not ready
- Reconciliation manages SYNC replica replication status
- Reconciliation handles ASYNC replica registration/dropping
- Reconciliation respects leader/non-leader roles
- Non-leaders update local state from configmap changes

## Stage 3: Simplify Failover Logic

**Goal**: Replace complex failover with simple design-contract failover
**Success Criteria**: Sub-second failover with zero race conditions
**Status**: Not Started

### Implementation Tasks

1. **Remove Complex Failover Logic**:
   - Simplify `pkg/controller/failover.go`
   - Remove discovery-based replica selection
   - Remove complex state classification for failover

2. **Implement Authority-Based Failover**:
   ```go
   func (c *Controller) handleFailover(ctx context.Context) error {
       // Authority-based failover - no discovery needed
       if !c.targetMainPodReady() {
           c.flipMainAndSyncReplica()           // Atomic state update
           c.updateConfigMap()                  // Coordinate other controllers
           c.notifyGateway()                   // Immediate gateway switch  
           c.promoteMemgraphRole()             // Best-effort, non-blocking
       }
   }
   ```

3. **Atomic State Management**:
   - Single atomic update of `TargetMainPod` ↔ `TargetSyncReplica`
   - Immediate configmap update for controller coordination
   - Gateway notification happens instantly, not after Memgraph role confirmation

### Tests
- Failover completes in <1 second
- Gateway switches immediately on controller state change
- ConfigMap coordinates multiple controller instances
- Memgraph promotion is best-effort (doesn't block failover)
- No race conditions in repeated failover tests

## Stage 4: Simplify Gateway Integration  

**Goal**: Implement simple 2-rule gateway from DESIGN.md
**Success Criteria**: Gateway follows exact connection rules, eliminates race conditions
**Status**: Not Started

### Implementation Tasks

1. **Implement Simple Gateway Rules**:
   - Rule 1: New connection → check `TargetMainPod` ready → proxy or reject
   - Rule 2: `TargetMainPod` changed → immediately disconnect all connections

2. **Authority-Based Gateway**:
   - Gateway uses controller `TargetMainPod` authority, not discovery
   - No waiting for Memgraph role confirmation
   - Immediate response to controller state changes

3. **Connection Management**:
   - Immediate disconnection on failover (not graceful draining)  
   - Reject new connections during target pod not-ready state
   - Simple and deterministic behavior

### Tests
- Gateway rejects connections when TargetMainPod not ready
- Gateway disconnects immediately on TargetMainPod change  
- Gateway switches without waiting for Memgraph role changes
- No "write forbidden on replica" errors in E2E tests

## Stage 5: Integration Testing and Cleanup

**Goal**: Verify simplified controller eliminates race conditions  
**Success Criteria**: E2E tests pass 100% of the time (vs current 80%)
**Status**: Not Started

### Implementation Tasks

1. **Remove Dead Code**:
   - Delete unused discovery methods
   - Delete complex state classification
   - Clean up imports and dependencies

2. **Integration Testing**:
   - Run E2E tests 20+ times to verify 0% failure rate
   - Test rapid successive failovers  
   - Test multiple controller coordination
   - Verify bootstrap handles all scenarios

3. **Performance Validation**:
   - Measure failover latency (target: <1 second)
   - Verify gateway switching speed
   - Validate controller coordination timing

### Tests
- 20+ consecutive E2E test runs with 0 failures
- Failover completes in <1 second consistently
- Multiple controller instances coordinate properly
- Bootstrap handles fresh and existing clusters
- No gateway routing errors under stress testing

## Implementation Guidelines

### Design Compliance Requirements

**Before ANY code changes**:
1. Quote the exact DESIGN.md section being implemented
2. Verify no contradictions with any part of DESIGN.md  
3. Reference design requirement in code comments
4. Use authority-based logic, not discovery-based

### Forbidden Patterns

❌ **Discovery-based logic** not specified in DESIGN.md
❌ **Complex algorithms** beyond design scope  
❌ **Smart logic** handling edge cases outside design
❌ **Runtime topology discovery** instead of controller authority

### Required Patterns  

✅ **Direct DESIGN.md implementation** with quoted requirements
✅ **Authority-based decisions** using controller state
✅ **Simple deterministic logic** following design steps exactly
✅ **Contract-based failover** using `TargetMainPod`/`TargetSyncReplica`

## Expected Outcomes

- **Eliminate race conditions**: 0% E2E test failure rate (vs current 20%)
- **Reduce complexity**: Remove ~15 methods, simplify 10+ others  
- **Improve reliability**: Deterministic contract-based behavior
- **Faster failover**: <1 second failover latency
- **Code maintainability**: Clear, simple logic matching DESIGN.md exactly

## Risk Mitigation

- **Incremental implementation**: Each stage builds on previous success
- **Test-driven**: All stages have specific test requirements
- **Design compliance**: Every change references exact DESIGN.md section
- **Rollback capability**: Commits are incremental and reversible