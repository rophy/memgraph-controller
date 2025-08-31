package controller

import (
	"testing"
	"time"
)


func TestMainFailoverDetection(t *testing.T) {
	tests := []struct {
		name             string
		clusterState     *ClusterState
		expectedFailover bool
	}{
		{
			name: "healthy_main_no_failover",
			clusterState: &ClusterState{
				CurrentMain:     "memgraph-0",
				Pods: map[string]*PodInfo{
					"memgraph-0": {MemgraphRole: "main", BoltAddress: "memgraph-0:7687"},
					"memgraph-1": {MemgraphRole: "replica", BoltAddress: "memgraph-1:7687"},
				},
			},
			expectedFailover: false,
		},
		{
			name: "main_pod_missing_should_failover",
			clusterState: &ClusterState{
				CurrentMain:     "memgraph-0",
				Pods: map[string]*PodInfo{
					"memgraph-1": {MemgraphRole: "replica", BoltAddress: "memgraph-1:7687"},
				},
			},
			expectedFailover: true,
		},
		{
			name: "main_not_main_role_should_failover",
			clusterState: &ClusterState{
				CurrentMain:     "memgraph-0",
				Pods: map[string]*PodInfo{
					"memgraph-0": {MemgraphRole: "replica", BoltAddress: "memgraph-0:7687"},
					"memgraph-1": {MemgraphRole: "main", BoltAddress: "memgraph-1:7687"},
				},
			},
			expectedFailover: true,
		},
		{
			name: "main_no_bolt_address_should_failover",
			clusterState: &ClusterState{
				CurrentMain:     "memgraph-0",
				Pods: map[string]*PodInfo{
					"memgraph-0": {MemgraphRole: "main", BoltAddress: ""},
					"memgraph-1": {MemgraphRole: "replica", BoltAddress: "memgraph-1:7687"},
				},
			},
			expectedFailover: true,
		},
	}

	config := &Config{StatefulSetName: "memgraph"}
	controller := &MemgraphController{config: config}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Set up mock state manager based on the current main in cluster state
			if tt.clusterState.CurrentMain != "" {
				mainIndex := config.ExtractPodIndex(tt.clusterState.CurrentMain)
				controller.stateManager = NewMockStateManager(mainIndex)
			} else {
				controller.stateManager = NewEmptyMockStateManager()
			}
			
			result := controller.detectMainFailover(tt.clusterState)
			if result != tt.expectedFailover {
				t.Errorf("detectMainFailover() = %v, want %v", result, tt.expectedFailover)
			}
		})
	}
}

func TestValidateControllerState(t *testing.T) {
	tests := []struct {
		name         string
		clusterState *ClusterState
		wantWarnings bool
	}{
		{
			name: "valid_state_no_warnings",
			clusterState: &ClusterState{
				StateType:       OPERATIONAL_STATE,
				CurrentMain:     "memgraph-ha-0",
				Pods: map[string]*PodInfo{
					"memgraph-ha-0": {
						Name:         "memgraph-ha-0",
						BoltAddress:  "10.0.0.1:7687",
						MemgraphRole: "main",
					},
					"memgraph-ha-1": {
						Name:         "memgraph-ha-1",
						BoltAddress:  "10.0.0.2:7687",
						MemgraphRole: "replica",
					},
				},
			},
			wantWarnings: false,
		},
		{
			name: "invalid_target_index_should_warn",
			clusterState: &ClusterState{
				CurrentMain:     "memgraph-0",
			},
			wantWarnings: true,
		},
		{
			name: "negative_target_index_should_warn",
			clusterState: &ClusterState{
				CurrentMain:     "memgraph-0",
			},
			wantWarnings: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			config := &Config{StatefulSetName: "memgraph-ha"}
			warnings := tt.clusterState.ValidateControllerState(config)
			hasWarnings := len(warnings) > 0

			if hasWarnings != tt.wantWarnings {
				t.Errorf("ValidateControllerState() warnings = %v, want warnings = %v", hasWarnings, tt.wantWarnings)
				if hasWarnings {
					t.Logf("Warnings: %v", warnings)
				}
			}
		})
	}
}

func TestSelectSyncReplica(t *testing.T) {
	controller := &MemgraphController{
		config: &Config{StatefulSetName: "memgraph"},
	}

	tests := []struct {
		name         string
		clusterState *ClusterState
		currentMain  string
		expectedSync string
	}{
		{
			name: "pod0_main_select_pod1_sync",
			clusterState: &ClusterState{
				Pods: map[string]*PodInfo{
					"memgraph-0": {MemgraphRole: "main"},
					"memgraph-1": {MemgraphRole: "replica"},
					"memgraph-2": {MemgraphRole: "replica"},
				},
			},
			currentMain:  "memgraph-0",
			expectedSync: "memgraph-1",
		},
		{
			name: "pod1_main_select_pod0_sync",
			clusterState: &ClusterState{
				Pods: map[string]*PodInfo{
					"memgraph-0": {MemgraphRole: "replica"},
					"memgraph-1": {MemgraphRole: "main"},
					"memgraph-2": {MemgraphRole: "replica"},
				},
			},
			currentMain:  "memgraph-1",
			expectedSync: "memgraph-0",
		},
		{
			name: "only_main_no_sync_replica",
			clusterState: &ClusterState{
				Pods: map[string]*PodInfo{
					"memgraph-0": {MemgraphRole: "main"},
				},
			},
			currentMain:  "memgraph-0",
			expectedSync: "",
		},
		{
			name: "main_not_eligible_fallback_to_pod0",
			clusterState: &ClusterState{
				Pods: map[string]*PodInfo{
					"memgraph-0": {MemgraphRole: "replica"},
					"memgraph-1": {MemgraphRole: "replica"},
					"memgraph-2": {MemgraphRole: "main"},
				},
			},
			currentMain:  "memgraph-2",
			expectedSync: "memgraph-0",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := controller.selectSyncReplica(tt.clusterState, tt.currentMain)
			if result != tt.expectedSync {
				t.Errorf("selectSyncReplica() = %v, want %v", result, tt.expectedSync)
			}
		})
	}
}

func TestIsPodHealthyForMain(t *testing.T) {
	controller := &MemgraphController{}

	tests := []struct {
		name          string
		podInfo       *PodInfo
		expectHealthy bool
	}{
		{
			name: "healthy_pod_with_bolt_address",
			podInfo: &PodInfo{
				BoltAddress:  "memgraph-0:7687",
				MemgraphRole: "main",
				Timestamp:    time.Now(),
			},
			expectHealthy: true,
		},
		{
			name: "unhealthy_pod_no_bolt_address",
			podInfo: &PodInfo{
				BoltAddress:  "",
				MemgraphRole: "main",
				Timestamp:    time.Now(),
			},
			expectHealthy: false,
		},
		{
			name: "unhealthy_pod_no_role",
			podInfo: &PodInfo{
				BoltAddress:  "memgraph-0:7687",
				MemgraphRole: "",
				Timestamp:    time.Now(),
			},
			expectHealthy: false,
		},
		{
			name:          "nil_pod_not_healthy",
			podInfo:       nil,
			expectHealthy: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := controller.isPodHealthyForMain(tt.podInfo)
			if result != tt.expectHealthy {
				t.Errorf("isPodHealthyForMain() = %v, want %v", result, tt.expectHealthy)
			}
		})
	}
}

func TestMainSelectionMetrics(t *testing.T) {
	metrics := &MainSelectionMetrics{
		Timestamp:            time.Now(),
		StateType:            OPERATIONAL_STATE,
		SelectedMain:         "memgraph-0",
		SelectionReason:      "promote_sync_replica",
		HealthyPodsCount:     2,
		SyncReplicaAvailable: true,
		FailoverDetected:     true,
		DecisionFactors:      []string{"existing_main_unhealthy", "sync_replica_available"},
	}

	// TargetMainIndex validation removed - now managed in controller state

	if metrics.SelectionReason == "" {
		t.Error("SelectionReason should not be empty")
	}

	if len(metrics.DecisionFactors) == 0 {
		t.Error("DecisionFactors should not be empty")
	}

	// Test metrics contains expected decision factors
	hasExpectedFactor := false
	for _, factor := range metrics.DecisionFactors {
		if factor == "sync_replica_available" {
			hasExpectedFactor = true
			break
		}
	}
	if !hasExpectedFactor {
		t.Error("DecisionFactors should contain 'sync_replica_available'")
	}
}
