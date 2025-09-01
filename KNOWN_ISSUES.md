# Known Issues

## 1. E2E Test Timeout Due to Inconsistent Reconciliation Timing

**Status**: Active  
**Severity**: High  
**First Observed**: 2025-09-01  
**Occurrence Rate**: ~30% (3/10 test runs fail)

### Description

E2E tests fail intermittently due to inconsistent timing in the controller's reconciliation loop. The failover functionality works correctly, but the time required for SYNC replica registration and topology stabilization varies significantly between runs.

### Root Cause

The controller's reconciliation logic does not follow the exact steps specified in DESIGN.md lines 75-100. Instead, it uses complex event-driven logic that creates timing inconsistencies:

1. **Missing SYNC replica registration**: After failover, the SYNC replica is not immediately registered
2. **Multiple reconciliation cycles needed**: Takes 3-9 cycles to achieve stable topology
3. **Non-deterministic step ordering**: Event-driven logic doesn't follow DESIGN.md sequence

### Reproduction Steps

1. Run E2E tests repeatedly:
   ```bash
   ./tests/scripts/repeat-e2e-tests.sh 10
   ```
2. Failure typically occurs during `TestE2E_FailoverReliability`
3. Test times out waiting for topology validation after pod deletion

### Evidence

**From test logs (test_run_3.log)**:
```
Starting post-deploy hooks...
pod/e2e-test-56xfd condition met
Error from server (NotFound): pods "e2e-test-c2l4p" not found
exit status 1
```

**From controller logs**:
- Early cycles: `sync_replicas:0` (SYNC replica missing)
- Later cycles: `sync_replicas:1` (SYNC replica finally registered)
- Health summary progression: `sync_replica: (healthy: false)` → `sync_replica: memgraph-ha-0 (healthy: true)`

**Timing variation observed**:
- Test 1: 8 retry attempts (~16s delay)
- Test 2: 1 retry attempt (~2s delay)  
- Test 3: 9 retry attempts (~18s delay)
- Tests 1,2,5: Initial timeout, then eventual success

### Impact

- **Test Reliability**: 30% E2E test failure rate
- **Development Velocity**: Failed tests block development workflow
- **Production Risk**: Indicates potential timing issues in production failover

### Analysis

The issue validates our IMPLEMENTATION_PLAN.md Stage 1 analysis:
- Current code has 1215 lines with complex event-driven logic
- DESIGN.md specifies exact 8-step reconciliation process
- Missing implementation of DESIGN.md Steps 4,6,8.2 (ASYNC replica health checks)
- Event processing conflicts with deterministic reconciliation

### Immediate Workaround

Use the automated test script to detect and investigate failures:
```bash
# Run tests until failure, then investigate logs
./tests/scripts/repeat-e2e-tests.sh 20
# Check logs/test_run_N.log for detailed failure information
```

### Proposed Solution

**Implement IMPLEMENTATION_PLAN.md Stage 1**:
1. Create `reconcile_actions.go` implementing DESIGN.md lines 75-100 exactly
2. Replace event-driven logic with deterministic 8-step process
3. Add missing ASYNC replica health monitoring (Steps 6, 8.2)

### Expected Outcome

- Consistent failover timing (~2-3s instead of 2-18s range)
- 100% E2E test reliability
- Simplified debugging through predictable reconciliation steps

### Related Components

- **Controller**: `pkg/controller/controller.go` (1215 lines - needs splitting)
- **Replication**: `pkg/controller/replication.go` - Missing exact DESIGN.md steps
- **Tests**: `tests/scripts/repeat-e2e-tests.sh` - Automated failure detection
- **Logs**: `logs/test_run_N.log` - Detailed failure analysis

### Monitoring

To track this issue:
1. Monitor E2E test success rate in CI/CD
2. Track `sync_replicas` count in controller health summaries
3. Measure time between pod deletion and topology stabilization

### Notes

- This is NOT a flaky test infrastructure issue
- The core failover functionality works correctly
- Issue is timing inconsistency in reconciliation, not functional failure
- Aligns with identified DESIGN.md compliance gaps (30% non-compliance rate)
- Automated test runner successfully reproduces and captures failures

---

## Historical Issues

### Gateway Routing Race Condition (Resolved)

**Status**: Resolved in recent commits  
**Resolution**: Gateway routing and connection handling improvements
**Note**: Replaced by the reconciliation timing issue documented above