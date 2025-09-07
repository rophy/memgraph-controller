# Failover Design

This document describes the failover strategy and implementation for handling main pod failures.

## Failover Actions

**Presumption**: pod status of `TargetMainPod` is not ready.

1. If pod status of `TargetSyncReplica` is also not ready, log error that this cluster is not recoverable and complete the failover as "failed".

2. Gateway disconnects all existing connections.

3. Promote `TargetSyncReplica` to MAIN.

4. Flip `TargetMainPod` with `TargetSyncReplica`, so that original `TargetSyncReplica` is now the `TargetMainPod`

## Failover Event Queue Architecture

The controller implements a dedicated failover event queue to ensure rapid response to main pod failures:

### Queue Characteristics
- **Buffer Size**: 50 events (smaller than reconciliation queue)
- **Processing**: Dedicated goroutine for immediate processing
- **Deduplication**: Prevents duplicate failover attempts within 2-second window
- **Leader-Only**: Only the leader controller processes failover events

### Event Types
- **pod-delete**: Main pod deleted
- **pod-update**: Main pod became unhealthy
- **manual-trigger**: Explicitly requested failover

### Failover Check Process

1. **Health Verification**: Check if TargetMainPod is healthy and has role "main"
2. **Sync Replica Validation**: Verify sync replica is healthy and ready
3. **Replication Status Check**: Confirm sync replica has "ready" status in `data_info`
4. **Promotion**: Promote sync replica to main role
5. **ConfigMap Update**: Update TargetMainIndex to reflect new main

## Safety Checks

Before performing failover, the controller validates:

1. **Pod Health**: Both Kubernetes readiness and Memgraph connectivity
2. **Replication Status**: Sync replica must have status "ready" in SHOW REPLICAS
3. **Sync Mode**: Replica must be in SYNC mode (not ASYNC)
4. **Data Consistency**: Verify `data_info` shows no lag (behind: 0)

## Connection Management During Failover

1. **Immediate Disconnection**: All existing client connections terminated
2. **Gateway State**: Gateway enters "main unavailable" state
3. **Connection Rejection**: New connections rejected until failover completes
4. **Recovery**: Gateway resumes accepting connections after ConfigMap update

## Failover Triggers

### Automatic Triggers
- Main pod deletion detected via Kubernetes informer
- Main pod readiness probe failure
- Main pod IP address change
- Memgraph process unresponsive on main pod

### Manual Triggers
- Admin-initiated failover via API endpoint
- Testing and maintenance operations

## Recovery After Failover

After successful failover:

1. **Old Main Recovery**: When old main pod recovers:
   - Automatically demoted to replica role
   - Registered as SYNC replica to new main
   - Replication established with new topology

2. **State Synchronization**: 
   - ConfigMap updated with new TargetMainIndex
   - All controller pods receive update via informers
   - Gateway routing tables updated

## Error Handling

### Failover Failures
- **Both Pods Unhealthy**: Log critical error, cluster requires manual intervention
- **Promotion Failure**: Retry with exponential backoff
- **ConfigMap Update Failure**: Retry and alert operators

### Mutex Protection
- `failoverMu`: Prevents concurrent failover attempts
- `reconcileMu`: Ensures failover doesn't conflict with reconciliation

## Implementation References

### Core Files
- `queue_failover.go` - Failover queue and processing logic
- `controller_events.go` - Event detection and queuing

### Key Functions
- `performFailoverCheck()` - Main failover logic
- `promoteSyncReplica()` - Handles replica promotion
- `getHealthyRole()` - Validates pod health and role
- `enqueueFailoverCheckEvent()` - Adds events to failover queue

## Metrics and Monitoring

The controller tracks:
- Failover attempt count
- Failover success/failure rate
- Time to complete failover
- Reason for failover trigger

## Related Documents

- For reconciliation logic, see [reconciliation.md](./reconciliation.md)
- For architecture overview, see [architecture.md](./architecture.md)
- For gateway behavior, see [architecture.md](./architecture.md#gateway-design)