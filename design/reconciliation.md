# Reconciliation Design

This document describes the reconciliation logic for maintaining cluster state consistency.

## Controller Code Flow

1. Initialization:
  - Setup informers.
  - Start HTTP service.
  - Start gateway service.
  - Start leader election.
2. Start reconciliation loop, which is described in section "Reconciliation Loop"

## Reconciliation Loop

1. If not leader then do no-op and ends the reconcile iteration.

2. If configmap does not exist, perform actions in section "Discover Cluster State".

3. Finally, perform actions in section "Reconcile Actions".

## Reconcile Actions

1. Call kubernetes api to list all memgraph pods, along with their kubernetes status (ready or not). Define this list as `podList`

2. If `TargetMainPod` is not ready, attempt to perform actions in section "Failover Actions" (see [failover.md](./failover.md)). Only continue if "Failover Actions" succeeded.

3. Run `SHOW REPLICAS` to `TargetMainPod` to get registered replications. Define this list as `replicaList`.

4. If `data_info` of `TargetSyncReplica` is not "ready", drop the replication.

5. If pod status of `TargetSyncReplica` is not "ready", log warning.

6. If `data_info` for any ASYNC replica is not `ready`, drop the replication.

7. If replication for any pod which outside pod-0/pod-1 is missing (could be dropped in step 3 or 4):

   1. If the pod is not ready (i.e. not in the list of step 3), log warning
   2. If the pod is ready, check replication role of the pod, if it is `MAIN`, demote it into `REPLICA`.
   3. Register ASYNC replica for the pod.

8. Once all register done, run `SHOW REPLICAS` to check final result:

   - If `data_info` of SYNC replica is not `ready`, log big error.
   - If `data_info` of ASYNC replica is not `ready`, log warning.

## Discover Cluster State

1. If kubernetes status of either pod-0 or pod-1 is not ready, log warning and stop.

2. If both pod-0 and pod-1 have replication role as `MAIN` and storage shows 0 edge_count, 0 vertex_count, perform "Initialize Memgraph Cluster" as described in next section, and create configmaps.

3. If one of pod-0 and pod-1 has replication role as `REPLICA`, the other one as `MAIN`, set the `MAIN` pod as `TargetMainPod`, and create configmap.

4. Otherwise, memgraph-ha is in an unknown state, controller log error and crash immediately, expecting human to fix the cluster.

## Initialize Memgraph Cluster

Controller always use pod-0 as MAIN, pod-1 as SYNC REPLICA.

1. Run this command against pod-1 to demote it into replica:

```mgconsole
SET REPLICATION ROLE TO REPLICA WITH PORT 10000
```

2. Run this command against pod-0 to set up sync replication:

```mgconsole
REGISTER REPLICA <pod_1_name> SYNC TO "<pod_1_fqdn>"
```
3. Run this command against pod-0 to verify replication:

```mgconsole
SHOW REPLICAS
```

Replication `<pod_1_name>` should show following in `data_info` field:

```yaml
{memgraph: {behind: 0, status: "ready", ts: 0}}
```

Once replication is good, controller picks pod-0 as MAIN, and create configmap.

## Actions to Kubernetes Events

- **Memgraph pod status changed to "not ready"**:
  - **If pod is `TargetMainPod`:**
    - **[Leader only]** Immediately terminate all gateway connections
    - **[Leader only]** Gateway rejects new connections (main unavailable state)
    - **[Leader only]** Flip `TargetMainPod` with `TargetSyncReplica`
    - **[Leader only]** Promote new `TargetMainPod` immediately
    - **[Leader only]** Update ConfigMap with new `TargetMainIndex`
    - **[ALL controllers]** Receive ConfigMap update → Gateway accepts connections to new main
  - **If pod is `TargetSyncReplica`:**
    - Controller logs error that MAIN will become read-only
    - Wait for `TargetSyncReplica` to recover
  - **If pod is neither pod-0 or pod-1:**
    - Drop replication from `TargetMainPod`
    - Wait for async replica to recover

## Reconciliation Event Queue

The controller maintains two event queues for processing:

### Reconciliation Queue
- Handles general state synchronization events
- Processes pod add/update/delete events
- Includes deduplication to prevent redundant operations
- Default buffer size: 100 events

### Failover Check Queue
- Dedicated queue for critical failover events
- Higher priority processing for main pod failures
- Smaller buffer (50 events) as failovers are less frequent
- See [failover.md](./failover.md) for detailed failover logic

## Implementation References

### Core Files
- `controller_reconcile.go` - Main reconciliation loop implementation
- `controller_events.go` - Event handlers for Kubernetes informers
- `memgraph_cluster.go` - Cluster state discovery and initialization
- `queue_reconcile.go` - Reconciliation event queue
- `queue_failover.go` - Failover event queue (see [failover.md](./failover.md))

### Key Functions
- `performReconciliationActions()` - Implements reconcile actions
- `discoverClusterState()` - Implements cluster discovery logic
- `initializeCluster()` - Sets up new cluster with replication

## Related Documents

- For architecture overview, see [architecture.md](./architecture.md)
- For failover specifics, see [failover.md](./failover.md)