package controller

import (
	"testing"
)

func TestDetermineMasterIndex(t *testing.T) {
	tests := []struct {
		name          string
		pods          map[string]*PodInfo
		expectedIndex int
		shouldFail    bool
		expectedError string
	}{
		{
			name: "fresh_cluster_all_masters",
			pods: map[string]*PodInfo{
				"memgraph-0": {MemgraphRole: "main"},
				"memgraph-1": {MemgraphRole: "main"},
				"memgraph-2": {MemgraphRole: "main"},
			},
			expectedIndex: 0,
			shouldFail:    false,
		},
		{
			name: "healthy_cluster_pod0_master",
			pods: map[string]*PodInfo{
				"memgraph-0": {MemgraphRole: "main"},
				"memgraph-1": {MemgraphRole: "replica"},
				"memgraph-2": {MemgraphRole: "replica"},
			},
			expectedIndex: 0,
			shouldFail:    false,
		},
		{
			name: "failover_cluster_pod1_master",
			pods: map[string]*PodInfo{
				"memgraph-0": {MemgraphRole: "replica"},
				"memgraph-1": {MemgraphRole: "main"},
				"memgraph-2": {MemgraphRole: "replica"},
			},
			expectedIndex: 1,
			shouldFail:    false,
		},
		{
			name: "no_eligible_pods_available",
			pods: map[string]*PodInfo{
				"memgraph-2": {MemgraphRole: "main"},
				"memgraph-3": {MemgraphRole: "replica"},
			},
			expectedIndex: -1,
			shouldFail:    true,
			expectedError: "neither pod-0 nor pod-1 available",
		},
		{
			name: "no_role_information",
			pods: map[string]*PodInfo{
				"memgraph-0": {MemgraphRole: ""},
				"memgraph-1": {MemgraphRole: ""},
			},
			expectedIndex: 0,
			shouldFail:    false,
		},
		{
			name: "lower_index_precedence",
			pods: map[string]*PodInfo{
				"memgraph-0": {MemgraphRole: "replica"},
				"memgraph-1": {MemgraphRole: "replica"},
				"memgraph-2": {MemgraphRole: "main"},
			},
			expectedIndex: 0,
			shouldFail:    false,
		},
	}

	config := &Config{StatefulSetName: "memgraph"}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clusterState := &ClusterState{
				Pods: tt.pods,
			}

			index, err := clusterState.DetermineMasterIndex(config)

			if tt.shouldFail {
				if err == nil {
					t.Errorf("DetermineMasterIndex() expected error but got none")
					return
				}
				if tt.expectedError != "" && !containsString(err.Error(), tt.expectedError) {
					t.Errorf("DetermineMasterIndex() error = %v, want to contain %v", err.Error(), tt.expectedError)
				}
			} else {
				if err != nil {
					t.Errorf("DetermineMasterIndex() unexpected error = %v", err)
					return
				}
				if index != tt.expectedIndex {
					t.Errorf("DetermineMasterIndex() = %v, want %v", index, tt.expectedIndex)
				}
			}
		})
	}
}

func TestAnalyzeExistingCluster(t *testing.T) {
	config := &Config{StatefulSetName: "memgraph"}

	tests := []struct {
		name          string
		clusterState  *ClusterState
		expectedIndex int
		shouldFail    bool
	}{
		{
			name: "pod0_is_master",
			clusterState: &ClusterState{
				Pods: map[string]*PodInfo{
					"memgraph-0": {MemgraphRole: "main"},
					"memgraph-1": {MemgraphRole: "replica"},
				},
			},
			expectedIndex: 0,
			shouldFail:    false,
		},
		{
			name: "pod1_is_master",
			clusterState: &ClusterState{
				Pods: map[string]*PodInfo{
					"memgraph-0": {MemgraphRole: "replica"},
					"memgraph-1": {MemgraphRole: "main"},
				},
			},
			expectedIndex: 1,
			shouldFail:    false,
		},
		{
			name: "non_eligible_master_prefer_pod0",
			clusterState: &ClusterState{
				Pods: map[string]*PodInfo{
					"memgraph-0": {MemgraphRole: "replica"},
					"memgraph-1": {MemgraphRole: "replica"},
					"memgraph-2": {MemgraphRole: "main"},
				},
			},
			expectedIndex: 0,
			shouldFail:    false,
		},
		{
			name: "no_eligible_pods",
			clusterState: &ClusterState{
				Pods: map[string]*PodInfo{
					"memgraph-2": {MemgraphRole: "main"},
					"memgraph-3": {MemgraphRole: "replica"},
				},
			},
			expectedIndex: -1,
			shouldFail:    true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mainPods := tt.clusterState.GetMainPods()
			replicaPods := tt.clusterState.GetReplicaPods()

			index, err := tt.clusterState.analyzeExistingCluster(mainPods, replicaPods, config)

			if tt.shouldFail {
				if err == nil {
					t.Errorf("analyzeExistingCluster() expected error but got none")
				}
			} else {
				if err != nil {
					t.Errorf("analyzeExistingCluster() unexpected error = %v", err)
					return
				}
				if index != tt.expectedIndex {
					t.Errorf("analyzeExistingCluster() = %v, want %v", index, tt.expectedIndex)
				}
			}
		})
	}
}

// Helper function to check if string contains substring
func containsString(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(substr) == 0 || 
		(len(s) > len(substr) && findInString(s, substr)))
}

func findInString(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}