# Memgraph Controller with Gateway

A Kubernetes controller that manages Memgraph clusters with built-in TCP gateway for transparent failover.

## Architecture Overview

This controller implements a **Master-SYNC-ASYNC** replication topology that provides robust write conflict protection and automatic failover capabilities.

### Cluster Topology

```
┌─────────────┐    SYNC     ┌──────────────┐
│   Pod-0     │◄───────────►│   Pod-1      │
│  (Master)   │             │(SYNC Replica)│
└─────┬───────┘             └──────────────┘
      │
      │ ASYNC     
      │
      ▼
┌───────────────┐
│   Pod-2       │
│(ASYNC Replica)│
└───────────────┘
```

### Key Design Principles

1. **Two-Pod Authority**: Only pod-0 and pod-1 are eligible for Master/SYNC replica roles
2. **SYNC Replica Strategy**: One SYNC replica ensures zero data loss during failover
3. **Deterministic Failover**: SYNC replica is always promoted during master failure
4. **Write Conflict Protection**: SYNC replication prevents dual-master scenarios

## Write Conflict Protection

The Master-SYNC-ASYNC topology provides robust protection against write conflicts through Memgraph's built-in SYNC replication mechanism:

### How SYNC Protection Works

1. **Master Dependency**: The master cannot commit transactions until the SYNC replica acknowledges them
2. **Promotion Safety**: When a SYNC replica is promoted to master, it stops acknowledging the old master
3. **Write Blocking**: The old master becomes write-blocked when its SYNC replica becomes unavailable
4. **Clean Failover**: Only the new master can accept writes, preventing dual-master conflicts

### Conflict Scenarios Analysis

#### ✅ **Network Partition** (Protected)
- **Scenario**: Controller loses connection to master but master can reach replicas
- **Protection**: When SYNC replica is promoted, it stops acknowledging old master transactions
- **Result**: Old master becomes write-blocked, new master handles all writes

#### ✅ **Controller Split-Brain** (Protected)  
- **Scenario**: Multiple controllers try to manage the same cluster
- **Protection**: SYNC replica can only acknowledge one master at a time
- **Result**: Only one master remains operational, others become write-blocked

#### ✅ **Gradual Failure** (Protected)
- **Scenario**: Master becomes slow/degraded but not completely failed  
- **Protection**: SYNC replica promotion immediately blocks old master writes
- **Result**: Clean transition to new master without conflicts

#### ⚠️ **Manual Intervention** (Risk)
- **Scenario**: Manual `DROP REPLICA` commands or configuration changes
- **Protection**: None - manual changes can override safety mechanisms
- **Mitigation**: Operational procedures and access controls

## Controller Lifecycle

This controller has two phase throughout its lifecycle: BOOTSTRAP and OPERATIONAL.

### Bootstrap Phase

Controller starts up as BOOTSTRAP phase, which goal is discover current state of a memgraph-ha-cluster.

Below describes the rules, which are expected to be deterministic.

1. If memgraph statefulset has <2 replicas with pod status as "ready", wait. Proceed to next step ONLY after >=2 replicas ready.

2. If both pod-0 and pod-1 have replication role as `MAIN` and storage shows 0 edge_count, 0 vertex_count, this cluster is in `INITIAL_STATE`.

3. If one of pod-0 and pod-1 has replication role as `REPLICA`, the other one as `MAIN`, this cluster is in `OPERATIONAL_STATE`.

4. Otherwise, the cluster is in `UNKNOWN_STATE`.

#### INITIAL_STATE

Controller always use pod-0 as MAIN, pod-1 as SYNC REPLICA.

Controller will perform following steps to set up the cluster, and then go into OPERATIONAL phase.

1. Run this command against pod-1 to demote it into replica:

```mgconsole
SET REPLICATION ROLE TO REPLICA WITH PORT 10000
```

2. Run this command against pod-0 to set up sync replication:

```mgconsole
REGISTER REPLICA <pod_1_name> SYNC TO "<pod_1_ip>:10000"
```
3. Run this command against pod-0 to verify replication:

```mgconsole
SHOW REPLICAS
```

Replication `<pod_1_name>` should show following in `data_info` field:

```yaml
{memgraph: {behind: 0, status: "ready", ts: 0}}
```

In case replica status of `<pod_1_name>` is not "ready", log warning and do exponential retry. In such case, memgraph-controller will stay in INITIAL_STATE.

Once replication is good, controller picks pod-0 as MAIN, and then go into OPERATIONAL phase.


#### OPERATIONAL_STATE

Controller pick the active MAIN as MAIN, and then go into OPERATIONAL phase.


#### UNKNOWN_STATE

Controller log error and crash immediately, expecting human to fix the cluster.


### Operational Phase

Once controller enters OPERATIONAL phase, controller continuously reconciles the cluster into expected status.

In this phase, controller receive events to kubernetes and do things as necessary to fix things.

#### Actions to Kubernetes Events

- Memgraph pod IP changes:
  - Controller updates pod IP information, and wait for pod ready event.
- Main memgraph pod status changed to "not ready":
  - If sync replica status is READY, controller uses it as master, and promotes it immediately.
  - If sync replica status is not READY, controller logs error, and waits for master to recover.
- SYNC replica memgraph pod status changed to "not ready":
  - Controller logs error than master will become read-only, and waits for async replica to recover.
- ASYNC replica memgraph pod status changed to "not ready":
  - Controller logs warning, drops the replication from master, and waits for async replica to recover.
- Any memgraph pod status changed to "ready":
  - Controller performs reconciliation.

### Actions for Reconciliation

1. Call kubernetes api to get memgraph pods which status is "ready", available to receive traffic.

2. Run `SHOW REPLICAS` to main pod to check replication status.

3. If `data_info` of SYNC replica is not `ready`, drop the replication and re-register immediately.

3. If `data_info` for any ASYNC replica is not `ready`, drop the replication.

4. If replication for any pod-N is is missing (could be dropped in step 3):

  1. Check replication role of the pod, if it is `MAIN`, demote it into `REPLICA`.
  2. Register ASYNC replica for the pod.

5. Once all register done, run `SHOW REPLICAS` to check final result:

  - If `data_info` of SYNC replica is not `ready`, log big error.
  - If `data_info` of ASYNC replica is not `ready`, log warning.

## Gateway Integration

The controller includes an embedded TCP gateway that provides transparent failover for client connections:

### Features
- **Transparent Proxying**: Raw TCP proxy to current master (no protocol interpretation)
- **Automatic Failover**: Terminates all connections on master change, clients reconnect to new master
- **Connection Tracking**: Full session lifecycle management with metrics
- **Health Monitoring**: Master connectivity validation and error rate tracking

### Configuration
```bash
# Enable gateway functionality
GATEWAY_ENABLED=true
GATEWAY_BIND_ADDRESS=0.0.0.0:7687
GATEWAY_MAX_CONNECTIONS=1000
GATEWAY_TIMEOUT=30s
```

## Deployment

The controller manages Memgraph StatefulSets with the following operational characteristics:

- **Bootstrap Safety**: Conservative startup - refuses ambiguous cluster states
- **Operational Authority**: Enforces known topology, resolves split-brain scenarios  
- **Master Selection**: SYNC replica priority, deterministic fallback to pod-0
- **Reconciliation**: Event-driven + periodic reconciliation with exponential backoff

## Monitoring

The controller exposes comprehensive metrics through its HTTP API:

- **Cluster State**: Master/replica roles, replication status
- **Gateway Stats**: Active connections, failover count, error rates  
- **Health Status**: Master connectivity, error thresholds, system health

Access metrics at: `http://controller:8080/status`

# Study Notes on Memgraph Community Edition

memgraph CE version: 3.4.0

## How to run cypher queries or commands against a memgraph instance

```bash
kubectl exec <pod-name> -- bash -c 'echo "<mgcommand>;" | mgconsole --output-format csv --username=memgraph'
```

The `--username=memgraph` is not a real username, it is to avoid memgraph showing following warnings:

```
[memgraph_log] [warning] The client didn't supply the principal field! Trying with ""...
[memgraph_log] [warning] The client didn't supply the credentials field! Trying with ""...
```

## Memgraph Replication

Reference: https://memgraph.com/docs/clustering/replication

- The following error in replica nodes can safely be ignored.

```
[memgraph_log] [error] Handling SystemRecovery, an enterprise RPC message, without license. Check your license status by running SHOW LICENSE INFO.
```

### Setting Replication Roles

Note: A new memgraph instance ALWAYS starts as MAIN. If you see a memgraph instance starts as REPLICA, it must have been configured.

Show replication role of a memgraph instance:

```mgcommand
SHOW REPLICATION ROLE
```

Demote master to replica:

```mgcommand
SET REPLICATION ROLE TO REPLICA WITH PORT 10000
```

Promote replica to mater:

```mgcommand
"SET REPLICATION ROLE TO MAIN
```

### Managing Replicas

#### Concept

- Replica instances do NOT automatically receive data from master. 
- Replications have to be set up explicitly from mater.
- All commands in this section are run against MASTER instance.

#### Commands For Managing Replications

1. Register target replica as a SYNC replica (guaranteed consistency - blocks master until confirmed)

```mgcommand
REGISTER REPLICA <replica_name> SYNC TO "<replica_ip>:10000"
```
For `replica_name`, I always use `pod_name` - pod-name converted to underscores.

2. Register target replica as ASYNC replica (eventual consistency - non-blocking)

```mgcommand
REGISTER REPLICA <replica_name> ASYNC TO "<replica_ip>:10000"
```

3. Drop a registration

```mgcommand
DROP REPLICA <replica_name>
```

4. Show current replicas registered in master

```mgcommand
SHOW REPLICAS
```

