# Memgraph Specifications

Pure Memgraph Community Edition specifications and command reference.

## Version

- **Memgraph Community Edition**: 3.5.1 (previously tested with 3.4.0)

## Command Execution

### Running queries against Memgraph instances

```bash
kubectl exec -c memgraph <pod-name> -- bash -c 'echo "<mgcommand>;" | mgconsole --output-format csv --username=memgraph'
```

The `--username=memgraph` is not a real username but is used to avoid Memgraph showing these warnings:

```
[memgraph_log] [warning] The client didn't supply the principal field! Trying with ""...
[memgraph_log] [warning] The client didn't supply the credentials field! Trying with ""...
```

## Replication

Reference: https://memgraph.com/docs/clustering/replication

### Error to Ignore

The following error in replica nodes can safely be ignored:

```
[memgraph_log] [error] Handling SystemRecovery, an enterprise RPC message, without license. Check your license status by running SHOW LICENSE INFO.
```

### Replication Roles

**Default behavior**: A new Memgraph instance ALWAYS starts as MAIN. If you see a Memgraph instance start as REPLICA, it must have been configured.

#### Role Management Commands

**Show current replication role:**
```mgcommand
SHOW REPLICATION ROLE
```

**Demote MAIN to replica:**
```mgcommand
SET REPLICATION ROLE TO REPLICA WITH PORT 10000
```

**Promote replica to MAIN:**
```mgcommand
SET REPLICATION ROLE TO MAIN
```

### Replica Management

#### Key Concepts

- Replica instances do NOT automatically receive data from MAIN
- Replications have to be set up explicitly from MAIN
- All replica management commands are run against the MAIN instance

#### Commands

**Register SYNC replica** (guaranteed consistency - blocks MAIN until confirmed):
```mgcommand
REGISTER REPLICA <replica_name> SYNC TO "<replica_ip>:10000"
```

**Register ASYNC replica** (eventual consistency - non-blocking):
```mgcommand
REGISTER REPLICA <replica_name> ASYNC TO "<replica_ip>:10000"
```

**Register STRICT_SYNC replica** (available in 3.5.1+, highest consistency - blocks MAIN until ALL STRICT_SYNC replicas confirm):
```mgcommand
REGISTER REPLICA <replica_name> STRICT_SYNC TO "<replica_ip>:10000"
```

**Port specification:**
- If port is omitted, it defaults to 10000
- `REGISTER REPLICA replica1 SYNC TO "192.168.1.100"` is equivalent to `REGISTER REPLICA replica1 SYNC TO "192.168.1.100:10000"`

**Replica naming constraints:**
- `replica_name` only accepts lowercase alphanumeric characters ([a-z0-9]) and underscores
- Kubernetes pod names like `memgraph-ha-0` must be normalized to `memgraph_ha_0`
- Hyphens (-) are NOT allowed and will cause registration to fail with parsing error:

```
Failed query: REGISTER REPLICA memgraph-ha-0 ASYNC TO "127.0.0.1"
Client received query exception: Error on line 1 position 26. The underlying parsing error is mismatched input '-' expecting {ASYNC, SYNC}
```

**Drop a replica registration:**
```mgcommand
DROP REPLICA <replica_name>
```

**Show current registered replicas:**
```mgcommand
SHOW REPLICAS
```

### data_info Field Reference

The `data_info` field from `SHOW REPLICAS` provides replication health metrics in YAML flow string format.

#### Observed Values

**Healthy ASYNC Replica:**
```yaml
"{memgraph: {behind: 0, status: \"ready\", ts: 2}}"
```
- `behind: 0` = replica is caught up with MAIN
- `status: "ready"` = replica is fully synchronized and idle
- `ts: 2` = timestamp/sequence number

**Active ASYNC Replica:**
```yaml
"{memgraph: {behind: 0, status: \"replicating\", ts: 4}}"
```
- `behind: 0` = replica is caught up with MAIN
- `status: "replicating"` = replica is actively receiving/processing changes
- Both "ready" and "replicating" indicate functional replication

**Unhealthy ASYNC Replica:**
```yaml
"{memgraph: {behind: -20, status: \"invalid\", ts: 0}}"
```
- `behind: -20` = negative value indicates replication error
- `status: "invalid"` = replica has failed/broken replication
- `ts: 0` = timestamp reset to zero

**Invalid STRICT_SYNC Replica:**
```yaml
"{memgraph: {behind: 0, status: \"invalid\", ts: 4}}"
```
- `status: "invalid"` = replica is unreachable or has conflicting role
- Once STRICT_SYNC replica becomes invalid, manual re-registration is required
- MAIN cannot commit writes until STRICT_SYNC replica is restored

**Failed SYNC Replica:**
```yaml
"{}"
```
- Empty object indicates replication is not working properly
- Failure reason typically recorded in MAIN pod logs
- Requires re-registration of the replica to restore replication

#### Parsing Notes

- Values are YAML strings requiring parsing
- Negative `behind` values indicate replication errors
- `status: "invalid"` is a clear failure indicator
- Empty `{}` values indicate replication failure
- Different replica types (SYNC vs ASYNC) may have different data_info formats

## STRICT_SYNC Mode Behavior (3.5.1+)

### Write Blocking Behavior
- When STRICT_SYNC replica is unavailable/invalid, MAIN **completely blocks write operations**
- Error message: `At least one STRICT_SYNC replica has not confirmed committing last transaction. Transaction will be aborted on all instances.`
- Read operations continue to work normally
- Prevents split-brain scenarios during failover situations

### Recovery Requirements
- Once a STRICT_SYNC replica becomes "invalid" (due to role change, disconnection, etc.), it does NOT automatically recover
- Manual intervention required: `DROP REPLICA` followed by `REGISTER REPLICA` with STRICT_SYNC mode
- Controller must handle re-registration after any role changes to restore write capability

### Split-Brain Prevention
STRICT_SYNC mode prevents the dual-main issue during rolling restart:
1. If pod-0 (old main) returns after failover and thinks it's still main
2. It cannot write data because pod-1 (now the actual main) is no longer its STRICT_SYNC replica
3. This prevents data divergence that would occur with regular SYNC mode

## Network Partition and Failover Behavior

### Async Replica Isolation Constraint

**Critical Memgraph Characteristic**: When an async replica is disconnected from the main during a network partition, it will **refuse to rejoin the cluster after failover**, even if the data between the new main and sync replica is perfectly synchronized.

#### The Problem Scenario

1. **Initial State**: Main with sync replica and async replica, all synchronized
2. **Network Partition**: Async replica becomes isolated (e.g., NetworkPolicy blocking traffic)
3. **Main Failure**: Original main fails, controller promotes sync replica to new main
4. **Partition Removal**: Network connectivity restored to async replica
5. **Rejection**: Async replica refuses to rejoin with error:
   ```
   You cannot register Replica <name> to this Main because at one point
   Replica <name> acted as the Main instance. Both the Main and Replica
   <name> now hold unique data. Please resolve data conflicts and start
   the replication on a clean instance.
   ```

#### Root Cause

During isolation, the async replica **automatically promotes itself to main** when it cannot reach the original main. When the network partition is resolved, Memgraph's split-brain protection detects that the async replica previously acted as main and refuses to allow it to become a replica again, regardless of actual data consistency.

#### Controller Design Implications

**The controller cannot simply assume isolated async replicas will rejoin after failover.** Key constraints:

1. **Proactive Management Required**: Controller must track and manage isolated replicas during failover
2. **Clean Rejoin Process**: Isolated async replicas need data cleanup/reset before re-registration
3. **Failover Timing**: Consider dropping unreachable async replicas before promoting sync replica
4. **Monitoring**: Track replica isolation state and handle rejoining scenarios explicitly

#### Current Controller Limitation

The current controller logic "trusts Memgraph auto-retry" for replica health issues, but this fails for split-brain scenarios where Memgraph explicitly refuses the registration. The controller should detect this specific error pattern and take corrective action (drop + clean + re-register).

#### NetworkPolicy Testing Insights

- NetworkPolicy blocks **new connections** but preserves **existing connections**
- After pod restart, NetworkPolicy effectively isolates the replica
- Controller health checks and replication queries fail due to connection timeouts
- This creates realistic network partition scenarios for testing failover behavior

---

*Pure Memgraph Community Edition 3.5.1 specifications*