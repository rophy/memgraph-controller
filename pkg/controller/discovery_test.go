package controller

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func TestPodDiscovery_DiscoverPods(t *testing.T) {
	now := time.Now()
	
	// Create test pods
	pod1 := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "memgraph-0",
			Namespace: "memgraph",
			Labels: map[string]string{
				"app.kubernetes.io/name": "memgraph",
				"role": "master",
			},
			CreationTimestamp: metav1.NewTime(now.Add(-10 * time.Minute)),
		},
		Status: v1.PodStatus{
			Phase:     v1.PodRunning,
			PodIP:     "10.0.0.1",
			StartTime: &metav1.Time{Time: now.Add(-5 * time.Minute)},
		},
	}

	pod2 := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "memgraph-1",
			Namespace: "memgraph",
			Labels: map[string]string{
				"app.kubernetes.io/name": "memgraph",
				"role": "replica",
			},
			CreationTimestamp: metav1.NewTime(now.Add(-8 * time.Minute)),
		},
		Status: v1.PodStatus{
			Phase:     v1.PodRunning,
			PodIP:     "10.0.0.2",
			StartTime: &metav1.Time{Time: now.Add(-3 * time.Minute)}, // More recent start time
		},
	}

	// Pod in pending state (should be skipped)
	pod3 := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "memgraph-2",
			Namespace: "memgraph",
			Labels: map[string]string{
				"app.kubernetes.io/name": "memgraph",
			},
		},
		Status: v1.PodStatus{
			Phase: v1.PodPending,
		},
	}

	fakeClient := fake.NewSimpleClientset(pod1, pod2, pod3)
	config := &Config{
		AppName:     "memgraph",
		Namespace:   "memgraph",
		ServiceName: "memgraph-service",
	}

	discovery := NewPodDiscovery(fakeClient, config)
	clusterState, err := discovery.DiscoverPods(context.Background())

	if err != nil {
		t.Fatalf("DiscoverPods() failed: %v", err)
	}

	if clusterState == nil {
		t.Fatal("DiscoverPods() returned nil cluster state")
	}

	// Should find 2 running pods, skip the pending one
	if len(clusterState.Pods) != 2 {
		t.Errorf("Found %d pods, want 2", len(clusterState.Pods))
	}

	// Check pod1
	if podInfo, exists := clusterState.Pods["memgraph-0"]; exists {
		if podInfo.BoltAddress != "10.0.0.1:7687" {
			t.Errorf("Pod memgraph-0 BoltAddress = %s, want 10.0.0.1:7687", podInfo.BoltAddress)
		}
	} else {
		t.Error("Pod memgraph-0 not found")
	}

	// Check pod2
	if podInfo, exists := clusterState.Pods["memgraph-1"]; exists {
		if podInfo.ReplicaName != "memgraph_1" {
			t.Errorf("Pod memgraph-1 ReplicaName = %s, want memgraph_1", podInfo.ReplicaName)
		}
	} else {
		t.Error("Pod memgraph-1 not found")
	}

	// After discovery, master selection is deferred until after Memgraph querying
	if clusterState.CurrentMaster != "" {
		t.Errorf("CurrentMaster = %s, want empty (deferred until after Memgraph querying)", clusterState.CurrentMaster)
	}
}

func TestPodDiscovery_GetPodsByLabel(t *testing.T) {
	pod := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "custom-pod",
			Namespace: "memgraph",
			Labels: map[string]string{
				"custom": "label",
			},
		},
		Status: v1.PodStatus{
			Phase: v1.PodRunning,
			PodIP: "10.0.0.3",
		},
	}

	fakeClient := fake.NewSimpleClientset(pod)
	config := &Config{
		Namespace:   "memgraph",
		ServiceName: "memgraph-service",
	}

	discovery := NewPodDiscovery(fakeClient, config)
	clusterState, err := discovery.GetPodsByLabel(context.Background(), "custom=label")

	if err != nil {
		t.Fatalf("GetPodsByLabel() failed: %v", err)
	}

	if len(clusterState.Pods) != 1 {
		t.Errorf("Found %d pods, want 1", len(clusterState.Pods))
	}

	if _, exists := clusterState.Pods["custom-pod"]; !exists {
		t.Error("Pod custom-pod not found")
	}
}

func TestPodDiscovery_SelectMaster_EmptyCluster(t *testing.T) {
	config := &Config{}
	discovery := NewPodDiscovery(nil, config)
	clusterState := NewClusterState()

	discovery.selectMaster(clusterState)

	if clusterState.CurrentMaster != "" {
		t.Errorf("CurrentMaster = %s, want empty", clusterState.CurrentMaster)
	}
}

func TestPodDiscovery_SelectMaster_SinglePod(t *testing.T) {
	now := time.Now()
	pod := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "single-pod",
		},
		Status: v1.PodStatus{
			StartTime: &metav1.Time{Time: now},
		},
	}

	config := &Config{}
	discovery := NewPodDiscovery(nil, config)
	clusterState := NewClusterState()
	
	podInfo := NewPodInfo(pod, "service")
	clusterState.Pods["single-pod"] = podInfo

	discovery.selectMaster(clusterState)

	if clusterState.CurrentMaster != "single-pod" {
		t.Errorf("CurrentMaster = %s, want single-pod", clusterState.CurrentMaster)
	}
}

func TestPodDiscovery_SelectMaster_LatestTimestamp(t *testing.T) {
	now := time.Now()
	
	// Pod with older timestamp
	pod1 := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "older-pod",
		},
		Status: v1.PodStatus{
			StartTime: &metav1.Time{Time: now.Add(-10 * time.Minute)},
		},
	}

	// Pod with newer timestamp
	pod2 := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "newer-pod",
		},
		Status: v1.PodStatus{
			StartTime: &metav1.Time{Time: now.Add(-5 * time.Minute)},
		},
	}

	config := &Config{}
	discovery := NewPodDiscovery(nil, config)
	clusterState := NewClusterState()
	
	clusterState.Pods["older-pod"] = NewPodInfo(pod1, "service")
	clusterState.Pods["newer-pod"] = NewPodInfo(pod2, "service")

	discovery.selectMaster(clusterState)

	// With SYNC replica strategy, no master should be selected when no SYNC replica available
	if clusterState.CurrentMaster != "" {
		t.Errorf("CurrentMaster = %s, want empty (no SYNC replica available)", clusterState.CurrentMaster)
	}
}

// Tests for main controller discovery functions

func TestMemgraphController_PerformBootstrapValidation_SafeStates(t *testing.T) {
	tests := []struct {
		name           string
		stateType      ClusterStateType
		bootstrapSafe  bool
		expectError    bool
		errorContains  string
	}{
		{
			name:          "operational_state_safe",
			stateType:     OPERATIONAL_STATE,
			bootstrapSafe: true,
			expectError:   false,
		},
		{
			name:          "initial_state_safe",
			stateType:     INITIAL_STATE,
			bootstrapSafe: true,
			expectError:   false,
		},
		{
			name:          "split_brain_state_unsafe",
			stateType:     SPLIT_BRAIN_STATE,
			bootstrapSafe: false,
			expectError:   true,
			errorContains: "split-brain during bootstrap",
		},
		{
			name:          "no_master_state_unsafe",
			stateType:     NO_MASTER_STATE,
			bootstrapSafe: false,
			expectError:   true,
			errorContains: "no master during bootstrap",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Create mock controller with minimal setup
			controller := &MemgraphController{
				lastKnownMaster:   "", // Simulate bootstrap scenario
				targetMasterIndex: -1,
				config: &Config{
					AppName:         "memgraph",
					StatefulSetName: "memgraph-ha",
				},
			}

			// Create cluster state with specified state type
			clusterState := &ClusterState{
				StateType: tt.stateType,
				Pods:      make(map[string]*PodInfo),
			}

			// Mock the cluster state methods
			if tt.stateType == OPERATIONAL_STATE {
				clusterState.Pods["memgraph-ha-0"] = &PodInfo{
					Name:         "memgraph-ha-0",
					MemgraphRole: "main",
				}
			} else if tt.stateType == SPLIT_BRAIN_STATE {
				// For SPLIT_BRAIN_STATE: multiple main pods (with some replicas to avoid INITIAL_STATE)
				clusterState.Pods["memgraph-ha-0"] = &PodInfo{
					Name:         "memgraph-ha-0",
					MemgraphRole: "main",
				}
				clusterState.Pods["memgraph-ha-1"] = &PodInfo{
					Name:         "memgraph-ha-1",
					MemgraphRole: "main",
				}
				clusterState.Pods["memgraph-ha-2"] = &PodInfo{
					Name:         "memgraph-ha-2",
					MemgraphRole: "replica",
				}
			} else if tt.stateType == NO_MASTER_STATE {
				clusterState.Pods["memgraph-ha-0"] = &PodInfo{
					Name:         "memgraph-ha-0",
					MemgraphRole: "replica",
				}
				clusterState.Pods["memgraph-ha-1"] = &PodInfo{
					Name:         "memgraph-ha-1",
					MemgraphRole: "replica",
				}
			}

			// Mock IsBootstrapSafe to return expected value
			originalBootstrapSafe := clusterState.BootstrapSafe
			clusterState.BootstrapSafe = tt.bootstrapSafe
			defer func() { clusterState.BootstrapSafe = originalBootstrapSafe }()

			err := controller.performBootstrapValidation(clusterState)

			if tt.expectError {
				if err == nil {
					t.Errorf("Expected error but got none")
				} else if tt.errorContains != "" && !strings.Contains(err.Error(), tt.errorContains) {
					t.Errorf("Error %v does not contain expected text %q", err, tt.errorContains)
				}
			} else {
				if err != nil {
					t.Errorf("Unexpected error: %v", err)
				}
			}
		})
	}
}

func TestMemgraphController_ApplyDeterministicRoles(t *testing.T) {
	controller := &MemgraphController{
		config: &Config{
			AppName:         "memgraph",
			StatefulSetName: "memgraph-ha",
		},
	}

	tests := []struct {
		name              string
		targetMasterIndex int
		expectedMaster    string
		podCount          int
	}{
		{
			name:              "master_index_0",
			targetMasterIndex: 0,
			expectedMaster:    "memgraph-ha-0",
			podCount:          3,
		},
		{
			name:              "master_index_1",
			targetMasterIndex: 1,
			expectedMaster:    "memgraph-ha-1",
			podCount:          3,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clusterState := &ClusterState{
				TargetMasterIndex: tt.targetMasterIndex,
				Pods:              make(map[string]*PodInfo),
			}

			// Add mock pods
			for i := 0; i < tt.podCount; i++ {
				podName := fmt.Sprintf("memgraph-ha-%d", i)
				clusterState.Pods[podName] = &PodInfo{Name: podName}
			}

			controller.applyDeterministicRoles(clusterState)

			if clusterState.CurrentMaster != tt.expectedMaster {
				t.Errorf("CurrentMaster = %s, want %s", clusterState.CurrentMaster, tt.expectedMaster)
			}
		})
	}
}

func TestMemgraphController_LearnExistingTopology(t *testing.T) {
	controller := &MemgraphController{
		config: &Config{
			AppName:         "memgraph",
			StatefulSetName: "memgraph-ha",
		},
	}

	tests := []struct {
		name           string
		mainPods       []string
		expectedMaster string
		shouldWarn     bool
	}{
		{
			name:           "single_master_pod_0",
			mainPods:       []string{"memgraph-ha-0"},
			expectedMaster: "memgraph-ha-0",
			shouldWarn:     false,
		},
		{
			name:           "single_master_pod_1",
			mainPods:       []string{"memgraph-ha-1"},
			expectedMaster: "memgraph-ha-1",
			shouldWarn:     false,
		},
		{
			name:           "multiple_masters_fallback",
			mainPods:       []string{"memgraph-ha-0", "memgraph-ha-1"},
			expectedMaster: "memgraph-ha-0", // Should use fallback
			shouldWarn:     true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clusterState := &ClusterState{
				TargetMasterIndex: 0,
				Pods:              make(map[string]*PodInfo),
			}

			// Set up pods based on test case
			for _, podName := range tt.mainPods {
				clusterState.Pods[podName] = &PodInfo{
					Name:         podName,
					MemgraphRole: "main",
				}
			}

			// Add SYNC replica for testing
			if len(tt.mainPods) == 1 {
				otherPodName := "memgraph-ha-1"
				if tt.mainPods[0] == "memgraph-ha-1" {
					otherPodName = "memgraph-ha-0"
				}
				clusterState.Pods[otherPodName] = &PodInfo{
					Name:          otherPodName,
					MemgraphRole:  "replica",
					IsSyncReplica: true,
				}
			}

			controller.learnExistingTopology(clusterState)

			if clusterState.CurrentMaster != tt.expectedMaster {
				t.Errorf("CurrentMaster = %s, want %s", clusterState.CurrentMaster, tt.expectedMaster)
			}

			// Verify target master index was updated for single master cases
			if len(tt.mainPods) == 1 {
				expectedIndex := controller.config.ExtractPodIndex(tt.expectedMaster)
				if controller.targetMasterIndex != expectedIndex {
					t.Errorf("targetMasterIndex = %d, want %d", controller.targetMasterIndex, expectedIndex)
				}
			}
		})
	}
}

func TestMemgraphController_SelectMasterAfterQuerying(t *testing.T) {
	controller := &MemgraphController{
		config: &Config{
			AppName:         "memgraph",
			StatefulSetName: "memgraph-ha",
		},
	}

	tests := []struct {
		name              string
		stateType         ClusterStateType
		targetMasterIndex int
		expectMethod      string // Which method should be called
	}{
		{
			name:              "initial_state_calls_apply_deterministic",
			stateType:         INITIAL_STATE,
			targetMasterIndex: 0,
			expectMethod:      "applyDeterministicRoles",
		},
		{
			name:              "operational_state_calls_learn_topology",
			stateType:         OPERATIONAL_STATE,
			targetMasterIndex: 1,
			expectMethod:      "learnExistingTopology",
		},
		{
			name:              "mixed_state_calls_enhanced_selection",
			stateType:         MIXED_STATE,
			targetMasterIndex: 0,
			expectMethod:      "enhancedMasterSelection",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clusterState := &ClusterState{
				StateType:         tt.stateType,
				TargetMasterIndex: tt.targetMasterIndex,
				IsBootstrapPhase:  true, // Should be set to false
				Pods:              make(map[string]*PodInfo),
			}

			// Setup minimal pod structure for function to work
			clusterState.Pods["memgraph-ha-0"] = &PodInfo{Name: "memgraph-ha-0"}
			clusterState.Pods["memgraph-ha-1"] = &PodInfo{Name: "memgraph-ha-1"}

			// Call the function (we can't easily mock internal method calls)
			controller.selectMasterAfterQuerying(clusterState)

			// Verify bootstrap phase was cleared - this is the main behavior we can test
			if clusterState.IsBootstrapPhase {
				t.Errorf("IsBootstrapPhase should be false after selection")
			}
		})
	}
}