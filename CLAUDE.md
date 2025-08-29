Notes for running up a memgraph cluster under community edition.

# IMPORTANT

1. For long running tasks such as `make run`, NEVER run as foreground command, NEVER as background command using '&'. ALWAYS run as background process using Claude run_in_background parameter.
2. After completing your tests, clean up the background processes.

# Development Workflow

## Running Unit Tests

```bash
make test
```

## Running E2E Tests

1. Run `make down` to remove skaffold resources. If kubectl context failed to connect to a kubernetes cluster, FAIL IMMEDIATELY and prompt human to fix kubectl context.
2. Run `make run` at background, which should buils and deploy a memgraph-ha cluster with skaffold.
3. Wait for the memgraph-ha cluster to stablize. Can check memgraph-controller pod logs to assist.
4. Run `make test-e2e`, which run the e2e tests in tests/ folder.

## Standard Development Process

For a new feature, our development workflow follows this pattern:

1. Run unit tests. If not all tests passed, pause and confirm with huamn whether claude should fix unit tests first.
2. Implement new feature.
4. Run staticcheck and make sure no errors.
3. Update unit tests,which should be in same folder as source code. Do NOT add unit tests to tests/ folder which is for e2e tests.
4. Run unit tests, make sure all tests pass.
5. Run e2e tests.


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

# Check registered replicas from main pod
kubectl exec <main-pod-name> -- bash -c 'echo "SHOW REPLICAS;" | mgconsole --output-format csv --username=memgraph'

# Check storage info
kubectl exec <pod-name> -- bash -c 'echo "SHOW STORAGE INFO;" | mgconsole --output-format csv --username=memgraph'
```

**Do NOT rely on the memgraph-controller status API for debugging** - always verify the actual Memgraph state directly using the above commands.

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
kubectl exec <main-pod> -- bash -c 'echo "DROP REPLICA <failed_sync_replica_name>;" | mgconsole --output-format csv --username=memgraph'

# Step 2: Promote healthy ASYNC replica to SYNC
kubectl exec <main-pod> -- bash -c 'echo "DROP REPLICA <async_replica_name>;" | mgconsole --username=memgraph'
kubectl exec <main-pod> -- bash -c 'echo "REGISTER REPLICA <async_replica_name> SYNC TO \"<replica_ip>:10000\";" | mgconsole --output-format csv --username=memgraph'

# Step 3: Verify new SYNC replica
kubectl exec <main-pod> -- bash -c 'echo "SHOW REPLICAS;" | mgconsole --output-format csv --username=memgraph'
```

#### Scenario: Split-Brain Resolution

**Manual Split-Brain Resolution (if controller fails to resolve automatically):**
```bash
# Step 1: Identify all mains
kubectl exec memgraph-ha-0 -- bash -c 'echo "SHOW REPLICATION ROLE;" | mgconsole --output-format csv --username=memgraph'
kubectl exec memgraph-ha-1 -- bash -c 'echo "SHOW REPLICATION ROLE;" | mgconsole --output-format csv --username=memgraph'

# Step 2: Choose main with most recent data (check storage info)
kubectl exec <pod> -- bash -c 'echo "SHOW STORAGE INFO;" | mgconsole --output-format csv --username=memgraph'

# Step 3: Demote incorrect mains (keep lowest index pod as main by convention)
kubectl exec <incorrect-main-pod> -- bash -c 'echo "SET REPLICATION ROLE TO REPLICA WITH PORT 10000;" | mgconsole --output-format csv --username=memgraph'

# Step 4: Restart controller after manual resolution
kubectl rollout restart deployment/memgraph-controller -n memgraph
```

## Controller Design: SYNC Replica Strategy

### Overview

The controller implements a **SYNC replica strategy** for zero data loss failover:

- **1 SYNC replica**: Guaranteed to have all committed transactions (blocks main until confirmed)
- **N-1 ASYNC replicas**: May lag behind but provide read scalability  
- **MAIN failure**: Always promote the SYNC replica (guaranteed zero data loss)

### Two-Pod MAIN/SYNC Strategy

- **pod-0 and pod-1**: Eligible for main OR SYNC replica roles
- **pod-2, pod-3, ...**: ALWAYS ASYNC replicas only
- **Controller Authority**: Maintains expected topology in-memory after bootstrap

# CRITICAL CODE PATTERNS TO WATCH FOR

## State Synchronization Anti-Pattern: Bypassing Consolidated State Updates

**PROBLEM**: The codebase has a consolidated method `updateTargetMainIndex()` that properly updates both local state and ConfigMap, but most code paths bypass it with direct field assignments. This creates race conditions and state inconsistency between leader and non-leader controller pods.

**CONSOLIDATED METHOD EXISTS**: `controller.go:479 - updateTargetMainIndex()`
- ✅ Updates `c.targetMainIndex` 
- ✅ Updates `clusterState.TargetMainIndex`
- ✅ Persists to ConfigMap via `stateManager.SaveState()`
- ✅ Handles rollback on failure

**PLACES CORRECTLY USING CONSOLIDATED METHOD**:
- ✅ `topology.go:62` - Existing main discovered
- ✅ `topology.go:76` - SYNC replica promotion  
- ✅ `topology.go:243` - Validation adjustment

**PLACES BYPASSING CONSOLIDATED METHOD** (MUST FIX):
- ❌ `failover.go:213-215` - **CRITICAL: Failover promotion** 
- ❌ `failover.go:115-116` - Detection logic update
- ❌ `bootstrap.go:260-261` - Learning from OPERATIONAL_STATE
- ❌ `discovery.go:431` - Updating from existing main
- ❌ `controller.go:467` - Loading from ConfigMap (acceptable)
- ❌ `controller.go:501` - Rollback on failure (acceptable)  
- ❌ `controller.go:982` - Split-brain resolution

**SYMPTOMS OF THIS ANTI-PATTERN**:
- Gateway routing failures during failover
- "Write queries are forbidden on the replica instance" errors  
- Non-leader controller pods have stale routing information
- E2E test failures during failover scenarios
- Race conditions between leader and non-leader pods

**DETECTION COMMANDS**:
```bash
# Find all direct assignments (violations)
grep -rn "targetMainIndex\s*=" pkg/controller/ | grep -v "func.*updateTargetMainIndex"

# Find all correct usage (should be more common)
grep -rn "\.updateTargetMainIndex" pkg/controller/
```

**ENFORCEMENT RULE**: 
- NEVER assign `c.targetMainIndex` or `c.lastKnownMain` directly
- ALWAYS use `c.updateTargetMainIndex()` for state changes
- Only exceptions: initialization, rollback, and ConfigMap loading

**IMMEDIATE ACTION REQUIRED**:
- Fix `failover.go:213-215` to use `updateTargetMainIndex()` 
- Fix `failover.go:115-116` to use consolidated method
- Fix `bootstrap.go:260-261` to use consolidated method
- Fix `discovery.go:431` to use consolidated method  
- Fix `controller.go:982` to use consolidated method

This anti-pattern is the root cause of the gateway routing race condition and MUST be eliminated.

# Known Issues Documentation

**All known issues are documented in `KNOWN_ISSUES.md`**

## Protocol for Claude

1. **Before investigating new issues**: ALWAYS read `KNOWN_ISSUES.md` first to check if the problem is already documented
2. **When encountering test failures or bugs**: Check against documented known issues to avoid duplicate investigation
3. **When documenting new issues**: Add them to `KNOWN_ISSUES.md` with:
   - Clear description and reproduction steps
   - Root cause analysis
   - Evidence (logs, error messages)
   - Proposed solutions
   - Impact assessment
4. **When fixing issues**: Update the status in `KNOWN_ISSUES.md` and reference the fix commit

This ensures we maintain a comprehensive knowledge base of system behavior and avoid repeatedly investigating the same problems.
