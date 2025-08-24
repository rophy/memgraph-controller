# Controller Refactoring Implementation Plan

## Overview

Break down the monolithic `pkg/controller/controller.go` (3,011 lines, 64 methods) into focused, testable modules with clear separation of concerns.

**Goal**: Improve maintainability while preserving all existing functionality and ensuring E2E tests continue to pass.

## Strategy

- **Incremental approach**: One module at a time with thorough testing
- **Preserve behavior**: No functional changes, only structural reorganization  
- **Test-driven**: Unit tests pass after each stage, E2E validation before next stage
- **Clean interfaces**: Public methods remain accessible, private implementation details stay private

## Refactoring Stages

### Stage 1: Discovery and Bootstrap Logic
**Goal**: Extract cluster discovery and bootstrap validation  
**Target size**: ~400-500 lines → `discovery.go`

**Functions to extract**:
- `DiscoverCluster()` - Main cluster discovery orchestration
- `performBootstrapValidation()` - Safety validation for initial startup
- `selectMasterAfterQuerying()` - Master selection based on Memgraph state
- `applyDeterministicRoles()` - Fresh cluster role assignment
- `learnExistingTopology()` - Operational topology learning

**Success Criteria**:
- All discovery-related unit tests pass
- Controller can successfully discover cluster state
- Bootstrap validation prevents unsafe startups
- Master selection works correctly

**Tests to update/create**:
- `TestDiscoverCluster_*` - Cluster discovery scenarios
- `TestBootstrapValidation_*` - Safety validation logic
- `TestSelectMasterAfterQuerying_*` - Master selection from Memgraph state

**Status**: ✅ Complete

---

### Stage 2: Master Selection and Topology Management  
**Goal**: Extract master selection algorithms and topology enforcement  
**Target size**: ~500-600 lines → `topology.go`

**Functions to extract**:
- `enhancedMasterSelection()` - Priority-based master selection
- `countHealthyPods()`, `buildDecisionFactors()`, `isPodHealthyForMaster()` - Health assessment
- `handleNoSafePromotion()`, `validateMasterSelection()` - Selection validation
- `enforceExpectedTopology()`, `resolveSplitBrain()`, `enforceKnownTopology()` - Topology enforcement
- `promoteExpectedMaster()` - Master promotion logic

**Success Criteria**:
- Master selection algorithms work correctly
- Split-brain resolution functions properly  
- Topology enforcement maintains expected state
- All selection criteria and priorities preserved

**Tests to update/create**:
- `TestEnhancedMasterSelection_*` - Master selection priority logic
- `TestTopologyEnforcement_*` - Split-brain and drift correction
- `TestMasterValidation_*` - Selection validation and safety checks

**Status**: Not Started

---

### Stage 3: Failover Detection and Handling
**Goal**: Extract failover detection and promotion logic  
**Target size**: ~300-400 lines → `failover.go`

**Functions to extract**:
- `detectMasterFailover()` - Failover condition detection
- `handleMasterFailover()` - Failover orchestration  
- `selectBestAsyncReplica()` - Replica selection for promotion
- `handleMasterFailurePromotion()` - Master failure promotion logic
- `identifyFailedMasterIndex()` - Failed master identification

**Success Criteria**:
- Failover detection accurately identifies master failures
- SYNC replica promotion works correctly (critical for zero data loss)
- Failover completes within acceptable time bounds
- No split-brain scenarios introduced

**Tests to update/create**:
- `TestMasterFailoverDetection_*` - Failover condition detection
- `TestFailoverPromotion_*` - SYNC replica promotion logic
- `TestFailoverTiming_*` - Failover completion timing

**Status**: Not Started

---

### Stage 4: Replication Configuration
**Goal**: Extract replication setup and management  
**Target size**: ~700-800 lines → `replication.go`

**Functions to extract**:
- `ConfigureReplication()` - Main replication configuration orchestration
- `selectSyncReplica()`, `identifySyncReplica()` - SYNC replica selection
- `configureReplicationWithEnhancedSyncStrategy()` - Enhanced replication setup
- `ensureCorrectSyncReplica()` - SYNC replica enforcement  
- `configureAsyncReplicas()`, `configurePodAsAsyncReplica()` - ASYNC replica setup
- `promoteAsyncToSync()`, `demoteSyncToAsync()` - Replica mode transitions
- SYNC replica health monitoring functions

**Success Criteria**:
- SYNC replication configured correctly (critical for data safety)
- ASYNC replicas register properly  
- Replica promotions/demotions work seamlessly
- Health monitoring detects and handles failures

**Tests to update/create**:
- `TestSyncReplicaConfiguration_*` - SYNC replica setup and validation
- `TestAsyncReplicaManagement_*` - ASYNC replica operations
- `TestReplicaTransitions_*` - SYNC↔ASYNC transitions

**Status**: Not Started

---

### Stage 5: Controller Runtime and Operations  
**Goal**: Extract controller lifecycle and operational functions  
**Target size**: ~400-500 lines → `runtime.go`

**Functions to extract**:
- `Run()`, `worker()`, `processReconcileRequest()` - Controller runtime
- `setupInformers()`, `onPod*()` event handlers - Kubernetes event handling
- `reconcileWithBackoff()`, reconciliation mechanics - Retry and backoff logic
- `enqueuePodEvent()`, `enqueueReconcile()` - Event queuing
- Metrics and monitoring functions

**Success Criteria**:
- Controller starts and runs correctly
- Kubernetes event handling works properly
- Reconciliation loop maintains cluster state  
- Metrics and observability preserved

**Tests to update/create**:
- `TestControllerRuntime_*` - Lifecycle and event handling
- `TestReconciliation_*` - Reconciliation loop and retry logic
- `TestEventHandling_*` - Kubernetes event processing

**Status**: Not Started

---

### Stage 6: Pod Operations and Utilities
**Goal**: Extract pod manipulation and utility functions  
**Target size**: ~200-300 lines → `pod_ops.go`

**Functions to extract**:
- `promoteToMaster()`, `demoteToReplica()` - Pod role changes
- `RefreshPodInfo()` - Pod information refresh
- `enforceMasterAuthority()`, `selectLowestIndexMaster()` - Authority enforcement
- `TestConnection()`, `TestMemgraphConnections()` - Connection testing
- `updateGatewayMaster()` - Gateway integration

**Success Criteria**:
- Pod promotion/demotion works correctly
- Master authority enforcement prevents conflicts
- Gateway integration maintains connectivity
- All utility functions preserved

**Tests to update/create**:
- `TestPodOperations_*` - Promotion/demotion operations
- `TestMasterAuthority_*` - Authority enforcement logic  
- `TestGatewayIntegration_*` - Gateway connectivity

**Status**: Not Started

---

## Final Structure

After completion, the controller package will have:

- **`controller.go`** (~800-1000 lines) - Core orchestration and public interface
- **`discovery.go`** (~400-500 lines) - Cluster discovery and bootstrap
- **`topology.go`** (~500-600 lines) - Master selection and topology management
- **`failover.go`** (~300-400 lines) - Failover detection and handling  
- **`replication.go`** (~700-800 lines) - Replication configuration
- **`runtime.go`** (~400-500 lines) - Controller runtime and operations
- **`pod_ops.go`** (~200-300 lines) - Pod operations and utilities

**Total reduction**: ~3,011 lines → ~800-1000 lines main controller (67-75% reduction)

## Testing Protocol

**After each stage**:
1. **Unit tests**: `make test` must pass
2. **Build verification**: `go build ./...` must succeed  
3. **E2E validation**: Human runs `make test-e2e` to verify functionality
4. **Commit**: Create clean commit for the completed stage

**Before proceeding to next stage**:
- Confirm E2E tests pass
- Review extracted module for clean interfaces
- Ensure no behavioral changes introduced

## Implementation Notes

- **Preserve all existing functionality** - This is purely structural refactoring
- **Maintain backward compatibility** - All public methods remain accessible
- **Keep consistent error handling** - Preserve existing error patterns
- **No performance regressions** - Controller should perform identically
- **Clean commit history** - One commit per completed stage for easy rollback