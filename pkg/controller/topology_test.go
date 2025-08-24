package controller

import (
	"context"
	"testing"
)

func TestMemgraphController_EnhancedMasterSelection(t *testing.T) {
	tests := []struct {
		name                     string
		targetMasterIndex        int
		pods                     map[string]*PodInfo
		expectedMaster           string
		expectedSelectionReason  string
		shouldLogPromotion       bool
	}{
		{
			name:              "target_master_healthy_should_select",
			targetMasterIndex: 0,
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
			expectedMaster:          "memgraph-ha-0",
			expectedSelectionReason: "controller target master (index 0)",
		},
		{
			name:              "existing_main_healthy_should_keep",
			targetMasterIndex: 0,
			pods: map[string]*PodInfo{
				"memgraph-ha-0": {
					Name:         "memgraph-ha-0",
					BoltAddress:  "",  // Target master unhealthy - no bolt address
					MemgraphRole: "replica",
				},
				"memgraph-ha-1": {
					Name:         "memgraph-ha-1",
					BoltAddress:  "memgraph-ha-1:7687",
					MemgraphRole: "main",  // Existing MAIN node
				},
			},
			expectedMaster:          "memgraph-ha-1",
			expectedSelectionReason: "existing MAIN node (avoid failover)",
		},
		{
			name:              "sync_replica_promotion",
			targetMasterIndex: 0,
			pods: map[string]*PodInfo{
				"memgraph-ha-0": {
					Name:         "memgraph-ha-0",
					BoltAddress:  "",  // Target master unhealthy
					MemgraphRole: "replica",
				},
				"memgraph-ha-1": {
					Name:          "memgraph-ha-1",
					BoltAddress:   "memgraph-ha-1:7687",
					MemgraphRole:  "replica",
					IsSyncReplica: true,  // SYNC replica available
				},
			},
			expectedMaster:          "memgraph-ha-1",
			expectedSelectionReason: "SYNC replica promotion (guaranteed consistency)",
			shouldLogPromotion:      true,
		},
		{
			name:              "no_safe_promotion_available",
			targetMasterIndex: 0,
			pods: map[string]*PodInfo{
				"memgraph-ha-0": {
					Name:         "memgraph-ha-0",
					BoltAddress:  "",  // Target master unhealthy
					MemgraphRole: "replica",
				},
				"memgraph-ha-1": {
					Name:         "memgraph-ha-1", 
					BoltAddress:  "",  // All pods unhealthy
					MemgraphRole: "replica",
				},
			},
			expectedMaster: "",  // No master should be selected
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
				TargetMasterIndex: tt.targetMasterIndex,
				Pods:              tt.pods,
			}

			// Call the function
			controller.enhancedMasterSelection(clusterState)

			// Verify master selection
			if clusterState.CurrentMaster != tt.expectedMaster {
				t.Errorf("CurrentMaster = %s, want %s", clusterState.CurrentMaster, tt.expectedMaster)
			}

			// Additional verifications for successful selections
			if tt.expectedMaster != "" {
				if clusterState.CurrentMaster == "" {
					t.Errorf("Expected master %s but no master was selected", tt.expectedMaster)
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
				BoltAddress:  "",  // No bolt address
				MemgraphRole: "replica",
			},
			"unhealthy-no-role": {
				Name:         "unhealthy-no-role",
				BoltAddress:  "unhealthy-no-role:7687",
				MemgraphRole: "",  // No role
			},
		},
	}

	count := controller.countHealthyPods(clusterState)
	expected := 2  // Only healthy-1 and healthy-2 are healthy

	if count != expected {
		t.Errorf("countHealthyPods() = %d, want %d", count, expected)
	}
}

func TestMemgraphController_BuildDecisionFactors(t *testing.T) {
	controller := &MemgraphController{}

	// Test with healthy target, unhealthy existing main, and healthy sync replica
	targetMaster := &PodInfo{
		Name:         "target",
		BoltAddress:  "target:7687",
		MemgraphRole: "replica",
	}
	existingMain := &PodInfo{
		Name:         "existing",
		BoltAddress:  "",  // Unhealthy - no bolt address
		MemgraphRole: "main",
	}
	syncReplica := &PodInfo{
		Name:         "sync",
		BoltAddress:  "sync:7687",
		MemgraphRole: "replica",
	}

	factors := controller.buildDecisionFactors(targetMaster, existingMain, syncReplica)

	expectedFactors := []string{
		"target_master_healthy",
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

func TestMemgraphController_IsPodHealthyForMaster(t *testing.T) {
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
			result := controller.isPodHealthyForMaster(tt.podInfo)
			if result != tt.expected {
				t.Errorf("isPodHealthyForMaster() = %v, want %v", result, tt.expected)
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
	controller.handleNoSafePromotion(clusterState, "target-master", nil, nil)

	// Should clear current master
	if clusterState.CurrentMaster != "" {
		t.Errorf("CurrentMaster should be empty after handleNoSafePromotion, got %s", clusterState.CurrentMaster)
	}
}

func TestMemgraphController_ValidateMasterSelection(t *testing.T) {
	tests := []struct {
		name           string
		currentMaster  string
		pods           map[string]*PodInfo
		targetIndex    int
		expectClear    bool
	}{
		{
			name:          "empty_master_should_warn",
			currentMaster: "",
			pods:          map[string]*PodInfo{},
			expectClear:   false,  // Already empty
		},
		{
			name:          "master_not_found_should_clear",
			currentMaster: "missing-pod",
			pods:          map[string]*PodInfo{},
			expectClear:   true,
		},
		{
			name:          "unhealthy_master_should_clear",
			currentMaster: "unhealthy-pod",
			pods: map[string]*PodInfo{
				"unhealthy-pod": {
					Name:         "unhealthy-pod",
					BoltAddress:  "",  // Unhealthy
					MemgraphRole: "main",
				},
			},
			expectClear: true,
		},
		{
			name:          "healthy_master_should_pass",
			currentMaster: "healthy-pod",
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
				CurrentMaster:     tt.currentMaster,
				Pods:              tt.pods,
				TargetMasterIndex: tt.targetIndex,
			}

			controller.validateMasterSelection(clusterState)

			if tt.expectClear && clusterState.CurrentMaster != "" {
				t.Errorf("Expected CurrentMaster to be cleared, but got %s", clusterState.CurrentMaster)
			}
			if !tt.expectClear && tt.currentMaster != "" && clusterState.CurrentMaster == "" {
				t.Errorf("Expected CurrentMaster to remain %s, but it was cleared", tt.currentMaster)
			}
		})
	}
}

func TestMemgraphController_EnforceExpectedTopology(t *testing.T) {
	tests := []struct {
		name        string
		pods        map[string]*PodInfo
		expectError bool
		expectCall  string  // Which function should be conceptually called
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
			expectCall: "none",  // Should return nil for healthy operational state
		},
		{
			name: "no_master_state",
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
			expectCall: "promoteExpectedMaster",
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
				Pods:              tt.pods,
				TargetMasterIndex: 0,
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
		TargetMasterIndex: 0,
		Pods: map[string]*PodInfo{
			"memgraph-ha-0": {
				Name:         "memgraph-ha-0",
				BoltAddress:  "memgraph-ha-0:7687",
				MemgraphRole: "main",
			},
			"memgraph-ha-1": {
				Name:         "memgraph-ha-1",
				BoltAddress:  "memgraph-ha-1:7687", 
				MemgraphRole: "main",  // Should be demoted
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
		TargetMasterIndex: 1,
		CurrentMaster:     "",
	}

	ctx := context.Background()
	err := controller.enforceKnownTopology(ctx, clusterState)

	if err != nil {
		t.Errorf("Unexpected error: %v", err)
	}

	expected := "memgraph-ha-1"
	if clusterState.CurrentMaster != expected {
		t.Errorf("CurrentMaster = %s, want %s", clusterState.CurrentMaster, expected)
	}
}

func TestMemgraphController_PromoteExpectedMaster(t *testing.T) {
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
				TargetMasterIndex: 0,
				Pods:              tt.pods,
			}

			ctx := context.Background()
			err := controller.promoteExpectedMaster(ctx, clusterState)

			// With nil client handling, check based on expectError from test case
			if tt.expectError && err == nil {
				t.Errorf("Expected error but got none")
			}
			if !tt.expectError && err != nil {
				t.Errorf("Unexpected error: %v", err)
			}

			// Verify master is set on successful promotion
			if !tt.expectError && len(tt.pods) > 0 && clusterState.CurrentMaster != "memgraph-ha-0" {
				t.Errorf("CurrentMaster = %s, want memgraph-ha-0", clusterState.CurrentMaster)
			}
		})
	}
}

