package httpapi

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
	ReconciliationMetrics ReconciliationMetrics `json:"reconciliation_metrics"`
	ReplicaRegistrations  []ReplicaRegistration `json:"replica_registrations"`
	ReadGatewayStatus     *ReadGatewayStatus    `json:"read_gateway_status,omitempty"`
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
	PodName   string `json:"pod_name"`   // Kubernetes pod name (e.g., "memgraph-ha-0")
	Address   string `json:"address"`    // Socket address (e.g., "10.244.0.4:10000")
	SyncMode  string `json:"sync_mode"`  // "sync" or "async"
	IsHealthy bool   `json:"is_healthy"` // Overall replication health
}

// ReadGatewayStatus represents the status of the read gateway
type ReadGatewayStatus struct {
	Enabled           bool   `json:"enabled"`
	CurrentUpstream   string `json:"current_upstream"`
	UpstreamPodName   string `json:"upstream_pod_name,omitempty"`
	UpstreamHealthy   bool   `json:"upstream_healthy"`
	ConnectionStats   GatewayConnectionStats `json:"connection_stats"`
	HealthCheckStats  HealthCheckStats `json:"health_check_stats"`
}

// GatewayConnectionStats represents connection statistics for a gateway
type GatewayConnectionStats struct {
	ActiveConnections   int64 `json:"active_connections"`
	TotalConnections    int64 `json:"total_connections"`
	RejectedConnections int64 `json:"rejected_connections"`
	Errors              int64 `json:"errors"`
}

// HealthCheckStats represents health check statistics
type HealthCheckStats struct {
	ConsecutiveFailures int  `json:"consecutive_failures"`
	LastHealthStatus    bool `json:"last_health_status"`
}
