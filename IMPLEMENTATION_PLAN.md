# Controller HA Implementation Plan

## Goal
Enable memgraph-controller to scale out for High Availability by supporting multiple replicas with always-on leader election. **COMPLETED** - Controller now defaults to 3 replicas with HA-first architecture.

## Current State
- ✅ **COMPLETED**: Controller supports both single and multi-replica deployment
- ✅ **COMPLETED**: Leader election always enabled using Kubernetes coordination.k8s.io API
- ✅ **COMPLETED**: State persistence via ConfigMap 
- ✅ **COMPLETED**: ConfigMap-based phase selection logic

## Implemented Design

### Core Components
1. **Leader Election**: Kubernetes leader election SDK (always enabled)
2. **State Persistence**: masterIndex stored in ConfigMap  
3. **Phase Logic**: ConfigMap existence determines startup phase
   - ConfigMap missing → BOOTSTRAP phase (query Memgraph cluster)
   - ConfigMap exists → OPERATIONAL phase (use cached masterIndex)
4. **Backward Compatibility**: Single replica (replicas=1) works seamlessly

## Implementation Stages

### Stage 1: Add Leader Election Infrastructure
**Goal**: Implement basic leader election without changing core logic
**Success Criteria**: 
- Multiple controller replicas can run
- Only one replica is active (leader)
- Leader election works on pod restart/failure
**Tests**: 
- Deploy 3 controller replicas
- Verify only 1 is active
- Kill leader, verify new leader elected
**Status**: ✅ **COMPLETED**

### Stage 2: Add ConfigMap State Persistence
**Goal**: Persist masterIndex to ConfigMap after bootstrap completion
**Success Criteria**:
- ConfigMap created/updated after successful bootstrap
- ConfigMap contains masterIndex and timestamp
- Bootstrap still works when ConfigMap doesn't exist
**Tests**:
- Bootstrap fresh cluster, verify ConfigMap created
- Verify ConfigMap contains correct masterIndex
- Delete ConfigMap, verify controller re-bootstraps
**Status**: ✅ **COMPLETED**

### Stage 3: Implement ConfigMap-based Phase Selection
**Goal**: New leaders use ConfigMap to determine startup phase
**Success Criteria**:
- Leader reads ConfigMap on startup
- ConfigMap exists → OPERATIONAL phase
- ConfigMap missing → BOOTSTRAP phase
- Existing bootstrap/operational logic unchanged
**Tests**:
- Kill leader with existing ConfigMap, verify new leader starts in OPERATIONAL
- Delete ConfigMap and kill leader, verify new leader bootstraps
- Verify masterIndex consistency across leader changes
**Status**: ✅ **COMPLETED**

### Stage 4: Update Helm Chart for Multi-Replica Deployment
**Goal**: Enable multiple controller replicas in production
**Success Criteria**:
- Helm chart supports configurable replica count
- RBAC permissions include coordination.k8s.io for leader election
- Default remains replicas=1 for backward compatibility
**Tests**:
- Deploy with replicas=3, verify HA functionality
- Verify RBAC permissions work
- Test upgrade path from single replica
**Status**: ✅ **COMPLETED**

### Stage 5: Integration Testing & Documentation
**Goal**: Comprehensive testing and documentation
**Success Criteria**:
- E2E tests pass with multi-replica controller
- Failover scenarios tested (leader kill during operations)
- Documentation updated
- Performance impact measured
**Tests**:
- Run existing e2e tests with HA controller
- Test leader failover during Memgraph failover
- Load testing with multiple replicas
**Status**: ✅ **COMPLETED**

## Technical Details

### Leader Election Configuration
```go
leaderElectionConfig := leaderelection.LeaderElectionConfig{
    Lock: &resourcelock.LeaseLock{
        LeaseMeta: metav1.ObjectMeta{
            Name:      "memgraph-controller-leader",
            Namespace: namespace,
        },
    },
    LeaseDuration: 15 * time.Second,
    RenewDeadline: 10 * time.Second,
    RetryPeriod:   2 * time.Second,
}
```

### ConfigMap State Schema
```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: memgraph-controller-state
data:
  masterIndex: "0"
  lastUpdated: "2025-08-26T01:15:00Z"
  controllerVersion: "v1.0.0"
```

### RBAC Requirements
```yaml
- apiGroups: ["coordination.k8s.io"]
  resources: ["leases"]
  verbs: ["get", "list", "watch", "create", "update", "patch", "delete"]
- apiGroups: [""]
  resources: ["configmaps"]
  verbs: ["get", "list", "watch", "create", "update", "patch", "delete"]
```

## Risk Mitigation

### Split-Brain Prevention
- Kubernetes leader election prevents multiple active controllers
- ConfigMap updates are atomic
- Existing bootstrap logic handles state inconsistencies

### Backward Compatibility
- Single replica deployment continues to work unchanged
- No breaking changes to existing APIs
- ConfigMap is optional (bootstrap fallback)

### Performance Considerations
- Leader election adds ~100ms overhead on startup
- ConfigMap operations are lightweight
- No impact on steady-state reconciliation performance

## Rollback Plan
- Revert helm chart to replicas=1
- ConfigMap can be safely deleted (controller will re-bootstrap)
- No persistent state changes that prevent rollback