# Memgraph Specifications

Pure Memgraph Community Edition specifications and command reference.

## Version

- **Memgraph Community Edition**: 3.4.0

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
- `status: "ready"` = replica is functioning normally  
- `ts: 2` = timestamp/sequence number

**Unhealthy ASYNC Replica:**
```yaml
"{memgraph: {behind: -20, status: \"invalid\", ts: 0}}"
```
- `behind: -20` = negative value indicates replication error
- `status: "invalid"` = replica has failed/broken replication
- `ts: 0` = timestamp reset to zero

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

---

*Pure Memgraph Community Edition 3.4.0 specifications*