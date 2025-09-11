# Known Issues

## 1. ASYNC/SYNC Replica Registration During Rolling Restart

**Status**: Active  
**Severity**: High  
**First Observed**: 2025-09-11  
**Occurrence Rate**: 100% during rolling restart

### Description

During StatefulSet rolling restart, replicas are incorrectly registered with wrong SYNC/ASYNC modes. Specifically, pod-1 gets registered as ASYNC replica when it should be SYNC, resulting in both replicas being ASYNC and no SYNC replica present.

### Root Cause

The controller has a fundamental confusion between "target main" (from ConfigMap - the desired state) and "actual main" (current reality) during replica registration:

1. **ConfigMap is source of truth**: Says pod-0 should be main (targetMainIndex=0)
2. **Rolling restart occurs**: Pod-0 restarts, pod-1 temporarily becomes main via failover
3. **Reconciliation confusion**: Controller tries to reconcile to ConfigMap's desired state
4. **Wrong registration target**: Controller attempts to register replicas to `targetMainNode` (pod-0) even when pod-0 is not actually main
5. **SYNC/ASYNC logic error**: The sync replica determination uses inconsistent logic between target and actual main

### Reproduction Steps

1. Ensure cluster is stable with pod-0 as main, pod-1 as SYNC replica
2. Trigger rolling restart:
   ```bash
   kubectl rollout restart statefulset/memgraph-ha -n memgraph
   ```
3. Monitor during pod-0 restart:
   ```bash
   kubectl exec memgraph-ha-1 -n memgraph -- bash -c 'echo "SHOW REPLICAS;" | mgconsole --output-format csv'
   ```
4. Observe both replicas showing as ASYNC mode

### Evidence

**From rolling restart test**:
```
Pod-0 restarting, pod-1 is now main
Controller logs: "Registered replication" replica_name="replica_1" sync_mode="ASYNC"
Controller logs: "Registered replication" replica_name="replica_2" sync_mode="ASYNC"
Result: No SYNC replica exists
```

**Code analysis** (controller_reconcile.go):
- Line 151: Gets `targetMainNode` from ConfigMap
- Line 259: Registers replicas to `targetMainNode` even if it's not actually main
- Lines 239-248: SYNC replica logic confused between target vs actual

### Impact

- **Data consistency risk**: No SYNC replica means potential data loss on failover
- **Availability risk**: Failover might fail without healthy SYNC replica
- **Rolling restart reliability**: Every rolling restart causes temporary loss of SYNC replication

### Investigation Attempts

**Attempt 1: Use actual main for registration**
- Modified controller to find actual main via `GetReplicationRole()` 
- Register replicas to actual main instead of target main
- Issue: Violates design principle that ConfigMap is source of truth

**Attempt 2: Fix SYNC replica determination**
- Changed logic to determine SYNC based on actual main index
- Issue: Still registering to wrong node (target instead of actual)

**Core Issue**: The reconciliation logic needs to properly handle the transition period where:
- ConfigMap says pod-0 should be main (desired state)
- Pod-1 is temporarily main (current state)
- Pod-0 is coming back as replica (transitional state)

### Proposed Investigation

1. **Clarify design intent**:
   - Should replicas always register to actual main or target main?
   - How should SYNC/ASYNC be determined during transitions?
   - What's the expected behavior during rolling restart?

2. **Review failover logic**:
   - Does failover update ConfigMap's targetMainIndex?
   - Should it update during temporary failures like rolling restart?
   - How to distinguish permanent vs temporary main changes?

3. **Examine reconciliation flow**:
   - Map exact sequence during rolling restart
   - Identify where target vs actual diverge
   - Determine correct reconciliation strategy

### Related Code

- `internal/controller/controller_reconcile.go:151-281` - Main reconciliation logic
- `internal/controller/controller_core.go:287-299` - getTargetMainNode function
- `internal/controller/queue_failover.go:105-122` - Failover check logic
- `internal/common/config.go` - GetPodIndex helper (added during investigation)

### Workaround

Currently none. Rolling restart will temporarily lose SYNC replication until manual intervention or controller restart.

### Notes

- ConfigMap as "source of truth" conflicts with dynamic pod state during rolling restart
- The controller needs clear logic for handling target vs actual main divergence
- This may require design clarification on expected behavior during transitions
- Issue discovered while implementing E2E test for rolling restart continuous availability

---

## 2. Memgraph Pod Termination Delays During Rolling Restart

**Status**: Active  
**Severity**: Medium  
**First Observed**: 2025-09-11  
**Occurrence Rate**: Intermittent (~30% of rolling restarts)

### Description

During StatefulSet rolling restart, Memgraph pods sometimes get stuck in "Terminating" status for extended periods (60-120+ seconds), causing E2E tests to timeout waiting for cluster convergence. This prevents the rolling restart from completing within expected timeframes.

### Root Cause

This appears to be an issue with Memgraph itself taking time to gracefully shut down, rather than a controller issue. The pod termination grace period may be insufficient for Memgraph to complete its shutdown procedures, including:

1. **Data persistence operations** - Memgraph may need time to flush data to disk
2. **Replication cleanup** - Ongoing replication operations may need to complete
3. **Connection cleanup** - Active client connections may need to be gracefully closed

### Reproduction Steps

1. Deploy a healthy Memgraph cluster
2. Trigger rolling restart:
   ```bash
   kubectl rollout restart statefulset/memgraph-ha -n memgraph
   ```
3. Monitor pod status during restart:
   ```bash
   kubectl get pods -n memgraph -w
   ```
4. Observe that some pods remain in "Terminating" status for 60+ seconds

### Evidence

**From E2E test failures**:
```
⏳ Waiting for proper topology... main=memgraph-ha-1, sync=0, async=1 (120s/120s)
E2ETestError: Cluster failed to converge within 120s
```

**From pod status during rolling restart**:
- Pod stuck in "Terminating" status for extended periods
- StatefulSet rollout waits for pod termination before proceeding
- Cluster appears unhealthy during this period due to missing replicas

### Impact

- **E2E Test Reliability**: Tests timeout waiting for cluster convergence  
- **Rolling Restart Duration**: Restarts take much longer than expected (2-3 minutes instead of 30-60 seconds)
- **Service Availability**: Prolonged periods with reduced replica count during restart

### Investigation Attempts

**Attempt 1: Increase termination grace period**
- Could configure longer `terminationGracePeriodSeconds` in StatefulSet
- Trade-off: Longer restart times vs more reliable termination

**Attempt 2: Optimize Memgraph shutdown**
- May need Memgraph configuration tuning for faster shutdown
- Could investigate Memgraph logs during termination for bottlenecks

### Workaround

- **For E2E tests**: Increase convergence timeout from 120s to 180-240s
- **For operations**: Allow extra time for rolling restarts to complete
- **Monitor**: Use `kubectl get pods -w` to track termination progress

### Proposed Investigation

1. **Analyze Memgraph shutdown behavior**:
   - Review Memgraph logs during pod termination
   - Identify what operations are causing delays
   
2. **Review termination configuration**:
   - Check current `terminationGracePeriodSeconds` setting
   - Consider if it needs adjustment
   
3. **Test Memgraph configuration options**:
   - Research Memgraph settings that affect shutdown time
   - Test different configurations for faster shutdown

### Related Components

- **StatefulSet configuration**: `charts/memgraph-ha/templates/statefulset.yaml`
- **Memgraph configuration**: Pod startup arguments and config
- **E2E tests**: `tests/e2e/test_rolling_restart.py` timeout settings

### Notes

- This is a **Memgraph application-level issue**, not a controller issue
- The controller works correctly once pods complete termination
- Issue affects test reliability more than production functionality
- Rolling restart **does work** - it just takes longer than expected

---

## Historical Issues

### Gateway Routing Race Condition (Resolved)

**Status**: Resolved in recent commits  
**Resolution**: Gateway routing and connection handling improvements
**Note**: Replaced by the reconciliation timing issue documented above