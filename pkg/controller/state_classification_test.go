package controller

import (
	"testing"
)

func TestPodInfo_ClassifyState(t *testing.T) {
	tests := []struct {
		name           string
		kubernetesRole string
		memgraphRole   string
		replicaCount   int
		expectedState  PodState
	}{
		{
			name:           "initial state - no label, MAIN with no replicas",
			kubernetesRole: "",
			memgraphRole:   "MAIN",
			replicaCount:   0,
			expectedState:  INITIAL,
		},
		{
			name:           "master state - master label, MAIN role",
			kubernetesRole: "master",
			memgraphRole:   "MAIN",
			replicaCount:   2,
			expectedState:  MASTER,
		},
		{
			name:           "replica state - replica label, REPLICA role",
			kubernetesRole: "replica",
			memgraphRole:   "REPLICA",
			replicaCount:   0,
			expectedState:  REPLICA,
		},
		{
			name:           "no memgraph role - return current state",
			kubernetesRole: "master",
			memgraphRole:   "",
			replicaCount:   0,
			expectedState:  INITIAL, // Current state unchanged
		},
		{
			name:           "inconsistent state - master label with no replicas",
			kubernetesRole: "master",
			memgraphRole:   "MAIN",
			replicaCount:   0,
			expectedState:  MASTER, // Still classified as MASTER despite no replicas
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			podInfo := &PodInfo{
				Name:           "test-pod",
				State:          INITIAL,
				KubernetesRole: tt.kubernetesRole,
				MemgraphRole:   tt.memgraphRole,
				Replicas:       make([]string, tt.replicaCount),
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
		name               string
		kubernetesRole     string
		memgraphRole       string
		currentState       PodState
		replicaCount       int
		expectInconsistency bool
		expectedDescription string
	}{
		{
			name:               "consistent initial state",
			kubernetesRole:     "",
			memgraphRole:       "MAIN",
			currentState:       INITIAL,
			replicaCount:       0,
			expectInconsistency: false,
		},
		{
			name:                "inconsistent - no role but has replicas",
			kubernetesRole:      "",
			memgraphRole:        "MAIN",
			currentState:        INITIAL,
			replicaCount:        2,
			expectInconsistency: true,
			expectedDescription: "Pod has no role label but is MAIN with 2 replicas (should be MASTER)",
		},
		{
			name:                "inconsistent - master label but REPLICA role",
			kubernetesRole:      "master",
			memgraphRole:        "REPLICA",
			currentState:        MASTER,
			replicaCount:        0,
			expectInconsistency: true,
			expectedDescription: "Pod labeled as master but Memgraph role is REPLICA",
		},
		{
			name:                "inconsistent - replica label but MAIN role",
			kubernetesRole:      "replica",
			memgraphRole:        "MAIN",
			currentState:        REPLICA,
			replicaCount:        0,
			expectInconsistency: true,
			expectedDescription: "Pod labeled as replica but Memgraph role is MAIN",
		},
		{
			name:                "inconsistent - no role but REPLICA",
			kubernetesRole:      "",
			memgraphRole:        "REPLICA",
			currentState:        INITIAL,
			replicaCount:        0,
			expectInconsistency: true,
			expectedDescription: "Pod has no role label but Memgraph role is REPLICA",
		},
		{
			name:               "no memgraph role info",
			kubernetesRole:     "master",
			memgraphRole:       "",
			currentState:       MASTER,
			replicaCount:       0,
			expectInconsistency: false,
		},
		{
			name:               "consistent master state",
			kubernetesRole:     "master",
			memgraphRole:       "MAIN",
			currentState:       MASTER,
			replicaCount:       1,
			expectInconsistency: false,
		},
		{
			name:               "consistent replica state",
			kubernetesRole:     "replica",
			memgraphRole:       "REPLICA",
			currentState:       REPLICA,
			replicaCount:       0,
			expectInconsistency: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			podInfo := &PodInfo{
				Name:           "test-pod",
				State:          tt.currentState,
				KubernetesRole: tt.kubernetesRole,
				MemgraphRole:   tt.memgraphRole,
				Replicas:       make([]string, tt.replicaCount),
			}

			inconsistency := podInfo.DetectStateInconsistency()

			if tt.expectInconsistency {
				if inconsistency == nil {
					t.Error("Expected inconsistency but got none")
					return
				}
				if inconsistency.Description != tt.expectedDescription {
					t.Errorf("Description = %q, want %q", inconsistency.Description, tt.expectedDescription)
				}
				if inconsistency.PodName != "test-pod" {
					t.Errorf("PodName = %q, want test-pod", inconsistency.PodName)
				}
				if inconsistency.KubernetesRole != tt.kubernetesRole {
					t.Errorf("KubernetesRole = %q, want %q", inconsistency.KubernetesRole, tt.kubernetesRole)
				}
				if inconsistency.MemgraphRole != tt.memgraphRole {
					t.Errorf("MemgraphRole = %q, want %q", inconsistency.MemgraphRole, tt.memgraphRole)
				}
			} else {
				if inconsistency != nil {
					t.Errorf("Expected no inconsistency but got: %s", inconsistency.Description)
				}
			}
		})
	}
}

func TestBuildInconsistencyDescription(t *testing.T) {
	tests := []struct {
		name           string
		kubernetesRole string
		memgraphRole   string
		replicaCount   int
		expected       string
	}{
		{
			name:           "no role but has replicas",
			kubernetesRole: "",
			memgraphRole:   "MAIN",
			replicaCount:   3,
			expected:       "Pod has no role label but is MAIN with 3 replicas (should be MASTER)",
		},
		{
			name:           "master but replica role",
			kubernetesRole: "master",
			memgraphRole:   "REPLICA",
			replicaCount:   0,
			expected:       "Pod labeled as master but Memgraph role is REPLICA",
		},
		{
			name:           "replica but main role",
			kubernetesRole: "replica",
			memgraphRole:   "MAIN",
			replicaCount:   0,
			expected:       "Pod labeled as replica but Memgraph role is MAIN",
		},
		{
			name:           "unknown inconsistency",
			kubernetesRole: "unknown",
			memgraphRole:   "UNKNOWN",
			replicaCount:   1,
			expected:       "Unknown inconsistency: k8s_role=unknown, memgraph_role=UNKNOWN, replicas=1",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			podInfo := &PodInfo{
				KubernetesRole: tt.kubernetesRole,
				MemgraphRole:   tt.memgraphRole,
				Replicas:       make([]string, tt.replicaCount),
			}

			got := buildInconsistencyDescription(podInfo)
			if got != tt.expected {
				t.Errorf("buildInconsistencyDescription() = %q, want %q", got, tt.expected)
			}
		})
	}
}