# Implementation Plan: Fast Failover Fix

## Overview

Fix the failover blocking issue where pod-deleted events wait 23+ seconds due to slow reconciliation operations. Current implementation violates the README.md design which requires IMMEDIATE failover via state updates, not expensive Memgraph operations during failover.

## Root Cause Analysis

**Problem**: Sequential work queue processing with expensive operations blocks urgent failover events:

1. **Single worker thread** processes all events sequentially from `c.workQueue`
2. **Long reconciliation cycles** (23+ seconds) include:
   - Memgraph connection attempts to failed pods with 5s socket timeouts
   - Exponential backoff retry (1s + 2s + 4s + 8s = 15s per operation)
   - Multiple operations per pod (promotion, registration, health checks)
3. **Pod-deleted events** queue behind these expensive operations
4. **By the time failover runs**, the pod has already restarted (no failover needed)

**Key Finding**: Failover itself is fast, but reconciliation after failover tries to configure deleted pods, causing 23+ second delays in the same cycle.

## Stage 1: Minimal Failover Implementation (README Design)
**Goal**: Implement immediate failover as specified in README.md line 141
**Success Criteria**: Failover completes in < 1 second, no Memgraph operations during critical path
**Tests**: E2E failover timing, gateway switches immediately
**Status**: Not Started

### Current vs Correct Design:

**README.md line 141**: *"If sync replica status is READY, controller uses it as MAIN, and promotes it immediately"*

**Current (WRONG)**:
```go
// Expensive operation during failover - blocks for 15+ seconds if pod unreachable
err := c.memgraphClient.SetReplicationRoleToMainWithRetry(ctx, newMain.BoltAddress)
if err != nil {
    return fmt.Errorf("failed to promote new main %s: %w", newMain.Name, err)
}

// State update comes AFTER expensive operation
c.updateTargetMainIndex(ctx, clusterState, newMainIndex, "failover")
```

**Correct (README Design)**:
```go
// Step 1: Single fast promotion (no retry, pod-1 is healthy)
if err := c.memgraphClient.SetReplicationRoleToMain(ctx, newMain.BoltAddress); err != nil {
    log.Printf("Warning: Promotion failed, proceeding with state update: %v", err)
}

// Step 2: IMMEDIATE state update (triggers gateway switch)
if err := c.updateTargetMainIndex(ctx, clusterState, newMainIndex, "IMMEDIATE failover"); err != nil {
    return fmt.Errorf("CRITICAL: failed to update main index: %w", err)
}

// Step 3: Remove failed pod from reconciliation to prevent delays
delete(clusterState.Pods, oldFailedMain)

log.Printf("✅ IMMEDIATE failover completed: pod-%d is now MAIN", newMainIndex)
// Reconciliation handles cleanup asynchronously
```

### Tasks:
- [ ] Modify `handleMainFailover()` to use single-attempt promotion (no retry)
- [ ] Move state update to critical path (before expensive operations)
- [ ] Filter failed pods from cluster state to prevent reconciliation delays
- [ ] Add fast-path logging for immediate failover completion
- [ ] Test failover timing (should be < 1 second)

## Stage 2: Reconciliation Resilience 
**Goal**: Prevent reconciliation from blocking on unreachable pods
**Success Criteria**: Failed pod operations don't delay healthy pod operations
**Tests**: Reconciliation completes in < 5 seconds even with failed pods
**Status**: Not Started

### Current Problem:
`ConfigureReplication()` tries to configure deleted pods as ASYNC replicas:

```go
// This tries to contact pod-0 even when it's deleted/terminating
for podName, podInfo := range clusterState.Pods {
    if err := c.configurePodAsAsyncReplica(ctx, mainPod, podInfo); err != nil {
        // Logs warning but continues - but this took 15+ seconds per failed pod
    }
}
```

### Solution:
```go
// Skip pods that aren't ready for replication (immediate check)
for podName, podInfo := range clusterState.Pods {
    if !c.isPodHealthyForReplication(podInfo) {
        log.Printf("Skipping unhealthy pod %s during reconciliation", podName)
        continue // Don't attempt expensive operations
    }
    // Only configure healthy pods
}
```

### Tasks:
- [ ] Add `isPodHealthyForReplication()` check before expensive operations
- [ ] Implement circuit breaker for repeatedly failing pod connections
- [ ] Add timeout contexts for all Memgraph operations (max 5s total)
- [ ] Use parallel goroutines for independent pod operations
- [ ] Test reconciliation with mixed healthy/unhealthy pods

## Stage 3: Priority Event Processing
**Goal**: Process urgent failover events ahead of routine reconciliation
**Success Criteria**: Pod-deleted events processed within 1 second regardless of queue
**Tests**: Rapid pod deletions trigger immediate failover
**Status**: Not Started

### Current Problem:
```go
// Single channel processes all events FIFO
case request := <-c.workQueue:
    c.processReconcileRequest(ctx, request, workerID)
```

### Solution:
```go
// Priority channels for different event types
select {
case urgentRequest := <-c.urgentQueue:      // Pod failures, health changes
    c.processUrgentRequest(ctx, urgentRequest)
case request := <-c.workQueue:              // Normal reconciliation  
    c.processReconcileRequest(ctx, request)
}
```

### Tasks:
- [ ] Add `urgentQueue` channel for pod-deleted events
- [ ] Modify event handlers to route by urgency
- [ ] Implement separate worker for urgent events
- [ ] Test priority processing under load
- [ ] Measure event processing latency

## Stage 4: End-to-End Validation
**Goal**: Verify fast failover works in all scenarios
**Success Criteria**: E2E tests pass consistently with < 2 second failover timing
**Tests**: `TestE2E_FailoverReliability` with timing assertions
**Status**: Not Started

### Tasks:
- [ ] Add timing measurements to E2E tests
- [ ] Test various failure scenarios (main pod, sync replica, both)
- [ ] Verify gateway switches within 1 second
- [ ] Test multiple rapid failovers
- [ ] Validate no client connection errors during failover

## Dependencies

- **Kubernetes Pod Events**: Pod deletion detection must work correctly
- **Gateway Integration**: Gateway must respond to ConfigMap changes
- **Leader Election**: Only leader should perform failover decisions

## Success Metrics

- **Failover Time**: < 1 second from pod deletion to ConfigMap update
- **Gateway Switch**: < 500ms from ConfigMap change to traffic redirect
- **Client Errors**: Zero "Write queries are forbidden" errors during failover
- **Queue Blocking**: Urgent events never wait > 1 second in queue

## Definition of Done

- [ ] Pod-deleted events trigger failover in < 1 second
- [ ] Reconciliation doesn't block on failed pods
- [ ] E2E tests pass with timing assertions
- [ ] No client connection errors during failover
- [ ] Performance benchmarks meet targets