package controller

import (
	"context"

	v1 "k8s.io/api/core/v1"
	"memgraph-controller/internal/httpapi"
)






// convertMemgraphNodeToStatus converts internal MemgraphNode to API PodStatus
func convertMemgraphNodeToStatus(node *MemgraphNode, healthy bool, _ *v1.Pod) httpapi.PodStatus {
	var inconsistency *httpapi.StatusInconsistency

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

		inconsistency = &httpapi.StatusInconsistency{
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

	return httpapi.PodStatus{
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
