package controller

import (
	"context"
	"testing"
)

func TestMemgraphController_EnhancedMainSelection(t *testing.T) {
	tests := []struct {
		name                    string
		targetMainIndex         int
		pods                    map[string]*PodInfo
		expectedMain            string
		expectedSelectionReason string
		shouldLogPromotion      bool
	}{
		{
			name:            "target_main_healthy_should_select",
			targetMainIndex: 0,
			pods: map[string]*PodInfo{
				"memgraph-ha-0": {
					Name:         "memgraph-ha-0",
					BoltAddress:  "memgraph-ha-0:7687",
					MemgraphRole: "replica",
				},
				"memgraph-ha-1": {
					Name:         "memgraph-ha-1",
					BoltAddress:  "memgraph-ha-1:7687",
					MemgraphRole: "replica",
				},
			},
			expectedMain:            "memgraph-ha-0",
			expectedSelectionReason: "controller target main (index 0)",
		},
		{
			name:            "existing_main_healthy_should_keep",
			targetMainIndex: 0,
			pods: map[string]*PodInfo{
				"memgraph-ha-0": {
					Name:         "memgraph-ha-0",
					BoltAddress:  "", // Target main unhealthy - no bolt address
					MemgraphRole: "replica",
				},
				"memgraph-ha-1": {
					Name:         "memgraph-ha-1",
					BoltAddress:  "memgraph-ha-1:7687",
					MemgraphRole: "main", // Existing MAIN node
				},
			},
			expectedMain:            "memgraph-ha-1",
			expectedSelectionReason: "existing MAIN node (avoid failover)",
		},
		{
			name:            "sync_replica_promotion",
			targetMainIndex: 0,
			pods: map[string]*PodInfo{
				"memgraph-ha-0": {
					Name:         "memgraph-ha-0",
					BoltAddress:  "", // Target main unhealthy
					MemgraphRole: "replica",
				},
				"memgraph-ha-1": {
					Name:          "memgraph-ha-1",
					BoltAddress:   "memgraph-ha-1:7687",
					MemgraphRole:  "replica",
					IsSyncReplica: true, // SYNC replica available
				},
			},
			expectedMain:            "memgraph-ha-1",
			expectedSelectionReason: "SYNC replica promotion (guaranteed consistency)",
			shouldLogPromotion:      true,
		},
		{
			name:            "no_safe_promotion_available",
			targetMainIndex: 0,
			pods: map[string]*PodInfo{
				"memgraph-ha-0": {
					Name:         "memgraph-ha-0",
					BoltAddress:  "", // Target main unhealthy
					MemgraphRole: "replica",
				},
				"memgraph-ha-1": {
					Name:         "memgraph-ha-1",
					BoltAddress:  "", // All pods unhealthy
					MemgraphRole: "replica",
				},
			},
			expectedMain: "", // No main should be selected
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			controller := &MemgraphController{
				config: &Config{
					AppName:         "memgraph",
					StatefulSetName: "memgraph-ha",
					Namespace:       "test-ns",
				},
			}

			clusterState := &ClusterState{
				TargetMainIndex: tt.targetMainIndex,
				Pods:            tt.pods,
			}

			// Call the function
			controller.enhancedMainSelection(context.Background(), clusterState)

			// Verify main selection
			if clusterState.CurrentMain != tt.expectedMain {
				t.Errorf("CurrentMain = %s, want %s", clusterState.CurrentMain, tt.expectedMain)
			}

			// Additional verifications for successful selections
			if tt.expectedMain != "" {
				if clusterState.CurrentMain == "" {
					t.Errorf("Expected main %s but no main was selected", tt.expectedMain)
				}
			}
		})
	}
}

func TestMemgraphController_CountHealthyPods(t *testing.T) {
	controller := &MemgraphController{}

	clusterState := &ClusterState{
		Pods: map[string]*PodInfo{
			"healthy-1": {
				Name:         "healthy-1",
				BoltAddress:  "healthy-1:7687",
				MemgraphRole: "main",
			},
			"healthy-2": {
				Name:         "healthy-2",
				BoltAddress:  "healthy-2:7687",
				MemgraphRole: "replica",
			},
			"unhealthy-no-bolt": {
				Name:         "unhealthy-no-bolt",
				BoltAddress:  "", // No bolt address
				MemgraphRole: "replica",
			},
			"unhealthy-no-role": {
				Name:         "unhealthy-no-role",
				BoltAddress:  "unhealthy-no-role:7687",
				MemgraphRole: "", // No role
			},
		},
	}

	count := controller.countHealthyPods(clusterState)
	expected := 2 // Only healthy-1 and healthy-2 are healthy

	if count != expected {
		t.Errorf("countHealthyPods() = %d, want %d", count, expected)
	}
}

func TestMemgraphController_BuildDecisionFactors(t *testing.T) {
	controller := &MemgraphController{}

	// Test with healthy target, unhealthy existing main, and healthy sync replica
	targetMain := &PodInfo{
		Name:         "target",
		BoltAddress:  "target:7687",
		MemgraphRole: "replica",
	}
	existingMain := &PodInfo{
		Name:         "existing",
		BoltAddress:  "", // Unhealthy - no bolt address
		MemgraphRole: "main",
	}
	syncReplica := &PodInfo{
		Name:         "sync",
		BoltAddress:  "sync:7687",
		MemgraphRole: "replica",
	}

	factors := controller.buildDecisionFactors(targetMain, existingMain, syncReplica)

	expectedFactors := []string{
		"target_main_healthy",
		"existing_main_unhealthy",
		"sync_replica_healthy",
	}

	if len(factors) != len(expectedFactors) {
		t.Fatalf("Expected %d factors, got %d: %v", len(expectedFactors), len(factors), factors)
	}

	for i, expected := range expectedFactors {
		if factors[i] != expected {
			t.Errorf("Factor %d = %s, want %s", i, factors[i], expected)
		}
	}
}

func TestMemgraphController_IsPodHealthyForMain(t *testing.T) {
	controller := &MemgraphController{}

	tests := []struct {
		name     string
		podInfo  *PodInfo
		expected bool
	}{
		{
			name: "healthy_pod",
			podInfo: &PodInfo{
				Name:         "healthy",
				BoltAddress:  "healthy:7687",
				MemgraphRole: "main",
			},
			expected: true,
		},
		{
			name: "no_bolt_address",
			podInfo: &PodInfo{
				Name:         "no-bolt",
				BoltAddress:  "",
				MemgraphRole: "main",
			},
			expected: false,
		},
		{
			name: "no_role",
			podInfo: &PodInfo{
				Name:         "no-role",
				BoltAddress:  "no-role:7687",
				MemgraphRole: "",
			},
			expected: false,
		},
		{
			name:     "nil_pod",
			podInfo:  nil,
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := controller.isPodHealthyForMain(tt.podInfo)
			if result != tt.expected {
				t.Errorf("isPodHealthyForMain() = %v, want %v", result, tt.expected)
			}
		})
	}
}

func TestMemgraphController_HandleNoSafePromotion(t *testing.T) {
	controller := &MemgraphController{
		config: &Config{Namespace: "test-ns"},
	}

	clusterState := &ClusterState{
		Pods: make(map[string]*PodInfo),
	}

	// Call the function
	controller.handleNoSafePromotion(clusterState, "target-main", nil, nil)

	// Should clear current main
	if clusterState.CurrentMain != "" {
		t.Errorf("CurrentMain should be empty after handleNoSafePromotion, got %s", clusterState.CurrentMain)
	}
}

func TestMemgraphController_ValidateMainSelection(t *testing.T) {
	tests := []struct {
		name        string
		currentMain string
		pods        map[string]*PodInfo
		targetIndex int
		expectClear bool
	}{
		{
			name:        "empty_main_should_warn",
			currentMain: "",
			pods:        map[string]*PodInfo{},
			expectClear: false, // Already empty
		},
		{
			name:        "main_not_found_should_clear",
			currentMain: "missing-pod",
			pods:        map[string]*PodInfo{},
			expectClear: true,
		},
		{
			name:        "unhealthy_main_should_clear",
			currentMain: "unhealthy-pod",
			pods: map[string]*PodInfo{
				"unhealthy-pod": {
					Name:         "unhealthy-pod",
					BoltAddress:  "", // Unhealthy
					MemgraphRole: "main",
				},
			},
			expectClear: true,
		},
		{
			name:        "healthy_main_should_pass",
			currentMain: "healthy-pod",
			pods: map[string]*PodInfo{
				"healthy-pod": {
					Name:         "healthy-pod",
					BoltAddress:  "healthy-pod:7687",
					MemgraphRole: "main",
				},
			},
			targetIndex: 0,
			expectClear: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			controller := &MemgraphController{
				config: &Config{
					StatefulSetName: "memgraph-ha",
				},
			}

			clusterState := &ClusterState{
				CurrentMain:     tt.currentMain,
				Pods:            tt.pods,
				TargetMainIndex: tt.targetIndex,
			}

			controller.validateMainSelection(context.Background(), clusterState)

			if tt.expectClear && clusterState.CurrentMain != "" {
				t.Errorf("Expected CurrentMain to be cleared, but got %s", clusterState.CurrentMain)
			}
			if !tt.expectClear && tt.currentMain != "" && clusterState.CurrentMain == "" {
				t.Errorf("Expected CurrentMain to remain %s, but it was cleared", tt.currentMain)
			}
		})
	}
}

func TestMemgraphController_EnforceExpectedTopology(t *testing.T) {
	tests := []struct {
		name        string
		pods        map[string]*PodInfo
		expectError bool
		expectCall  string // Which function should be conceptually called
	}{
		{
			name: "split_brain_state",
			pods: map[string]*PodInfo{
				"memgraph-ha-0": {
					Name:         "memgraph-ha-0",
					MemgraphRole: "main",
				},
				"memgraph-ha-1": {
					Name:         "memgraph-ha-1",
					MemgraphRole: "main",
				},
				"memgraph-ha-2": {
					Name:         "memgraph-ha-2",
					MemgraphRole: "replica",
				},
			},
			expectCall: "resolveSplitBrain",
		},
		{
			name: "operational_state_healthy",
			pods: map[string]*PodInfo{
				"memgraph-ha-0": {
					Name:         "memgraph-ha-0",
					MemgraphRole: "main",
				},
				"memgraph-ha-1": {
					Name:         "memgraph-ha-1",
					MemgraphRole: "replica",
				},
			},
			expectCall: "none", // Should return nil for healthy operational state
		},
		{
			name: "unknown_state_both_replicas",
			pods: map[string]*PodInfo{
				"memgraph-ha-0": {
					Name:         "memgraph-ha-0",
					MemgraphRole: "replica",
				},
				"memgraph-ha-1": {
					Name:         "memgraph-ha-1",
					MemgraphRole: "replica",
				},
			},
			expectError: false, // Operational phase doesn't check state classification per README.md
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			controller := &MemgraphController{
				config: &Config{
					StatefulSetName: "memgraph-ha",
				},
			}

			clusterState := &ClusterState{
				Pods:            tt.pods,
				TargetMainIndex: 0,
			}

			ctx := context.Background()
			err := controller.enforceExpectedTopology(ctx, clusterState)

			if tt.expectError && err == nil {
				t.Errorf("Expected error but got none")
			}
			if !tt.expectError && err != nil {
				t.Errorf("Unexpected error: %v", err)
			}
		})
	}
}

func TestMemgraphController_ResolveSplitBrain(t *testing.T) {
	controller := &MemgraphController{
		config: &Config{
			StatefulSetName: "memgraph-ha",
		},
		// Note: In real usage, this would have a proper memgraph client
		// For unit tests, we focus on the logic rather than the actual calls
	}

	clusterState := &ClusterState{
		TargetMainIndex: 0,
		Pods: map[string]*PodInfo{
			"memgraph-ha-0": {
				Name:         "memgraph-ha-0",
				BoltAddress:  "memgraph-ha-0:7687",
				MemgraphRole: "main",
			},
			"memgraph-ha-1": {
				Name:         "memgraph-ha-1",
				BoltAddress:  "memgraph-ha-1:7687",
				MemgraphRole: "main", // Should be demoted
			},
		},
	}

	ctx := context.Background()

	// This test will try to call the actual memgraph client methods
	// Since we don't have a mock, we expect an error due to nil client
	// But the logic of identifying which pods to demote should work
	err := controller.resolveSplitBrain(ctx, clusterState)

	// With nil client, the function should handle gracefully and not error
	if err != nil {
		t.Errorf("Unexpected error: %v", err)
	}
}

func TestMemgraphController_EnforceKnownTopology(t *testing.T) {
	controller := &MemgraphController{
		config: &Config{
			StatefulSetName: "memgraph-ha",
		},
	}

	clusterState := &ClusterState{
		TargetMainIndex: 1,
		CurrentMain:     "",
	}

	ctx := context.Background()
	err := controller.enforceKnownTopology(ctx, clusterState)

	if err != nil {
		t.Errorf("Unexpected error: %v", err)
	}

	expected := "memgraph-ha-1"
	if clusterState.CurrentMain != expected {
		t.Errorf("CurrentMain = %s, want %s", clusterState.CurrentMain, expected)
	}
}

func TestMemgraphController_PromoteExpectedMain(t *testing.T) {
	tests := []struct {
		name        string
		pods        map[string]*PodInfo
		expectError bool
		mockSuccess bool
	}{
		{
			name: "successful_promotion",
			pods: map[string]*PodInfo{
				"memgraph-ha-0": {
					Name:        "memgraph-ha-0",
					BoltAddress: "memgraph-ha-0:7687",
				},
			},
			expectError: false,
		},
		{
			name:        "pod_not_found",
			pods:        map[string]*PodInfo{},
			expectError: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			controller := &MemgraphController{
				config: &Config{
					StatefulSetName: "memgraph-ha",
				},
				// Note: memgraphClient is nil for these unit tests
			}

			clusterState := &ClusterState{
				TargetMainIndex: 0,
				Pods:            tt.pods,
			}

			ctx := context.Background()
			err := controller.promoteExpectedMain(ctx, clusterState)

			// With nil client handling, check based on expectError from test case
			if tt.expectError && err == nil {
				t.Errorf("Expected error but got none")
			}
			if !tt.expectError && err != nil {
				t.Errorf("Unexpected error: %v", err)
			}

			// Verify main is set on successful promotion
			if !tt.expectError && len(tt.pods) > 0 && clusterState.CurrentMain != "memgraph-ha-0" {
				t.Errorf("CurrentMain = %s, want memgraph-ha-0", clusterState.CurrentMain)
			}
		})
	}
}
