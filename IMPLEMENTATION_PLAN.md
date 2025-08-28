# Implementation Plan: Expose Replica Health Status in Controller API

## Problem Statement
The controller API currently shows registered replicas but doesn't expose their actual health status from Memgraph's perspective. When a replica is scaled down or becomes unreachable, Memgraph marks it as "invalid" in the `data_info` field, but this critical information is not visible through the controller's status API.

## Current State Analysis
- Controller correctly queries `SHOW REPLICAS` and parses the `data_info` field containing replica health
- `PodInfo.ReplicasInfo` stores detailed replica information including `ParsedDataInfo` with status ("ready", "invalid", etc.)
- Status API only exposes replica names via `ReplicasRegistered []string`, missing health details
- Users cannot determine if a registered replica is actually healthy or stale

## Stage 1: Define API Data Structures
**Goal**: Create new data structures to represent replica health in the API response
**Success Criteria**: New structs defined that capture all replica health information
**Tests**: Compile successfully with new struct definitions
**Status**: Not Started

### Tasks:
1. Add `ReplicaHealthInfo` struct to `status_api.go` with fields:
   - `Name`: Replica name
   - `SocketAddress`: Connection address
   - `SyncMode`: "sync" or "async"
   - `Status`: Health status ("ready", "invalid", "parse_error", etc.)
   - `ReplicationLag`: Behind count (-1 for errors)
   - `IsHealthy`: Boolean health indicator
   - `ErrorReason`: Human-readable error description (optional)

2. Add `ReplicasHealth []ReplicaHealthInfo` field to `PodStatus` struct

## Stage 2: Implement Data Conversion Logic
**Goal**: Convert internal replica health data to API format
**Success Criteria**: `convertPodInfoToStatus` correctly populates replica health information
**Tests**: Unit tests pass for conversion logic
**Status**: Not Started

### Tasks:
1. Update `convertPodInfoToStatus()` function to:
   - Iterate through `podInfo.ReplicasInfo`
   - Convert each `ReplicaInfo` to `ReplicaHealthInfo`
   - Populate the new `ReplicasHealth` field
   - Handle nil/missing `ParsedDataInfo` gracefully

2. Ensure backward compatibility:
   - Keep existing `ReplicasRegistered` field populated
   - API consumers not using new field continue to work

## Stage 3: Add Unit Tests
**Goal**: Ensure conversion logic handles all edge cases correctly
**Success Criteria**: 100% test coverage for new conversion logic
**Tests**: `go test ./pkg/controller -run TestStatusAPIConversion`
**Status**: Not Started

### Tasks:
1. Create test cases for:
   - Healthy SYNC replica
   - Healthy ASYNC replica
   - Invalid/unreachable replica
   - Replica with parse errors
   - Missing data_info field
   - Empty replicas list

2. Test backward compatibility of existing fields

## Stage 4: Update E2E Tests
**Goal**: Verify replica health is correctly exposed during scaling operations
**Success Criteria**: E2E tests validate replica health status changes
**Tests**: `make test-e2e` passes with new assertions
**Status**: Not Started

### Tasks:
1. Add E2E test scenario:
   - Deploy 3-node cluster
   - Verify all replicas show "ready" status
   - Scale down to 2 nodes
   - Verify removed replica shows "invalid" status
   - Verify remaining replica shows "ready" status

2. Update existing E2E test helpers to check replica health

## Stage 5: Documentation and Cleanup
**Goal**: Document the new API fields and remove this implementation plan
**Success Criteria**: API documentation updated, plan file removed
**Tests**: N/A
**Status**: Not Started

### Tasks:
1. Update API documentation/comments
2. Add example API response showing replica health
3. Update README if needed
4. Remove IMPLEMENTATION_PLAN.md

## Expected API Response After Implementation

```json
{
  "pods": [
    {
      "name": "memgraph-ha-0",
      "state": "MAIN",
      "replicas_registered": ["memgraph_ha_1", "memgraph_ha_2"],
      "replicas_health": [
        {
          "name": "memgraph_ha_1",
          "socket_address": "10.244.2.140:10000",
          "sync_mode": "sync",
          "status": "ready",
          "replication_lag": 0,
          "is_healthy": true
        },
        {
          "name": "memgraph_ha_2",
          "socket_address": "10.244.2.141:10000",
          "sync_mode": "async",
          "status": "invalid",
          "replication_lag": -1,
          "is_healthy": false,
          "error_reason": "Replica is unreachable"
        }
      ]
    }
  ]
}
```

## Success Metrics
- API clearly shows which replicas are healthy vs stale
- No breaking changes to existing API consumers
- E2E tests validate replica health during failover scenarios
- Zero performance impact on API response time