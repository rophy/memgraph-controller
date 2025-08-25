package controller

import (
	"testing"
)

func TestPodInfo_ClassifyState(t *testing.T) {
	tests := []struct {
		name          string
		memgraphRole  string
		replicaCount  int
		expectedState PodState
	}{
		{
			name:          "initial state - main with no replicas",
			memgraphRole:  "main",
			replicaCount:  0,
			expectedState: INITIAL,
		},
		{
			name:          "main state - main role with replicas",
			memgraphRole:  "main",
			replicaCount:  2,
			expectedState: MAIN,
		},
		{
			name:          "replica state - replica role",
			memgraphRole:  "replica",
			replicaCount:  0,
			expectedState: REPLICA,
		},
		{
			name:          "no memgraph role - return current state",
			memgraphRole:  "",
			replicaCount:  0,
			expectedState: INITIAL, // Current state unchanged
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			podInfo := &PodInfo{
				Name:         "test-pod",
				State:        INITIAL,
				MemgraphRole: tt.memgraphRole,
				Replicas:     make([]string, tt.replicaCount),
			}

			got := podInfo.ClassifyState()
			if got != tt.expectedState {
				t.Errorf("ClassifyState() = %s, want %s", got, tt.expectedState)
			}
		})
	}
}

func TestPodInfo_DetectStateInconsistency(t *testing.T) {
	tests := []struct {
		name                string
		memgraphRole        string
		currentState        PodState
		replicaCount        int
		expectInconsistency bool
	}{
		{
			name:                "consistent initial state",
			memgraphRole:        "main",
			currentState:        INITIAL,
			replicaCount:        0,
			expectInconsistency: false,
		},
		{
			name:                "inconsistent - should be main but marked as initial",
			memgraphRole:        "main",
			currentState:        INITIAL,
			replicaCount:        2,
			expectInconsistency: true,
		},
		{
			name:                "inconsistent - should be replica but marked as main",
			memgraphRole:        "replica",
			currentState:        MAIN,
			replicaCount:        0,
			expectInconsistency: true,
		},
		{
			name:                "inconsistent - should be main but marked as replica",
			memgraphRole:        "main",
			currentState:        REPLICA,
			replicaCount:        2,
			expectInconsistency: true,
		},
		{
			name:                "no memgraph role info",
			memgraphRole:        "",
			currentState:        MAIN,
			replicaCount:        0,
			expectInconsistency: false,
		},
		{
			name:                "consistent main state",
			memgraphRole:        "main",
			currentState:        MAIN,
			replicaCount:        1,
			expectInconsistency: false,
		},
		{
			name:                "consistent replica state",
			memgraphRole:        "replica",
			currentState:        REPLICA,
			replicaCount:        0,
			expectInconsistency: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			podInfo := &PodInfo{
				Name:         "test-pod",
				State:        tt.currentState,
				MemgraphRole: tt.memgraphRole,
				Replicas:     make([]string, tt.replicaCount),
			}

			inconsistency := podInfo.DetectStateInconsistency()

			if tt.expectInconsistency {
				if inconsistency == nil {
					t.Error("Expected inconsistency but got none")
					return
				}
				if inconsistency.MemgraphRole != tt.memgraphRole {
					t.Errorf("MemgraphRole = %q, want %q", inconsistency.MemgraphRole, tt.memgraphRole)
				}
			} else {
				if inconsistency != nil {
					t.Errorf("Expected no inconsistency but got: %+v", inconsistency)
				}
			}
		})
	}
}
