package controller

import (
	"context"
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
	testClient := NewMemgraphClient(config)
	controller.cluster = NewMemgraphCluster(fakeClient, config, testClient)
	err := controller.cluster.DiscoverPods(context.Background())

	if err != nil {
		t.Fatalf("DiscoverPods() failed: %v", err)
	}

	if controller.cluster == nil {
		t.Fatal("DiscoverPods() returned nil cluster state")
	}

	// Should find 2 running pods, skip the pending one
	if len(controller.cluster.MemgraphNodes) != 2 {
		t.Errorf("Found %d pods, want 2", len(controller.cluster.MemgraphNodes))
	}

	// Check pod1
	if podInfo, exists := controller.cluster.MemgraphNodes["memgraph-0"]; exists {
		if podInfo.BoltAddress != "10.0.0.1:7687" {
			t.Errorf("Pod memgraph-0 BoltAddress = %s, want 10.0.0.1:7687", podInfo.BoltAddress)
		}
	} else {
		t.Error("Pod memgraph-0 not found")
	}

	// Check pod2
	if podInfo, exists := controller.cluster.MemgraphNodes["memgraph-1"]; exists {
		if podInfo.ReplicaName != "memgraph_1" {
			t.Errorf("Pod memgraph-1 ReplicaName = %s, want memgraph_1", podInfo.ReplicaName)
		}
	} else {
		t.Error("Pod memgraph-1 not found")
	}

	// CurrentMain field has been removed - target main is now tracked via controller's target main index
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
	testClient := NewMemgraphClient(config)
	controller.cluster = NewMemgraphCluster(fakeClient, config, testClient)
	err := controller.cluster.GetPodsByLabel(context.Background(), "custom=label")

	if err != nil {
		t.Fatalf("GetPodsByLabel() failed: %v", err)
	}

	if len(controller.cluster.MemgraphNodes) != 1 {
		t.Errorf("Found %d pods, want 1", len(controller.cluster.MemgraphNodes))
	}

	if _, exists := controller.cluster.MemgraphNodes["custom-pod"]; !exists {
		t.Error("Pod custom-pod not found")
	}
}

// Tests for main controller discovery functions


// TestMemgraphController_ApplyDeterministicRoles was removed since the method was simplified
// and integrated into the discoverClusterState logic

// TestMemgraphController_LearnExistingTopology was removed since the method was simplified
// and integrated into the discoverClusterState logic

// TestMemgraphController_SelectMainAfterQuerying was removed since the method was simplified
// and integrated into the discoverClusterState logic
