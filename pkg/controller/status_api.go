package controller

import (
	"time"
)

// StatusResponse represents the complete API response for cluster status
type StatusResponse struct {
	Timestamp    time.Time     `json:"timestamp"`
	ClusterState ClusterStatus `json:"cluster_state"`
	Pods         []PodStatus   `json:"pods"`
}

// ClusterStatus represents high-level cluster state summary
type ClusterStatus struct {
	CurrentMain           string                `json:"current_main"`
	CurrentSyncReplica    string                `json:"current_sync_replica"`
	TotalPods             int                   `json:"total_pods"`
	HealthyPods           int                   `json:"healthy_pods"`
	UnhealthyPods         int                   `json:"unhealthy_pods"`
	SyncReplicaHealthy    bool                  `json:"sync_replica_healthy"`
	IsLeader              bool                  `json:"is_leader"`
	ReconciliationMetrics ReconciliationMetrics `json:"reconciliation_metrics"`
}

// PodStatus represents the status of a single pod for API response
type PodStatus struct {
	Name               string               `json:"name"`
	State              string               `json:"state"`
	MemgraphRole       string               `json:"memgraph_role"`
	BoltAddress        string               `json:"bolt_address"`
	ReplicationAddress string               `json:"replication_address"`
	Timestamp          time.Time            `json:"timestamp"`
	Healthy            bool                 `json:"healthy"`
	IsSyncReplica      bool                 `json:"is_sync_replica"`
	ReplicasRegistered []string             `json:"replicas_registered"`
	Inconsistency      *StatusInconsistency `json:"inconsistency"`
}

// StatusInconsistency represents pod state inconsistency for API response
type StatusInconsistency struct {
	Description  string `json:"description"`
	MemgraphRole string `json:"memgraph_role"`
}

// convertMemgraphNodeToStatus converts internal MemgraphNode to API PodStatus
func convertMemgraphNodeToStatus(node *MemgraphNode, healthy bool) PodStatus {
	var inconsistency *StatusInconsistency

	// Check for state inconsistencies
	if stateInc := node.DetectStateInconsistency(); stateInc != nil {
		inconsistency = &StatusInconsistency{
			Description:  stateInc.Description,
			MemgraphRole: stateInc.MemgraphRole,
		}
	} else if !healthy {
		// If pod is unhealthy and no inconsistency detected, create one for unreachable pod
		memgraphRole := "unknown"
		if node.MemgraphRole != "" {
			memgraphRole = node.MemgraphRole
		}

		inconsistency = &StatusInconsistency{
			Description:  "Pod is unreachable - cannot query Memgraph status",
			MemgraphRole: memgraphRole,
		}
	}

	// Convert replica names to readable format (underscore back to dash)
	replicasRegistered := make([]string, len(node.Replicas))
	copy(replicasRegistered, node.Replicas)

	return PodStatus{
		Name:               node.Name,
		State:              node.State.String(),
		MemgraphRole:       node.MemgraphRole,
		BoltAddress:        node.BoltAddress,
		ReplicationAddress: node.GetReplicationAddress(),
		Timestamp:          node.Timestamp,
		Healthy:            healthy,
		IsSyncReplica:      node.IsSyncReplica,
		ReplicasRegistered: replicasRegistered,
		Inconsistency:      inconsistency,
	}
}
