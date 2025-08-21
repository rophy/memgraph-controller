package controller

import (
	"context"
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
		if podInfo.KubernetesRole != "master" {
			t.Errorf("Pod memgraph-0 role = %s, want master", podInfo.KubernetesRole)
		}
		if podInfo.BoltAddress != "10.0.0.1:7687" {
			t.Errorf("Pod memgraph-0 BoltAddress = %s, want 10.0.0.1:7687", podInfo.BoltAddress)
		}
	} else {
		t.Error("Pod memgraph-0 not found")
	}

	// Check pod2
	if podInfo, exists := clusterState.Pods["memgraph-1"]; exists {
		if podInfo.KubernetesRole != "replica" {
			t.Errorf("Pod memgraph-1 role = %s, want replica", podInfo.KubernetesRole)
		}
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