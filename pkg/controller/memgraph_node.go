package controller

import (
	"context"
	"fmt"
	"strings"
	"time"

	v1 "k8s.io/api/core/v1"
)

// MemgraphNode represents a single Memgraph instance in the cluster
type MemgraphNode struct {
	Name               string
	State              PodState      // Derived from Memgraph queries only
	Timestamp          time.Time     // Pod creation/restart time
	MemgraphRole       string        // Result of SHOW REPLICATION ROLE ("MAIN", "REPLICA")
	BoltAddress        string        // Pod IP:7687 for Bolt connections
	ReplicaName        string        // Pod name with dashes → underscores for REGISTER REPLICA
	Replicas           []string      // Result of SHOW REPLICAS (only for MAIN nodes)
	ReplicasInfo       []ReplicaInfo // Detailed replica information including sync mode
	IsSyncReplica      bool          // True if this replica is configured as SYNC
	Pod                *v1.Pod       // Reference to Kubernetes pod object
	
	// Memgraph client for database operations (shared connection pool underneath)
	client *MemgraphClient
}

// NewMemgraphNode creates a new MemgraphNode from a Kubernetes pod with optional client injection
func NewMemgraphNode(pod *v1.Pod, client *MemgraphClient) *MemgraphNode {
	podName := pod.Name

	// Extract timestamp (prefer status start time, fallback to creation time)
	timestamp := pod.CreationTimestamp.Time
	if pod.Status.StartTime != nil {
		timestamp = pod.Status.StartTime.Time
	}

	// Build addresses
	boltAddress := ""
	if pod.Status.PodIP != "" {
		boltAddress = pod.Status.PodIP + ":7687"
	}

	// Convert pod name for replica registration (dashes to underscores)
	replicaName := convertPodNameForReplica(podName)

	return &MemgraphNode{
		Name:               podName,
		State:              INITIAL, // Will be determined later
		Timestamp:          timestamp,
		MemgraphRole:       "", // Will be queried later
		BoltAddress:        boltAddress,
		ReplicaName:        replicaName,
		Replicas:           []string{},
		ReplicasInfo:       []ReplicaInfo{},
		IsSyncReplica:      false,
		Pod:                pod,
		client:             client, // Can be nil, injected by MemgraphCluster
	}
}

// SetClient injects a MemgraphClient into this node (for dependency injection)
func (node *MemgraphNode) SetClient(client *MemgraphClient) {
	node.client = client
}

// convertPodNameForReplica converts pod name to replica name by replacing dashes with underscores
// Example: "memgraph-1" -> "memgraph_1"
func convertPodNameForReplica(podName string) string {
	result := ""
	for _, char := range podName {
		if char == '-' {
			result += "_"
		} else {
			result += string(char)
		}
	}
	return result
}

// ClassifyPodState determines the actual pod state based on Memgraph role and replica configuration
func (node *MemgraphNode) ClassifyState() PodState {
	// If we don't have Memgraph role information yet, return current state
	if node.MemgraphRole == "" {
		return node.State
	}

	// State classification based on actual Memgraph configuration:
	// - INITIAL: Memgraph role is main with no replicas (standalone)
	// - MAIN: Memgraph role is main with replicas configured
	// - REPLICA: Memgraph role is replica

	// Note: We classify based on actual Memgraph state only.

	switch {
	case node.MemgraphRole == "main" && len(node.Replicas) == 0:
		return INITIAL
	case node.MemgraphRole == "main" && len(node.Replicas) > 0:
		return MAIN
	case node.MemgraphRole == "replica":
		return REPLICA
	default:
		// Unknown Memgraph role, return current state
		return node.State
	}
}

// DetectStateInconsistency checks if pod state is inconsistent with Memgraph role
func (node *MemgraphNode) DetectStateInconsistency() *StateInconsistency {
	if node.MemgraphRole == "" {
		// Can't detect inconsistency without Memgraph role info
		return nil
	}

	expectedState := node.ClassifyState()

	// Check if current state matches expected state based on Memgraph role
	if node.State == expectedState {
		return nil // No inconsistency
	}

	return &StateInconsistency{
		PodName:       node.Name,
		MemgraphRole:  node.MemgraphRole,
		CurrentState:  node.State,
		ExpectedState: expectedState,
		ReplicaCount:  len(node.Replicas),
		Description:   buildInconsistencyDescription(node),
	}
}

// GetReplicaName converts pod name to replica name (dashes to underscores)
func (node *MemgraphNode) GetReplicaName() string {
	return strings.ReplaceAll(node.Name, "-", "_")
}

// GetReplicationAddress returns the replication address using pod IP for reliable connectivity
func (node *MemgraphNode) GetReplicationAddress() string {
	if node.Pod != nil && node.Pod.Status.PodIP != "" {
		return node.Pod.Status.PodIP + ":10000"
	}
	return "" // Pod IP not available
}

// IsReadyForReplication checks if pod is ready for replication (has IP and passes readiness checks)
func (node *MemgraphNode) IsReadyForReplication() bool {
	if node.Pod == nil || node.Pod.Status.PodIP == "" {
		return false
	}

	// Check Kubernetes readiness conditions
	for _, condition := range node.Pod.Status.Conditions {
		if condition.Type == v1.PodReady {
			return condition.Status == v1.ConditionTrue
		}
	}

	return false
}

// ShouldBecomeMain determines if this pod should be promoted to main
func (node *MemgraphNode) ShouldBecomeMain(currentMainName string) bool {
	// Pod should become main if:
	// 1. It's currently selected as the main pod (by timestamp)
	// 2. AND it's not already in MAIN state
	return node.Name == currentMainName && node.State != MAIN
}

// ShouldBecomeReplica determines if this pod should be demoted to replica
func (node *MemgraphNode) ShouldBecomeReplica(currentMainName string) bool {
	// Pod should become replica if:
	// 1. It's NOT the selected main pod
	// 2. AND it's not already in REPLICA state
	return node.Name != currentMainName && node.State != REPLICA
}

// NeedsReplicationConfiguration determines if this pod needs replication changes
func (node *MemgraphNode) NeedsReplicationConfiguration(currentMainName string) bool {
	return node.ShouldBecomeMain(currentMainName) || node.ShouldBecomeReplica(currentMainName)
}

// buildInconsistencyDescription builds a description of state inconsistency
func buildInconsistencyDescription(node *MemgraphNode) string {
	expectedState := node.ClassifyState()
	return fmt.Sprintf("Pod state %s does not match expected state %s (Memgraph role: %s, replicas: %d)",
		node.State.String(), expectedState.String(), node.MemgraphRole, len(node.Replicas))
}

// Node-specific client methods that use the shared connection pool

// QueryReplicationRole queries the replication role of this node
func (node *MemgraphNode) QueryReplicationRole(ctx context.Context) (*ReplicationRole, error) {
	if node.client == nil {
		return nil, fmt.Errorf("client not injected for node %s", node.Name)
	}
	return node.client.QueryReplicationRoleWithRetry(ctx, node.BoltAddress)
}

// QueryReplicas queries the registered replicas from this node (only for MAIN nodes)
func (node *MemgraphNode) QueryReplicas(ctx context.Context) (*ReplicasResponse, error) {
	if node.client == nil {
		return nil, fmt.Errorf("client not injected for node %s", node.Name)
	}
	return node.client.QueryReplicasWithRetry(ctx, node.BoltAddress)
}

// SetToMainRole promotes this node to MAIN role
func (node *MemgraphNode) SetToMainRole(ctx context.Context) error {
	if node.client == nil {
		return fmt.Errorf("client not injected for node %s", node.Name)
	}
	return node.client.SetReplicationRoleToMainWithRetry(ctx, node.BoltAddress)
}

// RegisterReplica registers a replica on this MAIN node
func (node *MemgraphNode) RegisterReplica(ctx context.Context, replicaName, replicationAddress string, syncMode string) error {
	if node.client == nil {
		return fmt.Errorf("client not injected for node %s", node.Name)
	}
	return node.client.RegisterReplicaWithModeAndRetry(ctx, node.BoltAddress, replicaName, replicationAddress, syncMode)
}

// SetToReplicaRole demotes this node to REPLICA role
func (node *MemgraphNode) SetToReplicaRole(ctx context.Context) error {
	if node.client == nil {
		return fmt.Errorf("client not injected for node %s", node.Name)
	}
	return node.client.SetReplicationRoleToReplicaWithRetry(ctx, node.BoltAddress)
}

// CheckConnectivity verifies that this node is reachable
func (node *MemgraphNode) CheckConnectivity(ctx context.Context) error {
	if node.client == nil {
		return fmt.Errorf("client not injected for node %s", node.Name)
	}
	return node.client.TestConnectionWithRetry(ctx, node.BoltAddress)
}

// QueryStorageInfo queries the storage information (vertex/edge count) from this node
func (node *MemgraphNode) QueryStorageInfo(ctx context.Context) (*StorageInfo, error) {
	if node.client == nil {
		return nil, fmt.Errorf("client not injected for node %s", node.Name)
	}
	return node.client.QueryStorageInfoWithRetry(ctx, node.BoltAddress)
}

// DropReplica drops a replica registration on this MAIN node
func (node *MemgraphNode) DropReplica(ctx context.Context, replicaName string) error {
	if node.client == nil {
		return fmt.Errorf("client not injected for node %s", node.Name)
	}
	return node.client.DropReplicaWithRetry(ctx, node.BoltAddress, replicaName)
}

// RegisterReplicaWithMode registers a replica with specified sync mode on this MAIN node
func (node *MemgraphNode) RegisterReplicaWithMode(ctx context.Context, replicaName, replicationAddress, syncMode string) error {
	if node.client == nil {
		return fmt.Errorf("client not injected for node %s", node.Name)
	}
	return node.client.RegisterReplicaWithModeAndRetry(ctx, node.BoltAddress, replicaName, replicationAddress, syncMode)
}