# Known Issues

## Summary (Updated 2025-09-18)

## 1. Investigation Results: Data Divergence During Rolling Restart - Wrong Root Cause Analysis

**Status**: Investigation Complete - Root Cause Corrected
**Severity**: Critical - Major Analysis Error
**Investigation Date**: 2025-09-21
**Analysis By**: Claude AI (corrected by human verification)

### Description

During investigation of rolling restart data divergence issues, Claude AI provided an **incorrect root cause analysis** claiming the issue was due to "persistent storage during StatefulSet rolling restart." This analysis was proven completely wrong through controlled experiments.

### Wrong Assumptions Made

**Claude AI's Incorrect Analysis:**
1. **False Claim**: "When pods are recreated during rolling restart, they retain old persistent data from before the failover"
2. **False Claim**: "The datasets are incompatible - you cannot set up replication from ha-1 (687 records) to ha-0/ha-2 (49 records) because they have diverged"
3. **False Claim**: "The root cause is persistent storage during StatefulSet rolling restart"
4. **False Solution**: "The fix is to clear the persistent storage of pods that are rejoining the cluster after failover"

### Experimental Proof of Wrong Analysis

**Controlled Test Setup:**
```bash
# Clean memgraph cluster without controller interference:
- memgraph-sandbox-0: main (wrote test1)
- memgraph-sandbox-1: replica
- memgraph-sandbox-2: replica
```

**Experimental Steps Proving Claude Wrong:**

1. **Write Initial Data**: Created test1 on pod-0, replicated to all pods ✓
2. **Promote Pod-1**: Made pod-1 main, wrote test2 - created data divergence ✓
3. **Delete & Recreate Pod-1**: Simulated rolling restart behavior
4. **Critical Result**: Pod-1 came back **empty** (no persistent data), contradicting Claude's claim
5. **Replication Success**: Successfully set up replication from pod-0 to recreated pod-1
6. **Data Sync**: All data (test1, test2, test3, test4) synchronized perfectly across pods

### Key Findings That Disprove Claude's Analysis

1. **Persistent Storage Behavior**:
   - **Claude claimed**: Pods retain old data after deletion/recreation
   - **Reality**: Pods start fresh with no data when recreated

2. **Replication Compatibility**:
   - **Claude claimed**: Cannot set up replication between diverged pods
   - **Reality**: Replication works fine even with data divergence

3. **Data Synchronization**:
   - **Claude claimed**: Diverged data causes permanent conflicts
   - **Reality**: Memgraph successfully syncs data across diverged pods

### Actual Evidence from Original Issue

**The real data pattern from memgraph-controller E2E test failure:**
- **memgraph-ha-0**: 49 records (05:06:28 → 05:07:58) - stopped at failover
- **memgraph-ha-1**: 687 records (05:06:28 → 05:18:40) - continued as main after failover
- **memgraph-ha-2**: 49 records (05:06:28 → 05:07:58) - stopped at failover

**The real error from Memgraph:**
```
You cannot register Replica memgraph_ha_0 to this Main because at one point
Replica memgraph_ha_0 acted as the Main instance. Both the Main and Replica
memgraph_ha_0 now hold unique data.
```

### Corrected Understanding

**What Actually Happened:**
1. **Dual Main Scenario**: During rolling restart, both pods acted as main simultaneously
2. **Split-Brain Protection**: Memgraph detected that pods had been main at different times
3. **Data Integrity Protection**: Memgraph refused replication to prevent data corruption
4. **Controller Coordination Issue**: The problem is controller/gateway coordination during rolling restart, not persistent storage

### Impact of Wrong Analysis

- **Wasted Investigation Time**: Focused on wrong root cause (persistent storage)
- **Incorrect Solutions Proposed**: Storage cleanup mechanisms that wouldn't solve the real issue
- **Missed Real Issue**: Controller coordination problems during dual-main scenarios
- **Analysis Credibility**: Demonstrates need for experimental validation of AI analysis

### Lessons Learned

1. **Experimental Validation Required**: AI analysis must be verified through controlled tests
2. **Question Assumptions**: Challenge fundamental assumptions (like persistent storage behavior)
3. **Test Edge Cases**: Verify behavior through actual system experiments
4. **Human Verification Essential**: AI can make significant analytical errors

### Correct Next Steps

1. **Focus on Controller Coordination**: Investigate why both pods act as main during rolling restart
2. **Gateway Routing Analysis**: Check if writes reach wrong pods during transitions
3. **Timing Analysis**: Examine the exact sequence during failover/rolling restart
4. **Split-Brain Prevention**: Understand how to prevent dual-main scenarios in controller

### Related Issues

- **Issue #6**: Rolling Restart Replication Failure - contains the real analysis of dual main promotion
- **Original Controller Logs**: Show the actual sequence of pod promotions/demotions during rolling restart

### Notes

- **Claude AI Limitation**: This demonstrates AI can make significant analytical errors when reasoning about complex distributed system behaviors
- **Experimental Method**: The controlled test using tests/memgraph-sandbox proved invaluable for disproving incorrect analysis
- **Real Root Cause**: See Issue #2 below - identified as replication timing coordination problem

## 2. Rolling Restart Data Divergence - Actual Root Cause Identified

**Status**: Root Cause Identified - Solution in Development
**Severity**: High - Affects rolling restart reliability
**Investigation Date**: 2025-09-21
**Analysis By**: Human analysis with Claude validation

### True Root Cause: Replication Timing Coordination Problem

After eliminating the false persistent storage hypothesis, the actual root cause has been identified as a **timing coordination issue** between StatefulSet rollout behavior and replication synchronization.

### Problem Timeline

```
T=0: main=pod-1, pod-1 replicates to pod-2
T=1: pod-2 gets deleted, pod-1 replication to pod-2 stuck
T=2: pod-2 becomes ready, StatefulSet IMMEDIATELY deletes pod-1 BEFORE pod-1 completes replicating to pod-2
T=3: pod-0 gets promoted to main, tries to register replication to pod-2, pod-2 complains data is different
```

### Critical Investigation Results

**Question**: Are pod-1 and pod-0 actually in strict_sync at T=1→T=2?
**Answer**: ✅ **YES** - This is confirmed

**Question**: Could there be data that pod-1 received but hasn't yet replicated to pod-0?
**Answer**: ❌ **NO** - Not possible with strict_sync

**Question**: Is strict_sync broken by pod-2's unavailability?
**Answer**: ❌ **NO** - Not possible

### The Replication Gap Problem

**Key Insight**: The issue occurs in the critical window at T=2:
- Pod-2 becomes ready and can accept replication
- Pod-1 still has data that pod-2 is missing (from when pod-2 was unavailable)
- StatefulSet immediately deletes pod-1 before it can catch up replication to pod-2
- Pod-0 takes over but pod-2 has diverged data, causing Memgraph's split-brain protection to trigger

### Error Message Analysis

The Memgraph error confirms this analysis:
```
You cannot register Replica memgraph_ha_0 to this Main because at one point
Replica memgraph_ha_0 acted as the Main instance. Both the Main and Replica
memgraph_ha_0 now hold unique data.
```

This indicates pod-2 was previously connected to pod-1 (when pod-1 was main) but has different data than the new main (pod-0).

### Proposed Solution: PreStop Hook Coordination

**Approach**: Use Kubernetes preStop lifecycle hook to delay pod-1 deletion until replication is synchronized.

**Implementation Plan**:
1. Add preStop hook to Memgraph StatefulSet pod spec
2. Create shell script that checks:
   - If current pod is main
   - If all async replicas have caught up
3. Delay pod termination until replication is ready
4. Use short delay (few seconds) to allow replication sync

**Why This Works**:
- Addresses the exact timing window where the problem occurs
- Uses native Kubernetes coordination mechanisms
- Minimal complexity - just delays deletion briefly
- Preserves data consistency without complex controller logic

### Implementation Details

**ConfigMap Script**: `pre-stop-hook.sh`
- Run `mgconsole` to check if pod is main
- If main, verify all async replicas are caught up
- Wait briefly for synchronization completion
- Allow pod termination to proceed

**StatefulSet Modification**:
```yaml
lifecycle:
  preStop:
    exec:
      command: ["/bin/sh", "/scripts/pre-stop-hook.sh"]
```

### Status

- **Root Cause**: ✅ Identified and validated
- **Solution Design**: ✅ Completed
- **Implementation**: 🔄 In Progress
- **Testing**: ⏳ Pending implementation
- **Deployment**: ⏳ Pending testing

### Related Issues

- **Issue #1**: Previous incorrect analysis about persistent storage
- **Issue #3**: Controller startup failure after refactoring - Fixed
- **Original E2E Test Failures**: Data divergence during rolling restart scenarios

## 3. Controller Startup Failure After Refactoring - Fixed

**Status**: Fixed
**Severity**: High - Controller cannot start
**Investigation Date**: 2025-09-21
**Fixed By**: Claude AI

### Description

After the latest git commit refactoring the memgraph client code, the controller failed to start with the error:

```
time=2025-09-21T10:10:05.542Z level=ERROR msg="Controller reconciliation loop failed" error="failed to discover cluster state: failed to check if this is a new cluster: failed to get replication role for memgraph-ha-0: failed to query replication role for node memgraph-ha-0: no results returned from SHOW REPLICATION ROLE"
```

### Root Cause

During the refactoring in commit `3b1f672`, the field checking logic in `QueryReplicationRole()` was incorrectly ordered:

**Problematic Code**:
```go
role, found := record.Get("replication role")
roleStr, ok := role.(string)
if !found {
    return nil, fmt.Errorf("replication role not found in result")
}
```

**Issue**: The type assertion `role.(string)` was performed before checking if the field was found (`!found`). When the field is not found, `role` is `nil`, causing the type assertion to potentially panic or behave unexpectedly.

### Fix Applied

**Corrected Code**:
```go
role, found := record.Get("replication role")
if !found {
    return nil, fmt.Errorf("replication role not found in result")
}
roleStr, ok := role.(string)
```

**Fix**: Check if the field was found first, then perform the type assertion. This ensures we only attempt to cast the value to string when we know it exists.

### Verification

- ✅ Unit tests pass
- ✅ Static analysis (staticcheck) passes
- ✅ Controller starts successfully and becomes leader
- ✅ No more "no results returned from SHOW REPLICATION ROLE" errors
- ✅ Gateway servers start properly

### Impact

- **Before Fix**: Controller could not start at all
- **After Fix**: Controller starts normally and can query replication roles successfully

### File Modified

- `internal/controller/memgraph_client.go:335-342` - Fixed field checking logic order
