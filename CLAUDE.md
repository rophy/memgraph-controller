Notes for running up a memgraph cluster under community edition.

Reference: https://memgraph.com/docs/clustering/replication

- The following error in replica nodes can safely be ignored.

```
[memgraph_log] [error] Handling SystemRecovery, an enterprise RPC message, without license. Check your license status by running SHOW LICENSE INFO.
```

# Development Workflow

## CRITICAL: Claude NEVER Runs Deployment Commands

**Claude must NEVER run the following commands:**
- `skaffold run`
- `skaffold dev` 
- `kubectl apply`
- `docker build`
- Any deployment or build commands

**After implementing fixes, Claude should explicitly state: "Please run skaffold and I'll check the logs"**

## Standard Development Process

Our development workflow follows this pattern:

1. **Claude Implements**: I implement the requested feature or fix
2. **User Runs Skaffold**: You run `skaffold run` or `skaffold dev` to deploy and test
3. **Claude Checks Logs**: I check the deployment logs to verify functionality and identify issues

## Change Management Protocol

When issues are found during log analysis, I will:

1. **Describe the Fix First**: Clearly explain what needs to be changed and why
2. **Ask for Confirmation**: Wait for your explicit approval before making changes
3. **Implement Only After Approval**: Make the changes only after you confirm

This ensures you stay informed about all modifications and maintains control over the codebase evolution.

## Debugging Memgraph Replication

**For debugging replication issues, always query Memgraph directly using mgconsole in the pods:**

```bash
# Check replication role of a pod
kubectl exec <pod-name> -- bash -c 'echo "SHOW REPLICATION ROLE;" | mgconsole --output-format csv --username=memgraph'

# Check registered replicas from master pod
kubectl exec <master-pod-name> -- bash -c 'echo "SHOW REPLICAS;" | mgconsole --output-format csv --username=memgraph'

# Check storage info
kubectl exec <pod-name> -- bash -c 'echo "SHOW STORAGE INFO;" | mgconsole --output-format csv --username=memgraph'
```

**Do NOT rely on the memgraph-controller status API for debugging** - always verify the actual Memgraph state directly using the above commands.

## Memgraph Replication Commands

### Setting Replication Roles

**CRITICAL**: Memgraph Community Edition requires specifying a port when setting replica role.

```bash
# Promote pod to master
kubectl exec <pod-name> -- bash -c 'echo "SET REPLICATION ROLE TO MAIN;" | mgconsole --output-format csv --username=memgraph'

# Demote pod to replica (Community Edition requires WITH PORT)
kubectl exec <pod-name> -- bash -c 'echo "SET REPLICATION ROLE TO REPLICA WITH PORT 10000;" | mgconsole --output-format csv --username=memgraph'
```

### Managing Replicas

```bash
# Register SYNC replica (guaranteed consistency - blocks master until confirmed)
kubectl exec <master-pod> -- bash -c 'echo "REGISTER REPLICA <replica_name> SYNC TO \"<replica_ip>:10000\";" | mgconsole --output-format csv --username=memgraph'

# Register ASYNC replica (eventual consistency - non-blocking)
kubectl exec <master-pod> -- bash -c 'echo "REGISTER REPLICA <replica_name> ASYNC TO \"<replica_ip>:10000\";" | mgconsole --output-format csv --username=memgraph'

# Drop replica registration
kubectl exec <master-pod> -- bash -c 'echo "DROP REPLICA <replica_name>;" | mgconsole --output-format csv --username=memgraph'

# Check replica status and sync modes
kubectl exec <master-pod> -- bash -c 'echo "SHOW REPLICAS;" | mgconsole --output-format csv --username=memgraph'
```

### Emergency Recovery Procedures

#### Scenario: SYNC Replica Down, Writes Blocked

**Option 1: Fast SYNC Replica Recovery (Preferred)**
```bash
# Force restart of SYNC replica pod
kubectl delete pod <sync-replica-pod>
# Wait for pod to restart - writes will resume automatically
```

**Option 2: Promote ASYNC Replica to SYNC (Emergency)**
```bash
# Step 1: Drop the failed SYNC replica
kubectl exec <master-pod> -- bash -c 'echo "DROP REPLICA <failed_sync_replica_name>;" | mgconsole --output-format csv --username=memgraph'

# Step 2: Promote healthy ASYNC replica to SYNC
kubectl exec <master-pod> -- bash -c 'echo "DROP REPLICA <async_replica_name>;" | mgconsole --username=memgraph'
kubectl exec <master-pod> -- bash -c 'echo "REGISTER REPLICA <async_replica_name> SYNC TO \"<replica_ip>:10000\";" | mgconsole --output-format csv --username=memgraph'

# Step 3: Verify new SYNC replica
kubectl exec <master-pod> -- bash -c 'echo "SHOW REPLICAS;" | mgconsole --output-format csv --username=memgraph'
```

#### Scenario: Split-Brain Resolution

**Manual Split-Brain Resolution (if controller fails to resolve automatically):**
```bash
# Step 1: Identify all masters
kubectl exec memgraph-ha-0 -- bash -c 'echo "SHOW REPLICATION ROLE;" | mgconsole --output-format csv --username=memgraph'
kubectl exec memgraph-ha-1 -- bash -c 'echo "SHOW REPLICATION ROLE;" | mgconsole --output-format csv --username=memgraph'

# Step 2: Choose master with most recent data (check storage info)
kubectl exec <pod> -- bash -c 'echo "SHOW STORAGE INFO;" | mgconsole --output-format csv --username=memgraph'

# Step 3: Demote incorrect masters (keep lowest index pod as master by convention)
kubectl exec <incorrect-master-pod> -- bash -c 'echo "SET REPLICATION ROLE TO REPLICA WITH PORT 10000;" | mgconsole --output-format csv --username=memgraph'

# Step 4: Restart controller after manual resolution
kubectl rollout restart deployment/memgraph-controller -n memgraph
```

## Controller Design: SYNC Replica Strategy

### Overview

The controller implements a **SYNC replica strategy** for zero data loss failover:

- **1 SYNC replica**: Guaranteed to have all committed transactions (blocks master until confirmed)
- **N-1 ASYNC replicas**: May lag behind but provide read scalability  
- **Master failure**: Always promote the SYNC replica (guaranteed zero data loss)

### Two-Pod Master/SYNC Strategy

- **pod-0 and pod-1**: Eligible for master OR SYNC replica roles
- **pod-2, pod-3, ...**: ALWAYS ASYNC replicas only
- **Controller Authority**: Maintains expected topology in-memory after bootstrap

### Bootstrap Safety vs. Operational Authority

- **Bootstrap**: Conservative - refuse to start on ambiguous states (mixed main/replica)
- **Operational**: Authoritative - enforce known topology against drift, resolve split-brain

### Key Operational Behaviors

#### SYNC Replica Failure Impact
- **SYNC replica failure** = **Complete write outage**
- **No graceful degradation** to ASYNC mode  
- **Manual intervention required** to restore write capability
- **Reads unaffected** but cluster effectively becomes read-only

#### Split-Brain Resolution
- **Controller enforces known master** during operational phase
- **Demotes restarted pods** that incorrectly promoted themselves to master
- **Uses lower-index precedence** as fallback (pod-0 over pod-1)

#### Master Failover Priority
1. **Existing MAIN node** (avoid unnecessary failover)
2. **SYNC replica** (guaranteed data consistency)
3. **Deterministic selection** (pod-0 default) with warnings about potential data loss

This design ensures **robust, predictable behavior** while preventing data loss during master failures through guaranteed SYNC replica consistency.

## Testing

### Unit Tests

```bash
make test
```

### E2E TEsts

This should be run by human:

```
make run
```

Once skaffold is up and running with port-forward enabled, this can be run by either human or AI:

```bash
make test-e2e
```

### Manual Verify

To send data to a specific pod using kubectl exec with mgconsole, use this format:

  kubectl exec <pod-name> -n memgraph -c memgraph -- bash -c 'echo "<CYPHER_QUERY>" | mgconsole'

  Examples:

  Send to master (memgraph-ha-0):
  kubectl exec memgraph-ha-0 -n memgraph -c memgraph -- bash -c 'echo "CREATE (n:TestNode {id: \"test-123\", value: \"my-test\"});" | mgconsole'

  Send to SYNC replica (memgraph-ha-1):
  kubectl exec memgraph-ha-1 -n memgraph -c memgraph -- bash -c 'echo "CREATE (n:TestNode {id: \"test-456\", value: \"direct-to-replica\"});" | mgconsole'

  Query data from any pod:
  kubectl exec memgraph-ha-0 -n memgraph -c memgraph -- bash -c 'echo "MATCH (n:TestNode) RETURN n.id, n.value;" | mgconsole'

  Check count:
  kubectl exec memgraph-ha-1 -n memgraph -c memgraph -- bash -c 'echo "MATCH (n) RETURN count(n);" | mgconsole'





## Known Issues

### SYNC Replication Not Functioning Properly

**Issue**: SYNC replication configuration appears correct in controller logs, but actual data synchronization is failing.

**Symptoms**:
- Controller reports "✅ Enhanced SYNC replication configuration completed"
- `SHOW REPLICAS` shows SYNC replica with `sync_mode: sync`
- However, data written to master does NOT replicate to SYNC replica
- ASYNC replicas receive data correctly
- Writes proceed without waiting for SYNC confirmation (should block until SYNC replica confirms)

**Impact**:
- **Data loss during failover**: When master fails, SYNC replica becomes new master but lacks recent data
- **False sense of security**: Controller believes SYNC replication is working, but it's not
- **Test failures**: E2E failover tests fail because expected data doesn't exist in failover target

**Debugging Steps**:
```bash
# 1. Write test data to master via gateway
# 2. Check data exists in all pods individually
kubectl exec memgraph-ha-0 -n memgraph -c memgraph -- bash -c 'echo "MATCH (n:TestNode) RETURN count(n);" | mgconsole --output-format csv --username=memgraph'
kubectl exec memgraph-ha-1 -n memgraph -c memgraph -- bash -c 'echo "MATCH (n:TestNode) RETURN count(n);" | mgconsole --output-format csv --username=memgraph'
kubectl exec memgraph-ha-2 -n memgraph -c memgraph -- bash -c 'echo "MATCH (n:TestNode) RETURN count(n);" | mgconsole --output-format csv --username=memgraph'

# 3. Verify replication topology
kubectl exec <master-pod> -n memgraph -c memgraph -- bash -c 'echo "SHOW REPLICAS;" | mgconsole --output-format csv --username=memgraph'
```

**Expected**: Data should exist in master and SYNC replica, may be missing from ASYNC replica  
**Actual**: Data exists in master and ASYNC replica, missing from SYNC replica

**Investigation Finding**: Controller ignores `data_info` field from `SHOW REPLICAS` command
- `ReplicaInfo` struct in `pkg/controller/memgraph_client.go:22-28` missing `DataInfo` field
- Parsing logic at `memgraph_client.go:175-215` skips `data_info` field entirely  
- Controller only checks `SystemTimestamp` but this field appears unused/incorrect
- Critical replication health indicators like `behind: -20, status: "invalid"` are ignored
- This prevents early detection and diagnosis of replication failures

**Potential Improvement**:
1. Add `DataInfo string` field to `ReplicaInfo` struct
2. Parse `data_info` field in replica parsing logic
3. Add replication health validation that detects `status: "invalid"` 
4. Use `behind` metric to monitor replication lag
5. Log/alert when replicas show unhealthy data_info status

**Status**: **INVESTIGATING** - Root cause of SYNC replication failure still unknown, but monitoring improvements identified

### data_info Field Values Documentation

This section documents observed `data_info` values from `SHOW REPLICAS` during testing to help implement proper parsing logic.

**Format**: `data_info` appears to be a JSON-like string containing replication health metrics.

#### Observed Values

**Healthy ASYNC Replica**:
```
"{memgraph: {behind: 0, status: \"ready\", ts: 2}}"
```
- `behind: 0` = replica is caught up
- `status: "ready"` = replica is functioning normally  
- `ts: 2` = timestamp/sequence number

**Unhealthy ASYNC Replica**:
```
"{memgraph: {behind: -20, status: \"invalid\", ts: 0}}"
```
- `behind: -20` = negative value indicates replication error
- `status: "invalid"` = replica has failed/broken replication
- `ts: 0` = timestamp reset to zero

**SYNC Replica (Empty)**:
```
"{}"
```
- Empty object - unclear if this indicates healthy or problematic state
- May require different parsing logic than ASYNC replicas

#### Notes for Implementation
- Values appear to be JSON strings that need parsing
- Negative `behind` values seem to indicate errors
- `status: "invalid"` is a clear failure indicator
- Empty `{}` values need investigation (healthy vs unhealthy)
- Different replica types (SYNC vs ASYNC) may have different data_info formats

*This section will be updated with additional values found during testing*
