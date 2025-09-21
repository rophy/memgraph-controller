package controller

import (
	"context"
	"fmt"
	"strings"
	"time"

	"memgraph-controller/internal/common"

	v1 "k8s.io/api/core/v1"
)

// MemgraphNode represents a single Memgraph instance in the cluster
type MemgraphNode struct {
	name            string
	timestamp       time.Time     // Pod creation/restart time
	memgraphRole    string        // Result of SHOW REPLICATION ROLE ("MAIN", "REPLICA")
	storageInfo     *StorageInfo  // Result of querying storage info (vertex/edge count)
	ipAddress       string        // Pod IP address
	replicasInfo    []ReplicaInfo // Detailed replica information
	hasReplicasInfo bool          // True if ReplicasInfo has been populated

	// Memgraph client for database operations (shared connection pool underneath)
	client *MemgraphClient
}

// NewMemgraphNode creates a new MemgraphNode from a Kubernetes pod with required client injection
func NewMemgraphNode(pod *v1.Pod, client *MemgraphClient) *MemgraphNode {
	if client == nil {
		panic("MemgraphClient cannot be nil - all node methods require a valid client")
	}

	node := MemgraphNode{
		name:            "",
		timestamp:       time.Time{},
		memgraphRole:    "",
		ipAddress:       "",
		replicasInfo:    []ReplicaInfo{},
		hasReplicasInfo: false,
		client:          client,
	}
	node.Refresh(pod)
	return &node
}

// Refresh updates the node's basic info from the given pod
func (node *MemgraphNode) Refresh(pod *v1.Pod) {
	node.name = pod.Name

	// Update timestamp (prefer status start time, fallback to creation time)
	if pod.Status.StartTime != nil {
		node.timestamp = pod.Status.StartTime.Time
	} else {
		node.timestamp = pod.CreationTimestamp.Time
	}

	node.ipAddress = pod.Status.PodIP
}

// ClearCachedInfo clears cached memgraph info, forcing re-query on next access
func (node *MemgraphNode) ClearCachedInfo() {
	node.memgraphRole = ""
	node.replicasInfo = []ReplicaInfo{}
	node.hasReplicasInfo = false
	node.storageInfo = nil
}

// GetBoltAddress returns the Bolt connection address for the node
func (node *MemgraphNode) GetBoltAddress() (string, error) {
	if node.ipAddress == "" {
		return "", fmt.Errorf("cannot get bolt address: pod %s has no IP address", node.name)
	}
	return node.ipAddress + ":7687", nil
}

// convertPodNameForReplica converts pod name to replica name by replacing dashes with underscores
// Example: "memgraph-1" -> "memgraph_1"
func GetReplicaName(podName string) string {
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

// GetReplicaName converts pod name to replica name (dashes to underscores)
func (node *MemgraphNode) GetReplicaName() string {
	return strings.ReplaceAll(node.name, "-", "_")
}

// GetIpAddress returns the pod IP address
func (node *MemgraphNode) GetIpAddress() string {
	return node.ipAddress
}

// GetReplicationRole returns the cached replication role, querying it if not already known
func (node *MemgraphNode) GetReplicationRole(ctx context.Context, tryRefresh bool) (string, error) {
	logger := common.GetLoggerFromContext(ctx)
	if node.memgraphRole == "" || tryRefresh {
		boltAddress, err := node.GetBoltAddress()
		if err != nil {
			return node.memgraphRole, err
		}
		roleResp, err := node.client.QueryReplicationRole(ctx, boltAddress)
		if err != nil {
			return node.memgraphRole, fmt.Errorf("failed to query replication role for node %s: %w", node.name, err)
		}
		logger.Info("memgraph role", "pod_name", node.name, "role", roleResp.Role)
		node.memgraphRole = roleResp.Role
	}
	return node.memgraphRole, nil
}

// GetReplicas returns the cached list of replicas, querying it if not already known
func (node *MemgraphNode) GetReplicas(ctx context.Context, tryRefresh bool) ([]ReplicaInfo, error) {
	logger := common.GetLoggerFromContext(ctx)
	role, err := node.GetReplicationRole(ctx, tryRefresh)
	if err != nil {
		return node.replicasInfo, err
	}
	if role != "main" {
		return nil, fmt.Errorf("cannot get replicas from non-main node %s", node.name)
	}
	if !node.hasReplicasInfo || tryRefresh {
		boltAddress, err := node.GetBoltAddress()
		if err != nil {
			return node.replicasInfo, err
		}
		replicasResp, err := node.client.QueryReplicas(ctx, boltAddress)
		if err != nil {
			return node.replicasInfo, fmt.Errorf("failed to query replicas for node %s: %w", node.name, err)
		}
		logger.Info("memgraph replicas", "pod_name", node.name, "replica_count", len(replicasResp.Replicas))
		node.replicasInfo = replicasResp.Replicas
		node.hasReplicasInfo = true
	}
	return node.replicasInfo, nil
}

// Node-specific client methods that use the shared connection pool

// SetToMainRole promotes this node to MAIN role
func (node *MemgraphNode) SetToMainRole(ctx context.Context) error {
	if node.client == nil {
		return fmt.Errorf("client not injected for node %s", node.name)
	}
	boltAddress, err := node.GetBoltAddress()
	if err != nil {
		return err
	}
	err = node.client.SetReplicationRoleToMain(ctx, boltAddress)
	if err != nil {
		return err
	}
	// Clear cached role after successful change
	node.memgraphRole = ""
	node.hasReplicasInfo = false
	return nil
}

// RegisterReplica registers a replica on this MAIN node
func (node *MemgraphNode) RegisterReplica(ctx context.Context, replicaName, replicationAddress string, syncMode string) error {
	boltAddress, err := node.GetBoltAddress()
	if err != nil {
		return err
	}
	err = node.client.RegisterReplica(ctx, boltAddress, replicaName, replicationAddress, syncMode)
	if err != nil {
		return err
	}
	// Clear cached replicas info after successful registration
	node.hasReplicasInfo = false
	return nil
}

// SetToReplicaRole demotes this node to REPLICA role
func (node *MemgraphNode) SetToReplicaRole(ctx context.Context) error {
	if node.client == nil {
		return fmt.Errorf("client not injected for node %s", node.name)
	}
	boltAddress, err := node.GetBoltAddress()
	if err != nil {
		return err
	}
	err = node.client.SetReplicationRoleToReplica(ctx, boltAddress)
	if err != nil {
		return err
	}
	// Clear cached role after successful change
	node.memgraphRole = ""
	node.hasReplicasInfo = false
	return nil
}

// Ping verifies that this node is reachable
func (node *MemgraphNode) Ping(ctx context.Context) error {
	boltAddress, err := node.GetBoltAddress()
	if err != nil {
		return err
	}
	return node.client.Ping(ctx, boltAddress, 3*time.Second)
}

// InvalidateConnection closes existing connections to this node
func (node *MemgraphNode) InvalidateConnection() error {
	boltAddress, err := node.GetBoltAddress()
	if err != nil {
		return err
	}
	return node.client.connectionPool.InvalidateConnection(boltAddress)
}

// QueryStorageInfo queries the storage information (vertex/edge count) from this node
func (node *MemgraphNode) QueryStorageInfo(ctx context.Context) error {
	boltAddress, err := node.GetBoltAddress()
	if err != nil {
		return err
	}
	storageInfo, err := node.client.QueryStorageInfo(ctx, boltAddress)
	if err != nil {
		return fmt.Errorf("failed to query storage info for node %s: %w", node.name, err)
	}
	node.storageInfo = storageInfo
	common.GetLogger().Info("memgraph storage info", "pod_name", node.name, "vertices", storageInfo.VertexCount, "edges", storageInfo.EdgeCount)
	return nil
}

// DropReplica drops a replica registration on this MAIN node
func (node *MemgraphNode) DropReplica(ctx context.Context, replicaName string) error {
	if node.client == nil {
		return fmt.Errorf("client not injected for node %s", node.name)
	}
	boltAddress, err := node.GetBoltAddress()
	if err != nil {
		return err
	}
	err = node.client.DropReplica(ctx, boltAddress, replicaName)
	if err != nil {
		return err
	}
	// If replicaName is in the cached list, remove it
	for i, replica := range node.replicasInfo {
		if replica.Name == replicaName {
			node.replicasInfo = append(node.replicasInfo[:i], node.replicasInfo[i+1:]...)
			break
		}
	}
	return nil
}

// GetName returns the pod name for the gateway
func (node *MemgraphNode) GetName() string {
	return node.name
}
