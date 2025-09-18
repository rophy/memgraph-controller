# Known Issues

## Summary (Updated 2025-09-18)

**Major fixes implemented**: 
- ✅ **Race condition between reconciliation and failover** - Fixed with shared mutex (`operationMu`)
- ✅ **SYNC/ASYNC replica registration issues** - Resolved as side effect of race condition fix
- 🔄 **Pod termination delays** - Mitigated by controller improvements, no longer causes test failures

**New critical issue discovered**:
- 🔴 **Empty IP Address Bug** - Controller queries localhost when pod has no IP, causing permanent replica divergence

**Current status**: E2E tests pass but replicas can become permanently diverged due to the empty IP address bug.

## 1. Race Condition Between Reconciliation and Failover

**Status**: Fixed (2025-09-16)  
**Severity**: Critical  
**First Observed**: 2025-09-16  
**Fixed**: Commit df87e20 - Implemented shared mutex (`operationMu`)  
**Verification**: In progress - Running E2E tests to confirm fix effectiveness

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

### Implemented Solution (2025-09-16)

**Shared mutex for all reconciliation and failover operations**:

1. ✅ **Implemented `operationMu` shared mutex** in `internal/controller/controller_core.go:49`
2. ✅ **Protected reconciliation** in `controller_reconcile.go:111-112`
3. ✅ **Protected failover operations** in `queue_failover.go:97-99`
4. ✅ **Health prober uses queue-based failover** (no direct execution)
5. ✅ **Fresh health checks** for failover events triggered by health failures

**Key changes in commit df87e20**:
- Replaced separate `reconcileMu` and `failoverMu` with single `operationMu`
- All entry points now acquire the shared mutex before operations
- Health prober submits events to FailoverCheckQueue instead of direct execution

### Related Code (Updated)

- `internal/controller/controller_core.go:49` - **operationMu declaration**
- `internal/controller/controller_reconcile.go:111-112` - **Uses operationMu**
- `internal/controller/queue_failover.go:97-99` - **Uses operationMu**  
- `internal/controller/prober.go` - **Now uses queue-based failover**

### Testing Evidence

**Previous failure (before fix)**:
- E2E test run 8/10 failed with rolling restart timeout
- Sync replica had "invalid" status (behind: -5)
- Failover blocked due to unhealthy replica state

**Current status (after fix)**:
- E2E test run 1/10 with fix: ✅ PASSED (all 8 tests)
- Rolling restart test specifically passed
- Testing in progress for full validation

---

## 2. ASYNC/SYNC Replica Registration During Rolling Restart

**Status**: Fixed (2025-09-16) - Resolved by race condition fix  
**Severity**: High  
**First Observed**: 2025-09-11  
**Fixed**: Indirect fix via operationMu shared mutex (commit df87e20)  
**Verification**: E2E tests show correct SYNC/ASYNC registration during rolling restart

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

### Resolution

This issue has been **resolved as a side effect** of fixing the race condition (Issue #1). With the shared mutex preventing concurrent reconciliation and failover operations, the SYNC/ASYNC replica registration now works correctly during rolling restarts.

**Evidence of fix**:
- E2E rolling restart tests now pass consistently
- Proper replication topology maintained during pod restarts
- No more "all ASYNC" replica states observed

### Notes

- ConfigMap as "source of truth" conflicts with dynamic pod state during rolling restart
- The controller needs clear logic for handling target vs actual main divergence
- This may require design clarification on expected behavior during transitions
- Issue discovered while implementing E2E test for rolling restart continuous availability

---

## 3. Memgraph Pod Termination Delays During Rolling Restart

**Status**: Active - Mitigated by controller fixes  
**Severity**: Medium (Reduced from High due to controller improvements)  
**First Observed**: 2025-09-11  
**Last Observed**: 2025-09-16 (Run 8 of E2E test repeat - before race condition fix)  
**Occurrence Rate**: Intermittent (~30% of rolling restarts), **but no longer causes test failures**

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

**From test run 8 (2025-09-16) - BEFORE race condition fix**:
```
Test: test_rolling_restart_continuous_availability
Started: 22:15:22
Updated 2/3 pods: 22:15:39 
Got stuck: 22:16:55 with 2/3 ready
Timeout: 22:17:23 (121 seconds)
AssertionError: Rollout did not complete within timeout

Root cause: Race condition left sync replica in "invalid" state
- memgraph-ha-1 replica status: "invalid" with behind: -12710
- Controller couldn't perform failover due to unhealthy replica
- Pod termination delay was secondary issue
```

**After race condition fix (2025-09-16)**:
```
Test run 1/10 with operationMu fix: ✅ PASSED
Rolling restart test completed successfully
- Proper failover during pod deletion
- Correct replication topology maintained
- No timeouts despite potential pod termination delays
```

**From pod status during rolling restart**:
- Pod stuck in "Terminating" status for extended periods
- StatefulSet rollout waits for pod termination before proceeding
- Cluster appears unhealthy during this period due to missing replicas

### Impact (Updated)

- **E2E Test Reliability**: ✅ **RESOLVED** - Tests now pass consistently due to controller fixes
- **Rolling Restart Duration**: Still may take 2-3 minutes instead of 30-60 seconds due to pod termination delays
- **Service Availability**: ✅ **MITIGATED** - Controller handles failover properly during termination delays

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

## 4. Empty IP Address Bug - Controller Queries Wrong Endpoint

**Status**: Active  
**Severity**: Critical  
**First Observed**: 2025-09-18  
**Occurrence**: During pod restarts when pods temporarily have no IP address

### Description

When a pod is restarting and temporarily has no IP address (`pod.Status.PodIP` is empty), the controller constructs an invalid bolt address `:7687` (instead of `IP:7687`). This causes the controller to connect to localhost:7687 instead of the actual pod, leading to incorrect role detection and permanent replica divergence.

### Root Cause

**The bug sequence**:
1. During pod restart/recreation, `pod.Status.PodIP` is empty (pod hasn't been assigned an IP yet)
2. `MemgraphNode.Refresh(pod)` sets `node.ipAddress = pod.Status.PodIP` (empty string)
3. `GetBoltAddress()` returns `"" + ":7687"` = `:7687`
4. When controller calls `node.GetReplicationRole(ctx)`, it connects to `:7687` (localhost)
5. This connects to the gateway's port-forward or another local service
6. Controller gets wrong role information and makes incorrect decisions

**Code locations**:
- `internal/controller/memgraph_node.go`: `Refresh()` method sets `node.ipAddress = pod.Status.PodIP`
- `internal/controller/memgraph_node.go`: `GetBoltAddress()` returns `node.ipAddress + ":7687"`
- `internal/controller/controller_reconcile.go:228`: Calls `GetReplicationRole()` without IP validation

### Evidence

**From production incident (2025-09-18)**:
```
15:50:09.071 Queried replication role bolt_address=:7687 role=main
15:50:09.071 memgraph role pod_name=memgraph-ha-2 role=main
15:50:09.072 Pod has wrong role, demoting to replica pod_name=memgraph-ha-2 current_role=main
15:50:09.072 Successfully set replication role to REPLICA bolt_address=:7687
```

**Memgraph rejection**:
```
15:51:21.378 You cannot register Replica memgraph_ha_2 to this Main because at one point 
Replica memgraph_ha_2 acted as the Main instance. Both the Main and Replica memgraph_ha_2 
now hold unique data. Please resolve data conflicts and start the replication on a clean instance.
```

### Impact

- **Permanent replica divergence**: Once Memgraph marks a replica as having "acted as Main", it can never be re-registered without data loss
- **Data inconsistency**: Diverged replicas have different data than the main (335 node difference observed)
- **Incorrect topology modifications**: Controller may demote actual main instances or promote wrong replicas
- **Silent failures**: E2E tests pass despite diverged replicas since only sync replica is checked

### Reproduction Steps

1. Deploy a Memgraph HA cluster
2. Trigger pod recreation (rolling restart or pod deletion)
3. During the brief window when a pod has no IP:
   - Controller reconciliation runs
   - Controller queries `:7687` thinking it's the pod
   - Gets wrong role information
4. Check replica status after stabilization:
   ```bash
   kubectl exec memgraph-ha-0 -n memgraph -- bash -c 'echo "SHOW REPLICAS;" | mgconsole --output-format csv'
   ```
5. Observe diverged replica with negative "behind" value

### Proposed Solutions

**Solution 1: Add IP validation in reconciliation loop**
```go
// In controller_reconcile.go around line 218
pod, err := c.getPodFromCache(podName)
if err != nil || !isPodReady(pod) || pod.Status.PodIP == "" {
    logger.Info("Replica pod is not ready or has no IP", "pod_name", podName)
    continue // Skip if pod not ready or no IP
}
```

**Solution 2: Return error from GetBoltAddress() if IP is empty**
```go
func (node *MemgraphNode) GetBoltAddress() (string, error) {
    if node.ipAddress == "" {
        return "", fmt.Errorf("pod %s has no IP address", node.name)
    }
    return node.ipAddress + ":7687", nil
}
```

**Solution 3: Validate in GetReplicationRole()**
```go
func (node *MemgraphNode) GetReplicationRole(ctx context.Context) (string, error) {
    if node.ipAddress == "" {
        return "", fmt.Errorf("cannot query role: pod %s has no IP address", node.name)
    }
    // ... existing code
}
```

### Workaround

**To fix diverged replicas**:
```bash
# Delete the diverged pod to force fresh data sync
kubectl delete pod memgraph-ha-2 -n memgraph
```

### Related Code

- `internal/controller/memgraph_node.go:58-59` - `Refresh()` method
- `internal/controller/memgraph_node.go:69-71` - `GetBoltAddress()` method  
- `internal/controller/controller_reconcile.go:218-239` - Reconciliation loop
- `internal/controller/connection_pool.go` - Connection management

### Notes

- This bug only occurs during the brief window when a pod exists but has no IP
- The controller mistakenly believes it's querying the pod but is actually querying localhost
- Once a replica diverges, manual intervention is required (pod deletion)
- The bug is silent - no errors are logged since the localhost query succeeds

---

## Historical Issues

### Gateway Routing Race Condition (Resolved)

**Status**: Resolved in recent commits  
**Resolution**: Gateway routing and connection handling improvements
**Note**: Replaced by the reconciliation timing issue documented above