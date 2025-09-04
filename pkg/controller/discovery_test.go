package controller

import (
	"context"
	"testing"
	"time"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

// mockPodStore implements cache.Store for testing
type mockPodStore struct {
	pods []*v1.Pod
}

func (m *mockPodStore) Add(obj interface{}) error                   { return nil }
func (m *mockPodStore) Update(obj interface{}) error                { return nil }
func (m *mockPodStore) Delete(obj interface{}) error               { return nil }
func (m *mockPodStore) Get(obj interface{}) (item interface{}, exists bool, err error) { return nil, false, nil }
func (m *mockPodStore) GetByKey(key string) (item interface{}, exists bool, err error) { return nil, false, nil }
func (m *mockPodStore) Replace([]interface{}, string) error        { return nil }
func (m *mockPodStore) Resync() error                              { return nil }

func (m *mockPodStore) List() []interface{} {
	result := make([]interface{}, len(m.pods))
	for i, pod := range m.pods {
		result[i] = pod
	}
	return result
}

func (m *mockPodStore) ListKeys() []string { return nil }


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
	
	// For the test, we need to simulate having pods in the cache store
	// Since the real implementation uses a podInformer cache store, let's create a mock one
	podStore := &mockPodStore{pods: []*v1.Pod{pod1, pod2, pod3}}
	controller.cluster = NewMemgraphCluster(podStore, config, testClient)
	err := controller.cluster.Refresh(context.Background())

	if err != nil {
		t.Fatalf("DiscoverPods() failed: %v", err)
	}

	if controller.cluster == nil {
		t.Fatal("DiscoverPods() returned nil cluster state")
	}

	// Should find all 3 pods (including pending one - filtering removed in refactoring)
	if len(controller.cluster.MemgraphNodes) != 3 {
		t.Errorf("Found %d pods, want 3", len(controller.cluster.MemgraphNodes))
	}

	// Check pod1
	if podInfo, exists := controller.cluster.MemgraphNodes["memgraph-0"]; exists {
		if podInfo.GetBoltAddress() != "10.0.0.1:7687" {
			t.Errorf("Pod memgraph-0 BoltAddress = %s, want 10.0.0.1:7687", podInfo.GetBoltAddress())
		}
	} else {
		t.Error("Pod memgraph-0 not found")
	}

	// Check pod2
	if podInfo, exists := controller.cluster.MemgraphNodes["memgraph-1"]; exists {
		if podInfo.GetReplicaName() != "memgraph_1" {
			t.Errorf("Pod memgraph-1 ReplicaName = %s, want memgraph_1", podInfo.GetReplicaName())
		}
	} else {
		t.Error("Pod memgraph-1 not found")
	}

	// CurrentMain field has been removed - target main is now tracked via controller's target main index
}


// Tests for main controller discovery functions


// TestMemgraphController_ApplyDeterministicRoles was removed since the method was simplified
// and integrated into the discoverClusterState logic

// TestMemgraphController_LearnExistingTopology was removed since the method was simplified
// and integrated into the discoverClusterState logic

// TestMemgraphController_SelectMainAfterQuerying was removed since the method was simplified
// and integrated into the discoverClusterState logic
