package controller

import (
	"testing"
	"time"
)

func TestClassifyClusterState(t *testing.T) {
	tests := []struct {
		name         string
		clusterState *ClusterState
		expectedType ClusterStateType
	}{
		{
			name: "initial_state_all_main",
			clusterState: &ClusterState{
				Pods: map[string]*PodInfo{
					"memgraph-0": {MemgraphRole: "main"},
					"memgraph-1": {MemgraphRole: "main"},
					"memgraph-2": {MemgraphRole: "main"},
				},
			},
			expectedType: INITIAL_STATE,
		},
		{
			name: "operational_state_one_main",
			clusterState: &ClusterState{
				Pods: map[string]*PodInfo{
					"memgraph-0": {MemgraphRole: "main"},
					"memgraph-1": {MemgraphRole: "replica"},
					"memgraph-2": {MemgraphRole: "replica"},
				},
			},
			expectedType: OPERATIONAL_STATE,
		},
		{
			name: "split_brain_multiple_mains",
			clusterState: &ClusterState{
				Pods: map[string]*PodInfo{
					"memgraph-0": {MemgraphRole: "main"},
					"memgraph-1": {MemgraphRole: "replica"},
					"memgraph-2": {MemgraphRole: "main"},
				},
			},
			expectedType: SPLIT_BRAIN_STATE,
		},
		{
			name: "no_main_all_replicas",
			clusterState: &ClusterState{
				Pods: map[string]*PodInfo{
					"memgraph-0": {MemgraphRole: "replica"},
					"memgraph-1": {MemgraphRole: "replica"},
					"memgraph-2": {MemgraphRole: "replica"},
				},
			},
			expectedType: NO_MAIN_STATE,
		},
		{
			name: "initial_state_no_roles",
			clusterState: &ClusterState{
				Pods: map[string]*PodInfo{
					"memgraph-0": {MemgraphRole: ""},
					"memgraph-1": {MemgraphRole: ""},
				},
			},
			expectedType: INITIAL_STATE,
		},
		{
			name: "empty_cluster",
			clusterState: &ClusterState{
				Pods: map[string]*PodInfo{},
			},
			expectedType: INITIAL_STATE,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := tt.clusterState.ClassifyClusterState()
			if result != tt.expectedType {
				t.Errorf("ClassifyClusterState() = %v, want %v", result, tt.expectedType)
			}
		})
	}
}

func TestIsBootstrapSafe(t *testing.T) {
	tests := []struct {
		name         string
		clusterState *ClusterState
		expectedSafe bool
	}{
		{
			name: "fresh_cluster_safe",
			clusterState: &ClusterState{
				Pods: map[string]*PodInfo{
					"memgraph-0": {MemgraphRole: "main"},
					"memgraph-1": {MemgraphRole: "main"},
				},
			},
			expectedSafe: true,
		},
		{
			name: "operational_cluster_safe",
			clusterState: &ClusterState{
				Pods: map[string]*PodInfo{
					"memgraph-0": {MemgraphRole: "main"},
					"memgraph-1": {MemgraphRole: "replica"},
				},
			},
			expectedSafe: true,
		},
		{
			name: "split_brain_unsafe",
			clusterState: &ClusterState{
				Pods: map[string]*PodInfo{
					"memgraph-0": {MemgraphRole: "main"},
					"memgraph-1": {MemgraphRole: "main"},
					"memgraph-2": {MemgraphRole: "replica"},
				},
			},
			expectedSafe: false,
		},
		{
			name: "no_main_unsafe",
			clusterState: &ClusterState{
				Pods: map[string]*PodInfo{
					"memgraph-0": {MemgraphRole: "replica"},
					"memgraph-1": {MemgraphRole: "replica"},
				},
			},
			expectedSafe: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := tt.clusterState.IsBootstrapSafe()
			if result != tt.expectedSafe {
				t.Errorf("IsBootstrapSafe() = %v, want %v", result, tt.expectedSafe)
			}
		})
	}
}

func TestGetMainPods(t *testing.T) {
	clusterState := &ClusterState{
		Pods: map[string]*PodInfo{
			"memgraph-0": {MemgraphRole: "main"},
			"memgraph-1": {MemgraphRole: "replica"},
			"memgraph-2": {MemgraphRole: "main"},
			"memgraph-3": {MemgraphRole: ""},
		},
	}

	mainPods := clusterState.GetMainPods()

	expectedCount := 2
	if len(mainPods) != expectedCount {
		t.Errorf("GetMainPods() returned %d pods, want %d", len(mainPods), expectedCount)
	}

	// Verify the correct pods are returned
	foundPods := make(map[string]bool)
	for _, podName := range mainPods {
		foundPods[podName] = true
	}

	if !foundPods["memgraph-0"] {
		t.Error("GetMainPods() should include memgraph-0")
	}
	if !foundPods["memgraph-2"] {
		t.Error("GetMainPods() should include memgraph-2")
	}
	if foundPods["memgraph-1"] {
		t.Error("GetMainPods() should not include memgraph-1 (replica)")
	}
	if foundPods["memgraph-3"] {
		t.Error("GetMainPods() should not include memgraph-3 (no role)")
	}
}

func TestGetReplicaPods(t *testing.T) {
	clusterState := &ClusterState{
		Pods: map[string]*PodInfo{
			"memgraph-0": {MemgraphRole: "main"},
			"memgraph-1": {MemgraphRole: "replica"},
			"memgraph-2": {MemgraphRole: "replica"},
			"memgraph-3": {MemgraphRole: ""},
		},
	}

	replicaPods := clusterState.GetReplicaPods()

	expectedCount := 2
	if len(replicaPods) != expectedCount {
		t.Errorf("GetReplicaPods() returned %d pods, want %d", len(replicaPods), expectedCount)
	}

	// Verify the correct pods are returned
	foundPods := make(map[string]bool)
	for _, podName := range replicaPods {
		foundPods[podName] = true
	}

	if !foundPods["memgraph-1"] {
		t.Error("GetReplicaPods() should include memgraph-1")
	}
	if !foundPods["memgraph-2"] {
		t.Error("GetReplicaPods() should include memgraph-2")
	}
	if foundPods["memgraph-0"] {
		t.Error("GetReplicaPods() should not include memgraph-0 (main)")
	}
	if foundPods["memgraph-3"] {
		t.Error("GetReplicaPods() should not include memgraph-3 (no role)")
	}
}

func TestClusterStateString(t *testing.T) {
	tests := []struct {
		stateType ClusterStateType
		expected  string
	}{
		{INITIAL_STATE, "INITIAL_STATE"},
		{OPERATIONAL_STATE, "OPERATIONAL_STATE"},
		{MIXED_STATE, "MIXED_STATE"},
		{NO_MAIN_STATE, "NO_MAIN_STATE"},
		{SPLIT_BRAIN_STATE, "SPLIT_BRAIN_STATE"},
	}

	for _, tt := range tests {
		t.Run(tt.expected, func(t *testing.T) {
			result := tt.stateType.String()
			if result != tt.expected {
				t.Errorf("ClusterStateType.String() = %v, want %v", result, tt.expected)
			}
		})
	}
}

func TestClusterStateSummary(t *testing.T) {
	now := time.Now()
	clusterState := &ClusterState{
		Pods: map[string]*PodInfo{
			"memgraph-0": {
				MemgraphRole:  "main",
				BoltAddress:   "memgraph-0:7687",
				IsSyncReplica: false,
			},
			"memgraph-1": {
				MemgraphRole:  "replica",
				BoltAddress:   "memgraph-1:7687",
				IsSyncReplica: true,
			},
			"memgraph-2": {
				MemgraphRole:  "replica",
				BoltAddress:   "", // Unhealthy pod
				IsSyncReplica: false,
			},
		},
		CurrentMain:      "memgraph-0",
		TargetMainIndex:  0,
		StateType:        OPERATIONAL_STATE,
		IsBootstrapPhase: false,
		LastStateChange:  now,
	}

	summary := clusterState.GetClusterHealthSummary()

	// Verify summary contains expected fields
	if summary["total_pods"] != 3 {
		t.Errorf("Summary total_pods = %v, want 3", summary["total_pods"])
	}
	if summary["healthy_pods"] != 2 {
		t.Errorf("Summary healthy_pods = %v, want 2", summary["healthy_pods"])
	}
	if summary["unhealthy_pods"] != 1 {
		t.Errorf("Summary unhealthy_pods = %v, want 1", summary["unhealthy_pods"])
	}
	if summary["main_pods"] != 1 {
		t.Errorf("Summary main_pods = %v, want 1", summary["main_pods"])
	}
	if summary["replica_pods"] != 2 {
		t.Errorf("Summary replica_pods = %v, want 2", summary["replica_pods"])
	}
	if summary["sync_replicas"] != 1 {
		t.Errorf("Summary sync_replicas = %v, want 1", summary["sync_replicas"])
	}
	if summary["current_main"] != "memgraph-0" {
		t.Errorf("Summary current_main = %v, want memgraph-0", summary["current_main"])
	}
	if summary["target_index"] != 0 {
		t.Errorf("Summary target_index = %v, want 0", summary["target_index"])
	}
	if summary["state_type"] != "OPERATIONAL_STATE" {
		t.Errorf("Summary state_type = %v, want OPERATIONAL_STATE", summary["state_type"])
	}
	if summary["bootstrap_phase"] != false {
		t.Errorf("Summary bootstrap_phase = %v, want false", summary["bootstrap_phase"])
	}
}
