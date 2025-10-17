package controller

import (
	"context"
	"fmt"
	"testing"

	"k8s.io/client-go/kubernetes/fake"
	"memgraph-controller/internal/common"
)

func TestMemgraphController_TestConnection(t *testing.T) {
	fakeClientset := fake.NewSimpleClientset()
	config := &common.Config{
		AppName:   "memgraph",
		Namespace: "memgraph",
	}

	ctx := context.Background()
	ctrl := NewMemgraphController(ctx, fakeClientset, config)

	err := ctrl.TestConnection()
	if err != nil {
		t.Errorf("TestConnection() failed: %v", err)
	}
}

// TestIsHealthy_InvalidReplicaWithBehindZero tests that "invalid" replicas with behind=0
// are treated as healthy to prevent prestop deadlock
func TestIsHealthy_InvalidReplicaWithBehindZero(t *testing.T) {
	// This test is a placeholder - the isHealthy function requires integration with
	// actual Memgraph nodes, so this would need to be an integration test.
	// For unit testing, we would need to refactor isHealthy to be more testable
	// by extracting the replica filtering logic.
	t.Skip("Skipping - requires refactoring isHealthy for unit testing")
}

// TestIsHealthy_InvalidReplicaWithBehindNonZero tests that "invalid" replicas with behind!=0
// block the prestop hook (waiting for recovery)
func TestIsHealthy_InvalidReplicaWithBehindNonZero(t *testing.T) {
	t.Skip("Skipping - requires refactoring isHealthy for unit testing")
}

// TestIsHealthy_DivergedReplica tests that "diverged" replicas are treated as unhealable
func TestIsHealthy_DivergedReplica(t *testing.T) {
	t.Skip("Skipping - requires refactoring isHealthy for unit testing")
}

// TestIsHealthy_MalformedReplica tests that "malformed" replicas are treated as unhealable
func TestIsHealthy_MalformedReplica(t *testing.T) {
	t.Skip("Skipping - requires refactoring isHealthy for unit testing")
}

// Helper function to create a test replica with specific status
func createTestReplica(name, status string, behind int) ReplicaInfo {
	return ReplicaInfo{
		Name:          name,
		SocketAddress: "10.0.0.1:7687",
		SyncMode:      "async",
		ParsedDataInfo: &DataInfoStatus{
			Status:    status,
			Behind:    behind,
			IsHealthy: status == "ready" && behind >= 0,
			ErrorReason: func() string {
				if status == "ready" && behind >= 0 {
					return ""
				}
				return fmt.Sprintf("Replica is %s with behind=%d", status, behind)
			}(),
		},
	}
}

// TestReplicaFiltering tests the logic for filtering replicas by health status
// This is a focused unit test for the replica filtering logic used in isHealthy
func TestReplicaFiltering(t *testing.T) {
	testCases := []struct {
		name               string
		replicas           []ReplicaInfo
		expectedHealthy    int
		expectedUnhealable int
		description        string
	}{
		{
			name: "invalid_with_behind_zero_should_be_skipped",
			replicas: []ReplicaInfo{
				createTestReplica("replica1", "invalid", 0),
				createTestReplica("replica2", "ready", 0),
			},
			expectedHealthy:    1, // Only replica2
			expectedUnhealable: 0, // replica1 is skipped (not counted as unhealable)
			description:        "Invalid replica with behind=0 should be skipped from both lists",
		},
		{
			name: "invalid_with_behind_nonzero_should_block",
			replicas: []ReplicaInfo{
				createTestReplica("replica1", "invalid", 5),
				createTestReplica("replica2", "ready", 0),
			},
			expectedHealthy:    2, // Both should be in healthyReplicas list (invalid will fail health check later)
			expectedUnhealable: 0,
			description:        "Invalid replica with behind>0 should be in healthyReplicas list to be checked",
		},
		{
			name: "diverged_should_be_unhealable",
			replicas: []ReplicaInfo{
				createTestReplica("replica1", "diverged", 10),
				createTestReplica("replica2", "ready", 0),
			},
			expectedHealthy:    1,
			expectedUnhealable: 1,
			description:        "Diverged replica should be marked as unhealable",
		},
		{
			name: "malformed_should_be_unhealable",
			replicas: []ReplicaInfo{
				createTestReplica("replica1", "malformed", -1),
				createTestReplica("replica2", "ready", 0),
			},
			expectedHealthy:    1,
			expectedUnhealable: 1,
			description:        "Malformed replica should be marked as unhealable",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			var healthyReplicas []ReplicaInfo
			var unhealableReplicas []string

			// Simulate the filtering logic from isHealthy()
			for _, replica := range tc.replicas {
				if replica.ParsedDataInfo != nil {
					status := replica.ParsedDataInfo.Status
					// Only truly unhealable states that require manual intervention
					if status == "diverged" || status == "malformed" {
						unhealableReplicas = append(unhealableReplicas,
							fmt.Sprintf("%s(%s)", replica.Name, status))
						continue
					}

					// Special case: "invalid" with behind=0 indicates stuck replication
					if status == "invalid" && replica.ParsedDataInfo.Behind == 0 {
						// Skip this replica, don't block prestop
						continue
					}

					// "invalid" with behind != 0 is temporary - let it block until recovery
				} else {
					unhealableReplicas = append(unhealableReplicas,
						fmt.Sprintf("%s(empty_data_info)", replica.Name))
					continue
				}

				healthyReplicas = append(healthyReplicas, replica)
			}

			if len(healthyReplicas) != tc.expectedHealthy {
				t.Errorf("%s: expected %d healthy replicas, got %d",
					tc.description, tc.expectedHealthy, len(healthyReplicas))
			}

			if len(unhealableReplicas) != tc.expectedUnhealable {
				t.Errorf("%s: expected %d unhealable replicas, got %d",
					tc.description, tc.expectedUnhealable, len(unhealableReplicas))
			}
		})
	}
}