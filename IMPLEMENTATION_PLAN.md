# Implementation Plan: PreStop Hook Persistent Logging

## Overview

**Objective**: Add persistent logging to PreStop hook Python script to debug rare failures that occur before requests reach the controller.

**Problem Statement**:
During 99-run reliability testing, approximately 5 FailedPreStopHook events were observed (~4% failure rate), but these failures occur **before requests reach the controller**, making them impossible to debug through controller logs alone. The Python script fails within 2-3 seconds without leaving any trace.

**Solution Design**:
Enhance the PreStop hook Python script with persistent logging to `/var/log/memgraph/prestop_{pod_id}_{pod_ip}.log`, leveraging the existing PVC-backed log storage that survives pod restarts.

**Design Compliance**:
This implementation does not directly map to existing design documents but follows the operational excellence principle of maintaining observability into critical failure paths.

---

## Stage 1: Enhanced PreStop Hook Script with Logging

**Goal**: Modify the PreStop hook Python script in `charts/memgraph-ha/values.yaml` to add comprehensive logging.

**Success Criteria**:
- Log file created at `/var/log/memgraph/prestop-{pod_id}_{pod_ip}.log`
- Each attempt logged with timestamp, attempt number
- Errors captured with full exception details
- Success/failure outcomes recorded
- Helm chart passes `helm lint`
- Helm template renders correctly

**Implementation Details**:

1. **Log Location**: `/var/log/memgraph/prestop-{pod_id}_{pod_ip}.log`
   - Already mounted from PVC `memgraph-ha-log-storage` (1Gi)
   - Persists across pod restarts
   - Accessible via kubectl exec for debugging

2. **Log Format**:
   ```
   [2025-10-19T12:34:56.789Z] START PreStop hook
   [2025-10-19T12:34:56.790Z] Attempt 1/3: Calling http://memgraph-controller:8080/prestop-hook/memgraph-ha-1
   [2025-10-19T12:34:56.800Z] Attempt 1/3: SUCCESS (status=200, duration=0.010s)
   [2025-10-19T12:34:56.801Z] END PreStop hook (exit=0)
   ```

3. **Environment Variables to Use**:
   - `POD_NAME`: Pod name (e.g., memgraph-ha-1)
   - `POD_ID`: Unique pod UID (survives pod recreation)
   - `POD_IP`: Pod IP address (for network debugging)
   - `PRESTOP_TIMEOUT_SECONDS`: Timeout value (600s)
   - `MEMGRAPH_USER`, `MEMGRAPH_PASSWORD`: Credentials

4. **Error Scenarios to Capture**:
   - Import failures (`import requests` fails)
   - DNS resolution failures (service name lookup)
   - Connection errors (network/timeout)
   - HTTP errors (non-200 responses)
   - Request timeouts (600s timeout)

**Files to Modify**:
- `charts/memgraph-ha/values.yaml` (lines 86-118, PreStop hook section)

**Testing Plan**:
- Validate Helm chart: `helm lint charts/memgraph-ha`
- Test template rendering: `helm template memgraph-ha charts/memgraph-ha -n memgraph`
- Deploy and verify log file creation
- Trigger rolling restart to generate logs
- Verify logs persist across pod recreation

**Status**: Complete

---

## Implementation Notes

**Environment Variables Available** (from charts/memgraph-ha/values.yaml):
```yaml
extraEnv:
- name: POD_NAME         # metadata.name
- name: POD_ID           # metadata.uid (unique across recreations)
- name: POD_IP           # status.podIP
```

**Current PreStop Hook** (lines 86-118):
- Location: `charts/memgraph-ha/values.yaml`
- Type: Python inline script
- Retry logic: 3 attempts with 3s sleep between attempts
- Timeout: 600 seconds (PRESTOP_TIMEOUT_SECONDS)
- Endpoint: `http://memgraph-controller:8080/prestop-hook/{pod_name}`

**Log Volume Mount**:
- PVC: `memgraph-ha-log-storage` (1Gi)
- Mount: `/var/log/memgraph/`
- Persistence: Survives pod restarts
- Access: Via kubectl exec or direct PVC mount

**Known Constraints**:
- Python script must be inline (no external file mounting currently)
- terminationGracePeriodSeconds: 1800 (30 minutes total)
- PRESTOP_TIMEOUT_SECONDS: 600 (10 minutes for PreStop hook)
- Remaining grace period: 1200s (20 minutes for actual shutdown)

---

## Timeline

- **Stage 1**: 1 hour (script modification + validation)
- **Stage 2**: 30 minutes (deployment + verification)
- **Stage 3**: 4-6 hours (99-run reliability test + analysis)
- **Stage 4**: 2-4 hours (depends on complexity of fix)

**Total Estimated Time**: 8-12 hours

---

## Rollback Plan

If issues arise:
1. Revert `charts/memgraph-ha/values.yaml` to previous version
2. Redeploy: `make down && make run`
3. Verify E2E tests pass: `make test-e2e`
4. No persistent state changes (logs are additive only)
