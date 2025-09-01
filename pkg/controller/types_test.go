package controller

import (
	"testing"
	"time"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestPodState_String(t *testing.T) {
	tests := []struct {
		state    PodState
		expected string
	}{
		{INITIAL, "INITIAL"},
		{MAIN, "MAIN"},
		{REPLICA, "REPLICA"},
		{PodState(999), "UNKNOWN"},
	}

	for _, tt := range tests {
		t.Run(tt.expected, func(t *testing.T) {
			if got := tt.state.String(); got != tt.expected {
				t.Errorf("PodState.String() = %v, want %v", got, tt.expected)
			}
		})
	}
}

func TestNewClusterState(t *testing.T) {
	config := &Config{
		AppName:         "memgraph",
		StatefulSetName: "memgraph-ha",
	}
	cs := NewClusterState(config)
	if cs == nil {
		t.Fatal("NewClusterState() returned nil")
	}
	if cs.MemgraphNodes == nil {
		t.Error("NewClusterState() Pods map is nil")
	}
	if len(cs.MemgraphNodes) != 0 {
		t.Errorf("NewClusterState() Pods map length = %d, want 0", len(cs.MemgraphNodes))
	}
	if cs.CurrentMain != "" {
		t.Errorf("NewClusterState() CurrentMain = %s, want empty", cs.CurrentMain)
	}
	if cs.connectionPool == nil {
		t.Error("NewClusterState() connectionPool is nil")
	}
}

func TestNewMemgraphNode(t *testing.T) {
	now := time.Now()
	startTime := metav1.NewTime(now.Add(-5 * time.Minute))
	creationTime := metav1.NewTime(now.Add(-10 * time.Minute))

	pod := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "memgraph-1",
			CreationTimestamp: creationTime,
			Labels: map[string]string{
				"role": "main",
				"app":  "memgraph",
			},
		},
		Status: v1.PodStatus{
			Phase:     v1.PodRunning,
			PodIP:     "10.0.0.1",
			StartTime: &startTime,
		},
	}

	node := NewMemgraphNode(pod, nil)

	if node.Name != "memgraph-1" {
		t.Errorf("Name = %s, want memgraph-1", node.Name)
	}

	if node.State != INITIAL {
		t.Errorf("State = %s, want INITIAL", node.State)
	}

	// Should use StartTime over CreationTimestamp
	expectedTime := startTime.Time
	if !node.Timestamp.Equal(expectedTime) {
		t.Errorf("Timestamp = %s, want %s", node.Timestamp, expectedTime)
	}

	if node.BoltAddress != "10.0.0.1:7687" {
		t.Errorf("BoltAddress = %s, want 10.0.0.1:7687", node.BoltAddress)
	}

	if node.GetReplicationAddress() != "10.0.0.1:10000" {
		t.Errorf("GetReplicationAddress = %s, want 10.0.0.1:10000", node.GetReplicationAddress())
	}

	if node.ReplicaName != "memgraph_1" {
		t.Errorf("ReplicaName = %s, want memgraph_1", node.ReplicaName)
	}

	if node.MemgraphRole != "" {
		t.Errorf("MemgraphRole = %s, want empty", node.MemgraphRole)
	}

	if len(node.Replicas) != 0 {
		t.Errorf("Replicas length = %d, want 0", len(node.Replicas))
	}
}

func TestNewMemgraphNode_NoStartTime(t *testing.T) {
	now := time.Now()
	creationTime := metav1.NewTime(now.Add(-10 * time.Minute))

	pod := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "memgraph-2",
			CreationTimestamp: creationTime,
		},
		Status: v1.PodStatus{
			Phase: v1.PodRunning,
			PodIP: "10.0.0.2",
			// StartTime is nil
		},
	}

	node := NewMemgraphNode(pod, nil)

	// Should fall back to CreationTimestamp
	expectedTime := creationTime.Time
	if !node.Timestamp.Equal(expectedTime) {
		t.Errorf("Timestamp = %s, want %s", node.Timestamp, expectedTime)
	}

}

func TestNewMemgraphNode_NoPodIP(t *testing.T) {
	pod := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "memgraph-3",
		},
		Status: v1.PodStatus{
			Phase: v1.PodPending,
			// PodIP is empty
		},
	}

	node := NewMemgraphNode(pod, nil)

	if node.BoltAddress != "" {
		t.Errorf("BoltAddress = %s, want empty", node.BoltAddress)
	}
}

func TestConvertPodNameForReplica(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"memgraph-0", "memgraph_0"},
		{"memgraph-1", "memgraph_1"},
		{"my-app-2", "my_app_2"},
		{"simple", "simple"},
		{"multi-dash-name", "multi_dash_name"},
		{"", ""},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			if got := convertPodNameForReplica(tt.input); got != tt.expected {
				t.Errorf("convertPodNameForReplica(%s) = %s, want %s", tt.input, got, tt.expected)
			}
		})
	}
}
