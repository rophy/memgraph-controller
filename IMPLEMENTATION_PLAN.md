# Implementation Plan: Fix Prestop Hook Deadlock

## Problem Statement

The current controller has a fundamental design conflict that causes deadlocks during rolling restarts:

1. **Controller triggers failover on pod deletion** (controller_events.go:179-181)
2. **Prestop hook waits for cluster health** before allowing pod termination

This creates a deadlock:
```
T0: pod-1 is main, pod-0 is sync replica (healthy)
T1: kubectl delete pod-1 (rolling restart)
T2: Controller detects deletion → triggers failover → promotes pod-0 to main
T3: Prestop hook fires → tries to wait for "main + 2 healthy replicas"
T4-TN: DEADLOCK
  - Dual-main state: pod-1 still "main" in Memgraph, pod-0 now "target main"
  - Can't register pod-1 as replica (FQDN broken during Terminating state)
  - Prestop hook waits forever for health that can't be achieved
```

## Root Cause

**Incorrect failover trigger semantics**:
- Current: Pod deletion event → immediate failover
- Problem: Deletion is a **planned operation**, not a failure
- Conflict: Prestop hook is designed to manage planned shutdown, but failover interferes

## Solution: Option 1 - Remove Failover Trigger from Pod Deletion

### Goal
Separate planned shutdown (managed by prestop hook) from unplanned failures (managed by failover).

### Success Criteria
- [ ] No dual-main state during rolling restart
- [ ] Prestop hook can achieve cluster health and complete successfully
- [ ] Rolling restart E2E tests pass consistently
- [ ] Unplanned failures (pod crashes) still trigger failover correctly

### Implementation

## Stage 1: Remove Failover Trigger from onPodDelete
**Goal**: Stop triggering failover when main pod deletion is detected
**Success Criteria**:
- onPodDelete no longer calls enqueueFailoverCheckEvent for main pod
- Main pod deletion only triggers reconciliation
- Replica pod deletion still handled correctly (read gateway switch)
**Tests**: Unit test for onPodDelete behavior
**Status**: Not Started

**Changes**:
1. Modify `internal/controller/controller_events.go:151-190` (onPodDelete function)
2. Remove failover trigger for main pod deletion
3. Keep reconciliation trigger for cleanup
4. Keep replica deletion handling for read gateway

**Expected behavior**:
```go
func (c *MemgraphController) onPodDelete(obj interface{}) {
    // ... existing code ...

    if currentMain != "" && pod.Name == currentMain {
        logger.Info("Main pod deletion detected - NOT triggering failover (prestop hook will manage transition)",
            "pod_name", pod.Name)
        // Don't trigger failover - prestop hook manages planned shutdown
        // Failover only for unplanned failures via onPodUpdate
    } else {
        // Handle replica deletion for read gateway
        c.handleReplicaDeleted(ctx, pod)
    }

    // Still trigger reconciliation for cleanup
    logger.Info("onPodDelete: pod deleted, triggering reconciliation", "pod_name", pod.Name)
    c.enqueueReconcileEvent(ctx, "pod-delete", "pod-deleted", pod.Name)
}
```

## Stage 2: Ensure Failover Still Works for Unplanned Failures
**Goal**: Verify failover triggers correctly for actual pod failures
**Success Criteria**:
- Pod readiness probe failure triggers failover via onPodUpdate
- Pod crash triggers failover
- Failover E2E tests pass
**Tests**: Failover E2E tests, crash simulation tests
**Status**: Not Started

**Verification**:
- onPodUpdate with health status change still calls `enqueueFailoverCheckEvent`
- isPodBecomeUnhealthy() correctly detects readiness failures
- Failover queue processes health-based failures

**No code changes needed** - this path already exists in controller_events.go:133-136

## Stage 3: Update Reconciliation to Handle Post-Deletion Cleanup
**Goal**: Ensure reconciliation properly handles cleanup after pod deletion
**Success Criteria**:
- Reconciliation detects missing target main pod
- Reconciliation triggers appropriate recovery actions
- No orphaned replication configurations
**Tests**: Reconciliation unit tests, integration tests
**Status**: Not Started

**Verification**:
- Check `internal/controller/controller_reconcile.go:97-155` (performReconciliationActions)
- Verify behavior when target main pod is not found
- Ensure reconciliation can trigger recovery if needed

**Potential gap**: If reconciliation needs to trigger failover after detecting missing main pod, add logic:
```go
// In performReconciliationActions, after detecting target main is gone:
if targetMainPod is missing {
    logger.Warn("Target main pod is missing, triggering failover via reconciliation")
    c.enqueueFailoverCheckEvent(ctx, "reconciliation", "target-main-missing", targetMainPodName)
}
```

## Stage 4: Run E2E Tests
**Goal**: Validate the fix with rolling restart E2E tests
**Success Criteria**:
- test_rolling_restart_continuous_availability passes consistently
- test_rolling_restart_with_main_changes passes consistently
- No prestop hook deadlocks (pods terminate within expected time)
- No data divergence
**Tests**: Full E2E test suite, repeated runs (10+)
**Status**: Not Started

**Test command**:
```bash
tests/scripts/repeat-e2e-tests.sh 10
```

## Stage 5: Update Design Documentation
**Goal**: Document the correct failover trigger semantics
**Success Criteria**:
- design/failover.md clearly states failover triggers
- design/reconciliation.md updated if needed
- KNOWN_ISSUES.md updated with resolution
**Tests**: N/A (documentation)
**Status**: Not Started

**Updates needed**:
- `design/failover.md` line 56-66: Clarify "Failover Triggers" section
  - Remove "Main pod deletion detected via Kubernetes informer"
  - Emphasize "Main pod readiness probe failure" as the primary trigger
- Add note: "Pod deletion is a planned operation managed by prestop hook, not a failure requiring failover"

## Alternative: Option 3 - Prestop Hook Coordinates Failover

**If Option 1 proves insufficient**, implement this alternative:

### Changes
1. Prestop hook explicitly triggers failover for main pod
2. Prestop hook waits for failover completion synchronously
3. Add `isHealthyExcludingPod()` helper to check cluster health excluding terminating pod
4. Modify failover queue to support synchronous completion channels

### Benefits
- Explicit control of failover timing
- No race condition between prestop and failover
- Clear ownership of shutdown sequence

### Drawbacks
- More complex implementation
- Requires failover queue modifications
- Need new helper functions

## Design Compliance

### Implements
- `design/failover.md` - Corrects failover trigger semantics (unplanned failures only)
- `design/reconciliation.md` - Reconciliation handles cleanup after deletion

### No Contradictions
- Prestop hook continues to ensure cluster health before termination
- Failover continues to handle unplanned failures
- Reconciliation continues to maintain cluster state

## Key Decisions

1. **Pod deletion is NOT a failure** - it's a planned operation managed by prestop hook
2. **Failover is for unplanned failures** - detected via health status changes (onPodUpdate)
3. **Prestop hook owns planned shutdown** - waits for cluster health before allowing termination
4. **Reconciliation handles cleanup** - processes the aftermath of deletion events

## Rollback Plan

If this approach causes issues:
1. Revert changes to controller_events.go:onPodDelete
2. Re-enable failover trigger on deletion
3. Consider Option 3 (prestop hook coordinates failover) as alternative
