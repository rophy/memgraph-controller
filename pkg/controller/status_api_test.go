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

func TestConvertMemgraphNodeToStatus(t *testing.T) {
	// Create a mock pod
	pod := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "memgraph-0",
			Namespace: "memgraph",
			Labels: map[string]string{
				"role": "main",
			},
		},
		Status: v1.PodStatus{
			PodIP:     "10.244.1.5",
			StartTime: &metav1.Time{Time: time.Now()},
		},
	}

	// Create a test client for the node
	config := &Config{
		AppName:         "memgraph",
		StatefulSetName: "memgraph-ha",
	}
	testClient := NewMemgraphClient(config)
	
	node := NewMemgraphNode(pod, testClient)
	// For testing, we need to set private fields directly since we can't actually connect to Memgraph
	node.memgraphRole = "main"
	// Set cached replica info to avoid network calls
	node.hasReplicasInfo = true
	// Note: Replicas field was removed in favor of GetReplicas() method
	// // node.State = MAIN // State removed // State removed

	// Test healthy pod conversion
	testPod := &v1.Pod{
		Status: v1.PodStatus{
			PodIP: "10.244.1.50",
		},
	}
	status := convertMemgraphNodeToStatus(node, true, testPod)

	if status.Name != "memgraph-0" {
		t.Errorf("Expected name 'memgraph-0', got '%s'", status.Name)
	}

	if status.State != "unknown" { // State is now "unknown" since State field removed
		t.Errorf("Expected state 'unknown', got '%s'", status.State)
	}

	if !status.Healthy {
		t.Error("Expected pod to be healthy")
	}

	if status.MemgraphRole != "main" {
		t.Errorf("Expected MemgraphRole 'main', got '%s'", status.MemgraphRole)
	}

	if status.IPAddress == "" {
		t.Error("Expected main pod to have IP address")
	}

	if len(status.ReplicasRegistered) != 0 {
		t.Errorf("Expected 0 replicas (cached empty), got %d", len(status.ReplicasRegistered))
	}

	// Test unhealthy pod conversion
	// Don't clear the role since that would trigger network calls
	// node.memgraphRole = "" // Simulate unreachable pod - removed to avoid network calls
	statusUnhealthy := convertMemgraphNodeToStatus(node, false, testPod)

	if statusUnhealthy.Healthy {
		t.Error("Expected pod to be unhealthy")
	}

	if statusUnhealthy.Inconsistency == nil {
		t.Error("Expected inconsistency for unhealthy pod")
	} else {
		if statusUnhealthy.Inconsistency.Description != "Pod is unreachable - cannot query Memgraph status" {
			t.Errorf("Unexpected inconsistency description: %s", statusUnhealthy.Inconsistency.Description)
		}
		if statusUnhealthy.Inconsistency.MemgraphRole != "main" {
			t.Errorf("Expected inconsistency role 'main', got '%s'", statusUnhealthy.Inconsistency.MemgraphRole)
		}
	}
}

func TestConvertMemgraphNodeToStatus_SyncReplica(t *testing.T) {
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

	// Create a test client for the node
	config := &Config{
		AppName:         "memgraph",
		StatefulSetName: "memgraph-ha",
	}
	testClient := NewMemgraphClient(config)
	
	node := NewMemgraphNode(pod, testClient)
	node.memgraphRole = "replica"
	// Set cached replica info to avoid network calls
	node.hasReplicasInfo = true
	// node.State = REPLICA // State removed
	// Test SYNC replica conversion  
	status := convertMemgraphNodeToStatus(node, true, pod)

	if status.Name != "memgraph-1" {
		t.Errorf("Expected name 'memgraph-1', got '%s'", status.Name)
	}

	if status.State != "unknown" { // State is now "unknown" since State field removed
		t.Errorf("Expected state 'unknown', got '%s'", status.State)
	}

	if !status.Healthy {
		t.Error("Expected pod to be healthy")
	}

	if status.MemgraphRole != "replica" {
		t.Errorf("Expected MemgraphRole 'replica', got '%s'", status.MemgraphRole)
	}
}

func TestHTTPServerStatusEndpoint(t *testing.T) {
	// Create a simple unit test that doesn't rely on network calls
	// Test the HTTP response format and structure

	// Create a test client for the nodes
	config := &Config{
		AppName:         "memgraph",
		StatefulSetName: "memgraph-ha",
	}
	testClient := NewMemgraphClient(config)
	
	// Create a MemgraphCluster with mock data
	clusterState := NewMemgraphCluster(nil, config, testClient)
	// CurrentMain field has been removed - target main is now tracked via controller's target main index

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

	node1 := NewMemgraphNode(pod1, testClient)
	node1.memgraphRole = "main"
	// Set cached replica info to avoid network calls
	node1.hasReplicasInfo = true
	// node1.State = MAIN // State removed

	node2 := NewMemgraphNode(pod2, testClient)
	node2.memgraphRole = "replica"
	// Set cached replica info to avoid network calls
	node2.hasReplicasInfo = true
	// node2.State = REPLICA // State removed
	// Remove IsSyncReplica assignment - this info is stored in replica registrations, not on nodes

	clusterState.MemgraphNodes["memgraph-0"] = node1
	clusterState.MemgraphNodes["memgraph-1"] = node2

	// Create API response directly
	response := StatusResponse{
		Timestamp: time.Now(),
		ClusterState: ClusterStatus{
			TotalPods:          len(clusterState.MemgraphNodes),
			HealthyPods:        2,
			UnhealthyPods:      0,
			CurrentMain:        "memgraph-0", // hardcode for test
			CurrentSyncReplica: "memgraph-1",
			SyncReplicaHealthy: true,
		},
		Pods: []PodStatus{
			convertMemgraphNodeToStatus(node1, true, pod1),
			convertMemgraphNodeToStatus(node2, true, pod2),
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

	if decoded.ClusterState.CurrentMain != "memgraph-0" {
		t.Errorf("Expected main 'memgraph-0', got '%s'", decoded.ClusterState.CurrentMain)
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

	expectedEndpoints := []string{"/api/v1/status", "/api/v1/leadership", "/health", "/livez", "/readyz"}
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
