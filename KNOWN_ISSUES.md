# Known Issues

## 1. Race Condition Between Reconciliation and Failover

**Status**: Active  
**Severity**: Critical  
**First Observed**: 2025-09-16  
**Occurrence Rate**: Intermittent but can cause complete replication topology corruption

### Description

The controller has a critical race condition where reconciliation and failover operations can execute concurrently, causing reconciliation to undo failovers and corrupt the replication topology. This results in clusters with no SYNC replicas (only ASYNC), making safe failover impossible.

### Root Cause

**Different mutexes for concurrent operations**:
- `performReconciliationActions()` uses `reconcileMu`
- `executeFailover()` uses `failoverMu`
- These different mutexes allow concurrent execution

**The race condition sequence**:
1. Reconciliation starts, reads `targetMainIndex=0` from ConfigMap
2. Reconciliation calls `performFailoverCheck()` (no mutex held during this call)
3. Meanwhile, health prober triggers failover in separate goroutine
4. Failover promotes pod-1 to main, updates ConfigMap to `targetMainIndex=1`
5. Reconciliation continues with stale `targetMainNode=pod-0`
6. Reconciliation sees pod-1 has role="main" but thinks it should be replica
7. **Reconciliation demotes the newly promoted main back to replica**
8. When registering replicas, sync/async calculation is wrong due to state mismatch

### Evidence

**From controller logs during rolling restart**:
```
11:45:23.776 performReconciliationActions started
11:45:23.779 promoting sync replica to main pod_name=memgraph-ha-1
11:45:23.900 failover: updated target main index target_main_index=1
11:45:24.337 Pod has wrong role, demoting to replica pod_name=memgraph-ha-1 current_role=main
11:45:24.340 Registered replication replica_name=memgraph_ha_1 sync_mode=ASYNC
```

The reconciliation that started at 11:45:23.776 with `targetMainIndex=0` continued executing after failover updated ConfigMap to `targetMainIndex=1`, causing it to demote the newly promoted main.

### Impact

- **Complete replication topology corruption**: All replicas become ASYNC, no SYNC replica
- **Failover failures**: Controller cannot perform safe failover without SYNC replica
- **Data consistency risk**: Without SYNC replica, data loss possible during failures
- **Rolling restart failures**: E2E tests fail due to incorrect topology after restart

### Multiple Entry Points Compound the Problem

Three different code paths can trigger these operations:
1. **Reconciliation loop** → `performReconciliationActions` → `performFailoverCheck`
2. **FailoverCheckQueue** → `performFailoverCheck` → `executeFailover`
3. **Health Prober** → `executeFailover` (directly, bypassing performFailoverCheck)

The health prober's direct call to `executeFailover` is particularly problematic as it can cause double failovers if not properly synchronized.

### Proposed Solution

**Use a single shared mutex** for all reconciliation and failover operations:

1. Replace both `reconcileMu` and `failoverMu` with a single `sharedMutex`
2. Acquire mutex at entry points, not inside functions:
   - In `performReconciliationActions()` - hold for entire reconciliation
   - In `FailoverCheckQueue.processEvent()` - before calling performFailoverCheck
   - In health prober - before any failover operations
3. Have health prober submit events to `FailoverCheckQueue` instead of direct execution
4. Always re-validate conditions after acquiring mutex (prevent stale decisions)

### Related Code

- `internal/controller/controller_reconcile.go:110-111` - Uses reconcileMu
- `internal/controller/queue_failover.go:137-138` - Uses failoverMu  
- `internal/controller/prober.go:211` - Direct executeFailover call
- `internal/controller/queue_failover.go:86` - No mutex before performFailoverCheck

### Workaround

None available. The race condition can occur at any time when reconciliation and failover triggers overlap.

---

## 2. ASYNC/SYNC Replica Registration During Rolling Restart

**Status**: Active (caused by race condition above)
**Severity**: High  
**First Observed**: 2025-09-11  
**Occurrence Rate**: 100% during rolling restart

### Description

During StatefulSet rolling restart, replicas are incorrectly registered with wrong SYNC/ASYNC modes. Specifically, pod-1 gets registered as ASYNC replica when it should be SYNC, resulting in both replicas being ASYNC and no SYNC replica present.

### Root Cause

This is a **symptom of the race condition described in Issue #1**. During rolling restart:

1. **ConfigMap is source of truth**: Says pod-0 should be main (targetMainIndex=0)
2. **Rolling restart occurs**: Pod-0 restarts, pod-1 temporarily becomes main via failover
3. **Race condition occurs**: Reconciliation with stale state undoes the failover
4. **Wrong sync calculation**: Sync replica calculated from ConfigMap doesn't match actual main
5. **SYNC/ASYNC logic error**: All replicas registered as ASYNC

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

This appears to be an issue with Memgraph itself taking time to gracefully shut down, rather than a controller issue. Research indicates several specific factors contribute to slow shutdown:

1. **Snapshot Creation on Exit** - Memgraph has `--storage-snapshot-on-exit=true` by default, creating a complete data snapshot during shutdown which can take significant time for large datasets
2. **Active Query Handling** - `--query-execution-timeout-sec=600` (10 minutes default) means long-running queries must complete or timeout before clean shutdown
3. **Storage Operations** - Garbage collection cycles and storage access timeouts, plus data persistence mechanisms that ensure durability during shutdown
4. **Insufficient Grace Period** - Kubernetes default `terminationGracePeriodSeconds` is 30 seconds, typically insufficient for database workloads
5. **Connection Cleanup** - Active client connections may need to be gracefully closed

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

### Confirmed Solutions

Based on internet research and Memgraph documentation, the following solutions can address slow termination:

**Solution 1: Increase Termination Grace Period**
```yaml
spec:
  template:
    spec:
      terminationGracePeriodSeconds: 120  # Increase from default 30s
```

**Solution 2: Optimize Memgraph Configuration**
```yaml
# Consider these configuration changes (trade-offs with data safety)
args:
  - --storage-snapshot-on-exit=false    # Skip snapshot on shutdown (faster, less safe)
  - --query-execution-timeout-sec=30    # Reduce query timeout (faster termination)
```

**Solution 3: Add PreStop Hook for Connection Draining**
```yaml
lifecycle:
  preStop:
    exec:
      command: ["/bin/sh", "-c", "sleep 15"]  # Allow graceful connection draining
```

### Workaround

**Immediate fixes**:
- **For E2E tests**: Increase convergence timeout from 120s to 180-240s
- **For StatefulSet**: Set `terminationGracePeriodSeconds: 120` or higher
- **For operations**: Allow 2-3 minutes for rolling restarts to complete

**Monitoring**:
- Use `kubectl get pods -w` to track termination progress
- Check pod events with `kubectl describe pod <name>` during termination
- Monitor Memgraph logs with `kubectl logs --previous <name>` after termination

### Investigation Results

**Research findings**:
1. **Common database issue**: Stateful applications typically need longer grace periods than the 30s default
2. **Memgraph-specific factors**: Data persistence features (snapshots, durability) contribute to longer shutdown times  
3. **No specific documentation**: Memgraph doesn't explicitly address Kubernetes termination optimization
4. **Kubernetes best practices**: Database workloads should configure appropriate `terminationGracePeriodSeconds`

### Related Components

- **StatefulSet configuration**: `charts/memgraph-ha/templates/statefulset.yaml`
- **Memgraph configuration**: Pod startup arguments and config
- **E2E tests**: `tests/e2e/test_rolling_restart.py` timeout settings

### Notes

- This is a **Memgraph application-level issue**, not a controller issue
- The controller works correctly once pods complete termination
- Issue affects test reliability more than production functionality  
- Rolling restart **does work** - it just takes longer than expected
- **Confirmed by research**: This is a common issue with stateful database workloads in Kubernetes
- **Solution available**: Increase `terminationGracePeriodSeconds` to 120+ seconds as primary fix

---

## Historical Issues

### Gateway Routing Race Condition (Resolved)

**Status**: Resolved in recent commits  
**Resolution**: Gateway routing and connection handling improvements
**Note**: Replaced by the reconciliation timing issue documented above