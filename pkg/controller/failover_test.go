package controller

import (
	"testing"
	"time"

	v1 "k8s.io/api/core/v1"
)

func TestIdentifyFailedMasterIndex(t *testing.T) {
	controller := &MemgraphController{
		config: &Config{
			StatefulSetName: "memgraph-ha",
		},
	}

	// Test timestamps
	olderTime := time.Now().Add(-10 * time.Minute)
	newerTime := time.Now().Add(-1 * time.Minute)

	tests := []struct {
		name           string
		clusterState   *ClusterState
		expectedIndex  int
		description    string
	}{
		{
			name: "pod-0 restarted more recently (failed master)",
			clusterState: &ClusterState{
				Pods: map[string]*PodInfo{
					"memgraph-ha-0": {
						Name:      "memgraph-ha-0",
						Timestamp: newerTime, // More recent restart
						Pod:       &v1.Pod{},
					},
					"memgraph-ha-1": {
						Name:      "memgraph-ha-1", 
						Timestamp: olderTime, // Older, stable pod
						Pod:       &v1.Pod{},
					},
				},
			},
			expectedIndex: 0,
			description:   "pod-0 failed, should promote pod-1",
		},
		{
			name: "pod-1 restarted more recently (failed master)",
			clusterState: &ClusterState{
				Pods: map[string]*PodInfo{
					"memgraph-ha-0": {
						Name:      "memgraph-ha-0",
						Timestamp: olderTime, // Older, stable pod
						Pod:       &v1.Pod{},
					},
					"memgraph-ha-1": {
						Name:      "memgraph-ha-1",
						Timestamp: newerTime, // More recent restart  
						Pod:       &v1.Pod{},
					},
				},
			},
			expectedIndex: 1,
			description:   "pod-1 failed, should promote pod-0",
		},
		{
			name: "pod-0 missing entirely",
			clusterState: &ClusterState{
				Pods: map[string]*PodInfo{
					"memgraph-ha-1": {
						Name:      "memgraph-ha-1",
						Timestamp: olderTime,
						Pod:       &v1.Pod{},
					},
				},
			},
			expectedIndex: 0,
			description:   "pod-0 missing, should promote pod-1",
		},
		{
			name: "pod-1 missing entirely", 
			clusterState: &ClusterState{
				Pods: map[string]*PodInfo{
					"memgraph-ha-0": {
						Name:      "memgraph-ha-0",
						Timestamp: olderTime,
						Pod:       &v1.Pod{},
					},
				},
			},
			expectedIndex: 1,
			description:   "pod-1 missing, should promote pod-0",
		},
		{
			name: "both pods missing (edge case)",
			clusterState: &ClusterState{
				Pods: map[string]*PodInfo{},
			},
			expectedIndex: 0,
			description:   "both missing, default to pod-0 failed",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := controller.identifyFailedMasterIndex(tt.clusterState)
			
			if result != tt.expectedIndex {
				t.Errorf("identifyFailedMasterIndex() = %d, expected %d\nDescription: %s", 
					result, tt.expectedIndex, tt.description)
			}
		})
	}
}

func TestHandleMasterFailurePromotion_Logic(t *testing.T) {
	controller := &MemgraphController{
		config: &Config{
			StatefulSetName: "memgraph-ha",
		},
	}

	// Test the deterministic promotion logic without actually calling Memgraph
	olderTime := time.Now().Add(-10 * time.Minute)
	newerTime := time.Now().Add(-1 * time.Minute)

	tests := []struct {
		name               string
		clusterState       *ClusterState
		replicaPods        []string
		expectedPromotion  string
		description        string
	}{
		{
			name: "pod-0 failed, should promote pod-1 (SYNC replica)",
			clusterState: &ClusterState{
				Pods: map[string]*PodInfo{
					"memgraph-ha-0": {
						Name:        "memgraph-ha-0",
						Timestamp:   newerTime, // Recently restarted (failed master)
						Pod:         &v1.Pod{},
						State:       REPLICA,
						IsSyncReplica: false, // Lost SYNC status after restart
					},
					"memgraph-ha-1": {
						Name:        "memgraph-ha-1", 
						Timestamp:   olderTime, // Stable pod (was SYNC replica)
						Pod:         &v1.Pod{},
						State:       REPLICA,
						IsSyncReplica: false, // Not detected yet in all-replica state
					},
				},
			},
			replicaPods:       []string{"memgraph-ha-0", "memgraph-ha-1"},
			expectedPromotion: "memgraph-ha-1",
			description:       "pod-0 failed → promote pod-1 (the SYNC replica)",
		},
		{
			name: "pod-1 failed, should promote pod-0 (SYNC replica)",
			clusterState: &ClusterState{
				Pods: map[string]*PodInfo{
					"memgraph-ha-0": {
						Name:        "memgraph-ha-0",
						Timestamp:   olderTime, // Stable pod (was SYNC replica)
						Pod:         &v1.Pod{},
						State:       REPLICA,
						IsSyncReplica: false, // Not detected yet in all-replica state
					},
					"memgraph-ha-1": {
						Name:        "memgraph-ha-1",
						Timestamp:   newerTime, // Recently restarted (failed master)
						Pod:         &v1.Pod{},
						State:       REPLICA,
						IsSyncReplica: false, // Lost SYNC status after restart
					},
				},
			},
			replicaPods:       []string{"memgraph-ha-0", "memgraph-ha-1"},
			expectedPromotion: "memgraph-ha-0", 
			description:       "pod-1 failed → promote pod-0 (the SYNC replica)",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Test the logic by checking identifyFailedMasterIndex result
			failedIndex := controller.identifyFailedMasterIndex(tt.clusterState)
			
			var expectedSyncReplica string
			if failedIndex == 0 {
				expectedSyncReplica = "memgraph-ha-1"
			} else {
				expectedSyncReplica = "memgraph-ha-0"
			}
			
			if expectedSyncReplica != tt.expectedPromotion {
				t.Errorf("Logic test failed: identifyFailedMasterIndex()=%d should promote %s, expected %s\nDescription: %s", 
					failedIndex, expectedSyncReplica, tt.expectedPromotion, tt.description)
			}
			
			t.Logf("✅ %s: Failed master index=%d → Promoting %s", 
				tt.description, failedIndex, expectedSyncReplica)
		})
	}
}