# Memgraph Cluster Controller Implementation Plan

## Overview
A Kubernetes controller that manages Memgraph cluster replication by inspecting StatefulSet pods and configuring master/replica roles based on pod labels and timestamps.

## Stage 1: Project Foundation & Environment Setup
**Goal**: Bootstrap Go controller project with Kubernetes client dependencies
**Success Criteria**: 
- Go module initialized with required dependencies
- Basic controller structure scaffolding
- Can connect to Kubernetes API and list pods
**Tests**: 
- Unit test for Kubernetes client initialization
- Integration test connecting to cluster (if available)
**Status**: Complete

### Tasks:
- Initialize Go module (`go mod init memgraph-controller`)
- Add dependencies: `client-go`, `apimachinery`, `api`
- Create basic main.go with controller structure
- Add configuration via environment variables (APP_NAME, NAMESPACE)
- Implement Kubernetes client setup and connection testing

## Stage 2: Pod Discovery & Basic Memgraph Connectivity
**Goal**: Implement pod discovery and basic Memgraph connection capability
**Success Criteria**:
- Can discover StatefulSet pods by app label
- Can establish Bolt connections to Memgraph instances
- Can query `SHOW REPLICATION ROLE` from each pod
- Tracks pod timestamps for master selection logic
**Tests**:
- Test pod discovery with various label configurations
- Test Bolt connection establishment to pods
- Test replication role queries
- Test timestamp-based master selection
**Status**: Complete

### Tasks:
- Implement `PodState` enum (INITIAL, MASTER, REPLICA)
- Create `ClusterState` struct to track all pods and their states
- Implement pod discovery using label selectors
- Add Neo4j Go driver dependency for Bolt protocol
- Implement basic Memgraph client for role queries
- Add timestamp extraction from pod metadata
- Implement master selection logic (latest timestamp wins)

## Stage 3: Advanced Memgraph Operations & State Classification
**Goal**: Implement comprehensive Memgraph operations and pod state classification
**Success Criteria**:
- Can query `SHOW REPLICAS` to understand replica topology
- Can classify pods into INITIAL/MASTER/REPLICA states based on actual Memgraph role
- Can map Kubernetes pod labels to actual Memgraph replication state
- Implements robust error handling for unreachable instances
**Tests**:
- Test `SHOW REPLICAS` query parsing
- Test state classification logic (K8s labels vs Memgraph reality)
- Test error handling for unreachable instances
- Test state inconsistency detection
**Status**: Complete

### Tasks:
- Implement `SHOW REPLICAS` query and response parsing
- Create state classification logic:
  - INITIAL: No role label AND Memgraph role is MAIN with no replicas
  - MASTER: role=master label AND Memgraph role is MAIN
  - REPLICA: role=replica label AND Memgraph role is REPLICA
- Add state inconsistency detection (labels vs reality)
- Implement connection pooling and retry logic
- Add comprehensive error handling and logging

## Stage 4: Replication Configuration Logic
**Goal**: Implement logic to configure master/replica relationships
**Success Criteria**:
- Can promote pods to MASTER role
- Can demote pods to REPLICA role
- Can register replicas with master instance
- Handles multiple masters by selecting one
**Tests**:
- Test master promotion workflow
- Test replica demotion and registration
- Test conflict resolution (multiple masters)
- Test failure scenarios (unreachable pods)
**Status**: Complete

### Tasks:
- Implement `SET REPLICATION ROLE TO MAIN` execution
- Implement `SET REPLICATION ROLE TO REPLICA WITH PORT 10000` execution
- Implement `REGISTER REPLICA <replica-name> ASYNC TO <pod-name>.<service-name>:10000` execution
  - `<replica-name>` = pod name with dashes converted to underscores (e.g. `memgraph-1` → `memgraph_1`)
  - **ASYNC Mode**: Use asynchronous replication for better performance and availability
- Add replica deregistration (`DROP REPLICA <replica-name>`)
- Implement conflict resolution for multiple masters

### Replication Mode Configuration:
- **Mode**: ASYNC (Asynchronous replication)
- **Rationale**: 
  - Better performance - master doesn't wait for replica acknowledgment
  - Higher availability - master continues operating if replicas are temporarily unavailable
  - Suitable for most use cases where eventual consistency is acceptable
  - Reduced latency for write operations
- **Trade-offs**: Potential data loss in master failure scenarios (eventual consistency vs strong consistency)

## Stage 5: Pod Label Management
**Goal**: Update Kubernetes pod labels to reflect current replication roles
**Success Criteria**:
- Can update pod labels with `role=master` or `role=replica`
- Labels accurately reflect actual Memgraph replication state
- Handles label update failures gracefully
**Tests**:
- Test label update operations
- Test label consistency with Memgraph state
- Test partial failure scenarios
**Status**: Complete

### Tasks:
- Implement pod label update via Kubernetes API
- Add label validation and consistency checks
- Implement rollback logic for failed updates
- Add proper RBAC permissions for pod label updates

## Stage 5.5: API for Memgraph Status
**Goal**: Expose HTTP API that shows latest replication status of all controlled Memgraph pods
**Success Criteria**:
- HTTP server exposes `/api/v1/status` endpoint
- API returns real-time Memgraph replication status (not just pod labels)
- Response includes cluster state summary and per-pod details
- API handles unreachable pods gracefully
- Status reflects actual Memgraph SHOW REPLICATION ROLE and SHOW REPLICAS results
**Tests**:
- Test HTTP server startup and endpoint availability
- Test status response with healthy pods
- Test status response with unreachable pods
- Test JSON response format and schema validation
- Test concurrent API requests
**Status**: Complete

### Tasks:
- Add HTTP server setup with standard library `net/http`
- Create status response data structures
- Implement `/api/v1/status` endpoint handler
- Add cluster state aggregation logic
- Implement per-pod status collection from actual Memgraph queries
- Add error handling for unreachable pods
- Configure HTTP server port via environment variable
- Add graceful HTTP server shutdown
- Include API server in main controller startup

### API Design:
**Endpoint**: `/api/v1/status`
**Method**: GET
**Response**: JSON with cluster summary and per-pod replication status
**Data Sources**: Live queries to Memgraph instances (SHOW REPLICATION ROLE, SHOW REPLICAS)
**Error Handling**: Mark pods as unhealthy when Memgraph queries fail

## Stage 6: Controller Loop & Reconciliation
**Goal**: Implement main controller reconciliation loop
**Success Criteria**:
- Controller runs continuously and monitors cluster state
- Automatically reconciles desired vs actual state
- Handles pod additions, deletions, and restarts
- Implements proper error handling and backoff
**Tests**:
- Test full reconciliation scenarios
- Test pod lifecycle events (add/delete/restart)
- Test error recovery and backoff behavior
**Status**: Complete

### Tasks:
- Implement main reconciliation loop with work queue and multiple workers
- Add event-driven pod watching using Kubernetes informers
- Implement exponential backoff for reconciliation failures (3 retries with 2s base delay)
- Add comprehensive logging and error tracking with failure count monitoring
- Implement graceful shutdown handling with proper cleanup

### Implementation Details:
- **Worker Model**: 2 concurrent workers processing reconcile requests from work queue
- **Event-Driven**: Kubernetes informers trigger reconciliation on pod add/update/delete
- **Periodic Reconciliation**: Timer-based reconciliation using configured interval
- **Exponential Backoff**: 3 retries with exponential backoff (2s, 4s, 8s delays)
- **Failure Handling**: Tracks failure count (max 5) with recovery on successful reconciliation
- **Graceful Shutdown**: Proper cleanup of informers, workers, and HTTP server

## Stage 7: SYNC Replica Strategy Implementation
**Goal**: Implement SYNC/ASYNC replication strategy for guaranteed data consistency
**Success Criteria**:
- One SYNC replica per cluster (guaranteed consistency)
- Remaining replicas configured as ASYNC (performance)
- Master selection prioritizes SYNC replicas (zero data loss)
- Emergency ASYNC→SYNC promotion procedures
- Enhanced status API with SYNC replica tracking
**Tests**:
- Test SYNC replica selection and configuration
- Test master selection with SYNC replica priority
- Test emergency procedures for SYNC replica failures
- Test status API SYNC replica information
**Status**: Complete

### Tasks:
- ✅ Implement `RegisterReplicaWithModeAndRetry()` supporting SYNC/ASYNC modes
- ✅ Add deterministic SYNC replica selection (alphabetical)
- ✅ Update master selection logic to prioritize SYNC replicas
- ✅ Add `IsSyncReplica` tracking to PodInfo structure
- ✅ Implement emergency ASYNC→SYNC promotion procedures
- ✅ Add SYNC replica health monitoring and failure detection
- ✅ Enhance status API with SYNC replica information
- ✅ Update tests to cover SYNC replica scenarios

### Implementation Details:
- **SYNC Replica Selection**: Deterministic (first pod alphabetically)
- **Master Selection Priority**: 1) Existing MAIN, 2) SYNC replica, 3) Latest timestamp
- **Emergency Procedures**: Automatic SYNC replica failure detection with manual promotion guidance
- **Status API**: `current_sync_replica`, `sync_replica_healthy`, and `is_sync_replica` fields

## Stage 8: Remove Pod Label Dependencies
**Goal**: Eliminate pod labels as source of truth to remove consistency issues
**Success Criteria**:
- Single source of truth: Only Memgraph state via mgconsole queries
- No pod label management code
- Simplified controller logic with fewer failure points
- Faster reconciliation without label update overhead
**Tests**:
- Test pod discovery without label dependencies
- Test state classification using only Memgraph data
- Verify no label-related consistency issues
**Status**: Complete

### Tasks:
- Remove all label management code (`UpdatePodLabel`, `SyncPodLabelsWithState`, etc.)
- Remove `KubernetesRole` field from `PodInfo` structure
- Simplify `DetectStateInconsistency` to only check Memgraph state
- Remove label-based logic from pod discovery
- Update status API to remove `kubernetes_role` field
- Update tests to remove label-based expectations

### Rationale:
- **Consistency Issues**: Pod labels create dual source of truth with Memgraph state
- **Race Conditions**: Labels can diverge from actual Memgraph replication state
- **Additional Complexity**: Label syncing adds failure points and maintenance overhead
- **Not Functional**: Labels are metadata only - don't control actual replication

## Stage 9: Configuration & Deployment
**Goal**: Package controller for deployment and add configuration options
**Success Criteria**:
- Controller can be deployed as Kubernetes Deployment
- All configuration externalized via environment variables
- Proper RBAC and ServiceAccount configured
- Ready for production deployment
**Tests**:
- Test deployment in Kubernetes cluster
- Test RBAC permissions
- Test configuration variations
**Status**: Complete

### Tasks:
- ✅ Create Kubernetes manifests (Deployment, ServiceAccount, RBAC)
- ✅ Add Dockerfile for container image
- ✅ Create comprehensive configuration documentation
- ✅ Add health check endpoints
- ✅ Implement proper signal handling

### Implementation Details:
- **Kubernetes Deployment**: Complete Helm chart with security best practices
- **RBAC**: Minimal required permissions (pods, statefulsets, services, events)
- **Container Security**: Non-root user, minimal Alpine base image, static binary
- **Configuration**: All settings externalized via environment variables
- **Health Checks**: `/health` endpoint with Kubernetes probe integration
- **Graceful Shutdown**: Proper signal handling with 5-second timeout

## Architecture Design

### Core Components:

1. **MemgraphController**: Main controller struct
   - Manages overall reconciliation loop
   - Coordinates between components

2. **PodDiscovery**: Handles Kubernetes pod operations
   - Discovers StatefulSet pods by labels
   - Updates pod labels with role information
   - Watches for pod lifecycle events

3. **MemgraphClient**: Manages database connections
   - Connects to Memgraph instances via Bolt
   - Executes replication configuration commands
   - Queries current replication status

4. **ClusterState**: Maintains cluster state model
   - Tracks pod states and roles
   - Implements master selection logic
   - Handles state transitions

5. **Reconciler**: Core business logic
   - Compares desired vs actual state
   - Orchestrates replication configuration
   - Handles conflict resolution

### Data Structures:

```go
type PodState int
const (
    INITIAL PodState = iota  // No role label, Memgraph is MAIN with no replicas
    MASTER                   // role=master label, Memgraph role is MAIN
    REPLICA                  // role=replica label, Memgraph role is REPLICA
)

type ClusterState struct {
    Pods map[string]*PodInfo
    CurrentMaster string
}

type PodInfo struct {
    Name string
    State PodState           // Derived from K8s labels + Memgraph queries
    Timestamp time.Time      // Pod creation/restart time
    KubernetesRole string    // Value of "role" label (empty, "master", "replica")
    MemgraphRole string      // Result of SHOW REPLICATION ROLE ("MAIN", "REPLICA")
    BoltAddress string       // Pod IP:7687 for Bolt connections
    ReplicationAddress string // <pod-name>.<service-name>:10000 for replication
    ReplicaName string       // Pod name with dashes → underscores for REGISTER REPLICA
    Replicas []string        // Result of SHOW REPLICAS (only for MAIN nodes)
}
```

### Environment Configuration:
- `APP_NAME`: Helm chart app name for pod discovery (default: "memgraph")
- `NAMESPACE`: Kubernetes namespace to monitor (default: "memgraph")
- `RECONCILE_INTERVAL`: Controller reconciliation interval (default: "30s")
- `BOLT_PORT`: Memgraph Bolt port (default: "7687")
- `REPLICATION_PORT`: Fixed port for replica replication (fixed: "10000")
- `SERVICE_NAME`: Headless service name for replication addresses (default: "memgraph")
- `HTTP_PORT`: HTTP server port for status API (default: "8080")

## Dependencies:
- `k8s.io/client-go`: Kubernetes API client
- `k8s.io/apimachinery`: Kubernetes API types
- `github.com/neo4j/neo4j-go-driver/v5`: Bolt protocol driver
- Standard Go libraries for HTTP, logging, etc.

## Deployment Strategy:
1. Build controller as container image
2. Deploy as Kubernetes Deployment in same namespace as Memgraph
3. Configure RBAC for pod read/write permissions
4. Monitor via logs and health check endpoints