package tests

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// Test configuration constants
const (
	controllerStatusURL = "http://memgraph-controller:8080/api/v1/status"
	gatewayURL          = "bolt://memgraph-controller:7687"
	testTimeout         = 30 * time.Second
	expectedPodCount    = 3
)

// ClusterStatus represents the controller's cluster status response
type ClusterStatus struct {
	Timestamp    string       `json:"timestamp"`
	ClusterState ClusterState `json:"cluster_state"`
	Pods         []PodInfo    `json:"pods"`
}

// ClusterState represents the high-level cluster state
type ClusterState struct {
	CurrentMain           string                `json:"current_main"`
	CurrentSyncReplica    string                `json:"current_sync_replica"`
	TotalPods             int                   `json:"total_pods"`
	HealthyPods           int                   `json:"healthy_pods"`
	UnhealthyPods         int                   `json:"unhealthy_pods"`
	SyncReplicaHealthy    bool                  `json:"sync_replica_healthy"`
	IsLeader              bool                  `json:"is_leader"`
	ReplicaRegistrations  []ReplicaRegistration `json:"replica_registrations"`
}

// PodInfo represents information about a single pod
type PodInfo struct {
	Name         string `json:"name"`
	State        string `json:"state"`
	MemgraphRole string `json:"memgraph_role"`
	IPAddress    string `json:"ip_address"`
	Timestamp    string `json:"timestamp"`
	Healthy      bool   `json:"healthy"`
}

// ReplicaRegistration represents a replica registration from the main node
type ReplicaRegistration struct {
	Name      string `json:"name"`       // Replica name (e.g., "memgraph_ha_0")
	PodName   string `json:"pod_name"`   // Pod name (e.g., "memgraph-ha-0")
	IPAddress string `json:"ip_address"` // IP address
	SyncMode  string `json:"sync_mode"`  // "SYNC" or "ASYNC"
	IsHealthy bool   `json:"is_healthy"` // Whether replica is healthy based on data_info
}

// GatewayInfo represents gateway status information
type GatewayInfo struct {
	Enabled           bool   `json:"enabled"`
	CurrentMain       string `json:"currentMain"`
	ActiveConnections int    `json:"activeConnections"`
	TotalConnections  int64  `json:"totalConnections"`
}

// waitForController waits for the controller to be ready
func waitForController(ctx context.Context) error {
	client := &http.Client{Timeout: 5 * time.Second}

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		resp, err := client.Get(controllerStatusURL)
		if err == nil && resp.StatusCode == http.StatusOK {
			resp.Body.Close()
			return nil
		}
		if resp != nil {
			resp.Body.Close()
		}

		time.Sleep(2 * time.Second)
	}
}

// getClusterStatus retrieves the cluster status from the controller API
func getClusterStatus(ctx context.Context) (*ClusterStatus, error) {
	client := &http.Client{Timeout: 10 * time.Second}

	req, err := http.NewRequestWithContext(ctx, "GET", controllerStatusURL, nil)
	if err != nil {
		return nil, fmt.Errorf("creating request: %w", err)
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("making request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("status %d: %s", resp.StatusCode, string(body))
	}

	var status ClusterStatus
	if err := json.NewDecoder(resp.Body).Decode(&status); err != nil {
		return nil, fmt.Errorf("decoding response: %w", err)
	}

	return &status, nil
}

// isReplicaSyncMode checks if a pod is registered as a SYNC replica
func isReplicaSyncMode(podName string, replicaRegistrations []ReplicaRegistration) bool {
	for _, replica := range replicaRegistrations {
		if replica.PodName == podName && replica.SyncMode == "SYNC" {
			return true
		}
	}
	return false
}

// isReplicaRegistered checks if a pod is registered as any type of replica
func isReplicaRegistered(podName string, replicaRegistrations []ReplicaRegistration) bool {
	for _, replica := range replicaRegistrations {
		if replica.PodName == podName {
			return true
		}
	}
	return false
}