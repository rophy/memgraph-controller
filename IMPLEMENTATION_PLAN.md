# Read-Write Gateway Separation Implementation Plan

## Overview
Implement read-write traffic separation using two independent gateway instances managed by the controller. This design keeps the gateway simple (just a TCP proxy) while the controller orchestrates routing decisions.

## Architecture Design

### Clean Separation Approach
- **Gateway remains unchanged**: Simple TCP proxy that forwards to configured upstream
- **Controller manages TWO gateway instances**:
  - Read/Write Gateway: Listens on port 7687, routes to main pod
  - Read-Only Gateway: Listens on port 7688, routes to best available replica
- **Controller updates each gateway independently** based on cluster state

### Service Configuration
- **`memgraph-controller`**: HTTP API only (port 8080) - controller management endpoints
- **`memgraph-gateway`**: Bolt port 7687 → read/write gateway → main pod
- **`memgraph-gateway-read`**: Bolt port 7687 → read-only gateway → replica pods

### Client Experience
```python
# Read/write operations (existing behavior)
writer = GraphDatabase.driver("bolt://memgraph-gateway:7687")

# Read-only operations (new capability)
reader = GraphDatabase.driver("bolt://memgraph-gateway-read:7687")
```

## Stage 1: Controller Dual-Gateway Management
**Goal**: Update controller to manage two independent gateway instances
**Success Criteria**:
- Controller creates and manages two gateway server instances
- Each gateway has independent configuration and upstream address
- Controller updates appropriate gateway based on cluster state changes
- Existing single-gateway functionality preserved
**Tests**: Unit tests for dual gateway management
**Status**: Complete

### Tasks:
1. Modify controller to create two gateway instances:
   - Create read/write gateway on port 7687
   - Create read-only gateway on port 7688
   - Independent configuration for each gateway
2. Update controller reconciliation logic:
   - Set read/write gateway upstream to current main pod
   - Set read-only gateway upstream to best available replica
   - Handle replica health changes and failover
3. Implement replica selection logic:
   - Choose healthiest replica for read traffic
   - Round-robin or least-connections strategy
   - Fallback to main when no replicas available
4. Update gateway lifecycle management:
   - Start/stop both gateways appropriately
   - Handle gateway failures independently
   - Proper cleanup on controller shutdown

## Stage 2: Service and Deployment Configuration
**Goal**: Configure Kubernetes services and deployments for dual gateways
**Success Criteria**:
- Two services exposing appropriate gateway ports
- Services correctly route to controller pod
- Existing clients continue working unchanged
- New read-only service available for clients
**Tests**: Service connectivity and routing tests
**Status**: Complete

### Tasks:
1. Update controller deployment:
   - Expose both ports 7687 and 7688
   - Update container port definitions
   - Ensure proper resource allocation
2. Create/update Kubernetes services:
   - `memgraph-gateway`: Routes to port 7687 (read/write)
   - `memgraph-gateway-read`: Routes to port 7688 (read-only)
   - Both target the controller pod(s)
3. Update Helm charts:
   - Add service definitions
   - Parameterize port configurations
   - Add toggle for read-only service
4. Update skaffold configuration:
   - Include new service definitions
   - Update port forwarding rules

**Completed Implementation**:
- ✅ Updated deployment.yaml to expose both ports (7687, 7688) and added ENABLE_READ_GATEWAY env var
- ✅ Created three services: memgraph-controller (HTTP), memgraph-gateway (RW), memgraph-gateway-read (RO)
- ✅ Updated skaffold.yaml with read gateway enablement and port forwarding for both gateways
- ✅ Added enableReadGateway configuration to values.yaml (disabled by default)

## Stage 3: Controller API and Monitoring
**Goal**: Enhance controller API to expose gateway status and metrics
**Success Criteria**:
- API exposes status of both gateways
- Metrics differentiate between read and write traffic
- Health checks cover both gateways
- Troubleshooting information available
**Tests**: API endpoint tests, metrics validation
**Status**: Not Started

### Tasks:
1. Extend status API:
   - Add gateway status to `/api/v1/status`
   - Include upstream addresses for both gateways
   - Report connection counts per gateway
2. Add gateway-specific metrics:
   - Separate metrics for read vs write connections
   - Track replica usage patterns
   - Monitor failover events
3. Update health endpoints:
   - Check both gateways in liveness/readiness
   - Report individual gateway health
   - Graceful degradation if one gateway fails
4. Add troubleshooting endpoints:
   - Current routing decisions
   - Replica health status
   - Connection distribution

## Stage 4: E2E Testing and Validation
**Goal**: Comprehensive testing of read-write separation
**Success Criteria**:
- E2E tests validate traffic separation
- Performance benchmarks show improvement
- Failover scenarios work correctly
- No regression in existing functionality
**Tests**: E2E tests, performance tests, chaos tests
**Status**: Not Started

### Tasks:
1. Create E2E tests:
   - Verify read traffic goes to replicas
   - Verify write traffic goes to main
   - Test failover scenarios
   - Validate service discovery
2. Performance testing:
   - Measure read throughput improvement
   - Test load distribution
   - Benchmark failover time
3. Chaos testing:
   - Kill replica pods
   - Network partitions
   - Gateway failures
   - Controller restarts
4. Client compatibility:
   - Test with Python Neo4j driver
   - Verify connection pooling
   - Test transaction handling

## Implementation Notes

### Key Principles
1. **No gateway code changes** - Gateway remains a simple TCP proxy
2. **Controller orchestrates everything** - All intelligence in controller
3. **Independent gateways** - Each gateway operates independently
4. **Backwards compatible** - Existing clients work unchanged
5. **Graceful degradation** - System works even if replicas unavailable

### Configuration
- No new gateway environment variables needed
- Controller manages all routing decisions
- Services hide implementation details from clients

### Migration Path
1. Deploy updated controller with dual gateways
2. Existing clients continue using `memgraph-gateway` service
3. New clients can opt into `memgraph-gateway-read` for read traffic
4. Monitor metrics to validate improvement
5. Gradually migrate read traffic to read-only service

## Rollback Plan
- Disable second gateway via controller flag
- Read-only service can route to main as fallback
- No client changes required for rollback
- Controller reverts to single gateway mode

## Success Metrics
- Read latency reduction (target: 30-50%)
- Read throughput increase (target: 2-3x)
- Zero increase in write latency
- Failover time < 5 seconds
- Zero data inconsistency issues