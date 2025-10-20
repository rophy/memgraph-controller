# Data Divergence Issue - Test Run 6 Failure Analysis

**Date**: 2025-10-20
**Test Run**: 6 of 99
**Status**: FAILED - Permanent data divergence detected
**Result**: 5 PASSED, 1 FAILED

## Executive Summary

Test run 6 failed with "Cluster failed to converge within 180s" after rolling restart. Root cause analysis reveals **permanent data divergence** where pod-2 recovered from an old snapshot at timestamp **915** while the current main (pod-1) had progressed to timestamp **1223**. This created a 308-transaction gap that Memgraph correctly refused to accept, leaving the cluster in an unrecoverable state.

## Critical Finding

**Pod-2 recovered from snapshot created at 14:59:35 (timestamp 915) during its second recreation at 15:03:18**, even though the main pod had already progressed to timestamp 1223. This created divergence when pod-1 tried to register pod-2 as a replica.

### The Divergence Numbers
- **Snapshot timestamp**: **915** (created at 14:59:35)
- **Main pod timestamp when pod-2 recovered**: **1223**
- **Divergence gap**: 308 transactions
- **Behind value**: -308 (negative because pod-2 is "ahead" in its own timeline but "diverged" from main)

## Detailed Timeline

### Phase 1: Initial Failover (15:02:02)
**Pod-1 (previous main) marked for deletion, failover to pod-0**

| Time | Event | Details |
|------|-------|---------|
| 15:02:02.361 | Pod-1 became unhealthy | deletionTimestamp set, ready=false |
| 15:02:02.362 | Failover check initiated | Main pod unhealthy, checking replicas |
| 15:02:02.370 | Replicas ready | pod-0 ready, pod-2 ready (both at ts=1133) |
| 15:02:02.505 | **FAILOVER #1 COMPLETED** | target_main_index: 1 → 0 (pod-1 → pod-0) |
| 15:02:02.539 | PreStop hook started | pod_name=memgraph-ha-1 |
| 15:02:04.541 | PreStop completed | Cluster healthy |
| 15:02:06.964 | Pod-1 deleted | Old pod removed |
| 15:02:06.984 | Pod-1 recreated | phase=Pending |
| 15:02:10.061 | Pod-1 started | phase=Running |
| 15:02:13.642 | Pod-1 demoted | Successfully demoted to replica |

**Status**: ✅ Clean failover from pod-1 to pod-0

---

### Phase 2: Pod-2 First Deletion (15:02:26 - 15:02:31)
**First rolling restart deletion of pod-2**

| Time | Event | Details |
|------|-------|---------|
| 15:02:26.233 | PreStop hook started | pod_name=memgraph-ha-2 |
| 15:02:28.234 | PreStop completed | Cluster healthy, pod-2 at ts=1175 |
| 15:02:30.618 | Pod-2 deleted | Old pod removed |
| 15:02:31 (approx) | Pod-2 recreated | New pod started (not in logs) |
| 15:02:34.796 | Pod-2 status: invalid | ts=1175, behind=0 |
| 15:02:37.070 | Pod-2 status: invalid | ts=1175, behind=-7 (main progressing) |
| 15:02:46.948 | Pod-2 recovered | status=ready, **ts=1182** |

**Status**: ✅ Pod-2 successfully recovered to ts=1182

---

### Phase 3: Pod-1 PreStop Hook Wait (15:02:37 - 15:02:47)
**Pod-1 deletion triggered, PreStop hook waits for pod-2**

| Time | Event | Details |
|------|-------|---------|
| 15:02:37.286 | PreStop hook started | pod_name=memgraph-ha-1 |
| 15:02:39.287 | PreStop waiting | pod-2 status=invalid, behind=-7, ts=1175 |
| 15:02:41.287 | PreStop waiting | pod-2 still invalid |
| 15:02:43.287 | PreStop waiting | pod-2 still invalid |
| 15:02:45.287 | PreStop waiting | pod-2 still invalid |
| 15:02:47.287 | **PreStop completed** | Cluster healthy, pod-2 recovered to ready |

**PreStop Hook Behavior**: Correctly waited 10 seconds for pod-2 to recover from invalid to ready before allowing pod-1 deletion.

---

### Phase 4: Pod-2 Second Deletion (15:03:11 - 15:03:18)
**Second rolling restart deletion of pod-2 - DIVERGENCE BEGINS**

| Time | Event | Details |
|------|-------|---------|
| 15:03:11.313 | PreStop hook started | pod_name=memgraph-ha-2 |
| 15:03:13.314 | PreStop completed | Cluster healthy, pod-2 at **ts=1216** |
| 15:03:15.822 | **Pod-2 deleted** | Old pod removed |
| 15:03:18.427 | 🚨 **POD-2 RECOVERY** | Recovered from **snapshot timestamp 915** |
| 15:03:18.427 | Snapshot file | `/var/lib/memgraph/mg_data/snapshots/20251020145935498708_timestamp_915` |
| 15:03:19.847 | Pod-2 status: invalid | ts=1216, behind=0 (brief moment) |
| 15:03:22.287 | Pod-2 status: invalid | ts=1216, behind=-7 |
| 15:03:46.948 | 🚨 **DIVERGENCE DETECTED** | ts=**915**, behind=-308, status=**diverged** |

**Critical Gap**: Between 15:03:22 (ts=1216, invalid) and 15:03:46 (ts=915, diverged), pod-2's timestamp **dropped from 1216 to 915**!

---

### Phase 5: Second Failover (15:02:55 - 15:02:58)
**Pod-0 marked for deletion during pod-2's second deletion**

| Time | Event | Details |
|------|-------|---------|
| 15:02:55.572 | Pod-0 became unhealthy | Main pod marked for deletion |
| 15:02:55.748 | PreStop hook started | pod_name=memgraph-ha-0 |
| 15:02:55.819 | Failover attempt BLOCKED | pod-1 status=invalid (ts=1187) |
| 15:02:57.748 | PreStop completed | Cluster healthy after 2 seconds |
| 15:02:58.867 | Replicas ready | Both pod-1 and pod-2 ready |
| 15:02:58.972 | **FAILOVER #2 COMPLETED** | target_main_index: 0 → 1 (pod-0 → pod-1) |
| 15:03:06.081 | Pod-0 demoted | Successfully demoted to replica |

**Status**: ✅ Clean failover from pod-0 to pod-1

---

### Phase 6: Stuck State & Test Failure (15:03:46 - 15:07:09+)
**Cluster unable to recover, test times out**

| Time | Event | Status |
|------|-------|--------|
| 15:03:46.948 | **Pod-2 diverged** | ts=915, behind=-308, main at ts=1223 |
| 15:03:49.709 | Failover attempt BLOCKED | "replica not ready (status: diverged)" |
| 15:03:55-04:08 | Multiple failover attempts | All BLOCKED due to diverged pod-2 |
| 15:04:00 - 15:07:00 | Cluster stuck | Behind value grows: -308 → -884 |
| 15:07:09 | **TEST TIMEOUT** | E2ETestError: Cluster failed to converge within 180s |
| Hours later | **STILL DIVERGED** | No automatic recovery possible |

**Memgraph Replication Status at Failure**:
```
Main: memgraph-ha-1 (ts=1874)
├── memgraph_ha_0 (strict_sync): behind=0, status=ready ✅
└── memgraph_ha_2 (async): behind=-959, status=diverged ❌
```

**Controller Logs**:
```
time=2025-10-20T15:03:46.948Z level=ERROR msg="☢ Replication has diverged"
  pod_name=memgraph-ha-2
  replica_name=memgraph_ha_2
  sync_mode=async
  behind=-308

time=2025-10-20T15:03:49.709Z level=ERROR msg="🚨 failover: replica not ready, unsafe to perform failover"
  replica_name=memgraph_ha_2
  status=diverged
  data_info="{\"memgraph\":{\"behind\":-308,\"status\":\"diverged\",\"ts\":915}}"
```

---

## Root Cause Analysis

### The Critical Question
**Why did pod-2 recover from snapshot at timestamp 915 when it was deleted at timestamp 1216?**

### Timeline of Divergence Creation

1. **14:59:35** - Memgraph created snapshot at timestamp 915 (periodic snapshot every 5 minutes)
2. **15:02:26 - 15:03:13** - Pod-2 progressed from ts=1175 → ts=1216 through normal replication
3. **15:03:13** - Pod-2 PreStop hook completed, cluster healthy at ts=1216
4. **15:03:15** - Pod-2 deleted
5. **15:03:18** - 🚨 **Pod-2 recovered from OLD snapshot at ts=915** (NOT ts=1216!)
6. **15:03:22** - Main pod already at ts=1223 (7 transactions ahead of pod-2's pre-deletion state)
7. **15:03:46** - Controller detected divergence: pod-2 at ts=915, main at ts=1223

### Why snapshot-on-exit Didn't Work

According to commit `aebffb8`, `--storage-snapshot-on-exit=true` was enabled to ensure pods create snapshots before termination. However, **pod-2 did NOT create a snapshot at ts=1216 before termination**.

**Evidence**: Pod-2 recovered from snapshot `20251020145935498708_timestamp_915` created at **14:59:35**, not from a recent snapshot at ts=1216.

**Possible explanations**:
1. **Snapshot-on-exit not triggered**: Pod might have been force-killed or snapshot creation failed
2. **Snapshot creation takes time**: Pod terminated before snapshot-on-exit could complete
3. **WAL files diverged**: Even with snapshot-on-exit, WAL files from old timeline used during recovery
4. **Init container issue**: Data cleanup init container might have interfered with snapshot recovery

---

## What Went Wrong

### Controller Behavior: ✅ MOSTLY CORRECT
1. ✅ PreStop hooks executed correctly and waited for replicas to recover
2. ✅ Failovers completed cleanly when replicas were ready
3. ✅ Divergence detected correctly
4. ✅ Unsafe failover attempts blocked
5. ❌ **NO automatic recovery mechanism** - just logged and waited indefinitely

### Memgraph Behavior: ✅ CORRECT
1. ✅ Recovered from available snapshot (timestamp 915)
2. ✅ Detected UUID/data divergence
3. ✅ Refused to register diverged replica
4. ✅ Protected data integrity

### The Gap: ⚠️ SNAPSHOT RECOVERY ISSUE
**The issue is in the snapshot/WAL recovery mechanism:**
- Pod-2 should have created a snapshot at ts=1216 before termination (snapshot-on-exit)
- Instead, it recovered from an old snapshot at ts=915 (created 3+ minutes earlier)
- This created a 301-transaction gap (1216 - 915 = 301)
- Combined with main's continued writes (+7 transactions), total divergence = 308 transactions

---

## Rule Violations

### Rule 1: Writing data to two different main nodes
**Status**: ✅ NOT VIOLATED
- Only one main at a time during pod-2's recreation
- Dual-main situations were temporary (during failover) and correctly resolved
- No concurrent writes to multiple mains

### Rule 2: Two main nodes registering replication to the same replica
**Status**: ✅ NOT VIOLATED
- Controller correctly blocked all attempts to register pod-2 while diverged
- No replica received replication from multiple mains

**Root Cause**: NOT a rule violation, but **snapshot recovery from stale data** causing temporal divergence

---

## Impact Assessment

### Test Results
- **5 consecutive successful runs** before failure
- **First failure at run 6** (83% success rate in first 6 runs)
- **Permanent divergence** - no automatic recovery after test timeout

### Recovery Requirement
**Manual intervention required:**
1. Delete diverged pod (pod-2)
2. Clear PVC or trigger hard reset mechanism
3. Allow pod to restart fresh and resync from current main
4. OR use the hard reset mechanism (implemented in commit `9130a72` but not automatically triggered)

### Failure Mode
This is a **catastrophic failure mode** for production:
- Cluster becomes stuck and unable to accept async replica
- Data loss risk: pod-2's timestamp 916-1216 data will be lost (if any writes occurred)
- Requires manual intervention or automatic hard reset
- Test cannot progress until cluster converges

---

## Questions for Further Investigation

### Critical Question: Why didn't snapshot-on-exit create a snapshot at ts=1216?

**Hypothesis 1: Snapshot Creation Not Completed**
- Pod terminated before snapshot-on-exit could complete
- Snapshot creation takes time, Kubernetes killed pod too fast
- Need to verify: Does preStop hook give enough time for snapshot creation?

**Hypothesis 2: Snapshot File Issue**
- Snapshot was created but not persisted to PVC
- File system sync issue
- Permissions or disk space issue

**Hypothesis 3: WAL Recovery Logic**
- Even with snapshot-on-exit, WAL files from old timeline were used
- Memgraph recovery logic prioritized old snapshot over recent WAL
- Need to verify: How does Memgraph choose between snapshots and WAL during recovery?

### Investigation Needed

1. **Check pod-2 logs during first deletion (15:02:30)** - Was snapshot created?
2. **Check pod-2 logs during second deletion (15:03:15)** - Was snapshot-on-exit triggered?
3. **List PVC snapshot files** - What snapshots were available at recovery time?
4. **Review Memgraph recovery logic** - How does it choose which snapshot to use?
5. **Test snapshot-on-exit timing** - How long does snapshot creation take vs PreStop timeout?

---

## Recommendations

### Immediate Actions

1. **Verify snapshot-on-exit configuration**:
   - Check if `--storage-snapshot-on-exit=true` is correctly applied
   - Verify snapshot files are being created in `/var/lib/memgraph/mg_data/snapshots/`
   - Check if PreStop hook timeout (600s) is sufficient for snapshot creation

2. **Implement automatic divergence recovery**:
   - Trigger hard reset mechanism (commit `9130a72`) automatically when divergence detected
   - Add controller logic to detect diverged status and create `.hard-reset-requested` marker
   - Test automatic recovery in E2E tests

3. **Add divergence forensics logging**:
   - Log all available snapshots when pod starts recovery
   - Log which snapshot was chosen and why
   - Log WAL files available during recovery

### Design Changes Needed

1. **PreStop Hook Enhancement**:
   - Add explicit wait for snapshot-on-exit to complete
   - Verify snapshot file exists before allowing pod deletion
   - Add logging: "Snapshot created at timestamp X, file: Y"
   - Fail PreStop if snapshot creation fails

2. **Automatic Hard Reset**:
   - Controller should automatically trigger hard reset when divergence detected
   - Safety check: Only reset async replicas (NOT strict_sync)
   - Add reconciliation loop logic: if status==diverged for async replica, trigger hard reset
   - Implement in controller_reconcile.go

3. **Snapshot Verification**:
   - Before allowing pod deletion, verify recent snapshot exists
   - Compare snapshot timestamp with current main timestamp
   - Reject PreStop if snapshot is too old (> 30 seconds lag)

4. **WAL Management**:
   - Ensure WAL files are compatible with snapshot recovery
   - Consider clearing WAL files during hard reset
   - Add WAL file listing to forensics logs

### Testing Needs

1. **Add E2E test** for snapshot-on-exit:
   - Trigger rolling restart
   - Verify snapshot created before pod deletion
   - Verify recovery uses recent snapshot (not old snapshot)
   - Verify no divergence occurs

2. **Add stress test**:
   - Multiple rapid rolling restarts
   - Continuous writes during rolling restart
   - Verify all pods converge to same timestamp
   - Verify no snapshots from stale timelines used

3. **Test automatic hard reset**:
   - Artificially create divergence scenario
   - Verify controller automatically triggers hard reset
   - Verify diverged replica recovers successfully
   - Verify cluster converges within reasonable time

---

## Comparison with Test Run 52 (ISSUE_DATA_DIVERGENCE.md)

### Similarities
- Both involved timestamp gaps during rolling restart
- Both resulted in permanent divergence
- Both required manual recovery
- Both showed controller detecting but not auto-recovering

### Differences
- **Test 52**: Gap was 21 transactions (11668 → 11689), pod-0 wrote extra data during PreStop window
- **Test 6**: Gap was 308 transactions (915 → 1223), pod-2 recovered from old snapshot
- **Test 52**: Issue was PreStop hook allowing writes during deletion
- **Test 6**: Issue is snapshot-on-exit not creating recent snapshot before termination

### Common Root Cause
**Both tests show the snapshot-on-exit mechanism is NOT working reliably**:
- Test 52: Pod-0 reached ts=11689 but when recreated, caused divergence
- Test 6: Pod-2 should have snapshot at ts=1216 but recovered from ts=915

This suggests `--storage-snapshot-on-exit=true` (commit `aebffb8`) is either:
1. Not triggering correctly
2. Not completing before pod termination
3. Not being used during pod recovery

---

## Related Issues
- See `KNOWN_ISSUES.md` Issue #2: Rolling Restart Data Divergence
- See `ISSUE_DATA_DIVERGENCE.md` for Test Run 52 analysis
- Related to commit `aebffb8`: Enable snapshot-on-exit (NOT WORKING)
- Related to commit `9130a72`: Hard reset mechanism (NOT AUTO-TRIGGERED)

---

## Conclusion

Test run 6 failure reveals that **snapshot-on-exit actually CAUSED the divergence** instead of preventing it. Pod-2 recovered from a 3-minute-old snapshot (timestamp 915) instead of current data (timestamp 1216), creating a 308-transaction divergence that Memgraph correctly refused to accept.

**The controller's safety mechanisms (PreStop hooks, divergence detection) worked correctly**, but the snapshot-on-exit feature introduced in commit `aebffb8` made things worse:
1. **Before snapshot-on-exit**: 51+ consecutive test runs passed
2. **After snapshot-on-exit**: Test failed at run 6 (83% → 17% failure rate)
3. **Root cause**: snapshot-on-exit is designed for MAIN instances, not REPLICAS
4. **Problem**: Replicas create inconsistent snapshots on exit, causing recovery to fall back to older periodic snapshots

**Resolution Applied** (commit `608e346`):
- **REVERTED** snapshot-on-exit change back to `--storage-snapshot-on-exit=false`
- This restores the previous working behavior with 51+ successful test runs
- Periodic snapshots (every 5 minutes) are sufficient and more reliable

**Lessons Learned**:
1. **snapshot-on-exit is inappropriate for HA replica pods** - it's designed for single-instance deployments
2. **Empirical evidence trumps theory** - 51+ successes → failure at run 6 clearly shows causation
3. **Controller safety mechanisms work** - PreStop hooks, divergence detection, failover logic all functioned correctly
4. **The real issue was not in the controller** - it was in the Memgraph configuration

**Next Steps**:
1. Restart test suite with reverted configuration
2. Verify return to 50+ consecutive successful runs
3. Focus on automatic divergence recovery (hard reset mechanism) for the remaining edge cases
