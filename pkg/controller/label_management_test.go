package controller

import (
	"context"
	"errors"
	"testing"
	"time"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"
)

func TestPodDiscovery_UpdatePodLabel(t *testing.T) {
	tests := []struct {
		name          string
		podName       string
		labelKey      string
		labelValue    string
		createPod     bool
		expectedError bool
	}{
		{
			name:          "valid label update",
			podName:       "memgraph-0",
			labelKey:      "role",
			labelValue:    "master",
			createPod:     true,
			expectedError: false,
		},
		{
			name:          "empty pod name",
			podName:       "",
			labelKey:      "role",
			labelValue:    "master",
			createPod:     false,
			expectedError: true,
		},
		{
			name:          "empty label key",
			podName:       "memgraph-0",
			labelKey:      "",
			labelValue:    "master",
			createPod:     true,
			expectedError: true,
		},
		{
			name:          "pod does not exist",
			podName:       "memgraph-missing",
			labelKey:      "role",
			labelValue:    "master",
			createPod:     false,
			expectedError: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var objects []runtime.Object
			
			if tt.createPod {
				pod := &v1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name:      tt.podName,
						Namespace: "test",
					},
				}
				objects = append(objects, pod)
			}

			clientset := fake.NewSimpleClientset(objects...)
			config := &Config{
				AppName:   "memgraph",
				Namespace: "test",
			}
			
			pd := NewPodDiscovery(clientset, config)
			ctx := context.Background()

			err := pd.UpdatePodLabel(ctx, tt.podName, tt.labelKey, tt.labelValue)
			
			if tt.expectedError && err == nil {
				t.Error("Expected error but got none")
			}
			if !tt.expectedError && err != nil {
				t.Errorf("Unexpected error: %v", err)
			}
		})
	}
}

func TestPodDiscovery_UpdatePodRoleLabel(t *testing.T) {
	tests := []struct {
		name          string
		podName       string
		role          PodState
		expectedError bool
		expectRemove  bool
	}{
		{
			name:          "set master role",
			podName:       "memgraph-0",
			role:          MASTER,
			expectedError: false,
			expectRemove:  false,
		},
		{
			name:          "set replica role",
			podName:       "memgraph-1",
			role:          REPLICA,
			expectedError: false,
			expectRemove:  false,
		},
		{
			name:          "set initial state (remove label)",
			podName:       "memgraph-2",
			role:          INITIAL,
			expectedError: false,
			expectRemove:  true,
		},
		{
			name:          "empty pod name",
			podName:       "",
			role:          MASTER,
			expectedError: true,
			expectRemove:  false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Create a pod with existing label for remove test
			pod := &v1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      tt.podName,
					Namespace: "test",
					Labels: map[string]string{
						"role": "replica",
					},
				},
			}

			clientset := fake.NewSimpleClientset(pod)
			config := &Config{
				AppName:   "memgraph",
				Namespace: "test",
			}
			
			pd := NewPodDiscovery(clientset, config)
			ctx := context.Background()

			err := pd.UpdatePodRoleLabel(ctx, tt.podName, tt.role)
			
			if tt.expectedError && err == nil {
				t.Error("Expected error but got none")
			}
			if !tt.expectedError && err != nil {
				t.Errorf("Unexpected error: %v", err)
			}
		})
	}
}

func TestPodDiscovery_RemovePodLabel(t *testing.T) {
	tests := []struct {
		name          string
		podName       string
		labelKey      string
		podExists     bool
		labelExists   bool
		expectedError bool
	}{
		{
			name:          "remove existing label",
			podName:       "memgraph-0",
			labelKey:      "role",
			podExists:     true,
			labelExists:   true,
			expectedError: false,
		},
		{
			name:          "remove non-existent label",
			podName:       "memgraph-0",
			labelKey:      "role",
			podExists:     true,
			labelExists:   false,
			expectedError: false,
		},
		{
			name:          "pod does not exist",
			podName:       "memgraph-missing",
			labelKey:      "role",
			podExists:     false,
			labelExists:   false,
			expectedError: true,
		},
		{
			name:          "empty pod name",
			podName:       "",
			labelKey:      "role",
			podExists:     false,
			labelExists:   false,
			expectedError: true,
		},
		{
			name:          "empty label key",
			podName:       "memgraph-0",
			labelKey:      "",
			podExists:     true,
			labelExists:   false,
			expectedError: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var objects []runtime.Object
			
			if tt.podExists {
				labels := make(map[string]string)
				if tt.labelExists {
					labels[tt.labelKey] = "master"
				}
				
				pod := &v1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name:      tt.podName,
						Namespace: "test",
						Labels:    labels,
					},
				}
				objects = append(objects, pod)
			}

			clientset := fake.NewSimpleClientset(objects...)
			config := &Config{
				AppName:   "memgraph",
				Namespace: "test",
			}
			
			pd := NewPodDiscovery(clientset, config)
			ctx := context.Background()

			err := pd.RemovePodLabel(ctx, tt.podName, tt.labelKey)
			
			if tt.expectedError && err == nil {
				t.Error("Expected error but got none")
			}
			if !tt.expectedError && err != nil {
				t.Errorf("Unexpected error: %v", err)
			}
		})
	}
}

func TestPodDiscovery_ValidatePodLabels(t *testing.T) {
	config := &Config{
		AppName:     "memgraph",
		Namespace:   "test",
		ServiceName: "memgraph",
	}

	clientset := fake.NewSimpleClientset()
	pd := NewPodDiscovery(clientset, config)
	ctx := context.Background()

	// Create cluster state with inconsistent labels
	clusterState := &ClusterState{
		Pods: map[string]*PodInfo{
			"memgraph-0": {
				Name:           "memgraph-0",
				State:          MASTER,
				KubernetesRole: "", // Should be "master"
				MemgraphRole:   "MAIN",
			},
			"memgraph-1": {
				Name:           "memgraph-1",
				State:          REPLICA,
				KubernetesRole: "master", // Should be "replica"
				MemgraphRole:   "REPLICA",
			},
			"memgraph-2": {
				Name:           "memgraph-2",
				State:          INITIAL,
				KubernetesRole: "replica", // Should be ""
				MemgraphRole:   "MAIN",
			},
		},
		CurrentMaster: "memgraph-0",
	}

	inconsistencies, err := pd.ValidatePodLabels(ctx, clusterState)
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}

	expectedInconsistencies := 3
	if len(inconsistencies) != expectedInconsistencies {
		t.Errorf("Expected %d inconsistencies, got %d", expectedInconsistencies, len(inconsistencies))
	}

	// Verify specific inconsistencies
	inconsistencyMap := make(map[string]LabelInconsistency)
	for _, inc := range inconsistencies {
		inconsistencyMap[inc.PodName] = inc
	}

	if inc, exists := inconsistencyMap["memgraph-0"]; exists {
		if inc.CurrentLabel != "" || inc.ExpectedLabel != "master" {
			t.Errorf("Wrong inconsistency for memgraph-0: current=%s, expected=%s", 
				inc.CurrentLabel, inc.ExpectedLabel)
		}
	} else {
		t.Error("Missing inconsistency for memgraph-0")
	}

	if inc, exists := inconsistencyMap["memgraph-1"]; exists {
		if inc.CurrentLabel != "master" || inc.ExpectedLabel != "replica" {
			t.Errorf("Wrong inconsistency for memgraph-1: current=%s, expected=%s", 
				inc.CurrentLabel, inc.ExpectedLabel)
		}
	} else {
		t.Error("Missing inconsistency for memgraph-1")
	}

	if inc, exists := inconsistencyMap["memgraph-2"]; exists {
		if inc.CurrentLabel != "replica" || inc.ExpectedLabel != "" {
			t.Errorf("Wrong inconsistency for memgraph-2: current=%s, expected=%s", 
				inc.CurrentLabel, inc.ExpectedLabel)
		}
	} else {
		t.Error("Missing inconsistency for memgraph-2")
	}
}

func TestPodDiscovery_BatchUpdatePodLabels(t *testing.T) {
	// Create pods for testing
	pod1 := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "memgraph-0",
			Namespace: "test",
			Labels:    map[string]string{"app": "memgraph"},
		},
	}
	
	pod2 := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "memgraph-1",
			Namespace: "test",
			Labels:    map[string]string{"role": "master", "app": "memgraph"},
		},
	}

	clientset := fake.NewSimpleClientset(pod1, pod2)
	config := &Config{
		AppName:   "memgraph",
		Namespace: "test",
	}
	
	pd := NewPodDiscovery(clientset, config)
	ctx := context.Background()

	updates := []PodLabelUpdate{
		{
			PodName:       "memgraph-0",
			RoleValue:     "master",
			OriginalValue: "",
			Remove:        false,
		},
		{
			PodName:       "memgraph-1",
			RoleValue:     "replica",
			OriginalValue: "master",
			Remove:        false,
		},
	}

	err := pd.BatchUpdatePodLabels(ctx, updates)
	if err != nil {
		t.Errorf("Unexpected error: %v", err)
	}

	// Verify that patch operations were called
	actions := clientset.Actions()
	patchCount := 0
	for _, action := range actions {
		if action.GetVerb() == "patch" {
			patchCount++
		}
	}

	expectedPatches := 2
	if patchCount != expectedPatches {
		t.Errorf("Expected %d patch operations, got %d", expectedPatches, patchCount)
	}
}

func TestPodDiscovery_BatchUpdatePodLabels_WithFailure(t *testing.T) {
	clientset := fake.NewSimpleClientset()
	
	// Make patch operations fail
	clientset.PrependReactor("patch", "pods", func(action ktesting.Action) (handled bool, ret runtime.Object, err error) {
		return true, nil, errors.New("simulated failure")
	})

	config := &Config{
		AppName:   "memgraph",
		Namespace: "test",
	}
	
	pd := NewPodDiscovery(clientset, config)
	ctx := context.Background()

	updates := []PodLabelUpdate{
		{
			PodName:   "memgraph-0",
			RoleValue: "master",
			Remove:    false,
		},
	}

	err := pd.BatchUpdatePodLabels(ctx, updates)
	if err == nil {
		t.Error("Expected error due to simulated failure")
	}
}

func TestPodDiscovery_SyncPodLabelsWithState(t *testing.T) {
	// Create pods with inconsistent labels
	pod1 := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "memgraph-0",
			Namespace: "test",
			Labels:    map[string]string{"app": "memgraph"}, // Should have role=master
		},
	}
	
	pod2 := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "memgraph-1",
			Namespace: "test",
			Labels:    map[string]string{"role": "master", "app": "memgraph"}, // Should have role=replica
		},
	}

	clientset := fake.NewSimpleClientset(pod1, pod2)
	config := &Config{
		AppName:   "memgraph",
		Namespace: "test",
	}
	
	pd := NewPodDiscovery(clientset, config)
	ctx := context.Background()

	clusterState := &ClusterState{
		Pods: map[string]*PodInfo{
			"memgraph-0": {
				Name:           "memgraph-0",
				State:          MASTER,
				KubernetesRole: "",
				MemgraphRole:   "MAIN",
			},
			"memgraph-1": {
				Name:           "memgraph-1",
				State:          REPLICA,
				KubernetesRole: "master",
				MemgraphRole:   "REPLICA",
			},
		},
		CurrentMaster: "memgraph-0",
	}

	err := pd.SyncPodLabelsWithState(ctx, clusterState)
	if err != nil {
		t.Errorf("Unexpected error: %v", err)
	}

	// Verify that patch operations were called to fix inconsistencies
	actions := clientset.Actions()
	patchCount := 0
	for _, action := range actions {
		if action.GetVerb() == "patch" {
			patchCount++
		}
	}

	expectedPatches := 2
	if patchCount != expectedPatches {
		t.Errorf("Expected %d patch operations, got %d", expectedPatches, patchCount)
	}
}

func TestPodDiscovery_SyncPodLabelsWithState_NoInconsistencies(t *testing.T) {
	clientset := fake.NewSimpleClientset()
	config := &Config{
		AppName:   "memgraph",
		Namespace: "test",
	}
	
	pd := NewPodDiscovery(clientset, config)
	ctx := context.Background()

	// Create cluster state with consistent labels
	clusterState := &ClusterState{
		Pods: map[string]*PodInfo{
			"memgraph-0": {
				Name:           "memgraph-0",
				State:          MASTER,
				KubernetesRole: "master",
				MemgraphRole:   "MAIN",
			},
			"memgraph-1": {
				Name:           "memgraph-1",
				State:          REPLICA,
				KubernetesRole: "replica",
				MemgraphRole:   "REPLICA",
			},
		},
		CurrentMaster: "memgraph-0",
	}

	err := pd.SyncPodLabelsWithState(ctx, clusterState)
	if err != nil {
		t.Errorf("Unexpected error: %v", err)
	}

	// Verify that no patch operations were called since labels are consistent
	actions := clientset.Actions()
	patchCount := 0
	for _, action := range actions {
		if action.GetVerb() == "patch" {
			patchCount++
		}
	}

	expectedPatches := 0
	if patchCount != expectedPatches {
		t.Errorf("Expected %d patch operations, got %d", expectedPatches, patchCount)
	}
}

func TestMemgraphController_SyncPodLabels(t *testing.T) {
	clientset := fake.NewSimpleClientset()
	config := &Config{
		AppName:            "memgraph",
		Namespace:          "test",
		ReconcileInterval:  30 * time.Second,
		ServiceName:        "memgraph",
	}

	controller := NewMemgraphController(clientset, config)
	ctx := context.Background()

	clusterState := &ClusterState{
		Pods: map[string]*PodInfo{
			"memgraph-0": {
				Name:           "memgraph-0",
				State:          INITIAL,
				KubernetesRole: "",
				MemgraphRole:   "MAIN",
				Replicas:       []string{},
			},
		},
		CurrentMaster: "memgraph-0",
	}

	err := controller.SyncPodLabels(ctx, clusterState)
	if err != nil {
		t.Errorf("Unexpected error: %v", err)
	}

	// Verify that the pod state was reclassified
	pod := clusterState.Pods["memgraph-0"]
	if pod.State != INITIAL {
		t.Errorf("Expected pod state to remain INITIAL, got %s", pod.State)
	}
}

func TestGetExpectedRoleLabel(t *testing.T) {
	config := &Config{
		AppName:   "memgraph",
		Namespace: "test",
	}

	clientset := fake.NewSimpleClientset()
	pd := NewPodDiscovery(clientset, config)

	tests := []struct {
		name           string
		state          PodState
		expectedLabel  string
	}{
		{
			name:          "MASTER state",
			state:         MASTER,
			expectedLabel: "master",
		},
		{
			name:          "REPLICA state",
			state:         REPLICA,
			expectedLabel: "replica",
		},
		{
			name:          "INITIAL state",
			state:         INITIAL,
			expectedLabel: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			podInfo := &PodInfo{State: tt.state}
			result := pd.getExpectedRoleLabel(podInfo, "memgraph-0")
			
			if result != tt.expectedLabel {
				t.Errorf("Expected label %q for state %s, got %q", 
					tt.expectedLabel, tt.state, result)
			}
		})
	}
}