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
**Status**: Not Started

### Tasks:
- Implement main reconciliation loop
- Add event-driven pod watching
- Implement exponential backoff for failures
- Add comprehensive logging and metrics
- Implement graceful shutdown handling

## Stage 7: Configuration & Deployment
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
**Status**: Not Started

### Tasks:
- Create Kubernetes manifests (Deployment, ServiceAccount, RBAC)
- Add Dockerfile for container image
- Create comprehensive configuration documentation
- Add health check endpoints
- Implement proper signal handling

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