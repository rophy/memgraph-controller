# Memgraph Controller with Gateway

A Kubernetes controller that manages Memgraph clusters with built-in TCP gateway for transparent failover.

## Architecture Overview

This controller implements a **Master-SYNC-ASYNC** replication topology that provides robust write conflict protection and automatic failover capabilities.

### Cluster Topology

```
┌─────────────┐    SYNC     ┌─────────────┐
│   Pod-0     │◄───────────►│   Pod-1     │
│  (Master)   │             │(SYNC Replica)│
└─────┬───────┘             └─────────────┘
      │                              │
      │ ASYNC                        │ ASYNC  
      │                              │
      ▼                              ▼
┌─────────────┐             ┌─────────────┐
│   Pod-2     │             │   Pod-N     │
│(ASYNC Replica)│             │(ASYNC Replica)│
└─────────────┘             └─────────────┘
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

### Validation Test Results

We validated this protection through live testing:

1. **Setup**: pod-0 (master) → pod-1 (SYNC) → pod-2 (ASYNC)
2. **Failover**: Promoted pod-1 to master while pod-0 remained running
3. **Write Test**: Attempted writes from both pod-0 and pod-1
4. **Result**: 
   - ✅ Pod-0 write blocked: "At least one SYNC replica has not confirmed committing last transaction"
   - ✅ Pod-1 writes succeeded
   - ✅ Pod-2 cleanly switched to follow pod-1
   - ✅ No data corruption or conflicts detected

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
