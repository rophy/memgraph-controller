# Implementation Plan: Design-Contract-Based Failover Logic

## Overview

Replace the current discovery-based failover logic with design-contract-based logic that leverages the README.md guarantee: "In OPERATIONAL state, either pod-0 OR pod-1 MUST be SYNC replica." This eliminates complex runtime discovery during failover and ensures guaranteed zero data loss.

## Root Cause Analysis

**Current Problem**: Over-engineered failover logic that tries to discover SYNC replicas at runtime:

1. **Complex discovery chain**: Main fails → Query remaining pods → Detect SYNC replica → Fallback to "best async"
2. **Information unavailable**: SYNC replica info stored in failed main pod → Cannot be queried during failover
3. **Misleading fallback**: `selectBestAsyncReplica()` promotes what should be the SYNC replica but labels it as "potential data loss"
4. **Unnecessary complexity**: Discovery logic when design contract already defines the answer

**Key Insight**: The README.md design contract eliminates the need for runtime discovery. In OPERATIONAL state, we KNOW by design which pod is the SYNC replica.

## Stage 1: Design-Contract-Based Failover Logic
**Goal**: Replace discovery-based logic with design contract enforcement
**Success Criteria**: Failover always promotes SYNC replica with zero data loss guarantee
**Tests**: E2E tests show "SYNC replica failover" instead of "ASYNC replica failover"
**Status**: Not Started

### Current vs Correct Design:

**Current (WRONG - Discovery-Based)**:
```go
// Try to discover SYNC replica at runtime (but info is in failed main!)
var syncReplica *PodInfo
for _, podInfo := range clusterState.Pods {
    if podInfo.IsSyncReplica {  // This info may not be available!
        syncReplica = podInfo
    }
}

if syncReplica != nil {
    newMain = syncReplica  // Ideal case
} else {
    // Fallback - but this should never happen in OPERATIONAL state!
    newMain = c.selectBestAsyncReplica(healthyReplicas, clusterState.TargetMainIndex)
    log.Printf("⚠️ WARNING: Potential data loss")  // Wrong warning!
}
```

**Correct (README Design Contract)**:
```go
// Design contract: In OPERATIONAL state, pod-0 or pod-1 MUST be SYNC replica
if clusterState.StateType == "OPERATIONAL_STATE" {
    failedMainIndex := c.config.ExtractPodIndex(oldMain)
    
    // The other pod (0 or 1) MUST be the SYNC replica by design
    var newMainIndex int
    if failedMainIndex == 0 {
        newMainIndex = 1  // pod-1 is SYNC replica
    } else {
        newMainIndex = 0  // pod-0 is SYNC replica  
    }
    
    newMainName := c.config.GetPodName(newMainIndex)
    newMain = clusterState.Pods[newMainName]
    promotionReason = "SYNC replica failover (design contract guarantee)"
    
    log.Printf("✅ SYNC REPLICA FAILOVER: Promoting %s (guaranteed zero data loss by design)", newMainName)
}
```

### Tasks:
- [ ] Replace discovery logic in `handleMainFailover()` with design contract logic
- [ ] Remove `selectBestAsyncReplica()` fallback for OPERATIONAL state
- [ ] Update promotion logging to reflect zero data loss guarantee
- [ ] Add design contract validation (ensure failed main is pod-0 or pod-1)
- [ ] Test that E2E logs show "SYNC replica failover" instead of "ASYNC replica failover"

## Stage 2: Cleanup Redundant Code
**Goal**: Remove over-engineered discovery code that is no longer needed
**Success Criteria**: Cleaner, simpler failover logic with fewer edge cases
**Tests**: All existing E2E tests continue to pass
**Status**: Not Started

### Cleanup Tasks:
- [ ] Remove or simplify `selectBestAsyncReplica()` function (only needed for non-OPERATIONAL states)
- [ ] Remove SYNC replica discovery loop during failover
- [ ] Clean up misleading function names and comments
- [ ] Simplify failover decision tree
- [ ] Update error messages to reflect design contract assumptions

## Stage 3: Design Contract Validation
**Goal**: Add validation that the design contract is being followed
**Success Criteria**: Early detection if deployment violates 2-pod MAIN/SYNC design
**Tests**: Controller logs warnings if design contract is violated
**Status**: Not Started

### Validation Tasks:
- [ ] Add startup validation that only pods 0 and 1 can be main-eligible
- [ ] Validate that exactly one of pod-0 or pod-1 is SYNC replica in OPERATIONAL state  
- [ ] Add alerts if 3+ pod deployment tries to make pod-2 a main
- [ ] Document the 2-pod MAIN/SYNC strategy constraints clearly
- [ ] Test behavior with various deployment configurations

## Success Metrics

- **Zero Data Loss**: Failover always promotes SYNC replica (no "potential data loss" warnings)
- **Correct Logging**: E2E tests show "SYNC replica failover (design contract guarantee)"
- **Simplified Logic**: Fewer lines of code, fewer edge cases
- **Design Compliance**: Clear validation that deployment follows README.md contract
- **Performance**: Maintain < 1 second failover timing (currently 525ms)

## Definition of Done

- [ ] Failover logic uses design contract instead of runtime discovery
- [ ] E2E tests show "SYNC replica failover" instead of "ASYNC replica failover"  
- [ ] No "potential data loss" warnings in OPERATIONAL state failover
- [ ] Code is simpler and more maintainable
- [ ] Design contract violations are detected and logged
- [ ] All existing performance benchmarks maintained