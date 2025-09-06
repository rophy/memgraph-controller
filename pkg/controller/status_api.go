package controller

import (
	"context"
	"time"

	v1 "k8s.io/api/core/v1"
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
	ReplicaRegistrations  []ReplicaRegistration `json:"replica_registrations"`
}

// PodStatus represents the status of a single pod for API response
type PodStatus struct {
	Name               string               `json:"name"`
	State              string               `json:"state"`
	MemgraphRole       string               `json:"memgraph_role"`
	IPAddress          string               `json:"ip_address"`
	Timestamp          time.Time            `json:"timestamp"`
	Healthy            bool                 `json:"healthy"`
	ReplicasRegistered []string             `json:"replicas_registered"`
	Inconsistency      *StatusInconsistency `json:"inconsistency"`
}

// StatusInconsistency represents pod state inconsistency for API response
type StatusInconsistency struct {
	Description  string `json:"description"`
	MemgraphRole string `json:"memgraph_role"`
}

// ReplicaRegistration represents a replica registration from the main node
type ReplicaRegistration struct {
	Name      string `json:"name"`       // Replica name (e.g., "memgraph_ha_0")
	PodName   string `json:"pod_name"`   // Pod name (e.g., "memgraph-ha-0")
	IPAddress string `json:"ip_address"` // IP address
	SyncMode  string `json:"sync_mode"`  // "SYNC" or "ASYNC"
	IsHealthy bool   `json:"is_healthy"` // Whether replica is healthy based on data_info
}

// convertMemgraphNodeToStatus converts internal MemgraphNode to API PodStatus
func convertMemgraphNodeToStatus(node *MemgraphNode, healthy bool, _ *v1.Pod) PodStatus {
	var inconsistency *StatusInconsistency

	// Check for state inconsistencies (TODO: implement state inconsistency detection)
	// if stateInc := node.DetectStateInconsistency(); stateInc != nil {
	//	inconsistency = &StatusInconsistency{
	//		Description:  stateInc.Description,
	//		MemgraphRole: stateInc.MemgraphRole,
	//	}
	// } else
	if !healthy {
		// If pod is unhealthy and no inconsistency detected, create one for unreachable pod
		memgraphRole := "unknown"
		if role, _ := node.GetReplicationRole(context.Background()); role != "" {
			memgraphRole = role
		}

		inconsistency = &StatusInconsistency{
			Description:  "Pod is unreachable - cannot query Memgraph status",
			MemgraphRole: memgraphRole,
		}
	}

	// Convert replica names to readable format (underscore back to dash)
	var replicasRegistered []string
	if replicas, err := node.GetReplicas(context.Background()); err == nil {
		replicasRegistered = make([]string, len(replicas))
		for i, replica := range replicas {
			replicasRegistered[i] = replica.Name
		}
	}

	return PodStatus{
		Name:               node.GetName(),
		State:              "unknown", // TODO: implement proper state
		MemgraphRole:       func() string { role, _ := node.GetReplicationRole(context.Background()); return role }(),
		IPAddress:          node.GetIpAddress(),
		Timestamp:          node.timestamp,
		Healthy:            healthy,
		ReplicasRegistered: replicasRegistered,
		Inconsistency:      inconsistency,
	}
}
