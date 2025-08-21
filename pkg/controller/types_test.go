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
		{MASTER, "MASTER"},
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
	cs := NewClusterState()
	if cs == nil {
		t.Fatal("NewClusterState() returned nil")
	}
	if cs.Pods == nil {
		t.Error("NewClusterState() Pods map is nil")
	}
	if len(cs.Pods) != 0 {
		t.Errorf("NewClusterState() Pods map length = %d, want 0", len(cs.Pods))
	}
	if cs.CurrentMaster != "" {
		t.Errorf("NewClusterState() CurrentMaster = %s, want empty", cs.CurrentMaster)
	}
}

func TestNewPodInfo(t *testing.T) {
	now := time.Now()
	startTime := metav1.NewTime(now.Add(-5 * time.Minute))
	creationTime := metav1.NewTime(now.Add(-10 * time.Minute))

	pod := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "memgraph-1",
			CreationTimestamp: creationTime,
			Labels: map[string]string{
				"role": "master",
				"app":  "memgraph",
			},
		},
		Status: v1.PodStatus{
			Phase:     v1.PodRunning,
			PodIP:     "10.0.0.1",
			StartTime: &startTime,
		},
	}

	podInfo := NewPodInfo(pod, "memgraph-service")

	if podInfo.Name != "memgraph-1" {
		t.Errorf("Name = %s, want memgraph-1", podInfo.Name)
	}

	if podInfo.State != INITIAL {
		t.Errorf("State = %s, want INITIAL", podInfo.State)
	}

	// Should use StartTime over CreationTimestamp
	expectedTime := startTime.Time
	if !podInfo.Timestamp.Equal(expectedTime) {
		t.Errorf("Timestamp = %s, want %s", podInfo.Timestamp, expectedTime)
	}


	if podInfo.BoltAddress != "10.0.0.1:7687" {
		t.Errorf("BoltAddress = %s, want 10.0.0.1:7687", podInfo.BoltAddress)
	}

	if podInfo.ReplicationAddress != "memgraph-1.memgraph-service:10000" {
		t.Errorf("ReplicationAddress = %s, want memgraph-1.memgraph-service:10000", podInfo.ReplicationAddress)
	}

	if podInfo.ReplicaName != "memgraph_1" {
		t.Errorf("ReplicaName = %s, want memgraph_1", podInfo.ReplicaName)
	}

	if podInfo.MemgraphRole != "" {
		t.Errorf("MemgraphRole = %s, want empty", podInfo.MemgraphRole)
	}

	if len(podInfo.Replicas) != 0 {
		t.Errorf("Replicas length = %d, want 0", len(podInfo.Replicas))
	}
}

func TestNewPodInfo_NoStartTime(t *testing.T) {
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

	podInfo := NewPodInfo(pod, "memgraph-service")

	// Should fall back to CreationTimestamp
	expectedTime := creationTime.Time
	if !podInfo.Timestamp.Equal(expectedTime) {
		t.Errorf("Timestamp = %s, want %s", podInfo.Timestamp, expectedTime)
	}

}

func TestNewPodInfo_NoPodIP(t *testing.T) {
	pod := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "memgraph-3",
		},
		Status: v1.PodStatus{
			Phase: v1.PodPending,
			// PodIP is empty
		},
	}

	podInfo := NewPodInfo(pod, "memgraph-service")

	if podInfo.BoltAddress != "" {
		t.Errorf("BoltAddress = %s, want empty", podInfo.BoltAddress)
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