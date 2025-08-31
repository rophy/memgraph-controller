package controller

import (
	"context"
	"fmt"
	"testing"
	"time"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)


func TestMemgraphController_DiscoverPods(t *testing.T) {
	now := time.Now()

	// Create test pods
	pod1 := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "memgraph-0",
			Namespace: "memgraph",
			Labels: map[string]string{
				"app.kubernetes.io/name": "memgraph",
				"role":                   "main",
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
				"role":                   "replica",
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
		AppName:   "memgraph",
		Namespace: "memgraph",
	}

	controller := &MemgraphController{
		clientset: fakeClient,
		config:    config,
	}
	controller.cluster = NewMemgraphCluster(fakeClient, config, nil, nil)
	err := controller.cluster.DiscoverPods(context.Background())

	if err != nil {
		t.Fatalf("DiscoverPods() failed: %v", err)
	}

	if controller.cluster == nil {
		t.Fatal("DiscoverPods() returned nil cluster state")
	}

	// Should find 2 running pods, skip the pending one
	if len(controller.cluster.Pods) != 2 {
		t.Errorf("Found %d pods, want 2", len(controller.cluster.Pods))
	}

	// Check pod1
	if podInfo, exists := controller.cluster.Pods["memgraph-0"]; exists {
		if podInfo.BoltAddress != "10.0.0.1:7687" {
			t.Errorf("Pod memgraph-0 BoltAddress = %s, want 10.0.0.1:7687", podInfo.BoltAddress)
		}
	} else {
		t.Error("Pod memgraph-0 not found")
	}

	// Check pod2
	if podInfo, exists := controller.cluster.Pods["memgraph-1"]; exists {
		if podInfo.ReplicaName != "memgraph_1" {
			t.Errorf("Pod memgraph-1 ReplicaName = %s, want memgraph_1", podInfo.ReplicaName)
		}
	} else {
		t.Error("Pod memgraph-1 not found")
	}

	// After discovery, main selection is deferred until after Memgraph querying
	if controller.cluster.CurrentMain != "" {
		t.Errorf("CurrentMain = %s, want empty (deferred until after Memgraph querying)", controller.cluster.CurrentMain)
	}
}

func TestMemgraphController_GetPodsByLabel(t *testing.T) {
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
		Namespace: "memgraph",
	}

	controller := &MemgraphController{
		clientset: fakeClient,
		config:    config,
	}
	controller.cluster = NewMemgraphCluster(fakeClient, config, nil, nil)
	err := controller.cluster.GetPodsByLabel(context.Background(), "custom=label")

	if err != nil {
		t.Fatalf("GetPodsByLabel() failed: %v", err)
	}

	if len(controller.cluster.Pods) != 1 {
		t.Errorf("Found %d pods, want 1", len(controller.cluster.Pods))
	}

	if _, exists := controller.cluster.Pods["custom-pod"]; !exists {
		t.Error("Pod custom-pod not found")
	}
}

// Tests for main controller discovery functions


func TestMemgraphController_ApplyDeterministicRoles(t *testing.T) {
	controller := &MemgraphController{
		config: &Config{
			AppName:         "memgraph",
			StatefulSetName: "memgraph-ha",
		},
	}

	tests := []struct {
		name            string
		targetMainIndex int
		expectedMain    string
		podCount        int
	}{
		{
			name:            "main_index_0",
			targetMainIndex: 0,
			expectedMain:    "memgraph-ha-0",
			podCount:        3,
		},
		{
			name:            "main_index_1",
			targetMainIndex: 1,
			expectedMain:    "memgraph-ha-1",
			podCount:        3,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Create a mock state manager with the expected state
			mockStateManager := NewMockStateManager(tt.targetMainIndex)
			controller.cluster = NewMemgraphCluster(nil, controller.config, nil, mockStateManager)

			// Add mock pods
			for i := 0; i < tt.podCount; i++ {
				podName := fmt.Sprintf("memgraph-ha-%d", i)
				controller.cluster.Pods[podName] = &PodInfo{Name: podName}
			}

			controller.applyDeterministicRoles(controller.cluster)

			if controller.cluster.CurrentMain != tt.expectedMain {
				t.Errorf("CurrentMain = %s, want %s", controller.cluster.CurrentMain, tt.expectedMain)
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
		name         string
		mainPods     []string
		expectedMain string
		shouldWarn   bool
	}{
		{
			name:         "single_main_pod_0",
			mainPods:     []string{"memgraph-ha-0"},
			expectedMain: "memgraph-ha-0",
			shouldWarn:   false,
		},
		{
			name:         "single_main_pod_1",
			mainPods:     []string{"memgraph-ha-1"},
			expectedMain: "memgraph-ha-1",
			shouldWarn:   false,
		},
		{
			name:         "multiple_mains_fallback",
			mainPods:     []string{"memgraph-ha-0", "memgraph-ha-1"},
			expectedMain: "memgraph-ha-0", // Should use fallback
			shouldWarn:   true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Reset the state manager for each test to ensure predictable behavior
			var mockStateManager StateManagerInterface
			if tt.name == "multiple_mains_fallback" {
				// For multiple mains case, set expected fallback to index 0
				mockStateManager = NewMockStateManager(0)
			} else {
				mockStateManager = NewEmptyMockStateManager()
			}
			controller.cluster = NewMemgraphCluster(nil, controller.config, nil, mockStateManager)

			// Set up pods based on test case
			for _, podName := range tt.mainPods {
				controller.cluster.Pods[podName] = &PodInfo{
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
				controller.cluster.Pods[otherPodName] = &PodInfo{
					Name:          otherPodName,
					MemgraphRole:  "replica",
					IsSyncReplica: true,
				}
			}

			controller.learnExistingTopology(controller.cluster)

			if controller.cluster.CurrentMain != tt.expectedMain {
				t.Errorf("CurrentMain = %s, want %s", controller.cluster.CurrentMain, tt.expectedMain)
			}

			// Verify target main index was updated for single main cases
			if len(tt.mainPods) == 1 {
				expectedIndex := controller.config.ExtractPodIndex(tt.expectedMain)
				actualIndex := controller.getTargetMainIndex()
				if actualIndex != expectedIndex {
					t.Errorf("targetMainIndex = %d, want %d", actualIndex, expectedIndex)
				}
			}
		})
	}
}

func TestMemgraphController_SelectMainAfterQuerying(t *testing.T) {
	controller := &MemgraphController{
		config: &Config{
			AppName:         "memgraph",
			StatefulSetName: "memgraph-ha",
		},
	}

	tests := []struct {
		name            string
		stateType       ClusterStateType
		targetMainIndex int
		expectMethod    string // Which method should be called
	}{
		{
			name:            "initial_state_calls_apply_deterministic",
			stateType:       INITIAL_STATE,
			targetMainIndex: 0,
			expectMethod:    "applyDeterministicRoles",
		},
		{
			name:            "operational_state_calls_learn_topology",
			stateType:       OPERATIONAL_STATE,
			targetMainIndex: 1,
			expectMethod:    "learnExistingTopology",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mockStateManager := NewMockStateManager(tt.targetMainIndex)
			controller.cluster = NewMemgraphCluster(nil, controller.config, nil, mockStateManager)
			
			// Setup test state
			controller.cluster.StateType = tt.stateType
			controller.cluster.IsBootstrapPhase = true // Should be set to false

			// Setup minimal pod structure for function to work
			controller.cluster.Pods["memgraph-ha-0"] = &PodInfo{Name: "memgraph-ha-0"}
			controller.cluster.Pods["memgraph-ha-1"] = &PodInfo{Name: "memgraph-ha-1"}

			// Call the function (we can't easily mock internal method calls)
			controller.selectMainAfterQuerying(context.Background(), controller.cluster)

			// Verify bootstrap phase was cleared - this is the main behavior we can test
			if controller.cluster.IsBootstrapPhase {
				t.Errorf("IsBootstrapPhase should be false after selection")
			}
		})
	}
}
