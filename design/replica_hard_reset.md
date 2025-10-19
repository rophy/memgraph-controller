# Replica Hard Reset Design

This document describes the mechanism for performing a "hard reset" on async replica pods to recover from diverged data states.

## Problem Statement

From **Issue #7 (KNOWN_ISSUES.md)**: During rolling restarts with multiple failovers, async replicas can become diverged due to persistent storage retaining replication metadata from a previous main pod's timeline. When a replica tries to register to a new main with incompatible timeline data, Memgraph returns "Error: 3 - diverged data".

**Current Manual Workaround:**
```bash
# 1. Delete pod
kubectl delete pod memgraph-ha-2 -n memgraph

# 2. Drop replica from main
kubectl exec -c memgraph memgraph-ha-1 -- bash -c 'echo "DROP REPLICA memgraph_ha_2;" | mgconsole ...'

# 3. Re-register replica
kubectl exec -c memgraph memgraph-ha-1 -- bash -c 'echo "REGISTER REPLICA memgraph_ha_2 ASYNC TO ..." | mgconsole ...'
```

**Constraint:** Cannot delete PVC directly while Memgraph pod is running.

## Design Overview

Implement an **automated hard reset mechanism** using a magic file marker + initContainer detection pattern:

1. **Controller** detects diverged async replica during reconciliation
2. **Controller** creates a magic marker file in the pod's PVC filesystem
3. **Controller** deletes the diverged pod
4. **Kubernetes** recreates the pod with StatefulSet guarantees
5. **InitContainer** detects the magic marker file on pod startup
6. **InitContainer** performs full data cleanup (removes all Memgraph data)
7. **Memgraph container** starts with clean slate
8. **Controller** registers the clean replica to current main (normal reconciliation)

## Architecture Components

### 1. Magic Marker File

**Location:** `/var/lib/memgraph/.hard-reset-requested`

**Format:** JSON metadata file
```json
{
  "timestamp": "2025-10-19T12:34:56Z",
  "reason": "diverged_data",
  "requested_by": "memgraph-controller",
  "original_main": "memgraph-ha-1",
  "replica_name": "memgraph_ha_2",
  "error_code": 3
}
```

**Purpose:**
- Survives pod deletion (written to PVC)
- Triggers initContainer cleanup logic
- Provides audit trail for debugging

### 2. InitContainer: Enhanced WAL Cleanup with Hard Reset

**Container Name:** `data-cleanup` (existing container, enhanced)

**Image:** `library/busybox` (lightweight, already available)

**Execution Order:** Existing initContainer, no new containers added

**Responsibilities:**
1. **Hard Reset Logic** (NEW): Check for magic marker file and perform full cleanup if requested
2. **WAL Cleanup Logic** (EXISTING): Clean WAL files for replicas on restart

**Implementation Strategy:** Enhance existing `data-cleanup` initContainer to handle both scenarios

**Enhanced Implementation:**
```yaml
initContainers:
  - name: data-cleanup
    image: library/busybox
    imagePullPolicy: IfNotPresent
    command: ['/bin/sh', '-c']
    args:
      - |
        echo "========================================="
        echo "Memgraph Data Cleanup Init Container"
        echo "========================================="
        echo "Pod: $(hostname)"
        echo "Timestamp: $(date)"

        MARKER_FILE="/var/lib/memgraph/.hard-reset-requested"
        REPLICATION_META="/var/lib/memgraph/mg_data/replication"
        WAL_DIR="/var/lib/memgraph/mg_data/wal"

        # === HARD RESET CHECK (Priority 1) ===
        if [ -f "$MARKER_FILE" ]; then
          echo ""
          echo "🔥 HARD RESET REQUESTED - Magic marker file detected"
          echo "Reading marker file:"
          cat "$MARKER_FILE" || echo "Failed to read marker file"

          echo ""
          echo "🗑️  Performing FULL data cleanup..."

          # Remove all Memgraph data directories
          rm -rf /var/lib/memgraph/mg_data/* || true

          # Remove marker file to prevent repeated cleanup
          rm -f "$MARKER_FILE"

          echo "✅ Hard reset complete - pod will start with clean slate"
          echo "========================================="
          exit 0
        fi

        # === NORMAL WAL CLEANUP (Priority 2) ===
        echo ""
        echo "ℹ️  No hard reset requested - checking for WAL cleanup"

        # Check if this is first startup (no metadata exists)
        if [ ! -d "$REPLICATION_META" ]; then
          echo "First startup detected - no replication metadata exists"
          echo "Skipping WAL cleanup"
          echo "========================================="
          exit 0
        fi

        # Check replication role from metadata
        ROLE=$(grep -rao '"replication_role":"[^"]*"' "$REPLICATION_META" 2>/dev/null | tail -1 | sed 's/.*"replication_role":"\([^"]*\)".*/\1/')
        echo "Detected replication role: ${ROLE:-unknown}"

        # Only clean WAL if this pod was a replica
        if [ "$ROLE" = "replica" ]; then
          echo "Pod was REPLICA - checking for WAL files"
          if [ -d "$WAL_DIR" ] && [ -n "$(ls -A $WAL_DIR 2>/dev/null)" ]; then
            echo "WAL files found:"
            ls -lah "$WAL_DIR"
            echo "Removing WAL files to prevent divergence..."
            rm -f "$WAL_DIR"/*
            echo "✅ WAL cleanup completed successfully"
            ls -lah "$WAL_DIR"
          else
            echo "No WAL files to clean"
          fi
        else
          echo "Pod was MAIN or unknown role - skipping WAL cleanup"
        fi

        echo "========================================="
        echo "Init container finished"
        echo "========================================="
    volumeMounts:
      - name: lib-storage
        mountPath: /var/lib/memgraph
```

**Logic Flow:**
1. **Priority 1 - Hard Reset Check:**
   - Check for magic marker file `/var/lib/memgraph/.hard-reset-requested`
   - If found → Full cleanup of `/var/lib/memgraph/mg_data/*` + remove marker + exit
   - This handles diverged replica recovery

2. **Priority 2 - Normal WAL Cleanup:**
   - If no hard reset marker, proceed with existing WAL cleanup logic
   - Check if first startup → skip
   - Check replication role from metadata
   - If pod was replica → clean WAL files only

**Key Design Decision:** Single initContainer with priority-based logic
- Simpler deployment (no new containers)
- Existing WAL cleanup logic preserved
- Hard reset takes precedence (checked first)
- Clean exit after hard reset (doesn't continue to WAL cleanup)

### 3. Controller: Divergence Detection & Hard Reset Trigger

**Detection Point:** `controller_reconcile.go` - Replica registration failure handling

**Current Code (controller_reconcile.go:348-355):**
```go
if err != nil {
    if strings.Contains(err.Error(), "Error: 3") {
        logger.Error("☢ Failed to register replication - diverged data",
            "replica_name", replicaName,
            "address", address,
            "sync_mode", mode)
        // TODO: Implement auto-recovery
        continue
    }
}
```

**Enhanced Code with Hard Reset:**
```go
if err != nil {
    if strings.Contains(err.Error(), "Error: 3") {
        logger.Error("☢ Failed to register replication - diverged data",
            "replica_name", replicaName,
            "address", address,
            "sync_mode", mode)

        // Auto-recovery: Trigger hard reset for ASYNC replicas only
        if mode == "ASYNC" {
            podName := replicaNameToPodName(replicaName)
            logger.Warn("🔄 Initiating hard reset for diverged async replica",
                "pod_name", podName,
                "replica_name", replicaName)

            if err := c.triggerHardReset(ctx, podName, replicaName, targetMainPod.GetName()); err != nil {
                logger.Error("Failed to trigger hard reset",
                    "pod_name", podName,
                    "error", err)
            }
        } else {
            // SYNC replica divergence is critical - requires manual intervention
            logger.Error("⚠️ SYNC replica diverged - MANUAL INTERVENTION REQUIRED",
                "replica_name", replicaName)
        }

        continue
    }
}
```

### 4. Controller: Hard Reset Implementation

**New Method in `controller_core.go`:**

```go
// triggerHardReset initiates a hard reset for a diverged async replica pod
// This function:
// 1. Creates a magic marker file in the pod's PVC
// 2. Deletes the pod to trigger recreation
// 3. Relies on initContainer to detect marker and clean data
func (c *Controller) triggerHardReset(ctx context.Context, podName, replicaName, mainPodName string) error {
    logger := c.logger.With(
        "pod_name", podName,
        "replica_name", replicaName,
        "main_pod", mainPodName,
    )

    logger.Info("🔥 Starting hard reset procedure for diverged replica")

    // Step 1: Create magic marker file in pod's PVC
    markerData := map[string]interface{}{
        "timestamp":     time.Now().UTC().Format(time.RFC3339),
        "reason":        "diverged_data",
        "requested_by":  "memgraph-controller",
        "original_main": mainPodName,
        "replica_name":  replicaName,
        "error_code":    3,
    }

    markerJSON, err := json.MarshalIndent(markerData, "", "  ")
    if err != nil {
        return fmt.Errorf("failed to marshal marker data: %w", err)
    }

    // Execute kubectl exec to write marker file
    cmd := fmt.Sprintf(
        "cat > /var/lib/memgraph/.hard-reset-requested <<'EOF'\n%s\nEOF",
        string(markerJSON),
    )

    execCmd := exec.CommandContext(ctx, "kubectl", "exec",
        "-n", c.namespace,
        "-c", "memgraph",
        podName,
        "--",
        "sh", "-c", cmd,
    )

    if output, err := execCmd.CombinedOutput(); err != nil {
        logger.Error("Failed to create marker file",
            "error", err,
            "output", string(output))
        return fmt.Errorf("failed to create marker file: %w", err)
    }

    logger.Info("✅ Magic marker file created successfully")

    // Step 2: Delete the pod (StatefulSet will recreate it)
    logger.Info("🗑️  Deleting pod to trigger hard reset")

    err = c.kubeClient.CoreV1().Pods(c.namespace).Delete(ctx, podName, metav1.DeleteOptions{
        GracePeriodSeconds: ptr.To(int64(30)), // Allow 30s for graceful shutdown
    })

    if err != nil {
        logger.Error("Failed to delete pod",
            "error", err)
        return fmt.Errorf("failed to delete pod: %w", err)
    }

    logger.Info("✅ Hard reset initiated - pod will be recreated with clean data",
        "expected_behavior", "initContainer will detect marker and clean all data")

    return nil
}

// Helper function to convert replica name to pod name
// Example: "memgraph_ha_2" -> "memgraph-ha-2"
func replicaNameToPodName(replicaName string) string {
    return strings.ReplaceAll(replicaName, "_", "-")
}
```

## Operational Flow

### Normal Startup (No Divergence)

```
1. Pod starts
2. InitContainer "data-cleanup" runs
3. Check /var/lib/memgraph/.hard-reset-requested
4. File NOT found → Proceed to normal WAL cleanup logic
5. If replica role detected → Clean WAL files only
6. Memgraph container starts normally
7. Controller registers replica (success)
```

### Hard Reset Flow (Divergence Detected)

```
1. Controller detects "Error: 3 - diverged data" during replica registration
2. Controller checks if replica is ASYNC (safe to reset)
3. Controller creates magic marker file in pod's PVC via kubectl exec
4. Controller deletes the diverged pod (graceful deletion, 30s timeout)
5. Kubernetes StatefulSet recreates the pod automatically
6. InitContainer "data-cleanup" runs on new pod
7. Detects magic marker file → Full data cleanup (rm -rf /var/lib/memgraph/mg_data/*)
8. Removes marker file
9. Exits without proceeding to WAL cleanup (data already cleaned)
10. Memgraph container starts with clean slate
11. Controller reconciliation registers clean replica to current main
12. ✅ Replica operational with fresh data from current main
```

## Safety Guarantees

### 1. ASYNC Replicas Only

**Rule:** Hard reset is ONLY applied to ASYNC replicas.

**Rationale:**
- ASYNC replicas are not part of the critical consistency path
- Data loss is acceptable (will be re-replicated from main)
- SYNC replica divergence requires manual investigation (critical issue)

**Implementation:**
```go
if mode == "ASYNC" {
    // Safe to auto-recover
    c.triggerHardReset(...)
} else {
    // SYNC replica - manual intervention required
    logger.Error("⚠️ SYNC replica diverged - MANUAL INTERVENTION REQUIRED")
}
```

### 2. Graceful Pod Deletion

**Grace Period:** 30 seconds (allows PreStop hook to complete)

**Purpose:**
- PreStop hook clears gateway upstreams
- Prevents writes reaching terminating pod
- Ensures clean cluster state before reset

### 3. Marker File Integrity

**Location:** Inside PVC (survives pod deletion)

**Format:** JSON with metadata (audit trail)

**Cleanup:** Removed by initContainer after successful cleanup

**Idempotency:** If initContainer fails, next pod restart retries cleanup

### 4. No Data Loss Risk

**Protected:** Main pod data is never affected

**Protected:** SYNC replica never auto-reset

**Acceptable:** ASYNC replica data loss (will be re-replicated)

## Error Handling

### Scenario 1: Marker File Creation Fails

**Behavior:** Hard reset aborted, divergence remains

**Recovery:** Controller retries on next reconciliation cycle

**Logging:** Error logged with kubectl exec output

### Scenario 2: Pod Deletion Fails

**Behavior:** Hard reset incomplete, marker file remains

**Recovery:** Next pod restart will still trigger cleanup

**Logging:** Error logged with Kubernetes API error

### Scenario 3: InitContainer Cleanup Fails

**Behavior:** Marker file persists, next restart retries

**Recovery:** Idempotent cleanup (rm -rf allows missing files)

**Logging:** InitContainer logs visible in kubectl logs --previous

### Scenario 4: Multiple Replicas Diverged

**Behavior:** Each replica gets independent hard reset

**Recovery:** Controller processes each diverged replica separately

**Logging:** Separate log entries for each replica

## Implementation Plan

### Stage 1: Enhance Existing InitContainer in Helm Chart
**Goal:** Enhance existing `data-cleanup` initContainer to handle hard reset

**Files:**
- `charts/memgraph-ha/values.yaml` - Update initContainer args

**Success Criteria:**
- Hard reset check added as Priority 1 (before WAL cleanup)
- Logs "No hard reset requested" on normal startup
- Existing WAL cleanup logic preserved and still works
- No impact on existing pod startup behavior

**Tests:**
- Deploy cluster and verify initContainer runs
- Check pod logs show "No hard reset requested" message
- Verify existing WAL cleanup still works on replica restarts

### Stage 2: Implement Controller Hard Reset Trigger
**Goal:** Add `triggerHardReset()` method to controller

**Files:**
- `internal/controller/controller_core.go` (new method)
- `internal/controller/controller_reconcile.go` (call site)

**Success Criteria:**
- Method creates marker file via kubectl exec
- Method deletes pod gracefully
- Method only triggers for ASYNC replicas
- Comprehensive logging at each step

**Tests:**
- Unit test for `triggerHardReset()` method
- Mock kubectl exec and pod delete calls
- Verify ASYNC-only logic

### Stage 3: Integrate with Reconciliation Loop
**Goal:** Auto-trigger hard reset on divergence detection

**Files:**
- `internal/controller/controller_reconcile.go:348-355`

**Success Criteria:**
- "Error: 3" detection triggers hard reset for ASYNC
- SYNC replica divergence logged but not auto-reset
- Hard reset only on registration failure, not other errors

**Tests:**
- E2E test: Manually trigger divergence, verify auto-recovery
- Verify SYNC replica does NOT trigger auto-reset

### Stage 4: E2E Testing and Validation
**Goal:** Verify hard reset works in real cluster

**Test Scenarios:**
1. **Happy Path:** Trigger divergence on async replica, verify auto-recovery
2. **Multi-Replica:** Diverge multiple async replicas, verify all recover
3. **SYNC Protection:** Manually diverge sync replica, verify no auto-reset
4. **Marker Persistence:** Delete pod before initContainer runs, verify cleanup on next start

**Success Criteria:**
- All diverged ASYNC replicas automatically recover
- No manual intervention required
- SYNC replicas protected from auto-reset
- Logs provide clear audit trail

## Monitoring and Observability

### Log Messages

**Controller Logs:**
```
🔄 Initiating hard reset for diverged async replica pod_name=memgraph-ha-2
✅ Magic marker file created successfully
🗑️  Deleting pod to trigger hard reset
✅ Hard reset initiated - pod will be recreated with clean data
```

**InitContainer Logs:**
```
🔥 HARD RESET REQUESTED - Magic marker file detected
🗑️  Performing full data cleanup...
✅ Hard reset complete - pod will start with clean slate
```

### Metrics (Future Enhancement)

**Counters:**
- `memgraph_controller_hard_reset_triggered_total{pod="memgraph-ha-2"}`
- `memgraph_controller_hard_reset_success_total{pod="memgraph-ha-2"}`
- `memgraph_controller_hard_reset_failure_total{pod="memgraph-ha-2"}`

**Gauges:**
- `memgraph_controller_diverged_replicas{count=0}`

## Alternatives Considered

### Alternative 1: Controller Direct Data Cleanup

**Approach:** Controller executes cleanup commands directly via kubectl exec

**Pros:**
- No initContainer needed
- Faster cleanup (no pod restart)

**Cons:**
- Requires stopping Memgraph process first (complex)
- Race conditions between cleanup and Memgraph restart
- Cannot ensure cleanup completes before Memgraph starts

**Decision:** Rejected - initContainer provides atomic cleanup guarantee

### Alternative 2: Drop Replica + Manual Cleanup

**Approach:** Controller only runs DROP REPLICA, human cleans data

**Pros:**
- Simple controller logic
- No risk of automated data deletion

**Cons:**
- Requires manual intervention (operational burden)
- Slow recovery (human in the loop)
- Not truly automated

**Decision:** Rejected - defeats purpose of auto-recovery

### Alternative 3: Memgraph Built-in Reset

**Approach:** Use Memgraph's built-in data cleanup features

**Pros:**
- Native Memgraph behavior
- No custom scripts needed

**Cons:**
- Memgraph has no "reset to clean state" command
- Would require Memgraph feature enhancement
- Not available in current versions

**Decision:** Rejected - not available in Memgraph

## Related Issues

**Issue #7 (KNOWN_ISSUES.md):** Data Divergence During Rolling Restart with Multiple Failovers
- This design implements "Option 1: Controller auto-recovery for diverged replicas"

**Issue #2 (KNOWN_ISSUES.md):** Rolling Restart Data Divergence
- Hard reset provides recovery mechanism for rare divergence cases

**Design Documents:**
- `design/reconciliation.md` - Reconcile Actions section updated
- `design/failover.md` - Recovery after failover section

## Production Readiness Checklist

- [ ] InitContainer added to Helm chart
- [ ] Controller method `triggerHardReset()` implemented
- [ ] Integration with reconciliation loop complete
- [ ] Unit tests for controller logic
- [ ] E2E tests for hard reset flow
- [ ] Documentation updated (KNOWN_ISSUES.md, README.md)
- [ ] Logging comprehensive and actionable
- [ ] SYNC replica protection verified
- [ ] 10+ consecutive E2E tests pass with auto-recovery
- [ ] Runbook for operators (monitoring hard reset events)

## Summary

This design provides a **safe, automated recovery mechanism** for diverged async replicas:

✅ **Zero manual intervention** - Controller detects and recovers automatically
✅ **Safe for production** - ASYNC replicas only, SYNC protected
✅ **Idempotent** - Can retry cleanup if initial attempt fails
✅ **Observable** - Comprehensive logging and audit trail
✅ **Non-invasive** - Uses existing Kubernetes patterns (initContainer)
✅ **Solves Issue #7** - Eliminates operational burden of manual recovery
