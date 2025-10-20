# Data Divergence Issue - Test Run 52 Failure Analysis

**Date**: 2025-10-20
**Test Run**: 52 of 99
**Status**: FAILED - Permanent data divergence detected
**Result**: 51 PASSED, 1 FAILED

## Executive Summary

Test run 52 failed with "Cluster failed to converge within 180s" after a rolling restart. Root cause analysis reveals **permanent data divergence** where pod-0 recovered with 21 more transactions (timestamp 11689) than the current main pod-1 (timestamp 11668). Memgraph's UUID-based divergence protection correctly refused to register the diverged replica, leaving the cluster in an unrecoverable state that persisted for 10+ hours.

## Critical Finding

**pod-0 wrote transactions 11669-11689 AFTER being marked for deletion**, creating divergence when pod-1 (which only had data up to 11668) was promoted to main.

### The Divergence Numbers
- **Before rolling restart**: Both replicas healthy at timestamp **11650** (behind=0, status=ready)
- **pod-1 recovered to**: Timestamp **11668**
- **pod-0 recovered to**: Timestamp **11689** (21 transactions ahead)
- **Divergence**: pod-0 has unique data (txn 11669-11689) that pod-1 will never have

## Detailed Timeline

### Phase 1: Initial State (before 02:36:14)
**Cluster healthy, ready for rolling restart**

```
Main: pod-0 (target_main_index=0)
├── pod-1 (strict_sync): Behind=0, Status=ready, Timestamp=11650
└── pod-2 (async):       Behind=0, Status=ready, Timestamp=11650
```

**Status**: ✅ All replicas fully synchronized

---

### Phase 2: pod-2 Deletion (02:36:14 - 02:36:25)
**First pod in rolling restart sequence**

| Time | Event | Details |
|------|-------|---------|
| 02:36:14.618 | pod-2 marked for deletion | delete_ts=2025-10-20T03:06:14Z |
| 02:36:14.823 | PreStop hook started | pod_name=memgraph-ha-2 |
| 02:36:18.210 | pod-2 terminated | phase=Succeeded |
| 02:36:19.004 | pod-2 deleted | Old pod removed |
| 02:36:19.021 | pod-2 recreated | phase=Pending |
| 02:36:22.114 | pod-2 started | phase=Running |
| 02:36:25.495 | pod-2 ready | Ready=true |

**Status**: ✅ pod-0 remained MAIN throughout, no failover

---

### Phase 3: pod-1 Deletion (02:36:25 - 02:36:52)
**Second pod in rolling restart sequence**

| Time | Event | Details |
|------|-------|---------|
| 02:36:25.522 | pod-1 marked for deletion | delete_ts=2025-10-20T03:06:25Z |
| 02:36:25.716 | PreStop hook started | pod_name=memgraph-ha-1 |
| 02:36:45.284 | pod-1 terminated | phase=Succeeded |
| 02:36:45.763 | pod-1 deleted | Old pod removed |
| 02:36:45.879 | pod-1 recreated | phase=Pending |
| 02:36:48.842 | pod-1 started | phase=Running, recovering from WAL |
| 02:36:48.410 | pod-1 WAL recovery complete | **Timestamp=11668** (from_11619_to_11668) |
| 02:36:52.192 | pod-1 ready | Ready=true |

**Status**: ✅ pod-0 remained MAIN throughout, no failover

---

### Phase 4: pod-0 Deletion & Failover (02:36:52 - 02:37:21)
**Third pod deletion triggers failover - DIVERGENCE BEGINS**

| Time | Event | Details |
|------|-------|---------|
| 02:36:52.208 | pod-0 marked for deletion | delete_ts=2025-10-20T03:06:52Z |
| 02:36:53.099 | PreStop hook started | pod_name=memgraph-ha-0 |
| 02:37:14.226 | pod-0 terminated | phase=Succeeded |
| 02:37:14.228 | **FAILOVER INITIATED** | Promoting pod-1 to MAIN |
| 02:37:14.308 | pod-1 promoted to main | Success |
| 02:37:14.316 | **target_main_index changed** | 0 → 1 (pod-0 → pod-1) |
| 02:37:14.615 | pod-0 deleted | Old pod removed |
| 02:37:14.633 | pod-0 recreated | phase=Pending |
| 02:37:17.320 | pod-0 WAL recovery complete | **Timestamp=11689** (from_11619_to_11689) |
| 02:37:17.675 | pod-0 started | phase=Running, **role=main** (wrong!) |
| 02:37:19.318 | **DUAL-MAIN DETECTED** | pod-0=main, pod-1=main |
| 02:37:19.318-326 | Controller blocks registration | 6 attempts to register pod-2 blocked |
| 02:37:21.031 | pod-0 ready | Ready=true |
| 02:37:21.278 | Controller demotes pod-0 | main → replica |
| 02:37:21.280 | Attempt to register pod-0 | To pod-1 as strict_sync replica |
| 02:37:21.283 | **❌ MEMGRAPH REFUSED** | "Replica acted as Main, has diverged data" |

**Status**: 🚨 PERMANENT DIVERGENCE - pod-0 has 21 extra transactions

**Controller State at Registration Attempt**:
- ✅ Controller safety check PASSED: Single main confirmed, all pods reachable
- ✅ Dual-main detection WORKED: Correctly blocked during dual-main window
- ❌ Memgraph UUID check FAILED: Detected diverged data (11689 vs 11668)

---

### Phase 5: Stuck State & Test Failure (02:37:21 - 02:40:24+)
**Cluster unable to recover, test times out**

| Time | Event | Status |
|------|-------|--------|
| 02:37:21.283 | pod-0 registration failed | data_info: {} (malformed) |
| 02:37:43.068 | Attempt to register pod-2 | To pod-1 as async replica |
| 02:37:43.071 | **❌ MEMGRAPH REFUSED** | "Replica acted as Main, has diverged data" |
| 02:37:43 - 02:40:24 | Cluster stuck (180s) | No convergence, replicas show empty data_info |
| 02:40:24 | **TEST TIMEOUT** | E2ETestError: Cluster failed to converge |
| 10+ hours later | **STILL DIVERGED** | No automatic recovery possible |

**Memgraph Error Messages**:
```
[memgraph_log] [error] You cannot register Replica memgraph_ha_0 to this Main
because at one point Replica memgraph_ha_0 acted as the Main instance.
Both the Main and Replica memgraph_ha_0 now hold unique data.
Please resolve data conflicts and start the replication on a clean instance.

[memgraph_log] [error] You cannot register Replica memgraph_ha_2 to this Main
because at one point Replica memgraph_ha_2 acted as the Main instance.
Both the Main and Replica memgraph_ha_2 now hold unique data.
Please resolve data conflicts and start the replication on a clean instance.
```

**Controller Logs**:
```
Status:malformed
ErrorReason:Missing 'memgraph' key in data_info
Behind:-1
```

---

## Root Cause Analysis

### The Critical Question
**How did pod-0 write transactions 11669-11689 when it was marked for deletion at 02:36:52?**

### Timeline of Divergence Creation
1. **02:36:14** - Rolling restart begins, all replicas at timestamp 11650
2. **02:36:52** - pod-0 marked for deletion, PreStop hook starts
3. **Between 02:36:52 - 02:37:14** - pod-0 continued processing writes while PreStop hook was running
4. **02:37:14** - pod-0 terminated with timestamp 11689 (39 transactions beyond initial 11650)
5. **02:36:48** - pod-1 had already restarted and recovered to timestamp 11668 (18 transactions beyond initial 11650)
6. **Divergence**: pod-0 has transactions 11669-11689 that pod-1 never replicated

### Why PreStop Hook Didn't Prevent This

The PreStop hook is designed to:
1. Stop gateway traffic to the pod
2. Wait for replicas to catch up
3. Ensure cluster is healthy before allowing pod termination

However, the **gap between 11668 and 11689 suggests**:
- pod-0 may have continued writing data during PreStop hook execution
- OR pod-1 (as replica) failed to replicate the final transactions before pod-0 was deleted
- OR writes occurred between gateway stop and actual termination

### Memgraph's UUID-Based Protection

Memgraph tracks database UUIDs to detect when a pod has acted as main:
- Each time a pod is promoted to main, it gets a unique database UUID
- When registering a replica, Memgraph checks if that replica's UUID indicates it was previously main
- If UUIDs don't match (indicating the replica was main with different data), registration is refused

**This protection correctly identified the divergence and prevented further data corruption.**

---

## What Went Wrong

### Controller Behavior: ✅ CORRECT
1. ✅ Dual-main detection worked: Blocked registrations during dual-main window (02:37:19)
2. ✅ Demotion worked: Correctly demoted pod-0 from main to replica (02:37:21)
3. ✅ Safety checks passed: Confirmed single main before attempting registration
4. ✅ PreStop hooks executed: Started and attempted to wait for cluster health

### Memgraph Behavior: ✅ CORRECT
1. ✅ UUID divergence detection worked: Refused to register diverged replicas
2. ✅ Prevented data corruption: Did not accept incompatible replicas
3. ✅ Clear error messages: Explicitly stated the divergence reason

### The Gap: ⚠️ TRANSACTION WINDOW
**The issue is in the transaction window between deletion and termination:**
- pod-0 was marked for deletion at 02:36:52
- pod-0 PreStop hook started at 02:36:53
- pod-0 terminated at 02:37:14 (22 seconds later)
- During this time, pod-0 had timestamp 11689 vs pod-1's 11668

**Possible causes**:
1. **Gateway didn't stop traffic fast enough** - Writes continued to reach pod-0 after PreStop hook started
2. **Replica lag at deletion time** - pod-1 was at 11668, pod-0 was at 11689 when deletion started
3. **PreStop hook timing** - Hook started but pod-0 already had diverged data from earlier writes

---

## Replication Status Evidence

### At Rolling Restart Start (02:36:13-14)
**Controller View of pod-0's replicas:**
```
Timestamp: 11650
├── pod-1 (strict_sync): Behind=0, Status=ready, Timestamp=11650, IsHealthy=true
└── pod-2 (async):       Behind=0, Status=ready, Timestamp=11650, IsHealthy=true
```

### After pod-0 Deletion & Failover (02:37:19-21)
**Controller View of pod-1's replicas:**
```
Timestamp: 11668 (pod-1 as new main)
├── replica_count=0 (no replicas registered yet)
```

**Attempting to register pod-0:**
```
pod-0: Timestamp=11689, role=main (wrong role, demoted to replica)
Result: ❌ REFUSED - "diverged data"
```

### At Test Timeout (02:37:43 - 02:40:24)
**Controller View:**
```
Main: pod-1
├── pod-0: Behind=-1, Status=malformed, Timestamp=0, ErrorReason="Missing 'memgraph' key"
└── pod-2: Behind=-1, Status=malformed, Timestamp=0, ErrorReason="Missing 'memgraph' key"
```

**Actual Memgraph State:**
- pod-1: main, no registered replicas, data_info: {}
- pod-0: replica (role), but has unique data (txn 11669-11689)
- pod-2: replica (role), but Memgraph believes it was previously main

---

## Rule Violations

### Rule 1: Writing data to two different main nodes
**Status**: ❌ VIOLATED during dual-main window (02:37:17 - 02:37:21)
- pod-0 recovered as main with timestamp 11689
- pod-1 was already main with timestamp 11668
- ~4 seconds of dual-main state where both could potentially accept writes
- Controller correctly detected and demoted pod-0, but damage was already done

### Rule 2: Two main nodes registering replication to the same replica
**Status**: ⚠️ ATTEMPTED BUT BLOCKED
- Controller correctly blocked registration during dual-main detection
- Once dual-main resolved, registration attempts failed due to divergence
- This rule was not the cause, but the UUID mismatch triggered by Rule 1 violation

---

## Impact Assessment

### Test Results
- **51 consecutive successful runs** before failure
- **First failure at run 52** indicates this is a race condition or timing-dependent issue
- **Permanent divergence** - no automatic recovery after 10+ hours

### Recovery Requirement
**Manual intervention required:**
1. Delete diverged pods (pod-0 and pod-2)
2. Clear their PVCs to wipe WAL data
3. Allow pods to restart fresh and resync from current main
4. No automatic recovery mechanism exists

### Failure Mode
This is a **catastrophic failure mode** for production:
- Cluster becomes stuck and unable to accept new replicas
- Data loss risk: pod-0 has 21 unique transactions that will be lost
- Requires manual data reconciliation or acceptance of data loss

---

## Questions for Further Investigation

### Critical Question: When did pod-0 write transactions 11669-11689?

**Hypothesis 1: During PreStop Hook**
- pod-0's PreStop hook started at 02:36:53
- Gateway should have stopped traffic immediately
- But pod-0 terminated 22 seconds later at 02:37:14
- Did writes continue during this window?

**Hypothesis 2: Before PreStop Hook**
- pod-0 may have already been at 11689 when deletion was triggered
- pod-1 (as replica) may have only caught up to 11668
- This would indicate **replication lag at deletion time**

**Hypothesis 3: Replica Lag During pod-1 Restart**
- pod-1 was deleted at 02:36:25, restarted at 02:36:48
- During pod-1's restart (~23 seconds), pod-0 (still main) may have written transactions
- pod-1 recovered from WAL to only 11668, missing the latest transactions

### Investigation Needed

1. **Check gateway logs** for write activity during 02:36:53 - 02:37:14
2. **Check test-client logs** for write timestamps during pod-0's PreStop window
3. **Analyze PreStop hook logic** - does it actually wait for replicas to catch up?
4. **Review strict_sync semantics** - should pod-1 have been at 11689 if it was strict_sync?

---

## Recommendations

### Immediate Actions
1. **Add PreStop Hook logging** to track exactly when gateway stops and when replicas catch up
2. **Add replication lag checks** before marking a pod for deletion
3. **Add timestamp logging** to track when divergence first occurs

### Design Changes Needed
1. **PreStop Hook Enhancement**:
   - Verify ALL replicas (not just one sync replica) are caught up before allowing deletion
   - Add explicit check: if pod is main, ensure ALL replicas have same timestamp
   - Add timeout with alerting if replicas cannot catch up

2. **Failover Logic Enhancement**:
   - Before promoting a replica to main, verify it has the LATEST data
   - Add timestamp comparison: new main must have >= timestamp of old main
   - Reject promotion if replica is behind

3. **Divergence Detection**:
   - Add controller-side timestamp tracking to detect divergence before Memgraph refuses
   - Fail fast with clear error if divergence is detected
   - Add automatic recovery: wipe diverged replica and trigger resync

4. **Hard Reset Enhancement**:
   - Implement automatic divergence recovery mechanism
   - When Memgraph refuses registration, automatically trigger hard reset of diverged replica
   - Add safety checks to prevent data loss

### Testing Needs
1. **Add E2E test** for this specific scenario:
   - Trigger rolling restart
   - Verify no timestamp gaps exist
   - Verify all replicas converge to same timestamp

2. **Add stress test**:
   - Continuous writes during rolling restart
   - Multiple rolling restarts in sequence
   - Verify no divergence accumulates

---

## Related Issues
- See `KNOWN_ISSUES.md` for other known data divergence scenarios
- Related to zero-downtime failover requirements
- Related to fast failover timing constraints

---

## Conclusion

The test failure reveals a **critical gap in the rolling restart process**: pod-0 had 21 transactions (11669-11689) that pod-1 never replicated before pod-1 was promoted to main. This created permanent divergence that Memgraph correctly refused to accept, leaving the cluster in an unrecoverable state.

**The controller's safety mechanisms (dual-main detection, PreStop hooks) worked correctly**, but they were insufficient to prevent the divergence because the gap occurred during the transition period when pod-0 was being deleted.

**The root cause appears to be replication lag or continued write activity during the PreStop hook window**, allowing pod-0 to reach timestamp 11689 while pod-1 only reached 11668.

**Resolution requires understanding exactly when those 21 transactions were written** and implementing stricter synchronization in the PreStop hook to ensure perfect replication before allowing pod deletion.
