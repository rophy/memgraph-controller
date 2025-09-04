package controller

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	v1 "k8s.io/api/core/v1"
)

// MemgraphNode represents a single Memgraph instance in the cluster
type MemgraphNode struct {
	Name          string
	Timestamp     time.Time     // Pod creation/restart time
	MemgraphRole  string        // Result of SHOW REPLICATION ROLE ("MAIN", "REPLICA")
	StorageInfo   *StorageInfo  // Result of querying storage info (vertex/edge count)
	BoltAddress   string        // Pod IP:7687 for Bolt connections
	ReplicaName   string        // Pod name with dashes → underscores for REGISTER REPLICA
	Replicas      []string      // Result of SHOW REPLICAS (only for MAIN nodes)
	ReplicasInfo  []ReplicaInfo // Detailed replica information including sync mode
	IsSyncReplica bool          // True if this replica is configured as SYNC

	// Memgraph client for database operations (shared connection pool underneath)
	client *MemgraphClient
}

// NewMemgraphNode creates a new MemgraphNode from a Kubernetes pod with required client injection
func NewMemgraphNode(pod *v1.Pod, client *MemgraphClient) *MemgraphNode {
	if client == nil {
		panic("MemgraphClient cannot be nil - all node methods require a valid client")
	}

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
		Name:          podName,
		Timestamp:     timestamp,
		MemgraphRole:  "", // Will be queried later
		BoltAddress:   boltAddress,
		ReplicaName:   replicaName,
		Replicas:      []string{},
		ReplicasInfo:  []ReplicaInfo{},
		IsSyncReplica: false,
		client:        client, // Required - all node methods depend on this
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

// UpdatePod updates the node with new pod information (IP, addresses)
// Note: IP change detection is handled by the connection pool
func (node *MemgraphNode) UpdatePod(pod *v1.Pod) {
	timestamp := pod.CreationTimestamp.Time
	if pod.Status.StartTime != nil {
		timestamp = pod.Status.StartTime.Time
	}
	node.Timestamp = timestamp

	// Update Bolt address if Pod IP is available
	if pod.Status.PodIP != "" {
		node.BoltAddress = pod.Status.PodIP + ":7687"
	} else {
		node.BoltAddress = ""
	}
}

// MarkPodDeleted marks the node as having no available pod (pod deleted)
func (node *MemgraphNode) MarkPodDeleted() {
	node.BoltAddress = ""
}

// GetReplicaName converts pod name to replica name (dashes to underscores)
func (node *MemgraphNode) GetReplicaName() string {
	return strings.ReplaceAll(node.Name, "-", "_")
}

// GetReplicationAddress returns the replication address using pod IP for reliable connectivity
func (node *MemgraphNode) GetReplicationAddress(pod *v1.Pod) string {
	if pod != nil && pod.Status.PodIP != "" {
		return pod.Status.PodIP + ":10000"
	}
	return "" // Pod IP not available
}

// IsReadyForReplication checks if pod is ready for replication (has IP and passes readiness checks)
func (node *MemgraphNode) IsReadyForReplication(pod *v1.Pod) bool {
	if pod == nil || pod.Status.PodIP == "" {
		return false
	}

	// Check Kubernetes readiness conditions
	for _, condition := range pod.Status.Conditions {
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
	return node.Name == currentMainName && node.MemgraphRole != "MAIN"
}

// ShouldBecomeReplica determines if this pod should be demoted to replica
func (node *MemgraphNode) ShouldBecomeReplica(currentMainName string) bool {
	// Pod should become replica if:
	// 1. It's NOT the selected main pod
	// 2. AND it's not already in REPLICA state
	return node.Name != currentMainName && node.MemgraphRole != "REPLICA"
}

// NeedsReplicationConfiguration determines if this pod needs replication changes
func (node *MemgraphNode) NeedsReplicationConfiguration(currentMainName string) bool {
	return node.ShouldBecomeMain(currentMainName) || node.ShouldBecomeReplica(currentMainName)
}

// Node-specific client methods that use the shared connection pool

// QueryReplicationRole queries the replication role of this node
func (node *MemgraphNode) QueryReplicationRole(ctx context.Context) error {
	roleResp, err := node.client.QueryReplicationRoleWithRetry(ctx, node.BoltAddress)
	if err != nil {
		return fmt.Errorf("failed to query replication role for node %s: %w", node.Name, err)
	}
	node.MemgraphRole = roleResp.Role
	log.Printf("Pod %s has Memgraph role: %s", node.Name, roleResp.Role)
	return nil
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
	err := node.client.SetReplicationRoleToMainWithRetry(ctx, node.BoltAddress)
	if err != nil {
		return err
	}
	// Update cached role after successful change
	node.MemgraphRole = "main"
	return nil
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
	err := node.client.SetReplicationRoleToReplicaWithRetry(ctx, node.BoltAddress)
	if err != nil {
		return err
	}
	// Update cached role after successful change
	node.MemgraphRole = "replica"
	return nil
}

// CheckConnectivity verifies that this node is reachable
func (node *MemgraphNode) CheckConnectivity(ctx context.Context) error {
	if node.client == nil {
		return fmt.Errorf("client not injected for node %s", node.Name)
	}
	return node.client.TestConnectionWithRetry(ctx, node.BoltAddress)
}

// InvalidateConnection closes existing connections to this node
func (node *MemgraphNode) InvalidateConnection() error {
	return node.client.connectionPool.InvalidateConnection(node.BoltAddress)
}

// QueryStorageInfo queries the storage information (vertex/edge count) from this node
func (node *MemgraphNode) QueryStorageInfo(ctx context.Context) error {
	storageInfo, err := node.client.QueryStorageInfoWithRetry(ctx, node.BoltAddress)
	if err != nil {
		return fmt.Errorf("failed to query storage info for node %s: %w", node.Name, err)
	}
	node.StorageInfo = storageInfo
	log.Printf("Pod %s has storage info: vertices=%d, edges=%d", node.Name, storageInfo.VertexCount, storageInfo.EdgeCount)
	return nil
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

// Gateway interface methods to satisfy gateway requirements
// GetBoltAddress returns the Bolt connection address for the gateway
func (node *MemgraphNode) GetBoltAddress() string {
	return node.BoltAddress
}

// GetName returns the pod name for the gateway  
func (node *MemgraphNode) GetName() string {
	return node.Name
}

// IsReady returns true if the Kubernetes pod is ready for connections
func (node *MemgraphNode) IsReady(pod *v1.Pod) bool {
	if pod == nil {
		return false
	}
	
	// Check pod phase
	if pod.Status.Phase != v1.PodRunning {
		return false
	}
	
	// Check readiness conditions
	for _, condition := range pod.Status.Conditions {
		if condition.Type == v1.PodReady {
			return condition.Status == v1.ConditionTrue
		}
	}
	
	return false
}

