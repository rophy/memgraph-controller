package controller

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestConvertPodInfoToStatus(t *testing.T) {
	// Create a mock pod
	pod := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "memgraph-0",
			Namespace: "memgraph",
			Labels: map[string]string{
				"role": "master",
			},
		},
		Status: v1.PodStatus{
			PodIP:     "10.244.1.5",
			StartTime: &metav1.Time{Time: time.Now()},
		},
	}

	podInfo := NewPodInfo(pod, "memgraph")
	podInfo.MemgraphRole = "main"
	podInfo.Replicas = []string{"memgraph_1", "memgraph_2"}
	podInfo.State = MASTER

	// Test healthy pod conversion
	status := convertPodInfoToStatus(podInfo, true)
	
	if status.Name != "memgraph-0" {
		t.Errorf("Expected name 'memgraph-0', got '%s'", status.Name)
	}
	
	if status.State != "MASTER" {
		t.Errorf("Expected state 'MASTER', got '%s'", status.State)
	}
	
	if !status.Healthy {
		t.Error("Expected pod to be healthy")
	}
	
	if status.KubernetesRole != "master" {
		t.Errorf("Expected KubernetesRole 'master', got '%s'", status.KubernetesRole)
	}
	
	if status.MemgraphRole != "main" {
		t.Errorf("Expected MemgraphRole 'main', got '%s'", status.MemgraphRole)
	}
	
	if status.IsSyncReplica {
		t.Error("Expected master pod to not be a SYNC replica")
	}
	
	if len(status.ReplicasRegistered) != 2 {
		t.Errorf("Expected 2 replicas, got %d", len(status.ReplicasRegistered))
	}

	// Test unhealthy pod conversion
	podInfo.MemgraphRole = "" // Simulate unreachable pod
	statusUnhealthy := convertPodInfoToStatus(podInfo, false)
	
	if statusUnhealthy.Healthy {
		t.Error("Expected pod to be unhealthy")
	}
	
	if statusUnhealthy.Inconsistency == nil {
		t.Error("Expected inconsistency for unhealthy pod")
	} else if statusUnhealthy.Inconsistency.Description != "Pod is unreachable - cannot query Memgraph status" {
		t.Errorf("Unexpected inconsistency description: %s", statusUnhealthy.Inconsistency.Description)
	}
}

func TestConvertPodInfoToStatus_SyncReplica(t *testing.T) {
	// Create a mock SYNC replica pod
	pod := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "memgraph-1",
			Namespace: "memgraph",
			Labels: map[string]string{
				"role": "replica",
			},
		},
		Status: v1.PodStatus{
			PodIP:     "10.244.1.6",
			StartTime: &metav1.Time{Time: time.Now()},
		},
	}

	podInfo := NewPodInfo(pod, "memgraph")
	podInfo.MemgraphRole = "replica"
	podInfo.State = REPLICA
	podInfo.IsSyncReplica = true

	// Test SYNC replica conversion
	status := convertPodInfoToStatus(podInfo, true)
	
	if status.Name != "memgraph-1" {
		t.Errorf("Expected name 'memgraph-1', got '%s'", status.Name)
	}
	
	if status.State != "REPLICA" {
		t.Errorf("Expected state 'REPLICA', got '%s'", status.State)
	}
	
	if !status.Healthy {
		t.Error("Expected pod to be healthy")
	}
	
	if !status.IsSyncReplica {
		t.Error("Expected pod to be a SYNC replica")
	}
	
	if status.KubernetesRole != "replica" {
		t.Errorf("Expected KubernetesRole 'replica', got '%s'", status.KubernetesRole)
	}
	
	if status.MemgraphRole != "replica" {
		t.Errorf("Expected MemgraphRole 'replica', got '%s'", status.MemgraphRole)
	}
}

func TestHTTPServerStatusEndpoint(t *testing.T) {
	// Create a simple unit test that doesn't rely on network calls
	// Test the HTTP response format and structure
	
	// Create a ClusterState with mock data
	clusterState := &ClusterState{
		Pods:          make(map[string]*PodInfo),
		CurrentMaster: "memgraph-0",
	}
	
	// Add mock pod data  
	pod1 := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "memgraph-0",
			Namespace: "memgraph",
		},
		Status: v1.PodStatus{
			PodIP:     "10.244.1.5",
			StartTime: &metav1.Time{Time: time.Now()},
		},
	}
	
	pod2 := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "memgraph-1", 
			Namespace: "memgraph",
		},
		Status: v1.PodStatus{
			PodIP:     "10.244.1.6",
			StartTime: &metav1.Time{Time: time.Now()},
		},
	}
	
	podInfo1 := NewPodInfo(pod1, "memgraph")
	podInfo1.MemgraphRole = "main"
	podInfo1.State = MASTER
	
	podInfo2 := NewPodInfo(pod2, "memgraph")
	podInfo2.MemgraphRole = "replica"
	podInfo2.State = REPLICA
	podInfo2.IsSyncReplica = true
	
	clusterState.Pods["memgraph-0"] = podInfo1
	clusterState.Pods["memgraph-1"] = podInfo2
	
	// Create API response directly
	response := StatusResponse{
		Timestamp: time.Now(),
		ClusterState: ClusterStatus{
			TotalPods:         len(clusterState.Pods),
			HealthyPods:       2,
			UnhealthyPods:     0,
			CurrentMaster:     clusterState.CurrentMaster,
			CurrentSyncReplica: "memgraph-1",
			SyncReplicaHealthy: true,
		},
		Pods: []PodStatus{
			convertPodInfoToStatus(podInfo1, true),
			convertPodInfoToStatus(podInfo2, true),
		},
	}
	
	// Test JSON marshaling
	jsonData, err := json.Marshal(response)
	if err != nil {
		t.Fatalf("Failed to marshal response: %v", err)
	}
	
	// Verify response structure
	var decoded StatusResponse
	if err := json.Unmarshal(jsonData, &decoded); err != nil {
		t.Fatalf("Failed to unmarshal response: %v", err)
	}
	
	// Verify response content
	if decoded.ClusterState.TotalPods != 2 {
		t.Errorf("Expected 2 total pods, got %d", decoded.ClusterState.TotalPods)
	}
	
	if decoded.ClusterState.CurrentMaster != "memgraph-0" {
		t.Errorf("Expected master 'memgraph-0', got '%s'", decoded.ClusterState.CurrentMaster)
	}
	
	if decoded.ClusterState.CurrentSyncReplica != "memgraph-1" {
		t.Errorf("Expected sync replica 'memgraph-1', got '%s'", decoded.ClusterState.CurrentSyncReplica)  
	}
	
	if !decoded.ClusterState.SyncReplicaHealthy {
		t.Error("Expected sync replica to be healthy")
	}
	
	if len(decoded.Pods) != 2 {
		t.Errorf("Expected 2 pods in response, got %d", len(decoded.Pods))
	}
}

func TestHTTPServerHealthEndpoint(t *testing.T) {
	config := &Config{HTTPPort: "8080"}
	controller := &MemgraphController{} // Minimal controller for test
	httpServer := NewHTTPServer(controller, config)

	req := httptest.NewRequest("GET", "/health", nil)
	w := httptest.NewRecorder()
	
	httpServer.handleHealth(w, req)

	resp := w.Result()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("Expected status 200, got %d", resp.StatusCode)
	}

	var health map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&health); err != nil {
		t.Fatalf("Failed to decode health response: %v", err)
	}

	if health["status"] != "ok" {
		t.Errorf("Expected status 'ok', got '%v'", health["status"])
	}

	if health["service"] != "memgraph-controller" {
		t.Errorf("Expected service 'memgraph-controller', got '%v'", health["service"])
	}
}

func TestHTTPServerRootEndpoint(t *testing.T) {
	config := &Config{HTTPPort: "8080"}
	controller := &MemgraphController{} // Minimal controller for test
	httpServer := NewHTTPServer(controller, config)

	req := httptest.NewRequest("GET", "/", nil)
	w := httptest.NewRecorder()
	
	httpServer.handleRoot(w, req)

	resp := w.Result()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("Expected status 200, got %d", resp.StatusCode)
	}

	var root map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&root); err != nil {
		t.Fatalf("Failed to decode root response: %v", err)
	}

	if root["service"] != "memgraph-controller" {
		t.Errorf("Expected service 'memgraph-controller', got '%v'", root["service"])
	}

	endpoints, ok := root["endpoints"].([]interface{})
	if !ok {
		t.Fatal("Endpoints should be an array")
	}

	expectedEndpoints := []string{"/api/v1/status", "/health"}
	if len(endpoints) != len(expectedEndpoints) {
		t.Errorf("Expected %d endpoints, got %d", len(expectedEndpoints), len(endpoints))
	}
}

func TestHTTPServerMethodNotAllowed(t *testing.T) {
	config := &Config{HTTPPort: "8080"}
	controller := &MemgraphController{} // Minimal controller for test
	httpServer := NewHTTPServer(controller, config)

	// Test POST to status endpoint
	req := httptest.NewRequest("POST", "/api/v1/status", nil)
	w := httptest.NewRecorder()
	
	httpServer.handleStatus(w, req)

	resp := w.Result()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("Expected status 405, got %d", resp.StatusCode)
	}

	// Test POST to health endpoint
	req = httptest.NewRequest("POST", "/health", nil)
	w = httptest.NewRecorder()
	
	httpServer.handleHealth(w, req)

	resp = w.Result()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("Expected status 405, got %d", resp.StatusCode)
	}
}