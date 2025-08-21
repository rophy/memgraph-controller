package controller

import (
	"context"
	"testing"
	"time"
)

func TestPodInfo_GetReplicaName(t *testing.T) {
	tests := []struct {
		name     string
		podName  string
		expected string
	}{
		{
			name:     "simple name",
			podName:  "memgraph-0",
			expected: "memgraph_0",
		},
		{
			name:     "multiple dashes",
			podName:  "memgraph-cluster-1",
			expected: "memgraph_cluster_1",
		},
		{
			name:     "no dashes",
			podName:  "memgraph0",
			expected: "memgraph0",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			podInfo := &PodInfo{Name: tt.podName}
			got := podInfo.GetReplicaName()
			if got != tt.expected {
				t.Errorf("GetReplicaName() = %q, want %q", got, tt.expected)
			}
		})
	}
}

func TestPodInfo_GetReplicationAddress(t *testing.T) {
	tests := []struct {
		name        string
		podName     string
		serviceName string
		expected    string
	}{
		{
			name:        "standard service",
			podName:     "memgraph-0",
			serviceName: "memgraph",
			expected:    "memgraph-0.memgraph:10000",
		},
		{
			name:        "custom service",
			podName:     "memgraph-1",
			serviceName: "my-memgraph-svc",
			expected:    "memgraph-1.my-memgraph-svc:10000",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			podInfo := &PodInfo{Name: tt.podName}
			got := podInfo.GetReplicationAddress(tt.serviceName)
			if got != tt.expected {
				t.Errorf("GetReplicationAddress() = %q, want %q", got, tt.expected)
			}
		})
	}
}

func TestPodInfo_ShouldBecomeMaster(t *testing.T) {
	tests := []struct {
		name              string
		podName           string
		currentState      PodState
		currentMasterName string
		expected          bool
	}{
		{
			name:              "should become master - selected and not master",
			podName:           "memgraph-0",
			currentState:      INITIAL,
			currentMasterName: "memgraph-0",
			expected:          true,
		},
		{
			name:              "should not become master - not selected",
			podName:           "memgraph-1",
			currentState:      INITIAL,
			currentMasterName: "memgraph-0",
			expected:          false,
		},
		{
			name:              "should not become master - already master",
			podName:           "memgraph-0",
			currentState:      MASTER,
			currentMasterName: "memgraph-0",
			expected:          false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			podInfo := &PodInfo{
				Name:  tt.podName,
				State: tt.currentState,
			}
			got := podInfo.ShouldBecomeMaster(tt.currentMasterName)
			if got != tt.expected {
				t.Errorf("ShouldBecomeMaster() = %v, want %v", got, tt.expected)
			}
		})
	}
}

func TestPodInfo_ShouldBecomeReplica(t *testing.T) {
	tests := []struct {
		name              string
		podName           string
		currentState      PodState
		currentMasterName string
		expected          bool
	}{
		{
			name:              "should become replica - not master and not replica",
			podName:           "memgraph-1",
			currentState:      INITIAL,
			currentMasterName: "memgraph-0",
			expected:          true,
		},
		{
			name:              "should not become replica - is master",
			podName:           "memgraph-0",
			currentState:      INITIAL,
			currentMasterName: "memgraph-0",
			expected:          false,
		},
		{
			name:              "should not become replica - already replica",
			podName:           "memgraph-1",
			currentState:      REPLICA,
			currentMasterName: "memgraph-0",
			expected:          false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			podInfo := &PodInfo{
				Name:  tt.podName,
				State: tt.currentState,
			}
			got := podInfo.ShouldBecomeReplica(tt.currentMasterName)
			if got != tt.expected {
				t.Errorf("ShouldBecomeReplica() = %v, want %v", got, tt.expected)
			}
		})
	}
}

func TestPodInfo_NeedsReplicationConfiguration(t *testing.T) {
	tests := []struct {
		name              string
		podName           string
		currentState      PodState
		currentMasterName string
		expected          bool
	}{
		{
			name:              "needs config - should become master",
			podName:           "memgraph-0",
			currentState:      INITIAL,
			currentMasterName: "memgraph-0",
			expected:          true,
		},
		{
			name:              "needs config - should become replica",
			podName:           "memgraph-1",
			currentState:      INITIAL,
			currentMasterName: "memgraph-0",
			expected:          true,
		},
		{
			name:              "no config needed - already master",
			podName:           "memgraph-0",
			currentState:      MASTER,
			currentMasterName: "memgraph-0",
			expected:          false,
		},
		{
			name:              "no config needed - already replica",
			podName:           "memgraph-1",
			currentState:      REPLICA,
			currentMasterName: "memgraph-0",
			expected:          false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			podInfo := &PodInfo{
				Name:  tt.podName,
				State: tt.currentState,
			}
			got := podInfo.NeedsReplicationConfiguration(tt.currentMasterName)
			if got != tt.expected {
				t.Errorf("NeedsReplicationConfiguration() = %v, want %v", got, tt.expected)
			}
		})
	}
}

func TestConfigureReplication_EmptyCluster(t *testing.T) {
	config := &Config{
		AppName:            "memgraph",
		Namespace:          "test",
		ReconcileInterval:  30 * time.Second,
		ServiceName:        "memgraph",
	}

	controller := &MemgraphController{
		config: config,
	}

	ctx := context.Background()
	clusterState := &ClusterState{
		Pods: make(map[string]*PodInfo),
	}

	err := controller.ConfigureReplication(ctx, clusterState)
	if err != nil {
		t.Errorf("ConfigureReplication() with empty cluster should not error, got: %v", err)
	}
}

func TestConfigureReplication_NoMaster(t *testing.T) {
	config := &Config{
		AppName:            "memgraph",
		Namespace:          "test",
		ReconcileInterval:  30 * time.Second,
		ServiceName:        "memgraph",
	}

	controller := &MemgraphController{
		config: config,
	}

	ctx := context.Background()
	clusterState := &ClusterState{
		Pods: map[string]*PodInfo{
			"memgraph-0": {
				Name:  "memgraph-0",
				State: INITIAL,
			},
		},
		CurrentMaster: "", // No master selected
	}

	err := controller.ConfigureReplication(ctx, clusterState)
	if err == nil {
		t.Error("ConfigureReplication() with no master should return error")
	}
	
	expectedErrMsg := "no master pod selected for replication configuration"
	if err.Error() != expectedErrMsg {
		t.Errorf("ConfigureReplication() error = %q, want %q", err.Error(), expectedErrMsg)
	}
}

func TestReplicationConfiguration_Integration(t *testing.T) {
	// This is a more complex integration test
	// In a real environment, you'd want to use a test database or mock the MemgraphClient
	
	config := &Config{
		AppName:            "memgraph",
		Namespace:          "test",
		ReconcileInterval:  30 * time.Second,
		ServiceName:        "memgraph",
	}

	// Create a cluster state with 3 pods, all in INITIAL state
	clusterState := &ClusterState{
		Pods: map[string]*PodInfo{
			"memgraph-0": {
				Name:           "memgraph-0",
				State:          INITIAL,
				MemgraphRole:   "main",
				BoltAddress:    "10.0.0.1:7687",
				Timestamp:      time.Now(),
			},
			"memgraph-1": {
				Name:           "memgraph-1",
				State:          INITIAL,
				MemgraphRole:   "main",
				BoltAddress:    "10.0.0.2:7687",
				Timestamp:      time.Now().Add(-1 * time.Minute),
			},
			"memgraph-2": {
				Name:           "memgraph-2",
				State:          INITIAL,
				MemgraphRole:   "main",
				BoltAddress:    "10.0.0.3:7687",
				Timestamp:      time.Now().Add(-2 * time.Minute),
			},
		},
		CurrentMaster: "memgraph-0", // Latest timestamp
	}

	// Test the helper functions
	master := clusterState.Pods["memgraph-0"]
	replica1 := clusterState.Pods["memgraph-1"]
	replica2 := clusterState.Pods["memgraph-2"]

	// Verify master logic
	if !master.ShouldBecomeMaster("memgraph-0") {
		t.Error("memgraph-0 should become master")
	}
	if master.ShouldBecomeReplica("memgraph-0") {
		t.Error("memgraph-0 should not become replica")
	}

	// Verify replica logic
	if replica1.ShouldBecomeMaster("memgraph-0") {
		t.Error("memgraph-1 should not become master")
	}
	if !replica1.ShouldBecomeReplica("memgraph-0") {
		t.Error("memgraph-1 should become replica")
	}

	if replica2.ShouldBecomeMaster("memgraph-0") {
		t.Error("memgraph-2 should not become master")
	}
	if !replica2.ShouldBecomeReplica("memgraph-0") {
		t.Error("memgraph-2 should become replica")
	}

	// Test replica name conversion
	if replica1.GetReplicaName() != "memgraph_1" {
		t.Errorf("Expected replica name 'memgraph_1', got %q", replica1.GetReplicaName())
	}

	// Test replication address generation
	expectedAddr := "memgraph-1.memgraph:10000"
	if replica1.GetReplicationAddress(config.ServiceName) != expectedAddr {
		t.Errorf("Expected replication address %q, got %q", 
			expectedAddr, replica1.GetReplicationAddress(config.ServiceName))
	}
}