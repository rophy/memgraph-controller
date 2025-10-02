package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"memgraph-controller/internal/common"
)

// MockController implements ControllerInterface for testing
type MockController struct {
	isLeader      bool
	isRunning     bool
	clusterStatus *StatusResponse
	statusError   error
}

func (m *MockController) GetClusterStatus(ctx context.Context) (*StatusResponse, error) {
	if m.statusError != nil {
		return nil, m.statusError
	}
	if m.clusterStatus != nil {
		return m.clusterStatus, nil
	}
	return &StatusResponse{
		Timestamp: time.Now(),
		ClusterState: ClusterStatus{
			CurrentMain: "memgraph-0",
			TotalPods:   2,
			HealthyPods: 2,
		},
		Pods: []PodStatus{},
	}, nil
}

func (m *MockController) GetLeaderElection() LeaderElectionInterface {
	return &MockLeaderElection{isLeader: m.isLeader}
}

func (m *MockController) IsLeader() bool {
	return m.isLeader
}

func (m *MockController) IsRunning() bool {
	return m.isRunning
}

func (m *MockController) ResetAllConnections(ctx context.Context) (int, error) {
	// Mock implementation - just return a fake count
	return 3, nil
}

func (m *MockController) HandlePreStopHook(ctx context.Context, podName string) error {
	// Mock implementation - just return success
	return nil
}

// MockLeaderElection implements LeaderElectionInterface for testing
type MockLeaderElection struct {
	isLeader bool
}

func (m *MockLeaderElection) GetCurrentLeader(ctx context.Context) (string, error) {
	return "controller-0", nil
}

func (m *MockLeaderElection) GetMyIdentity() string {
	return "controller-0"
}

func (m *MockLeaderElection) IsLeader() bool {
	return m.isLeader
}

func TestHTTPServerHealthEndpoint(t *testing.T) {
	config := &common.Config{HTTPPort: "8080"}
	controller := &MockController{isRunning: true}
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
	config := &common.Config{HTTPPort: "8080"}
	controller := &MockController{isRunning: true}
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

	if root["version"] != "1.0.0" {
		t.Errorf("Expected version '1.0.0', got '%v'", root["version"])
	}
}

func TestHTTPServerMethodNotAllowed(t *testing.T) {
	config := &common.Config{HTTPPort: "8080"}
	controller := &MockController{isRunning: true}
	httpServer := NewHTTPServer(controller, config)

	// Test POST on status endpoint (should only accept GET)
	req := httptest.NewRequest("POST", "/api/v1/status", nil)
	w := httptest.NewRecorder()
	httpServer.handleStatus(w, req)
	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("Expected status 405 for POST on /api/v1/status, got %d", w.Code)
	}

	// Test POST on health endpoint (should only accept GET)
	req = httptest.NewRequest("POST", "/health", nil)
	w = httptest.NewRecorder()
	httpServer.handleHealth(w, req)
	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("Expected status 405 for POST on /health, got %d", w.Code)
	}
}

func TestHTTPServerLivenessProbe(t *testing.T) {
	config := &common.Config{HTTPPort: "8080"}

	// Test when controller is running
	controller := &MockController{isRunning: true}
	httpServer := NewHTTPServer(controller, config)

	req := httptest.NewRequest("GET", "/livez", nil)
	w := httptest.NewRecorder()
	httpServer.handleLiveness(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("Expected status 200 when controller is running, got %d", w.Code)
	}

	// Test when controller is not running
	controller.isRunning = false
	w = httptest.NewRecorder()
	httpServer.handleLiveness(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("Expected status 503 when controller is not running, got %d", w.Code)
	}
}

func TestHTTPServerReadinessProbe(t *testing.T) {
	config := &common.Config{HTTPPort: "8080"}

	// Test when controller is leader
	controller := &MockController{isLeader: true, isRunning: true}
	httpServer := NewHTTPServer(controller, config)

	req := httptest.NewRequest("GET", "/readyz", nil)
	w := httptest.NewRecorder()
	httpServer.handleReadiness(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("Expected status 200 when controller is leader, got %d", w.Code)
	}

	// Test when controller is not leader
	controller.isLeader = false
	w = httptest.NewRecorder()
	httpServer.handleReadiness(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("Expected status 503 when controller is not leader, got %d", w.Code)
	}
}
